package api

// POST /api/v0/turns: a program starts a turn, an agent with a prompt. Every
// turn runs as a job: this makes a turn job in the queue and answers once a
// runner has started its turn (package runner, which starts it through
// package run). The reply keeps its shape: the session, the turn and its base
// URL. With session in the body, the same endpoint starts that session's next
// turn. docs/CLIENT_API.md is the contract.

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

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// DefaultMaxRuns bounds the turns running at once when nothing else says.
const DefaultMaxRuns = run.DefaultMaxRuns

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
	Session  string `json:"session,omitempty" doc:"A session the snapshot calls resumable: this turn is its next one. Absent: a new session."`
	Agent    string `json:"agent,omitempty" doc:"A saved agent's id, from GET /api/v0/agents: its harness, access and model run the turn. claude and codex also name that harness on its own subscription when no saved agent has that id. Required without session. With session it is optional: absent, the turn runs on the agent of the session's latest turn; sent, this turn runs on that agent, in the session's folder, and later turns that name none keep it. On the same harness as the latest turn the conversation continues by own replay; on another harness the turn gets the earlier turns as plain text (continuity transcript)."`
	Folder   string `json:"folder,omitempty" doc:"An absolute path to an existing directory on the runner's machine. Required without session. With session it is the session's own; if sent it must match."`
	Isolate  *bool  `json:"isolate,omitempty" doc:"True unless false, for a new session. True: when folder is in a git repo with a commit, the session runs in a new git worktree of that repo at its current HEAD, in the same folder inside it; otherwise in folder as it is. Uncommitted changes in folder are not in the worktree. False: always folder as it is. Not sent with session."`
	Prompt   string `json:"prompt" doc:"From 1 byte to 256 KiB, no NUL byte. Always passed after --."`
	Model    string `json:"model,omitempty" doc:"Passed only as the value of --model. Absent with agent: the saved agent's model, for a new session or a turn that switches agent; a next turn that names the session's own latest agent keeps the session's model. A session's next turn with no agent and no model keeps the model of the session's latest turn, from its turn.started."`
	ReadOnly *bool  `json:"read_only,omitempty" doc:"False unless true. True: claude gets Read, Grep and Glob only; codex runs with --sandbox read-only. False: claude keeps its own tools and permission settings, plus the edit rule of permissions.toml (by default it edits its session's folder without asking); with edit = \"session\" codex runs with --sandbox workspace-write and network access on, so it can edit the folder and reach the network, and with edit = \"none\" read-only. Read-only codex has no network."`
	// IdempotencyKey makes a retry safe: a client whose request timed out
	// sends it again with the same key and gets the same turn.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. It is the turn job's key. A retry with the same key and the same fields answers 202 with the turn the key first started, existing true, and starts nothing. The same key with other fields gets 409. A request refused with 400 used no key."`
}

