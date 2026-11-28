package replay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// fakeNotes holds the notes waiting for a turn; TakeNotes empties it, and
// ReturnNotes puts them back.
type fakeNotes struct {
	waiting  []event.Added
	returned int
}

func (f *fakeNotes) TakeNotes(_, _ string) []event.Added {
	out := f.waiting
	f.waiting = nil
	return out
}

func (f *fakeNotes) ReturnNotes(_, _ string, notes []event.Added) {
	f.returned += len(notes)
	f.waiting = append(notes, f.waiting...)
}

// noteCase is one API format's requests of one turn whose prompt is "one":
// its first main-thread request, the two next ones of its tool loop, each
// longer than the one before, a request after the agent compacted, and a
// side call; how a call is stored with a finished reply; the next turn's
// fresh request, whose prompt is "two"; and the note as a message.
type noteCase struct {
	name                  string
	replayer              func(Source) *Replayer
	first, second, third  string
	compacted, side, next string
	store                 func(f *fakeSource, t *testing.T, turn, exchange, request string)
	conversation          string
	note                  string
	wantCompacted         func(note string) []string
}

const folderNote = "your folder changed"

func noteCases() []noteCase {
	note := jsonQuote(event.NotePrefix + folderNote)
	claudeHead := func(p string) string {
		return `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"},{"type":"text","text":"` + p + `"}]},` +
			`{"role":"system","content":[{"type":"text","text":"# Environment\nenv\n\nToday's date is d.","cache_control":{"type":"ephemeral"}}]}`
	}
	claudeTail := `],"tools":[{"name":"Bash","input_schema":{}}],"stream":true}`
	claudeStep := func(id string) string {
		return `,{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"ls"}}]}` +
			`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"a.go"}]}`
	}
	claudeSummary := `{"role":"user","content":[{"type":"text","text":"This session is being continued from a previous conversation. Summary: listed files."}]}`
	claudeSys := `{"role":"system","content":"# Environment\nenv"}`
	codexHead := func(p string) string {
		return `{"model":"gpt-x","stream":true,"store":false,"prompt_cache_key":"th1","input":[` +
			`{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + p + `"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}`
	}
	codexStep := func(id string) string {
		return `,{"type":"function_call","call_id":"` + id + `","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"` + id + `","output":"a.go"}`
	}
	codexBase := `{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}}`
	codexSummary := `{"type":"compaction","encrypted_content":"U1VN"}`
	chatHead := func(p string) string {
		return `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"system","content":"You are opencode."},{"role":"user","content":"` + p + `"}`
	}
	chatTail := `],"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],"stream":true}`
	chatStep := func(id string) string {
		return `,{"role":"assistant","content":null,"tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"bash","arguments":"{}"}}]}` +
			`,{"role":"tool","tool_call_id":"` + id + `","content":"a.go"}`
	}
	chatSys := `{"role":"system","content":"You are opencode."}`
	chatSummary := `{"role":"user","content":"Summary: listed files."}`
	split := func(s string) []string {
		var out []string
		els, _ := body.Elements(json.RawMessage("[" + strings.TrimPrefix(s, ",") + "]"))
		for _, e := range els {
			out = append(out, string(e))
		}
		return out
	}
	return []noteCase{
		{name: "anthropic", replayer: New, conversation: "messages",
			first: claudeHead("one") + claudeTail, second: claudeHead("one") + claudeStep("t1") + claudeTail,
			third:     claudeHead("one") + claudeStep("t1") + claudeStep("t2") + claudeTail,
			compacted: `{"model":"claude-x","messages":[` + claudeSummary + `,` + claudeSys + claudeStep("t3") + claudeTail,
			side:      `{"model":"claude-haiku","system":"Write a title.","messages":[{"role":"user","content":"one"}]}`,
			next:      claudeHead("two") + claudeTail,
			store: func(f *fakeSource, t *testing.T, turn, x, req string) {
				f.addCall(t, turn, x, req, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`, "end_turn")
			},
			note: `{"role":"user","content":[{"type":"text","text":` + note + `}]}`,
			wantCompacted: func(n string) []string {
				return append([]string{claudeSummary, n, claudeSys}, split(claudeStep("t3"))...)
			}},
		{name: "openai-responses", replayer: NewCodex, conversation: "input",
			first: codexHead("one") + `]}`, second: codexHead("one") + codexStep("c1") + `]}`,
			third:     codexHead("one") + codexStep("c1") + codexStep("c2") + `]}`,
			compacted: `{"model":"gpt-x","stream":true,"store":false,"prompt_cache_key":"th1","input":[` + codexBase + `,` + codexSummary + codexStep("c3") + `]}`,
			side:      `{"model":"gpt-x","prompt_cache_key":"other","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}]}`,
			next:      strings.Replace(codexHead("two"), `"th1"`, `"th2"`, 1) + `]}`,
			store: func(f *fakeSource, t *testing.T, turn, x, req string) {
				f.addCodexCall(t, turn, x, req, completedCall, items(answer2))
			},
			note: `{"type":"message","role":"user","content":[{"type":"input_text","text":` + note + `}]}`,
			wantCompacted: func(n string) []string {
				return append([]string{codexBase, codexSummary, n}, split(codexStep("c3"))...)
			}},
		{name: "chat-completions", replayer: NewChatCompletions, conversation: "messages",
			first: chatHead("one") + chatTail, second: chatHead("one") + chatStep("k1") + chatTail,
			third:     chatHead("one") + chatStep("k1") + chatStep("k2") + chatTail,
			compacted: `{"model":"z-ai/glm-5.3-flash","messages":[` + chatSys + `,` + chatSummary + chatStep("k3") + chatTail,
			side:      `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"system","content":"Write a title."},{"role":"user","content":"one"}]}`,
			next:      chatHead("two") + chatTail,
			store: func(f *fakeSource, t *testing.T, turn, x, req string) {
				f.addChatCall(t, turn, x, req, chatStop, `{"role":"assistant","content":"done"}`)
			},
			note: `{"role":"user","content":` + note + `}`,
			wantCompacted: func(n string) []string {
				return append([]string{chatSys, chatSummary, n}, split(chatStep("k3"))...)
			}},
	}
}

