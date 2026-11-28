// Package replay continues a session's conversation from the gateway's own
// store instead of the vendor's resume: own replay.
//
// Every turn of a session started by POST /api/v0/turns is a one-shot child
// process. A turn that continues a session by replay starts its child fresh,
// without --resume, and its turn.started names the earlier turn it continues
// (replay_from). On every main model request of that turn, the gateway puts
// the earlier turn's whole history in front: the messages of that turn's last
// request as the vendor received them, then that call's reply rebuilt from the
// stream, with tool_use, tool_result and thinking blocks (signatures included)
// as sent and received. The model then sees what a resumed process would have
// shown it.
//
// A turn that did not finish (its agent was stopped, the service restarted,
// or its last model call failed) never ends the session. The next turn
// continues from it: what it did, made valid for the API, then one note
// saying how it ended and to check the folder (endUnfinished, notReached).
//
// Three formats, one Replayer each: the Anthropic Messages format as Claude Code
// sends it (New), the OpenAI Responses format as codex sends it
// (NewCodex, codex.go), and the OpenAI Chat Completions format as OpenCode
// sends it (NewChatCompletions, chatcompletions.go). The rules are the same
// for all of them and live here: calls
// in request order, the newest main-thread call and never an older one, one
// identity rule for the main thread, refusal with replay.failed on a layout
// the gateway does not know, and the same note for a turn that did not
// finish. What a format knows of its vendor's layout is the format interface.
//
// The layout was worked out and measured with a Python tap against Claude Code
// 2.1.319 on 2026-11-06:
// a spliced turn-2 request equalled the resumed one in every message the model
// sees, read the history from cache, and a 10-turn run passed with 26 of 26
// requests accepted, 15 of them carrying replayed thinking blocks.
package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/store"
)

// Source is what a Replayer reads: the store.
type Source interface {
	TurnStart(session, turn string) (prompt, replayFrom string, transcript, found bool, err error)
	SessionTurns(session string) ([]string, error)
	TurnCalls(session, turn string) ([]store.Call, error)
	TurnIncomplete(session, turn string) (bool, error)
	TurnClosed(session, turn string) (bool, error)
	TurnNotes(session, turn string) ([]store.GivenNote, error)
	TurnEnd(session, turn string) (store.TurnEnd, error)
	CallContext(exchange string, full bool) (store.CallContext, bool, error)
	Reply(exchange string) (body.Reply, bool, error)
}

// Replayer splices an earlier turn's history into a replayed turn's requests.
// It is a gateway.Rewriter for one vendor's gateway.
type Replayer struct {
	src Source
	f   format

	mu    sync.Mutex
	turns map[[2]string]turnInfo // (session, turn): its turn.started, read once
	hists map[[2]string]*history // (session, turn): its history, once built
	mains map[[2]string]string   // (session, turn): its main thread's system identity
	// fits: (session, turn) of a replayed turn: the history its requests
	// carry, fitted to the budget once, at its first main request (fit).
	fits map[[2]string]*history
	// carried: (session, turn) of a replayed turn one of whose requests
	// went on with the history in front.
	carried map[[2]string]bool
	// budget gives the history budget, a share of the model's window.
	budget func() float64
	// notes gives a running turn its waiting notes (notes.go); nil gives none.
	// placed: (session, turn): notes a 2xx gave whose context.added the
	// store may not hold yet.
	notes  Notes
	placed map[[2]string][]event.Added
}

// New returns a Replayer for Claude Code's requests, reading src.
func New(src Source) *Replayer { return newReplayer(src, claude{}) }

// NewCodex returns a Replayer for codex's requests, reading src.
func NewCodex(src Source) *Replayer { return newReplayer(src, codex{}) }

// NewChatCompletions returns a Replayer for OpenCode's Chat Completions
// requests, reading src.
func NewChatCompletions(src Source) *Replayer { return newReplayer(src, chatCompletions{}) }

func newReplayer(src Source, f format) *Replayer {
	return &Replayer{src: src, f: f, turns: map[[2]string]turnInfo{}, hists: map[[2]string]*history{},
		mains: map[[2]string]string{}, fits: map[[2]string]*history{}, carried: map[[2]string]bool{},
		placed: map[[2]string][]event.Added{}}
}

// format is what own replay knows of one vendor's request layout. Every
// rule that is the same for all vendors stays in the Replayer.
type format interface {
	// read sorts a stored call: its conversation (messages, or input items),
	// its main-thread identity, and how many tools it offers.
	read(c store.CallContext) (msgs []json.RawMessage, id string, tools int)
	// recoverable says a turn's main-thread identity can be read back from
	// its stored calls when it is not in memory.
	recoverable() bool
	// mainLayout reports whether a stored request has the main thread's
	// layout and was not compacted; hasPrompt whether it carries the turn's
	// prompt at all.
	mainLayout(msgs []json.RawMessage, prompt string) bool
	hasPrompt(msgs []json.RawMessage, prompt string) bool
	// replyKept says a whole reply (status 200, no error) with this stop
	// reason has a kept reply the history needs.
	replyKept(stop string) bool
	// carry copies what a history keeps of its last request besides the
	// messages.
	carry(h *history, c store.CallContext)
	// asHistory lays out one stored request message as an earlier part of a
	// later request.
	asHistory(m json.RawMessage) (json.RawMessage, error)
	// reply lays out a kept reply as history, and says how it left the turn.
	// A tool call it asked for gets unanswered as its result (ending.results).
	reply(rep body.Reply, stop, unanswered string) ([]json.RawMessage, ending, error)
	// canEnd reports whether a history of a turn that did not finish may be
	// followed by the note. kept says it ends with the turn's kept reply.
	canEnd(msgs []json.RawMessage, kept bool) bool
	// prompt is the person's message; note is AgentBus's note.
	prompt(text string) json.RawMessage
	note(text string) json.RawMessage
	// refusal answers the agent in its vendor's error format.
	refusal(msg string) *gateway.Refusal
	// conversation is the request's field that holds its conversation;
	// userNote is AgentBus's note as a user message, and afterSummary the
	// place right after a compacted conversation's summary (notes.go).
	conversation() string
	userNote(text string) json.RawMessage
	afterSummary(msgs []json.RawMessage) int
	// parse reads a request; ok is false for a body that is not this
	// format's, which passes unchanged.
	parse(data []byte) (request, bool)
	// fresh reports whether a request has the layout a fresh process sends
	// on its turn; freshRule says that layout in words.
	fresh(req request, prompt string) bool
	freshRule() string
	// build is the request with the history in front, and the header edit
	// that goes with it, if any.
	build(req request, h *history, prompt string) ([]byte, func(http.Header), error)
	// otherModel lays out a history for a request to another model than the
	// one that made it, as after the person switched agents: what belongs to
	// the model or the process that made it goes, the conversation stays.
	otherModel(msgs []json.RawMessage) ([]json.RawMessage, error)

	// What the history budget needs (budget.go).
	//
	// isPrompt reports whether m is the person's message of a turn whose
	// prompt is prompt.
	isPrompt(m json.RawMessage, prompt string) bool
	// shorten is m with each tool result (inputs false) or each tool call's
	// input (inputs true) longer than over bytes made the stub stub writes,
	// and how many it made. A message it cannot read stays as it is.
	shorten(m json.RawMessage, inputs bool, over int, stub func(kind string, size int, head string) string) (json.RawMessage, int)
	// said is what m holds for a summary: the model's words and AgentBus's
	// notes, and its tool calls, one line each (callLine).
	said(m json.RawMessage) (texts, calls []string)
	// summaryIn finds AgentBus's summary of earlier turns in m: its text,
	// and m without it and without the harness's context (nil when m held
	// only the summary and that context).
	summaryIn(m json.RawMessage) (text string, rest json.RawMessage, ok bool)
	// context is the harness's own standing context inside m, the person's
	// message, and m without it: Claude Code's system-reminder blocks in a
	// session's first message. Formats whose harness puts its context in
	// messages of its own (the head) have none here.
	context(m json.RawMessage) (ctx []json.RawMessage, rest json.RawMessage)
	// withSummary lays the summary out in front of first, the person's
	// message of the oldest turn kept, with ctx (from context) kept in
	// front of it, byte for byte.
	withSummary(ctx []json.RawMessage, first json.RawMessage, text string) []json.RawMessage
	// withStanding is h with the standing context of req, a fresh request,
	// put in where h lacks it (Claude Code: a first message whose context a
	// step before 0.6.8 summarised away), before the budget fits h. h itself
	// is never changed.
	withStanding(h *history, req request) *history
	// passOn is a main request the harness compacted, as it goes on: its
	// conversation untouched, under the identity the turn's other requests
	// went out with, so the record keeps the turn one thread; h is the
	// history the turn carried. nil sends the request as it came.
	passOn(req request, h *history) ([]byte, func(http.Header))
}

// request is one model request as the splice reads it.
type request struct {
	fields []body.Field
	msgs   []json.RawMessage // the conversation: messages, or input items
	tools  int
	id     string // its main-thread identity
}

// ending is how a kept reply left its turn.
type ending struct {
	finished bool
	// asked says the reply asked for a tool; results are what the history
	// gets in place of the results that never came.
	asked   bool
	results []json.RawMessage
}

// notFinished is the result a history gives a tool call whose result never
// came back.
const notFinished = "Not finished: the turn was interrupted before this tool's result came back. It may have run in part."

type turnInfo struct {
	turn, prompt, from string
	// transcript: the turn got the earlier turns as text in its prompt, so
	// no earlier history stands behind it (another harness's, perhaps).
	transcript bool
	found      bool
}

