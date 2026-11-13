// Package run starts one turn of a stock agent CLI and watches it. It
// prepares the turn (its session, own replay or the vendor's resume, the
// person's permissions.toml, the guard's forbid list, the gateway path), stores
// session.opened and turn.started, starts the agent as a child, and stores
// session.closed with the exit code and the vendor's own session id
// (resume_id) when it exits. All three carry the turn id. The child's base URL
// is <service>/s/<session>/t/<turn>, so the gateway gives every event of the
// child's model calls the same turn id.
//
// It knows nothing about jobs or queues: a runner calls it (package runner).
// A session's next turn continues by own replay for claude and codex (the
// gateway puts the earlier turn's history in front of its requests); with
// replay off, or for a session that has no history to replay and never used
// replay, it continues the vendor's conversation named by its resume_id
// (Continuity).
//
// The service log keeps the word run (run.start, run.exit): a run is the
// child process that does one turn.
package run

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/logfile"
	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/sandbox"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// DefaultMaxRuns bounds the runs at once, when Config.MaxRuns is zero.
const DefaultMaxRuns = 4

// MaxPrompt bounds a turn's prompt, in bytes.
const MaxPrompt = 256 << 10

// MaxFolder bounds a turn's folder, in bytes.
const MaxFolder = 4096

// ModelShape is the shape of a model name: it is only ever the value of
// --model. It takes OpenRouter's names too, such as
// deepseek/deepseek-v4.1-flash or a name with a :free variant. It never starts
// with a dash, so it is never read as a flag.
var ModelShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// SessionShape is the shape of a session id, and of a turn id.
var SessionShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Launch is how a run reaches an agent binary: the binary's path, the child's
// whole environment, and the arguments that go before the run's own.
type Launch struct {
	Path string
	Env  []string
	Args []string
	// HomeFiles are files the child needs in the session's own home, by
	// path in it, such as agy's placeholder sign-in (harness.HomeFiles).
	// They are written before every turn, and a turn with them that has no
	// home of its own does not start: they would land in this user's home.
	HomeFiles map[string][]byte
}

// Wire returns the Launch for agent, with baseURL reaching it through the
// child's environment or arguments, and acc saying how it reaches its model.
// It returns ErrOwnEndpoint, ErrNotFound or a *Refusal when it cannot.
type Wire func(agent, baseURL string, acc Access) (Launch, error)

// Access is how a turn's agent reaches its model. The zero value is the
// harness's own subscription.
type Access struct {
	// Kind is "" or "subscription", or "openrouter".
	Kind string
	// KeyEnv, for openrouter, names the variable that holds the key. The
	// runner reads it when the turn starts; it is never stored.
	KeyEnv string
	// Model is the turn's model, for a wire that must name it in more than
	// --model.
	Model string
	// Placeholder, when set, is the credential the child gets instead of
	// one of this machine's: a runner holds no secrets, and control's
	// gateway swaps the real credential in on the way out. Nothing is read
	// from the runner's environment or sign-in.
	Placeholder string
}

// Refused makes a refusal a Wire can return: status and the person's words.
func Refused(status int, msg string) error { return refuse(status, msg) }

var (
	// ErrOwnEndpoint: the service's environment already sends the agent to
	// another endpoint, whose credentials would then go to the vendor.
	ErrOwnEndpoint = errors.New("run: the agent already has its own endpoint")
	// ErrNotFound: the agent's binary is not on the service's PATH.
	ErrNotFound = errors.New("run: the agent's binary was not found")
)

// ReplayPlan is how a new claude or codex turn continues its session by own
// replay.
type ReplayPlan struct {
	// Turn is the earlier turn whose history is replayed: the session's
	// newest. Empty when the session cannot be continued by replay.
	Turn string
	// Failed, with Turn, says how that turn ended without finishing, as the
	// history's note tells the model. Empty when it finished.
	Failed string
	// Refused, without Turn, says why the session cannot be continued by
	// replay, naming the turn.
	Refused string
}

// Config is what the engine needs.
type Config struct {
	Store *store.Store
	// BaseURL is the address a turn's agent reaches the service on, such as
	// http://127.0.0.1:8787. The agent runs on this machine, so it is a
	// loopback address whenever the service listens on one.
	BaseURL string
	// Wire says how a turn reaches each agent's binary. Nil refuses every
	// turn.
	Wire Wire
	// ReplayBase turns on own replay for claude and codex: it plans how a new
	// turn of agent's session continues it (replay.Replayer.Base). Nil turns
	// own replay off, and both continue by the vendor's resume (Continuity).
	ReplayBase func(agent, session string) (ReplayPlan, error)
	// Settle, when set, is called after a turn's child exits and before its
	// session.closed is stored. It returns once the gateway has written every
	// model call of the turn (gateway.Settle). It reports false when that
	// could not be confirmed; session.closed then carries unsettled, and own
	// replay will not continue from that turn. Nil stores session.closed at
	// once.
	Settle func(session, turn string) bool
	// MaxRuns bounds the turns running at once. Past it Start refuses with
	// 429. Zero means DefaultMaxRuns.
	MaxRuns int
	// RunGrace is how long a stop waits after SIGTERM before SIGKILL. Zero
	// means 5 seconds.
	RunGrace time.Duration
	// Settings hold the policy and the instructions every turn gets, read at
	// the start of every turn. Nil gives every turn the defaults.
	Settings settings.Store
	// Protect are files no turn may edit: the data folder's policy,
	// instructions, saved agents and tokens.
	Protect []string
	// APIDir is a folder that holds agentbus-api, the API client a turn's
	// agent calls (cmd/agentbus, apicall.go). It goes first on each turn's
	// PATH, and the turn's instructions name agentbus-api: it reads
	// AGENTBUS_URL and AGENTBUS_TOKEN itself, so the command holds no
	// variable, and a claude turn that may write may run it without asking.
	// The name stays the same when the program moves, so a session's earlier
	// calls still work. Empty puts nothing on PATH: the instructions still
	// name agentbus-api, and the service log says why it is missing.
	APIDir string
	// Homes, when set, is the folder that holds a home of its own for each
	// session: <Homes>/<session>, made empty at the session's first turn,
	// kept for its next ones (the vendor's resume reads its transcript
	// there), and removed with the session (RemoveHome). A session started
	// before it was set has none, and keeps the service's own HOME. Each turn gets it as
	// HOME and as its harness's config folder (harness.Info.ConfigEnv). No
	// sign-in is put in it: set it only where the credential comes from
	// elsewhere, as on a runner that holds no secrets. Empty runs every turn
	// in the service's own HOME, with the harness's own sign-in.
	Homes string
	// AgentName names a saved agent by its id in the transcript a turn on
	// another harness gets; "" or a nil func names it by harness and model.
	AgentName func(id string) string
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Logf writes one line to the service log: a child starting and exiting.
	// It is never given a token, a prompt or the child's output.
	Logf func(format string, args ...any)
	// RunLog keeps a child's output, once per exit: a header line and the
	// last RunOutputMax bytes of its stdout and stderr together. Nil discards
	// it. That output is the agent's own and may hold the person's data, so
	// it goes only here.
	RunLog func(session string, data []byte)
}

