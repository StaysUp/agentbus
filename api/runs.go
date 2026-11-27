package api

// POST /api/v0/turns: a program starts a turn, an agent with a prompt. Every
// turn runs as a job: this makes a turn job in the queue and answers once a
// runner has started its turn (package runner, which starts it through
// package run). The reply keeps its shape: the session, the turn and its base
// URL. With session in the body, the same endpoint starts that session's next
// turn.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/staysup/agentbus/agents"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// MaxRunBody bounds a turn's request body. A 256 KiB prompt can take six
// bytes per byte once JSON-escaped.
const MaxRunBody = 2 << 20

// MaxPrompt bounds a turn's prompt, in bytes.
const MaxPrompt = run.MaxPrompt

// DefaultTurnWait bounds how long a workspace's changes preview (changes.go)
// waits on its preview job, when Config.TurnWait is zero. POST /api/v0/turns
// and the workspace actions (apply, discard, update, close) never wait.
const DefaultTurnWait = 30 * time.Second

var runModel = run.ModelShape

const badRun = "the body must be one JSON object with session, agent and prompt, and optionally model, delivery and idempotency_key, at most 2 MiB"

// TurnRequest is the body of POST /api/v0/turns. A turn names the session it
// runs in and its agent, and nothing about where it runs: the folder and the
// mode are the session's workspace's, set when the workspace was created. The
// three calls are POST /api/v0/workspaces, POST /api/v0/sessions, then this.
type TurnRequest struct {
	Session string `json:"session" doc:"The session the turn runs in, from POST /api/v0/sessions. Its workspace is where the turn runs (the folder and the mode), and its earlier turns are the conversation this turn continues. Required."`
	Agent   string `json:"agent" doc:"A saved agent's id, from GET /api/v0/agents: its harness, access and model run the turn. A removed agent, or a default one (claude, codex, agy), is refused: pick another. Required on every turn (400 without it): a session keeps no agent of its own. The agent is read as it is when the turn is asked for, and its model is the turn's unless model is sent. On the same harness as the latest turn the conversation continues by own replay; on another harness the turn gets the earlier turns as plain text (continuity transcript)."`
	Prompt  string `json:"prompt" doc:"From 1 byte to 256 KiB, no NUL byte. Always passed after --."`
	Model   string `json:"model,omitempty" doc:"Passed only as the value of --model, for this turn only. Absent: the named agent's model, as it is when the turn is asked for; an agent with none runs on its harness's own default."`
	// IdempotencyKey makes a retry safe: a client whose request timed out
	// sends it again with the same key and gets the same turn.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key and the same fields answers 202 with the turn the key first started, existing true, and starts nothing. The same key with other fields gets 409. A request refused with 400 used no key."`
	// Delivery records the on for the turn's reply in the SAME write as the turn
	// (his call (a), 2026-11-19), so starting and delivering are one atomic step.
	Delivery string `json:"delivery,omitempty" doc:"A session to deliver this turn's reply to when it completes, recorded as a turn.completed on in the same transaction that starts the turn (the same as POST /api/v0/ons, but atomic with the start, so no separate call can be lost). A turn token may name any session it may read; the main token may name any. Absent: no delivery is registered. The turn's idempotency_key covers the on too, so a retry adds no second."`
}

// Started is the answer to a turn's start, and to a review's.
type Started struct {
	Session string `json:"session" doc:"The turn's session: a new one, or the session sent. A review's is a session of its own."`
	Turn    string `json:"turn" doc:"The turn's id. Every event of the turn carries it in turn; GET /api/v0/turns/{id} follows it from now on."`
	State   string `json:"state" doc:"running: a runner started it. queued: it waits behind the session's earlier turns (ahead says how many), or for a runner. It runs next, in order. A retry of a turn that has ended says how it ended: finished, failed or stopped."`
	BaseURL string `json:"base_url,omitempty" doc:"The address the agent uses, <service>/s/<session>/t/<turn>. Present only when this service runs the turn itself (agentbus up). Absent while queued, and on a server, where the turn runs on another machine's runner and that runner serves its gateway."`
	// Existing marks the answer to a retry: the idempotency key had already
	// made this turn.
	Existing bool `json:"existing,omitempty" doc:"True when idempotency_key had already made this turn, so nothing new was made."`
	Ahead    int  `json:"ahead,omitempty" doc:"With state queued: how many turns of the session, running or waiting, are before this one. Absent when the turn waits only for a runner."`
}