// history is a finished turn's conversation, laid out as an earlier part of a
// later request: every message without cache_control, a role-system message
// as a plain string, and the turn's last reply at the end.
type history struct {
	messages []json.RawMessage
	// prior is the prior_turn_context of the turn's last request, a JSON
	// array, or nil (Claude Code).
	prior []json.RawMessage
	// thread is the thread id of the turn's last request (codex): the id
	// every later request of the session is sent under.
	thread string
	// failed, when set, says how the turn ended without finishing: the
	// history then ends with a note saying so (endUnfinished).
	failed string
	// model is the model the turn's last request asked for, or "".
	model string
	// tools are the tool definitions of the turn's last request (Claude
	// Code): among them each deferred tool ToolSearch loaded (carryLoaded).
	tools []json.RawMessage
	// src is where each message's full bytes are in the record, for the
	// stubs the budget writes; shorter than messages when the last ones
	// are AgentBus's own (a note, a result that never came).
	src []source
}

// memoMax bounds each memo. Past it the memo starts over: an entry is only a
// saved read.
const memoMax = 256

// ErrNoBase means the session has no turn to continue from.
var ErrNoBase = errors.New("replay: the session has no turn to continue from")

// ErrPending means a call the newest turn's history needs is not fully
// stored: its request, or the reply of a call that finished. An older answer
// never stands in for it.
var ErrPending = errors.New("replay: a call of the newest turn is not stored")

// ErrUnusable means the newest turn's history cannot be rebuilt: it may be
// missing events, its last main-thread request was compacted, it ended
// normally with none of its calls stored, or a stored message cannot be laid
// out. No older turn stands in for it.
var ErrUnusable = errors.New("replay: the newest turn's history cannot be rebuilt")

// NoBase is Base's answer when the session cannot be continued by replay. Why
// says the reason in plain words, naming the turn; errors.Is matches it to
// ErrNoBase, ErrPending or ErrUnusable.
type NoBase struct {
	Why string
	err error
}

func (e *NoBase) Error() string { return e.err.Error() + ": " + e.Why }
func (e *NoBase) Unwrap() error { return e.err }

// Plan is the turn a new turn of a session continues, and whether that turn
// failed. Failed, when set, says how it ended without finishing, in the
// words its history's note tells the model.
type Plan struct {
	Turn   string
	Failed string
}

// Base names the turn a new turn of session continues: always the session's
// newest turn, never an older one in its place. Its history must be one the
// gateway can rebuild (history); if it is not, the error is a *NoBase saying
// why. A turn that did not finish (interrupted, its last model call failed,
// or it never reached the model) still has a history: what it did, made
// valid, then a note saying how it ended (endUnfinished). Plan.Failed then says
// how. Base keeps nothing it reads.
//
// Base reads a turn as final. The service makes that true: a turn's
// run.ended is stored only after the gateway has written all of the
// turn's calls (gateway.Settle), and a next turn starts only once the session
// is closed.
func (r *Replayer) Base(session string) (Plan, error) {
	turns, err := r.src.SessionTurns(session)
	if err != nil {
		return Plan{}, err
	}
	if len(turns) == 0 {
		return Plan{}, &NoBase{Why: "it has no turn", err: ErrNoBase}
	}
	h, err := r.history(session, turns[0], false)
	if err != nil {
		var miss *missing
		if !errors.As(err, &miss) {
			return Plan{}, err
		}
		sentinel := ErrUnusable
		if miss.kind == pendingCall {
			sentinel = ErrPending
		}
		return Plan{}, &NoBase{Why: miss.why, err: sentinel}
	}
	return Plan{Turn: turns[0], Failed: h.failed}, nil
}

// missing is a turn without a usable history: not a store failure.
type missing struct {
	why  string
	kind missingKind
}

type missingKind int

const (
	unusableCall missingKind = iota // its history cannot be rebuilt
	pendingCall                     // a call its history needs is not fully stored
)

func (m *missing) Error() string { return m.why }

func (r *Replayer) turn(session, turn string) (turnInfo, error) {
	k := [2]string{session, turn}
	r.mu.Lock()
	ti, ok := r.turns[k]
	r.mu.Unlock()
	if ok {
		return ti, nil
	}
	p, from, transcript, found, err := r.src.TurnStart(session, turn)
	if err != nil {
		return ti, err
	}
	ti = turnInfo{turn: turn, prompt: p, from: from, transcript: transcript, found: found}
	// A turn's turn.started is stored before its child starts and never
	// changes, so a turn not found yet is not kept: a later read may find it.
	if found {
		r.mu.Lock()
		if len(r.turns) >= memoMax {
			r.turns = map[[2]string]turnInfo{}
		}
		r.turns[k] = ti
		r.mu.Unlock()
	}
	return ti, nil
}

// history builds turn's history from its newest main-thread call, by request
// order. Calls are read from model.requested, so a call whose reply event was
// lost is still seen, and a turn that may be missing events (TurnIncomplete)
// has no usable history. Which calls are the main thread's is decided by one
// rule (isMain): the main thread's identity is the system prompt of the
// turn's first request that has the main layout, and a call is the main
// thread's exactly when its system prompt is that one. No older call is ever
// used in place of the newest main one.
//
// When the newest main call is one its harness compacted (compacted), the
// history is built from the turn's last call before that, never from the
// harness's summary (compactedHistory). Otherwise:
//   - a finished reply (status 200, no error, a stop reason other than
//     tool_use, pause_turn or none): the request's messages and the reply;
//   - a reply that asked for a tool (status 200, no error, tool_use): the
//     turn stopped inside its tool loop. The request's messages, the reply, a tool_result
//     marked as an error for each tool_use, then the note (endUnfinished);
//   - no usable reply (no reply event, a status other than 200, a reply with
//     an error such as a stream that ended before message_stop, a reply cut
//     off or paused): the request's messages, then the note.
//
// A turn whose calls are all side calls (no tools, no prompt), or that made
// none, never reached the model; a call in a layout the gateway does not know
// gives no history. If a turn that never reached the model failed, its
// history is the previous turn's, then the person's message and a note that
// it was not acted on. If it ended normally (exit 0), its calls
// are not in the store, as in a session from before calls were stored: there
// is no history.
//
// keep stores a built history for later requests; Base does not keep.
func (r *Replayer) history(session, turn string, keep bool) (*history, error) {
	k := [2]string{session, turn}
	r.mu.Lock()
	h, ok := r.hists[k]
	r.mu.Unlock()
	if ok {
		return h, nil
	}
	ti, err := r.turn(session, turn)
	if err != nil {
		return nil, err
	}
	if !ti.found {
		return nil, &missing{why: "turn " + turn + " has no turn.started"}
	}
	// A turn that may be missing events cannot say which call was its last.
	incomplete, err := r.src.TurnIncomplete(session, turn)
	if err != nil {
		return nil, err
	}
	if incomplete {
		return nil, &missing{why: "turn " + turn + " may be missing events (events were dropped, or the service could not confirm its calls were stored), so its history cannot be rebuilt", kind: unusableCall}
	}
	answered, err := r.src.TurnCalls(session, turn) // every request, newest first
	if err != nil {
		return nil, err
	}
	calls := make([]call, 0, len(answered))
	for _, a := range answered {
		c, ok, err := r.src.CallContext(a.Exchange, true)
		if err != nil {
			return nil, err
		}
		if !ok {
			// Without its request the call cannot be classified.
			return nil, &missing{why: "turn " + turn + "'s call " + a.Exchange + " has no stored request", kind: pendingCall}
		}
		msgs, id, tools := r.f.read(c)
		calls = append(calls, call{a: a, c: c, msgs: msgs, id: id, tools: tools})
	}
	// The main thread's identity: the oldest request with the main layout.
	main, known := "", false
	for i := len(calls) - 1; i >= 0; i-- {
		if r.f.mainLayout(calls[i].msgs, ti.prompt) {
			main, known = calls[i].id, true
			break
		}
	}
	if !known {
		// No request has the main layout. The turn never reached the model
		// only if every call is a side call, by the same rule the splice
		// uses before the main thread's first request: no tools and no
		// prompt. Any other call may be the main thread's in a layout the
		// gateway does not know; reading the turn as not acted on would
		// drop its real work, so there is no history.
		for _, c := range calls {
			if c.tools > 0 || r.f.hasPrompt(c.msgs, ti.prompt) {
				return nil, &missing{why: "turn " + turn + "'s call " + c.a.Exchange +
					" is in a layout the gateway does not know (no request of the turn has the main thread's layout), so its history cannot be rebuilt", kind: unusableCall}
			}
		}
		h, err = r.notReached(session, turn, ti, keep)
		if err != nil {
			return nil, err
		}
		return r.keep(k, h, keep), nil
	}
	// The turn's main-thread calls, newest first.
	var mains []call
	for _, c := range calls {
		if isMain(c.id, main, true) || c.a.Compacted {
			mains = append(mains, c)
		}
	}
	newest := &mains[0]
	x := newest.a.Exchange
	if r.compacted(*newest, ti) {
		// The harness compacted its context inside the turn. The history is
		// built from the record, never from the harness's summary.
		h, err = r.compactedHistory(session, turn, ti, mains)
		if err != nil {
			return nil, err
		}
		return r.keep(k, h, keep), nil
	}
	a := newest.a
	// A reply with an error did not arrive whole: its stop reason is what
	// came before the stream broke, and the reply was never kept. It counts
	// as no usable reply, whatever its stop reason says.
	whole := a.Responded && a.Status == 200 && a.Error == ""
	kept := whole && r.f.replyKept(a.Stop)
	var rep body.Reply
	if kept {
		var ok bool
		rep, ok, err = r.src.Reply(x)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &missing{why: "turn " + turn + "'s last main-thread reply " + x + " is not stored", kind: pendingCall}
		}
	}
	h, err = r.fromCall(turn, newest.c, newest.msgs)
	if err != nil {
		return nil, err
	}
	var e ending
	if kept {
		items, how, err := r.f.reply(rep, a.Stop, notFinished)
		if err != nil {
			return nil, &missing{why: "turn " + turn + "'s stored reply cannot be replayed: " + err.Error(), kind: unusableCall}
		}
		h.add(turn, x, -1, items...)
		e = how
	}
	// A finished reply ends the history whole, even for a turn the person
	// stopped after that reply came and before its agent exited: the model
	// had done the turn's work, and Claude Code's layout has no place for a
	// note after a final reply (the note is an assistant message). The stop
	// is still on the turn's run.ended and in the turn list.
	if !e.finished {
		end, err := r.src.TurnEnd(session, turn)
		if err != nil {
			return nil, err
		}
		if err := endUnfinished(r.f, h, turn, a, end, kept, e); err != nil {
			return nil, err
		}
	}
	return r.keep(k, h, keep), nil
}