// Engine starts and stops turns. It is safe for concurrent use.
type Engine struct {
	cfg        Config
	runs       runs
	turnTokens turnTokens // the live turns' own tokens (tokens.go)
	turnRules  turnRules  // the live turns' forbid entries, for the gateway (slots.go)
}

// New returns the engine. It closes every turn's hold that the service's
// previous process left open: that process can no longer report their exit.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil {
		return nil, errors.New("run: no store")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunGrace == 0 {
		cfg.RunGrace = 5 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.MaxRuns == 0 {
		cfg.MaxRuns = DefaultMaxRuns
	}
	if cfg.MaxRuns < 0 {
		return nil, errors.New("run: MaxRuns below zero")
	}
	e := &Engine{cfg: cfg}
	e.runs.max = cfg.MaxRuns
	if _, err := cfg.Store.CloseStaleHolds(cfg.Now().UTC(), newEventID); err != nil {
		return nil, err
	}
	return e, nil
}

// SetSettle sets Config.Settle, for a gateway made after the engine (it needs
// the engine's TurnSink). Call it before the first turn.
func (e *Engine) SetSettle(f func(session, turn string) bool) { e.cfg.Settle = f }

// MaxRuns is how many turns may run at once.
func (e *Engine) MaxRuns() int { return e.cfg.MaxRuns }

// Refusal is why a turn did not start. Status says what kind, in HTTP's
// words: 400 for the request or the service's own setup, 404 for a session no
// event names, 409 for a session that cannot take this turn now, 429 at the
// run limit, 500 for a store that cannot be written, 503 while stopping.
// Nothing ran.
type Refusal struct {
	Status int
	Msg    string
}

func (r *Refusal) Error() string { return r.Msg }

func refuse(status int, msg string) *Refusal { return &Refusal{status, msg} }

// unusable is the refusal of a turn whose setting cannot be used, with the
// log line that says why. reason names the setting in the log.
func (e *Engine) unusable(agent, reason string, err error) *Refusal {
	var se *settings.Error
	if !errors.As(err, &se) {
		e.cfg.Logf("run.refused agent=%s reason=%s", agent, reason)
		return refuse(http.StatusInternalServerError, "the settings could not be read, so the turn did not start")
	}
	e.cfg.Logf("run.refused agent=%s reason=%s error=%q", agent, reason, se.Why)
	return refuse(http.StatusBadRequest, se.Where+" cannot be used, so the turn did not start: "+se.Why+". "+se.Fix+", and send the turn again.")
}

var storeFailed = refuse(http.StatusInternalServerError, "the store could not be read or written")

// Request is one turn to start.
type Request struct {
	// Session continues that session. Empty starts a new one, which needs
	// Harness and Folder.
	Session string
	// NewSession is the id of a new session; empty picks one.
	NewSession string
	// Turn is the turn's id; empty picks one, t_ and 16 hex characters.
	Turn string
	// Harness is the program that runs the turn's loop, such as claude.
	Harness, Folder string
	// FolderRef is the reference of the person's folder a new session is
	// started in, for the record; "" when no connector owns it.
	FolderRef      string
	Prompt, Model  string
	ReadOnly       bool
	IdempotencyKey string // stored on turn.started, as the caller's key
	// AgentID, Access and KeyEnv: the saved agent and how it reaches its
	// model (Access). Every turn needs Harness, a next one too (NoAgent):
	// with Session, the turn runs on that agent in the session's folder.
	AgentID, Access, KeyEnv string
	// AgentModel is the named agent's model, for a next turn that names no
	// Model: the turn runs with it.
	AgentModel string
	// Origin, when set, is stored on turn.started: who started the turn
	// when the person did not.
	Origin *event.Origin
	// Root and Depth place the turn in a request tree, for its token (Grant):
	// Root is the session the person's turn ran in, Depth how many agents
	// asked in a row before it. Empty Root means the turn's own session.
	Root  string
	Depth int
	// Token is the turn's own token when control minted it, for a runner of
	// a server: the child calls control's API at APIURL with it, and with
	// Placeholder its model calls carry it as their credential, which
	// control's gateway swaps for the real one. Empty Token mints one here,
	// for this service's API.
	Token, APIURL string
	Placeholder   bool
	// Signin, when set, is why control can give this turn's agent no
	// credential now, such as "connect your claude subscription in
	// Settings": the turn is refused with it, after a missing program and
	// before anything is stored.
	Signin string
}

// Turn is a turn that started.
type Turn struct {
	Session, ID string
	// BaseURL is the address the agent uses, <service>/s/<session>/t/<turn>.
	BaseURL string
	done    chan struct{}
	end     Ended
}

// Ended is how a turn's child ended.
type Ended struct {
	ExitCode  int
	Stopped   bool // a stop came while it ran
	Unsettled bool // the gateway's writes could not be confirmed
}

// Done is closed once the turn's session.closed is stored.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Ended is how the turn ended. Read it after Done.
func (t *Turn) Ended() Ended { <-t.done; return t.end }

// missingAgent is the fixed reply when agent's program is not on the service's
// PATH. The program has the agent's name.
func missingAgent(agent string) string {
	product := ""
	if h, ok := harness.Get(agent); ok {
		product = h.Info().Product
	}
	return "agent " + agent + " cannot start: the program " + agent + " is not on the service's PATH. " +
		"Install " + product + " and run " + agent + " once to sign in, then try again. " +
		"If it is installed, start agentbus from a shell where " + agent + " runs."
}

// KnownHarness says a turn can start on name.
func KnownHarness(name string) bool {
	_, ok := harness.Get(name)
	return ok
}

// unknownHarness refuses a turn on a harness no turn can start.
func unknownHarness() string { return "agent must be " + harness.NameList() }

// CheckPrompt says what is wrong with a prompt and a model, or "".
func CheckPrompt(prompt, model string) string {
	if !PromptOK(prompt) {
		return "prompt must be from 1 byte to 256 KiB, with no NUL byte"
	}
	if model != "" && !ModelShape.MatchString(model) {
		return "model must match " + ModelShape.String()
	}
	return ""
}

// PromptOK says a prompt is from 1 byte to MaxPrompt, with no NUL byte.
func PromptOK(prompt string) bool {
	return prompt != "" && len(prompt) <= MaxPrompt && strings.IndexByte(prompt, 0) < 0
}

