package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/staysup/agentbus/event"
)

// H1: the gateway holds a streamed reply at an Ask call, keeping the agent's
// model-call connection open with keep-alive pings, until a person answers or
// the hold times out. An Allow releases the held call unchanged, so it runs in
// the SAME turn; a Refuse, or no answer in HoldTimeout, refuses it and the turn
// ends with no automatic next turn (his call, 2026-11-17: 60 s is enough, and a
// continuation turn costs turns the agent may no longer need).
//
// The runner's own gateway holds the reply; it learns the answer by asking
// control through the approvals API (a Decider the runner wires with its token),
// because control's record is the truth and the same hold then runs for a
// built-in runner and a remote one (one gateway path, 0.5.10). The guard emits
// approval.requested when it starts holding and, on timeout, approval.decided,
// so nothing stays pending; a person answers over POST /approvals as before.
//
// The earlier hold (ASK_APPROVALS_2026-11-15.md) was dropped after four review
// rounds of life-cycle P1s, every one from holding the connection: a second ask
// call released unapproved, approvals leaked on disconnect, the unstreamed path
// decided after settlement, a ping blocked a stalled client, and a disconnect
// was never seen. This hold avoids each: all Ask calls of a reply are held
// under the one approval and released or refused together; only streamed
// replies are held (an unstreamed reply is refused inline, as before); the hold
// watches the request context and writes its pings, so a disconnect ends it;
// and the 60 s cap bounds every goroutine.

// HoldTimeout is how long the gateway holds a reply at an Ask call before it
// refuses it for want of an answer. PingInterval is the gap between keep-alive
// pings to the agent meanwhile; the measurements (ask-hold-measurements.md)
// show Claude Code and codex both hold a call well past 60 s with a 15 s ping.
// PollInterval is how often the hold asks control for the decision.
const (
	HoldTimeout  = 60 * time.Second
	PingInterval = 15 * time.Second
	PollInterval = 1 * time.Second
)

// Decider answers whether a held approval has been decided, and how, from
// control's record. The runner wires it to the runner-protocol decision read
// with its own token, scoped to the attempt running turn, so a built-in runner
// and a remote one hold a reply the same way. allow is read only when decided
// is true; an error, or a turn the runner cannot resolve to a current attempt,
// is treated as "not yet decided", so a transient read never refuses a call a
// person is still answering.
type Decider interface {
	Decided(ctx context.Context, turn, approval string) (allow, decided bool, err error)
}

// SetDecider gives the gateway the way it asks control for a held approval's
// decision while it holds the reply. Without one, a reply with Ask calls is
// refused inline (no hold), as before H1. Call it before serving.
func (g *Gateway) SetDecider(d Decider) { g.decider = d }

// SetApprovalScope gives the gateway the way it scopes a raised approval's id to
// the attempt that raised it (the runner resolves its turn to its attempt id),
// so the id is <attempt>-<local> and control can bind every approval to one
// attempt. Without one the local id stands alone (tests). Call it before
// serving.
func (g *Gateway) SetApprovalScope(f func(turn, localID string) string) { g.approvalScope = f }

// SetHoldTiming overrides the hold's timeout, ping and poll gaps, for tests
// that must exercise the hold without waiting a real minute. Zero keeps a
// field's default.
func (g *Gateway) SetHoldTiming(timeout, ping, poll time.Duration) {
	if timeout > 0 {
		g.holdTimeout = timeout
	}
	if ping > 0 {
		g.pingInterval = ping
	}
	if poll > 0 {
		g.pollInterval = poll
	}
}

func (g *Gateway) holdTimeoutDur() time.Duration { return orDur(g.holdTimeout, HoldTimeout) }
func (g *Gateway) pingDur() time.Duration        { return orDur(g.pingInterval, PingInterval) }
func (g *Gateway) pollDur() time.Duration        { return orDur(g.pollInterval, PollInterval) }

func orDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// holdState is where a held reply is in its life — the single source of truth
// that replaces the earlier toHold/holding/latch flags. A streamed reply with
// Ask calls and a decider moves holdNone -> holdPending as it buffers, then
// holdPending -> holdActive once it is complete and the hold goes live, then
// -> holdDone when it is finalised (released on allow, refused on refuse or a
// timeout, or ended by a closed connection). A reply with nothing to hold stays
// holdNone. The four lifecycle guards the earlier hold needed as separate flags
// are now one question about this state: only an ACTIVE hold is polled, pinged
// and finalised by an ending; only a PENDING hold keeps buffering; a DONE hold
// never restarts; and a never-held (NONE) reply is not a hold, so closing an
// inline refusal records nothing.
type holdState int32