// fromCall starts a history from one stored call's request: its messages
// laid out as history, each with where it is in the record.
func (r *Replayer) fromCall(turn string, c store.CallContext, msgs []json.RawMessage) (*history, error) {
	h := &history{model: body.Label(c.Request, "model")}
	r.f.carry(h, c)
	var pieces []int
	for i, p := range c.Pieces {
		if p.Kind == body.Message || p.Kind == body.InputItem {
			pieces = append(pieces, i)
		}
	}
	for i, m := range msgs {
		hm, err := r.f.asHistory(m)
		if err != nil {
			return nil, &missing{why: "turn " + turn + "'s stored request cannot be replayed: " + err.Error(), kind: unusableCall}
		}
		n := -2
		if i < len(pieces) {
			n = pieces[i]
		}
		h.add(turn, c.Exchange, n, hm)
	}
	return h, nil
}

// add appends msgs, which are piece n of call (-1: its reply; -2: not
// known) of turn, to h.
func (h *history) add(turn, call string, n int, msgs ...json.RawMessage) {
	for len(h.src) < len(h.messages) {
		h.src = append(h.src, source{})
	}
	for _, m := range msgs {
		h.messages = append(h.messages, m)
		if n == -2 {
			h.src = append(h.src, source{})
		} else {
			h.src = append(h.src, source{turn: turn, call: call, piece: n})
		}
	}
}

// compactedResult is what a tool call gets as its result in the history of a
// turn whose harness compacted its context after the call: it ran, and its
// result is in the record.
const compactedResult = "This tool ran, but the agent's harness compacted its context afterwards, so its result is not shown here. It is in AgentBus's record."

// compactedHistory is the history of a turn whose harness compacted its
// context inside the turn, built from the record and never from the
// harness's summary: the newest main call that still has the main layout,
// with its reply when that reply asked for tools (they ran: compactedResult),
// then one note saying the turn went on, how many model calls followed, and
// the model's last words when its newest reply finished the turn. mains are
// the turn's main-thread calls, newest first, the newest one compacted
// (compacted).
func (r *Replayer) compactedHistory(session, turn string, ti turnInfo, mains []call) (*history, error) {
	var base *call
	var rep body.Reply
	asked := false
	for i := range mains {
		c := &mains[i]
		if r.compacted(*c, ti) {
			continue
		}
		if base == nil {
			base = c
		}
		if !(c.a.Responded && c.a.Status == 200 && c.a.Error == "" && r.f.replyKept(c.a.Stop)) {
			continue
		}
		got, ok, err := r.src.Reply(c.a.Exchange)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, e, err := r.f.reply(got, c.a.Stop, compactedResult); err == nil && e.asked {
			base, rep, asked = c, got, true
			break
		}
	}
	if base == nil {
		return nil, &missing{why: "turn " + turn + "'s harness compacted its context and none of its main-thread requests from before that is stored, so its history cannot be rebuilt", kind: unusableCall}
	}
	after := 0
	for _, c := range mains {
		if c.a.Exchange == base.a.Exchange {
			break
		}
		after++
	}
	h, err := r.fromCall(turn, base.c, base.msgs)
	if err != nil {
		return nil, err
	}
	if asked {
		items, e, err := r.f.reply(rep, base.a.Stop, compactedResult)
		if err != nil {
			return nil, &missing{why: "turn " + turn + "'s stored reply cannot be replayed: " + err.Error(), kind: unusableCall}
		}
		h.add(turn, base.a.Exchange, -1, items...)
		h.add("", "", -2, e.results...)
	}
	if !r.f.canEnd(h.messages, asked) {
		return nil, &missing{why: "turn " + turn + "'s last main-thread request before its harness compacted ends in a layout the gateway does not know", kind: unusableCall}
	}
	// The model's last words: the newest reply, when it finished the turn.
	last, finished := "", false
	if n := mains[0]; n.a.Responded && n.a.Status == 200 && n.a.Error == "" && r.f.replyKept(n.a.Stop) {
		if got, ok, err := r.src.Reply(n.a.Exchange); err != nil {
			return nil, err
		} else if ok {
			if items, e, err := r.f.reply(got, n.a.Stop, compactedResult); err == nil && e.finished {
				finished = true
				for _, it := range items {
					if t, _ := r.f.said(it); len(t) > 0 {
						last = t[len(t)-1]
					}
				}
			}
		}
	}
	end, err := r.src.TurnEnd(session, turn)
	if err != nil {
		return nil, err
	}
	how := ""
	switch {
	case end.Error != "":
		how = strings.TrimSuffix(end.Error, ".")
	case end.Stopped:
		how = stoppedWords
	case end.ExitCode != nil && *end.ExitCode != 0:
		how = exitWords(*end.ExitCode)
	case !finished:
		how = "its last model call did not finish the turn"
	}
	calls := " more model calls"
	if after == 1 {
		calls = " more model call"
	}
	note := notePrefix + "The turn above went on after this point for " + strconv.Itoa(after) +
		calls + ", but its agent's harness compacted its context, so AgentBus shows the rest only in short."
	if last != "" {
		note += " The model's last words in the turn were:\n" + last + "\n"
	}
	if how != "" {
		h.failed = "its harness compacted its context, and " + how
		note += " The turn did not finish: " + how + "."
	}
	note += " Every call of the turn is in AgentBus's record: " + apiGet("/api/v0/turns/"+turn+"/record") +
		". It may have changed files: check the folder before going on."
	h.add("", "", -2, r.f.note(note))
	// The notes given after base, which it does not carry, are said here,
	// at the next turn's start (notes.go).
	later, err := r.notesAfter(session, turn, mains, base)
	if err != nil {
		return nil, err
	}
	h.add("", "", -2, later...)
	return h, nil
}

// call is one stored call of a turn, as history reads it.
type call struct {
	a     store.Call
	c     store.CallContext
	msgs  []json.RawMessage
	id    string
	tools int
}

// compacted says a main-thread call is one its harness compacted: a request
// not in the layout a fresh process sends (format.fresh), the one test of a
// compaction. Who applies it depends on whether the record holds the agent's
// own request:
//   - a turn that carries no history (no replay_from) is never spliced, so
//     its stored requests are the agent's own bytes, and the history applies
//     the test to them. The gateway applies the same test to the same bytes
//     (judge), so the two cannot disagree. This reads a record from before
//     history.compacted, and a call the gateway could not judge (its main
//     identity was not in memory after a restart), the same way (review of
//     cbb539f: both were read as uncompacted).
//   - a turn that carries a history has spliced requests in the record, which
//     are not the agent's; there only the gateway's recorded verdict says. A
//     request there that is not fresh before the history was carried is
//     refused (splice), so it never stands in the record unjudged, and builds
//     before 0.6.7 refused every such request.
//
// Not model.requested's replayed: a call through control's relay, as every
// runner's is, records 0 there even when the runner's gateway spliced it.
func (r *Replayer) compacted(c call, ti turnInfo) bool {
	if c.a.Compacted {
		return true
	}
	return ti.from == "" && !r.f.fresh(request{msgs: c.msgs, tools: c.tools, id: c.id}, ti.prompt)
}

// keep stores h as turn k's history when keep is set, and returns it.
func (r *Replayer) keep(k [2]string, h *history, keep bool) *history {
	if keep {
		r.mu.Lock()
		if len(r.hists) >= memoMax {
			r.hists = map[[2]string]*history{}
		}
		r.hists[k] = h
		r.mu.Unlock()
	}
	return h
}

// notReached is the history of a turn whose main thread made no call. One
// that failed (it ended with an error or a nonzero exit) was never acted on:
// its history is the previous turn's, then the person's message, then a note
// saying it was not acted on. A session's first turn has no previous one.
func (r *Replayer) notReached(session, turn string, ti turnInfo, keep bool) (*history, error) {
	end, err := r.src.TurnEnd(session, turn)
	if err != nil {
		return nil, err
	}
	if !end.Closed {
		return nil, &missing{why: "turn " + turn + " made no model call and has not ended", kind: unusableCall}
	}
	if end.Error == "" && end.ExitCode != nil && *end.ExitCode == 0 && !end.ReplayFailed && !end.Stopped {
		return nil, &missing{why: "turn " + turn + " ended normally but none of its model calls are stored", kind: unusableCall}
	}
	if end.ExitCode == nil && end.Error == "" && !end.ReplayFailed {
		// Closed with no exit code and no error: nothing says it failed.
		return nil, &missing{why: "turn " + turn + " made no model call and its end is not known", kind: unusableCall}
	}
	turns, err := r.src.SessionTurns(session)
	if err != nil {
		return nil, err
	}
	// The previous turn's history comes first, except behind a turn that
	// got the earlier turns as text: its prompt already holds them, and the
	// turn before it may be another harness's, in another format.
	prev := ""
	for i, t := range turns {
		if t == turn && i+1 < len(turns) && !ti.transcript {
			prev = turns[i+1]
		}
	}
	h := &history{}
	if prev != "" {
		ph, err := r.history(session, prev, keep)
		if err != nil {
			return nil, err
		}
		h.messages = append(h.messages, ph.messages...)
		h.src = append(h.src, ph.src...)
		h.prior, h.thread, h.model, h.tools = ph.prior, ph.thread, ph.model, ph.tools
	}
	why := "it ended before the model was called"
	switch {
	case end.ReplayFailed:
		why = "AgentBus could not give it the earlier conversation, so the model was not called"
	case end.Error != "":
		why += " (" + strings.TrimSuffix(end.Error, ".") + ")"
	case end.Stopped:
		why += " (" + stoppedWords + ")"
	case end.ExitCode != nil:
		why += " (" + exitWords(*end.ExitCode) + ")"
	}
	h.failed = "it was not acted on: " + why
	h.messages = append(h.messages, r.f.prompt(ti.prompt),
		r.f.note(notePrefix+"The message above was not acted on: "+why+"."))
	return h, nil
}