// Start starts a turn: a new session's first, or req.Session's next. It
// returns once the turn's session.opened and turn.started are stored and its
// child has started, or a *Refusal saying why nothing ran.
func (e *Engine) Start(req Request) (*Turn, error) {
	if msg := CheckPrompt(req.Prompt, req.Model); msg != "" {
		return nil, refuse(http.StatusBadRequest, msg)
	}
	if req.Turn == "" {
		req.Turn = "t_" + randomHex(8)
	} else if !SessionShape.MatchString(req.Turn) {
		return nil, refuse(http.StatusBadRequest, "turn must match "+SessionShape.String())
	}
	if req.Session != "" {
		return e.next(req)
	}
	if !KnownHarness(req.Harness) {
		return nil, refuse(http.StatusBadRequest, unknownHarness())
	}
	if len(req.Folder) > MaxFolder || !filepath.IsAbs(req.Folder) || !isDir(req.Folder) {
		return nil, refuse(http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
	}
	id := req.NewSession
	if id == "" {
		id = randomHex(8)
	} else if !SessionShape.MatchString(id) {
		return nil, refuse(http.StatusBadRequest, "session must match "+SessionShape.String())
	}
	// Before the slot is taken: a refusal here holds nothing.
	if req.Access != "" && req.Access != "subscription" && req.Access != "openrouter" {
		return nil, refuse(http.StatusBadRequest, "access must be subscription or openrouter")
	}
	if code, msg := e.runs.take(id); code != 0 {
		return nil, refuse(code, msg)
	}
	return e.start(turn{harness: req.Harness, folder: req.Folder, folderRef: req.FolderRef, session: id, id: req.Turn,
		model: req.Model, readOnly: req.ReadOnly, prompt: req.Prompt, key: req.IdempotencyKey,
		agentID: req.AgentID, access: req.Access, keyEnv: req.KeyEnv, origin: req.Origin, root: req.Root, depth: req.Depth,
		token: req.Token, apiURL: req.APIURL, placeholder: req.Placeholder, signin: req.Signin})
}

// next starts the next turn of req.Session, in its own agent and folder.
func (e *Engine) next(req Request) (*Turn, error) {
	session := req.Session
	if !SessionShape.MatchString(session) {
		return nil, refuse(http.StatusBadRequest, "session must match "+SessionShape.String())
	}
	c, err := e.cfg.Store.Continuation(session)
	if err != nil {
		return nil, storeFailed
	}
	if !c.Known {
		return nil, refuse(http.StatusNotFound, "no such session")
	}
	// The slot and the session are taken before the session is read again,
	// so a turn that started meanwhile is either refused here as busy or
	// already shows its open session.opened below.
	if code, msg := e.runs.take(session); code != 0 {
		return nil, refuse(code, msg)
	}
	fail := func(r *Refusal) (*Turn, error) {
		e.runs.release(nil, session)
		return nil, r
	}
	if c, err = e.cfg.Store.Continuation(session); err != nil {
		return fail(storeFailed)
	}
	if c.Open {
		return fail(refuse(http.StatusConflict, TurnRunning))
	}
	if !KnownHarness(c.Harness) {
		return fail(refuse(http.StatusConflict, "this session cannot be continued from here"))
	}
	// The turn runs on the agent it names, as control read it once when the
	// turn was asked for: the same answer control's relay adds the
	// credential for. A session keeps no agent of its own, only its folder;
	// the record keeps which agent and model each past turn ran with, as
	// history. With no model of its own, the turn takes the agent's.
	if req.Harness == "" {
		return fail(refuse(http.StatusBadRequest, NoAgent))
	}
	if !KnownHarness(req.Harness) {
		return fail(refuse(http.StatusBadRequest, unknownHarness()))
	}
	if req.Access != "" && req.Access != "subscription" && req.Access != "openrouter" {
		return fail(refuse(http.StatusBadRequest, "access must be subscription or openrouter"))
	}
	agent, agentID, access, keyEnv, model := req.Harness, req.AgentID, req.Access, req.KeyEnv, req.Model
	if model == "" {
		model = req.AgentModel
	}
	// How the turn reaches the session's earlier turns depends on the agent
	// (Continuity): by own replay, the child starts fresh and the gateway
	// puts the earlier turn's history in front of its requests; by resume,
	// the vendor's own resume_id. A turn on another harness than the latest
	// turn's cannot read that turn's history, which is in the other vendor's
	// format, nor can a turn of codex on OpenRouter give or take one
	// (replayLayout): it starts fresh with the earlier turns as plain text.
	var from, recovered, recoveredWhy, context string
	continuity := e.Continuity(agent)
	if agent != c.Harness || (continuity == ByReplay && !(replayLayout(agent, access) && replayLayout(c.Harness, c.Access))) {
		continuity = ByTranscript
		if context, err = e.transcript(session, len(req.Prompt)); err != nil {
			return fail(storeFailed)
		}
	}
	if continuity == ByReplay {
		plan, err := e.cfg.ReplayBase(c.Harness, session)
		if err != nil {
			return fail(storeFailed)
		}
		if plan.Turn != "" {
			from = plan.Turn
			if plan.Failed != "" {
				// The newest turn did not finish. The turn continues after it
				// anyway, and its history ends with a note saying how.
				recovered, recoveredWhy = plan.Turn, plan.Failed
			}
		} else {
			// The newest turn's history cannot be rebuilt. A session that
			// never used replay, such as one from before replies were
			// stored, still continues by the vendor's resume: its resume_id
			// names the whole conversation. A session that did use replay
			// does not: its resume_id names only its last turn's own process.
			used, err := e.cfg.Store.UsedReplay(session)
			if err != nil {
				return fail(storeFailed)
			}
			if used || c.ResumeID == "" {
				return fail(refuse(http.StatusConflict, "this session cannot be continued from here: "+plan.Refused))
			}
			continuity = ByResume
		}
	}
	// A vendor's resume_id names the conversation of the turn that gave it.
	// One from before a switch to another harness (the turns since left
	// none) lacks the turns since, and may be the other vendor's: the turn
	// gets the earlier turns as text instead.
	if continuity == ByResume && c.ResumeID != "" && c.ResumeStale {
		continuity = ByTranscript
		if context, err = e.transcript(session, len(req.Prompt)); err != nil {
			return fail(storeFailed)
		}
	}
	if continuity == ByResume && c.ResumeID == "" {
		return fail(refuse(http.StatusConflict, "this session cannot be continued from here"))
	}
	// A session continued by own replay cannot go back to the vendor's
	// resume, as after a restart with -replay=false: its resume_id names only
	// its last turn's own fresh process, so every earlier turn would be lost
	// without a sign.
	if continuity == ByResume {
		used, err := e.cfg.Store.UsedReplay(session)
		if err != nil {
			return fail(storeFailed)
		}
		if used {
			return fail(refuse(http.StatusConflict, replayOnly))
		}
	}
	// The session decides its folder. One sent that differs is refused,
	// never quietly replaced.
	if req.Folder != "" && filepath.Clean(req.Folder) != filepath.Clean(c.Folder) {
		return fail(refuse(http.StatusBadRequest, "folder is not this session's folder "+strconv.Quote(c.Folder)+
			"; leave folder out to use the session's"))
	}
	t := turn{harness: agent, folder: c.Folder, folderRef: c.FolderRef, session: session, id: req.Turn, resumeID: c.ResumeID, replayFrom: from,
		recoveredFrom: recovered, recoveredWhy: recoveredWhy, continuity: continuity, model: model,
		readOnly: req.ReadOnly, prompt: req.Prompt, context: context, key: req.IdempotencyKey,
		agentID: agentID, access: access, keyEnv: keyEnv, origin: req.Origin, root: req.Root, depth: req.Depth,
		token: req.Token, apiURL: req.APIURL, placeholder: req.Placeholder, signin: req.Signin}
	if from != "" || continuity == ByTranscript {
		// A fresh child: the history comes from the gateway, or from the
		// transcript in its prompt.
		t.resumeID = ""
	}
	return e.start(t)
}

// How a session's next turn reaches its earlier turns.
const (
	// ByReplay: the child starts fresh, with no vendor resume, and the
	// gateway puts the earlier turn's whole history in front of each of its
	// model requests (package replay). The turn's turn.started names that
	// turn in replay_from.
	ByReplay = "replay"
	// ByResume: the vendor's own resume, from the session's resume_id.
	ByResume = "resume"
	// ByTranscript: the turn runs on another harness than the session's
	// latest turn. The child starts fresh, and its prompt is the earlier
	// turns as plain text (transcript.go), then the person's words. The
	// turn's turn.started keeps that text in context.
	ByTranscript = "transcript"
)

// Continuity says how agent's next turns continue a session. A harness with a
// replay format (harness.Info.Replay) uses own replay when ReplayBase is set:
// claude and codex send the whole conversation on every request, so the
// gateway can rebuild what the vendor's resume would send. Any other agent,
// and every agent with ReplayBase nil, use the vendor's resume.
func (e *Engine) Continuity(agent string) string {
	if h, ok := harness.Get(agent); ok && h.Info().Replay != "" && e.cfg.ReplayBase != nil {
		return ByReplay
	}
	return ByResume
}

// replayLayout says own replay knows the requests agent sends with access:
// on a subscription always, on OpenRouter only when the harness says so
// (harness.Info.ReplayOnOpenRouter). A turn of a harness that does not
// neither gives nor takes a replayed history: a turn after one, or on one,
// gets the transcript.
func replayLayout(agent, access string) bool {
	if h, ok := harness.Get(agent); ok && h.Info().ReplayOnOpenRouter {
		return true
	}
	return access != harness.OpenRouter
}

// TurnRunning is the refusal of a turn sent to a session whose turn runs.
const TurnRunning = "a turn is already running in this session"

// replayOnly refuses a vendor resume for a session that used own replay.
const replayOnly = "this session cannot be continued from here: it was continued by own replay, and its resume_id holds only its last turn; start agentbus with replay on (the default) to continue it"

// turn is one start of an agent: a new session's first, or a session's next.
type turn struct {
	harness, folder, session string
	folderRef                string // the person's folder, by its reference
	id                       string // the turn id
	resumeID                 string // a next turn by resume: the conversation it continues; empty for a first
	replayFrom               string // a next turn by replay: the earlier turn whose history the gateway replays
	recoveredFrom            string // a next turn by replay after a turn that did not finish: that turn
	recoveredWhy             string // and how it ended
	continuity               string // a next turn: ByReplay or ByResume; empty for a first
	model                    string
	readOnly                 bool
	prompt                   string
	context                  string // put before prompt: the earlier turns as text (ByTranscript)
	key                      string // the caller's idempotency key, or ""
	agentID, access          string // the saved agent and how it reaches its model
	keyEnv                   string // for openrouter: the name of the key's variable
	origin                   *event.Origin
	root                     string // the request tree's root session; "" for this one
	depth                    int
	token, apiURL            string // control's token for the turn and its API (Request.Token)
	placeholder              bool   // the token is the child's model credential
	signin                   string // why control has no credential for the agent now
}

// start stores session.opened and turn.started, starts t's child in a slot
// already taken for t.session, and stores session.closed when it exits. A
// failed session.opened write starts nothing: the store is the only record of
// the child, and of its agent and folder for TurnSink. A child that cannot
// start after session.opened is stored, or whose turn.started cannot be
// stored, gets its session.closed with the reason. The slot is freed when
// nothing started, or once the child's session.closed is stored.
func (e *Engine) start(t turn) (*Turn, error) {
	h, _ := harness.Get(t.harness) // Start and next checked it (KnownHarness)
	started := false
	var tok string // the turn's own token, once minted
	defer func() {
		if !started {
			e.turnTokens.revoke(tok)
			e.turnRules.clear(t.session, t.id)
			e.runs.release(nil, t.session)
		}
	}()
	// The person's settings, read for every turn at one moment, before
	// anything is stored: the policy, the instructions and the settings
	// version the turn records. A policy that cannot be used stops the
	// turn: it never runs without it. Instructions that cannot be used stop
	// it too.
	snap, err := e.settingsNow()
	if err != nil {
		reason := "permissions_file"
		var se *settings.Error
		if errors.As(err, &se) && se.Setting == "instructions" {
			reason = "instructions_file"
		}
		return nil, e.unusable(t.harness, reason, err)
	}
	perm := snap.Policy
	hint := e.fillInstructions(snap.Instructions, t.session)
	// The gateway refuses a command of this turn that forbid names, for
	// claude and codex alike (gateway/guard.go). A harness whose format the
	// guard does not read refuses a turn with forbid (Harness.Refuse).
	e.turnRules.set(t.session, t.id, perm.Forbid)
	baseURL := e.cfg.BaseURL + "/s/" + t.session + "/t/" + t.id
	if e.cfg.Wire == nil {
		return nil, refuse(http.StatusNotFound, "this service starts no turns")
	}
	acc := Access{Kind: t.access, KeyEnv: t.keyEnv, Model: t.model}
	if t.placeholder {
		acc.Placeholder = t.token
	}
	l, err := e.cfg.Wire(t.harness, baseURL, acc)
	var refusal *Refusal
	switch {
	case errors.As(err, &refusal):
		e.cfg.Logf("run.refused agent=%s access=%s reason=wire", t.harness, t.access)
		return nil, refusal
	case errors.Is(err, ErrOwnEndpoint):
		return nil, refuse(http.StatusBadRequest, "the service's environment already sends this agent to another endpoint; unset it and restart agentbus")
	case errors.Is(err, ErrNotFound):
		// 400, as the endpoint refusal above: both are the service's own
		// setup, which the person fixes, and nothing is stored. The reply
		// never quotes PATH: it names the person's home folder and tools.
		e.cfg.Logf("run.refused agent=%s program=%s reason=not_on_path", t.harness, t.harness)
		return nil, refuse(http.StatusBadRequest, missingAgent(t.harness))
	case err != nil:
		return nil, refuse(http.StatusInternalServerError, "the agent could not be started")
	}
	if t.signin != "" {
		e.cfg.Logf("run.refused agent=%s access=%s reason=signin", t.harness, t.access)
		return nil, refuse(http.StatusBadRequest, t.signin)
	}
	// The turn's own token (api/tokens.go says what it may do). It goes into
	// the child's environment only, beside the service's address; the main
	// token never does.
	root := t.root
	if root == "" {
		root = t.session
	}
	// A token control minted is control's to end; one minted here ends
	// when the child exits.
	apiURL, childTok := e.cfg.BaseURL, t.token
	if t.token != "" {
		apiURL = t.apiURL
	} else {
		if tok, err = e.turnTokens.mint(Grant{Session: t.session, Turn: t.id, Root: root, Depth: t.depth, ReadOnly: t.readOnly, Harness: t.harness}); err != nil {
			return nil, storeFailed
		}
		childTok = tok
	}
	cmd := exec.Command(l.Path)
	asked := askedBy(t.origin)
	cmd.Env = turnEnv(l.Env, os.Environ(), apiURL, childTok, e.cfg.APIDir, asked)
	if e.cfg.Homes != "" {
		var made bool
		cmd.Env, made, err = e.home(t.session, t.continuity == "", h, cmd.Env)
		// A home made for a turn that does not start goes again: no session
		// owns it.
		defer func() {
			if made && !started {
				_ = e.RemoveHome(t.session)
			}
		}()
		if err != nil {
			e.cfg.Logf("run.refused agent=%s reason=home", t.harness)
			return nil, refuse(http.StatusInternalServerError, "the turn's home folder could not be made, so the turn did not start")
		}
	}
	if len(l.HomeFiles) > 0 {
		if why, err := e.writeHome(t.session, l.HomeFiles); err != nil {
			e.cfg.Logf("run.refused agent=%s reason=%s", t.harness, why)
			return nil, refuse(http.StatusBadRequest, err.Error())
		}
	}
	if why := e.copyHomeFiles(perm, t.session); why != "" {
		e.cfg.Logf("run.refused agent=%s reason=home_files", t.harness)
		return nil, refuse(http.StatusBadRequest, why+", so the turn did not start. Fix it on this runner, or change Home files in the cockpit's Settings.")
	}
	// The tool server ([agent_api] tools), for a harness that takes it, unless
	// the person's own setting for the harness is in the way.
	tools := h.Info().ToolServer && perm.AgentAPI.Tools != nil && *perm.AgentAPI.Tools
	if tools {
		if why := h.OwnTools(cmd.Env, t.folder); why != "" {
			tools = false
			e.cfg.Logf("run.tools session=%s turn=%s left_out=%s", t.session, t.id, why)
		}
	}
	// The turn may apply as its token may: [agent_api] apply on, and not a
	// read-only turn (a review), whose token refuses it (api turnRefusal).
	apply := !t.readOnly && perm.AgentAPI.Apply != nil && *perm.AgentAPI.Apply
	hint = withTools(hint, t.session, tools, apply)
	// The folder's AGENTS.md, then the instructions, then who asked: the
	// same for every harness, each once.
	doc, why := folderDoc(h, t.folder)
	if why != "" {
		e.cfg.Logf("run.doc session=%s turn=%s left_out=%s", t.session, t.id, why)
	}
	askText := askedLine(asked, perm.AgentAPI, e.cfg.APIDir != "", tools)
	added := addedContext(doc, hint, askText)
	hint = joinText(doc, hint, askText)
	perm = withAPI(perm, e.cfg.APIDir != "", t.readOnly)
	if hint != "" {
		if why := h.OwnInstructions(cmd.Env, t.folder); why != "" {
			hint, added = "", nil
			e.cfg.Logf("run.hint session=%s turn=%s left_out=%s", t.session, t.id, why)
		}
	}
	// The tool server gives its own instructions when the agent connects.
	if tools {
		text := settings.ToolServerInstructions(apply)
		added = append(added, event.Added{Kind: event.AddedMCPInstructions, Bytes: len(text), Text: text})
	}
	w, why := writesFor(h, t.folder, t.readOnly, perm, e.cfg.Protect)
	if why != "" {
		e.cfg.Logf("run.edit session=%s turn=%s left_out=%s", t.session, t.id, why)
	}
	// Inside the session's sandbox, or closed off from the network, when the
	// policy says so: before anything is stored, a runner that cannot do it
	// refuses the turn. The sandbox closes the network off too.
	var jail wall
	forward, pass := reach(e.cfg.BaseURL, apiURL)
	session, turn := t.session, t.id
	netSpec := netjail.Spec{Forward: forward, Pass: pass, Allow: perm.Network.Allow,
		Refused: func(target, why string) {
			if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.NetworkRefused,
				Time: e.cfg.Now().UTC(), Session: session, Turn: turn, Host: target, Error: why}); err != nil {
				e.cfg.Logf("store.error what=network_refused session=%s turn=%s", session, turn)
			}
		}}
	if perm.Sandbox.Mode == settings.SandboxOn {
		// A read-only turn cannot write its folder in the wall. Nor can a
		// turn that may edit nothing, of a harness whose own sandbox kept it
		// from writing: that one is off in the wall (harness.Turn.Walled).
		readOnly := w.ReadOnly || (h.Info().OwnSandbox && len(w.Edit) == 0)
		box, why := e.sandbox(perm, t.session, t.folder, readOnly, readOnly && h.Info().OwnSandbox,
			append(sandbox.Program(l.Path), e.apiProgram()...), netSpec)
		if why != "" {
			e.cfg.Logf("run.refused agent=%s reason=sandbox", t.harness)
			return nil, refuse(http.StatusBadRequest, why+", so the turn did not start. Fix it on this runner, or turn off \"Run each session in a sandbox\" in the cockpit's Settings (Grants, Sandbox).")
		}
		jail = box
		cmd.Env = setEnv(cmd.Env, box.Env())
	} else if on, why := e.contained(perm); why != "" {
		e.cfg.Logf("run.refused agent=%s reason=network", t.harness)
		return nil, refuse(http.StatusBadRequest, why)
	} else if on {
		j := netjail.New(netSpec)
		jail = j
		cmd.Env = setEnv(cmd.Env, j.Env())
	}
	defer func() {
		if !started && jail != nil {
			jail.Close()
		}
	}()
	spec := turnSpec(hint, t.resumeID, t.model, w, perm, t.context+t.prompt)
	if tools {
		spec.Tools = strings.TrimSuffix(apiURL, "/") + ToolPath
	}
	// A setting the harness has no way to keep refuses the turn, before
	// anything is stored (harness.Harness.Refuse).
	if why := h.Refuse(spec); why != "" {
		e.cfg.Logf("run.refused agent=%s reason=harness_setting", t.harness)
		return nil, refuse(http.StatusBadRequest, why)
	}
	unkept := harness.Unkept(h, spec)
	args := h.Args(spec)
	all := append(append([]string{}, l.Args...), args...)
	cmd.Args = append([]string{l.Path}, all...)
	cmd.Dir = t.folder
	// Stdin stays nil: the child reads an empty stdin. Stdout is read for the
	// vendor's session id, the only thing from it that reaches an event; the
	// work is in the events. Stdout and stderr together, their last
	// RunOutputMax bytes, go to the run's own log and nowhere else.
	ids := &idScanner{harness: h}
	out := &logfile.Tail{Max: RunOutputMax}
	// The end of its stderr alone, for session.closed when it exits with
	// an error (errtail.go).
	errs := &logfile.Tail{Max: ErrorTailKept}
	// Its own session, and so its own process group: stopping it reaches
	// what it started, and the session marks its descendants for procTree,
	// even those that leave the group, as codex's tools do.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	holdID := "hold_" + randomHex(8)
	// Before the child starts, so its first model call finds the agent and
	// folder, and a store that cannot write starts nothing.
	if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionOpened,
		Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Hold: holdID, Harness: t.harness, Folder: t.folder, FolderRef: t.folderRef,
		AgentID: t.agentID, Access: t.access, KeyEnv: t.keyEnv, Model: t.model}); err != nil {
		return nil, storeFailed
	}
	// session.opened is stored, so on any failure below the hold must close,
	// or the session would read open until the next restart.
	closeHoldWhy := func(why string) {
		if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionClosed,
			Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Hold: holdID, Error: why}); err != nil {
			e.cfg.Logf("run.unclosed session=%s turn=%s hold=%s reason=store", t.session, t.id, holdID)
		}
	}
	closeHold := func() { closeHoldWhy("the agent could not be started") }
	// The turn's own turn.started, with the person's words as sent, before the
	// child starts: the turn is listed from the moment it is started, and the
	// turn.started the gateway reads from the child's first model call is the
	// same (session, turn), so the store keeps this one only.
	if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.TurnStarted,
		Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Text: t.prompt, Harness: t.harness, Folder: t.folder, FolderRef: t.folderRef,
		ReplayFrom: t.replayFrom, Continuity: t.continuity, RecoveredFrom: t.recoveredFrom,
		RecoveredReason: t.recoveredWhy, IdempotencyKey: t.key, AgentID: t.agentID, Access: t.access, KeyEnv: t.keyEnv,
		Context: t.context, Model: t.model, Origin: t.origin, SettingsVersion: snap.Revision, Unkept: unkept}); err != nil {
		closeHold()
		return nil, storeFailed
	}
	// What AgentBus gave the turn besides the person's words, so a screen
	// can mark it inside the harness's own prompt.
	if len(added) > 0 {
		if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.ContextAdded,
			Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Added: added}); err != nil {
			closeHold()
			return nil, storeFailed
		}
	}
	// The output goes through pipes of our own, read by our own goroutines,
	// so cmd.Wait returns when the child's process exits, not when the last
	// process holding its output closes it. The turn's token is revoked at
	// that moment: a process the child left behind keeps the pipes open, and
	// must not keep the token working.
	pipes, err := newOutputPipes()
	if err != nil {
		closeHold()
		return nil, storeFailed
	}
	cmd.Stdout, cmd.Stderr = pipes.outW, pipes.errW
	startCmd := cmd.Start
	if jail != nil {
		startCmd = func() error { return jail.Start(cmd) }
	}
	code, msg, lt := e.runs.start(cmd, startCmd, t.session, t.id)
	pipes.closeWriters() // the child holds its own copies
	if code != 0 {
		pipes.closeReaders()
		closeHoldWhy(msg)
		return nil, refuse(code, msg)
	}
	copied := pipes.copy(io.MultiWriter(ids, out), io.MultiWriter(out, errs))
	started = true
	begun := e.cfg.Now()
	kind := "run"
	if t.continuity != "" {
		kind = "message"
	}
	e.cfg.Logf("%s.start session=%s agent=%s turn=%s hold=%s pid=%d", kind, t.session, t.harness, t.id, holdID, cmd.Process.Pid)
	res := &Turn{Session: t.session, ID: t.id, BaseURL: baseURL, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		// The token stops working once the child's own process exits, before
		// its output is collected and before the turn's session.closed is
		// stored: nothing of the turn may read after it.
		e.turnTokens.revoke(tok)
		e.turnRules.clear(t.session, t.id)
		// From here a stop is refused: the child has exited. stopped says a
		// stop came before that.
		stopped, killAt := lt.exit()
		code := ExitCode(cmd.ProcessState, err)
		ended := e.cfg.Now()
		tree := lt.tree
		// A stopped child (a signal's exit code, a stop, or the service
		// stopping) ends its whole tree: a tool it started must not outlive
		// it. A child that ended by itself keeps what it left running.
		tree.exited()
		switch {
		case stopped:
			// The stop's own deadline: no second grace period.
			tree.end(max(0, time.Until(killAt)))
		case stoppedBySignal(code) || e.runs.isStopping():
			tree.end(e.cfg.RunGrace)
		}
		// A process the child left behind can keep its output open; the
		// output is given up on that long after the child exits.
		copied(OutputGrace)
		e.keepOutput(t, all[:len(all)-1], code, ended, out)
		unsettled := e.cfg.Settle != nil && !e.cfg.Settle(t.session, t.id)
		if jail != nil {
			jail.Close()
		}
		e.cfg.Logf("%s.exit session=%s agent=%s turn=%s hold=%s exit=%d stopped=%t duration=%s", kind, t.session, t.harness, t.id, holdID,
			code, stopped, ended.Sub(begun).Round(time.Millisecond))
		var tail string
		if code != 0 && !stopped {
			b, cut := errs.Bytes()
			tail = errorTail(b, cut, append(secretValues(cmd.Env), tok, childTok)...)
		}
		if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionClosed,
			Time: ended.UTC(), Session: t.session, Turn: t.id, Hold: holdID, ExitCode: &code, ResumeID: ids.ID(),
			Unsettled: unsettled, Stopped: stopped, ErrorTail: tail}); err != nil {
			// The session reads open until the next start.
			e.cfg.Logf("run.unclosed session=%s turn=%s hold=%s reason=store", t.session, t.id, holdID)
		}
		e.runs.release(cmd, t.session)
		res.end = Ended{ExitCode: code, Stopped: stopped, Unsettled: unsettled}
		close(res.done)
	}()
	return res, nil
}