// Started is the answer to a turn's start.
type Started struct {
	BaseURL string `json:"base_url" doc:"The address the agent uses, <service>/s/<id>/t/<turn>. Empty while queued."`
	ID      string `json:"id" doc:"The session id: a new one, or the session sent."`
	Turn    string `json:"turn" doc:"The new turn's id: the id of the job's attempt that runs it. Every event of the turn carries it in turn, and GET /api/v0/turns lists it. Empty while queued."`
	// Existing marks the answer to a retry: the idempotency key had already
	// made this turn's job.
	Existing bool   `json:"existing,omitempty" doc:"True when idempotency_key had already made this turn's job, so nothing new was made."`
	Job      string `json:"job" doc:"The turn job that runs the turn. GET /api/v0/jobs/{id} reads it."`
	Queued   bool   `json:"queued,omitempty" doc:"True when the turn has not started: it waits behind the session's earlier turns (ahead says how many), or no runner started it within 30 seconds. The job stays queued and runs next, in order; the turn then shows in the session. turn and base_url are empty."`
	Ahead    int    `json:"ahead,omitempty" doc:"With queued: how many turns of the session, running or waiting, are before this one. Absent when the turn waits only for a runner."`
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
			writeError(w, http.StatusBadRequest, "isolate is for a new session; a session keeps its own workspace")
			return
		}
		if body.Agent != "" {
			ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
			if !ok {
				return
			}
			spec.Agent, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
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
				writeError(w, http.StatusConflict, turnRunning)
				return
			}
		}
		spec.Session = body.Session
	} else {
		ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
		if !ok {
			return
		}
		if len(body.Folder) > run.MaxFolder || !filepath.IsAbs(body.Folder) {
			writeError(w, http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
			return
		}
		if msg := run.CheckPrompt(body.Prompt, body.Model); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		spec.Agent, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		if spec.Model == "" && ag.Model != "" {
			spec.Model, spec.ModelFromAgent = ag.Model, true
		}
		spec.Folder = body.Folder
		spec.Isolate = body.Isolate == nil || *body.Isolate
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
		writeError(w, http.StatusConflict, "this session's workspace is closed; it takes no more turns")
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end and retry")
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
	ag, msg := a.resolveAgent(id)
	if msg == "" {
		return ag, true
	}
	known := false
	if key != "" && sessionID.MatchString(id) {
		var err error
		if known, err = a.cfg.Queue.HasKey(queue.DefaultTeam, key); err != nil {
			a.internal(w)
			return agents.Agent{}, false
		}
	}
	if !known {
		writeError(w, http.StatusBadRequest, msg)
		return agents.Agent{}, false
	}
	return agents.Agent{ID: id, Harness: id}, true
}

// resolveAgent is the saved agent a turn names, or why it cannot start. Without saved agents only claude and codex are known, each
// on its own subscription.
func (a *API) resolveAgent(id string) (agents.Agent, string) {
	if a.cfg.Agents == nil {
		if !run.KnownAgent(id) {
			return agents.Agent{}, "agent must be claude or codex"
		}
		return agents.Agent{Harness: id, Access: agents.Subscription}, ""
	}
	if !sessionID.MatchString(id) {
		return agents.Agent{}, "agent must be a saved agent's id; GET /api/v0/agents lists them"
	}
	ag, err := a.cfg.Agents.Resolve(id)
	if err != nil {
		return agents.Agent{}, "agent " + strconv.Quote(id) + " is not a saved agent; GET /api/v0/agents lists them"
	}
	return ag, ""
}

const (
	turnRunning = "a turn is already running in this session"
	noTurns     = "this service starts no turns: it keeps no job queue"
)

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
			writeJSON(w, http.StatusAccepted, Started{ID: job.Session, Existing: existing, Job: job.ID, Queued: true, Ahead: ahead})
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
			writeJSON(w, http.StatusAccepted, Started{BaseURL: a.cfg.BaseURL + "/s/" + j.Session + "/t/" + turn, ID: j.Session, Turn: turn, Existing: existing, Job: j.ID})
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
			writeJSON(w, http.StatusAccepted, Started{ID: j.Session, Existing: existing, Job: j.ID, Queued: true})
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
		if turn := startedTurn(j); turn != "" && j.Attempt == turn && a.turnEnded(turn) {
			continue
		}
		n++
	}
	return n, nil
}

// startedTurn is the job's newest attempt whose turn has started, else "".
func startedTurn(j queue.Job) string {
	for i := len(j.Attempts) - 1; i >= 0; i-- {
		if j.Attempts[i].TurnStarted {
			return j.Attempts[i].ID
		}
	}
	return ""
}

// notStarted answers for a job that ended before its turn started: the
// refusal the runner recorded, with its status.
func (a *API) notStarted(w http.ResponseWriter, j queue.Job) {
	var res struct {
		Refused string `json:"refused"`
		Status  int    `json:"status"`
	}
	_ = json.Unmarshal(j.Result, &res)
	switch {
	case j.State == queue.Failed && j.Reason == queue.ReasonSetup && res.Refused != "":
		status := res.Status
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		writeError(w, status, res.Refused)
	case j.State == queue.Cancelled:
		writeError(w, http.StatusConflict, "the turn's job was cancelled before its turn started")
	default:
		writeError(w, http.StatusInternalServerError, "the turn's job ended before its turn started: "+j.State+" "+j.Reason)
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
	ID   string `json:"id" doc:"The turn's session."`
	Turn string `json:"turn" doc:"The turn being stopped. Its session.closed, with stopped true, comes once its process tree has ended."`
}

const (
	turnNotRunning = "this turn is not running: it has ended, or it is not a job of this service"
	noTurn         = "no such turn"
)

// POST /api/v0/turns/<id>/stop: a turn is its job's attempt, so a stop
// cancels the job. Its runner hears it on its next heartbeat and stops the
// turn's whole process tree.
func (a *API) stopTurn(w http.ResponseWriter, _ *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, err := a.cfg.Queue.JobByAttempt(queue.DefaultTeam, id)
	switch {
	case err == nil:
		if j.Ended != nil || j.Attempt != id || a.turnEnded(id) {
			writeError(w, http.StatusConflict, turnNotRunning)
			return
		}
		if _, err := a.cfg.Queue.Cancel(queue.DefaultTeam, j.ID); err != nil {
			a.internal(w)
			return
		}
		writeJSON(w, http.StatusAccepted, Stopping{j.Session, id})
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
