package replay

// A note for a running turn (queue/notes.go) is a message in the turn's
// record, and own replay lays it out from there. One rule: every main-thread
// request of the turn carries each note the turn was given, at the note's
// place in the agent's conversation, so the model reads one conversation for
// the whole turn and the turn's newest main call, which the next turn's
// history is built from, holds them all.
//
// A note waiting for the turn goes at the end of its next main-thread
// request. It is given once the vendor accepts that call (a 2xx): then the
// call's context.added records it, with the call's exchange and its place
// (event.Added's At), and control marks it delivered when that event reaches
// its record. A call the vendor does not accept gives it back, and the next
// call carries it.
//
// Its place is how many of the agent's messages came before it. Until the
// agent compacts, its conversation only grows, so a later request carries it
// at that place. Whether a request is compacted is the one test that decides
// it everywhere (format.fresh, recorded as the call's history.compacted),
// never a count of messages: a compacted request carries every note right
// after its summary, however many compactions came before, so a note is
// never put inside a tool loop.
//
// The next turn's history is rebuilt from one call of this turn: its newest
// main call, or after a compaction its last one before it
// (compactedHistory). A note that call did not carry, one given after it, is
// carried at the next turn's start, from the record (notesAfter).
//
// The record is the truth. Between the vendor's 2xx and the store holding
// its context.added, the Replayer keeps the note in memory (placed): its
// own work in progress, never read by anyone else.
//
// A side call (a title, a subagent) carries no note: which request is the
// main thread's is the one rule isMain says.

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// Notes gives a running turn its waiting notes, each a context.added piece of
// kind note, as the agent gets it, and takes back those a call that was not
// accepted carried.
type Notes interface {
	TakeNotes(session, turn string) []event.Added
	ReturnNotes(session, turn string, notes []event.Added)
}

// SetNotes sets where the turns' notes come from. Call it before serving.
func (r *Replayer) SetNotes(n Notes) { r.notes = n }

// noteCall is what withNotes made of one request: the request with the turn's
// notes at their places, the waiting notes it took, and their place.
type noteCall struct {
	data    []byte
	changed bool
	taken   []event.Added
	at      int
}

// withNotes lays out the turn's notes in data, the agent's request, when it is
// the turn's main thread's: those its record holds, each at its place, then
// the waiting ones, at the end.
func (r *Replayer) withNotes(session, turn string, data []byte) noteCall {
	nc := noteCall{data: data}
	if r.notes == nil {
		return nc
	}
	ti, err := r.turn(session, turn)
	if err != nil || !ti.found {
		return nc
	}
	req, ok := r.f.parse(data)
	if !ok {
		return nc
	}
	fresh := r.f.fresh(req, ti.prompt)
	main, known, err := r.mainIdentity(session, ti, req.id, fresh)
	if err != nil || !isMain(req.id, main, known) {
		return nc
	}
	given, err := r.given(session, turn)
	if err != nil {
		return nc
	}
	nc.taken = r.notes.TakeNotes(session, turn)
	if len(given) == 0 && len(nc.taken) == 0 {
		return nc
	}
	// A compacted request: every note right after the summary. Any other:
	// each given note at its place, then the waiting ones at the end.
	nc.at = len(req.msgs)
	if !fresh {
		nc.at = r.f.afterSummary(req.msgs)
	}
	type placed struct {
		at   int
		text string
	}
	var out []placed
	for _, n := range given {
		at := nc.at
		if fresh {
			at = min(max(n.At, 0), len(req.msgs))
		}
		out = append(out, placed{at, n.Text})
	}
	for _, n := range nc.taken {
		out = append(out, placed{nc.at, n.Text})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	msgs := make([]json.RawMessage, 0, len(req.msgs)+len(out))
	next := 0
	for _, p := range out {
		msgs = append(msgs, req.msgs[next:p.at]...)
		next = p.at
		msgs = append(msgs, r.f.userNote(p.text))
	}
	msgs = append(msgs, req.msgs[next:]...)
	nc.data, nc.changed = withConversation(req.fields, r.f.conversation(), msgs), true
	return nc
}

// given are the notes the turn was given, oldest first: those its record
// holds, then those a 2xx gave whose context.added the store does not hold
// yet.
func (r *Replayer) given(session, turn string) ([]event.Added, error) {
	stored, err := r.src.TurnNotes(session, turn)
	if err != nil {
		return nil, err
	}
	k := [2]string{session, turn}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Added
	seen := map[int64]bool{}
	for _, n := range stored {
		seen[n.Note] = true
		out = append(out, n.Added)
	}
	var left []event.Added
	for _, n := range r.placed[k] {
		if !seen[n.Note] {
			left = append(left, n)
			out = append(out, n)
		}
	}
	if len(left) == 0 {
		delete(r.placed, k)
	} else {
		r.placed[k] = left
	}
	return out, nil
}

// notesAfter are the notes of turn its call base did not carry, oldest
// first: those first given at a call newer than base. mains are the turn's
// main-thread calls, newest first, base among them.
func (r *Replayer) notesAfter(session, turn string, mains []call, base *call) ([]json.RawMessage, error) {
	given, err := r.src.TurnNotes(session, turn)
	if err != nil {
		return nil, err
	}
	newer := map[string]bool{}
	for _, c := range mains {
		if c.a.Exchange == base.a.Exchange {
			break
		}
		newer[c.a.Exchange] = true
	}
	var out []json.RawMessage
	for _, n := range given {
		if newer[n.Exchange] {
			out = append(out, r.f.userNote(n.Text))
		}
	}
	return out, nil
}

// replied is the call's Replied: a 2xx gives the notes it took, recorded with
// their place; any other answer gives them back to the runner.
func (r *Replayer) replied(session, turn string, nc noteCall) func(int) []event.Event {
	if len(nc.taken) == 0 {
		return nil
	}
	return func(status int) []event.Event {
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			r.notes.ReturnNotes(session, turn, nc.taken)
			return nil
		}
		given := make([]event.Added, len(nc.taken))
		for i, n := range nc.taken {
			n.At = nc.at
			given[i] = n
		}
		k := [2]string{session, turn}
		r.mu.Lock()
		if len(r.placed) >= memoMax {
			r.evictClosed()
		}
		r.placed[k] = append(r.placed[k], given...)
		r.mu.Unlock()
		return []event.Event{{Kind: event.ContextAdded, Added: given}}
	}
}

