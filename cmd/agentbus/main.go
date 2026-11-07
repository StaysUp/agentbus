// Command agentbus runs the framework on this machine.
//
//	agentbus up [-listen 127.0.0.1:8787] [-origin http://localhost:5173]
//	agentbus version
//
// up starts one service on one loopback address. /api and /s/<session>/api go
// to the client API, which needs the token in the data folder. Vendor paths go
// to that vendor's gateway, which forwards them and stores their events:
// /backend-api/codex to chatgpt.com (codex), /v1 to api.anthropic.com (Claude
// Code), each with or without /s/<session> in front. Any other path is
// answered 404 here and sent nowhere.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/api"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/cost"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/logfile"
	"github.com/staysup/agentbus/replay"
	"github.com/staysup/agentbus/store"
)

const (
	upstream = "https://api.anthropic.com"
	// chatgpt is where codex signed in with a ChatGPT plan sends model calls,
	// under /backend-api/codex (codex-cli 0.164.0, 2026-11-05).
	chatgpt = "https://chatgpt.com"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version" || args[0] == "-version") {
		fmt.Fprintln(stdout, versionLine(debug.ReadBuildInfo()))
		return 0
	}
	if len(args) == 0 || args[0] != "up" {
		fmt.Fprintln(stderr, "usage: agentbus up [-listen 127.0.0.1:8787] [-origin http://localhost:5173] [-replay=false]")
		fmt.Fprintln(stderr, "       agentbus version")
		return 2
	}
	fl := flag.NewFlagSet("up", flag.ContinueOnError)
	fl.SetOutput(stderr)
	listen := fl.String("listen", "127.0.0.1:8787", "address to listen on; must be a loopback IP")
	origin := fl.String("origin", "", "the one cross-origin cockpit allowed, such as http://localhost:5173")
	replayOn := fl.Bool("replay", true, "continue claude and codex sessions by own replay (the gateway sends the earlier turn's history); false uses the vendor's resume")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "agentbus up: unexpected argument %q\n", fl.Arg(0))
		return 2
	}

	// net/http writes some of its own messages to the global logger, and they
	// can quote a vendor reply's headers. Nothing else here uses that logger.
	log.SetOutput(io.Discard)
	own := log.New(stderr, "agentbus: ", log.LstdFlags)

	home, err := dataFolder(getenv)
	if err != nil {
		own.Print(err)
		return 1
	}
	up, _ := url.Parse(upstream)
	cg, _ := url.Parse(chatgpt)
	svc, err := start(config{listen: *listen, origin: *origin, home: home, upstream: up, chatgpt: cg, logf: own.Printf,
		noReplay: !*replayOn})
	if err != nil {
		own.Print(err)
		return 1
	}
	own.Printf("listening on http://%s, data folder %s", svc.addr, home)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-svc.done:
		own.Printf("stopped: %v", err)
		svc.stop()
		return 1
	}
	svc.stop()
	return 0
}

type config struct {
	listen   string
	origin   string
	home     string
	upstream *url.URL // Anthropic
	chatgpt  *url.URL // codex's ChatGPT backend
	logf     func(string, ...any)

	// For runs. Nil means the process's own: os.Getenv, os.Environ and
	// exec.LookPath. Zero runGrace means the API's default.
	getenv   func(string) string
	environ  func() []string
	lookPath func(string) (string, error)
	runGrace time.Duration
	// logMax is where logs/service.log rotates, in bytes. Zero means 10 MiB.
	logMax int64
	// noReplay turns own replay off: claude's and codex's next turns use the
	// vendor's resume. The zero value, replay on, is the default.
	noReplay bool
}

type service struct {
	addr  string // the address listened on
	srv   *http.Server
	gws   []*gateway.Gateway
	api   *api.API
	store *store.Store
	log   *logfile.Log // logs/service.log
	done  chan error
	once  sync.Once
}

