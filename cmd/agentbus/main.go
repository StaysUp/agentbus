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
// Code), /v1internal: to daily-cloudcode-pa.googleapis.com (agy), each with or
// without /s/<session> in front. Any other path is
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
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/gemini"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/api"
	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/cost"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/logfile"
	"github.com/staysup/agentbus/netjail"
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
	// cloudCode is where agy signed in with a Google account sends its Code
	// Assist calls, model calls under /v1internal: among them (agy 1.5.5,
	// 2026-11-11, read from its own log). The production host,
	// cloudcode-pa.googleapis.com, answered every agy model call 429
	// RESOURCE_EXHAUSTED while this one answered at once.
	cloudCode = "https://daily-cloudcode-pa.googleapis.com"
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
	if args[0] == "connect" {
		fmt.Fprintln(stderr, connectRemoved)
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
	if args[0] == "login" {
		return runLogin(args[1:], home, stdout, stderr, systemLogin())
	}
	c, ok := parse(args, home, stderr)
	if !ok {
		return 2
	}
	c.home, c.logf, c.self = home, own.Printf, self()
	c.upstream, _ = url.Parse(upstream)
	c.chatgpt, _ = url.Parse(chatgpt)
	svc, err := start(c)
	if err != nil {
		own.Print(err)
		return 1
	}
	if c.mode == modeRunner {
		own.Printf("runner taking jobs from %s; its agents' gateway on http://%s, data folder %s", c.server, svc.addr, home)
		own.Print(networkNote())
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
	// cloudCode is agy's Code Assist host; nil means cloudCode above.
	cloudCode *url.URL
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
	// A runner's: the server's URL, the runner's name, and where its agents
	// send model calls.
	server, runnerName, gatewayMode string
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
	saved    *agents.Store // the saved agents, with the queue; nil in a runner
	eng      *engine.Engine
	store    *store.Store
	queue    *queue.Queue
	remote   *settings.Remote  // a runner's settings: the server's
	settings settings.Store    // the settings the built-in runner reads
	sdb      *settings.DB      // control's settings database (up, server)
	folders  *folders.Registry // the folders this machine owns (up, runner)
	blobs    *blobs.Store      // the control plane's blob store (up, server)
	blobSwp  *jobs.Sweeper     // cleans it
	jobsSrv  *jobs.Server      // the runner protocol; its finisher ends jobs (up, server)
	log      *logfile.Log      // logs/service.log
	logins   *credentials.Manager
	done     chan error
	once     sync.Once

	// The built-in runner, and the lease sweep: stopped before the API.
	stopRunner func()
	runnerDone chan struct{}
	stopSweep  func()
	subs       *credentials.Subscriptions // control's connected subscriptions; nil without a queue
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
	// The turns token: it may only start, stop, review and read turns, for a job
	// queue or a runner that should not hold the main token.
	turnsToken, err := loadToken(filepath.Join(c.home, "turns_token"), turnsTokenPrefix)
	if err != nil {
		return nil, err
	}
	// The settings: the policy, the instructions every turn gets and the
	// saved agents. up and server keep them in settings.db in the data
	// folder, reached only through the API. The first start of this build
	// copies the data folder beside it, then imports the settings files of
	// 0.4 and before once (settings.Upgrade). A runner reads the server's,
	// through the runner protocol, so every runner follows one policy.
	var cfgStore settings.Store
	var remote *settings.Remote
	var sdb *settings.DB
	var upgraded settings.Report
	sdbHanded := false // once the service holds it, its stop closes it
	defer func() {
		if sdb != nil && !sdbHanded {
			sdb.Close()
		}
	}()
	if c.mode == modeRunner {
		remote = &settings.Remote{Server: strings.TrimSuffix(c.server, "/"), Local: c.home}
		cfgStore = remote
	} else {
		sdb, upgraded, err = settings.Upgrade{Dir: c.home, Lock: filepath.Join(c.home, "events.db") + ".lock",
			Skip: []string{copiesDir, oldCopiesDir, "blobs"}, Agents: agents.ImportRows, Starters: starters(c)}.Run()
		if err != nil {
			return nil, err
		}
		if upgraded.Copy != "" {
			c.logf("the data folder was copied to %s before the settings moved into %s", upgraded.Copy, settings.DBFile)
		}
		cfgStore = sdb
	}
	// The data folder's own files, which no turn may edit: a turn that could
	// would raise its own limits, or read a token.
	protect := cfgStore.Protected()
	// Control keeps the person's connected subscriptions there: no turn
	// edits them either.
	subs := subscriptionStore(c.home)
	if c.hasQueue() {
		for _, h := range harness.All() {
			if l := h.Info().Login; l != nil {
				protect = append(protect, filepath.Join(subs.Dir, h.Info().Name+".json"))
				if l.Keep != "" {
					// The harness's own home there, and its sign-in by
					// its own name, as every harness's rules take a file.
					protect = append(protect, filepath.Join(subs.Dir, l.Keep), filepath.Join(subs.Dir, l.Keep, l.File))
				}
			}
		}
	}
	// The enrollment token adds the built-in runner of agentbus up, and no
	// other: it is made new at each start, kept in memory and never written,
	// so no other machine can hold it. Every other machine joins by agentbus
	// login, which the person approves; a runner has no token of this kind.
	var enrollToken string
	switch {
	case c.hasQueue():
		if enrollToken, err = newToken(enrollTokenPrefix); err != nil {
			return nil, err
		}
	case c.mode == modeRunner && !signedIn(c.home):
		// Nothing is opened for a machine that never signed in.
		return nil, runner.ErrNotSignedIn
	}
	st, err := store.Open(filepath.Join(c.home, "events.db"))
	if err != nil {
		return nil, err
	}
	// The job queue, in its own file beside the record: the record knows
	// nothing about jobs. Opened after the store, which refuses a second
	// service on this folder.
	var q *queue.Queue
	var runnerJobs *jobs.Server     // nil without a queue
	var logins *credentials.Manager // connects subscriptions, where the queue is
	closeQ := func() {
		if q != nil {
			q.Close()
		}
	}
	// The saved agents live with the queue: where turns are asked for, not
	// where they run.
	var saved *agents.Store
	// The folders this machine holds, its runner's: in up and in a runner,
	// where the folders are. In up the API and the jobs routes hand it the
	// paths they are sent, and keep only references.
	var fr *folders.Registry
	var issuer queue.FolderIssuer // nil, not a nil *folders.Registry, in server
	var bs *blobs.Store           // the control plane's blob store (up, server)
	var blobSweep *jobs.Sweeper   // and what cleans it
	// Every model.responded is priced, and every turn.started of a run's
	// session gets the run's agent and folder, before it is stored.
	sink := cost.Sink{Table: cost.Default(), Next: engine.TurnSink(st, st)}
	// The relay's gateways, which record the runners' calls it carries.
	var relayGws []*gateway.Gateway
	if c.hasEngine() {
		if fr, err = folders.Open(filepath.Join(c.home, "folders.json")); err != nil {
			st.Close()
			return nil, err
		}
		fr.CopiesIn(filepath.Join(c.home, copiesDir), filepath.Join(c.home, oldCopiesDir))
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
		if saved, err = agents.Open(sdb, nil); err != nil {
			st.Close()
			return nil, err
		}
		if q, err = queue.Open(filepath.Join(c.home, "jobs.db"), nil); err != nil {
			st.Close()
			return nil, err
		}
		// Jobs stored before folder references name paths: this machine's
		// folders keep them, and the jobs keep references.
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
		// Runners' model calls get their credential here: an API key from
		// this process's environment, by its variable's name, or the
		// person's own subscription, connected once (credentials).
		orUp := c.openrouter
		if orUp == nil {
			orUp, _ = url.Parse(openrouter)
		}
		// Each records the calls it carries in this record, as a runner's
		// own gateway does (jobs/relay.go). In up, the built-in runner's
		// gateways record each of its calls in this same record already,
		// before they send it to this relay: its calls go through gateways
		// that record nothing (own), and only get the credential here. Any
		// other runner's calls are recorded here, as in server.
		upstream := func(u *url.URL, as ...adapter.Adapter) (gs, own []*gateway.Gateway) {
			for _, a := range as {
				g := gateway.New(u, a, sink)
				gs = append(gs, g)
				relayGws = append(relayGws, g)
				if c.mode == modeUp {
					own = append(own, gateway.New(u, a, unrecorded{}))
				}
			}
			return gs, own
		}
		relayed := func(up jobs.Upstream, as ...adapter.Adapter) jobs.Upstream {
			up.Gateways, up.Own = upstream(up.URL, as...)
			return up
		}
		relay := map[string]jobs.Upstream{
			relayAnthropic: relayed(jobs.Upstream{URL: c.upstream, Access: credentials.AccessSubscription, Harness: "claude"}, anthropic.Adapter{}),
			relayChatGPT:   relayed(jobs.Upstream{URL: c.chatgpt, Access: credentials.AccessSubscription, Harness: "codex"}, openai.Adapter{}),
			// agy's Code Assist methods only: the Google sign-in reaches
			// nothing else of that host.
			relayCloudCode: relayed(jobs.Upstream{URL: c.cloudCodeOr(), Access: credentials.AccessSubscription, Harness: "agy", Under: "v1internal:"},
				gemini.Adapter{}),
			// OpenRouter takes both API formats: the Responses one's calls
			// go to the openai adapter, any other to the anthropic one.
			relayOpenRouter: relayed(jobs.Upstream{URL: orUp, Access: credentials.AccessOpenRouter}, openai.Adapter{}, anthropic.Adapter{}),
		}
		creds := credentials.Plug{credentials.AccessOpenRouter: credentials.Env{Getenv: c.getenvOr()}, credentials.AccessSubscription: subs}
		js, err := jobs.New(jobs.Config{Queue: q, Token: token, EnrollToken: enrollToken, Heartbeat: c.heartbeat, Agents: saved, Settings: cfgStore,
			Folders: issuer, Blobs: bs, Sweeper: blobSweep, Record: st, Relay: relay, Credentials: creds})
		if err != nil {
			closeQ()
			st.Close()
			return nil, err
		}
		runnerJobs = js
		logins = subscriptionLogins(c, subs)
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
	if sdb != nil {
		if upgraded.Done {
			svcLog.Printf("settings.imported copy=%q files=%q", upgraded.Copy, strings.Join(upgraded.Imported, ","))
		}
		// Each settings change waits in settings.db until the record has
		// its event; those from before this start go now.
		sdb.SetSink(api.RecordSettings(st), svcLog.Printf)
	}
	if runnerJobs != nil {
		runnerJobs.Logf = svcLog.Printf
	}
	if logins != nil {
		logins.Logf = svcLog.Printf
		logins.Ended = func(h, state, why string) {
			e := event.Event{ID: queue.NewID("evt_"), Kind: event.SubscriptionFailed, Time: time.Now().UTC(), Harness: h, Error: why}
			if state == credentials.Connected {
				e.Kind, e.Error = event.SubscriptionConnected, ""
			}
			if _, _, err := st.Append(e); err != nil {
				svcLog.Printf("store.error what=%s", e.Kind)
			}
		}
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
		// A runner that holds no secrets, agentbus up's built-in one too,
		// runs each session in a home of its own: its agents' credentials
		// come from control, so nothing of this user's home is needed. A
		// runner with -gateway local uses the harness's own sign-in, which
		// lives in this user's home, so its turns run there.
		homes := ""
		if c.noSecrets() {
			homes = filepath.Join(c.home, homesDir)
		}
		eng, err = engine.New(engine.Config{Store: st, BaseURL: baseURL, MaxRuns: c.maxRuns, ReplayBase: replayBase, AgentName: agentName, Homes: homes,
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
	acfg := api.Config{Store: st, Token: token, TurnsToken: turnsToken, Port: port, AnyHost: !loopback,
		BaseURL: baseURL, Origin: c.origin, Hosts: c.hosts, Environ: c.environ, Folders: issuer, Build: build(debug.ReadBuildInfo()),
		Queue: q, Jobs: runnerJobs, Engine: eng, TurnWait: c.turnWait, Agents: saved, Settings: cfgStore, Subscriptions: logins,
		Logf: svcLog.Printf}
	var relayHandler http.Handler
	if runnerJobs != nil {
		acfg.RunnerTokens, relayHandler = runnerJobs.TurnGrant, runnerJobs.Relay()
	}
	a, err := api.New(acfg)
	if err != nil {
		ln.Close()
		svcLog.Close()
		st.Close()
		closeQ()
		return nil, err
	}
	anthURL, chatURL, orURL, ccURL := c.upstream, c.chatgpt, c.openrouter, c.cloudCodeOr()
	if orURL == nil {
		orURL, _ = url.Parse(openrouter)
	}
	// A runner that holds no secret sends each call on to the server's
	// relay, which adds the credential and records the call in control's
	// record (jobs/relay.go). It records the call here too, for its own
	// record, and names each call's exchange to the relay. agentbus up sends
	// each call to its own relay, on its own address: one record, written
	// here, so no exchange is named.
	if c.noSecrets() {
		srv := strings.TrimSuffix(c.server, "/") + jobs.RelayPrefix
		if c.mode == modeUp {
			srv = baseURL + jobs.RelayPrefix
		}
		anthURL, orURL = mustParse(srv+relayAnthropic), mustParse(srv+relayOpenRouter)
		chatURL, ccURL = mustParse(srv+relayChatGPT), mustParse(srv+relayCloudCode)
	}
	anth := gateway.New(anthURL, anthropic.Adapter{}, sink)
	oai := gateway.New(chatURL, openai.Adapter{}, sink)
	// agy's Code Assist calls, on its own Google sign-in: on this machine's
	// sign-in, or, on a runner that holds no secret, the one connected on
	// control, which the relay adds.
	gem := gateway.New(ccURL, gemini.Adapter{}, sink)
	gws := []*gateway.Gateway{anth, oai, gem}
	// An OpenRouter agent's calls, under /openrouter of its base URL: one
	// gateway per API format, both to OpenRouter.
	orAnth := gateway.New(orURL, anthropic.Adapter{}, sink)
	orOAI := gateway.New(orURL, openai.Adapter{}, sink)
	orAnth.SetPrefix(openRouterPrefix)
	orOAI.SetPrefix(openRouterPrefix)
	gws = append(gws, orAnth, orOAI)
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
	for _, g := range relayGws {
		g.KeepBodies(keep)
		g.SetLog(svcLog.Printf)
	}
	for _, g := range gws {
		g.KeepBodies(keep)
		g.SetLog(svcLog.Printf)
		if c.noSecrets() && c.mode == modeRunner {
			g.SendExchange()
		}
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
	s := &service{addr: addr, shown: net.JoinHostPort(host, port), loopback: loopback, gws: append(gws, relayGws...), api: a, eng: eng, store: st, blobs: bs, blobSwp: blobSweep, jobsSrv: runnerJobs,
		queue: q, saved: saved, remote: remote, settings: cfgStore, sdb: sdb, folders: fr, log: svcLog, logins: logins, done: make(chan error, 1)}
	sdbHanded = true
	if q != nil {
		s.subs = subs
	}
	s.srv = &http.Server{
		Handler:           logRequests(svcLog, route(a, vendors{anthropic: anth, openai: oai, openrouterAnth: orAnth, openrouterResps: orOAI, gemini: handler(gem), relay: relayHandler}, !loopback)),
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
	// The sweeps that run beside the queue's, each on its own clock.
	var beside sync.WaitGroup
	// Control's one finisher: it alone writes each saved finish's outcome
	// and then ends its job (jobs/finisher.go). It runs until the runner
	// has stopped, since the runner finishes its jobs through the API.
	if s.jobsSrv != nil {
		beside.Add(1)
		go func() {
			defer beside.Done()
			s.jobsSrv.RunFinisher(stop)
		}()
	}
	if s.blobSwp != nil {
		beside.Add(1)
		go func() { defer beside.Done(); s.blobSwp.Run(ctx) }()
	}
	// Control renews an OAuth sign-in before it expires, as the vendor's own
	// CLI would, so one no turn used still works. On its own clock: a
	// renewal waits on the vendor, and the queue's sweep must not.
	if s.subs != nil {
		beside.Add(1)
		go func() {
			defer beside.Done()
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					for h, err := range s.subs.RenewDue() {
						if err != nil {
							svcLog.Printf("subscription.renew_failed harness=%s why=%q", h, err.Error())
						} else {
							svcLog.Printf("subscription.renewed harness=%s", h)
						}
					}
				}
			}
		}()
	}
	go func() {
		defer close(s.sweepDone)
		defer beside.Wait()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var presence jobs.Presence
		tick := 0
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
				// A remote turn whose runner was lost: its hold in the
				// record is closed, so the session takes its next turn.
				if n, err := jobs.CloseLost(s.queue, s.store, time.Now().UTC()); err != nil {
					svcLog.Printf("record.close_lost_failed")
				} else if n > 0 {
					svcLog.Printf("record.lost_turns_closed n=%d", n)
				}
				// An apply whose runner was lost: its turn.applied, which
				// no finish wrote.
				if n, err := jobs.CloseLostApplies(s.queue, s.store); err != nil {
					svcLog.Printf("record.lost_applies_failed")
				} else if n > 0 {
					svcLog.Printf("record.lost_applies_closed n=%d", n)
				}
				// A runner that stopped answering, or came back.
				if tick++; tick%10 == 0 {
					gone, back, err := presence.Check(s.queue, time.Now().UTC())
					if err != nil {
						svcLog.Printf("queue.runners_failed")
					}
					for _, r := range gone {
						svcLog.Printf("runner.gone id=%s name=%q seen=%s", r.ID, r.Name, r.Seen.Format(time.RFC3339))
						if _, _, err := s.store.Append(event.Event{ID: queue.NewID("evt_"), Kind: event.RunnerGone, Time: time.Now().UTC(), Runner: r.ID}); err != nil {
							svcLog.Printf("store.error what=%s", event.RunnerGone)
						}
					}
					for _, r := range back {
						svcLog.Printf("runner.back id=%s name=%q", r.ID, r.Name)
					}
				}
				// Asked work that ended: its result becomes the asking
				// session's next turn (queue.Deliver).
				if n, err := s.queue.Deliver(queue.DefaultTeam, name, s.deliveredAgent); err != nil {
					svcLog.Printf("queue.deliver_failed")
				} else if n > 0 {
					svcLog.Printf("queue.results_delivered n=%d", n)
				}
			}
		}
	}()
}

// startRunner enrolls the built-in runner and starts it, holding this
// machine's folders: it makes each session's copy from its folder and does
// the folders' jobs (applies, typed folders) itself, so one program runs on
// the machine. In up it shares the control plane's blob store on disk, so
// a copy is made by clone or overlay and no file is sent over HTTP. A runner
// of a server keeps its own store, with the server's behind it.
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
		Workdir: filepath.Join(c.home, copiesDir), OldWorkdir: filepath.Join(c.home, oldCopiesDir), StateFile: filepath.Join(c.home, runnerState), Settings: s.settings,
		Forward: c.mode == modeRunner, NoSecrets: c.noSecrets(), Sandbox: c.canSandbox}
	// A runner of a server keeps its own store, which its folder side
	// shares: one handle, so its sweep and the folder side's touches share
	// one lock.
	store := s.blobs
	if store != nil {
		cfg.Blobs, cfg.BlobSink = s.blobs, s.blobs
	} else {
		var err error
		if store, err = blobs.Open(filepath.Join(c.home, cacheDir)); err != nil {
			return fail(err)
		}
	}
	fs := folderSide(c.home, s.folders, store)
	cfg.Folders, cfg.FolderSide, cfg.Lender = s.folders, connector.Jobs{Connector: fs}, fs
	rn, err := runner.Enroll(ctx, cfg)
	if err != nil {
		return fail(fmt.Errorf("the runner could not enroll with %s: %w", self, err))
	}
	if s.blobs == nil {
		client := blobClient(self, rn.Token())
		fs.Remote = client
		rn.UseBlobs(blobs.Local{Store: store, From: client}, client)
		go sweepCache(ctx, store, svcLog.Printf)
	}
	if s.remote != nil {
		s.remote.Enrolled(rn.ID(), rn.Token())
	}
	// up's own runner: its gateways record its calls, so the relay does not
	// again (jobs.Upstream.Own).
	if c.mode == modeUp && s.jobsSrv != nil {
		s.jobsSrv.SetOwnRunner(rn.ID())
	}
	svcLog.Printf("runner.start id=%s capacity=%d machine=%s folders=%d", rn.ID(), capacity, s.folders.Connector(), len(s.folders.List(queue.MaxFolders)))
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

// deliveredAgent is the agent a delivered result's turn runs on: the asking
// turn's saved agent as it is now (or its harness, for a turn that named one
// by harness), so every turn names its agent and reads it once. A removed one
// says so.
func (s *service) deliveredAgent(asking queue.Spec) (queue.Spec, string) {
	id := asking.AgentID
	if id == "" {
		id = asking.Harness
	}
	if s.saved == nil {
		return asking, ""
	}
	ag, err := s.saved.Resolve(id)
	switch {
	case errors.Is(err, agents.ErrRemoved):
		return queue.Spec{}, "the asking turn's agent " + strconv.Quote(id) + " was removed, so its result was not delivered"
	case err != nil:
		return queue.Spec{}, "the asking turn's agent " + strconv.Quote(id) + " is not a saved agent, so its result was not delivered"
	}
	return queue.Spec{Harness: ag.Harness, AgentID: ag.ID, Access: ag.Access, KeyEnv: ag.KeyEnv, AgentModel: ag.Model}, ""
}

// unrecorded is the sink of up's relay gateways for its own runner's calls
// (jobs.Upstream.Own): that runner's gateways record each of them in up's
// one record.
type unrecorded struct{}

func (unrecorded) Emit(event.Event) {}

// The relay's names for the vendors it adds credentials for: under
// jobs.RelayPrefix of the server.
const (
	relayAnthropic  = "anthropic"
	relayChatGPT    = "chatgpt"
	relayCloudCode  = "cloudcode"
	relayOpenRouter = "openrouter"
)

// cloudCodeOr is c.cloudCode, or agy's own Code Assist host.
func (c config) cloudCodeOr() *url.URL {
	if c.cloudCode != nil {
		return c.cloudCode
	}
	return mustParse(cloudCode)
}

// subscriptionsDir is where control keeps connected subscriptions, in its
// data folder.
const subscriptionsDir = "subscriptions"

// homesDir is where a runner that holds no secrets keeps each session's own
// home, in its data folder (engine.Config.Homes).
const homesDir = "homes"

// copiesDir holds a runner's isolated copies, each named after its session.
// oldCopiesDir is where builds before 0.4 made them, named after a job: a
// runner still finds, uses and removes those there.
const (
	copiesDir    = "sessions"
	oldCopiesDir = "worktrees"
)

// getenvOr is c.getenv, or the process's own environment.
func (c config) getenvOr() func(string) string {
	if c.getenv != nil {
		return c.getenv
	}
	return os.Getenv
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// starters are the agents a new data folder starts with: in up, one for each
// harness its built-in runner finds, as that runner reports them; in server,
// none, since no machine is there yet. The person adds agents from what the
// runners report.
func starters(c config) func() []settings.AgentRow {
	return func() []settings.AgentRow {
		if c.mode != modeUp {
			return agents.Starters(nil)
		}
		look := c.lookPath
		if look == nil {
			look = exec.LookPath
		}
		return agents.Starters(harness.Found(look))
	}
}

// subscriptionStore is control's connected subscriptions in home, with how
// each harness's OAuth sign-in is renewed (harness.Login): by control, or by
// the harness itself in its own home on control, found on PATH.
func subscriptionStore(home string) *credentials.Subscriptions {
	s := &credentials.Subscriptions{Dir: filepath.Join(home, subscriptionsDir), OAuth: map[string]credentials.OAuth{}, Own: map[string]credentials.Own{}}
	for _, h := range harness.All() {
		l := h.Info().Login
		switch {
		case l == nil:
		case l.Keep != "":
			path, err := exec.LookPath(h.Info().Name)
			if err != nil {
				path = h.Info().Name // the renewal says it could not start
			}
			o := credentials.Own{Home: l.Keep, File: l.File, TokenKey: l.TokenKey, ExpiryKey: l.ExpiryKey,
				Within: time.Duration(l.RenewWithinMinutes) * time.Minute, Path: path, Args: l.RenewArgs, Unset: l.Unset, Set: l.Set}
			if l.RenewRefused != "" {
				o.Refused = regexp.MustCompile(l.RenewRefused)
			}
			s.Own[h.Info().Name] = o
		case l.RenewURL != "":
			s.OAuth[h.Info().Name] = credentials.OAuth{URL: l.RenewURL, ClientID: l.ClientID, AccountHeader: l.AccountHeader}
		}
	}
	return s
}

// subscriptionLogins are the harnesses' own logins, for connecting a
// subscription on control (harness.Login).
func subscriptionLogins(c config, subs *credentials.Subscriptions) *credentials.Manager {
	look := c.lookPath
	if look == nil {
		look = exec.LookPath
	}
	m := &credentials.Manager{Store: subs, Logins: map[string]credentials.Login{}}
	for _, h := range harness.All() {
		l := h.Info().Login
		if l == nil {
			continue
		}
		path, err := look(h.Info().Name)
		if err != nil {
			path = h.Info().Name // the connect says it could not start
		}
		login := credentials.Login{Path: path, Args: l.Args, Prefix: l.Prefix, Valid: time.Duration(l.ValidDays) * 24 * time.Hour,
			Home: l.Home, File: l.File, TokenKey: l.TokenKey, RefreshKey: l.RefreshKey, AccountKey: l.AccountKey,
			Keep: l.Keep, ExpiryKey: l.ExpiryKey, Unset: l.Unset, Set: l.Set, OnlyOn: l.OnlyOn, OnlyOnWhy: l.OnlyOnWhy}
		if l.Token != "" {
			login.Token = regexp.MustCompile(l.Token)
		}
		if l.Code != "" {
			login.Code = regexp.MustCompile(l.Code)
		}
		if l.Refused != "" {
			login.Refused = regexp.MustCompile(l.Refused)
		}
		m.Logins[h.Info().Name] = login
	}
	return m
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
	// relay takes runners' model calls and adds their credential, under
	// jobs.RelayPrefix; nil without a queue. A runner's turn token guards
	// it, so it answers callers on other machines.
	relay http.Handler
	// gemini is agy's Code Assist gateway: to Google, or on a runner that
	// holds no secret, to control's relay. Nil: none.
	gemini http.Handler
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
		if v.relay != nil && strings.HasPrefix(r.URL.Path, jobs.RelayPrefix) {
			v.relay.ServeHTTP(w, r)
			return
		}
		_, _, rest := gateway.SplitSession(r.URL.EscapedPath())
		var gw http.Handler
		switch {
		case under(rest, "/backend-api/codex"):
			gw = v.openai
		case strings.HasPrefix(rest, "/v1internal:") && v.gemini != nil:
			// Code Assist's methods are a colon after the version, such
			// as /v1internal:streamGenerateContent.
			gw = v.gemini
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
	if s.logins != nil {
		s.logins.Stop()
	}
	if s.queue != nil {
		_ = s.queue.Close()
	}
	if s.sdb != nil {
		_ = s.sdb.Close()
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

// enrollTokenPrefix marks the enrollment token, which adds the built-in
// runner.
const enrollTokenPrefix = "abe_"

// newToken is a new token, prefix and 64 hex characters, kept nowhere.
func newToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("the token could not be made")
	}
	return prefix + hex.EncodeToString(b), nil
}

// loadToken reads a token, or makes it on first start with mode 0600: prefix
// and 64 hex characters. Its value is never printed or put in an error.
func loadToken(path, prefix string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		tok, err := newToken(prefix)
		if err != nil {
			f.Close()
			return "", err
		}
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
// networkNote is what a runner says at start about its agents' network.
func networkNote() string {
	if !netjail.Supported {
		return "network control is Linux-only: on this runner, agents reach any host, and a turn under [network] contain = \"on\" is refused"
	}
	if err := netjail.Check(); err != nil {
		return err.Error() + "; a turn under [network] contain = \"on\" is refused until that is fixed here"
	}
	return "network control is ready: with network.contain = \"on\" in the server's permissions, each agent reaches only this runner's gateway, the server, and the hosts in [network] allow"
}

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

// handler is g as an http.Handler, or nil when there is no g: a nil
// *gateway.Gateway in an interface would not be nil.
func handler(g *gateway.Gateway) http.Handler {
	if g == nil {
		return nil
	}
	return g
}
