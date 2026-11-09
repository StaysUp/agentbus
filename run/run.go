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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/logfile"
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

// Agents are the agents a turn can start.
var Agents = []string{"claude", "codex"}

// Launch is how a run reaches an agent binary: the binary's path, the child's
// whole environment, and the arguments that go before the run's own.
type Launch struct {
	Path string
	Env  []string
	Args []string
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
	// Permissions is the path of permissions.toml, read at the start of every
	// turn. Empty gives every turn the defaults.
	Permissions string
	// Instructions is the path of instructions.md, the text every turn gets
	// as an instruction of its agent's own, read at the start of every turn.
	// Empty, or a missing file, gives DefaultInstructions.
	Instructions string
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

var storeFailed = refuse(http.StatusInternalServerError, "the store could not be read or written")

// Request is one turn to start.
type Request struct {
	// Session continues that session. Empty starts a new one, which needs
	// Agent and Folder.
	Session string
	// NewSession is the id of a new session; empty picks one.
	NewSession string
	// Turn is the turn's id; empty picks one, t_ and 16 hex characters.
	Turn           string
	Agent, Folder  string
	Prompt, Model  string
	ReadOnly       bool
	IdempotencyKey string // stored on turn.started, as the caller's key
	// AgentID, Access and KeyEnv: the saved agent and how it reaches its
	// model (Access). A new session needs Agent; for a next turn they are
	// optional. Set with Session, Agent (the harness) and these run this turn
	// on another agent, in the session's folder; left out, the turn runs on
	// the agent of the session's latest turn, read from the record.
	AgentID, Access, KeyEnv string
	// Origin, when set, is stored on turn.started: who started the turn
	// when the person did not.
	Origin *event.Origin
	// Root and Depth place the turn in a request tree, for its token (Grant):
	// Root is the session the person's turn ran in, Depth how many agents
	// asked in a row before it. Empty Root means the turn's own session.
	Root  string
	Depth int
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

// agentProducts names the product that installs each agent's program, for the
// missing-agent reply.
var agentProducts = map[string]string{"claude": "Claude Code", "codex": "the Codex CLI"}

// missingAgent is the fixed reply when agent's program is not on the service's
// PATH. The program has the agent's name.
func missingAgent(agent string) string {
	return "agent " + agent + " cannot start: the program " + agent + " is not on the service's PATH. " +
		"Install " + agentProducts[agent] + " and run " + agent + " once to sign in, then try again. " +
		"If it is installed, start agentbus from a shell where " + agent + " runs."
}

// KnownAgent says a turn can start agent.
func KnownAgent(agent string) bool {
	_, ok := turnArgs(agent, "", "", "", Writes{}, Permissions{}, "")
	return ok
}

// CheckPrompt says what is wrong with a prompt and a model, or "".
func CheckPrompt(prompt, model string) string {
	if prompt == "" || len(prompt) > MaxPrompt || strings.IndexByte(prompt, 0) >= 0 {
		return "prompt must be from 1 byte to 256 KiB, with no NUL byte"
	}
	if model != "" && !ModelShape.MatchString(model) {
		return "model must match " + ModelShape.String()
	}
	return ""
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
	if !KnownAgent(req.Agent) {
		return nil, refuse(http.StatusBadRequest, "agent must be claude or codex")
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
	return e.start(turn{agent: req.Agent, folder: req.Folder, session: id, id: req.Turn,
		model: req.Model, readOnly: req.ReadOnly, prompt: req.Prompt, key: req.IdempotencyKey,
		agentID: req.AgentID, access: req.Access, keyEnv: req.KeyEnv, origin: req.Origin, root: req.Root, depth: req.Depth})
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
		return fail(refuse(http.StatusConflict, turnRunning))
	}
	if !KnownAgent(c.Agent) {
		return fail(refuse(http.StatusConflict, "this session cannot be continued from here"))
	}
	// The turn runs on the agent it names, or on the agent of the session's
	// latest turn. The folder is always the session's.
	// The model too: a turn that names neither keeps the model the latest
	// turn ran with, read from the same event as its agent, so the two never
	// disagree, whichever turns started.
	agent, agentID, access, keyEnv, model := c.Agent, c.AgentID, c.Access, c.KeyEnv, req.Model
	if req.Agent == "" && model == "" {
		model = c.Model
	}
	if req.Agent != "" {
		if !KnownAgent(req.Agent) {
			return fail(refuse(http.StatusBadRequest, "agent must be claude or codex"))
		}
		if req.Access != "" && req.Access != "subscription" && req.Access != "openrouter" {
			return fail(refuse(http.StatusBadRequest, "access must be subscription or openrouter"))
		}
		agent, agentID, access, keyEnv = req.Agent, req.AgentID, req.Access, req.KeyEnv
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
	if agent != c.Agent || (continuity == ByReplay && !(replayLayout(agent, access) && replayLayout(c.Agent, c.Access))) {
		continuity = ByTranscript
		if context, err = e.transcript(session, len(req.Prompt)); err != nil {
			return fail(storeFailed)
		}
	}
	if continuity == ByReplay {
		plan, err := e.cfg.ReplayBase(c.Agent, session)
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
	t := turn{agent: agent, folder: c.Folder, session: session, id: req.Turn, resumeID: c.ResumeID, replayFrom: from,
		recoveredFrom: recovered, recoveredWhy: recoveredWhy, continuity: continuity, model: model,
		readOnly: req.ReadOnly, prompt: req.Prompt, context: context, key: req.IdempotencyKey,
		agentID: agentID, access: access, keyEnv: keyEnv, origin: req.Origin, root: req.Root, depth: req.Depth}
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

// Continuity says how agent's next turns continue a session. claude and codex
// use own replay when ReplayBase is set: both send the whole conversation on
// every request, so the gateway can rebuild what the vendor's resume would
// send. Any other agent, and both with ReplayBase nil, use the vendor's
// resume.
func (e *Engine) Continuity(agent string) string {
	if (agent == "claude" || agent == "codex") && e.cfg.ReplayBase != nil {
		return ByReplay
	}
	return ByResume
}

// replayLayout says own replay knows the requests agent sends with access:
// claude with either access, codex on its subscription. codex on OpenRouter
// sends its input items without the content kinds codex puts on them for
// chatgpt.com, which is how the codex format finds the prompt and the
// context (codex-cli 0.167.0, 2026-11-09), so its turns neither give nor take
// a replayed history: a turn after one, or on one, gets the transcript.
func replayLayout(agent, access string) bool {
	return agent == "claude" || access != "openrouter"
}

const turnRunning = "a turn is already running in this session"

// replayOnly refuses a vendor resume for a session that used own replay.
const replayOnly = "this session cannot be continued from here: it was continued by own replay, and its resume_id holds only its last turn; start agentbus with replay on (the default) to continue it"

// turn is one start of an agent: a new session's first, or a session's next.
type turn struct {
	agent, folder, session string
	id                     string // the turn id
	resumeID               string // a next turn by resume: the conversation it continues; empty for a first
	replayFrom             string // a next turn by replay: the earlier turn whose history the gateway replays
	recoveredFrom          string // a next turn by replay after a turn that did not finish: that turn
	recoveredWhy           string // and how it ended
	continuity             string // a next turn: ByReplay or ByResume; empty for a first
	model                  string
	readOnly               bool
	prompt                 string
	context                string // ByTranscript: the earlier turns as text, put before prompt
	key                    string // the caller's idempotency key, or ""
	agentID, access        string // the saved agent and how it reaches its model
	keyEnv                 string // for openrouter: the name of the key's variable
	origin                 *event.Origin
	root                   string // the request tree's root session; "" for this one
	depth                  int
}

// start stores session.opened and turn.started, starts t's child in a slot
// already taken for t.session, and stores session.closed when it exits. A
// failed session.opened write starts nothing: the store is the only record of
// the child, and of its agent and folder for TurnSink. A child that cannot
// start after session.opened is stored, or whose turn.started cannot be
// stored, gets its session.closed with the reason. The slot is freed when
// nothing started, or once the child's session.closed is stored.
func (e *Engine) start(t turn) (*Turn, error) {
	started := false
	var tok string // the turn's own token, once minted
	defer func() {
		if !started {
			e.turnTokens.revoke(tok)
			e.turnRules.clear(t.session, t.id)
			e.runs.release(nil, t.session)
		}
	}()
	// The person's permissions.toml, read for every turn, before anything is
	// stored. A broken file stops the turn: it never runs without the policy.
	perm, err := e.permissions()
	if err != nil {
		e.cfg.Logf("run.refused agent=%s reason=permissions_file error=%q", t.agent, err.Error())
		return nil, refuse(http.StatusBadRequest, "the permissions file "+PermissionsFile+" in the data folder cannot be used, so the turn did not start: "+
			err.Error()+". Fix the file, or remove it for the defaults, and send the turn again.")
	}
	// The instructions every turn gets, from instructions.md, read for every
	// turn too. A file that cannot be used stops the turn, as the policy does.
	hint, err := e.instructions(t.session)
	if err != nil {
		e.cfg.Logf("run.refused agent=%s reason=instructions_file error=%q", t.agent, err.Error())
		return nil, refuse(http.StatusBadRequest, "the instructions file "+InstructionsFile+" in the data folder cannot be used, so the turn did not start: "+
			err.Error()+". Fix the file, or remove it for the default text, and send the turn again.")
	}
	// The gateway refuses a command of this turn that forbid names, for
	// claude and codex alike (gateway/guard.go).
	e.turnRules.set(t.session, t.id, perm.Forbid)
	baseURL := e.cfg.BaseURL + "/s/" + t.session + "/t/" + t.id
	if e.cfg.Wire == nil {
		return nil, refuse(http.StatusNotFound, "this service starts no turns")
	}
	l, err := e.cfg.Wire(t.agent, baseURL, Access{Kind: t.access, KeyEnv: t.keyEnv, Model: t.model})
	var refusal *Refusal
	switch {
	case errors.As(err, &refusal):
		e.cfg.Logf("run.refused agent=%s access=%s reason=wire", t.agent, t.access)
		return nil, refusal
	case errors.Is(err, ErrOwnEndpoint):
		return nil, refuse(http.StatusBadRequest, "the service's environment already sends this agent to another endpoint; unset it and restart agentbus")
	case errors.Is(err, ErrNotFound):
		// 400, as the endpoint refusal above: both are the service's own
		// setup, which the person fixes, and nothing is stored. The reply
		// never quotes PATH: it names the person's home folder and tools.
		e.cfg.Logf("run.refused agent=%s program=%s reason=not_on_path", t.agent, t.agent)
		return nil, refuse(http.StatusBadRequest, missingAgent(t.agent))
	case err != nil:
		return nil, refuse(http.StatusInternalServerError, "the agent could not be started")
	}
	// The turn's own token (api/tokens.go says what it may do). It goes into
	// the child's environment only, beside the service's address; the main
	// token never does.
	root := t.root
	if root == "" {
		root = t.session
	}
	if tok, err = e.turnTokens.mint(Grant{Session: t.session, Turn: t.id, Root: root, Depth: t.depth, ReadOnly: t.readOnly}); err != nil {
		return nil, storeFailed
	}
	cmd := exec.Command(l.Path)
	cmd.Env = turnEnv(l.Env, os.Environ(), e.cfg.BaseURL, tok, e.cfg.APIDir)
	perm = withAPI(perm, e.cfg.APIDir != "", t.readOnly)
	if t.agent == "codex" && hint != "" && codexOwnInstructions(cmd.Env, t.folder) {
		hint = ""
		e.cfg.Logf("run.hint session=%s turn=%s left_out=codex_developer_instructions_set", t.session, t.id)
	}
	w, why := writesFor(t.agent, t.folder, t.readOnly, perm, e.cfg.Protect)
	if why != "" {
		e.cfg.Logf("run.edit session=%s turn=%s left_out=%s", t.session, t.id, why)
	}
	args, _ := turnArgs(t.agent, hint, t.resumeID, t.model, w, perm, t.context+t.prompt)
	all := append(append([]string{}, l.Args...), args...)
	cmd.Args = append([]string{l.Path}, all...)
	cmd.Dir = t.folder
	// Stdin stays nil: the child reads an empty stdin. Stdout is read for the
	// vendor's session id, the only thing from it that reaches an event; the
	// work is in the events. Stdout and stderr together, their last
	// RunOutputMax bytes, go to the run's own log and nowhere else.
	ids := &idScanner{agent: t.agent}
	out := &logfile.Tail{Max: RunOutputMax}
	// Its own session, and so its own process group: stopping it reaches
	// what it started, and the session marks its descendants for procTree,
	// even those that leave the group, as codex's tools do.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	holdID := "hold_" + randomHex(8)
	// Before the child starts, so its first model call finds the agent and
	// folder, and a store that cannot write starts nothing.
	if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionOpened,
		Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Hold: holdID, Agent: t.agent, Folder: t.folder,
		AgentID: t.agentID, Access: t.access, KeyEnv: t.keyEnv, Model: t.model}); err != nil {
		return nil, storeFailed
	}
	// session.opened is stored, so on any failure below the hold must close,
	// or the session would read open until the next restart.
	closeHold := func() {
		_, _, _ = e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionClosed,
			Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Hold: holdID, Error: "the agent could not be started"})
	}
	// The turn's own turn.started, with the person's words as sent, before the
	// child starts: the turn is listed from the moment it is started, and the
	// turn.started the gateway reads from the child's first model call is the
	// same (session, turn), so the store keeps this one only.
	if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.TurnStarted,
		Time: e.cfg.Now().UTC(), Session: t.session, Turn: t.id, Text: t.prompt, Agent: t.agent, Folder: t.folder,
		ReplayFrom: t.replayFrom, Continuity: t.continuity, RecoveredFrom: t.recoveredFrom,
		RecoveredReason: t.recoveredWhy, IdempotencyKey: t.key, AgentID: t.agentID, Access: t.access, KeyEnv: t.keyEnv,
		Context: t.context, Model: t.model, Origin: t.origin}); err != nil {
		closeHold()
		return nil, storeFailed
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
	code, msg, lt := e.runs.start(cmd, t.session, t.id)
	pipes.closeWriters() // the child holds its own copies
	if code != 0 {
		pipes.closeReaders()
		closeHold()
		return nil, refuse(code, msg)
	}
	copied := pipes.copy(io.MultiWriter(ids, out), out)
	started = true
	begun := e.cfg.Now()
	kind := "run"
	if t.continuity != "" {
		kind = "message"
	}
	e.cfg.Logf("%s.start session=%s agent=%s turn=%s hold=%s pid=%d", kind, t.session, t.agent, t.id, holdID, cmd.Process.Pid)
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
		e.cfg.Logf("%s.exit session=%s agent=%s turn=%s hold=%s exit=%d stopped=%t duration=%s", kind, t.session, t.agent, t.id, holdID,
			code, stopped, ended.Sub(begun).Round(time.Millisecond))
		_, _, _ = e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionClosed,
			Time: ended.UTC(), Session: t.session, Turn: t.id, Hold: holdID, ExitCode: &code, ResumeID: ids.ID(),
			Unsettled: unsettled, Stopped: stopped})
		e.runs.release(cmd, t.session)
		res.end = Ended{ExitCode: code, Stopped: stopped, Unsettled: unsettled}
		close(res.done)
	}()
	return res, nil
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

