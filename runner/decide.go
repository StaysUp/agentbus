package runner

// The runner's gateway holds a reply at an Ask and learns the decision by
// polling control's record through the runner protocol (gateway/hold.go,
// jobs/decision.go). The gateway is shared across turns, so the Decider maps
// the held reply's turn to the attempt running it, then reads that attempt's
// decision — scoped to the runner's own current attempt. agentbus up's built-in
// runner polls its own control over loopback; a remote runner polls control
// over the network. One code path (his call, 2026-11-18).

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
)

// Decider returns the runner's gateway decider. It satisfies gateway.Decider
// without this package importing gateway, so main wires it with g.SetDecider.
func (r *Runner) Decider() *Decider { return &Decider{r: r} }

// Decider resolves a held reply's turn to the attempt running it and reads that
// attempt's decision from control.
type Decider struct{ r *Runner }

// Decided reports whether the approval the gateway is holding for turn was
// answered, and how. A turn with no current attempt of this runner, or any read
// error, is "not yet decided", so a hold is never released or refused on a poll
// the runner cannot ground in its own current attempt: it keeps holding until a
// later poll succeeds or the 60 s timeout ends it.
func (d *Decider) Decided(ctx context.Context, turn, approval string) (allow, decided bool, err error) {
	at := d.r.attemptForTurn(turn)
	if at == nil {
		return false, false, nil
	}
	u := at.path("decision") + "?number=" + strconv.Itoa(at.a.Number) + "&approval=" + url.QueryEscape(approval)
	var out jobs.AttemptDecision
	status, err := call(ctx, d.r.short, u, d.r.token, nil, &out)
	if err != nil || status != http.StatusOK {
		return false, false, err
	}
	switch out.State {
	case jobs.DecisionAllowed:
		return true, true, nil
	case jobs.DecisionRefused:
		return false, true, nil
	default:
		return false, false, nil
	}
}

// registerAttempt records a running attempt by its turn, so the Decider can
// find it; unregisterAttempt removes it at the turn's end. A turn runs one
// attempt at a time, so by-turn is enough; a retry replaces the earlier one,
// and unregister removes only its own entry.
func (r *Runner) registerAttempt(at *attempt) {
	if at.job.Turn == "" {
		return
	}
	r.attemptsMu.Lock()
	if r.attempts == nil {
		r.attempts = map[string]*attempt{}
	}
	r.attempts[at.job.Turn] = at
	r.attemptsMu.Unlock()
}

func (r *Runner) unregisterAttempt(at *attempt) {
	if at.job.Turn == "" {
		return
	}
	r.attemptsMu.Lock()
	if r.attempts[at.job.Turn] == at {
		delete(r.attempts, at.job.Turn)
	}
	r.attemptsMu.Unlock()
}

func (r *Runner) attemptForTurn(turn string) *attempt {
	if turn == "" {
		return nil
	}
	r.attemptsMu.Lock()
	defer r.attemptsMu.Unlock()
	return r.attempts[turn]
}

// ScopeApproval scopes a raised approval's local id to the attempt running turn,
// as <attempt>.<local> (gateway.SetApprovalScope). So the id names its one
// owning attempt, and control binds every approval to the attempt that raised
// it. A turn with no current attempt leaves the local id alone — control then
// refuses it as unscoped, which is correct: an approval with no attempt to own
// it is not one a person should answer for a runner.
func (r *Runner) ScopeApproval(turn, localID string) string {
	at := r.attemptForTurn(turn)
	if at == nil {
		return localID
	}
	// The attempt id is t_<hex> (no '-'), so '-' is an unambiguous prefix
	// separator, and the composite stays within the id charset the client
	// approvals route accepts ([A-Za-z0-9_-]).
	return at.a.ID + "-" + localID
}

// CallOwner says whether a model call on the path of session's turn, carrying
// token as its credential, is that turn's own: a turn this runner runs now,
// in that session, whose stand-in the call carries. A runner's gateway asks it
// before it holds or records a call, so a stand-in sent on another turn's path
// is refused there and recorded nowhere (cmd/agentbus turnGate). known is
// false when the token is no running turn's stand-in at all.
func (r *Runner) CallOwner(session, turn, token string) (own, known bool) {
	if token == "" {
		return false, false
	}
	r.attemptsMu.Lock()
	defer r.attemptsMu.Unlock()
	for _, at := range r.attempts {
		at.mu.Lock()
		standIn := at.standIn
		at.mu.Unlock()
		if standIn != token {
			continue
		}
		return at.job.Kind == queue.KindTurn && at.job.Turn == turn && at.job.Session == session && turn != "", true
	}
	return false, false
}