const (
	holdNone holdState = iota
	holdPending
	holdActive
	holdDone
)

// holdTransitions declares every allowed hold move — the one place the
// lifecycle is defined. holdNone -> holdPending (a streamed Ask reply begins
// buffering); holdPending -> holdActive (complete, the hold goes live) or
// holdDone (nothing clean to wait on, refused inline); holdActive -> holdDone
// (finalised). Anything else is rejected, so no code path can leave the hold in
// a shape a guard forgot to handle.
var holdTransitions = map[holdState]map[holdState]bool{
	holdNone:    {holdPending: true},
	holdPending: {holdActive: true, holdDone: true},
	holdActive:  {holdDone: true},
}

func holdCanMove(from, to holdState) bool { return holdTransitions[from][to] }

// holdLifecycle is the hold's state with its transitions in one place. begin
// and activate run only in the Read goroutine as the reply streams; finish can
// run from the Read goroutine (a decision, the timeout, a disconnect) or the
// proxy's teardown goroutine (Close on a gone client), so the whole type is
// mutex-guarded and finish reports whether THIS caller made the move — the
// single-winner rule the earlier hold broke when a second answer released an
// unapproved call.
type holdLifecycle struct {
	mu sync.Mutex
	s  holdState
}

func (h *holdLifecycle) state() holdState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.s
}

// begin moves holdNone -> holdPending: the reply has Ask calls to hold once it
// is complete. It is idempotent (a second Ask call of the same reply finds it
// already pending), and runs only in the Read goroutine.
func (h *holdLifecycle) begin() { h.move(holdNone, holdPending) }

// activate moves holdPending -> holdActive: the reply is complete and the hold
// goes live, pinging and polling. Read goroutine only.
func (h *holdLifecycle) activate() { h.move(holdPending, holdActive) }

// move performs one declared transition from the current state, reporting
// whether it happened. An undeclared move (or one from a different state) is
// rejected rather than corrupting the lifecycle.
func (h *holdLifecycle) move(from, to holdState) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.s != from || !holdCanMove(from, to) {
		return false
	}
	h.s = to
	return true
}

// finish moves a pending or active hold to holdDone and reports whether this
// caller made the move. The first of {a polled decision, the timeout, a closed
// connection} wins; the rest find it already done. A holdNone reply (never
// held) cannot finish, so closing an inline-refused reply records nothing.
func (h *holdLifecycle) finish() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !holdCanMove(h.s, holdDone) {
		return false
	}
	h.s = holdDone
	return true
}

// Hold texts: what the model is told in place of a held call's output when the
// hold ends without an allow.
const (
	holdTimeoutText = "Held by the AgentBus gateway: no one answered in time; ask again later. The command did not run."
	holdRefusedText = "Held by the AgentBus gateway: a person refused this command. It did not run."
	// holdIncompleteText: the reply ended before it was complete, so its Ask
	// calls could not be held for a person; they are refused rather than let
	// through unapproved (guard.go, the pending-EOF path).
	holdIncompleteText = "Held by the AgentBus gateway: the reply ended before this command could be approved. It did not run."
)

// startHold begins holding a complete streamed reply that has Ask calls: it
// records the one approval (so a person sees the card) and holds, pinging, for a
// decision. It is called once, when the reply is complete.
func (b *guardBody) startHold() {
	b.registerApproval()
	if b.holdID == "" {
		// Nothing clean to wait on: refuse the held calls and pass the reply.
		b.hold.finish() // holdPending -> holdDone
		b.refuseHeldAll(holdRefusedText)
		b.flush()
		return
	}
	b.hold.activate() // holdPending -> holdActive
	now := b.g.now()
	b.holdStart, b.holdPinged, b.holdPolled = now, now, now
	// The hold's pings are written by the proxy between two Reads, so a ping
	// the agent stops reading blocks the next Read, and with it the poll and
	// the deadline. Each write to the agent must end by the hold's deadline:
	// one that cannot fails then, the proxy closes the body, and Close ends
	// the hold as its timeout does.
	b.setWriteDeadline(b.holdDeadline())
}

// holdDeadline is when an active hold times out.
func (b *guardBody) holdDeadline() time.Time { return b.holdStart.Add(b.g.holdTimeoutDur()) }

// setWriteDeadline sets the deadline of each write to the agent's
// connection; the zero time clears it. A call with no connection of its own
// to set (a test's, or a writer that cannot) keeps none.
func (b *guardBody) setWriteDeadline(t time.Time) {
	if b.ex != nil && b.ex.writeDeadline != nil {
		_ = b.ex.writeDeadline(t)
	}
}

