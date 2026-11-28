package api

// POST /api/v0/ons, GET /api/v0/ons and DELETE /api/v0/ons/{id}: register,
// list and remove an on, the standing instruction "when this event happens, do
// this" (queue/watchon.go). The public on watches an event kind on the record
// with a filter: for now turn.completed, filtered by session, so every turn of
// that session that ends from now on has its reply delivered to a session as
// that session's next turn. ask_agent and review_turn compose it themselves
// (apps.go). The on over one turn, and POST /api/v0/turns' delivery, were
// removed in 0.6.8 (ARCHITECTURE, "no shortcuts in the API"): both answer 410.
//
// The caller's token decides what it may do. A turn token may register an on
// whose watched session and target session are both in its reach
// (inAuthority), its own session as the target when it names none; it may
// remove an on its own turn made or one that delivers to a session it may
// read, and it lists those same ons (agentOn). Registering an on is free;
// each turn it delivers counts as work its maker asked for (queue/asked.go). The main token may register an on
// over any session, naming the session to deliver to, and list and remove any.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// OnRequest is the body of POST /api/v0/ons.
type OnRequest struct {
	Event   string    `json:"event" doc:"The event to react to. turn.completed: a turn ended, however it ended. Required."`
	Filter  *OnFilter `json:"filter" doc:"Which events it reacts to. Required: filter.session."`
	Session string    `json:"session,omitempty" doc:"The session each reply is delivered to as its next turn. The main token must set it. A turn token may name any session it may read, and its own is used when it leaves this out. It may not be the watched session, nor one whose replies already reach the watched session through other ons (409): that would deliver in a circle."`
	// IdempotencyKey is the same mechanism POST /api/v0/turns takes: a key
	// already used returns the on it made, so a retry does not add a second.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"An optional idempotency key, as POST /api/v0/turns takes: the same key with the same fields returns the same on; with other fields it is 409."`
}

// OnFilter narrows an on's event.
type OnFilter struct {
	Session string `json:"session" doc:"The session whose turns it watches. A turn token may name any session it may read, its own too."`
}

// OnView is a registered on, as the API shows it. The queue's job id stays out.
type OnView struct {
	ID     string    `json:"id" doc:"The on's id (on_...), to remove it with DELETE /api/v0/ons/{id}."`
	Event  string    `json:"event" doc:"The event it reacts to: turn.completed, or command.ended for a command an agent handed to AgentBus (POST /api/v0/commands)."`
	Filter *OnFilter `json:"filter,omitempty" doc:"For an on over a session: the session whose turns it watches. It stays until it is removed."`
	Turn   string    `json:"turn,omitempty" doc:"For an on over one turn or command, which control registers itself (a command's delivery, or an on made before 0.6.8): the turn it watches, or for command.ended the command (c_...; GET /api/v0/commands/{id}). It goes once it fires."`
	// Session is where the on delivers, for both shapes.
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
	if on.Filter.Session != "" {
		v.Filter = &OnFilter{Session: on.Filter.Session}
	}
	if on.Maker.Turn != "" {
		v.By = &OnMaker{Session: on.Maker.Session, Turn: on.Maker.Turn}
	}
	return v
}

const (
	turnOnRemoveRefused = "a turn token may remove only an on its own turn registered or one that delivers to a session it may read"
	// onTurnGone and deliveryGone answer the forms removed in 0.6.8.
	onTurnGone   = "an on over one turn was removed in 0.6.8: register an on over the turn's session instead, {\"event\":\"turn.completed\",\"filter\":{\"session\":\"<session>\"}}, which delivers the reply of each of its turns that ends from now on"
	deliveryGone = "delivery was removed in 0.6.8: first register an on over the session (POST /api/v0/ons {\"event\":\"turn.completed\",\"filter\":{\"session\":\"<session>\"}}), then start the turn without delivery"
	onCircle     = "this on would deliver in a circle: replies to the session it delivers to already reach the session it watches, through other ons (GET /api/v0/ons); remove one of them first"
)

// sentField says the JSON object data has a top-level field name.
func sentField(data []byte, name string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	_, ok := m[name]
	return ok
}

// POST /api/v0/ons
func (a *API) registerOn(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Queue == nil || a.cfg.Store == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with an event and a filter")
		return
	}
	if sentField(data, "turn") {
		writeError(w, http.StatusGone, onTurnGone)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var body OnRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with an event and a filter")
		return
	}
	if body.Event != queue.OnTurnCompleted {
		writeError(w, http.StatusBadRequest, "event is required: turn.completed is the event an on may watch")
		return
	}
	if body.Filter == nil || !sessionID.MatchString(body.Filter.Session) {
		writeError(w, http.StatusBadRequest, "filter.session is required: the session whose turns it watches")
		return
	}
	if body.Session != "" && !sessionID.MatchString(body.Session) {
		writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
		return
	}
	// The same key validator as POST /turns (his call, 2026-11-19): a key with a
	// '/' is refused here too, so no caller can land on an internal delivery/
	// result/ or apply/ key and collide with work the core registers itself.
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	c := callerOf(r)
	to, maker, held, ok := c.limits.watch(a, w, &body)
	if !ok {
		return
	}
	// Both sessions must exist, whoever asks: a session the record does not
	// hold would make an on that can never fire, or never deliver.
	for _, s := range []string{body.Filter.Session, to} {
		switch _, found, err := a.workspaceOf(s); {
		case err != nil:
			a.internal(w)
			return
		case !found:
			writeError(w, http.StatusNotFound, "no such session: "+s)
			return
		}
	}
	// The delivered turns are held to the caps of the turn that made the on,
	// its cost cap among them; the main token's are held to none.
	var budget *queue.Limits
	if held {
		lim := queueLimits(c.rules)
		budget = &lim
	}
	on, err := a.cfg.Queue.RegisterWatchOn(queue.DefaultTeam, body.Event, queue.OnFilter{Session: body.Filter.Session}, to, body.IdempotencyKey, maker, budget, a.cfg.Store.Last())
	switch {
	case errors.Is(err, queue.ErrCycle):
		writeError(w, http.StatusConflict, onCircle)
		return
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key was already used to register an on with other fields; send a new key")
		return
	case err != nil:
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusCreated, onView(on))
}

// GET /api/v0/ons: the ons still waiting to react, oldest first, the ones the
// caller may remove: every one for the main token; for a turn token, agentOn's,
// the rule of DELETE /api/v0/ons/{id}. Control's own ons over the record (the
// put-away) are not listed: no caller registers or removes them.
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