// turnArgs is what an agent runs, after the Launch's own arguments: a new
// conversation when resumeID is empty, else the next turn of that one. The
// prompt is always last, after "--", so it is never read as a flag. hint, when
// not empty, goes in as an instruction of the agent's own, never inside the
// prompt (historyHint). perm is the person's permissions.toml (permissions.go).
// ok is false for an agent a turn cannot start.
func turnArgs(agent, hint, resumeID, model string, w Writes, perm Permissions, prompt string) (args []string, ok bool) {
	readOnly := w.ReadOnly
	switch agent {
	case "claude":
		// --output-format json prints one result object with the session_id.
		a := []string{"-p", "--output-format", "json"}
		// The hint goes at the end of Claude Code's system prompt. Own replay
		// keeps each request's system prompt as the agent sent it, and the
		// text is the same on every turn of the session, so the main thread
		// is still recognised and the prompt cache still holds.
		if hint != "" {
			a = append(a, "--append-system-prompt", hint)
		}
		if resumeID != "" {
			a = append(a, "--resume", resumeID)
		}
		// One --allowedTools, comma-joined (Claude Code 2.1.318,
		// 2026-11-05): what the tools may do without asking.
		allowed := readRules(perm.ReadFolders)
		if readOnly {
			// --tools removes every other tool. --allowedTools only approves
			// without asking, and the person's settings could approve more.
			// allow never widens a read-only turn.
			allowed = append([]string{"Read", "Grep", "Glob"}, allowed...)
			a = append(a, "--tools", "Read,Grep,Glob", "--allowedTools", strings.Join(allowed, ","))
			// --tools restricts built-in tools only: an MCP server from the
			// person's settings could still write. --strict-mcp-config with
			// no --mcp-config loads no MCP server at all.
			a = append(a, "--strict-mcp-config")
		} else {
			// edit: Edit(<folder>/**) lets Edit and Write change files there
			// without asking; one Edit rule covers every built-in tool that
			// edits files. Without it a -p turn cannot edit at all: nobody
			// is there to approve.
			allowed = append(allowed, editRules(w.Edit)...)
			// allow: each command runs without asking, as Bash(<cmd>:*)
			// (Claude Code 2.1.319, 2026-11-07, checked live with a rule
			// that has a space in it).
			if len(perm.Allow) > 0 {
				allowed = append(allowed, bashRules(perm.Allow))
			}
			if len(allowed) > 0 {
				a = append(a, "--allowedTools", strings.Join(allowed, ","))
			}
		}
		// forbid: never run, even when allow or the person's settings
		// approve it; a deny wins over an allow (checked live, 2026-11-07).
		// The gateway's guard refuses them too, in the shapes this rule
		// misses, such as git -C repo push (gateway/guard.go). The data
		// folder's own files are never edited: a deny on each, which wins
		// over the edit rule and the person's settings alike.
		var denied []string
		if len(perm.Forbid) > 0 {
			denied = append(denied, bashRules(perm.Forbid))
		}
		if !readOnly {
			denied = append(denied, protectRules(w.Protect)...)
			// No edit rule: a deny on every tool that edits files, so the
			// person's own settings (an acceptEdits mode, an Edit allow
			// rule) cannot approve one either. A deny wins over any allow.
			if len(w.Edit) == 0 {
				denied = append(denied, "Edit", "Write", "NotebookEdit")
			}
		}
		if len(denied) > 0 {
			a = append(a, "--disallowedTools", strings.Join(denied, ","))
		}
		if model != "" {
			a = append(a, "--model", model)
		}
		return append(a, "--", prompt), true
	case "codex":
		a := []string{"exec", "--skip-git-repo-check"}
		if resumeID == "" {
			// --json prints events as lines; the first is thread.started
			// with the thread_id (codex-cli 0.164.0, 2026-11-05).
			a = append(a, "--json")
		}
		// The sandbox is always named, so a turn gets what read_only says
		// and not codex's own default, which for `codex exec` is read-only
		// (acceptance soak, 2026-11-06: every turn with read_only false
		// could not write). `codex exec resume` takes no --sandbox of its
		// own. Given to exec before resume it reaches the resumed turn: the
		// turn's recorded sandbox policy follows it (codex-cli 0.164.0,
		// 2026-11-05).
		// workspace-write only with an edit rule: edit = "none", or a
		// folder codex could not be kept out of the data folder's files
		// in, runs read-only (writesFor).
		write := !readOnly && len(w.Edit) > 0
		a = append(a, "--sandbox", codexSandbox(!write))
		// The hint as codex's developer_instructions, which codex puts in a
		// developer message of the context before the prompt; own replay
		// keeps it from the session's first turn.
		if hint != "" {
			a = append(a, "-c", "developer_instructions="+tomlString(hint))
		}
		// A turn that may write may also reach the network, as Claude Code
		// can under typical settings: the person's decision, 2026-11-06. Per
		// turn, on the command line; ~/.codex/config.toml is never edited.
		if write {
			a = append(a, codexNetwork()...)
		}
		// permissions.toml gives codex no flags. read_folders: codex's
		// sandbox reads every folder already, in read-only and in
		// workspace-write, and its --add-dir makes a folder writable, which
		// read_folders never means (codex-cli 0.164.0, 2026-11-07, checked
		// with codex sandbox). allow: codex has no per-command flag. forbid
		// reaches codex through the gateway's guard (gateway/guard.go).
		if resumeID != "" {
			a = append(a, "resume", "--json")
		}
		if model != "" {
			a = append(a, "--model", model)
		}
		a = append(a, "--")
		if resumeID != "" {
			// After "--" too, so an id can never be read as a flag.
			a = append(a, resumeID)
		}
		return append(a, prompt), true
	}
	return nil, false
}

