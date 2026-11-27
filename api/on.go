package api

// POST /api/v0/ons, GET /api/v0/ons and DELETE /api/v0/ons/{id}: register,
// list and remove an on, the standing instruction "when this event happens, do
// this" (queue/on.go). B13
// has one event, a turn completing, and one action, deliver that turn's reply
// to a session as its next turn. It is the public form of what ask_agent
// registers for itself (runs.go): the primitive every later reaction is built
// on (schedule, gate), one row on control, nothing waiting.
//
// The caller's token decides what it may do. A turn token may register an on
// for any turn it may read (mayReadTurn, the check GET /api/v0/turns/{id}
// uses), its own session's too, and its result is delivered to a session it
// may read (its own when it names none); it may remove an on its own turn made
// or one that delivers to a session it may read, and it lists those same ons
// (agentOn). Such an on counts as work its turn asked for, under the per-turn
// and per-tree caps ([limits] max_jobs_per_turn, max_jobs_per_tree). The main
// token may register an on for any turn, naming the session to deliver to, and
// list and remove any on.

import (
	"errors"
	"net/http"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// OnRequest is the body of POST /api/v0/ons.
type OnRequest struct {
	Turn    string `json:"turn" doc:"The turn to watch: when it completes, its reply is delivered. A turn token may name any turn it may read with GET /api/v0/turns/{id}, its own session's too."`
	Session string `json:"session,omitempty" doc:"The session the reply is delivered to as its next turn. The main token must set it. A turn token may name any session it may read, and its own is used when it leaves this out."`
	// IdempotencyKey is the same mechanism POST /api/v0/turns takes: a key
	// already used returns the on it made, so a retry does not add a second.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"An optional idempotency key, as POST /api/v0/turns takes: the same key returns the same on, so a retry registers no second delivery."`
}

// OnView is a registered on, as the API shows it. The queue's job id stays out.
type OnView struct {
	ID      string `json:"id" doc:"The on's id (on_...), to remove it with DELETE /api/v0/ons/{id}."`
	Event   string `json:"event" doc:"The event it reacts to: turn.completed, or command.ended for a command an agent handed to AgentBus (POST /api/v0/commands)."`
	Turn    string `json:"turn" doc:"The turn it watches, or for command.ended the command (c_...; GET /api/v0/commands/{id})."`
	Session string `json:"session" doc:"The session its action delivers to."`
	// By is who registered it, as the queue's Maker names it.
	By *OnMaker `json:"by,omitempty" doc:"The turn whose token registered the on, and that turn's session. Absent for an on the main token registered."`
}

// OnMaker is the turn that registered an on.
type OnMaker struct {
	Session string `json:"session" doc:"The registering turn's session."`
	Turn    string `json:"turn" doc:"The registering turn."`
}

// OnList is the reply of GET /api/v0/ons.
type OnList struct {
	Ons []OnView `json:"ons" doc:"The ons waiting to react, oldest first. Empty when none."`
}

// onView is on as the API shows it.
func onView(on queue.On) OnView {
	v := OnView{ID: on.ID, Event: on.Event, Turn: on.Subject, Session: on.Session}
	if on.Maker.Turn != "" {
		v.By = &OnMaker{Session: on.Maker.Session, Turn: on.Maker.Turn}
	}
	return v
}

const (
	turnOnRefused       = "a turn token may watch only a turn it may read"
	turnOnRemoveRefused = "a turn token may remove only an on its own turn registered or one that delivers to a session it may read"
)

// POST /api/v0/ons
func (a *API) registerOn(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	var body OnRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with a turn to watch")
		return
	}
	if body.Turn == "" {
		writeError(w, http.StatusBadRequest, "turn is required: the turn to watch")
		return
	}
	// The same key validator as POST /turns (his call, 2026-11-19): a key with a
	// '/' is refused here too, so no caller can land on an internal delivery/
	// result/ or apply/ key and collide with work the core registers itself.
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// The subject turn must exist, whoever the caller is: a nonexistent one is
	// 404, never a reaction that can never fire.
	// It may also be a command an agent handed to AgentBus (c_...): its on
	// watches command.ended, so a handover can move a command's delivery as it
	// moves a turn's.
	event := queue.OnTurnCompleted
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, body.Turn)
	if errors.Is(err, queue.ErrNotFound) {
		event = queue.OnCommandEnded
		j, err = a.cfg.Queue.JobByCommand(queue.DefaultTeam, body.Turn)
	}
	switch {
	case errors.Is(err, queue.ErrNotFound):
		a.noJobOn(w, callerOf(r), &body)
		return
	case err != nil:
		a.internal(w)
		return
	}
	// B15 (his call, 2026-11-19): an on may watch ANY turn the caller's token
	// may reach — core does not single out asked work as a kind. What stays is
	// credential scoping, not a kind check:
	//   - a turn token may watch any turn it may read (C2, 2026-11-25: a
	//     coordinator was refused the review its builder asked for). The check
	//     is the read's own (mayReadTurn), so a wider read widens this too.
	//     Delivery goes to a session it may read, its own by default (O2.3).
	//     The on counts as asked work under the per-turn and per-tree caps
	//     (2026-11-25): a delivered turn skips them (deliverResult), so a chain
	//     of ons over ended turns, each fired at once into a new turn that
	//     registers the next, or a session watching itself, stops at the caps
	//     here instead.
	//   - the main token may watch any turn and deliver to any session it names;
	//     it can already start a turn in any session. A main-token on over a
	//     person's turn is made safe by the self-retire checks (B13b), not by a
	//     refusal here.
	c := callerOf(r)
	session, maker, asked, ok := c.limits.watch(a, w, j, &body)
	if !ok {
		return
	}
	on, err := a.cfg.Queue.RegisterOnLinked(queue.DefaultTeam, event, body.Turn, session, body.IdempotencyKey, maker, asked, queueLimits(c.rules))
	var limit *queue.LimitError
	switch {
	case errors.As(err, &limit):
		writeError(w, http.StatusForbidden, limit.Msg)
		return
	case errors.Is(err, queue.ErrUnknownEvent):
		writeError(w, http.StatusBadRequest, "that event is not known")
		return
	case errors.Is(err, queue.ErrBadState):
		writeError(w, http.StatusBadRequest, "turn and session are required")
		return
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key was already used to register an on for a different turn or session; send a new key")
		return
	case err != nil:
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusCreated, onView(on))
}

