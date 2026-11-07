package api

// POST /api/v0/turns: a program starts a turn, an agent with a prompt. The
// service makes a new session and a new turn id, stores session.opened (with
// the agent and folder) and the turn's turn.started (with the prompt), starts
// the person's own agent binary as its child, and stores session.closed with
// the exit code and the vendor's own session id (resume_id) when it exits.
// All three carry the turn id. The child's base URL is
// <service>/s/<session>/t/<turn>, so the gateway gives every event of the
// child's model calls the same turn id. Every turn.started of the session gets
// the agent and folder of its latest session.opened (TurnSink).
//
// The file and the service log keep the word run (run.start, run.exit): a run
// is the child process that does one turn.
//
// With session in the body, the same endpoint starts that session's next
// turn, the same way. A claude or codex turn continues by own replay (the
// gateway puts the earlier turn's history in front of its requests); with
// replay off, or for a session that has no history to replay and never used
// replay, it continues the vendor's conversation named by its resume_id
// (Continuity).
// docs/CLIENT_API.md is the contract.

import (
	"bytes"
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
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/logfile"
)

// MaxRuns bounds the runs at once, service-wide. Past it a run gets 429.
const MaxRuns = 4

// MaxRunBody bounds a run's request body. A 256 KiB prompt can take six
// bytes per byte once JSON-escaped.
const MaxRunBody = 2 << 20

// MaxPrompt bounds a run's prompt, in bytes.
const MaxPrompt = 256 << 10

var runModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Launch is how a run reaches an agent binary: the binary's path, the child's
// whole environment, and the arguments that go before the run's own.
type Launch struct {
	Path string
	Env  []string
	Args []string
}

// Wire returns the Launch for agent, with baseURL reaching it through the
// child's environment or arguments. It returns ErrOwnEndpoint or ErrNotFound
// when it cannot.
type Wire func(agent, baseURL string) (Launch, error)

var (
	// ErrOwnEndpoint: the service's environment already sends the agent to
	// another endpoint, whose credentials would then go to the vendor.
	ErrOwnEndpoint = errors.New("api: the agent already has its own endpoint")
	// ErrNotFound: the agent's binary is not on the service's PATH.
	ErrNotFound = errors.New("api: the agent's binary was not found")
)

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