// notePrefix starts every note AgentBus writes into a history, so the model
// never takes it for its own words.
const notePrefix = "[Note from AgentBus, not written by the model] "

// endUnfinished ends the history of a turn that did not finish. A reply that
// asked for tools gets, for each call, a result marked as an error (the
// format's ending.results), since no result came back. Then one note, as the
// last message, says how the turn ended, that it may have changed files in
// part, and to check the folder. For Claude Code the note is an assistant
// message, so the history alternates as a finished turn's does (a
// role-system message is never followed by a user message in Claude Code's
// layout), and a new turn's message can follow it. kept says the history ends
// with the turn's kept reply, which e describes.
func endUnfinished(f format, h *history, turn string, a store.Call, end store.TurnEnd, kept bool, e ending) error {
	if len(h.messages) == 0 {
		return &missing{why: "turn " + turn + "'s last main-thread request has no messages", kind: unusableCall}
	}
	var how string
	switch {
	case kept && e.asked && len(e.results) > 0:
		h.messages = append(h.messages, e.results...)
		how = "it stopped while a tool was running"
	case kept && e.asked:
		how = "it stopped after the model asked for a tool"
	case kept:
		// The reply said the turn goes on after it (codex's end_turn false).
		how = "it stopped before the model finished its work"
	case !a.Responded:
		how = "its last model request got no reply"
	case a.Status == 0:
		// No vendor answered: the agent closed the call's connection itself,
		// as when its turn is stopped (the gateway stores no status for it).
		// It is not a vendor failure, and the note must not call it one
		// (acceptance soak, 2026-11-06: the model told the person "the turn
		// ended with a 502").
		how = "it was stopped while its last model request was in flight"
		if a.Error != "" {
			how += " (" + strings.TrimSuffix(a.Error, ".") + ")"
		}
		how += ", so that request got no reply"
	case a.Status != 200:
		how = "its last model request failed with status " + strconv.Itoa(a.Status)
	case a.Error != "":
		how = "its last reply did not arrive whole (" + strings.TrimSuffix(a.Error, ".") + "), so that reply is left out"
	case a.Stop == "pause_turn":
		how = "its last reply paused and was not continued, so that reply is left out"
	default:
		how = "its last reply was cut off, so that reply is left out"
	}
	if !f.canEnd(h.messages, kept) {
		// A request that ended in a way the format cannot follow with the
		// note: for Claude Code, the model's own words (a prefill); for
		// codex, a tool call with no result.
		return &missing{why: "turn " + turn + "'s last main-thread request ends in a layout the gateway does not know (an assistant message for Claude Code, a tool call with no result for codex)", kind: unusableCall}
	}
	switch {
	case end.Error != "":
		how += "; " + strings.TrimSuffix(end.Error, ".")
	case end.Stopped:
		how += "; " + stoppedWords
	case end.ExitCode != nil && *end.ExitCode != 0:
		how += "; " + exitWords(*end.ExitCode)
	}
	h.failed = how
	h.messages = append(h.messages, f.note(notePrefix+"The turn above did not finish: "+how+
		". It may have changed files in part. Check the folder before going on."))
	return nil
}

// stoppedWords says the person stopped a turn (run.ended's stopped).
const stoppedWords = "the person stopped this turn"

// exitWords says how an agent's process ended, from its exit code.
func exitWords(code int) string {
	switch code {
	case 143:
		return "its agent process was stopped (signal 15, exit 143), as when it is stopped or the service restarts"
	case 137:
		return "its agent process was killed (signal 9, exit 137)"
	case 130:
		return "its agent process was interrupted (signal 2, exit 130)"
	}
	return "its agent process exited with code " + strconv.Itoa(code)
}

// noteMessage is an assistant message holding one text block.
func noteMessage(text string) json.RawMessage {
	block := body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)}, {Key: "text", Value: jsonString(text)}})
	return body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"assistant"`)},
		{Key: "content", Value: joinArray([]json.RawMessage{block})}})
}

// toolUseIDs lists the ids of an assistant message's tool_use blocks, the
// client tools that wait for a tool_result. A server tool's block
// (server_tool_use) carries its own result and is not listed.
func toolUseIDs(m json.RawMessage) ([]string, error) {
	if role(m) != "assistant" {
		return nil, errors.New("the reply is not an assistant message")
	}
	blocks, ok := body.Elements(field(m, "content"))
	if !ok {
		return nil, errors.New("the reply's content is not a list")
	}
	var ids []string
	for _, b := range blocks {
		if body.Label(b, "type") != "tool_use" {
			continue
		}
		var id string
		if json.Unmarshal(field(b, "id"), &id) != nil || id == "" {
			return nil, errors.New("a tool_use block has no id")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// mainIdentity returns the turn's main-thread identity, fixing it from this
// request when the turn has none yet and the request is in the fresh layout.
// A turn with no identity in memory that already has calls in the store
// (its identity was evicted, or the service restarted) gets it back from its
// oldest stored main-layout call, never from this request; if it cannot be
// recovered, the request is refused.
func (r *Replayer) mainIdentity(session string, ti turnInfo, id string, fresh bool) (string, bool, error) {
	k := [2]string{session, ti.turn}
	r.mu.Lock()
	main, known := r.mains[k]
	r.mu.Unlock()
	if known || !fresh {
		return main, known, nil
	}
	calls, err := r.src.TurnCalls(session, ti.turn)
	if err != nil {
		return "", false, errors.New("the store could not be read")
	}
	if len(calls) > 0 {
		main, found, unread, err := r.storedIdentity(calls, ti.prompt)
		switch {
		case err != nil:
			return "", false, errors.New("the store could not be read")
		case found:
			id = main
		case unread:
			// A call whose request is not stored could be the main
			// thread's first: this request cannot be trusted to be it.
			return "", false, errors.New("the turn's main-thread identity is not in memory and cannot be recovered from its stored calls")
		}
		// Otherwise its earlier calls are side calls: this request is the
		// main thread's first.
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.mains[k]; ok {
		return m, true, nil
	}
	if len(r.mains) >= memoMax {
		r.evictClosed()
	}
	r.mains[k] = id
	return id, true, nil
}

// storedIdentity reads the main-thread identity from a turn's stored calls
// (newest first): the system prompt of the oldest one with the main layout.
// unread says a call's request was not stored, so it could not be read.
func (r *Replayer) storedIdentity(calls []store.Call, prompt string) (id string, found, unread bool, err error) {
	for i := len(calls) - 1; i >= 0; i-- {
		c, ok, err := r.src.CallContext(calls[i].Exchange, true)
		if err != nil {
			return "", false, false, err
		}
		if !ok {
			unread = true
			continue
		}
		msgs, id, _ := r.f.read(c)
		if r.f.mainLayout(msgs, prompt) {
			if !r.f.recoverable() {
				// The stored request is not the one the agent sent (codex:
				// its thread id was rewritten), so it cannot say which of
				// the agent's requests is the main thread's.
				return "", false, true, nil
			}
			return id, true, unread, nil
		}
	}
	return "", false, unread, nil
}

// evictClosed drops the identities, and the notes placed, of turns whose
// child has exited. An open
// turn's identity is never dropped: the memo then grows past memoMax, bounded
// by the turns running at once. Called with r.mu held.
func (r *Replayer) evictClosed() {
	for k := range r.mains {
		if closed, err := r.src.TurnClosed(k[0], k[1]); err == nil && closed {
			delete(r.mains, k)
		}
	}
	for k := range r.placed {
		if closed, err := r.src.TurnClosed(k[0], k[1]); err == nil && closed {
			delete(r.placed, k)
		}
	}
}

// isMain is the one rule for the main thread: once its identity is known, a
// request is the main thread's exactly when its system prompt is that one,
// whatever its messages say. A subagent given the person's words verbatim is
// still not main; a compacted main request is still main.
func isMain(id, main string, known bool) bool { return known && id == main }

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// Rewrite is the gateway's hook: for a request of a replayed turn's main
// thread, the request with the earlier turn's history in front.
//
// A request passes unchanged when its turn is not replayed, or when it is not
// the main thread's (isMain: its system prompt is not the one the turn's first
// fresh-layout request fixed). A main-thread request not in the fresh layout
// is refused with replay.failed: sending it on without the history would give
// the model a turn with no past, which it would answer as if that were the
// whole conversation.
//
// Either way, a main-thread request carries the turn's notes first
// (withNotes), as if the agent had sent them.
func (r *Replayer) Rewrite(session, turn string, data []byte) gateway.Rewrite {
	nc := r.withNotes(session, turn, data)
	rw := r.rewrite(session, turn, nc.data)
	if rw.Refuse != nil {
		if len(nc.taken) > 0 {
			r.notes.ReturnNotes(session, turn, nc.taken)
		}
		return rw
	}
	if rw.Body == nil && nc.changed {
		rw.Body = nc.data
	}
	rw.Replied = r.replied(session, turn, nc)
	return rw
}

func (r *Replayer) rewrite(session, turn string, data []byte) gateway.Rewrite {
	ti, err := r.turn(session, turn)
	if err != nil {
		return r.refuse("the store could not be read")
	}
	if !ti.found {
		return gateway.Rewrite{}
	}
	if ti.from == "" {
		// A turn that carries no history goes on as sent; its main requests
		// are still judged, so a compaction in it is in the record too.
		return gateway.Rewrite{Events: r.judge(session, ti, data)}
	}
	out, hdr, n, pass, evs, err := r.splice(session, ti, data)
	if err != nil {
		return r.refuse(err.Error())
	}
	if pass {
		return gateway.Rewrite{Events: evs}
	}
	return gateway.Rewrite{Body: out, Replayed: n, Header: hdr, Events: evs}
}

// judge records a compaction in a turn that carries no history: a request of
// the turn's main thread not in the fresh layout, by the same test splice
// forwards by. Nothing else is decided here and nothing is refused.
func (r *Replayer) judge(session string, ti turnInfo, data []byte) []event.Event {
	req, ok := r.f.parse(data)
	if !ok {
		return nil
	}
	fresh := r.f.fresh(req, ti.prompt)
	main, known, err := r.mainIdentity(session, ti, req.id, fresh)
	if err != nil || !known || !isMain(req.id, main, known) || fresh {
		return nil
	}
	return compactedEvent()
}

func (r *Replayer) refuse(why string) gateway.Rewrite {
	return gateway.Rewrite{
		Refuse: r.f.refusal("agentbus replay: " + why),
		Events: []event.Event{{Kind: event.ReplayFailed, Error: why}},
	}
}

// The splice's markers in Claude Code 2.1.319's first system message, the
// message right after the person's (seen 2026-11-06). A fresh process puts
// there: its SessionStart hook text, then "# Environment" with the
// environment, deferred tools, skills listing, then the UserPromptSubmit hook
// text, then the date. A resumed process sends only the two hook texts: the
// rest is already in the replayed history's own copy of that message.
const (
	envMark  = "\n\n# Environment\n"
	upsMark  = "\n\nUserPromptSubmit hook additional context:"
	dateMark = "\n\nToday's date is "
)

// Two parts of the environment that change when the turn's tools change
// (Claude Code 2.1.321, 2026-11-11, read from a fresh turn's request): the
// list of deferred tools, one name a line after this header, and the
// instructions MCP servers give, a section up to the skills listing. An MCP
// server's tools are deferred, so a tool server new to the session is in
// both and nowhere else.
const (
	deferredMark = "\n\nThe following deferred tools are now available via ToolSearch."
	mcpMark      = "\n\n# MCP Server Instructions\n"
	skillsMark   = "\n\nThe following skills are available"
)

// cutTurnSystem keeps a fresh first system message's hook texts: everything
// before the environment, and the UserPromptSubmit hook text without the date
// after it. Without a UserPromptSubmit hook, everything from the environment
// on goes. It fails when the environment marker is missing.
//
// prior is the text of the history's system messages. What the environment
// adds to it is kept too, between the two hook texts, where the environment
// was (newInEnv): a turn whose tools changed since the earlier one, such as
// one given the tool server after [agent_api] tools was turned on, would
// otherwise see only the earlier turn's copy, and never learn of its new
// tools (found on his runner, 2026-11-11: the tools were listed and the
// model never saw their names).
func cutTurnSystem(t, prior string) (string, error) {
	i := strings.Index(t, envMark)
	if strings.HasPrefix(t, envMark[2:]) {
		// No SessionStart hook text: the message starts with the section.
		i = 0
	}
	if i < 0 {
		return "", errors.New(`the first system message has no "# Environment" section, so the harness's layout is not one the gateway knows`)
	}
	head, rest := t[:i], t[i:]
	var ups string
	if u := strings.Index(rest, upsMark); u >= 0 {
		ups = rest[u+2:]
		if d := strings.LastIndex(ups, dateMark); d >= 0 {
			ups = ups[:d]
		}
	}
	var parts []string
	for _, p := range []string{head, newInEnv(rest, prior), ups} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// newInEnv is what env, a fresh environment, tells the model that prior does
