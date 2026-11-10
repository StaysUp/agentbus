// Command agentbus runs the framework on this machine.
//
//	agentbus up [-listen 127.0.0.1:8787] [-origin http://localhost:5173] [-max-runs 4]
//	agentbus server [-listen 127.0.0.1:8787]
//	agentbus runner -server URL
//	agentbus version
//
// modes.go says what each mode runs. up starts one service on one address, 127.0.0.1:8787 unless -listen says
// another, such as 0.0.0.0:8787 in a container. /api and /s/<session>/api go
// to the client API, which needs the token in the data folder. Vendor paths go
// to that vendor's gateway, which forwards them and stores their events:
// /backend-api/codex to chatgpt.com (codex), /v1 to api.anthropic.com (Claude
// Code), each with or without /s/<session> in front. Any other path is
// answered 404 here and sent nowhere. The vendor paths answer only callers on
// this machine, whatever the address. GET / redirects to /docs.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/staysup/agentbus/agents"
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
	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/cost"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/logfile"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/replay"
	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/runner"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

const (
	upstream = "https://api.anthropic.com"
	// chatgpt is where codex signed in with a ChatGPT plan sends model calls,
	// under /backend-api/codex (codex-cli 0.164.0, 2026-11-05).
	chatgpt = "https://chatgpt.com"
	// openrouter is where an OpenRouter agent's model calls go: its Anthropic
	// Messages endpoint for Claude Code and its Responses endpoint for codex,
	// both under /api/v1 (research note 2026-11-08).
	openrouter = "https://openrouter.ai/api"
)