// turnArgs is what an agent runs, after the Launch's own arguments: a new
// conversation when resumeID is empty, else the next turn of that one. The
// prompt is always last, after "--", so it is never read as a flag. hint, when
// not empty, goes in as an instruction of the agent's own, never inside the
// prompt (historyHint). perm is the person's permissions.toml (permissions.go).
// ok is false for an agent the API cannot start.
func turnArgs(agent, hint, resumeID, model string, readOnly bool, perm Permissions, prompt string) (args []string, ok bool) {
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
		if len(perm.Forbid) > 0 {
			a = append(a, "--disallowedTools", bashRules(perm.Forbid))
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
		a = append(a, "--sandbox", codexSandbox(readOnly))
		// The hint as codex's developer_instructions, which codex puts in a
		// developer message of the context before the prompt; own replay
		// keeps it from the session's first turn.
		if hint != "" {
			a = append(a, "-c", "developer_instructions="+tomlString(hint))
		}
		// A turn that may write may also reach the network, as Claude Code
		// can under typical settings: the person's decision, 2026-11-06. Per
		// turn, on the command line; ~/.codex/config.toml is never edited.
		if !readOnly {
			a = append(a, codexNetwork()...)
		}
		// permissions.toml gives codex nothing yet. read_folders: codex's
		// sandbox reads every folder already, in read-only and in
		// workspace-write, and its --add-dir makes a folder writable, which
		// read_folders never means (codex-cli 0.164.0, 2026-11-07, checked
		// with codex sandbox). allow and forbid: codex has no per-command
		// flag; forbid for codex will come from the gateway guard, a later
		// step.
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

// historyHint tells the agent that its session's earlier turns are stored and
// how to search them with its turn token (tokens.go). It names the session
// and nothing that changes between turns, so it is the same text on every
// turn of a session.
func historyHint(session string) string {
	return "Your session's earlier turns are stored by AgentBus. To look something up, run:\n" +
		`curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/sessions/` + session + `/search?q=<text>"` + "\n" +
		`with <text> URL-encoded. Each hit's "read" is a path under $AGENTBUS_URL that returns the whole of what matched, with the same header.`
}

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
// 0.164.0, `codex exec --help`: read-only, workspace-write,
// danger-full-access). danger-full-access is never used.
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

// runs tracks the children the API started.
type runs struct {
	mu       sync.Mutex
	stopping bool
	n        int             // slots taken, starting or running
	busy     map[string]bool // sessions with a run or turn starting or running
	live     map[*exec.Cmd]*liveTurn
	byTurn   map[string]*liveTurn // the same runs, by turn id, for POST /api/v0/turns/{id}/stop
	wg       sync.WaitGroup
}

const badRun = "the body must be one JSON object with prompt, agent and folder (or session), and optionally model and read_only, at most 2 MiB"

// TurnRequest is the body of POST /api/v0/turns. omitempty marks a field as
// optional in the API's description.
type TurnRequest struct {
	Session  string `json:"session,omitempty" doc:"A session the snapshot calls resumable: this turn is its next one. Absent: a new session."`
	Agent    string `json:"agent,omitempty" doc:"Required without session. With session it is the session's own; if sent it must match."`
	Folder   string `json:"folder,omitempty" doc:"An absolute path to an existing directory. Required without session. With session it is the session's own; if sent it must match."`
	Prompt   string `json:"prompt" doc:"From 1 byte to 256 KiB, no NUL byte. Always passed after --."`
	Model    string `json:"model,omitempty" doc:"Passed only as the value of --model. Not remembered between turns."`
	ReadOnly *bool  `json:"read_only,omitempty" doc:"False unless true. True: claude gets Read, Grep and Glob only; codex runs with --sandbox read-only. False: claude keeps its own tools and permission settings; codex runs with --sandbox workspace-write and network access on, so it can edit the folder and reach the network. Read-only codex has no network."`
}

// Started is the answer to a turn's start.
type Started struct {
	BaseURL string `json:"base_url" doc:"The address the agent uses, <service>/s/<id>/t/<turn>. Give it to the agent's environment only."`
	ID      string `json:"id" doc:"The session id: a new one, or the session sent."`
	Turn    string `json:"turn" doc:"The new turn's id. Every event of the turn carries it in turn, and GET /api/v0/turns lists it."`
}

func (a *API) run(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Wire == nil {
		writeError(w, http.StatusNotFound, "no such API path")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var body TurnRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	// Only a body without session starts a new session. A session sent as
	// "" or null is refused by next, never read as absent: that would start
	// a new session in place of the one the caller meant.
	var sent struct {
		Session json.RawMessage `json:"session"`
	}
	if json.Unmarshal(data, &sent) != nil {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	if sent.Session != nil {
		a.next(w, body)
		return
	}
	if _, ok := turnArgs(body.Agent, "", "", "", false, Permissions{}, ""); !ok {
		writeError(w, http.StatusBadRequest, "agent must be claude or codex")
		return
	}
	if len(body.Folder) > MaxFolder || !filepath.IsAbs(body.Folder) || !isDir(body.Folder) {
		writeError(w, http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
		return
	}
	if !promptAndModel(w, body.Prompt, body.Model) {
		return
	}
	id := randomHex(8)
	if code, msg := a.runs.take(id); code != 0 {
		writeError(w, code, msg)
		return
	}
	a.startTurn(w, turn{agent: body.Agent, folder: body.Folder, session: id,
		model: body.Model, readOnly: body.ReadOnly != nil && *body.ReadOnly, prompt: body.Prompt})
}

// next starts the next turn of body.Session, in its own agent and folder.
func (a *API) next(w http.ResponseWriter, body TurnRequest) {
	session := body.Session
	if !sessionID.MatchString(session) {
		writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
		return
	}
	if !promptAndModel(w, body.Prompt, body.Model) {
		return
	}
	c, err := a.cfg.Store.Continuation(session)
	if err != nil {
		a.internal(w)
		return
	}
	if !c.Known {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	// The slot and the session are taken before the session is read again,
	// so a turn that started meanwhile is either refused here as busy or
	// already shows its open session.opened below.
	if code, msg := a.runs.take(session); code != 0 {
		writeError(w, code, msg)
		return
	}
	refuse := func(code int, msg string) {
		a.runs.release(nil, session)
		writeError(w, code, msg)
	}
	if c, err = a.cfg.Store.Continuation(session); err != nil {
		a.runs.release(nil, session)
		a.internal(w)
		return
	}
	if c.Open {
		refuse(http.StatusConflict, turnRunning)
		return
	}
	if _, ok := turnArgs(c.Agent, "", "", "", false, Permissions{}, ""); !ok {
		refuse(http.StatusConflict, "this session cannot be continued from here")
		return
	}
	// How the turn reaches the session's earlier turns depends on the agent
	// (Continuity): by own replay, the child starts fresh and the gateway
	// puts the earlier turn's history in front of its requests; by resume,
	// the vendor's own resume_id.
	var from, recovered, recoveredWhy string
	continuity := a.Continuity(c.Agent)
	if continuity == ByReplay {
		plan, err := a.cfg.ReplayBase(c.Agent, session)
		if err != nil {
			a.runs.release(nil, session)
			a.internal(w)
			return
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
			used, err := a.cfg.Store.UsedReplay(session)
			if err != nil {
				a.runs.release(nil, session)
				a.internal(w)
				return
			}
			if used || c.ResumeID == "" {
				refuse(http.StatusConflict, "this session cannot be continued from here: "+plan.Refused)
				return
			}
			continuity = ByResume
		}
	}
	if continuity == ByResume && c.ResumeID == "" {
		refuse(http.StatusConflict, "this session cannot be continued from here")
		return
	}
	// A session continued by own replay cannot go back to the vendor's
	// resume, as after a restart with -replay=false: its resume_id names only
	// its last turn's own fresh process, so every earlier turn would be lost
	// without a sign.
	if continuity == ByResume {
		used, err := a.cfg.Store.UsedReplay(session)
		if err != nil {
			a.runs.release(nil, session)
			a.internal(w)
			return
		}
		if used {
			refuse(http.StatusConflict, replayOnly)
			return
		}
	}
	// The session decides its agent and folder. One sent that differs is
	// refused, never quietly replaced.
	if body.Agent != "" && body.Agent != c.Agent {
		refuse(http.StatusBadRequest, "agent "+strconv.Quote(body.Agent)+" is not this session's agent "+
			strconv.Quote(c.Agent)+"; leave agent out to use the session's")
		return
	}
	if body.Folder != "" && filepath.Clean(body.Folder) != filepath.Clean(c.Folder) {
		refuse(http.StatusBadRequest, "folder is not this session's folder "+strconv.Quote(c.Folder)+
			"; leave folder out to use the session's")
		return
	}
	t := turn{agent: c.Agent, folder: c.Folder, session: session, resumeID: c.ResumeID, replayFrom: from,
		recoveredFrom: recovered, recoveredWhy: recoveredWhy, continuity: continuity, model: body.Model, readOnly: body.ReadOnly != nil && *body.ReadOnly, prompt: body.Prompt}
	if from != "" {
		t.resumeID = "" // a fresh child: the history comes from the gateway
	}
	a.startTurn(w, t)
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
)

// Continuity says how agent's next turns continue a session. claude and codex
// use own replay when ReplayBase is set: both send the whole conversation on
// every request, so the gateway can rebuild what the vendor's resume would
// send. claude's history is plain Anthropic Messages; codex's is input items,
// its encrypted reasoning sent back byte for byte (measured 2026-11-06). Any
// other agent, and both with ReplayBase nil, use the vendor's resume.
func (a *API) Continuity(agent string) string {
	if (agent == "claude" || agent == "codex") && a.cfg.ReplayBase != nil {
		return ByReplay
	}
	return ByResume
}

const turnRunning = "a turn is already running in this session"

// replayOnly refuses a vendor resume for a session that used own replay.
const replayOnly = "this session cannot be continued from here: it was continued by own replay, and its resume_id holds only its last turn; start agentbus with replay on (the default) to continue it"

// promptAndModel refuses a bad prompt or model with 400.
func promptAndModel(w http.ResponseWriter, prompt, model string) bool {
	if prompt == "" || len(prompt) > MaxPrompt || strings.IndexByte(prompt, 0) >= 0 {
		writeError(w, http.StatusBadRequest, "prompt must be from 1 byte to 256 KiB, with no NUL byte")
		return false
	}
	if model != "" && !runModel.MatchString(model) {
		writeError(w, http.StatusBadRequest, "model must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
		return false
	}
	return true
}

// turn is one start of an agent: a new session's first, or a session's next.
type turn struct {
	agent, folder, session string
	resumeID               string // a next turn by resume: the conversation it continues; empty for a first
	replayFrom             string // a next turn by replay: the earlier turn whose history the gateway replays
	recoveredFrom          string // a next turn by replay after a turn that did not finish: that turn
	recoveredWhy           string // and how it ended
	continuity             string // a next turn: ByReplay or ByResume; empty for a first
	model                  string
	readOnly               bool
	prompt                 string
}

// startTurn gives the turn its id, stores session.opened and turn.started,
// starts t's child in a slot already taken for t.session, stores
// session.closed when it exits, and answers 202. A failed session.opened write
// answers 500 and starts nothing: the store is the only record of the child,
// and of its agent and folder for TurnSink. A child that cannot start after
// session.opened is stored, or whose turn.started cannot be stored, gets its
// session.closed with the reason. The slot is freed when nothing started, or
// once the child's session.closed is stored.
func (a *API) startTurn(w http.ResponseWriter, t turn) {
	started := false
	var tok string // the turn's own token, once minted
	defer func() {
		if !started {
			a.turnTokens.revoke(tok)
			a.runs.release(nil, t.session)
		}
	}()
	// The same shape as the gateway's own turn ids, t_ and 16 hex characters,
	// so a client sees one shape. Random, so two turns never share one.
	// The person's permissions.toml, read for every turn, before anything is
	// stored. A broken file stops the turn: it never runs without the policy.
	perm, err := a.permissions()
	if err != nil {
		a.cfg.Logf("run.refused agent=%s reason=permissions_file error=%q", t.agent, err.Error())
		writeError(w, http.StatusBadRequest, "the permissions file "+PermissionsFile+" in the data folder cannot be used, so the turn did not start: "+
			err.Error()+". Fix the file and send the turn again; a missing file means empty lists.")
		return
	}
	turnID := "t_" + randomHex(8)
	baseURL := a.cfg.BaseURL + "/s/" + t.session + "/t/" + turnID
	l, err := a.cfg.Wire(t.agent, baseURL)
	switch {
	case errors.Is(err, ErrOwnEndpoint):
		writeError(w, http.StatusBadRequest, "the service's environment already sends this agent to another endpoint; unset it and restart agentbus")
		return
	case errors.Is(err, ErrNotFound):
		// 400, as the endpoint refusal above: both are the service's own
		// setup, which the person fixes, and nothing is stored. The reply
		// never quotes PATH: it names the person's home folder and tools,
		// and replies and the service log are what get pasted into issues.
		a.cfg.Logf("run.refused agent=%s program=%s reason=not_on_path", t.agent, t.agent)
		writeError(w, http.StatusBadRequest, missingAgent(t.agent))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the agent could not be started")
		return
	}
	// The turn's own token: read-only, for its own session (tokens.go). It
	// goes into the child's environment only, beside the service's address;
	// the main token never does.
	if tok, err = a.turnTokens.mint(t.session, turnID); err != nil {
		a.internal(w)
		return
	}
	cmd := exec.Command(l.Path)
	cmd.Env = turnEnv(l.Env, os.Environ(), a.cfg.BaseURL, tok)
	hint := historyHint(t.session)
	if t.agent == "codex" && codexOwnInstructions(cmd.Env, t.folder) {
		hint = ""
		a.cfg.Logf("run.hint session=%s turn=%s left_out=codex_developer_instructions_set", t.session, turnID)
	}
	args, _ := turnArgs(t.agent, hint, t.resumeID, t.model, t.readOnly, perm, t.prompt)
	all := append(append([]string{}, l.Args...), args...)
	cmd.Args = append([]string{l.Path}, all...)
	cmd.Dir = t.folder
	// Stdin stays nil: the child reads an empty stdin. Stdout is read for the
	// vendor's session id, the only thing from it that reaches an event; the
	// work is in the events. Stdout and stderr together, their last
	// RunOutputMax bytes, go to the run's own log and nowhere else, so a child
	// that fails before reaching the gateway leaves more than its exit code.
	ids := &idScanner{agent: t.agent}
	out := &logfile.Tail{Max: RunOutputMax}
	// Its own session, and so its own process group: stopping it reaches
	// what it started, and the session marks its descendants for procTree,
	// even those that leave the group, as codex's tools do.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	holdID := "hold_" + randomHex(8)
	// Before the child starts, so its first model call finds the agent and
	// folder, and a store that cannot write starts nothing (the write after
	// Start was ignored, so a child ran with no record).
	if _, _, err := a.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionOpened,
		Time: a.cfg.Now().UTC(), Session: t.session, Turn: turnID, Hold: holdID, Agent: t.agent, Folder: t.folder}); err != nil {
		a.internal(w)
		return
	}
	// session.opened is stored, so on any failure below the hold must close,
	// or the session would read open until the next restart.
	closeHold := func() {
		_, _, _ = a.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionClosed,
			Time: a.cfg.Now().UTC(), Session: t.session, Turn: turnID, Hold: holdID, Error: "the agent could not be started"})
	}
	// The turn's own turn.started, with the person's words as sent, before the
	// child starts: the turn is listed from the moment it is started, and the
	// turn.started the gateway reads from the child's first model call is the
	// same (session, turn), so the store keeps this one only.
	if _, _, err := a.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.TurnStarted,
		Time: a.cfg.Now().UTC(), Session: t.session, Turn: turnID, Text: t.prompt, Agent: t.agent, Folder: t.folder,
		ReplayFrom: t.replayFrom, Continuity: t.continuity, RecoveredFrom: t.recoveredFrom,
		RecoveredReason: t.recoveredWhy}); err != nil {
		closeHold()
		a.internal(w)
		return
	}
	// The output goes through pipes of our own, read by our own goroutines,
	// so cmd.Wait returns when the child's process exits, not when the last
	// process holding its output closes it. The turn's token is revoked at
	// that moment: a process the child left behind keeps the pipes open, and
	// must not keep the token working.
	pipes, err := newOutputPipes()
	if err != nil {
		closeHold()
		a.internal(w)
		return
	}
	cmd.Stdout, cmd.Stderr = pipes.outW, pipes.errW
	code, msg, lt := a.runs.start(cmd, t.session, turnID)
	pipes.closeWriters() // the child holds its own copies
	if code != 0 {
		pipes.closeReaders()
		closeHold()
		writeError(w, code, msg)
		return
	}
	copied := pipes.copy(io.MultiWriter(ids, out), out)
	started = true
	begun := a.cfg.Now()
	kind := "run"
	if t.resumeID != "" || t.replayFrom != "" {
		kind = "message"
	}
	a.cfg.Logf("%s.start session=%s agent=%s turn=%s hold=%s pid=%d", kind, t.session, t.agent, turnID, holdID, cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		// The token stops working once the child's own process exits, before
		// its output is collected and before the turn's session.closed is
		// stored: nothing of the turn may read after it.
		a.turnTokens.revoke(tok)
		// From here a stop is refused: the child has exited. stopped says a
		// stop came before that.
		stopped, killAt := lt.exit()
		code := ExitCode(cmd.ProcessState, err)
		ended := a.cfg.Now()
		tree := lt.tree
		// A stopped child (a signal's exit code, a stop by the person, or the
		// service stopping) ends its whole tree: a tool it started must not
		// outlive it (acceptance soak, 2026-11-06). A child that ended by
		// itself keeps what it left running, as before.
		tree.exited()
		switch {
		case stopped:
			// The stop's own deadline: no second grace period.
			tree.end(max(0, time.Until(killAt)))
		case stoppedBySignal(code) || a.runs.isStopping():
			tree.end(a.cfg.RunGrace)
		}
		// A process the child left behind can keep its output open; the
		// output is given up on that long after the child exits.
		copied(OutputGrace)
		a.keepOutput(t, all[:len(all)-1], code, ended, out)
		unsettled := a.cfg.Settle != nil && !a.cfg.Settle(t.session, turnID)
		a.cfg.Logf("%s.exit session=%s agent=%s turn=%s hold=%s exit=%d stopped=%t duration=%s", kind, t.session, t.agent, turnID, holdID,
			code, stopped, ended.Sub(begun).Round(time.Millisecond))
		_, _, _ = a.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.SessionClosed,
			Time: ended.UTC(), Session: t.session, Turn: turnID, Hold: holdID, ExitCode: &code, ResumeID: ids.ID(),
			Unsettled: unsettled, Stopped: stopped})
		a.runs.release(cmd, t.session)
	}()
	writeJSON(w, http.StatusAccepted, Started{baseURL, t.session, turnID})
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
func (a *API) keepOutput(t turn, args []string, code int, ended time.Time, out *logfile.Tail) {
	if a.cfg.RunLog == nil {
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
	a.cfg.RunLog(t.session, buf.Bytes())
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

// take reserves a slot for session, or says why not.
func (rs *runs) take(session string) (int, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.stopping {
		return http.StatusServiceUnavailable, "the service is stopping"
	}
	if rs.busy[session] {
		return http.StatusConflict, turnRunning
	}
	if rs.n >= MaxRuns {
		return http.StatusTooManyRequests, "4 runs are already running; wait for one to end and retry"
	}
	if rs.busy == nil {
		rs.busy = map[string]bool{}
	}
	rs.busy[session] = true
	rs.n++
	return 0, ""
}

// start starts cmd, the child of turn in session, in a slot already taken,
// unless the service began stopping, and starts tracking its process tree.
func (rs *runs) start(cmd *exec.Cmd, session, turn string) (int, string, *liveTurn) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.stopping {
		return http.StatusServiceUnavailable, "the service is stopping", nil
	}
	if err := cmd.Start(); err != nil {
		return http.StatusInternalServerError, "the agent could not be started", nil
	}
	if rs.live == nil {
		rs.live = map[*exec.Cmd]*liveTurn{}
		rs.byTurn = map[string]*liveTurn{}
	}
	lt := &liveTurn{session: session, turn: turn, tree: newProcTree(cmd.Process.Pid), done: make(chan struct{})}
	rs.live[cmd] = lt
	rs.byTurn[turn] = lt
	rs.wg.Add(1)
	return 0, "", lt
}

func (rs *runs) isStopping() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.stopping
}