// wall is what a turn starts in: netjail's namespace, or the
// session's sandbox.
type wall interface {
	Start(*exec.Cmd) error
	Close()
}

// sandbox builds the wall around one of session's turns in
// folder (read only when readOnly; with no network beyond its gateway and
// the API when offline, see wallNet), with read readable too, or says why it cannot (fixed wording).
// It needs the session's own home, which holds the home_files already
// (copyHomeFiles), so they are readable inside the wall.
// The network goes through the proxy: the hosts in [network] allow when
// contain is on, every host when it is off.
func (e *Engine) sandbox(p settings.Policy, session, folder string, readOnly, offline bool, read []string, net netjail.Spec) (*sandbox.Box, string) {
	if e.cfg.Homes == "" || !SessionShape.MatchString(session) {
		return nil, "the sandbox is on in Settings, and " + NoHomes
	}
	home := filepath.Join(e.cfg.Homes, session)
	if info, err := os.Lstat(home); err != nil || !info.IsDir() {
		return nil, "the sandbox is on in Settings, and this session has no home of its own: it began before homes"
	}
	net = wallNet(p, offline, net)
	box, err := sandbox.New(sandbox.Spec{Copy: folder, ReadOnly: readOnly, Home: home, Read: append(read, p.ReadFolders...), Net: net})
	if err != nil {
		return nil, err.Error()
	}
	return box, ""
}

