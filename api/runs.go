package api

// POST /api/v0/turns: a program starts a turn, an agent with a prompt. Every
// turn runs as a job: this makes a turn job in the queue and answers once a
// runner has started its turn (package runner, which starts it through
// package run). The reply keeps its shape: the session, the turn and its base
// URL. With session in the body, the same endpoint starts that session's next
// turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/staysup/agentbus/agents"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// MaxRunBody bounds a turn's request body. A 256 KiB prompt can take six
// bytes per byte once JSON-escaped.
const MaxRunBody = 2 << 20

// MaxPrompt bounds a turn's prompt, in bytes.
const MaxPrompt = run.MaxPrompt

// DefaultTurnWait bounds how long POST /api/v0/turns waits for a runner to
// start the turn, when Config.TurnWait is zero.
const DefaultTurnWait = 30 * time.Second

var runModel = run.ModelShape

const badRun = "the body must be one JSON object with prompt, agent and folder (or session, and optionally agent), and optionally model, read_only, isolate and idempotency_key, at most 2 MiB"

// TurnRequest is the body of POST /api/v0/turns. omitempty marks a field as
// optional in the API's description.
type TurnRequest struct {
	Session   string `json:"session,omitempty" doc:"A session the snapshot calls resumable: this turn is its next one. Absent: a new session."`
	Agent     string `json:"agent,omitempty" doc:"A saved agent's id, from GET /api/v0/agents: its harness, access and model run the turn. claude and codex also name that harness on its own subscription when no saved agent has that id. Required without session. With session it is optional: absent, the turn runs on the agent of the session's latest turn; sent, this turn runs on that agent, in the session's folder, and later turns that name none keep it. On the same harness as the latest turn the conversation continues by own replay; on another harness the turn gets the earlier turns as plain text (continuity transcript)."`
	Folder    string `json:"folder,omitempty" doc:"An absolute path to a directory on the machine named in connector, that the user its runner runs as can read. Typing it is the grant for this session: that machine's runner checks it and registers it (agentbus up's own; on agentbus server the machine named in connector, or the only one online), and the session runs on that machine for its whole life. The queue keeps only the folder's reference, and GET /api/v0/folders lists the folder from then on. Without session, folder or folder_ref is required. With session it is the session's own; if sent it must match."`
	FolderRef string `json:"folder_ref,omitempty" doc:"A folder's reference, fld_ and 16 hex digits, from GET /api/v0/folders: the turn runs in that folder, on the runner of the machine that holds it. For a new session, instead of folder."`
	Connector string `json:"connector,omitempty" doc:"The machine the session runs on (con_ and 16 hex digits): a runner's connector in GET /api/v0/runners, or a folder's in GET /api/v0/folders. Only the runner on that machine takes the session's work. With folder_ref: the machine that holds the folder. With folder: the machine whose runner checks and registers it. Absent: this service's own machine (agentbus up), or else the only one online; with several online, it is required for folder. The name is kept from before 0.5, when a connector held a machine's folders."`
	Isolate   *bool  `json:"isolate,omitempty" doc:"True unless false, for a new session. True: the session runs in its own copy of folder, exactly as folder is now, uncommitted edits included, made as the isolation setting says (auto, overlay, copy, git or none). False: always folder as it is. Not sent with session."`
	Prompt    string `json:"prompt" doc:"From 1 byte to 256 KiB, no NUL byte. Always passed after --."`
	Model     string `json:"model,omitempty" doc:"Passed only as the value of --model. Absent with agent: the saved agent's model, for a new session or a turn that switches agent; a next turn that names the session's own latest agent keeps the session's model. A session's next turn with no agent and no model keeps the model of the session's latest turn, from its turn.started."`
	ReadOnly  *bool  `json:"read_only,omitempty" doc:"False unless true. True: claude gets Read, Grep and Glob only; codex runs with --sandbox read-only. False: claude keeps its own tools and permission settings, plus the edit rule of the settings (by default it edits its session's folder without asking); with edit = \"session\" codex runs with --sandbox workspace-write and network access on, so it can edit the folder and reach the network, and with edit = \"none\" read-only. Read-only codex has no network."`
	// IdempotencyKey makes a retry safe: a client whose request timed out
	// sends it again with the same key and gets the same turn.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key and the same fields answers 202 with the turn the key first started, existing true, and starts nothing. The same key with other fields gets 409. A request refused with 400 used no key."`
}