// start opens the store, makes the token if needed, and serves.
func start(c config) (*service, error) {
	if err := loopbackOnly(c.listen); err != nil {
		return nil, err
	}
	if err := checkOrigin(c.origin); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.home, 0o700); err != nil {
		return nil, fmt.Errorf("data folder %s could not be made: %w", c.home, err)
	}
	// MkdirAll leaves an existing folder's mode alone, and the store holds the
	// person's conversations and tool output. Make the folder private every
	// start, so files inside it are unreachable to other users whatever their
	// own modes.
	if err := os.Chmod(c.home, 0o700); err != nil {
		return nil, fmt.Errorf("data folder %s could not be made private: %w", c.home, err)
	}
	token, err := loadToken(filepath.Join(c.home, "token"))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(c.home, "events.db"))
	if err != nil {
		return nil, err
	}
	// The logs are opened after the store, which refuses a second service on
	// this folder, so two services never write one log.
	logs := filepath.Join(c.home, "logs")
	runLogs := filepath.Join(logs, "runs")
	if err := logfile.Dir(logs); err != nil {
		st.Close()
		return nil, fmt.Errorf("log folder %s could not be made: %w", logs, err)
	}
	if err := logfile.Dir(runLogs); err != nil {
		st.Close()
		return nil, fmt.Errorf("log folder %s could not be made: %w", runLogs, err)
	}
	svcLog, err := logfile.Open(filepath.Join(logs, "service.log"), c.logMax, nil)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("the service log could not be opened: %w", err)
	}
	st.OnError = func(err error) {
		c.logf("an event could not be stored: %v", err)
		svcLog.Printf("store.error error=%q", err.Error())
	}
	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		svcLog.Close()
		st.Close()
		return nil, fmt.Errorf("-listen %s: %w", c.listen, err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	if c.getenv == nil {
		c.getenv = os.Getenv
	}
	if c.environ == nil {
		c.environ = os.Environ
	}
	if c.lookPath == nil {
		c.lookPath = exec.LookPath
	}
	// Own replay: claude's and codex's next turns start fresh, and each
	// vendor's gateway puts the earlier turn's history in front of their
	// requests.
	var claudeReplay, codexReplay *replay.Replayer
	var replayBase func(string, string) (api.ReplayPlan, error)
	if !c.noReplay {
		claudeReplay, codexReplay = replay.New(st), replay.NewCodex(st)
		replayBase = func(agent, session string) (api.ReplayPlan, error) {
			r := claudeReplay
			if agent == "codex" {
				r = codexReplay
			}
			p, err := r.Base(session)
			var no *replay.NoBase
			if errors.As(err, &no) {
				return api.ReplayPlan{Refused: no.Why}, nil
			}
			if err != nil {
				return api.ReplayPlan{}, err
			}
			return api.ReplayPlan{Turn: p.Turn, Failed: p.Failed}, nil
		}
	}
	a, err := api.New(api.Config{Store: st, Token: token, Port: port, BaseURL: "http://" + addr, Origin: c.origin, ReplayBase: replayBase,
		Wire: runWire(c.getenv, c.environ, c.lookPath), RunGrace: c.runGrace,
		LookPath: c.lookPath, Environ: c.environ, Logf: svcLog.Printf,
		RunLog: func(session string, data []byte) {
			// The session id is the service's own hex, so it is a plain
			// file name.
			if err := logfile.AppendTail(filepath.Join(runLogs, session+".log"), data, api.RunOutputMax); err != nil {
				svcLog.Printf("runlog.error session=%s", session)
			}
		}})
	if err != nil {
		ln.Close()
		svcLog.Close()
		st.Close()
		return nil, err
	}
	// Every model.responded is priced, and every turn.started of a run's
	// session gets the run's agent and folder, before it is stored.
	sink := cost.Sink{Table: cost.Default(), Next: a.TurnSink(st)}
	anth := gateway.New(c.upstream, anthropic.Adapter{}, sink)
	oai := gateway.New(c.chatgpt, openai.Adapter{}, sink)
	// A turn's session.closed waits for the gateway to write all of the
	// turn's calls: their events and replies are written off the reply's
	// path and can land after the child has exited.
	a.SetSettle(func(session, turn string) bool {
		settled := true
		for _, g := range []*gateway.Gateway{anth, oai} {
			if !g.Settle(session, turn, settleTimeout) {
				svcLog.Printf("settle.unconfirmed session=%s turn=%s", session, turn)
				settled = false
			}
		}
		return settled
	})
	// Every model request's body is kept as pieces, for a call's context
	// view, and every reply an adapter can rebuild, for own replay.
	keep := keeper{st: st, logf: svcLog.Printf}
	anth.KeepBodies(keep)
	oai.KeepBodies(keep)
	if claudeReplay != nil {
		anth.SetRewriter(claudeReplay)
		oai.SetRewriter(codexReplay)
	}
	s := &service{addr: addr, gws: []*gateway.Gateway{anth, oai}, api: a, store: st, log: svcLog, done: make(chan error, 1)}
	s.srv = &http.Server{
		Handler:           logRequests(svcLog, route(a, anth, oai)),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	svcLog.Printf("service.start listen=%s pid=%d", addr, os.Getpid())
	go func() { s.done <- s.srv.Serve(ln) }()
	return s, nil
}

// settleTimeout bounds how long a turn's session.closed waits for the
// gateway to write the turn's calls.
const settleTimeout = 30 * time.Second

// keeper keeps calls and replies in the store, and logs a failure.
type keeper struct {
	st   *store.Store
	logf func(string, ...any)
}

func (k keeper) KeepCall(c body.Call) error {
	err := k.st.KeepCall(c)
	if err != nil {
		k.logf("store.error what=call error=%q", err.Error())
	}
	return err
}

func (k keeper) KeepReply(r body.Reply) error {
	err := k.st.KeepReply(r)
	if err != nil {
		k.logf("store.error what=reply error=%q", err.Error())
	}
	return err
}

// route refuses odd paths, then sends the API namespace to the API and each
// vendor path to its vendor's gateway. A request carries the agent's sign-in,
// so it goes only to the vendor whose path it uses: a path no vendor claims is
// answered 404 here and forwarded nowhere.
func route(a *api.API, anthropicGW, openaiGW http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !plainPath(r) {
			localError(w, "agentbus: the path has a dot, empty or encoded segment and is refused", http.StatusBadRequest)
			return
		}
		if api.Reserved(r) {
			a.ServeHTTP(w, r)
			return
		}
		_, _, rest := gateway.SplitSession(r.URL.EscapedPath())
		switch {
		case under(rest, "/backend-api/codex"):
			openaiGW.ServeHTTP(w, r)
		case under(rest, "/v1"):
			anthropicGW.ServeHTTP(w, r)
		default:
			localError(w, "agentbus: no vendor is routed for this path", http.StatusNotFound)
		}
	})
}