// wallNet is what the proxy of a walled turn lets through: the
// hosts in [network] allow when contain is on, the public internet when it
// is off. An offline one reaches its gateway and the API only: a turn of a
// harness whose own read-only sandbox kept it off the network, which is
// off in the wall (harness.Info.OwnSandbox).
func wallNet(p settings.Policy, offline bool, net netjail.Spec) netjail.Spec {
	if offline {
		net.Allow, net.Open = nil, false
		return net
	}
	net.Open = p.Network.Contain != settings.ContainOn
	return net
}

// copyHomeFiles copies the policy's home_files into session's own home at
// the start of each turn, sandbox on or off, and takes back any the policy no
// longer lists (sandbox.CopyGrants), or says why it cannot (fixed wording). A
// session with no home of its own gets none, since they would land in this
// user's own home: a service without homes, or a session begun before them.
func (e *Engine) copyHomeFiles(p settings.Policy, session string) string {
	if e.cfg.Homes == "" || !SessionShape.MatchString(session) {
		if len(p.HomeFiles) > 0 {
			e.cfg.Logf("run.home_files session=%s left_out=no_homes", session)
		}
		return ""
	}
	home := filepath.Join(e.cfg.Homes, session)
	if info, err := os.Lstat(home); err != nil || !info.IsDir() {
		if len(p.HomeFiles) > 0 {
			e.cfg.Logf("run.home_files session=%s left_out=no_home", session)
		}
		return ""
	}
	userHome, _ := os.UserHomeDir()
	if err := sandbox.CopyGrants(home, home+grantsRecord, userHome, p.HomeFiles); err != nil {
		return err.Error()
	}
	return ""
}

