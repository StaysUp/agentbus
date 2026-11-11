package api

// POST /api/v0/turns/{id}/review: ask another agent to review a turn. A review
// is a turn of its own: read-only, in a session control names for it, in the
// reviewed turn's copy. Its verdict and findings are read from its reply
// when it ends (queue.ParseVerdict), and shown on it.

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// ReviewRequest is the body of POST /api/v0/turns/{id}/review.
type ReviewRequest struct {
	Agent          string `json:"agent" doc:"The reviewer: a saved agent's id, from GET /api/v0/agents. Pick another agent than the one that did the turn."`
	Model          string `json:"model,omitempty" doc:"The reviewer's model. Absent: the saved agent's."`
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key and fields answers with the same review turn."`
	// IncludeReply puts the author's reply in the prompt. Off by default:
	// a reviewer handed the author's conclusion tends to confirm it.
	IncludeReply bool `json:"include_reply,omitempty" doc:"Put the author's own reply in the review's prompt. Off by default: a reviewer handed the author's conclusion tends to confirm it."`
}

// reviewerShape is a reviewer: a saved agent's id or a harness name.
var reviewerShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

const badReview = "the body must be one JSON object with agent, and optionally model, idempotency_key and include_reply"

// POST /api/v0/turns/<id>/review
func (a *API) reviewTurn(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	var b ReviewRequest
	if !decodeObject(r, &b) {
		writeError(w, http.StatusBadRequest, badReview)
		return
	}
	if !reviewerShape.MatchString(b.Agent) || (b.Model != "" && !run.ModelShape.MatchString(b.Model)) {
		writeError(w, http.StatusBadRequest, "agent must match "+reviewerShape.String()+" and model "+run.ModelShape.String())
		return
	}
	if b.IdempotencyKey != "" && !idempotencyKey.MatchString(b.IdempotencyKey) {
		writeError(w, http.StatusBadRequest, "idempotency_key must match "+idempotencyKey.String())
		return
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noTurn)
		return
	case err != nil:
		a.internal(w)
		return
	}
	// The reviewer is a saved agent, as for a turn; without saved agents its
	// name is the harness.
	as := queue.Spec{Harness: b.Agent, Model: b.Model}
	if a.cfg.Agents != nil {
		// A retry of a review whose reviewer was removed since gets the
		// first review, as a turn's retry does.
		var retried func() (bool, error)
		if b.IdempotencyKey != "" {
			retried = func() (bool, error) { return a.cfg.Queue.HasKey(queue.DefaultTeam, b.IdempotencyKey) }
		}
		ag, why, err := agents.Lookup(b.Agent, a.cfg.Agents.Resolve, retried)
		if err != nil {
			a.internal(w)
			return
		}
		if why != nil {
			writeError(w, http.StatusBadRequest, "agent "+strconv.Quote(b.Agent)+" is not a saved agent; GET /api/v0/agents lists them")
			return
		}
		as.Harness, as.AgentID, as.Access, as.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		if as.Model == "" && ag.Model != "" {
			as.Model, as.ModelFromAgent = ag.Model, true
		}
	}
	// A review a turn asks for names that turn, within its caps.
	var links queue.Links
	var lim queue.Limits
	if g := grantOf(r); g != nil {
		links, lim = askedLinks(*g), queueLimits(policyOf(r))
	}
	rv, created, err := a.cfg.Queue.ReviewLinked(queue.DefaultTeam, j.ID, as, b.IdempotencyKey, b.IncludeReply, links, lim)
	var limit *queue.LimitError
	switch {
	case errors.As(err, &limit):
		writeError(w, http.StatusForbidden, limit.Msg)
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noTurn)
	case errors.Is(err, queue.ErrNotReviewable):
		writeError(w, http.StatusConflict, "this turn cannot be reviewed: it must have ended, with its changed files recorded in its session's copy or folder")
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusConflict, "the turn's session is closed, so its copy is gone")
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key already names another turn")
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns are waiting; wait for some to end")
	case err != nil:
		a.internal(w)
	default:
		writeJSON(w, http.StatusAccepted, Started{Session: turnSession(rv), Turn: rv.Turn, State: queue.TurnState(rv.State), Existing: !created})
	}
}