// withAPI is perm with the agent's way to the API allowed without asking,
// for a turn that may write: Claude Code refuses a command with a variable in
// it unless the person approves it, which a turn cannot (Claude Code
// 2.1.319, 2026-11-09). What the call may do is the turn token's to say
// (api/tokens.go). forbid still wins over it.
func withAPI(perm Permissions, api, readOnly bool) Permissions {
	if !api || readOnly {
		return perm
	}
	perm.Allow = append(append([]string(nil), perm.Allow...), APIClient)
	return perm
}

// APIClient is the name of the API client on a turn's PATH (Config.APIDir).
const APIClient = "agentbus-api"

// codexOwnInstructions says the person set codex's developer_instructions
// themselves, in $CODEX_HOME/config.toml (~/.codex by default, read from env,
// the child's environment) or in a .codex/config.toml of the folder or any
// folder above it: codex reads a project's config from its root down to the
// folder it runs in (codex-cli 0.164.0, 2026-11-07, found in review). Every
// folder above is checked, so a file codex would skip still leaves the hint
// out, which keeps the person's setting either way. A -c value
// would replace theirs, so a turn then gets no hint: their own setting wins.
// A top-level key is a line naming it before the first table header; nothing
// else of the file is read, and the file is never written.
func codexOwnInstructions(env []string, folder string) bool {
	var home, codexHome string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME":
			home = v
		case "CODEX_HOME":
			codexHome = v
		}
	}
	if codexHome == "" && home != "" {
		codexHome = filepath.Join(home, ".codex")
	}
	// A relative CODEX_HOME is read by codex from its own working folder,
	// the turn's folder, not from the service's (found in review).
	if codexHome != "" && !filepath.IsAbs(codexHome) {
		codexHome = filepath.Join(folder, codexHome)
	}
	if codexHome != "" && topLevelKey(filepath.Join(codexHome, "config.toml"), "developer_instructions") {
		return true
	}
	// Both the folder as given and its real path: codex walks the physical
	// parents of a folder reached through a symlink (found in review).
	starts := []string{filepath.Clean(folder)}
	if real, err := filepath.EvalSymlinks(folder); err == nil && real != starts[0] {
		starts = append(starts, real)
	}
	for _, start := range starts {
		for dir := start; ; dir = filepath.Dir(dir) {
			if topLevelKey(filepath.Join(dir, ".codex", "config.toml"), "developer_instructions") {
				return true
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return false
}

// topLevelKey says a TOML file sets key before its first table header. A
// missing or unreadable file sets nothing.
func topLevelKey(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return false
		}
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.Trim(strings.TrimSpace(k), `"'`) == key {
			return true
		}
	}
	return false
}