// grantsRecord ends the name of the file beside a session's home that
// records the home_files copied into it (sandbox.CopyGrants).
// The session cannot write it: it is not in its home.
const grantsRecord = ".grants"

// apiProgram is what a turn reads to run agentbus-api: the link and the
// program it names, one file, never the folder that holds it (which may
// hold anything, such as a download folder or the data folder).
func (e *Engine) apiProgram() []string {
	if e.cfg.APIDir == "" {
		return nil
	}
	return []string{filepath.Join(e.cfg.APIDir, APIClient)}
}

// contained says whether a turn on policy p runs closed off from the
// network, or why it cannot start: the policy asks for it and this runner
// cannot do it.
func (e *Engine) contained(p settings.Policy) (bool, string) {
	if p.Network.Contain != settings.ContainOn {
		return false, ""
	}
	if !netjail.Supported {
		return false, "[network] contain is \"on\" in the permissions, and network control is Linux-only, so the turn did not start. " +
			"Run the agent on a Linux runner, or set contain to \"off\"."
	}
	if err := netjail.Check(); err != nil {
		return false, err.Error() + ", so the turn did not start. Fix it on this runner, or set [network] contain = \"off\" in the permissions to run agents open."
	}
	return true, ""
}

// reach sorts the addresses a turn's agent is given: loopback ones are
// forwarded into its namespace as they are, others pass the proxy as
// host:port.
func reach(urls ...string) (forward, pass []string) {
	for _, s := range urls {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "80"
			if u.Scheme == "https" {
				port = "443"
			}
		}
		host := u.Hostname()
		if host == "localhost" {
			host = "127.0.0.1"
		}
		addr := net.JoinHostPort(host, port)
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			if !slices.Contains(forward, addr) {
				forward = append(forward, addr)
			}
		} else if !slices.Contains(pass, addr) {
			pass = append(pass, addr)
		}
	}
	return forward, pass
}