// Started is the answer to a turn's start, and to a review's.
type Started struct {
	Session string `json:"session" doc:"The turn's session: a new one, or the session sent. A review's is a session of its own."`
	Turn    string `json:"turn" doc:"The turn's id. Every event of the turn carries it in turn; GET /api/v0/turns/{id} follows it from now on."`
	State   string `json:"state" doc:"running: a runner started it. queued: it waits behind the session's earlier turns (ahead says how many), or for a runner. It runs next, in order."`
	BaseURL string `json:"base_url,omitempty" doc:"The address the agent uses, <service>/s/<session>/t/<turn>. Absent while queued."`
	// Existing marks the answer to a retry: the idempotency key had already
	// made this turn.
	Existing bool `json:"existing,omitempty" doc:"True when idempotency_key had already made this turn, so nothing new was made."`
	Ahead    int  `json:"ahead,omitempty" doc:"With state queued: how many turns of the session, running or waiting, are before this one. Absent when the turn waits only for a runner."`
}

// idempotencyKey is the shape of TurnRequest.IdempotencyKey.
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (a *API) run(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
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
	// "" or null is refused, never read as absent: that would start a new
	// session in place of the one the caller meant.
	var sent struct {
		Session json.RawMessage `json:"session"`
	}
	if json.Unmarshal(data, &sent) != nil {
		writeError(w, http.StatusBadRequest, badRun)
		return
	}
	if body.IdempotencyKey != "" && !idempotencyKey.MatchString(body.IdempotencyKey) {
		writeError(w, http.StatusBadRequest, "idempotency_key must match "+idempotencyKey.String())
		return
	}
	// A turn token asks another agent: its checks (tokens.go) fill the new
	// session's folder, and the job names the asking turn.
	var links queue.Links
	if g := grantOf(r); g != nil {
		var ok bool
		if links, ok = a.askChecks(w, *g, policyOf(r), &body, sent.Session != nil); !ok {
			return
		}
	}
	spec := queue.Spec{Prompt: body.Prompt, Model: body.Model, ReadOnly: body.ReadOnly != nil && *body.ReadOnly}
	if sent.Session != nil {
		if !sessionID.MatchString(body.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return
		}
		if msg := run.CheckPrompt(body.Prompt, body.Model); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		// The session decides its folder. One sent that differs is refused,
		// never quietly replaced. The record of this service knows the
		// sessions its own runner ran. An agent sent runs this turn: a switch.
		c, err := a.cfg.Store.Continuation(body.Session)
		if err != nil {
			a.internal(w)
			return
		}
		if body.Isolate != nil {
			writeError(w, http.StatusBadRequest, "isolate is for a new session; a session keeps its own copy")
			return
		}
		if body.FolderRef != "" || body.Connector != "" {
			writeError(w, http.StatusBadRequest, "folder_ref and connector are for a new session; a session keeps its own folder")
			return
		}
		if body.Agent != "" {
			ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
			if !ok {
				return
			}
			spec.Harness, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
			// The agent's model is used only if the turn switches agent,
			// which the runner knows when the turn starts: the session's
			// latest turn may change while this one waits in the queue.
			if spec.Model == "" {
				spec.AgentModel = ag.Model
			}
		}
		if c.Known && body.Folder != "" && filepath.Clean(body.Folder) != filepath.Clean(c.Folder) {
			writeError(w, http.StatusBadRequest, "folder is not this session's folder "+strconv.Quote(c.Folder)+
				"; leave folder out to use the session's")
			return
		}
		// A session whose turn runs takes the next one too: its job waits in
		// the queue behind the turns before it and runs next, in order. A
		// session the record shows open with no job of this queue behind it
		// (a turn this service did not start) cannot be waited for, so it is
		// refused, before the key is used; a retry of a known key goes on to
		// the queue, which returns its job.
		if c.Open {
			busy, err := a.cfg.Queue.SessionHasWork(queue.DefaultTeam, body.Session)
			if err != nil {
				a.internal(w)
				return
			}
			known, err := a.cfg.Queue.HasKey(queue.DefaultTeam, body.IdempotencyKey)
			if err != nil {
				a.internal(w)
				return
			}
			if !busy && !known {
				writeError(w, http.StatusConflict, run.TurnRunning)
				return
			}
		}
		spec.Session = body.Session
	} else {
		ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
		if !ok {
			return
		}
		if body.FolderRef == "" && (len(body.Folder) > run.MaxFolder || !filepath.IsAbs(body.Folder)) {
			writeError(w, http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
			return
		}
		if msg := run.CheckPrompt(body.Prompt, body.Model); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		spec.Harness, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		if spec.Model == "" && ag.Model != "" {
			spec.Model, spec.ModelFromAgent = ag.Model, true
		}
		spec.Folder, spec.FolderRef, spec.Connector = body.Folder, body.FolderRef, body.Connector
		spec.Isolate = body.Isolate == nil || *body.Isolate
		if body.Folder != "" && body.FolderRef == "" {
			// A typed folder: its machine's runner checks and registers it, and the
			// session goes on by its reference. A retry of a key already used
			// is not checked again: the queue answers with the key's turn, or
			// 409 when the request differs.
			spec.Typed, spec.TypedConnector = body.Folder, body.Connector
			known := false
			if body.IdempotencyKey != "" {
				if known, err = a.cfg.Queue.HasKey(queue.DefaultTeam, body.IdempotencyKey); err != nil {
					a.internal(w)
					return
				}
			}
			ref, conn := "", ""
			if !known {
				var status int
				var msg string
				ref, conn, status, msg = a.typedFolder(r.Context(), body.Folder, body.Connector)
				switch {
				case status == 0 && msg != "":
					return // the caller went away
				case status == http.StatusInternalServerError:
					a.internal(w)
					return
				case msg != "":
					writeError(w, status, msg)
					return
				}
			}
			spec.Folder, spec.FolderRef, spec.Connector = "", ref, conn
		}
		if msg := queue.ResolveFolders(&spec, a.cfg.Folders); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	// Never refused for a busy session: the job waits behind the session's
	// turns that have not ended, as a delivered result does.
	job, created, err := a.cfg.Queue.CreateTurnLinked(queue.DefaultTeam, body.IdempotencyKey, spec, links, queueLimits(policyOf(r)))
	var limit *queue.LimitError
	switch {
	case errors.As(err, &limit):
		writeError(w, http.StatusForbidden, limit.Msg)
		return
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key already started a turn with other fields; send a new key for a new turn")
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusConflict, "this session is closed; it takes no more turns")
		return
	case errors.Is(err, queue.ErrSessionEnded):
		writeError(w, http.StatusConflict, queue.SessionEnded)
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns are waiting; wait for some to end and retry")
		return
	case err != nil:
		a.internal(w)
		return
	}
	a.awaitTurn(w, r.Context(), job, !created)
}

// turnAgent is the saved agent a turn names, or false once it has answered
// why the turn cannot start. A retry of a request whose agent was removed
// since gets a stand-in: the queue matches the key against the request as
// asked, the agent by its id, and answers with the first job.
func (a *API) turnAgent(w http.ResponseWriter, id, key string) (agents.Agent, bool) {
	var retried func() (bool, error)
	if key != "" && sessionID.MatchString(id) {
		retried = func() (bool, error) { return a.cfg.Queue.HasKey(queue.DefaultTeam, key) }
	}
	ag, why, err := agents.Lookup(id, a.resolveAgent, retried)
	switch {
	case err != nil:
		a.internal(w)
		return agents.Agent{}, false
	case why != nil:
		writeError(w, http.StatusBadRequest, why.Error())
		return agents.Agent{}, false
	}
	return ag, true
}

// resolveAgent is the saved agent a turn names, or why it cannot start, in
// fixed words fit for the caller. Without saved agents only claude and codex
// are known, each on its own subscription.
func (a *API) resolveAgent(id string) (agents.Agent, error) {
	if a.cfg.Agents == nil {
		if !run.KnownHarness(id) {
			return agents.Agent{}, errors.New("agent must be " + harness.NameList())
		}
		return agents.Agent{Harness: id, Access: agents.Subscription}, nil
	}
	if !sessionID.MatchString(id) {
		return agents.Agent{}, errors.New("agent must be a saved agent's id; GET /api/v0/agents lists them")
	}
	ag, err := a.cfg.Agents.Resolve(id)
	if err != nil {
		return agents.Agent{}, errors.New("agent " + strconv.Quote(id) + " is not a saved agent; GET /api/v0/agents lists them")
	}
	return ag, nil
}

const noTurns = "this service starts no turns: it keeps no queue"

// awaitTurn answers once the job's turn has started: 202 with the turn. A job
// that ended before its turn started answers why, with the status the runner
// was refused with. A job no runner took within TurnWait answers 202 with
// queued: it stays queued. A job that waits behind its session's earlier
// turns answers queued at once, with how many are ahead.
func (a *API) awaitTurn(w http.ResponseWriter, ctx context.Context, job queue.Job, existing bool) {
	wait := a.cfg.TurnWait
	if wait == 0 {
		wait = DefaultTurnWait
	}
	if job.State == queue.Queued && job.Session != "" {
		ahead, err := a.turnsAhead(job.ID)
		if err != nil {
			a.internal(w)
			return
		}
		if ahead > 0 {
			writeJSON(w, http.StatusAccepted, Started{Session: job.Session, Turn: job.Turn, State: queue.TurnQueued, Existing: existing, Ahead: ahead})
			return
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil {
			a.internal(w)
			return
		}
		if turn := startedTurn(j); turn != "" {
			writeJSON(w, http.StatusAccepted, Started{BaseURL: a.cfg.BaseURL + "/s/" + j.Session + "/t/" + turn, Session: j.Session, Turn: turn,
				State: queue.TurnRunning, Existing: existing})
			return
		}
		if j.Ended != nil {
			a.notStarted(w, j)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			writeJSON(w, http.StatusAccepted, Started{Session: j.Session, Turn: j.Turn, State: queue.TurnQueued, Existing: existing})
			return
		case <-tick.C:
		}
	}
}

// turnsAhead counts the turns the job waits behind: its session's earlier
// turn jobs that have not ended, less those whose turn the record shows
// ended, which are only finishing and free the session in a moment.
func (a *API) turnsAhead(job string) (int, error) {
	jobs, err := a.cfg.Queue.TurnsAhead(queue.DefaultTeam, job)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if turn := startedTurn(j); turn != "" && a.turnEnded(turn) {
			continue
		}
		n++
	}
	return n, nil
}

// startedTurn is the job's turn once an attempt has started it, else "".
func startedTurn(j queue.Job) string {
	for _, at := range j.Attempts {
		if at.TurnStarted {
			return j.Turn
		}
	}
	return ""
}

// notStarted answers for a job that ended before its turn started: the
// refusal the runner recorded, with its status.
func (a *API) notStarted(w http.ResponseWriter, j queue.Job) {
	res, _ := queue.ReadResult(j.Result)
	switch {
	case j.State == queue.Failed && j.Reason == queue.ReasonSetup && res.Refused != "":
		status := res.Status
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		writeError(w, status, res.Refused)
	case j.State == queue.Cancelled:
		writeError(w, http.StatusConflict, "the turn was stopped before it started")
	default:
		writeError(w, http.StatusInternalServerError, "the turn ended before it started: "+queue.TurnState(j.State)+" "+j.Reason)
	}
}

// turnEnded says the record shows the turn ended: its job may still be
// finishing. A turn this record does not hold, as on a server whose runner
// keeps its own, is not known to have ended.
func (a *API) turnEnded(id string) bool {
	list, err := a.cfg.Store.Turns(store.TurnQuery{Turn: id, Limit: 1})
	return err == nil && len(list.Turns) == 1 && list.Turns[0].State != store.TurnRunning
}

// Stopping is the answer to a stop.
type Stopping struct {
	Session string `json:"session" doc:"The turn's session."`
	Turn    string `json:"turn" doc:"The turn being stopped. It reads stopped once its process tree has ended; a queued turn at once."`
}

const (
	turnNotRunning = "this turn has ended, or it is not a turn of this service"
	noTurn         = "no such turn"
)

// POST /api/v0/turns/<id>/stop: a stop cancels the turn's job. A queued one
// ends at once; a running one's runner hears it on its next heartbeat and
// stops the turn's whole process tree.
func (a *API) stopTurn(w http.ResponseWriter, _ *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
	switch {
	case err == nil:
		if j.Ended != nil || (started(j) && a.turnEnded(id)) {
			writeError(w, http.StatusConflict, turnNotRunning)
			return
		}
		if _, err := a.cfg.Queue.Cancel(queue.DefaultTeam, j.ID); err != nil {
			a.internal(w)
			return
		}
		writeJSON(w, http.StatusAccepted, Stopping{turnSession(j), id})
		return
	case !errors.Is(err, queue.ErrNotFound):
		a.internal(w)
		return
	}
	recs, err := a.cfg.Store.After(0, store.Filter{Turn: id}, 1)
	if err != nil {
		a.internal(w)
		return
	}
	if len(recs) == 0 {
		writeError(w, http.StatusNotFound, noTurn)
		return
	}
	writeError(w, http.StatusConflict, turnNotRunning)
}
