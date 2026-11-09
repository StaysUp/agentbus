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
// Two formats, one Replayer each: the Anthropic Messages format as Claude Code
// sends it (New), and the OpenAI Responses format as codex sends it
// (NewCodex, codex.go). The rules are the same for both and live here: calls
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
}

// New returns a Replayer for Claude Code's requests, reading src.
func New(src Source) *Replayer { return newReplayer(src, claude{}) }

// NewCodex returns a Replayer for codex's requests, reading src.
func NewCodex(src Source) *Replayer { return newReplayer(src, codex{}) }

func newReplayer(src Source, f format) *Replayer {
	return &Replayer{src: src, f: f, turns: map[[2]string]turnInfo{}, hists: map[[2]string]*history{},
		mains: map[[2]string]string{}}
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
	reply(rep body.Reply, stop string) ([]json.RawMessage, ending, error)
	// canEnd reports whether a history of a turn that did not finish may be
	// followed by the note. kept says it ends with the turn's kept reply.
	canEnd(msgs []json.RawMessage, kept bool) bool
	// prompt is the person's message; note is AgentBus's note.
	prompt(text string) json.RawMessage
	note(text string) json.RawMessage
	// refusal answers the agent in its vendor's error format.
	refusal(msg string) *gateway.Refusal
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
// session.closed is stored only after the gateway has written all of the
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
// The newest main call's request must still carry the turn's prompt (a
// compacted request does not, and its history would be the summary's, not the
// turn's). Then:
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
	type call struct {
		a     store.Call
		c     store.CallContext
		msgs  []json.RawMessage
		id    string
		tools int
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
	var newest *call
	for i := range calls {
		if isMain(calls[i].id, main, true) {
			newest = &calls[i]
			break
		}
	}
	x := newest.a.Exchange
	if !r.f.mainLayout(newest.msgs, ti.prompt) {
		return nil, &missing{why: "turn " + turn + "'s last main-thread request " + x +
			" no longer carries the turn's prompt, as after a compaction, and a compacted request cannot be replayed", kind: unusableCall}
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
	h = &history{model: body.Label(newest.c.Request, "model")}
	r.f.carry(h, newest.c)
	for _, m := range newest.msgs {
		hm, err := r.f.asHistory(m)
		if err != nil {
			return nil, &missing{why: "turn " + turn + "'s stored request cannot be replayed: " + err.Error(), kind: unusableCall}
		}
		h.messages = append(h.messages, hm)
	}
	var e ending
	if kept {
		items, how, err := r.f.reply(rep, a.Stop)
		if err != nil {
			return nil, &missing{why: "turn " + turn + "'s stored reply cannot be replayed: " + err.Error(), kind: unusableCall}
		}
		h.messages = append(h.messages, items...)
		e = how
	}
	// A finished reply ends the history whole, even for a turn the person
	// stopped after that reply came and before its agent exited: the model
	// had done the turn's work, and Claude Code's layout has no place for a
	// note after a final reply (the note is an assistant message). The stop
	// is still on the turn's session.closed and in the turn list.
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
		h.prior, h.thread, h.model = ph.prior, ph.thread, ph.model
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

// stoppedWords says the person stopped a turn (session.closed's stopped).
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

// evictClosed drops the identities of turns whose child has exited. An open
// turn's identity is never dropped: the memo then grows past memoMax, bounded
// by the turns running at once. Called with r.mu held.
func (r *Replayer) evictClosed() {
	for k := range r.mains {
		if closed, err := r.src.TurnClosed(k[0], k[1]); err == nil && closed {
			delete(r.mains, k)
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
func (r *Replayer) Rewrite(session, turn string, data []byte) gateway.Rewrite {
	ti, err := r.turn(session, turn)
	if err != nil {
		return r.refuse("the store could not be read")
	}
	if !ti.found || ti.from == "" {
		return gateway.Rewrite{}
	}
	out, hdr, n, pass, err := r.splice(session, ti, data)
	if err != nil {
		return r.refuse(err.Error())
	}
	if pass {
		return gateway.Rewrite{}
	}
	return gateway.Rewrite{Body: out, Replayed: n, Header: hdr}
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

// cutTurnSystem keeps a fresh first system message's hook texts: everything
// before the environment, and the UserPromptSubmit hook text without the date
// after it. Without a UserPromptSubmit hook, everything from the environment
// on goes. It fails when the environment marker is missing.
func cutTurnSystem(t string) (string, error) {
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
	for _, p := range []string{head, ups} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// maxBreakpoints is the API's limit on cache_control blocks ("A maximum of 4
// blocks with cache_control may be provided", seen 2026-11-05).
const maxBreakpoints = 4

// splice returns the request with the history in front, the header edit
// that goes with it, and how many history messages went in. pass means the
// request is not the turn's main thread and goes on unchanged.
func (r *Replayer) splice(session string, ti turnInfo, data []byte) (out []byte, hdr func(http.Header), n int, pass bool, err error) {
	req, ok := r.f.parse(data)
	if !ok {
		return nil, nil, 0, true, nil
	}
	fresh := r.f.fresh(req, ti.prompt)
	// The one rule (isMain). The main thread's identity is fixed by the
	// turn's first request in the fresh layout, which always comes first, and
	// is never changed after.
	main, known, err := r.mainIdentity(session, ti, req.id, fresh)
	if err != nil {
		return nil, nil, 0, false, err
	}
	switch {
	case known && !isMain(req.id, main, known):
		// Another system prompt (codex: another thread): a subagent or a
		// side call, never main, whatever its messages carry.
		return nil, nil, 0, true, nil
	case known && !fresh:
		// The main thread without the prompt where a fresh process puts it:
		// after the agent compacted the conversation, or a layout the
		// gateway does not know. The gateway has no capture of a compacted
		// request, so it cannot say whether the summary covered the
		// replayed history: it refuses rather than send a turn with no past.
		return nil, nil, 0, false, errors.New("a request of the turn's main thread " + r.f.freshRule() + ", as after a compaction or a layout change, so the replayed history cannot be placed")
	case !known:
		// Before the main thread's first request. One with no tools and no
		// prompt is a side call (Claude Code's quota check); anything else
		// is a layout the gateway does not know.
		if req.tools == 0 && !r.f.hasPrompt(req.msgs, ti.prompt) {
			return nil, nil, 0, true, nil
		}
		return nil, nil, 0, false, errors.New("a request came before the turn's main thread's first one, in a layout the gateway does not know, so the replayed history cannot be placed")
	}
	h, err := r.history(session, ti.from, true)
	if err != nil {
		var miss *missing
		if errors.As(err, &miss) {
			return nil, nil, 0, false, fmt.Errorf("the history of turn %s cannot be read: %s", ti.from, miss.why)
		}
		return nil, nil, 0, false, errors.New("the store could not be read")
	}
	// A request to another model than the history's, as after a switch of
	// agent on the same harness: the history is laid out for it.
	if m := requestModel(req.fields); h.model != "" && m != "" && m != h.model {
		msgs, err := r.f.otherModel(h.messages)
		if err != nil {
			return nil, nil, 0, false, errors.New("the history of turn " + ti.from + " cannot be laid out for another model: " + err.Error())
		}
		other := *h
		other.messages = msgs
		h = &other
	}
	out, hdr, err = r.f.build(req, h, ti.prompt)
	if err != nil {
		return nil, nil, 0, false, err
	}
	return out, hdr, len(h.messages), false, nil
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

func (claude) carry(h *history, c store.CallContext) { h.prior = priorTurnContext(c.Request) }

func (claude) asHistory(m json.RawMessage) (json.RawMessage, error) { return asHistory(m) }

// reply: the assistant message. One that stopped for tool_use asked for
// tools; its results are a user message with a tool_result marked as an
// error for each tool_use.
func (claude) reply(rep body.Reply, stop string) ([]json.RawMessage, ending, error) {
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
				{Key: "content", Value: jsonString(notFinished)},
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

	// The fresh process's first system message, cut to its hook texts: the
	// environment it also holds is already in the history's own copy. A
	// history with no system message has no such copy (no earlier turn
	// reached the model, as when a session's first turn failed before its
	// first call), so the message is kept whole, as a fresh process sends it.
	m1 := msgs[1]
	if hasSystem(h.messages) {
		if m1, err = cutMessage(msgs[1]); err != nil {
			return nil, nil, err
		}
	}

	hist := make([]json.RawMessage, len(h.messages))
	copy(hist, h.messages)
	// On the turn's first request only (the person's message and Claude
	// Code's system message, nothing else yet), a breakpoint on the history's
	// last message, the earlier turn's reply. That is where a resumed
	// process puts one, so the whole history is read from cache from then on.
	// Its cache_control is copied from Claude Code's own (breakpointValue).
	addBP := len(msgs) == 2
	var newMsgs []json.RawMessage
	newMsgs = append(newMsgs, hist...)
	newMsgs = append(newMsgs, body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"user"`)},
		{Key: "content", Value: promptText}}))
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

// hasSystem reports whether any of msgs is a role-system message.
func hasSystem(msgs []json.RawMessage) bool {
	for _, m := range msgs {
		if role(m) == "system" {
			return true
		}
	}
	return false
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

// cutMessage cuts a fresh first system message to its hook texts. Claude Code
// sends it as a list of one text block while it is the request's last message
// (the turn's first request) and as a plain string after that (2.1.319,
// 2026-11-06; the string case is what broke part 3's first tap). It returns
// nil when nothing is left.
func cutMessage(m json.RawMessage) (json.RawMessage, error) {
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
			cut, err := cutTurnSystem(s)
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
		cut, err := cutTurnSystem(text)
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