// setEnv is env with each of set's variables in place of any earlier value.
func setEnv(env, set []string) []string {
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(set, func(s string) bool { return strings.HasPrefix(s, k+"=") }) {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// Stop stops a running turn: SIGTERM to its whole process tree, then SIGKILL
// after the grace period to what still runs. It returns at once; the turn's
// session.closed, with stopped true, comes once the tree has ended. running
// is false when this engine runs no such turn.
func (e *Engine) Stop(turnID string) (running bool) {
	lt := e.runs.find(turnID)
	if lt == nil {
		return false
	}
	killAt := time.Now().Add(e.cfg.RunGrace) // real time, not the store's clock
	running, first := lt.stop(killAt)
	if running && first {
		e.cfg.Logf("run.stop session=%s turn=%s", lt.session, lt.turn)
		lt.tree.signal(syscall.SIGTERM)
		go func() {
			select {
			case <-lt.done:
			case <-time.After(time.Until(killAt)):
				lt.tree.signal(syscall.SIGKILL)
			}
		}()
	}
	return running
}

// Interrupt ends a running turn the way StopAll ends every turn: SIGTERM to
// its tree, SIGKILL after the grace period. Unlike Stop it is not a stop by a
// person: the turn's session.closed does not say stopped, and its exit code
// says the signal. running is false when this engine runs no such turn.
func (e *Engine) Interrupt(turnID string) (running bool) {
	lt := e.runs.find(turnID)
	if lt == nil {
		return false
	}
	lt.mu.Lock()
	exited := lt.exited
	lt.mu.Unlock()
	if exited {
		return false
	}
	e.cfg.Logf("run.interrupt session=%s turn=%s", lt.session, lt.turn)
	lt.tree.signal(syscall.SIGTERM)
	go func() {
		select {
		case <-lt.done:
		case <-time.After(e.cfg.RunGrace):
			lt.tree.signal(syscall.SIGKILL)
		}
	}()
	return true
}

// StopAll refuses new turns, sends every running child's tree SIGTERM, then
// SIGKILL after the grace period, and returns once each has stored its
// session.closed. Call it before the store closes.
func (e *Engine) StopAll() {
	e.runs.mu.Lock()
	e.runs.stopping = true
	e.runs.mu.Unlock()
	e.runs.signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { e.runs.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(e.cfg.RunGrace):
	}
	e.runs.signal(syscall.SIGKILL)
	<-done
}

// turnArgs is what h runs, after the Launch's own arguments: a new
// conversation when resumeID is empty, else the next turn of that one. The
// prompt is always last, after "--", so it is never read as a flag. hint, when
// not empty, goes in as an instruction of the agent's own, never inside the
// prompt (historyHint). perm is the person's permissions.toml (permissions.go).
func turnArgs(h harness.Harness, hint, resumeID, model string, w Writes, perm settings.Policy, prompt string) []string {
	return h.Args(turnSpec(hint, resumeID, model, w, perm, prompt))
}

// turnSpec is what a turn's command line is built from (turnArgs).
func turnSpec(hint, resumeID, model string, w Writes, perm settings.Policy, prompt string) harness.Turn {
	return harness.Turn{Hint: hint, ResumeID: resumeID, Model: model, Writes: w,
		ReadFolders: perm.ReadFolders, Allow: perm.Allow, Forbid: perm.Forbid,
		NoSubagents: perm.Subagents != nil && !*perm.Subagents, Walled: perm.Sandbox.Mode == settings.SandboxOn, Prompt: prompt}
}

// withAPI is perm with the agent's way to the API allowed without asking,
// for a turn that may write: Claude Code refuses a command with a variable in
// it unless the person approves it, which a turn cannot (Claude Code
// 2.1.319, 2026-11-09). What the call may do is the turn token's to say
// (api/tokens.go). forbid still wins over it.
func withAPI(perm settings.Policy, api, readOnly bool) settings.Policy {
	if !api || readOnly {
		return perm
	}
	perm.Allow = append(append([]string(nil), perm.Allow...), APIClient)
	return perm
}

// askedBy is the turn that asked for this one, from its origin; nil for a
// turn the person started, or one that delivers a result.
func askedBy(o *event.Origin) *event.Origin {
	if o == nil || o.Kind != event.OriginAsked || o.Session == "" || o.Turn == "" {
		return nil
	}
	return o
}

// askedLine tells an asked agent where it was asked from and how to read
// that session's changes, which its own folder may not hold: a new session
// gets a copy of the person's folder. "" when its token may not read them
// ([agent_api] read off, or scope session).
func askedLine(asked *event.Origin, api settings.AgentAPI, client, tools bool) string {
	if asked == nil || (api.Read != nil && !*api.Read) || api.Scope == settings.ScopeSession {
		return ""
	}
	if tools {
		return "Another agent asked for this work, from session " + asked.Session + ", turn " + asked.Turn +
			" (also in AGENTBUS_ASKED_BY_SESSION and AGENTBUS_ASKED_BY_TURN). Your folder may not hold its changes. " +
			"To read what that turn changed, with each file's diff, call turn_diff with turn " + asked.Turn + " and session " + asked.Session +
			". Its files are null while that turn still runs; read it again once its state is no longer running. " +
			"That session's earlier turns: list_turns with session " + asked.Session + "."
	}
	get := `agentbus-api GET "%s"`
	if !client {
		get = `curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL%s"`
	}
	diff := fmt.Sprintf(get, "/api/v0/turns/"+asked.Turn+"/diff?session="+asked.Session)
	list := fmt.Sprintf(get, "/api/v0/turns?session="+asked.Session)
	return "Another agent asked for this work, from session " + asked.Session + ", turn " + asked.Turn +
		" (also in AGENTBUS_ASKED_BY_SESSION and AGENTBUS_ASKED_BY_TURN, and in your own turn's origin in " +
		fmt.Sprintf(get, "/api/v0/turns") + "). Your folder may not hold its changes. " +
		"To read what that turn changed, with each file's diff, run:\n" + diff +
		"\nIts files are null while that turn still runs; read it again once its state is no longer running. " +
		"That session's earlier turns, each with its own diff: " + list
}

// ToolPath is control's tool server, under a turn's API address: the API as
// agentbus MCP tools over MCP (api/mcp.go), for [agent_api] tools.
const ToolPath = "/api/v0/mcp"

// APIClient is the name of the API client on a turn's PATH (Config.APIDir).
const APIClient = settings.APIClient

// OutputGrace is how long a run waits for its child's output after the child
// exits, for a process the child left behind that keeps it open.
const OutputGrace = 5 * time.Second

// outputPipes are a child's stdout and stderr.
type outputPipes struct {
	outR, outW, errR, errW *os.File
}

func newOutputPipes() (*outputPipes, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	return &outputPipes{outR, outW, errR, errW}, nil
}

func (p *outputPipes) closeWriters() { p.outW.Close(); p.errW.Close() }
func (p *outputPipes) closeReaders() { p.outR.Close(); p.errR.Close() }

// copy copies the child's stdout to stdout and its stderr to stderr. The
// function it returns waits up to grace for both to end, then closes the
// pipes, and returns once nothing more is written.
func (p *outputPipes) copy(stdout, stderr io.Writer) func(grace time.Duration) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stdout, p.outR); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stderr, p.errR); done <- struct{}{} }()
	return func(grace time.Duration) {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		for n := 0; n < 2; n++ {
			select {
			case <-done:
			case <-timer.C:
				p.closeReaders() // each copy then ends with an error
				for ; n < 2; n++ {
					<-done
				}
				return
			}
		}
		p.closeReaders()
	}
}