// not: the deferred tools prior never names, under the same header, and the
// MCP instructions section when prior does not hold it as it is now. It is
// "" when there is nothing new, so an unchanged turn's request stays as it
// was. A tool prior names and env does not is not announced as gone: the
// request's own tools list is the new process's, and a deferred tool that is
// gone is not found by ToolSearch.
func newInEnv(env, prior string) string {
	var parts []string
	if i := strings.Index(env, deferredMark); i >= 0 {
		list := env[i+2:]
		if j := strings.Index(list, "\n\n"); j >= 0 {
			list = list[:j]
		}
		header, names, _ := strings.Cut(list, "\n")
		known := map[string]bool{}
		for _, l := range strings.Split(prior, "\n") {
			known[l] = true
		}
		var add []string
		for _, n := range strings.Split(names, "\n") {
			if n != "" && !known[n] {
				add = append(add, n)
			}
		}
		if len(add) > 0 {
			parts = append(parts, header+"\n"+strings.Join(add, "\n"))
		}
	}
	if i := strings.Index(env, mcpMark); i >= 0 {
		sec := env[i+2:]
		end := len(sec)
		for _, m := range []string{skillsMark, "\n\n# ", upsMark, dateMark} {
			if j := strings.Index(sec, m); j >= 0 && j < end {
				end = j
			}
		}
		if sec = sec[:end]; !strings.Contains(prior, sec) {
			parts = append(parts, sec)
		}
	}
	return strings.Join(parts, "\n\n")
}

// systemText is the text of msgs' role-system messages, one after another:
// what the history has told the model of its environment.
func systemText(msgs []json.RawMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if role(m) != "system" {
			continue
		}
		c := field(m, "content")
		var s string
		if json.Unmarshal(c, &s) == nil {
			b.WriteString(s + "\n")
			continue
		}
		blocks, _ := body.Elements(c)
		for _, bl := range blocks {
			if body.Label(bl, "type") == "text" && json.Unmarshal(field(bl, "text"), &s) == nil {
				b.WriteString(s + "\n")
			}
		}
	}
	return b.String()
}

// maxBreakpoints is the API's limit on cache_control blocks ("A maximum of 4
// blocks with cache_control may be provided", seen 2026-11-05).
const maxBreakpoints = 4

// splice returns the request with the history in front, the header edit
// that goes with it, and how many history messages went in. pass means the
// request is not the turn's main thread and goes on unchanged.
func (r *Replayer) splice(session string, ti turnInfo, data []byte) (out []byte, hdr func(http.Header), n int, pass bool, evs []event.Event, err error) {
	req, ok := r.f.parse(data)
	if !ok {
		return nil, nil, 0, true, nil, nil
	}
	fresh := r.f.fresh(req, ti.prompt)
	// The one rule (isMain). The main thread's identity is fixed by the
	// turn's first request in the fresh layout, which always comes first, and
	// is never changed after.
	main, known, err := r.mainIdentity(session, ti, req.id, fresh)
	if err != nil {
		return nil, nil, 0, false, nil, err
	}
	carried := r.spliced(session, ti.turn)
	switch {
	case known && !isMain(req.id, main, known):
		// Another system prompt (codex: another thread): a subagent or a
		// side call, never main, whatever its messages carry.
		return nil, nil, 0, true, nil, nil
	case known && !fresh && carried != nil:
		// The main thread without the prompt where a fresh process puts it,
		// after its requests carried the history: the harness compacted its
		// context inside this one turn, and its summary covers the history.
		// The request goes on as the harness sent it, recorded; nothing
		// depends on it, since the next turn is built from the record
		// (compactedHistory), not from the harness's summary.
		out, hdr := r.f.passOn(req, carried)
		return out, hdr, 0, out == nil, compactedEvent(), nil
	case known && !fresh:
		// No request of the turn carried the history yet (its first was
		// refused, or the service restarted inside the turn): sent on, it
		// would give the model a turn with no past. Refused.
		return nil, nil, 0, false, nil, errors.New("a request of the turn's main thread " + r.f.freshRule() + ", as after a compaction or a layout change, before any of its requests carried the history, so the replayed history cannot be placed")
	case !known:
		// Before the main thread's first request. One with no tools and no
		// prompt is a side call (Claude Code's quota check); anything else
		// is a layout the gateway does not know.
		if req.tools == 0 && !r.f.hasPrompt(req.msgs, ti.prompt) {
			return nil, nil, 0, true, nil, nil
		}
		return nil, nil, 0, false, nil, errors.New("a request came before the turn's main thread's first one, in a layout the gateway does not know, so the replayed history cannot be placed")
	}
	h, evs, err := r.fitted(session, ti, req)
	if err != nil {
		var miss *missing
		if errors.As(err, &miss) {
			return nil, nil, 0, false, nil, fmt.Errorf("the history of turn %s cannot be read: %s", ti.from, miss.why)
		}
		return nil, nil, 0, false, nil, errors.New("the store could not be read")
	}
	// A request to another model than the history's, as after a switch of
	// agent on the same harness: the history is laid out for it.
	if m := requestModel(req.fields); h.model != "" && m != "" && m != h.model {
		msgs, err := r.f.otherModel(h.messages)
		if err != nil {
			return nil, nil, 0, false, nil, errors.New("the history of turn " + ti.from + " cannot be laid out for another model: " + err.Error())
		}
		other := *h
		other.messages = msgs
		h = &other
	}
	out, hdr, err = r.f.build(req, h, ti.prompt)
	if err != nil {
		return nil, nil, 0, false, nil, err
	}
	r.mark(r.carried, session, ti.turn)
	return out, hdr, len(h.messages), false, evs, nil
}

// fitted is the history a replayed turn's requests carry: the earlier turn's,
// within the budget (fit). It is fitted once, at the turn's first main
// request, for model, and every later request of the turn carries the same
// bytes, so the cache holds inside the turn; the memo drops only closed
// turns. evs, on that first request only, record the step it took, if any.
// The harness's standing context the history lacks is put in before the fit
// (withStanding), so the budget counts it.
func (r *Replayer) fitted(session string, ti turnInfo, req request) (*history, []event.Event, error) {
	model := requestModel(req.fields)
	k := [2]string{session, ti.turn}
	r.mu.Lock()
	h, ok := r.fits[k]
	r.mu.Unlock()
	if ok {
		return h, nil, nil
	}
	full, err := r.history(session, ti.from, true)
	if err != nil {
		return nil, nil, err
	}
	full = r.f.withStanding(full, req)
	h, step := r.fit(session, ti.from, full, model)
	r.mu.Lock()
	if len(r.fits) >= memoMax {
		// Only closed turns go: a running turn's requests must keep the
		// bytes its first one carried.
		for c := range r.fits {
			if closed, err := r.src.TurnClosed(c[0], c[1]); err == nil && closed {
				delete(r.fits, c)
			}
		}
	}
	if got, ok := r.fits[k]; ok {
		h, step = got, nil // another request of the turn fitted it first
	} else {
		r.fits[k] = h
	}
	r.mu.Unlock()
	if step == nil {
		return h, nil, nil
	}
	return h, []event.Event{{Kind: event.HistoryShortened, History: step}}, nil
}