// idempotencyKey is the shape of any idempotency_key a request sends. It
// excludes '/', which is how the internal key namespaces (delivery/, result/,
// apply/) stay unreachable by a caller — so every route that accepts a key must
// check it with checkIdempotencyKey, not just this one (his call, 2026-11-19:
// one rule, one function, after a key that could hold '/' at POST /ons let a
// caller's on collide with a turn's delivery on).
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// checkIdempotencyKey is the one validator every route with an idempotency_key
// shares. It returns "" for a valid key (including an empty one, which means no
// key) or the message to refuse with. It reads only the incoming value; keys
// already stored are never re-checked, so this changes no existing row.
func checkIdempotencyKey(key string) string {
	if key != "" && !idempotencyKey.MatchString(key) {
		return "idempotency_key must match " + idempotencyKey.String()
	}
	return ""
}

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
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// A turn token asks: its checks (tokens.go) verify the session is in its
	// reach and its tree is within its cost cap, and name the asking turn.
	who := callerOf(r)
	links, ok := who.limits.ask(a, w, who.rules, &body)
	if !ok {
		return
	}
	if !sessionID.MatchString(body.Session) {
		writeError(w, http.StatusBadRequest, "session is required and must match "+sessionID.String())
		return
	}
	if msg := run.CheckPrompt(body.Prompt, body.Model); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// Every turn names its agent (his rule, 2026-11-12): it is read here, once,
	// as it is now, and the turn's tool and credential both come from it.
	if body.Agent == "" {
		writeError(w, http.StatusBadRequest, run.NoAgent)
		return
	}
	ag, ok := a.turnAgent(w, body.Agent, body.IdempotencyKey)
	if !ok {
		return
	}
	// The turn runs in its session's workspace: the folder, the machine and the
	// sandbox mode are the workspace's, the one source (his API rules,
	// 2026-11-21). The session must already exist (POST /api/v0/sessions).
	ws, found, err := a.cfg.Store.WorkspaceOf(ids.SessionID(body.Session))
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	wsp, wok, err := a.cfg.Store.Workspace(ws)
	if err != nil {
		a.internal(w)
		return
	}
	if !wok {
		// A session the record knows but places in no workspace (no
		// workspace.created): every session is created in a workspace (POST
		// /api/v0/sessions, or the gateway/hook at the session's birth), so this
		// is a session recorded without one. It cannot run a turn until it has a
		// workspace, and nothing here guesses one (his rule: nothing implicit).
		writeError(w, http.StatusInternalServerError, "this session has no workspace to run in")
		return
	}
	spec := queue.Spec{Prompt: body.Prompt, Model: body.Model, Session: ids.SessionID(body.Session),
		Workspace: ids.WorkspaceID(wsp.ID), FolderRef: wsp.FolderRef, Folders: Places(wsp), Machine: wsp.Machine, Isolate: wsp.Sandbox}
	spec.Harness, spec.AgentID, spec.Access, spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
	if spec.Model == "" {
		spec.AgentModel = ag.Model
	}
	// A session whose turn runs takes the next one too: its job waits in the
	// queue behind the turns before it. A session the record shows open with no
	// job of this queue behind it (a turn this service did not start) cannot be
	// waited for, so it is refused, before the key is used; a retry of a known
	// key goes on to the queue, which returns its job.
	c, err := a.cfg.Store.Continuation(body.Session)
	if err != nil {
		a.internal(w)
		return
	}
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
	// Atomic delivery (his call (a), 2026-11-19): if delivery is set, record the
	// on in the SAME write as the turn, under the one authorization that starts
	// it — the same rules POST /api/v0/ons applies, but no separate call that a
	// stop or a lost token can drop. A turn token delivers to a session in its
	// reach; the main token may name any session. The turn's key covers the on.
	var deliver *queue.Delivery
	if body.Delivery != "" {
		if deliver, ok = who.limits.deliver(w, body.Delivery); !ok {
			return
		}
	}
	// Never refused for a busy session: the job waits behind the session's turns
	// that have not ended, as a delivered result does.
	job, created, err := a.cfg.Queue.CreateTurnLinked(queue.DefaultTeam, body.IdempotencyKey, spec, links, queueLimits(who.rules), deliver)
	var limit *queue.LimitError
	var removed *queue.RunnerRemovedError
	switch {
	case errors.As(err, &limit):
		writeError(w, http.StatusForbidden, limit.Msg)
		return
	case errors.As(err, &removed):
		writeError(w, http.StatusConflict, removed.Error())
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
	a.answerStarted(w, job.ID, !created)
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
	if errors.Is(err, agents.ErrRemoved) {
		return agents.Agent{}, errors.New("agent " + strconv.Quote(id) + " was removed; pick another (GET /api/v0/agents lists them)")
	}
	if err != nil {
		return agents.Agent{}, errors.New("agent " + strconv.Quote(id) + " is not a saved agent; GET /api/v0/agents lists them")
	}
	return ag, nil
}