// conversationOf is a request's conversation, each message as sent.
func conversationOf(t *testing.T, data []byte, key string) []string {
	t.Helper()
	fields, err := body.Fields(data)
	if err != nil {
		t.Fatalf("request: %v: %s", err, data)
	}
	for _, f := range fields {
		if f.Key == key {
			els, _ := body.Elements(f.Value)
			out := make([]string, len(els))
			for i, e := range els {
				out[i] = string(e)
			}
			return out
		}
	}
	t.Fatalf("request has no %s: %s", key, data)
	return nil
}

func eqConversation(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n%s\nwant:\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// withNoteAt is msgs with note inserted at i.
func withNoteAt(msgs []string, i int, note string) []string {
	return append(append(append([]string{}, msgs[:i]...), note), msgs[i:]...)
}

// The note's whole life in one turn, for each API format, as Codex's two
// reviews found it could go wrong (2026-11-28):
//   - a call the vendor refuses (429) gives the note back, records nothing,
//     and the next call carries it again;
//   - the call the vendor accepts records it once, with its place;
//   - every later main-thread call carries it at that place, before the
//     store holds its context.added and after;
//   - after a compaction it goes right after the summary, never between a
//     tool call and its result;
//   - a side call never carries it, nor takes it;
//   - the next turn's history, built from the turn's newest main call,
//     keeps the note and the tool loop, and is not judged compacted.
func TestNoteLifeInATurn(t *testing.T) {
	for _, c := range noteCases() {
		t.Run(c.name, func(t *testing.T) {
			src := newSource()
			src.addTurn("t1", "one", "")
			r := c.replayer(src)
			notes := &fakeNotes{}
			r.SetNotes(notes)
			conv := func(data string) []string { return conversationOf(t, []byte(data), c.conversation) }
			sent := func(req string) ([]string, func(int) []event.Event) {
				t.Helper()
				rw := r.Rewrite("s", "t1", []byte(req))
				if rw.Refuse != nil {
					t.Fatalf("refused: %+v", rw.Events)
				}
				for _, e := range rw.Events {
					if e.Kind == event.ContextAdded {
						t.Fatalf("a note was recorded before the vendor answered: %+v", e)
					}
				}
				if rw.Body == nil {
					return conv(req), rw.Replied
				}
				if !json.Valid(rw.Body) {
					t.Fatalf("not JSON: %s", rw.Body)
				}
				return conversationOf(t, rw.Body, c.conversation), rw.Replied
			}

			if got, replied := sent(c.first); replied != nil || strings.Join(got, "\n") != strings.Join(conv(c.first), "\n") {
				t.Fatal("the first request, with no note, was changed")
			}
			text := event.NotePrefix + folderNote
			notes.waiting = []event.Added{{Kind: event.AddedNote, Bytes: len(text), Text: text, Note: 7}}

			// A side call leaves the note waiting.
			if got, replied := sent(c.side); replied != nil || strings.Join(got, "\n") != strings.Join(conv(c.side), "\n") {
				t.Fatal("a side call was changed")
			}
			if len(notes.waiting) != 1 {
				t.Fatal("a side call took the note")
			}

			// Refused: given back, nothing recorded.
			two := conv(c.second)
			got, replied := sent(c.second)
			eqConversation(t, "the refused call", got, append(append([]string{}, two...), c.note))
			if evs := replied(429); len(evs) != 0 || notes.returned != 1 || len(notes.waiting) != 1 {
				t.Fatalf("a 429 recorded %+v and returned %d", evs, notes.returned)
			}

			// Accepted: recorded once, with its place.
			got, replied = sent(c.second)
			eqConversation(t, "the accepted call", got, append(append([]string{}, two...), c.note))
			evs := replied(200)
			if len(evs) != 1 || evs[0].Kind != event.ContextAdded || len(evs[0].Added) != 1 {
				t.Fatalf("a 200 recorded %+v", evs)
			}
			given := evs[0].Added[0]
			if given.Note != 7 || given.At != len(two) || given.Compacted || given.Text != text {
				t.Fatalf("recorded %+v, want note 7 at %d", given, len(two))
			}

			// A later call carries it at its place, from memory first, then
			// from the record, the same.
			wantThird := withNoteAt(conv(c.third), len(two), c.note)
			got, replied = sent(c.third)
			eqConversation(t, "a later call, before the record holds the note", got, wantThird)
			if replied != nil {
				t.Fatal("a later call took a note again")
			}
			src.notes = append(src.notes, given)
			got, _ = sent(c.third)
			eqConversation(t, "a later call, from the record", got, wantThird)

			// The next turn is built from the turn's newest main call, which
			// carried the note.
			rw := r.Rewrite("s", "t1", []byte(c.third))
			c.store(src, t, "t1", "x1", string(rw.Body))

			// After a compaction: right after the summary.
			got, _ = sent(c.compacted)
			eqConversation(t, "after a compaction", got, c.wantCompacted(c.note))

			src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
			src.addTurn("t2", "two", "t1")
			next := c.replayer(src).Rewrite("s", "t2", []byte(c.next))
			if next.Refuse != nil || next.Body == nil {
				t.Fatalf("turn 2 not spliced: %+v %+v", next.Events, next.Refuse)
			}
			for _, e := range next.Events {
				if e.Kind == event.HistoryCompacted || e.Kind == event.ReplayFailed {
					t.Fatalf("turn 2: %s %s", e.Kind, e.Error)
				}
			}
			hist := strings.Join(conversationOf(t, next.Body, c.conversation), "\n")
			if strings.Count(hist, folderNote) != 1 || !strings.Contains(hist, "a.go") {
				t.Fatalf("turn 2's history should keep the note once and the tool loop:\n%s", hist)
			}
		})
	}
}

// A turn with no notes sends its requests as the agent sent them.
func TestNoNoteLeavesTheRequest(t *testing.T) {
	for _, c := range noteCases() {
		t.Run(c.name, func(t *testing.T) {
			src := newSource()
			src.addTurn("t1", "one", "")
			r := c.replayer(src)
			r.SetNotes(&fakeNotes{})
			for _, req := range []string{c.first, c.second, c.third, c.compacted} {
				if rw := r.Rewrite("s", "t1", []byte(req)); rw.Body != nil || rw.Replied != nil {
					t.Fatalf("changed with no note: %s", rw.Body)
				}
			}
		})
	}
}