func main() {
	// Run as agentbus-api (the link apiLink makes), it is the API client.
	if filepath.Base(os.Args[0]) == engine.APIClient {
		os.Exit(apiCall(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	// Not root on Linux, up and runner run in a namespace of their own
	// (userns_linux.go).
	if code, done := userns(os.Args[1:], os.Getenv, os.Stderr); done {
		os.Exit(code)
	}
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version" || args[0] == "-version") {
		fmt.Fprintln(stdout, versionLine(debug.ReadBuildInfo()))
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if args[0] == "api" {
		return apiCall(args[1:], getenv, os.Stdin, stdout, stderr)
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
	c, ok := parse(args, home, stderr)
	if !ok {
		return 2
	}
	c.home, c.logf, c.self = home, own.Printf, self()
	if c.mode == modeConnector {
		return runConnector(c, own.Printf)
	}
	c.upstream, _ = url.Parse(upstream)
	c.chatgpt, _ = url.Parse(chatgpt)
	svc, err := start(c)
	if err != nil {
		own.Print(err)
		return 1
	}
	if c.mode == modeRunner {
		own.Printf("runner taking jobs from %s; its agents' gateway on http://%s, data folder %s", c.server, svc.addr, home)
	}
	if svc.loopback {
		own.Printf("listening on http://%s, data folder %s", svc.addr, home)
	} else {
		own.Printf("listening on http://%s, data folder %s; reachable from the network; the token guards the API; no HTTPS", svc.shown, home)
	}

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
	hosts    []string // more names the API answers to on loopback (-host)
	home     string
	upstream *url.URL // Anthropic
	chatgpt  *url.URL // codex's ChatGPT backend
	// openrouter is OpenRouter's API; nil means openrouter above.
	openrouter *url.URL
	logf       func(string, ...any)
	// self is this program's path, which agentbus-api links to
	// (apiLink). Empty tells turns to use curl.
	self string

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
	// maxRuns bounds the turns running at once. Zero means the API's default.
	maxRuns int
	// For tests: how often a runner heartbeats, and how long POST
	// /api/v0/turns waits for a turn to start. Zero means the defaults.
	heartbeat, turnWait time.Duration

	// mode is up (the zero value), server or runner (modes.go).
	mode string
	// A runner's: the server's URL, its enroll_token file, the runner's
	// name, and where its agents send model calls.
	server, enrollFile, runnerName, gatewayMode string
	// A runner's folders it owns (-folder), and its labels (-labels).
	folders, labels []string
}

type service struct {
	addr     string // the address listened on
	shown    string // -listen with its port filled in: Go reports 0.0.0.0 as [::]
	loopback bool   // addr is 127.0.0.1 or [::1]
	srv      *http.Server
	gws      []*gateway.Gateway
	api      *api.API
	eng      *engine.Engine
	store    *store.Store
	queue    *queue.Queue
	remote   *settings.Remote  // a runner's settings: the server's
	settings settings.Store    // the settings the built-in runner reads
	folders  *folders.Registry // the folders this machine owns (up, runner)
	blobs    *blobs.Store      // the control plane's blob store (up, server)
	blobSwp  *jobs.Sweeper     // cleans it
	log      *logfile.Log      // logs/service.log
	done     chan error
	once     sync.Once

	// The built-in runner, and the lease sweep: stopped before the API.
	stopRunner func()
	runnerDone chan struct{}
	stopSweep  func()
	sweepDone  chan struct{}
}

// start opens the store, makes the token if needed, and serves.
func start(c config) (*service, error) {
	c.listen = withPort(c.listen)
	loopback, err := checkListen(c.listen)
	if err != nil {
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
	token, err := loadToken(filepath.Join(c.home, "token"), "")
	if err != nil {
		return nil, err
	}
	// The turns token: it may only start, stop and read turns, for a job
	// queue or a runner that should not hold the main token.
	turnsToken, err := loadToken(filepath.Join(c.home, "turns_token"), turnsTokenPrefix)
	if err != nil {
		return nil, err
	}
	// The settings: the policy and the instructions every turn gets. up and
	// server keep them as files in the data folder, written with every
	// default when missing. A runner reads the server's, through the runner
	// protocol, so every runner follows one policy.
	var cfgStore settings.Store
	var remote *settings.Remote
	if c.mode == modeRunner {
		remote = &settings.Remote{Server: strings.TrimSuffix(c.server, "/"), Local: c.home}
		cfgStore = remote
	} else {
		files := settings.NewFiles(c.home)
		if err := files.Init(); err != nil {
			return nil, err
		}
		cfgStore = files
	}
	// The data folder's own files, which no turn may edit: a turn that could
	// would raise its own limits, or read a token.
	protect := cfgStore.Protected()
	// The enrollment token adds a runner. A service with a queue keeps it in
	// its data folder: the built-in runner reads it, and a runner on
	// another machine is given a copy. A runner reads the server's copy.
	var enrollToken string
	switch {
	case c.hasQueue():
		if enrollToken, err = loadToken(filepath.Join(c.home, "enroll_token"), enrollTokenPrefix); err != nil {
			return nil, err
		}
	case c.mode == modeRunner:
		if enrollToken, err = readTokenFile(c.enrollFile); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(filepath.Join(c.home, "events.db"))
	if err != nil {
		return nil, err
	}
	// The job queue, in its own file beside the record: the record knows
	// nothing about jobs. Opened after the store, which refuses a second
	// service on this folder.
	var q *queue.Queue
	var jobsAPI http.Handler // nil, not a nil *jobs.Server, without a queue
	closeQ := func() {
		if q != nil {
			q.Close()
		}
	}
	// The saved agents live with the queue: where turns are asked for, not
	// where they run.
	var saved *agents.Store
	// The folders this machine owns, as their connector: in up and in a
	// runner, where the folders are. In up the API and the jobs routes hand
	// it the paths they are sent, and keep only references.
	var fr *folders.Registry
	var issuer queue.FolderIssuer // nil, not a nil *folders.Registry, in server
	var bs *blobs.Store           // the control plane's blob store (up, server)
	var blobSweep *jobs.Sweeper   // and what cleans it
	if c.hasEngine() {
		if fr, err = folders.Open(filepath.Join(c.home, "folders.json")); err != nil {
			st.Close()
			return nil, err
		}
		if c.mode == modeUp {
			issuer = fr
		}
		for _, f := range c.folders {
			if _, err := fr.Register(f); err != nil {
				st.Close()
				return nil, fmt.Errorf("-folder %s: %w", f, err)
			}
		}
	}
	if c.hasQueue() {
		if saved, err = agents.Open(c.home, nil); err != nil {
			st.Close()
			return nil, err
		}
		if q, err = queue.Open(filepath.Join(c.home, "jobs.db"), nil); err != nil {
			st.Close()
			return nil, err
		}
		// Jobs stored before folder references name paths: the built-in
		// connector keeps them, and the jobs keep references.
		if issuer != nil {
			if _, err := q.MigrateFolders(issuer); err != nil {
				closeQ()
				st.Close()
				return nil, err
			}
		}
		// Every file of a snapshot or of a turn's changes goes through the
		// control plane's blob store.
		if bs, err = blobs.Open(filepath.Join(c.home, "blobs")); err != nil {
			closeQ()
			st.Close()
			return nil, err
		}
		bs.Clean(time.Hour)
		blobSweep = jobs.NewSweeper(q, bs, nil) // logs once the log is open
		js, err := jobs.New(jobs.Config{Queue: q, Token: token, EnrollToken: enrollToken, Heartbeat: c.heartbeat, Agents: saved, Settings: cfgStore,
			Folders: issuer, Blobs: bs, Sweeper: blobSweep, Record: st})
		if err != nil {
			closeQ()
			st.Close()
			return nil, err
		}
		jobsAPI = js
	}
	// The logs are opened after the store, which refuses a second service on
	// this folder, so two services never write one log.
	logs := filepath.Join(c.home, "logs")
	runLogs := filepath.Join(logs, "runs")
	if err := logfile.Dir(logs); err != nil {
		st.Close()
		closeQ()
		return nil, fmt.Errorf("log folder %s could not be made: %w", logs, err)
	}
	if err := logfile.Dir(runLogs); err != nil {
		st.Close()
		closeQ()
		return nil, fmt.Errorf("log folder %s could not be made: %w", runLogs, err)
	}
	svcLog, err := logfile.Open(filepath.Join(logs, "service.log"), c.logMax, nil)
	if err != nil {
		st.Close()
		closeQ()
		return nil, fmt.Errorf("the service log could not be opened: %w", err)
	}
	if blobSweep != nil {
		blobSweep.Logf = svcLog.Printf
	}
	st.OnError = func(err error) {
		c.logf("an event could not be stored: %v", err)
		svcLog.Printf("store.error error=%q", err.Error())
	}
	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		svcLog.Close()
		st.Close()
		closeQ()
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
	var replayBase func(string, string) (engine.ReplayPlan, error)
	if !c.noReplay {
		claudeReplay, codexReplay = replay.New(st), replay.NewCodex(st)
		replayBase = func(agent, session string) (engine.ReplayPlan, error) {
			// Each harness names the history format own replay uses for it.
			r := claudeReplay
			if h, ok := harness.Get(agent); ok && h.Info().Replay == harness.FormatOpenAI {
				r = codexReplay
			}
			p, err := r.Base(session)
			var no *replay.NoBase
			if errors.As(err, &no) {
				return engine.ReplayPlan{Refused: no.Why}, nil
			}
			if err != nil {
				return engine.ReplayPlan{}, err
			}
			return engine.ReplayPlan{Turn: p.Turn, Failed: p.Failed}, nil
		}
	}
	baseURL := "http://" + agentAddr(ln.Addr())
	// The engine starts each turn's agent, on this machine. Only a runner
	// calls it, for the jobs it takes from the queue: the built-in one of up,
	// or this process's own in runner mode. A server runs no turns.
	var eng *engine.Engine
	if c.hasEngine() {
		// A turn on another harness than its session's latest names the
		// earlier turns' agents by their saved names, where this process
		// keeps them.
		var agentName func(string) string
		if saved != nil {
			agentName = func(id string) string {
				a, err := saved.Get(id)
				if err != nil {
					return ""
				}
				return a.Name
			}
		}
		eng, err = engine.New(engine.Config{Store: st, BaseURL: baseURL, MaxRuns: c.maxRuns, ReplayBase: replayBase, AgentName: agentName,
			Settings: cfgStore, Protect: protect, APIDir: apiLink(c.home, c.self, svcLog), Wire: runWire(c.getenv, c.environ, c.lookPath), RunGrace: c.runGrace, Logf: svcLog.Printf,
			RunLog: func(session string, data []byte) {
				// The session id is the service's own hex, so it is a plain
				// file name.
				if err := logfile.AppendTail(filepath.Join(runLogs, session+".log"), data, engine.RunOutputMax); err != nil {
					svcLog.Printf("runlog.error session=%s", session)
				}
			}})
		if err != nil {
			ln.Close()
			svcLog.Close()
			st.Close()
			closeQ()
			return nil, err
		}
	}
	a, err := api.New(api.Config{Store: st, Token: token, TurnsToken: turnsToken, Port: port, AnyHost: !loopback,
		BaseURL: baseURL, Origin: c.origin, Hosts: c.hosts, Environ: c.environ, Folders: issuer, Build: build(debug.ReadBuildInfo()),
		Queue: q, Jobs: jobsAPI, Engine: eng, TurnWait: c.turnWait, Agents: saved, Settings: cfgStore})
	if err != nil {
		ln.Close()
		svcLog.Close()
		st.Close()
		closeQ()
		return nil, err
	}
	// Every model.responded is priced, and every turn.started of a run's
	// session gets the run's agent and folder, before it is stored.
	sink := cost.Sink{Table: cost.Default(), Next: engine.TurnSink(st, st)}
	anth := gateway.New(c.upstream, anthropic.Adapter{}, sink)
	oai := gateway.New(c.chatgpt, openai.Adapter{}, sink)
	// An OpenRouter agent's calls, under /openrouter of its base URL: one
	// gateway per API format, both to OpenRouter.
	orURL := c.openrouter
	if orURL == nil {
		orURL, _ = url.Parse(openrouter)
	}
	orAnth := gateway.New(orURL, anthropic.Adapter{}, sink)
	orOAI := gateway.New(orURL, openai.Adapter{}, sink)
	orAnth.SetPrefix(openRouterPrefix)
	orOAI.SetPrefix(openRouterPrefix)
	gws := []*gateway.Gateway{anth, oai, orAnth, orOAI}
	// A turn's session.closed waits for the gateway to write all of the
	// turn's calls: their events and replies are written off the reply's
	// path and can land after the child has exited.
	if eng != nil {
		eng.SetSettle(func(session, turn string) bool {
			settled := true
			for _, g := range gws {
				if !g.Settle(session, turn, settleTimeout) {
					svcLog.Printf("settle.unconfirmed session=%s turn=%s", session, turn)
					settled = false
				}
			}
			return settled
		})
	}
	// Every model request's body is kept as pieces, for a call's context
	// view, and every reply an adapter can rebuild, for own replay.
	keep := keeper{st: st, logf: svcLog.Printf}
	for _, g := range gws {
		g.KeepBodies(keep)
		// forbid in permissions.toml, applied to each run turn's replies.
		if eng != nil {
			g.SetGuard(eng)
		}
	}
	if claudeReplay != nil {
		anth.SetRewriter(claudeReplay)
		orAnth.SetRewriter(claudeReplay)
		oai.SetRewriter(codexReplay)
		orOAI.SetRewriter(codexReplay)
	}
	host, _, _ := net.SplitHostPort(c.listen)
	s := &service{addr: addr, shown: net.JoinHostPort(host, port), loopback: loopback, gws: gws, api: a, eng: eng, store: st, blobs: bs, blobSwp: blobSweep,
		queue: q, remote: remote, settings: cfgStore, folders: fr, log: svcLog, done: make(chan error, 1)}
	s.srv = &http.Server{
		Handler:           logRequests(svcLog, route(a, vendors{anth, oai, orAnth, orOAI}, !loopback)),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	svcLog.Printf("service.start mode=%s listen=%s pid=%d", c.modeName(), addr, os.Getpid())
	go func() { s.done <- s.srv.Serve(ln) }()
	if q != nil {
		// A delivered result names the saved agent that did the work.
		name := func(id string) string {
			if saved == nil {
				return ""
			}
			a, err := saved.Get(id)
			if err != nil {
				return ""
			}
			return a.Name
		}
		s.sweep(svcLog, name)
	}
	// The runner takes jobs through the runner protocol over HTTP: the
	// built-in one from this service on loopback, as a runner on another
	// machine does from its server.
	switch {
	case c.mode == modeRunner:
		err = s.startRunner(c, strings.TrimSuffix(c.server, "/"), enrollToken, svcLog)
	case eng != nil:
		err = s.startRunner(c, baseURL, enrollToken, svcLog)
	}
	if err != nil {
		s.stop()
		return nil, err
	}
	return s, nil
}

// sweep settles expired leases, and delivers the results of asked work, every
// second until the service stops.
func (s *service) sweep(svcLog *logfile.Log, name func(string) string) {
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	s.stopSweep = sync.OnceFunc(func() { close(stop); cancel() })
	s.sweepDone = make(chan struct{})
	// The blob store: blobs no snapshot or changeset of an open session
	// reaches (jobs.Sweeper).
	var blobsDone sync.WaitGroup
	if s.blobSwp != nil {
		blobsDone.Add(1)
		go func() { defer blobsDone.Done(); s.blobSwp.Run(ctx) }()
	}
	go func() {
		defer close(s.sweepDone)
		defer blobsDone.Wait()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if n, err := s.queue.Sweep(); err != nil {
					svcLog.Printf("queue.sweep_failed")
				} else if n > 0 {
					svcLog.Printf("queue.leases_expired n=%d", n)
				}
				// Asked work that ended: its result becomes the asking
				// session's next turn (queue.Deliver).
				if n, err := s.queue.Deliver(queue.DefaultTeam, name); err != nil {
					svcLog.Printf("queue.deliver_failed")
				} else if n > 0 {
					svcLog.Printf("queue.results_delivered n=%d", n)
				}
			}
		}
	}()
}

// startRunner enrolls the built-in runner and starts it, and beside it the
// connector of this machine's folders when it has any: in up always, and in
// a runner with -folder. In up both share the control plane's blob store on
// disk, so a copy is made by clone or overlay and no file is sent over
// HTTP. A runner of a server keeps its own store, with the server's behind
// it.
func (s *service) startRunner(c config, self, enrollToken string, svcLog *logfile.Log) error {
	capacity := s.eng.MaxRuns()
	name := c.runnerName
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "built-in"
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	fail := func(err error) error {
		cancel()
		wg.Wait()
		return err
	}
	cfg := runner.Config{Queue: self, EnrollToken: enrollToken, Name: name, Capacity: capacity,
		Engine: s.eng, Store: s.store, Logf: svcLog.Printf, Labels: c.labels,
		LookPath: c.lookPath, Environ: c.environ,
		Workdir: filepath.Join(c.home, "worktrees"), StateFile: filepath.Join(c.home, "runner.json"), Settings: s.settings,
		Forward: c.mode == modeRunner}
	// A runner of a server keeps its own store; a connector beside it
	// shares the one handle.
	var own *blobs.Store
	if s.blobs != nil {
		cfg.Blobs, cfg.BlobSink = s.blobs, s.blobs
	} else {
		var err error
		if own, err = blobs.Open(filepath.Join(c.home, cacheDir)); err != nil {
			return fail(err)
		}
	}
	if s.folders != nil && (c.mode == modeUp || len(c.folders) > 0) {
		wg.Add(1)
		conn, err := startConnector(ctx, connectorConfig{server: self, enrollToken: enrollToken, name: name, home: c.home,
			folders: s.folders, store: s.blobs, own: own, logf: svcLog.Printf}, wg.Done)
		if err != nil {
			wg.Done()
			return fail(err)
		}
		cfg.Folders, cfg.Lender = s.folders, conn
	}
	rn, err := runner.Enroll(ctx, cfg)
	if err != nil {
		return fail(fmt.Errorf("the runner could not enroll with %s: %w", self, err))
	}
	if own != nil {
		client := blobClient(self, rn.Token())
		rn.UseBlobs(blobs.Local{Store: own, From: client}, client)
		go sweepCache(ctx, own, svcLog.Printf)
	}
	if s.remote != nil {
		s.remote.Enrolled(rn.ID(), rn.Token())
	}
	svcLog.Printf("runner.start id=%s capacity=%d", rn.ID(), capacity)
	s.stopRunner = cancel
	s.runnerDone = make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		rn.Run(ctx)
	}()
	go func() {
		wg.Wait()
		close(s.runnerDone)
	}()
	return nil
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
// answered 404 here and forwarded nowhere. The root sends a browser to the
// docs.
//
// open is true when the service listens beyond loopback. The vendor paths
// have no token: a request there carries its own sign-in to the vendor, and
// a run's turn gets its session's history put in front. Only a run's agent
// needs them, and it runs on this machine, so with open set they answer 403
// to any other caller, before anything is forwarded. Without it only this
// machine can connect anyway. Paths no vendor claims get the same 404 from
// any caller.
// vendors are the gateways the router sends model calls to.
type vendors struct {
	anthropic, openai               http.Handler // the vendors' own, for a subscription
	openrouterAnth, openrouterResps http.Handler // OpenRouter's, under /openrouter
}

func route(a *api.API, v vendors, open bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !plainPath(r) {
			localError(w, "agentbus: the path has a dot, empty or encoded segment and is refused", http.StatusBadRequest)
			return
		}
		if api.Reserved(r) {
			a.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, "/docs", http.StatusFound)
			return
		}
		_, _, rest := gateway.SplitSession(r.URL.EscapedPath())
		var gw http.Handler
		switch {
		case under(rest, "/backend-api/codex"):
			gw = v.openai
		case under(rest, "/v1"):
			gw = v.anthropic
		case under(rest, openRouterPrefix+"/v1/responses"):
			gw = v.openrouterResps
		case under(rest, openRouterPrefix+"/v1"):
			gw = v.openrouterAnth
		default:
			localError(w, "agentbus: no vendor is routed for this path", http.StatusNotFound)
			return
		}
		if open && !thisMachine(r) {
			localError(w, "agentbus: the model gateway answers callers on this machine only", http.StatusForbidden)
			return
		}
		gw.ServeHTTP(w, r)
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
	// The runner first: it stops its jobs' turns and finishes them through
	// the API, which is still serving.
	if s.stopRunner != nil {
		s.stopRunner()
		<-s.runnerDone
	}
	// Turns next: each stores its session.closed, and new ones are refused.
	if s.eng != nil {
		s.eng.StopAll()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // streams never go idle, so this usually times out
	_ = s.srv.Close()
	for _, gw := range s.gws {
		gw.Close()
	}
	if s.stopSweep != nil {
		s.stopSweep()
		<-s.sweepDone
	}
	if s.queue != nil {
		_ = s.queue.Close()
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

// turnsTokenPrefix marks the turns token, so a person who finds one can tell
// it from the main token and from a turn's own abt_ token.
const turnsTokenPrefix = "abq_"

// enrollTokenPrefix marks the enrollment token, which adds a runner.
const enrollTokenPrefix = "abe_"

// loadToken reads a token, or makes it on first start with mode 0600: prefix
// and 64 hex characters. Its value is never printed or put in an error.
func loadToken(path, prefix string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			f.Close()
			return "", errors.New("the token could not be made")
		}
		tok := prefix + hex.EncodeToString(b)
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

// defaultPort is the port -listen uses when it names an IP alone.
const defaultPort = "8787"

// withPort gives -listen the default port when it names an IP alone:
// 0.0.0.0, 127.0.0.1, [::1] or ::1. Anything else is returned as it is, for
// checkListen to accept or refuse. A name gets no port here, so it is still
// refused as a name.
func withPort(addr string) string {
	host := addr
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if net.ParseIP(host) == nil {
		return addr
	}
	return net.JoinHostPort(host, defaultPort)
}

// checkListen accepts an IP literal and a port, or a port alone (":8787",
// every address), and reports whether the address is loopback: 127.0.0.1 or
// [::1], where the API checks the Host. A name such as localhost is refused:
// it resolves through the hosts file, which can map it to any address. Other
// loopback addresses such as 127.0.0.2 are refused as before: every API
// request would fail the Host check there.
func checkListen(addr string) (loopback bool, err error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("-listen %q is not an IP, with or without a port, such as 127.0.0.1, 0.0.0.0 or 0.0.0.0:8787", addr)
	}
	if host == "127.0.0.1" || host == "::1" {
		return true, nil
	}
	if host == "" {
		return false, nil
	}
	ip := net.ParseIP(host)
	if ip == nil || strings.Contains(host, "%") {
		return false, fmt.Errorf("-listen %q must use an IP address such as 127.0.0.1 or 0.0.0.0, not a name", addr)
	}
	if ip.IsLoopback() {
		return false, fmt.Errorf("-listen %q: of the loopback addresses only 127.0.0.1 and [::1] are accepted", addr)
	}
	return false, nil
}

// agentAddr is the address a run's agent reaches the service on. The agent
// runs on this machine, so on every address (0.0.0.0, [::] or none) it is
// 127.0.0.1, and the agent's sign-in never crosses a network. Go listens on
// both IPv4 and IPv6 there, and reports [::] even for 0.0.0.0. On one
// address it is that address.
func agentAddr(listened net.Addr) string {
	host, port, _ := net.SplitHostPort(listened.String())
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// thisMachine reports whether the request came from this machine: from a
// loopback address, or from the very address it arrived on.
func thisMachine(r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(peer)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(local.String())
	return err == nil && ip.Equal(net.ParseIP(host))
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