// tomlString quotes s as a TOML basic string, for a codex -c value, which
// codex reads as TOML. A JSON string is one: TOML takes the same escapes,
// \uXXXX among them.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// codexSandbox is the value of codex's --sandbox for a turn: read-only when
// read_only is true, else workspace-write, which lets codex write in the
// turn's folder (and its temporary folders) and nowhere else (codex-cli
// 0.164.0, `codex exec --help`). danger-full-access is never used.
func codexSandbox(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "workspace-write"
}

// codexNetwork turns on network access in codex's workspace-write sandbox,
// which has it off by default. Without it a codex turn could not reach the
// service with its turn token: curl exited 7 (acceptance soak, 2026-11-06,
// item 2). The key is the [sandbox_workspace_write] table's network_access
// (codex-cli 0.164.0). A read-only turn never gets it.
func codexNetwork() []string {
	return []string{"-c", "sandbox_workspace_write.network_access=true"}
}

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
	fmt.Fprintf(&buf, "=== %s agent=%s exit=%d args=[%s <prompt>]\n", ended.UTC().Format(time.RFC3339), t.agent, code,
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

var resumeIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// idScanner reads a child's stdout a line at a time and keeps only the
// vendor's session id: the session_id of claude's "type":"result" object, or
// the thread_id of codex's first "type":"thread.started" line. Each line is
// decoded whole as JSON, so text inside a reply cannot pose as the id. It
// holds at most one line of MaxStdoutLine bytes, and nothing else.
type idScanner struct {
	agent string
	line  []byte
	over  bool // the current line passed MaxStdoutLine
	id    string
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
		var v struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			ThreadID  string `json:"thread_id"`
		}
		if json.Unmarshal(s.line, &v) == nil {
			switch {
			case s.agent == "claude" && v.Type == "result" && resumeIDShape.MatchString(v.SessionID):
				s.id = v.SessionID
			case s.agent == "codex" && v.Type == "thread.started" && s.id == "" && resumeIDShape.MatchString(v.ThreadID):
				s.id = v.ThreadID
			}
		}
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
		ev.Agent, ev.Folder, _ = s.st.RunOf(ev.Session)
	}
	s.next.Emit(ev)
}