const onNoJob = "this turn was not run as a job of this service (an older turn, or one recorded at the gateway), so nothing reports its end and no on can react to it"

// noJobOn answers an on over a turn with no job. The caller's own check comes
// first (limits.watch), so a turn token that may not read such a turn gets
// what it gets for any turn it cannot read. A turn that passes it, and that
// the record holds, is 409: it is there, but an on over it could never fire
// (FireOns reads the turn.ended its job's end writes, and the job's reply).
// Review of dd2dcb1: every such turn got 404, a readable one too.
func (a *API) noJobOn(w http.ResponseWriter, c *caller, body *OnRequest) {
	if _, _, _, ok := c.limits.watch(a, w, queue.Job{}, body); !ok {
		return
	}
	switch session, err := a.sessionOfTurn(body.Turn, ""); {
	case err != nil:
		a.internal(w)
	case session == "":
		writeError(w, http.StatusNotFound, noTurn)
	default:
		writeError(w, http.StatusConflict, onNoJob)
	}
}

// GET /api/v0/ons: the turns' ons still waiting to react, oldest first, the
// ones the caller may remove: every one for the main token; for a turn token,
// agentOn's, the rule of DELETE /api/v0/ons/{id}. Control's own ons over the
// record (the put-away) are not listed: no caller registers or removes them.
func (a *API) listOns(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	session, sent, ok := sessionQuery(w, r)
	if !ok {
		return
	}
	out := OnList{Ons: []OnView{}}
	if a.cfg.Queue == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ons, err := a.cfg.Queue.ListOns(queue.DefaultTeam)
	if err != nil {
		a.internal(w)
		return
	}
	c := callerOf(r)
	every := c.everywhere(run.ActRead)
	for _, on := range ons {
		if !queue.Delivers(on.Event) || sent && on.Session != session {
			continue
		}
		if !every {
			in, err := a.agentOn(c.Grant, on)
			if err != nil {
				a.internal(w)
				return
			}
			if !in {
				continue
			}
		}
		out.Ons = append(out.Ons, onView(on))
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/v0/ons/{id}
func (a *API) removeOn(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noOn)
		return
	}
	if !callerOf(r).limits.removeOn(a, w, id) {
		return
	}
	switch err := a.cfg.Queue.RemoveOn(queue.DefaultTeam, id); {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noOn)
	case err != nil:
		a.internal(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

const noOn = "no such on"