// withConversation is fields with the conversation field key set to msgs.
func withConversation(fields []body.Field, key string, msgs []json.RawMessage) []byte {
	out := make([]body.Field, len(fields))
	copy(out, fields)
	for i, f := range out {
		if f.Key == key {
			out[i].Value = joinArray(msgs)
		}
	}
	return body.Object(out)
}

// Claude Code's requests (Messages API): a user message of one text block.
// The API reads it with a user message next to it, such as the one with the
// tool results it follows, as one turn.
func (claude) conversation() string { return "messages" }

func (claude) userNote(text string) json.RawMessage {
	block := body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)}, {Key: "text", Value: jsonString(text)}})
	return body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"user"`)},
		{Key: "content", Value: joinArray([]json.RawMessage{block})}})
}

// afterSummary: a compacted Claude Code request starts with the summary as
// the user's message.
func (claude) afterSummary(msgs []json.RawMessage) int { return afterLeading(msgs) }

// codex's requests (Responses API): a user message item, often after a tool
// call's output.
func (codex) conversation() string { return "input" }

func (codex) userNote(text string) json.RawMessage { return openai.UserMessage(text) }

// afterSummary: after codex's last compaction item, or else after its first
// item that is not context.
func (codex) afterSummary(msgs []json.RawMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if openai.IsCompaction(msgs[i]) {
			return i + 1
		}
	}
	for i, m := range msgs {
		if _, _, ok := openai.ContextKey(m); !ok {
			return i + 1
		}
	}
	return len(msgs)
}

// OpenCode's requests (Chat Completions): a user message, often after a
// tool's result.
func (chatCompletions) conversation() string { return "messages" }

func (chatCompletions) userNote(text string) json.RawMessage { return chatUserMessage(text) }

// afterSummary: OpenCode's summary is the first message after its system
// and developer messages.
func (chatCompletions) afterSummary(msgs []json.RawMessage) int { return afterLeading(msgs) }

// afterLeading is the place right after the first message that is not a
// system or developer message: the summary of a compacted conversation.
func afterLeading(msgs []json.RawMessage) int {
	for i, m := range msgs {
		if r := role(m); r != "system" && r != "developer" {
			return i + 1
		}
	}
	return len(msgs)
}