// under reports whether path is prefix or below it, by whole segments, so
// /v1x is not under /v1. plainPath has already refused encoded slashes, so the
// escaped path's segments are the vendor's.
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// plainPath reports whether the request's path is one no normalising can
// change: origin form, no "." or ".." segment, no empty segment, and no
// percent-encoded dot, slash or backslash. Both the escaped path and the
// request line are read. No agent or cockpit sends anything else, and refusing
// these is what lets the API namespace be a plain prefix: otherwise some
// spellings are read as the API by one normaliser and as a vendor path by
// another.
func plainPath(r *http.Request) bool {
	line, _, _ := strings.Cut(r.RequestURI, "?")
	for _, p := range []string{r.URL.EscapedPath(), line} {
		if !strings.HasPrefix(p, "/") {
			return false
		}
		lower := strings.ToLower(p)
		if strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return false
		}
		segs := strings.Split(p[1:], "/")
		for i, seg := range segs {
			if seg == "." || seg == ".." || (seg == "" && i < len(segs)-1) {
				return false
			}
		}
	}
	return true
}

// stop ends runs' children, ends connections, delivers queued events, and
// closes the store.
func (s *service) stop() { s.once.Do(s.shutdown) }

func (s *service) shutdown() {
	s.log.Printf("service.stopping")
	// Runs first: each stores its session.closed, and new runs are refused.
	s.api.StopRuns()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // streams never go idle, so this usually times out
	_ = s.srv.Close()
	for _, gw := range s.gws {
		gw.Close()
	}
	_ = s.store.Close()
	s.log.Printf("service.stop")
	_ = s.log.Close()
}

// dataFolder is $AGENTBUS_HOME, else $XDG_DATA_HOME/agentbus, else
// ~/.local/share/agentbus. Never v1's ~/.agentbus.
func dataFolder(getenv func(string) string) (string, error) {
	if h := getenv("AGENTBUS_HOME"); h != "" {
		return h, nil
	}
	if x := getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "agentbus"), nil
	}
	h := getenv("HOME")
	if h == "" {
		var err error
		if h, err = os.UserHomeDir(); err != nil {
			return "", errors.New("no data folder: set AGENTBUS_HOME")
		}
	}
	return filepath.Join(h, ".local", "share", "agentbus"), nil
}

// loadToken reads the token, or makes it on first start with mode 0600. Its
// value is never printed or put in an error.
func loadToken(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			f.Close()
			return "", errors.New("the token could not be made")
		}
		tok := hex.EncodeToString(b)
		if _, err := f.WriteString(tok + "\n"); err != nil {
			f.Close()
			return "", fmt.Errorf("the token file %s could not be written", path)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("the token file %s could not be written", path)
		}
		return tok, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("the token file %s could not be made", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("the token file %s could not be read", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the token file %s can be read by others (mode %v); make it 0600", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the token file %s could not be read", path)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("the token file %s is empty; delete it to make a new one", path)
	}
	return tok, nil
}

// loopbackOnly keeps the service on this machine, as cmd/abgateway does, and
// on an address the API's Host check accepts: 127.0.0.1 or [::1]. Only those
// IP literals are accepted. Other loopback addresses such as 127.0.0.2 would
// listen, but every API request would fail the Host check. A name such as
// localhost resolves through the hosts file, which can map it to any address.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", addr, err)
	}
	if host == "127.0.0.1" || host == "::1" {
		return nil
	}
	return fmt.Errorf("-listen %q must use 127.0.0.1 or [::1]; the service stays on this machine", addr)
}

// checkOrigin accepts an empty origin or one scheme://host[:port], as a browser
// sends it in the Origin header.
func checkOrigin(o string) error {
	if o == "" {
		return nil
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.String() != o {
		return fmt.Errorf("-origin %q is not an origin such as http://localhost:5173", o)
	}
	return nil
}