// holdStep waits for the soonest of the next poll, the next ping, the deadline,
// or the agent disconnecting, then acts. Read calls it while holding; it writes
// a ping, finalises the hold, or returns to be called again.
func (b *guardBody) holdStep() {
	if b.hold.state() == holdDone {
		// The hold was finalised elsewhere (the body was closed mid-hold). Stop,
		// rather than wait a full ping interval on a hold this goroutine can no
		// longer win.
		if b.err == nil {
			b.err = context.Canceled
		}
		return
	}
	now := b.g.now()
	deadline := b.holdDeadline()
	if !now.Before(deadline) {
		if b.hold.finish() {
			b.timeoutHold()
		}
		return
	}
	wake := b.holdPinged.Add(b.g.pingDur())
	if p := b.holdPolled.Add(b.g.pollDur()); p.Before(wake) {
		wake = p
	}
	if deadline.Before(wake) {
		wake = deadline
	}
	d := wake.Sub(now)
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	var ctxDone <-chan struct{}
	if b.ctx != nil {
		ctxDone = b.ctx.Done()
	}
	select {
	case <-ctxDone:
		// The agent disconnected: end the hold and record it, so the approval is
		// not left pending. Nothing is flushed; the connection is gone.
		if b.hold.finish() {
			b.recordDecided("the agent's connection closed before an answer")
		}
		b.err = context.Canceled
	case <-timer.C:
		now = b.g.now()
		if !now.Before(b.holdPolled.Add(b.g.pollDur())) {
			b.holdPolled = now
			if allow, decided := b.pollDecision(); decided && b.hold.finish() {
				b.endHold(allow)
				return
			}
		}
		if now.Before(deadline) && !now.Before(b.holdPinged.Add(b.g.pingDur())) {
			b.holdPinged = now
			b.out.Write(b.pingFrame())
		}
	}
}

// pollDecision asks control whether the held approval was decided, and how. A
// read error is "not yet decided", so a control briefly unreachable never
// refuses a call a person may still be answering.
func (b *guardBody) pollDecision() (allow, decided bool) {
	base := b.ctx
	if base == nil {
		base = context.Background()
	}
	// Bound each poll, so a control that stalls cannot block the pings or the
	// 60 s deadline: an over-long poll is treated as "not yet decided".
	ctx, cancel := context.WithTimeout(base, b.g.pollDur())
	defer cancel()
	allow, decided, err := b.g.decider.Decided(ctx, b.ex.turn, b.holdID)
	if err != nil {
		return false, false
	}
	return allow, decided
}

// endHold finalises a hold a person answered. On allow the held calls pass
// unchanged, so the agent runs them this turn; on refuse they are refused. The
// person's POST already recorded approval.decided, so nothing is recorded here.
func (b *guardBody) endHold(allow bool) {
	// finish() already moved the lifecycle to holdDone; here we act on the answer.
	b.setWriteDeadline(time.Time{}) // the reply is written as any other
	if !allow {
		b.refuseHeldAll(holdRefusedText)
	}
	b.flush()
}

// timeoutHold finalises a hold no one answered in time: the held calls are
// refused, approval.decided is recorded so the approval is no longer pending,
// and the turn ends with no automatic next turn (his call, 2026-11-17).
func (b *guardBody) timeoutHold() {
	// finish() already moved the lifecycle to holdDone; here we refuse and record.
	b.setWriteDeadline(time.Time{}) // the refusal is written as any other reply
	b.refuseHeldAll(holdTimeoutText)
	b.recordDecided("no one answered in time")
	b.flush()
}

// refuseHeldAll refuses every held call with text, so flush passes them renamed.
func (b *guardBody) refuseHeldAll(text string) {
	for _, h := range b.heldKeys {
		b.refuse(h.key, h.callID, h.name, h.input, h.rule, text)
	}
}

// recordDecided writes approval.decided for the held approval, so control's
// record shows it answered. The gateway records it only when the hold itself
// ends the approval (a timeout or a closed connection); a person's answer is
// recorded by the API that took it.
func (b *guardBody) recordDecided(text string) {
	if b.holdID == "" {
		return
	}
	b.g.emit(b.ex, []event.Event{{Kind: event.ApprovalDecided, Approval: b.holdID, Text: text}})
}

// pingFrame is one keep-alive frame for the agent's stream, per vendor: an
// Anthropic ping event, or an OpenAI comment line. Both are frames the agents
// already send and ignore (ask-hold-measurements.md).
func (b *guardBody) pingFrame() []byte {
	if b.vendor == "anthropic" {
		return []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")
	}
	return []byte(": keep-alive\n\n")
}
