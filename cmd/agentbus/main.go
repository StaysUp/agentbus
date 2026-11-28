// Command agentbus runs the framework on this machine.
//
//	agentbus up [-listen 127.0.0.1:8787] [-origin http://localhost:5173]
//	agentbus server [-listen 127.0.0.1:8787]
//	agentbus runner -server URL
//	agentbus doctor
//	agentbus version
//
// modes.go says what each mode runs. up starts one service on one address, 127.0.0.1:8787 unless -listen says
// another, such as 0.0.0.0:8787 in a container. /api and /s/<session>/api go
// to the client API, which needs the token in the data folder. Vendor paths go
// to that vendor's gateway, which forwards them and stores their events:
// /backend-api/codex to chatgpt.com (codex), /v1 to api.anthropic.com (Claude
// Code), /v1internal: to daily-cloudcode-pa.googleapis.com (agy), each with or
// without /s/<session> in front. In up and server, any other GET is the
// cockpit's (package cockpit): its files, and its page at / and its own
// routes; a runner redirects GET / to /docs. Any other path is answered 404
// here and sent nowhere. The vendor paths answer only callers on this
// machine, whatever the address.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/cockpit"
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
	"github.com/staysup/agentbus/adapter/chatcompletions"
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
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/logfile"
	"github.com/staysup/agentbus/models"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/replay"
	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/runner"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/toolreg"
	"github.com/staysup/agentbus/wall"
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
	if args[0] == "doctor" {
		return runDoctor(args[1:], home, getenv, stdout, stderr)
	}
	if args[0] == "login" {
		return runLogin(args[1:], home, stdout, stderr, systemLogin())
	}
	c, ok := parse(args, home, stderr)
	if !ok {
		return 2
	}
	c.home, c.logf, c.self = home, own.Printf, self()
	c.spawn = c.mode == modeUp
	if c.mode == modeRunner {
		c.builtin = readBuiltinEnv(getenv)
	}
	c.upstream, _ = url.Parse(upstream)
	c.chatgpt, _ = url.Parse(chatgpt)
	// A test points control's model upstream at a fake vendor on loopback, so a
	// two-process test can make the gateway guard fire on a real reply. Read
	// only from the environment, never a flag or in any help, usage or doc, and
	// only a loopback address is taken: any other refuses the start, so a real
	// sign-in or key can never be sent to another host.
	if u := getenv("AGENTBUS_TEST_UPSTREAM"); u != "" {
		tu, err := url.Parse(u)
		if err != nil || (tu.Scheme != "http" && tu.Scheme != "https") || tu.Host == "" || !loopbackHost(tu.Hostname()) {
			own.Printf("AGENTBUS_TEST_UPSTREAM must be a loopback http address")
			return 1
		}
		c.upstream = tu
	}
	// The same for codex's ChatGPT upstream, so a two-process test can point
	// both vendors at fakes and prove each call reaches only its own.
	if u := getenv("AGENTBUS_TEST_CHATGPT_UPSTREAM"); u != "" {
		tu, err := url.Parse(u)
		if err != nil || (tu.Scheme != "http" && tu.Scheme != "https") || tu.Host == "" || !loopbackHost(tu.Hostname()) {
			own.Printf("AGENTBUS_TEST_CHATGPT_UPSTREAM must be a loopback http address")
			return 1
		}
		c.chatgpt = tu
	}
	// Env only, like the test upstreams: a two-process test shortens the Ask
	// hold so its timeout path does not take a real minute.
	// Env only, like the hold: a cross-process test holds a runner's taken job
	// before its start, so it can stop control in between.
	if ms := getenv("AGENTBUS_TEST_START_DELAY_MS"); ms != "" {
		n, err := strconv.Atoi(ms)
		if err != nil || n <= 0 {
			own.Printf("AGENTBUS_TEST_START_DELAY_MS must be a positive number of milliseconds")
			return 1
		}
		c.startDelay = time.Duration(n) * time.Millisecond
	}
	if ms := getenv("AGENTBUS_TEST_HOLD_MS"); ms != "" {
		n, err := strconv.Atoi(ms)
		if err != nil || n <= 0 {
			own.Printf("AGENTBUS_TEST_HOLD_MS must be a positive number of milliseconds")
			return 1
		}
		c.holdMS = n
	}
	// Before start, which makes the folder.
	fresh := freshFolder(home)
	svc, err := start(c)
	if err != nil {
		own.Print(err)
		return 1
	}
	switch {
	case c.mode == modeRunner:
		// One line: a runner has nothing for the person to do at start. A
		// turn it cannot run (no network control here, say) says why itself.
		own.Printf("runner ready, taking jobs from %s", c.server)
	case svc.loopback:
		own.Printf("listening on http://%s, data folder %s", svc.addr, home)
	default:
		own.Printf("listening on http://%s, data folder %s; reachable from the network; the token guards the API; no HTTPS", svc.shown, home)
	}
	if c.mode == modeUp {
		fmt.Fprintf(stdout, "\n%s\n", svc.dataFolderLine(home, fresh))
	}
	switch {
	case c.mode == modeRunner:
	case !cockpit.Packed():
		// No cockpit to sign in to: no code is made.
		fmt.Fprintf(stdout, "\n%s\n\n", notPackedLine(svc.addr))
	default:
		// The cockpit's one-time sign-in link: printed, never the token.
		link := signInLink(svc.addr, svc.api.NewSignInCode())
		fmt.Fprintf(stdout, "\nOpen the cockpit (the link works once, for %d minutes; start agentbus up again for a new one):\n  %s\n\n",
			int(api.SignInTTL.Minutes()), link)
		if c.mode == modeUp && !c.noOpen {
			openBrowser(home, link, getenv)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	// up's built-in runner leaves on its own once it retired (builtin.go).
	var retired <-chan struct{}
	if svc.retire != nil {
		retired = svc.runnerDone
	}
	select {
	case <-sig:
	case <-retired:
		own.Printf("the built-in runner retired")
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
	// holdMS, when > 0, shortens the Ask hold's timeout to that many ms (its
	// ping and poll scaled down with it), so a two-process test can exercise the
	// hold's timeout without a real minute. Env only (AGENTBUS_TEST_HOLD_MS),
	// never a flag or in any help or doc, like the test upstreams above.
	holdMS int
	// startDelay, test only (AGENTBUS_TEST_START_DELAY_MS): a runner holds
	// each taken job this long before its start.
	startDelay time.Duration
	// cloudCode is agy's Code Assist host; nil means cloudCode above.
	cloudCode *url.URL
	// openrouter is OpenRouter's API; nil means openrouter above.
	openrouter *url.URL
	// apiKeys are the vendor APIs of access "api", by harness; a harness
	// absent here uses its own (harness.APIKey.URL).
	apiKeys map[string]*url.URL
	// providers are the providers' APIs, by name; a provider not here uses
	// its own (harness.Providers). Tests point them at fakes.
	providers map[string]*url.URL
	logf      func(string, ...any)
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
	// noOpen: agentbus up without -open (the default), which prints the sign-in link and
	// opens no browser.
	noOpen bool
	// noReplay turns own replay off: claude's and codex's next turns use the
	// vendor's resume. The zero value, replay on, is the default.
	noReplay bool
	// capacity is a runner's -capacity: the most delegated turns its machine
	// runs at once. Zero means the settings decide (runner.Config.Capacity).
	capacity int
	// For tests: how often a runner heartbeats, and how long POST
	// /api/v0/turns waits for a turn to start. Zero means the defaults.
	heartbeat, turnWait time.Duration

	// mode is up (the zero value), server or runner (modes.go).
	mode string
	// spawn: up runs its built-in runner as a process of its own (the
	// command does; a test of up in this process does not). builtin marks a
	// runner as up's built-in one (builtin.go).
	spawn   bool
	builtin *builtinRunner
	// A runner's: the server's URL, the runner's name, and where its agents
	// send model calls.
	server, runnerName string
	// A runner's folders it owns (-folder), and its labels (-labels).
	folders, labels []string
}

// controlWait is how long a runner's gateway holds a model call while control's
// relay refuses the connection (gateway.SetUpstreamWait): long enough for a
// restart of control, short of the agents' own request timeouts.
const controlWait = 2 * time.Minute

// lazyDecider is the gateways' Ask-hold Decider, set on each guarded gateway
// before serving (as SetGuard is) and filled in once the runner that backs it
// is enrolled (startRunner). It satisfies gateway.Decider; the lock keeps the
// gateway's read race-free with that later write. Nil (before the runner is up)
// reads as "not yet decided", so a hold keeps holding rather than acting.
type lazyDecider struct {
	mu sync.RWMutex
	d  gateway.Decider
}

func (l *lazyDecider) set(d gateway.Decider) {
	l.mu.Lock()
	l.d = d
	l.mu.Unlock()
}

func (l *lazyDecider) Decided(ctx context.Context, turn, approval string) (bool, bool, error) {
	l.mu.RLock()
	d := l.d
	l.mu.RUnlock()
	if d == nil {
		return false, false, nil
	}
	return d.Decided(ctx, turn, approval)
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

	// holdDecider is the Ask-hold Decider set on the guarded gateways before
	// serving; startRunner fills in the runner that backs it once it is up
	// (gateway/hold.go, runner/decide.go). guardedGws are those gateways, so
	// startRunner can also give them the runner's approval-id scope.
	holdDecider *lazyDecider
	gate        *turnGate // the runner's gateways' turn check, set once it enrolled
	guardedGws  []*gateway.Gateway

	// The built-in runner, and the lease sweep: stopped before the API.
	stopRunner func()
	runnerDone chan struct{}
	// up's built-in runner (builtin.go): in this process for a test
	// (inRunner), else watched as a process of its own until up stops
	// (stopSupervise). retire is the built-in runner's own Config.Retire,
	// when it runs as that process.
	inRunner      *service
	stopSupervise func()
	retire        chan struct{}
	stopSweep     func()
	subs          *credentials.Subscriptions // control's connected subscriptions; nil without a queue
	sweepDone     chan struct{}
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
			Skip: []string{copiesDir, oldCopiesDir, "blobs"}, Agents: agents.ImportRows, Starters: agents.Starters}.Run()
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
	// up's built-in runner too, which runs in up's data folder.
	if c.hasQueue() || c.builtin != nil {
		protect = append(protect, subscriptionsProtected(subs)...)
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
	case c.builtin != nil:
		// up's built-in runner: up's token, handed over at its start
		// (builtin.go). A runner that enrolled keeps its identity.
		enrollToken = c.builtin.enroll
	case c.mode == modeRunner && !signedIn(c.home):
		// Nothing is opened for a machine that never signed in.
		return nil, runner.ErrNotSignedIn
	}
	st, err := store.Open(c.storePath())
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
	// A gateway or hook session's direct workspace is born in the TurnSink; it
	// carries this service's own machine (the folder registry's), read lazily
	// because fr opens below. "" on a server with no folders, which is right.
	sink := cost.Sink{Table: cost.Default(), Next: engine.TurnSinkMachine(st, st, func() string {
		if fr != nil {
			return fr.Machine()
		}
		return ""
	})}
	// The relay's gateways, which record the runners' calls it carries.
	var relayGws []*gateway.Gateway
	if c.hasEngine() || c.mode == modeUp {
		// up and its built-in runner, two processes, share the file (folders/).
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
		// Every existing session gets a workspace.created, so a workspace's
		// folder, machine and sandbox mode have one source from the first
		// start of this build. The session's own id becomes its workspace id,
		// so its on-disk copy and checkpoints keep resolving without a rename.
		// The queue's sessions give the sandbox mode (a copy or worktree is a
		// sandbox); a session with no queue row -- hook or gateway traffic --
		// is a direct workspace. Idempotent: a migrated session is skipped.
		if sessions, err := q.Sessions(queue.DefaultTeam, 1<<30); err != nil {
			closeQ()
			st.Close()
			return nil, err
		} else {
			seeds := make([]store.WorkspaceSeed, 0, len(sessions))
			for _, w := range sessions {
				// The queue holds the authoritative folder reference and
				// machine of the session (the same SessionFolder askChecks
				// reads); the folder registry resolves the reference to the
				// person's real folder when this machine owns it. The
				// session's own events are not used: for an isolated session
				// they name the runner's scratch copy path, not the folder.
				ref, machine, _, err := q.SessionFolder(queue.DefaultTeam, w.Session)
				if err != nil {
					closeQ()
					st.Close()
					return nil, err
				}
				folder := queue.DisplayFolder(issuer, ref, machine)
				seeds = append(seeds, store.WorkspaceSeed{
					Session:   w.Session,
					Sandbox:   w.Kind == queue.WorkspaceCopy || w.Kind == queue.WorkspaceWorktree,
					Machine:   machine,
					FolderRef: ref,
					Folder:    folder,
				})
			}
			if _, err := st.MigrateWorkspaces(seeds); err != nil {
				closeQ()
				st.Close()
				return nil, err
			}
		}
		// A workspace recorded before 0.6.6 has part of its applied state in the
		// queue's job rows only: snapshot it into the record, once, before the
		// finisher runs, so the record alone answers it (jobs.MigrateApplied).
		if _, err := jobs.MigrateApplied(st, q); err != nil {
			closeQ()
			st.Close()
			return nil, err
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
		// Runners' model calls get their credential here: an API key set
		// in Settings, or else from this process's environment by its
		// variable's name, or the person's own subscription, connected
		// once (credentials).
		orUp := c.openrouter
		if orUp == nil {
			orUp, _ = url.Parse(openrouter)
		}
		// Each records the calls it carries in this record, as a runner's
		// own gateway does (jobs/relay.go). It records every call it
		// carries: in up too, where only other machines' runners use it,
		// since up's own gateways get the credential in this process.
		relayed := func(up jobs.Upstream, as ...adapter.Adapter) jobs.Upstream {
			for _, a := range as {
				g := gateway.New(up.URL, a, sink)
				up.Gateways = append(up.Gateways, g)
				relayGws = append(relayGws, g)
			}
			return up
		}
		relay := map[string]jobs.Upstream{
			relayAnthropic: relayed(jobs.Upstream{URL: c.upstream, Access: credentials.AccessSubscription, Harness: "claude"}, anthropic.Adapter{}),
			relayChatGPT:   relayed(jobs.Upstream{URL: c.chatgpt, Access: credentials.AccessSubscription, Harness: "codex"}, openai.Adapter{}),
			// agy's Code Assist methods only: the Google sign-in reaches
			// nothing else of that host.
			relayCloudCode: relayed(jobs.Upstream{URL: c.cloudCodeOr(), Access: credentials.AccessSubscription, Harness: "agy", Under: "v1internal:"},
				gemini.Adapter{}),
			// OpenRouter takes three API formats: each call goes to the
			// adapter that records it (jobs.gatewayFor), the Responses API's
			// to openai, Chat Completions' to chatcompletions (opencode), the
			// Messages API's to anthropic.
			relayOpenRouter: relayed(jobs.Upstream{URL: orUp, Access: credentials.AccessOpenRouter}, openai.Adapter{}, chatcompletions.Adapter{}, anthropic.Adapter{}),
		}
		// Each vendor's API, for agents on its API key set in Settings
		// (harness.Info.APIKey): their calls go to that vendor only.
		apiKeys := map[string]credentials.VendorKey{}
		for _, h := range harness.All() {
			if k := h.Info().APIKey; k != nil {
				relay[relayAPIKey(h.Info().Name)] = relayed(jobs.Upstream{URL: c.apiKeyURL(h.Info()), Access: credentials.AccessAPI, Harness: h.Info().Name}, formatAdapter(k.Format))
				apiKeys[h.Info().Name] = credentials.VendorKey{Name: k.Name, Label: k.Label, Header: k.Header}
			}
		}
		// An OpenRouter key set in Settings, kept with the subscriptions,
		// wins over the variable an agent names.
		creds := credentials.Plug{credentials.AccessOpenRouter: credentials.KeyFirst{Kept: subs, Env: credentials.Env{Getenv: c.getenvOr()}},
			credentials.AccessSubscription: subs, credentials.AccessAPI: credentials.APIKeys{Kept: subs, Of: apiKeys}}
		// Each provider's Chat Completions API (harness.Providers), with its
		// key set in Settings.
		for _, p := range harness.Providers {
			relay[relayProvider(p.Name)] = relayed(jobs.Upstream{URL: c.providerURL(p), Access: p.Name}, chatcompletions.Adapter{})
			creds[p.Name] = credentials.ProviderKey{Kept: subs, Key: p.Key, Label: p.KeyLabel}
		}
		js, err := jobs.New(jobs.Config{Queue: q, Token: token, EnrollToken: enrollToken, Version: build(debug.ReadBuildInfo()).Version,
			Heartbeat: c.heartbeat, Agents: saved, Settings: cfgStore,
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
	svcLog, err := logfile.Open(filepath.Join(logs, c.logName()), c.logMax, nil)
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
		// The store's lock is held, so no login of another control runs
		// here: a login folder now is one a stopped control left.
		if n := logins.RemoveLeftLogins(); n > 0 {
			svcLog.Printf("subscriptions.left_logins_removed n=%d", n)
		}
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
		return nil, listenError(c.listen, err)
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
	// Own replay: claude's, codex's and opencode's next turns start fresh, and
	// each vendor's gateway puts the earlier turn's history in front of their
	// requests.
	var claudeReplay, codexReplay, chatReplay *replay.Replayer
	var replayBase func(string, string) (engine.ReplayPlan, error)
	if !c.noReplay {
		claudeReplay, codexReplay, chatReplay = replay.New(st), replay.NewCodex(st), replay.NewChatCompletions(st)
		// The history budget is the person's setting on control (the budget
		// of GET /api/v0/settings/compaction), read at each turn's start: from
		// control's database, or on a runner from the server (settings.Remote),
		// so every runner keeps the same budget.
		budget := settings.BudgetOf(cfgStore)
		for _, r := range []*replay.Replayer{claudeReplay, codexReplay, chatReplay} {
			r.SetBudget(budget)
		}
		replayBase = func(agent, session string) (engine.ReplayPlan, error) {
			// Each harness names the history format own replay uses for it.
			r := claudeReplay
			if h, ok := harness.Get(agent); ok {
				switch h.Info().Replay {
				case harness.FormatOpenAI:
					r = codexReplay
				case harness.FormatChatCompletions:
					r = chatReplay
				}
			}
			return replayPlan(r.Base(session))
		}
	}
	baseURL := "http://" + agentAddr(ln.Addr())
	// The engine starts each turn's agent, on this machine. Only a runner
	// calls it, for the jobs it takes from the queue: the built-in one of up,
	// or this process's own in runner mode. A server runs no turns.
	var eng *engine.Engine
	// The runner's gateways' turn check, and the agents' names it was given:
	// set once the runner enrolled (startRunner).
	gate := &turnGate{}
	if c.hasEngine() {
		// A turn on another harness than its session's latest names the
		// earlier turns' agents by their saved names, where this process
		// keeps them.
		// A runner keeps no saved agents: it names them as control did with
		// the turn's start (runner.AgentName), once it enrolled.
		agentName := func(id string) string {
			gate.mu.RLock()
			rn := gate.rn
			gate.mu.RUnlock()
			if rn == nil {
				return ""
			}
			return rn.AgentName(id)
		}
		if saved != nil {
			agentName = func(id string) string {
				a, err := saved.Get(id)
				if err != nil {
					return ""
				}
				return a.Name
			}
		}
		// Runner profiles (his call, 2026-11-23). A compute runner (joined for
		// work, holds no secrets) runs each session in a clean home of its own.
		// agentbus up is the PERSONAL runner: its ordinary sessions run in the
		// person's real home, so their setup (CLAUDE.md, hooks, plugins, aws,
		// ssh, gitconfig) comes along (Config.Personal). The clean home dir is
		// still made, because a personal turn that needs one — a whole-process
		// wall (a legacy toggle), a harness whose relay writes a home file, or a
		// continuation of a session that already has a clean home — still uses
		// it (run/run.go decides per turn).
		homes := filepath.Join(c.home, homesDir)
		eng, err = engine.New(engine.Config{Store: st, BaseURL: baseURL, ReplayBase: replayBase, AgentName: agentName, Homes: homes, Personal: c.builtin != nil,
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
	// The tool registry's overlay lives with the other control-plane files:
	// the added and removed tools, never the built-in ones. A runner serves
	// the built-in tools from code and keeps no overlay.
	var toolReg *toolreg.Registry
	if c.mode != modeRunner {
		if toolReg, err = toolreg.Open(filepath.Join(c.home, settings.ToolsFile)); err != nil {
			ln.Close()
			svcLog.Close()
			st.Close()
			closeQ()
			return nil, err
		}
	}
	acfg := api.Config{Store: st, Token: token, TurnsToken: turnsToken, Port: port, AnyHost: !loopback,
		BaseURL: baseURL, Origin: c.origin, Hosts: c.hosts, Getenv: c.getenvOr(), Folders: issuer, Build: build(debug.ReadBuildInfo()),
		Queue: q, Jobs: runnerJobs, Engine: eng, TurnWait: c.turnWait, Agents: saved, Settings: cfgStore, Subscriptions: logins,
		Tools: toolReg, Logf: svcLog.Printf}
	if q != nil {
		acfg.Models = modelSources(c, subs)
		if runnerJobs != nil {
			acfg.Models.Served = modelServer(c, runnerJobs, baseURL)
		}
	}
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
	// A runner that holds no secret — agentbus up's built-in one too — sends
	// each call to the relay, which adds the credential and records the call
	// in control's record (jobs/relay.go), and names each call's exchange to
	// the relay. The relay is control's: the server for a runner on another
	// machine, and up for its built-in runner, a process of its own on this
	// machine (builtin.go). One gateway path, so what works in up works across
	// machines.
	relayBase := ""
	if c.noSecrets() {
		relayBase = strings.TrimSuffix(c.server, "/")
	}
	if relayBase != "" {
		srv := relayBase + jobs.RelayPrefix
		anthURL, orURL = mustParse(srv+relayAnthropic), mustParse(srv+relayOpenRouter)
		chatURL, ccURL = mustParse(srv+relayChatGPT), mustParse(srv+relayCloudCode)
	}
	// A runner, with a record of its own, keeps each full call and leaves out
	// what the relay records when it forwards (runner/record.go).
	var gwSink event.Sink = sink
	anth := gateway.New(anthURL, anthropic.Adapter{}, gwSink)
	oai := gateway.New(chatURL, openai.Adapter{}, gwSink)
	// agy's Code Assist calls, on its own Google sign-in: on this machine's
	// sign-in, or, on a runner that holds no secret, the one connected on
	// control, which the relay adds.
	gem := gateway.New(ccURL, gemini.Adapter{}, gwSink)
	gws := []*gateway.Gateway{anth, oai, gem}
	// An OpenRouter agent's calls, under /openrouter of its base URL: one
	// gateway per API format, all to OpenRouter.
	orAnth := gateway.New(orURL, anthropic.Adapter{}, gwSink)
	orOAI := gateway.New(orURL, openai.Adapter{}, gwSink)
	orChat := gateway.New(orURL, chatcompletions.Adapter{}, gwSink)
	orAnth.SetPrefix(openRouterPrefix)
	orOAI.SetPrefix(openRouterPrefix)
	orChat.SetPrefix(openRouterPrefix)
	gws = append(gws, orAnth, orOAI, orChat)
	// An agent on its vendor's API key, under /key/<harness> of its base
	// URL: to the vendor's API, or, on a runner that holds no secret, to
	// control's relay, which adds the key set in Settings.
	keyed := map[string]http.Handler{}
	for _, h := range harness.All() {
		k := h.Info().APIKey
		if k == nil {
			continue
		}
		u := c.apiKeyURL(h.Info())
		if relayBase != "" {
			u = mustParse(relayBase + jobs.RelayPrefix + relayAPIKey(h.Info().Name))
		}
		g := gateway.New(u, formatAdapter(k.Format), gwSink)
		g.SetPrefix(apiKeyPrefix(h.Info().Name))
		gws = append(gws, g)
		keyed[apiKeyPrefix(h.Info().Name)] = g
		if claudeReplay != nil {
			switch k.Format {
			case harness.FormatAnthropic:
				g.SetRewriter(claudeReplay)
			case harness.FormatOpenAI:
				g.SetRewriter(codexReplay)
			}
		}
	}
	// An agent on a provider (harness.Providers), under /p/<name> of its
	// base URL: to control's relay, which adds the provider's key. A
	// provider agent runs only where there is one (runWire).
	for _, p := range harness.Providers {
		u := c.providerURL(p)
		if relayBase != "" {
			u = mustParse(relayBase + jobs.RelayPrefix + relayProvider(p.Name))
		}
		g := gateway.New(u, chatcompletions.Adapter{}, gwSink)
		g.SetPrefix(harness.ProviderPrefix(p.Name))
		gws = append(gws, g)
		keyed[harness.ProviderPrefix(p.Name)] = g
		if chatReplay != nil {
			g.SetRewriter(chatReplay)
		}
	}
	// A turn's run.ended waits for the gateway to write all of the
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
	// holdDecider backs every guarded gateway's Ask hold; startRunner fills in
	// the runner that polls control for the decision (H1).
	holdDecider := &lazyDecider{}
	var guardedGws []*gateway.Gateway
	for _, g := range gws {
		g.KeepBodies(keep)
		g.SetLog(svcLog.Printf)
		if relayBase != "" {
			g.SendExchange()
			// Control restarting (an upgrade of agentbus up) refuses the
			// connection for a few seconds: the call waits for it.
			g.SetUpstreamWait(controlWait)
		}
		// A runner checks each call's turn at its gateway's entrance, before it
		// is processed or recorded (turnGate), and sends it to control's relay,
		// which credentials it.
		if relayBase != "" {
			g.SetCredential(gate.credential())
		}
		// The command rules of permissions.toml (allow, ask, forbid), applied
		// to each run turn's replies: refuse, hold for a person, or run. A
		// command on none of the lists is held for a person.
		if eng != nil {
			g.SetGuard(eng)
			// A held Ask reply (H1) learns its decision from control's record
			// through the decider; startRunner fills in the runner that polls it,
			// so without a runner a guarded gateway refuses Ask inline, as before.
			g.SetDecider(holdDecider)
			guardedGws = append(guardedGws, g)
			if c.holdMS > 0 {
				// Test only: shorten the hold so its timeout path is quick. Ping
				// and poll scale down with it, keeping their order (poll < ping <
				// timeout).
				t := time.Duration(c.holdMS) * time.Millisecond
				g.SetHoldTiming(t, t/3, t/20)
			}
		}
	}
	if claudeReplay != nil {
		anth.SetRewriter(claudeReplay)
		orAnth.SetRewriter(claudeReplay)
		oai.SetRewriter(codexReplay)
		orOAI.SetRewriter(codexReplay)
		orChat.SetRewriter(chatReplay)
	}
	host, _, _ := net.SplitHostPort(c.listen)
	s := &service{addr: addr, shown: net.JoinHostPort(host, port), loopback: loopback, gws: append(gws, relayGws...), api: a, eng: eng, store: st, blobs: bs, blobSwp: blobSweep, jobsSrv: runnerJobs,
		queue: q, saved: saved, remote: remote, settings: cfgStore, sdb: sdb, folders: fr, log: svcLog, logins: logins, holdDecider: holdDecider, gate: gate, guardedGws: guardedGws, done: make(chan error, 1)}
	sdbHanded = true
	if q != nil {
		s.subs = subs
	}
	routed := vendors{anthropic: anth, openai: oai, openrouterAnth: orAnth, openrouterResps: orOAI, openrouterChat: orChat, gemini: handler(gem), keyed: keyed, relay: relayHandler}
	if c.mode == modeUp {
		// up's agents call the gateway of its built-in runner, a process of
		// its own (builtin.go), which relays to control. Control's own
		// address serves no model path: a call pointed at it by hand, which
		// carries no turn's stand-in, is refused and recorded nowhere, as up
		// refused it before.
		routed = vendors{relay: relayHandler}
	}
	if q != nil {
		// up and server: the cockpit at /. A runner's own service is for
		// its agents only.
		routed.cockpit = cockpitFiles
	}
	s.srv = &http.Server{
		Handler:           logRequests(svcLog, route(a, routed, !loopback)),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	svcLog.Printf("service.start mode=%s listen=%s pid=%d", c.modeName(), addr, os.Getpid())
	// While control was not running no runner could heartbeat: every attempt
	// gets a lease from now (control alone decides an attempt's end,
	// runner/lease.go). Here, once the address is held and before anything is
	// served or swept, so no call can find a lease run out before it is
	// renewed.
	if q != nil {
		if _, err := q.RenewLeases(jobs.Lease); err != nil {
			ln.Close()
			s.stop()
			return nil, err
		}
	}
	go func() { s.done <- s.srv.Serve(ln) }()
	if q != nil {
		// The session list's first fold reads the whole record (0.3 s at
		// 41,000 events); made now, the cockpit's first list after a start
		// reads from it like every later one (store/snapshotfold.go).
		go func() { _, _ = st.Snapshot() }()
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
	// built-in one of up, a process of its own, from this service on
	// loopback, as a runner on another machine does from its server.
	switch {
	case c.mode == modeRunner:
		err = s.startRunner(c, strings.TrimSuffix(c.server, "/"), enrollToken, svcLog)
	case c.mode == modeUp:
		err = s.startBuiltin(c, baseURL, enrollToken)
	}
	if err != nil {
		s.stop()
		return nil, err
	}
	return s, nil
}

// replayPlan is the engine's plan for a replayer's Base answer. A session
// replay cannot continue says why, and whether that turn's history can never
// be rebuilt (replay.ErrUnusable) rather than not stored yet.
func replayPlan(p replay.Plan, err error) (engine.ReplayPlan, error) {
	var no *replay.NoBase
	if errors.As(err, &no) {
		return engine.ReplayPlan{Refused: no.Why, Unusable: errors.Is(err, replay.ErrUnusable)}, nil
	}
	if err != nil {
		return engine.ReplayPlan{}, err
	}
	return engine.ReplayPlan{Turn: p.Turn, Failed: p.Failed}, nil
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
	// The built-in ons: a sandbox workspace whose work was applied or discarded
	// gets a put-away, queued to run once it has been quiet ([workspaces]
	// put_away_after_minutes). Registered once by key; a new one reads the
	// record from now on, so an upgrade does not queue one for every apply ever
	// recorded.
	if s.store != nil {
		for _, kind := range queue.RecordEvents {
			if _, err := s.queue.RegisterRecordOn(queue.DefaultTeam, kind, queue.Action{Queue: queue.KindPutAway}, "builtin/put_away/"+kind, s.store.Last()); err != nil {
				svcLog.Printf("queue.builtin_on_failed event=%s", kind)
			}
		}
	}
	go func() {
		defer close(s.sweepDone)
		defer beside.Wait()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var presence jobs.Presence
		var endedCursor *queue.Job // walks ended turn jobs for RecordTurnEnds
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
				// Record that each ended turn job ended, AFTER the steps above
				// that end jobs (Sweep expires leases; CloseLost writes a lost
				// turn's run.ended) and BEFORE FireOns, so a turn ended
				// this tick has its turn.ended in the record — after its
				// run.ended — and is delivered this tick. Idempotent by
				// job id; the finisher also writes it on its own path.
				if n, last, err := jobs.RecordTurnEnds(s.queue, s.store, endedCursor, time.Now().Add(-jobs.TurnEndedGrace), 200); err != nil {
					svcLog.Printf("record.turn_ends_failed")
				} else {
					endedCursor = last
					if n > 0 {
						svcLog.Printf("record.turn_ends n=%d", n)
					}
				}
				// An on that matured: asked work that ended delivers its
				// result as the asking session's next turn (queue/on.go,
				// FireOns), the generalisation of the old Deliver.
				// TODO(B13): jobs stored before an upgrade to this build hold
				// their provenance under links.asker, which this build no longer
				// reads: (1) asked work that ended but was not delivered gets no
				// on row, so FireOns never delivers it; (2) the asked-work caps
				// (countAsked) now count links.chain.by, so pre-upgrade jobs are
				// not counted and the per-turn and per-tree limits reset until
				// those jobs drain. A one-time startup migration (register an on
				// for each undelivered asked job, and backfill chain.by/by_session
				// from asker) would close both; for this dev milestone the store
				// is drained before upgrade instead.
				deliverTo := func(session string) (queue.DeliverTo, bool, error) {
					ws, found, err := s.store.WorkspaceOf(ids.SessionID(session))
					if err != nil || !found {
						return queue.DeliverTo{}, false, err
					}
					w, ok, err := s.store.Workspace(ws)
					if err != nil || !ok {
						return queue.DeliverTo{}, false, err
					}
					return queue.DeliverTo{Workspace: w.ID, FolderRef: w.FolderRef, Folders: api.Places(w), Machine: w.Machine, Isolate: w.Sandbox}, true, nil
				}
				if n, err := s.queue.FireOns(queue.DefaultTeam, s.store.Ended, deliverTo, name, s.deliveredAgent); err != nil {
					svcLog.Printf("queue.fire_ons_failed")
				} else if n > 0 {
					svcLog.Printf("queue.results_delivered n=%d", n)
				}
				// An on over a session: each of its turns that ended since
				// the on's cursor delivers its reply (queue/watchon.go).
				if n, err := s.queue.FireWatchOns(queue.DefaultTeam, s.recordEvents, deliverTo, name, s.deliveredAgent); err != nil {
					svcLog.Printf("queue.fire_watch_ons_failed")
				} else if n > 0 {
					svcLog.Printf("queue.results_delivered n=%d", n)
				}
				// An on over the record: an event of its kind recorded since
				// its cursor queues its job (queue/recordon.go), such as the
				// put-away of a workspace whose work is settled.
				if n, err := s.queue.FireRecordOns(queue.DefaultTeam, s.recordEvents, s.workspacePlace, s.jobDelay); err != nil {
					svcLog.Printf("queue.fire_record_ons_failed")
				} else if n > 0 {
					svcLog.Printf("queue.put_aways_queued n=%d", n)
				}
			}
		}
	}()
}

// startRunner enrolls the built-in runner and starts it, holding this
// machine's folders: it makes each session's copy from its folder and does
// the folders' jobs (applies, typed folders) itself, so one program runs on
// the machine. Every runner, up's built-in one too (a process of its own,
// builtin.go), keeps its own blob store (blob-cache/), with control's behind
// it, and sends control only what it lacks, over HTTP.
func (s *service) startRunner(c config, self, enrollToken string, svcLog *logfile.Log) error {
	capacity := runnerCapacity(c.capacity)
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
		Version: build(debug.ReadBuildInfo()).Version,
		Engine:  s.eng, Store: s.store, Logf: svcLog.Printf, Labels: c.labels,
		LookPath: c.lookPath, Environ: c.environ,
		Wall:    func(ctx context.Context) wall.Result { return wall.Probe(ctx, exec.LookPath, c.environ()) },
		Workdir: filepath.Join(c.home, copiesDir), OldWorkdir: filepath.Join(c.home, oldCopiesDir), StateFile: filepath.Join(c.home, runnerState), Settings: s.settings,
		Forward: true, NoSecrets: c.noSecrets(), StartDelay: c.startDelay,
		Finishes: filepath.Join(c.home, finishesDir)}
	if c.builtin != nil && c.builtin.process {
		retire := make(chan struct{})
		cfg.Retire, s.retire = retire, retire
	}
	// The runner's own store, which its folder side shares: one handle, so
	// its sweep and the folder side's touches share one lock.
	store, err := blobs.Open(filepath.Join(c.home, cacheDir))
	if err != nil {
		return fail(err)
	}
	fs := folderSide(c.home, s.folders, store)
	cfg.Folders, cfg.FolderSide, cfg.Lender = s.folders, connector.Jobs{Connector: fs}, fs
	rn, err := runner.Enroll(ctx, cfg)
	if err != nil {
		return fail(fmt.Errorf("the runner could not enroll with %s: %w", self, err))
	}
	// Now the guarded gateways can hold an Ask: the runner polls control for the
	// decision, scoped to its own attempt (runner/decide.go, jobs/decision.go),
	// and scopes each raised approval id to the attempt that raised it.
	if s.holdDecider != nil {
		s.holdDecider.set(rn.Decider())
	}
	if s.gate != nil {
		s.gate.set(rn)
	}
	for _, g := range s.guardedGws {
		g.SetApprovalScope(rn.ScopeApproval)
	}
	client := blobClient(self, rn.Token())
	fs.Remote = client
	rn.UseBlobs(blobs.Local{Store: store, From: client}, client)
	go sweepCache(ctx, store, svcLog.Printf)
	if s.remote != nil {
		s.remote.Enrolled(rn.ID(), rn.Token())
	}
	svcLog.Printf("runner.start id=%s capacity=%d machine=%s folders=%d", rn.ID(), capacity, s.folders.Machine(), len(s.folders.List(queue.MaxFolders)))
	if s.retire != nil {
		go s.watchRetire(c, rn.Agents, rn.Busy, rn.Away, s.retire)
	}
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

// recordEvents reads the record's events of kind past after, for an on over
// the record (queue.RecordReader).
func (s *service) recordEvents(kind string, after int64, limit int) ([]queue.RecordEvent, error) {
	evs, err := s.store.EventsOfKind(kind, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]queue.RecordEvent, len(evs))
	for i, e := range evs {
		out[i] = queue.RecordEvent{ID: e.ID, Seq: e.Seq, Time: e.Time, Workspace: e.Workspace, Turn: e.Turn}
	}
	return out, nil
}

// workspacePlace is where workspace ws is, from the record: its machine and
// whether it has a copy of its own, for the job an on over the record queues.
func (s *service) workspacePlace(ws string) (queue.DeliverTo, bool, error) {
	w, ok, err := s.store.Workspace(ids.WorkspaceID(ws))
	if err != nil || !ok {
		return queue.DeliverTo{}, false, err
	}
	return queue.DeliverTo{Workspace: w.ID, FolderRef: w.FolderRef, Folders: api.Places(w), Machine: w.Machine, Isolate: w.Sandbox}, true, nil
}

// jobDelay is how long a job an on over the record queues waits before a
// runner may take it: for a put-away, the quiet time of the settings. false is
// off. A policy that cannot be read is off, so nothing is put away on a guess.
func (s *service) jobDelay(kind string) (time.Duration, bool) {
	if kind != queue.KindPutAway || s.settings == nil {
		return 0, false
	}
	p, _, err := s.settings.Policy()
	if err != nil {
		return 0, false
	}
	return p.PutAwayAfter()
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

// The relay's names for the vendors it adds credentials for: under
// jobs.RelayPrefix of the server.
const (
	relayAnthropic  = "anthropic"
	relayChatGPT    = "chatgpt"
	relayCloudCode  = "cloudcode"
	relayOpenRouter = "openrouter"
)

// modelSources are where control reads the lists of models
// (api.ModelSources): OpenRouter's list-models call with the OpenRouter key,
// each vendor's list-models call with its API key set in Settings, agy's
// own listing, run unmodified in its home on control, and on an API key in a
// clean home with no call (keyModelLister), and codex's own
// model/list (modelServer, set where control has a relay).
func modelSources(c config, subs *credentials.Subscriptions) *api.ModelSources {
	apiBase := map[string]string{}
	for _, h := range harness.All() {
		if k := h.Info().APIKey; k != nil {
			apiBase[k.Name] = c.apiKeyURL(h.Info()).String()
		}
	}
	or := c.openrouter
	if or == nil {
		or = mustParse(openrouter)
	}
	return &api.ModelSources{Cache: &models.Cache{}, Client: &http.Client{Timeout: 20 * time.Second},
		OpenRouterBase: or.String(), Own: subs.OwnModels, KeyListed: keyModelLister(c),
		// The key an OpenRouter agent would get: set in Settings, or else
		// the default variable's, where control runs.
		OpenRouterKey: func() string {
			if k, err := subs.Key(credentials.AccessOpenRouter); err == nil {
				return k
			}
			return c.getenvOr()(agents.DefaultKeyEnv)
		},
		APIBase: apiBase,
		// A vendor's API key: only ever the one set in Settings.
		APIKey: func(name string) string {
			k, _ := subs.Key(name)
			return k
		}}
}

// relayProvider is the relay's name for the provider named name.
func relayProvider(name string) string { return "p-" + name }

// providerURL is where a provider's calls go: c.providers's entry for it, or
// the provider's own API.
func (c config) providerURL(p harness.Provider) *url.URL {
	if u := c.providers[p.Name]; u != nil {
		return u
	}
	return mustParse(p.URL)
}

// relayAPIKey is the relay's name for the vendor API of harness's access
// "api".
func relayAPIKey(harness string) string { return "key-" + harness }

// apiKeyPrefix is the path under a turn's base URL that the service forwards
// to the vendor API of harness's access "api" (harness.Info.APIKey).
func apiKeyPrefix(harness string) string { return "/key/" + harness }

// apiKeyURL is where the calls of info's access "api" go: c.apiKeys's entry
// for it, or the vendor's own API.
func (c config) apiKeyURL(info harness.Info) *url.URL {
	if u := c.apiKeys[info.Name]; u != nil {
		return u
	}
	return mustParse(info.APIKey.URL)
}

// formatAdapter is the adapter that reads calls of an API format.
func formatAdapter(format string) adapter.Adapter {
	switch format {
	case harness.FormatOpenAI:
		return openai.Adapter{}
	case harness.FormatGemini:
		return gemini.Adapter{}
	default:
		return anthropic.Adapter{}
	}
}

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
				Within: time.Duration(l.RenewWithinMinutes) * time.Minute, Path: path, Args: l.RenewArgs, ModelArgs: l.ModelsArgs, Unset: l.Unset, Set: l.Set}
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

// subscriptionsProtected are the files of subs that no turn may edit: each
// harness's sign-in, the home of a harness that keeps its own, and every key
// set in Settings.
func subscriptionsProtected(subs *credentials.Subscriptions) []string {
	var out []string
	for _, h := range harness.All() {
		if l := h.Info().Login; l != nil {
			out = append(out, filepath.Join(subs.Dir, h.Info().Name+".json"))
			if l.Keep != "" {
				// The harness's own home there, and its sign-in by
				// its own name, as every harness's rules take a file.
				out = append(out, filepath.Join(subs.Dir, l.Keep), filepath.Join(subs.Dir, l.Keep, l.File))
			}
		}
	}
	for _, name := range credentials.KeyNames {
		if p, err := subs.KeyFile(name); err == nil {
			out = append(out, p)
		}
	}
	return out
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

// settleTimeout bounds how long a turn's run.ended waits for the
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
// cockpitFiles serves the cockpit packed into this binary, or the page that
// says none is (package cockpit). Tests set their own.
var cockpitFiles = cockpit.Handler()

// site is the cockpit's files (cockpit.Site).
type site interface {
	http.Handler
	Serves(r *http.Request) bool
}

type vendors struct {
	anthropic, openai               http.Handler // the vendors' own, for a subscription
	openrouterAnth, openrouterResps http.Handler // OpenRouter's, under /openrouter
	// openrouterChat is OpenRouter's Chat Completions gateway (opencode),
	// under /openrouter.
	openrouterChat http.Handler
	// relay takes runners' model calls and adds their credential, under
	// jobs.RelayPrefix; nil without a queue. A running turn's stand-in guards
	// it, so it answers callers on other machines.
	relay http.Handler
	// gemini is agy's Code Assist gateway: to Google, or on a runner that
	// holds no secret, to control's relay. Nil: none.
	gemini http.Handler
	// cockpit serves the cockpit's files and pages, in up and server: the
	// requests no vendor claims that it Serves (package cockpit). Nil, as
	// in a runner: / sends the caller to the docs.
	cockpit site
	// keyed are the gateways of agents on their vendor's API key, by their
	// prefix (apiKeyPrefix).
	keyed map[string]http.Handler
}

func route(a *api.API, v vendors, open bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !plainPath(r) {
			localError(w, "agentbus: the path has a dot, empty or encoded segment and is refused", http.StatusBadRequest)
			return
		}
		if api.Reserved(r) {
			if api.Public(r) {
				boundPublic(w, r, a)
				return
			}
			a.ServeHTTP(w, r)
			return
		}
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		if v.cockpit == nil && r.URL.Path == "/" && read {
			boundPublic(w, r, http.RedirectHandler("/docs", http.StatusFound))
			return
		}
		if v.relay != nil && strings.HasPrefix(r.URL.Path, jobs.RelayPrefix) {
			v.relay.ServeHTTP(w, r)
			return
		}
		session, _, rest := gateway.SplitSession(r.URL.EscapedPath())
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
		case keyedGateway(v.keyed, rest) != nil:
			gw = keyedGateway(v.keyed, rest)
		case under(rest, openRouterPrefix+"/v1/responses"):
			gw = v.openrouterResps
		case under(rest, openRouterPrefix+"/v1/chat/completions"):
			gw = v.openrouterChat
		case under(rest, openRouterPrefix+"/v1"):
			gw = v.openrouterAnth
		default:
			// The cockpit's files and pages, to any caller: they hold
			// nothing of the person's. Never under a session's prefix,
			// which is an agent's.
			if v.cockpit != nil && session == "" && !strings.HasPrefix(r.URL.Path, "/s/") && v.cockpit.Serves(r) {
				// A page or a file takes no body. One sent anyway is
				// refused before it is read, and its connection closed
				// soon: the server would otherwise wait for it with no
				// deadline.
				if r.ContentLength != 0 {
					localError(w, "agentbus: a cockpit request takes no body", http.StatusBadRequest)
					return
				}
				boundPublic(w, r, v.cockpit)
				return
			}
			localError(w, "agentbus: no vendor is routed for this path", http.StatusNotFound)
			return
		}
		if gw == nil {
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

// publicWait bounds how long a reply served without a token may take to
// read its request and to write itself: anyone may send one, so a caller
// that sends slowly or stops reading holds a connection this long at most.
// Tests shorten it.
var publicWait = 30 * time.Second

// boundPublic serves every reply that needs no token (the cockpit, the docs,
// the descriptions, the sign-in trade) with a read and a write deadline of
// publicWait. Such a reply never shares its connection: it goes out with
// Connection: close, so net/http closes the connection after it. The write
// deadline therefore stays set to the end, which bounds the flush net/http
// makes after the handler returns, and no later request on the connection
// (a token's stream among them) can inherit it.
func boundPublic(w http.ResponseWriter, r *http.Request, h http.Handler) {
	rc := http.NewResponseController(w)
	until := time.Now().Add(publicWait)
	_ = rc.SetReadDeadline(until)
	_ = rc.SetWriteDeadline(until)
	w.Header().Set("Connection", "close")
	h.ServeHTTP(w, r)
}

// keyedGateway is the gateway of keyed whose prefix path is under, or nil.
func keyedGateway(keyed map[string]http.Handler, path string) http.Handler {
	for p, g := range keyed {
		if under(path, p) {
			return g
		}
	}
	return nil
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
	// up stops watching its built-in runner, which keeps running: its turns
	// go on and reach control when up is back (builtin.go). One in this
	// process (a test) stops first, while the API still serves it.
	if s.stopSupervise != nil {
		s.stopSupervise()
	}
	if s.inRunner != nil {
		s.inRunner.stop()
	}
	// The runner first: it stops its jobs' turns and finishes them through
	// the API, which is still serving.
	if s.stopRunner != nil {
		s.stopRunner()
		<-s.runnerDone
	}
	// Turns next: each stores its run.ended, and new ones are refused.
	if s.eng != nil {
		s.eng.StopAll()
	}
	// The sweep before the address: once control stops answering, no runner
	// can heartbeat, so no lease may run out from then (runner/lease.go).
	if s.stopSweep != nil {
		s.stopSweep()
		<-s.sweepDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // streams never go idle, so this usually times out
	_ = s.srv.Close()
	for _, gw := range s.gws {
		gw.Close()
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

// freshFolder says home, the data folder, is new: missing, or holding
// nothing yet.
func freshFolder(home string) bool {
	entries, err := os.ReadDir(home)
	return len(entries) == 0 && (err == nil || errors.Is(err, fs.ErrNotExist))
}

// notPackedLine is what a source build prints instead of a sign-in link.
// Release binaries pack the cockpit; go install and go build do not.
func notPackedLine(listen string) string {
	return "The cockpit is not packed in this build: " + packCockpit + ". The API's docs: http://" + linkHost(listen) + "/docs"
}

// packCockpit is how a source build gets the cockpit, for up and doctor.
const packCockpit = "install a release to get it (brew install staysup/tap/agentbus, or the install script in the README), " +
	"or from a clone run tools/pack-cockpit.sh, then go build"

// dataFolderLine is what agentbus up says about its data folder when it
// starts: which one, and whether it is new or what an earlier run left in
// it. up reuses an existing folder, so its old agents and sessions show in
// the cockpit: this line says so before the person looks.
func (s *service) dataFolderLine(home string, fresh bool) string {
	line := "Data folder: " + tildePath(home)
	if fresh {
		return line + " (new)"
	}
	snap, err := s.store.Snapshot()
	if err != nil {
		return line + " (existing)"
	}
	return line + " (existing: " + counted(len(s.saved.List()), "agent") + ", " + counted(len(snap.Sessions), "session") + ")"
}

// tildePath is p as a person writes it: from ~ when it is under their home.
func tildePath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h+string(filepath.Separator)) {
		return "~" + p[len(h):]
	}
	return p
}

// counted is n things: "1 agent", "3 agents".
func counted(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.Itoa(n) + " " + thing + "s"
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

// listenError is the refusal when net.Listen fails: fixed wording, never
// Go's own message.
func listenError(addr string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return errors.New("-listen " + addr + " is in use. Stop the other process, or pick another port with -listen")
	}
	return errors.New("-listen " + addr + " could not be bound")
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

// handler is g as an http.Handler, or nil when there is no g: a nil
// *gateway.Gateway in an interface would not be nil.
func handler(g *gateway.Gateway) http.Handler {
	if g == nil {
		return nil
	}
	return g
}