// release frees session's slot; cmd is the child that held it, or nil if
// none started.
func (rs *runs) release(cmd *exec.Cmd, session string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.n--
	delete(rs.busy, session)
	if cmd != nil {
		if lt := rs.live[cmd]; lt != nil {
			delete(rs.byTurn, lt.turn)
		}
		delete(rs.live, cmd)
		rs.wg.Done()
	}
}

// signal sends sig to every running child's whole tree: its group and every
// process procTree counts in it.
func (rs *runs) signal(sig syscall.Signal) {
	rs.mu.Lock()
	trees := make([]*procTree, 0, len(rs.live))
	for _, lt := range rs.live {
		trees = append(trees, lt.tree)
	}
	rs.mu.Unlock()
	for _, t := range trees {
		t.signal(sig)
	}
}

// StopRuns refuses new runs, sends every running child's tree SIGTERM, then
// SIGKILL after the grace period, and returns once each has stored its
// session.closed, which comes after what was left of its tree has ended.
// Call it before the store closes.
func (a *API) StopRuns() {
	a.runs.mu.Lock()
	a.runs.stopping = true
	a.runs.mu.Unlock()
	a.runs.signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { a.runs.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(a.cfg.RunGrace):
	}
	a.runs.signal(syscall.SIGKILL)
	<-done
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
// The API keeps no state for it, so nothing grows with the runs, and a turn
// whose events arrive after its child exited, or after a restart, is still
// named. A session no run started, such as one pointed at the gateway by
// hand, has no session.opened and keeps them empty.
func (a *API) TurnSink(next event.Sink) event.Sink { return turnSink{a, next} }

type turnSink struct {
	a    *API
	next event.Sink
}

// Emit implements event.Sink.
func (s turnSink) Emit(e event.Event) {
	if e.Kind == event.TurnStarted && e.Session != "" {
		// A failed read leaves them empty: the sink cannot return an error,
		// and the turn itself must still be stored.
		e.Agent, e.Folder, _ = s.a.cfg.Store.RunOf(e.Session)
	}
	s.next.Emit(e)
}