// spliced is the history a request of the turn already went on with, in
// this process, or nil when none has.
func (r *Replayer) spliced(session, turn string) *history {
	k := [2]string{session, turn}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.carried[k] {
		return nil
	}
	return r.fits[k]
}

// mark sets (session, turn) in memo m, and reports whether it was new.
func (r *Replayer) mark(m map[[2]string]bool, session, turn string) bool {
	k := [2]string{session, turn}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m[k] {
		return false
	}
	if len(m) >= memoMax {
		// Only closed turns go, as evictClosed does for identities.
		for c := range m {
			if closed, err := r.src.TurnClosed(c[0], c[1]); err == nil && closed {
				delete(m, c)
			}
		}
	}
	m[k] = true
	return true
}

// compactedEvent is the gateway's verdict on one request, recorded on its
// call: its harness had compacted it, and it went on as the harness sent it.
// The history reads it through compacted: in a turn that carries a history
// this verdict is the only record of a compaction; in a turn with no earlier
// history the same test is applied to the stored request.
func compactedEvent() []event.Event {
	return []event.Event{{Kind: event.HistoryCompacted,
		Text: "The agent's harness compacted its context in this request. AgentBus let it through as the harness sent it; the next turn is built from AgentBus's record, not from the harness's summary."}}
}

// requestModel is a request's model field, or "".
func requestModel(fields []body.Field) string {
	for _, f := range fields {
		if f.Key == "model" {
			var s string
			if json.Unmarshal(f.Value, &s) == nil {
				return s
			}
		}
	}
	return ""
}

// claude is Claude Code's format: Anthropic Messages.
type claude struct{}

// passOn: the request goes on as sent; its identity is its system prompt.
func (claude) passOn(request, *history) ([]byte, func(http.Header)) { return nil, nil }

func (claude) read(c store.CallContext) ([]json.RawMessage, string, int) {
	var msgs, sys []json.RawMessage
	tools := 0
	for _, p := range c.Pieces {
		switch p.Kind {
		case body.Message:
			msgs = append(msgs, p.JSON)
		case body.System:
			sys = append(sys, p.JSON)
		case body.Tool:
			tools++
		}
	}
	return msgs, identityOf(sys), tools
}

func (claude) recoverable() bool { return true }

func (claude) mainLayout(msgs []json.RawMessage, prompt string) bool { return mainLayout(msgs, prompt) }

func (claude) hasPrompt(msgs []json.RawMessage, prompt string) bool { return hasPrompt(msgs, prompt) }

// replyKept: a finished reply (a stop reason other than pause_turn or none)
// or one that asked for a tool.
func (claude) replyKept(stop string) bool { return stop != "pause_turn" && stop != "" }

func (claude) carry(h *history, c store.CallContext) {
	h.prior = priorTurnContext(c.Request)
	for _, p := range c.Pieces {
		if p.Kind == body.Tool {
			h.tools = append(h.tools, p.JSON)
		}
	}
}

func (claude) asHistory(m json.RawMessage) (json.RawMessage, error) { return asHistory(m) }

// reply: the assistant message. One that stopped for tool_use asked for
// tools; its results are a user message with a tool_result marked as an
// error for each tool_use.
func (claude) reply(rep body.Reply, stop, unanswered string) ([]json.RawMessage, ending, error) {
	m, err := asHistory(rep.Message)
	if err != nil {
		return nil, ending{}, err
	}
	if stop != "tool_use" {
		return []json.RawMessage{m}, ending{finished: true}, nil
	}
	ids, err := toolUseIDs(m)
	if err != nil {
		return nil, ending{}, err
	}
	e := ending{asked: true}
	if len(ids) > 0 {
		blocks := make([]json.RawMessage, len(ids))
		for i, id := range ids {
			blocks[i] = body.Object([]body.Field{{Key: "tool_use_id", Value: jsonString(id)},
				{Key: "type", Value: json.RawMessage(`"tool_result"`)},
				{Key: "content", Value: jsonString(unanswered)},
				{Key: "is_error", Value: json.RawMessage(`true`)}})
		}
		e.results = []json.RawMessage{body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"user"`)},
			{Key: "content", Value: joinArray(blocks)}})}
	}
	return []json.RawMessage{m}, e, nil
}

// canEnd: the note is an assistant message, so the history must not end
// with one.
func (claude) canEnd(msgs []json.RawMessage, _ bool) bool {
	return role(msgs[len(msgs)-1]) != "assistant"
}

func (claude) prompt(text string) json.RawMessage {
	return body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"user"`)}, {Key: "content", Value: jsonString(text)}})
}

func (claude) note(text string) json.RawMessage { return noteMessage(text) }

func (claude) refusal(msg string) *gateway.Refusal {
	b, _ := json.Marshal(map[string]any{"type": "error",
		"error": map[string]string{"type": "invalid_request_error", "message": msg}})
	return &gateway.Refusal{Status: 400, ContentType: "application/json", Body: b}
}

func (claude) parse(data []byte) (request, bool) {
	fields, err := body.Fields(data)
	if err != nil {
		return request{}, false
	}
	var msgsRaw json.RawMessage
	tools := 0
	for _, f := range fields {
		switch f.Key {
		case "messages":
			msgsRaw = f.Value
		case "tools":
			els, _ := body.Elements(f.Value)
			tools = len(els)
		}
	}
	msgs, ok := body.Elements(msgsRaw)
	if !ok {
		return request{}, false
	}
	return request{fields: fields, msgs: msgs, tools: tools, id: systemIdentity(fields)}, true
}

func (claude) fresh(req request, prompt string) bool {
	_, ok := freshLayout(req.msgs, prompt)
	return ok
}

func (claude) freshRule() string {
	return "(its system prompt) does not carry the turn's prompt as the last text block of its first message followed by a system message"
}