// RunOutputMax bounds what a run's log keeps of one child's output, and of
// the whole file across a session's turns.
const RunOutputMax = 1 << 20

// keepOutput hands one child's output to RunLog after a header line: the time
// it ended, the agent, its exit code and its arguments. args never holds the
// prompt, which is always the last argument and is left out by the caller.
func (e *Engine) keepOutput(t turn, args []string, code int, ended time.Time, out *logfile.Tail) {
	if e.cfg.RunLog == nil {
		return
	}
	quoted := make([]string, len(args))
	for i, s := range args {
		quoted[i] = strconv.Quote(s)
	}
	b, cut := out.Bytes()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=== %s agent=%s exit=%d args=[%s <prompt>]\n", ended.UTC().Format(time.RFC3339), t.harness, code,
		strings.Join(quoted, " "))
	if cut {
		fmt.Fprintf(&buf, "(only the last %d bytes of this turn's output are kept)\n", RunOutputMax)
	}
	buf.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		buf.WriteByte('\n')
	}
	e.cfg.RunLog(t.session, buf.Bytes())
}

// MaxStdoutLine bounds one line of a child's stdout that is read for its
// session id. A longer line is skipped.
const MaxStdoutLine = 1 << 20

// idScanner reads a child's stdout a line at a time and keeps only the
// vendor's session id, as its harness reads it from each line
// (harness.Harness.ResumeID). It holds at most one line of MaxStdoutLine
// bytes, and nothing else.
type idScanner struct {
	harness harness.Harness
	line    []byte
	over    bool // the current line passed MaxStdoutLine
	id      string
}

func (s *idScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !s.over {
			if len(s.line)+len(chunk) > MaxStdoutLine {
				s.over, s.line = true, s.line[:0]
			} else {
				s.line = append(s.line, chunk...)
			}
		}
		if i < 0 {
			break
		}
		s.endLine()
		p = p[i+1:]
	}
	return n, nil
}

func (s *idScanner) endLine() {
	if !s.over && len(s.line) > 0 {
		s.id = s.harness.ResumeID(s.line, s.id)
	}
	s.line, s.over = s.line[:0], false
}

// ID is the session id the output named, or "". Call it after the child's
// output has ended; a last line without a newline counts.
func (s *idScanner) ID() string {
	s.endLine()
	return s.id
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func newEventID() string { return "evt_" + randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ExitCode is a child's exit code, or 128 plus the signal that ended it, as a
// shell reports it.
func ExitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if code := ps.ExitCode(); code >= 0 {
		return code
	}
	if err != nil {
		return 1
	}
	return 0
}

// TurnSink passes every event on to next, first filling agent and folder on
// each turn.started from the session's latest session.opened in the store.
// It keeps no state, so nothing grows with the runs, and a turn whose events
// arrive after its child exited, or after a restart, is still named. A
// session no turn started, such as one pointed at the gateway by hand, has no
// session.opened and keeps them empty.
func (e *Engine) TurnSink(next event.Sink) event.Sink { return TurnSink(e.cfg.Store, next) }

// TurnSink is the engine's TurnSink for a service that runs no turns itself,
// such as a server: the record's sessions are named the same way.
func TurnSink(st *store.Store, next event.Sink) event.Sink { return turnSink{st, next} }

type turnSink struct {
	st   *store.Store
	next event.Sink
}

// Emit implements event.Sink.
func (s turnSink) Emit(ev event.Event) {
	if ev.Kind == event.TurnStarted && ev.Session != "" {
		// A failed read leaves them empty: the sink cannot return an error,
		// and the turn itself must still be stored.
		ev.Harness, ev.Folder, _ = s.st.RunOf(ev.Session)
	}
	s.next.Emit(ev)
}