const noTurns = "this service starts no turns: it keeps no queue"

// startedRunning is the Started for a turn a runner has started: its running
// state, and the gateway BaseURL when this service runs the turn itself
// (agentbus up). A retry of a turn already running answers through it.
func (a *API) startedRunning(j queue.Job, turn string, existing bool) Started {
	st := Started{Session: j.Session, Turn: turn, State: queue.TurnRunning, Existing: existing}
	// BaseURL is the turn's gateway, which only this service serves, and only
	// when it runs turns itself. A server runs no turns of its own: the gateway
	// is on the runner that ran it, a machine this service does not serve, so
	// BaseURL is left out rather than pointing at control.
	if a.cfg.Engine != nil {
		st.BaseURL = a.cfg.BaseURL + "/s/" + j.Session + "/t/" + turn
	}
	return st
}

// answerStarted answers POST /api/v0/turns at once, never waiting for a
// runner (C1, 2026-11-25: the 30 s wait was old code; every client handles a
// queued turn). It reports the job's real state, not a hardcoded queued: a
// retry whose turn already runs says running, and one whose turn was stopped
// or ended before it started says why (found in review 2026-11-19). A turn
// queued behind its session's earlier turns says how many are ahead.
func (a *API) answerStarted(w http.ResponseWriter, id string, existing bool) {
	j, err := a.cfg.Queue.Get(queue.DefaultTeam, id)
	if err != nil {
		a.internal(w)
		return
	}
	if turn := startedTurn(j); turn != "" {
		if j.Ended != nil {
			// A retry of a turn that ran and ended says how it ended, as GET
			// /api/v0/turns/{id} does, never running (review of dd2dcb1).
			writeJSON(w, http.StatusAccepted, Started{Session: j.Session, Turn: turn, State: a.endedState(j, turn), Existing: existing})
			return
		}
		writeJSON(w, http.StatusAccepted, a.startedRunning(j, turn, existing))
		return
	}
	if j.Ended != nil {
		a.notStarted(w, j)
		return
	}
	st := Started{Session: j.Session, Turn: j.Turn, State: queue.TurnQueued, Existing: existing}
	if j.Session != "" {
		if st.Ahead, err = a.turnsAhead(j.ID); err != nil {
			a.internal(w)
			return
		}
	}
	writeJSON(w, http.StatusAccepted, st)
}

// endedState is how a started turn whose job has ended ended: the record's
// state, as GET /api/v0/turns/{id} reads it, or the job's when the record
// holds no end (a server whose runner keeps its own record).
func (a *API) endedState(j queue.Job, turn string) string {
	list, err := a.cfg.Store.Turns(store.TurnQuery{Turn: turn, Limit: 1})
	if err == nil && len(list.Turns) == 1 && list.Turns[0].State != store.TurnRunning {
		return list.Turns[0].State
	}
	return queue.TurnState(j.State)
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