// build puts the history in front of a fresh Claude Code request.
func (claude) build(req request, h *history, prompt string) ([]byte, func(http.Header), error) {
	fields, msgs := req.fields, req.msgs
	var safeguards json.RawMessage
	for _, f := range fields {
		if f.Key == "safeguards" {
			safeguards = f.Value
		}
	}
	promptText, _ := freshLayout(msgs, prompt)
	var err error

	// The fresh process's first system message, cut to its hook texts and
	// what its environment adds to the history's own copy (cutTurnSystem). A
	// history with no environment has no such copy (no earlier turn reached
	// the model, as when a session's first turn failed before its first
	// call, or the turn that had it went into AgentBus's summary), so the
	// message is kept whole, as a fresh process sends it.
	m1 := msgs[1]
	if prior := systemText(h.messages); strings.Contains(prior, envMark[2:]) {
		if m1, err = cutMessage(msgs[1], prior); err != nil {
			return nil, nil, err
		}
	}

	hist := make([]json.RawMessage, len(h.messages))
	copy(hist, h.messages)
	// The request's tools, with each tool the history loaded and this
	// process has put back (carryLoaded). A history reference to a tool still
	// not among them becomes text (unreferenced): the API refuses the whole
	// request over one, as it did for a read-only turn after a turn that
	// loaded the agentbus tools (six 400s, 2026-11-11).
	var tools json.RawMessage
	for i, f := range fields {
		if f.Key == "tools" {
			fields[i].Value = carryLoaded(f.Value, h, systemText(msgs[1:2]))
			tools = fields[i].Value
		}
	}
	if hist, err = unreferenced(hist, searchable(tools)); err != nil {
		return nil, nil, err
	}
	// Claude Code's standing context (CLAUDE.md, memory, AGENTS.md: context)
	// rides in the history's first message (withStanding put it there before
	// the fit when it was missing), so the fresh process's own copy in
	// msgs[0] does not go in again.
	// A history the budget made AgentBus's summary alone ends with that
	// summary, a user message after the context: the context and the
	// summary's block go first in the new prompt's message, where a later
	// step finds them (context, summaryIn), and the request alternates as
	// Claude Code's does.
	content := promptText
	if n := len(hist); n > 0 {
		if text, rest, ok := (claude{}).summaryIn(hist[n-1]); ok && rest == nil {
			ctx, _ := claude{}.context(hist[n-1])
			hist = hist[:n-1]
			content = joinArray(append(append([]json.RawMessage{}, ctx...),
				body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)}, {Key: "text", Value: jsonString(text)}}),
				body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)}, {Key: "text", Value: promptText}})))
		}
	}
	// On the turn's first request only (the person's message and Claude
	// Code's system message, nothing else yet), a breakpoint on the history's
	// last message, the earlier turn's reply. That is where a resumed
	// process puts one, so the whole history is read from cache from then on.
	// Its cache_control is copied from Claude Code's own (breakpointValue).
	addBP := len(msgs) == 2 && len(hist) > 0
	var newMsgs []json.RawMessage
	newMsgs = append(newMsgs, hist...)
	newMsgs = append(newMsgs, body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"user"`)},
		{Key: "content", Value: content}}))
	if m1 != nil {
		newMsgs = append(newMsgs, m1)
	}
	newMsgs = append(newMsgs, msgs[2:]...)
	if addBP {
		last := len(hist) - 1
		withBP, err := breakpointOnLast(hist[last], breakpointValue(fields, newMsgs, last))
		if err != nil {
			return nil, nil, err
		}
		trial := append([]json.RawMessage{}, newMsgs...)
		trial[last] = withBP
		if countBreakpoints(fields, trial) <= maxBreakpoints {
			newMsgs = trial
		}
	}

	var b strings.Builder
	b.WriteByte('[')
	for i, m := range newMsgs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(m)
	}
	b.WriteByte(']')
	for i, f := range fields {
		switch f.Key {
		case "messages":
			fields[i].Value = json.RawMessage(b.String())
		case "safeguards":
			if s, ok := carryPrior(safeguards, h.prior); ok {
				fields[i].Value = s
			}
		}
	}
	return body.Object(fields), nil, nil
}

// carryLoaded is tools, a fresh request's tool definitions, with each tool the
// history's ToolSearch loaded put back: a tool_reference block names it, and
// the API refuses a request whose tools lack it ("Tool reference
// 'mcp__agentbus__…' not found in available tools", 400, his runner,
// 2026-11-11). A resumed process keeps what it loaded in its tools, with
// defer_loading; a fresh one has only its own. So each such tool goes in as
// the earlier turn's last request had it, when the request has ToolSearch and
// env, the fresh process's first system message, still lists it as
// deferred: a tool the new process does not have is never added, and its
// reference becomes text (unreferenced).
func carryLoaded(tools json.RawMessage, h *history, env string) json.RawMessage {
	els, ok := body.Elements(tools)
	if !ok {
		return tools
	}
	have := map[string]bool{}
	for _, t := range els {
		var n string
		_ = json.Unmarshal(field(t, "name"), &n)
		have[n] = true
	}
	if !have["ToolSearch"] {
		// No tool search, so nothing is loaded and nothing deferred may be
		// sent (searchable).
		return tools
	}
	deferred := map[string]bool{}
	for _, l := range strings.Split(env, "\n") {
		deferred[l] = true
	}
	refs := toolRefs(h.messages)
	added := false
	for _, t := range h.tools {
		var n string
		_ = json.Unmarshal(field(t, "name"), &n)
		if refs[n] && !have[n] && deferred[n] {
			els = append(els, t)
			have[n] = true
			added = true
		}
	}
	if !added {
		return tools
	}
	return joinArray(els)
}

// searchable names the tools a tool_reference may load in a request with
// tools: every tool there, when ToolSearch is one of them; none without it,
// since a reference is ToolSearch's answer. Claude Code drops its own
// references when tool search is off ("Tool references removed - tool search
// not enabled", 2.1.321, 2026-11-11).
func searchable(tools json.RawMessage) map[string]bool {
	els, _ := body.Elements(tools)
	out := map[string]bool{}
	for _, t := range els {
		var n string
		_ = json.Unmarshal(field(t, "name"), &n)
		out[n] = true
	}
	if !out["ToolSearch"] {
		return map[string]bool{}
	}
	return out
}

// unreferenced is msgs with each tool_reference block inside a tool_result
// that names a tool outside offered made a text block saying the tool is not
// in this turn. A message with none is kept byte for byte.
func unreferenced(msgs []json.RawMessage, offered map[string]bool) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if !bytes.Contains(m, []byte(`"tool_reference"`)) {
			continue
		}
		fields, err := body.Fields(m)
		if err != nil {
			return nil, err
		}
		changed := false
		for j, f := range fields {
			if f.Key != "content" {
				continue
			}
			blocks, ok := body.Elements(f.Value)
			if !ok {
				continue
			}
			for k, b := range blocks {
				if body.Label(b, "type") != "tool_result" {
					continue
				}
				inner, ok := body.Elements(field(b, "content"))
				if !ok {
					continue
				}
				hit := false
				for x, ib := range inner {
					var n string
					if body.Label(ib, "type") != "tool_reference" || json.Unmarshal(field(ib, "tool_name"), &n) != nil || offered[n] {
						continue
					}
					inner[x] = body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)},
						{Key: "text", Value: jsonString("Tool " + n + " was loaded in an earlier turn. This turn does not have it.")}})
					hit = true
				}
				if !hit {
					continue
				}
				bf, err := body.Fields(b)
				if err != nil {
					return nil, err
				}
				for y, g := range bf {
					if g.Key == "content" {
						bf[y].Value = joinArray(inner)
					}
				}
				blocks[k] = body.Object(bf)
				changed = true
			}
			fields[j].Value = joinArray(blocks)
		}
		if changed {
			out[i] = body.Object(fields)
		}
	}
	return out, nil
}

// toolRefs names the tools msgs' tool_reference blocks load, at the top of a
// message's content or inside a tool_result's.
func toolRefs(msgs []json.RawMessage) map[string]bool {
	out := map[string]bool{}
	var walk func(blocks []json.RawMessage)
	walk = func(blocks []json.RawMessage) {
		for _, b := range blocks {
			switch body.Label(b, "type") {
			case "tool_reference":
				var n string
				if json.Unmarshal(field(b, "tool_name"), &n) == nil {
					out[n] = true
				}
			case "tool_result":
				if inner, ok := body.Elements(field(b, "content")); ok {
					walk(inner)
				}
			}
		}
	}
	for _, m := range msgs {
		if blocks, ok := body.Elements(field(m, "content")); ok {
			walk(blocks)
		}
	}
	return out
}

// otherModel: thinking blocks go, since their signatures belong to the model
// that made them. So does what a system message sets for the earlier
// process: its tool_addition and tool_removal blocks, which announce that
// process's deferred tools, and every field but role and content, such as
// output_config with its reasoning effort. A process on another model may
// set neither. Claude Code on a subscription sets both, and OpenRouter
// refused each on deepseek/deepseek-v4.1-flash: "Mid-conversation tool
// changes (tool_addition/tool_removal system blocks) are not supported" and
// "Mid-conversation reasoning effort (configuration_update) is not
// supported" (2.1.319, 2026-11-09). The new process's own first system
// message sets its own again. A message left with nothing goes; a system
// message left with text only becomes a plain string, as asHistory lays it
// out.
func (claude) otherModel(msgs []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		var drop map[string]bool
		switch role(m) {
		case "assistant":
			drop = map[string]bool{"thinking": true, "redacted_thinking": true}
		case "system":
			drop = map[string]bool{"tool_addition": true, "tool_removal": true}
		default:
			out = append(out, m)
			continue
		}
		all, err := body.Fields(m)
		if err != nil {
			return nil, err
		}
		fields := all[:0:0]
		for _, f := range all {
			if role(m) != "system" || f.Key == "role" || f.Key == "content" {
				fields = append(fields, f)
			}
		}
		keep := true
		for i, f := range fields {
			if f.Key != "content" {
				continue
			}
			blocks, ok := body.Elements(f.Value)
			if !ok {
				break // a string: nothing to drop
			}
			var left []json.RawMessage
			for _, b := range blocks {
				if !drop[body.Label(b, "type")] {
					left = append(left, b)
				}
			}
			switch {
			case len(left) == len(blocks):
			case len(left) == 0:
				keep = false
			case role(m) == "system" && allText(left):
				var sb strings.Builder
				for _, b := range left {
					var s string
					if json.Unmarshal(field(b, "text"), &s) == nil {
						sb.WriteString(s)
					}
				}
				fields[i].Value = jsonString(sb.String())
			default:
				fields[i].Value = joinArray(left)
			}
		}
		if keep {
			out = append(out, body.Object(fields))
		}
	}
	return out, nil
}

// systemIdentity names a request's system prompt: its blocks after the first,
// without cache_control. The first block is Claude Code's billing header,
// whose suffix differed between processes in the research (2026-11-06), so it
// is left out. A subagent's request has a system prompt of its own.
func systemIdentity(top []body.Field) string {
	for _, f := range top {
		if f.Key != "system" {
			continue
		}
		if els, ok := body.Elements(f.Value); ok {
			return identityOf(els)
		}
		return identityOf([]json.RawMessage{f.Value})
	}
	return ""
}

// identityOf names a system prompt from its blocks, as the request carried
// them (the store keeps them as pieces, one per block, or the string as one).
// A system string is named whole; a list by its blocks after the first.
func identityOf(blocks []json.RawMessage) string {
	if len(blocks) == 1 && body.IsString(blocks[0]) {
		return string(blocks[0])
	}
	var b strings.Builder
	for i, e := range blocks {
		if i == 0 {
			continue
		}
		if s, err := stripBlock(e); err == nil {
			b.Write(s)
		} else {
			b.Write(e)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// freshLayout checks a fresh Claude Code request's layout: message 0 is the
// person's, a list whose last block is the text of the turn's prompt, and
// message 1 is a system message. It returns that text block's text value.
func freshLayout(msgs []json.RawMessage, prompt string) (json.RawMessage, bool) {
	if len(msgs) < 2 || role(msgs[0]) != "user" || role(msgs[1]) != "system" {
		return nil, false
	}
	blocks, ok := body.Elements(field(msgs[0], "content"))
	if !ok || len(blocks) == 0 {
		return nil, false
	}
	last := blocks[len(blocks)-1]
	if body.Label(last, "type") != "text" {
		return nil, false
	}
	text := field(last, "text")
	var s string
	if json.Unmarshal(text, &s) != nil || !samePrompt(s, prompt) {
		return nil, false
	}
	return text, true
}

// mainLayout reports whether a stored request has the main thread's layout: a
// user message carries the turn's prompt and a role-system message is present.
// It fixes the main thread's identity (the oldest such request) and checks
// that the newest main call was not compacted; it never decides on its own
// whether a call is the main thread's (isMain does).
func mainLayout(msgs []json.RawMessage, prompt string) bool {
	if !hasPrompt(msgs, prompt) {
		return false
	}
	for _, m := range msgs {
		if role(m) == "system" {
			return true
		}
	}
	return false
}

func samePrompt(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// hasPrompt reports whether a user message carries the turn's prompt: its
// content as a string, or one of its text blocks.
func hasPrompt(msgs []json.RawMessage, prompt string) bool {
	if strings.TrimSpace(prompt) == "" {
		return false
	}
	for _, m := range msgs {
		if role(m) != "user" {
			continue
		}
		c := field(m, "content")
		var s string
		if json.Unmarshal(c, &s) == nil {
			if samePrompt(s, prompt) {
				return true
			}
			continue
		}
		blocks, _ := body.Elements(c)
		for _, b := range blocks {
			if body.Label(b, "type") == "text" && json.Unmarshal(field(b, "text"), &s) == nil && samePrompt(s, prompt) {
				return true
			}
		}
	}
	return false
}

// cutMessage cuts a fresh first system message to its hook texts and what its
// environment adds to prior (cutTurnSystem). Claude Code
// sends it as a list of one text block while it is the request's last message
// (the turn's first request) and as a plain string after that (2.1.319,
// 2026-11-06; the string case is what broke part 3's first tap). It returns
// nil when nothing is left.
func cutMessage(m json.RawMessage, prior string) (json.RawMessage, error) {
	fields, err := body.Fields(m)
	if err != nil {
		return nil, errors.New("the first system message is not an object")
	}
	for i, f := range fields {
		if f.Key != "content" {
			continue
		}
		var s string
		if json.Unmarshal(f.Value, &s) == nil {
			cut, err := cutTurnSystem(s, prior)
			if err != nil {
				return nil, err
			}
			if cut == "" {
				return nil, nil
			}
			fields[i].Value = jsonString(cut)
			return body.Object(fields), nil
		}
		blocks, ok := body.Elements(f.Value)
		if !ok || len(blocks) == 0 || body.Label(blocks[0], "type") != "text" {
			return nil, errors.New("the first system message does not start with a text block")
		}
		bf, err := body.Fields(blocks[0])
		if err != nil {
			return nil, errors.New("the first system message's block is not an object")
		}
		var text string
		if json.Unmarshal(field(blocks[0], "text"), &text) != nil {
			return nil, errors.New("the first system message's text is not a string")
		}
		cut, err := cutTurnSystem(text, prior)
		if err != nil {
			return nil, err
		}
		if cut == "" {
			// No hook text: the block goes, and the message with it when it
			// held nothing else. An empty text block is refused by the API.
			blocks = blocks[1:]
			if len(blocks) == 0 {
				return nil, nil
			}
		} else {
			for j, g := range bf {
				if g.Key == "text" {
					bf[j].Value = jsonString(cut)
				}
			}
			blocks[0] = body.Object(bf)
		}
		fields[i].Value = joinArray(blocks)
		return body.Object(fields), nil
	}
	return nil, errors.New("the first system message has no content")
}

// asHistory lays out one message of an earlier turn as Claude Code lays out an
// earlier message: cache_control removed from every content block and from
// the blocks inside a tool_result, never from inside a tool's input, which is
// the application's data; and a role-system message's list of text blocks
// joined into one plain string (Claude Code sends a system message of text
// only as a list only while it is the request's last message, 2.1.319). A
// system message with other blocks, such as MCP tool_addition blocks, stays a
// list: Claude Code sent those as lists mid-conversation (2.1.318, part 2 of
// the per-turn research). A string content stays.
func asHistory(m json.RawMessage) (json.RawMessage, error) {
	fields, err := body.Fields(m)
	if err != nil {
		return nil, err
	}
	sys := role(m) == "system"
	for i, f := range fields {
		if f.Key != "content" {
			continue
		}
		blocks, ok := body.Elements(f.Value)
		if !ok {
			continue
		}
		if sys && allText(blocks) {
			var sb strings.Builder
			for _, b := range blocks {
				var s string
				if body.Label(b, "type") == "text" && json.Unmarshal(field(b, "text"), &s) == nil {
					sb.WriteString(s)
				}
			}
			fields[i].Value = jsonString(sb.String())
			continue
		}
		for j, b := range blocks {
			blocks[j], err = stripBlock(b)
			if err != nil {
				return nil, err
			}
		}
		fields[i].Value = joinArray(blocks)
	}
	return body.Object(fields), nil
}

func allText(blocks []json.RawMessage) bool {
	for _, b := range blocks {
		if body.Label(b, "type") != "text" {
			return false
		}
	}
	return true
}

// stripBlock removes cache_control from a content block, and from the blocks
// of a tool_result's content.
func stripBlock(b json.RawMessage) (json.RawMessage, error) {
	bf, err := body.Fields(b)
	if err != nil {
		return nil, err
	}
	toolResult := body.Label(b, "type") == "tool_result"
	out := bf[:0]
	for _, f := range bf {
		if f.Key == "cache_control" {
			continue
		}
		if toolResult && f.Key == "content" {
			if inner, ok := body.Elements(f.Value); ok {
				for k, ib := range inner {
					if inner[k], err = stripBlock(ib); err != nil {
						return nil, err
					}
				}
				f.Value = joinArray(inner)
			}
		}
		out = append(out, f)
	}
	return body.Object(out), nil
}

// breakpointOnLast puts bp on the last content block of a message.
func breakpointOnLast(m json.RawMessage, bp json.RawMessage) (json.RawMessage, error) {
	fields, err := body.Fields(m)
	if err != nil {
		return nil, err
	}
	for i, f := range fields {
		if f.Key != "content" {
			continue
		}
		blocks, ok := body.Elements(f.Value)
		if !ok || len(blocks) == 0 {
			return m, nil
		}
		bf, err := body.Fields(blocks[len(blocks)-1])
		if err != nil {
			return nil, err
		}
		bf = append(bf, body.Field{Key: "cache_control", Value: bp})
		blocks[len(blocks)-1] = body.Object(bf)
		fields[i].Value = joinArray(blocks)
	}
	return body.Object(fields), nil
}

// breakpointValue picks the cache_control for the breakpoint on message at.
// The API wants lifetimes in order: a ttl='1h' block must not come after a
// ttl='5m' one, counting tools, then system, then messages (a 400 said so on
// 2026-11-06, when Claude Code 2.1.319 put its 1h breakpoint on the last of
// four blocks of its system message, after the MCP tool_addition blocks, and
// the gateway had copied a 5m one from the first). So it copies the next
// breakpoint after at, which is never longer than one before it; without
// one, the last breakpoint before at; without any, a plain ephemeral one.
func breakpointValue(top []body.Field, msgs []json.RawMessage, at int) json.RawMessage {
	for _, m := range msgs[at+1:] {
		blocks, _ := body.Elements(field(m, "content"))
		for _, b := range blocks {
			if v := field(b, "cache_control"); v != nil {
				return v
			}
		}
	}
	var prev json.RawMessage
	for _, k := range []string{"tools", "system"} {
		for _, f := range top {
			if f.Key != k {
				continue
			}
			els, _ := body.Elements(f.Value)
			for _, e := range els {
				if v := field(e, "cache_control"); v != nil {
					prev = v
				}
			}
		}
	}
	for _, m := range msgs[:at+1] {
		blocks, _ := body.Elements(field(m, "content"))
		for _, b := range blocks {
			if v := field(b, "cache_control"); v != nil {
				prev = v
			}
		}
	}
	if prev != nil {
		return prev
	}
	return json.RawMessage(`{"type":"ephemeral"}`)
}

// countBreakpoints counts cache_control blocks in system, tools and messages.
func countBreakpoints(top []body.Field, msgs []json.RawMessage) int {
	n := 0
	has := func(b json.RawMessage) bool { return field(b, "cache_control") != nil }
	for _, f := range top {
		if f.Key == "system" || f.Key == "tools" {
			els, _ := body.Elements(f.Value)
			for _, e := range els {
				if has(e) {
					n++
				}
			}
		}
	}
	for _, m := range msgs {
		blocks, _ := body.Elements(field(m, "content"))
		for _, b := range blocks {
			if has(b) {
				n++
			}
		}
	}
	return n
}

// priorTurnContext reads safeguards[0].classifier_context.prior_turn_context
// from a request's other top-level fields.
func priorTurnContext(top json.RawMessage) []json.RawMessage {
	sg, ok := body.Elements(field(top, "safeguards"))
	if !ok || len(sg) == 0 {
		return nil
	}
	els, _ := body.Elements(field(field(sg[0], "classifier_context"), "prior_turn_context"))
	return els
}

// carryPrior puts the earlier turn's prior_turn_context in front of this
// request's. safeguards[0].classifier_context.prior_turn_context lists every
// earlier Bash tool use with the context of the request that made it: a
// resumed process sends the whole list, a fresh one only this turn's (found
// 2026-11-06). The key goes where Claude Code puts it, before
// classifier_facts, when the request has none yet. ok is false when there is
// nothing to carry or the shape is not the known one; the request then goes
// on with its own list, which is outside the messages and the cache.
func carryPrior(safeguards json.RawMessage, prior []json.RawMessage) (json.RawMessage, bool) {
	if len(prior) == 0 {
		return nil, false
	}
	sg, ok := body.Elements(safeguards)
	if !ok || len(sg) == 0 {
		return nil, false
	}
	s0, err := body.Fields(sg[0])
	if err != nil {
		return nil, false
	}
	for i, f := range s0 {
		if f.Key != "classifier_context" {
			continue
		}
		cc, err := body.Fields(f.Value)
		if err != nil {
			return nil, false
		}
		var out []body.Field
		placed := false
		for _, g := range cc {
			switch {
			case g.Key == "prior_turn_context":
				mine, _ := body.Elements(g.Value)
				out = append(out, body.Field{Key: g.Key, Value: joinArray(append(append([]json.RawMessage{}, prior...), mine...))})
				placed = true
			case g.Key == "classifier_facts" && !placed && !hasKey(cc, "prior_turn_context"):
				out = append(out, body.Field{Key: "prior_turn_context", Value: joinArray(prior)}, g)
				placed = true
			default:
				out = append(out, g)
			}
		}
		if !placed {
			out = append(out, body.Field{Key: "prior_turn_context", Value: joinArray(prior)})
		}
		s0[i].Value = body.Object(out)
		sg[0] = body.Object(s0)
		return joinArray(sg), true
	}
	return nil, false
}

func hasKey(fs []body.Field, k string) bool {
	for _, f := range fs {
		if f.Key == k {
			return true
		}
	}
	return false
}

// field reads one top-level field of a JSON object; nil when absent.
func field(raw json.RawMessage, key string) json.RawMessage {
	fs, err := body.Fields(raw)
	if err != nil {
		return nil
	}
	for _, f := range fs {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func role(m json.RawMessage) string { return body.Label(m, "role") }

func joinArray(els []json.RawMessage) json.RawMessage {
	var b strings.Builder
	b.WriteByte('[')
	for i, e := range els {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(e)
	}
	b.WriteByte(']')
	return json.RawMessage(b.String())
}

// jsonString encodes s without escaping <, > and &, as Claude Code's own
// JSON does.
func jsonString(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
