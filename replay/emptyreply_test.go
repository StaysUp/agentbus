package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/store"
)

// emptyWords is what the note for a reply that said nothing says.
const emptyWords = "reply here was empty"

// emptyRow is one stored reply and what the history must do with it.
type emptyRow struct {
	name string
	// reply is the stored reply; stop its stop reason, "" for the format's usual.
	reply, stop string
	// silent: it said nothing, so exactly one note stands in for it.
	silent bool
	// keep lists what the history must still carry of it; gone what it must
	// not, such as an empty list the vendor refuses.
	keep, gone []string
}

// emptyFormat is what the scenarios need to know of one format: how to store
// a turn, the requests a fresh process sends, and what its vendor refuses.
type emptyFormat struct {
	name     string
	stop     string
	plain    emptyRow
	plainOut string
	replayer func(src *fakeSource) *Replayer
	// fresh is turn n's first request from a fresh process, with the prompt p.
	fresh func(n int, p string) string
	// afterTools is a turn's request that ends in a tool result, after pairs
	// of tool calls and results.
	afterTools func(n int, p string, pairs int) string
	store      func(t *testing.T, src *fakeSource, turn, x, request, reply, stop string)
	items      func(t *testing.T, raw []byte) []json.RawMessage
	// refused says what the vendor refuses in the history before the prompt.
	refused func(items []json.RawMessage) string
	// bare lays a history message out without a breakpoint, which a history
	// carries on its last messages only.
	bare func(m json.RawMessage) json.RawMessage
	rows []emptyRow
}

func emptyPrompt(n int) string { return fmt.Sprintf("word %d", n) }

func blankN(n int, one string) string {
	return strings.TrimSuffix(strings.Repeat(one+",", n), ",")
}

func emptyFormats() []emptyFormat {
	return []emptyFormat{emptyClaude(), emptyCodex(), emptyChat()}
}

func emptyClaude() emptyFormat {
	msg := func(blocks string) string { return `{"role":"assistant","content":` + blocks + `}` }
	txt := func(s string) string { return `{"type":"text","text":` + jsonQuote(s) + `}` }
	list := func(bs ...string) string { return "[" + strings.Join(bs, ",") + "]" }
	const (
		think  = `{"type":"thinking","thinking":"","signature":"U0lHLXR3bw=="}`
		redact = `{"type":"redacted_thinking","data":"ZW5j"}`
		use    = `{"type":"tool_use","id":"toolu_9","name":"Bash","input":{"__proto__":1,"type":"text","text":""}}`
		srv    = `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"x"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}`
	)
	plain := msg(list(txt("ok")))
	return emptyFormat{
		name: "claude", stop: "end_turn",
		plain:    emptyRow{name: "plain", reply: plain},
		plainOut: plain,
		replayer: func(src *fakeSource) *Replayer { return New(src) },
		fresh: func(n int, p string) string {
			return strings.ReplaceAll(t2First(p), "env two", fmt.Sprintf("env %d", n))
		},
		afterTools: func(n int, p string, pairs int) string {
			var more []string
			for i := 0; i < pairs; i++ {
				more = append(more, fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_p%d","name":"Bash","input":{}}]},{"role":"user","content":[{"tool_use_id":"toolu_p%d","type":"tool_result","content":"x","is_error":false}]},`, i, i))
			}
			loop := strings.Replace(t2Loop, "which word?", p, 1)
			return strings.Replace(loop, `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2"`, strings.Join(more, "")+`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2"`, 1)
		},
		store: func(t *testing.T, src *fakeSource, turn, x, request, reply, stop string) {
			src.addCall(t, turn, x, request, reply, stop)
		},
		items: func(t *testing.T, raw []byte) []json.RawMessage { return mustElements(t, field(raw, "messages")) },
		bare: func(m json.RawMessage) json.RawMessage {
			b, err := asHistory(m)
			if err != nil {
				return m
			}
			return b
		},
		refused: func(items []json.RawMessage) string {
			for i, m := range items {
				if role(m) == "system" {
					continue
				}
				if why := emptyForTheAPI(m); why != "" && (i < len(items)-1 || strings.Contains(why, "breakpoint")) {
					return fmt.Sprintf("message %d has %s: %s", i, why, m)
				}
			}
			return ""
		},
		rows: []emptyRow{
			{name: "none", reply: msg(`[]`), silent: true},
			{name: "empty-string", reply: msg(`""`), silent: true},
			{name: "blank-string", reply: msg(jsonQuote("  \n")), silent: true},
			{name: "empty-text", reply: msg(list(txt(""))), silent: true},
			{name: "blank-text", reply: msg(list(txt("\n\n"))), silent: true},
			{name: "unicode-blank", reply: msg(list(txt("\u00a0\u2003\u3000"))), silent: true},
			{name: "two-blank", reply: msg(list(txt(" "), txt(""))), silent: true},
			{name: "many-blank", reply: msg("[" + blankN(200, txt(" ")) + "]"), silent: true},
			{name: "thinking-only", reply: msg(list(think)), silent: true, keep: []string{`"signature":"U0lHLXR3bw=="`}},
			{name: "thinking-blank", reply: msg(list(think, txt("\n"))), silent: true, keep: []string{`"signature":"U0lHLXR3bw=="`}},
			{name: "redacted-only", reply: msg(list(redact)), silent: true, keep: []string{`"data":"ZW5j"`}},
			{name: "refusal-none", reply: msg(`[]`), stop: "refusal", silent: true},
			{name: "max-tokens-empty", reply: msg(`[]`), stop: "max_tokens", silent: true},
			{name: "null-content", reply: msg(`null`), silent: true},
			{name: "missing-content", reply: `{"role":"assistant"}`, silent: true},
			{name: "zwsp", reply: msg(list(txt("\u200b"))), keep: []string{jsonQuote("\u200b")}},
			{name: "server-tool-only", reply: msg(list(srv)), keep: []string{`srvtoolu_1`}},
			{name: "blank-then-words", reply: msg(list(txt(" "), txt("ok2"))), keep: []string{`"ok2"`}},
			{name: "words-then-blank", reply: msg(list(txt("ok2"), txt("\n"))), keep: []string{`"ok2"`}},
			{name: "tool-only", reply: msg(list(use)), stop: "tool_use", keep: []string{`"id":"toolu_9"`, `"tool_use_id":"toolu_9"`}},
			{name: "blank-then-tool", reply: msg(list(txt(" "), use)), stop: "tool_use", keep: []string{`"id":"toolu_9"`, `"tool_use_id":"toolu_9"`}},
		},
	}
}

func emptyCodex() emptyFormat {
	msg := func(text string) string {
		return `{"type":"message","id":"msg_x","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + jsonQuote(text) + `}]}`
	}
	const (
		cont    = `{"type":"message","id":"msg_c","role":"assistant","end_turn":false,"content":[{"type":"output_text","text":" "}]}`
		refusal = `{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`
		search  = `{"type":"web_search_call","id":"ws_1","status":"completed"}`
		noParts = `{"type":"message","role":"assistant","content":[]}`
	)
	thread := func(n int) string { return fmt.Sprintf("019a0000-0000-7000-8000-%012d", n) }
	req := func(n int, p string, more ...string) string {
		id := fmt.Sprintf("c%d", n)
		return codexReq(thread(n), append(append(context(id, "read-only"), prompt(id, p)), more...)...)
	}
	return emptyFormat{
		name: "codex", stop: "completed",
		plain:    emptyRow{name: "plain", reply: items(answer2)},
		plainOut: answer2,
		replayer: func(src *fakeSource) *Replayer { return NewCodex(src) },
		fresh:    func(n int, p string) string { return req(n, p) },
		afterTools: func(n int, p string, pairs int) string {
			var more []string
			for i := 0; i < pairs; i++ {
				more = append(more,
					fmt.Sprintf(`{"type":"custom_tool_call","id":"ctc_p%d","status":"completed","call_id":"call_p%d","name":"exec","input":"ls"}`, i, i),
					fmt.Sprintf(`{"type":"custom_tool_call_output","id":"ctco_p%d","call_id":"call_p%d","output":"a.txt"}`, i, i))
			}
			return req(n, p, append(more, toolCall2, toolOut2)...)
		},
		store: func(t *testing.T, src *fakeSource, turn, x, request, reply, _ string) {
			src.addCodexCall(t, turn, x, request, completedCall, reply)
		},
		items: func(t *testing.T, raw []byte) []json.RawMessage { return mustElements(t, field(raw, "input")) },
		refused: func(its []json.RawMessage) string {
			for i, it := range its {
				if body.Label(it, "type") == "reasoning" && (i == len(its)-1 || body.Label(its[i+1], "role") == "user") {
					return fmt.Sprintf("a reasoning item with no item after it of its reply: %s", it)
				}
			}
			return ""
		},
		rows: []emptyRow{
			{name: "no-items", reply: items(), silent: true},
			{name: "reasoning-only", reply: items(reasoning1), silent: true, keep: []string{reasoning1}},
			{name: "reasoning-blank", reply: items(reasoning1, msg("\n")), silent: true, keep: []string{reasoning1}},
			{name: "blank-message", reply: items(msg("\n\n")), silent: true},
			{name: "empty-text-message", reply: items(msg("")), silent: true},
			{name: "unicode-blank", reply: items(msg("\u00a0\u2003\u3000")), silent: true},
			{name: "two-blank", reply: items(msg(" "), msg("")), silent: true},
			{name: "many-blank", reply: "[" + blankN(200, msg(" ")) + "]", silent: true},
			{name: "empty-content-list", reply: items(noParts), silent: true},
			{name: "zwsp", reply: items(msg("\u200b")), keep: []string{jsonQuote("\u200b")}},
			{name: "blank-then-words", reply: items(msg(" "), msg("ok2")), keep: []string{`"ok2"`}},
			{name: "words-then-blank", reply: items(msg("ok2"), msg("\n")), keep: []string{`"ok2"`}},
			{name: "reasoning-words", reply: items(reasoning1, answer1), keep: []string{reasoning1, answer1}},
			{name: "refusal-part", reply: items(refusal), keep: []string{refusal}},
			{name: "hosted-search-only", reply: items(search), keep: []string{search}},
			{name: "tool-only", reply: items(toolCall2), keep: []string{toolCall2, `"call_id":"call_2"`}},
			{name: "reasoning-tool", reply: items(reasoning1, toolCall2), keep: []string{reasoning1, toolCall2}},
			{name: "continues-blank", reply: items(cont), keep: []string{cont}},
		},
	}
}

func emptyChat() emptyFormat {
	asst := func(fields string) string { return `{"role":"assistant",` + fields + `}` }
	part := func(s string) string { return `{"type":"text","text":` + jsonQuote(s) + `}` }
	const (
		details  = `[{"type":"reasoning.text","text":"t","signature":"s"}]`
		nullTool = `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]}`
		parts    = `{"role":"assistant","content":[{"type":"text","text":" "},{"type":"text","text":"ok2"}]}`
		refPart  = `{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`
	)
	plain := asst(`"content":"ok"`)
	return emptyFormat{
		name: "opencode", stop: "stop",
		plain:    emptyRow{name: "plain", reply: plain},
		plainOut: plain,
		replayer: func(src *fakeSource) *Replayer { return NewChatCompletions(src) },
		fresh: func(n int, p string) string {
			return chatReq("glm", chatSysMsg(chatSys), chatUser(p))
		},
		afterTools: func(n int, p string, pairs int) string {
			var more []string
			for i := 0; i < pairs; i++ {
				more = append(more,
					fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"call_p%d","type":"function","function":{"name":"bash","arguments":"{}"}}]}`, i),
					fmt.Sprintf(`{"role":"tool","tool_call_id":"call_p%d","content":"a.txt"}`, i))
			}
			return chatReq("glm", append(append([]string{chatSysMsg(chatSys), chatUser(p)}, more...), chatCall, chatOut)...)
		},
		store: func(t *testing.T, src *fakeSource, turn, x, request, reply, stop string) {
			src.addChatCall(t, turn, x, request, store.Call{Responded: true, Status: 200, Stop: stop}, reply)
		},
		items:   func(t *testing.T, raw []byte) []json.RawMessage { return mustElements(t, field(raw, "messages")) },
		refused: emptyChatMessages,
		rows: []emptyRow{
			{name: "empty-string", reply: asst(`"content":""`), silent: true},
			{name: "blank-string", reply: asst(`"content":` + jsonQuote("  \n")), silent: true},
			{name: "unicode-blank", reply: asst(`"content":` + jsonQuote("\u00a0\u2003\u3000")), silent: true},
			{name: "null-content", reply: asst(`"content":null`), silent: true},
			{name: "missing-content", reply: `{"role":"assistant"}`, silent: true},
			{name: "empty-list", reply: asst(`"content":[]`), silent: true},
			{name: "blank-part", reply: asst(`"content":[` + part(" ") + `]`), silent: true},
			{name: "two-blank-parts", reply: asst(`"content":[` + part(" ") + `,` + part("") + `]`), silent: true},
			{name: "many-blank", reply: asst(`"content":[` + blankN(200, part(" ")) + `]`), silent: true},
			{name: "reasoning-empty", reply: asst(`"content":"","reasoning_content":"secret"`), silent: true, keep: []string{`"reasoning_content":"secret"`}},
			{name: "reasoning-details-empty", reply: asst(`"content":null,"reasoning_details":` + details), silent: true, keep: []string{`"reasoning_details":` + details}},
			{name: "length-empty", reply: asst(`"content":""`), stop: "length", silent: true},
			{name: "filter-empty", reply: asst(`"content":""`), stop: "content_filter", silent: true},
			{name: "empty-tool-calls", reply: asst(`"content":"","tool_calls":[]`), silent: true, gone: []string{`"tool_calls":[]`}},
			{name: "null-empty-tool-calls", reply: asst(`"content":null,"tool_calls":[]`), silent: true, gone: []string{`"tool_calls":[]`}},
			{name: "null-audio-and-images", reply: asst(`"content":"","audio":null,"images":[]`), silent: true},
			{name: "function-call", reply: asst(`"content":null,"function_call":{"name":"bash","arguments":"{}"}`), keep: []string{`"function_call"`}},
			{name: "audio", reply: asst(`"content":null,"audio":{"id":"audio_1","expires_at":1}`), keep: []string{`"audio_1"`}},
			{name: "images", reply: asst(`"content":"","images":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`), keep: []string{`"images"`}},
			{name: "zwsp", reply: asst(`"content":` + jsonQuote("\u200b")), keep: []string{jsonQuote("\u200b")}},
			{name: "blank-part-then-words", reply: parts, keep: []string{parts}},
			{name: "refusal-field", reply: asst(`"content":null,"refusal":"no"`), keep: []string{`"refusal":"no"`}},
			{name: "refusal-part", reply: refPart, keep: []string{refPart}},
			{name: "tool-only", reply: chatCall, stop: "tool_calls", keep: []string{chatCall}},
			{name: "null-tool-only", reply: nullTool, stop: "tool_calls", keep: []string{nullTool}},
		},
	}
}

// emptyChatMessages says what the API refuses in a Chat Completions history:
// an assistant message with no tool call, no refusal and no text.
func emptyChatMessages(msgs []json.RawMessage) string {
	for i, m := range msgs {
		if role(m) != "assistant" || i == len(msgs)-1 {
			continue
		}
		if calls, ok := body.Elements(field(m, "tool_calls")); ok && len(calls) > 0 {
			continue
		}
		var refusal string
		if json.Unmarshal(field(m, "refusal"), &refusal) == nil && strings.TrimSpace(refusal) != "" {
			continue
		}
		other := false
		for _, k := range []string{"function_call", "audio", "images"} {
			if v := field(m, k); len(v) > 0 && string(v) != "null" && string(v) != "[]" {
				other = true
			}
		}
		if other {
			continue
		}
		c := field(m, "content")
		if len(c) == 0 || string(c) == "null" {
			return fmt.Sprintf("message %d has no content: %s", i, m)
		}
		var s string
		if json.Unmarshal(c, &s) == nil {
			if strings.TrimSpace(s) == "" {
				return fmt.Sprintf("message %d has blank content: %s", i, m)
			}
			continue
		}
		parts, ok := body.Elements(c)
		if !ok {
			continue
		}
		said := false
		for _, p := range parts {
			var text string
			if body.Label(p, "type") != "text" || (json.Unmarshal(field(p, "text"), &text) == nil && strings.TrimSpace(text) != "") {
				said = true
			}
		}
		if !said {
			return fmt.Sprintf("message %d has no text in its parts: %s", i, m)
		}
	}
	return ""
}

// fingerprint is everything the fake store holds, as one string.
func fingerprint(src *fakeSource) string {
	var b strings.Builder
	xs := make([]string, 0, len(src.replies))
	for x := range src.replies {
		xs = append(xs, x)
	}
	sort.Strings(xs)
	for _, x := range xs {
		r := src.replies[x]
		fmt.Fprintf(&b, "reply %s %s %s\n", x, r.Stop, r.Message)
	}
	xs = xs[:0]
	for x := range src.calls {
		xs = append(xs, x)
	}
	sort.Strings(xs)
	for _, x := range xs {
		c := src.calls[x]
		fmt.Fprintf(&b, "call %s %s\n", x, c.Request)
		for _, p := range c.Pieces {
			fmt.Fprintf(&b, "  %s %s %s\n", p.Kind, p.Label, p.JSON)
		}
	}
	return b.String()
}

// rewrite rewrites a turn's first request and holds it to what a rewrite
// must not do: change its input, change what is stored, refuse, or give
// another answer the second time. again is off for a long session, where
// comparing everything stored at every turn would cost more than the test.
func (f emptyFormat) rewrite(t *testing.T, src *fakeSource, r *Replayer, turn, request string, again bool) []byte {
	t.Helper()
	before := ""
	if again {
		before = fingerprint(src)
	}
	in := []byte(request)
	rw := r.Rewrite("s", turn, in)
	if string(in) != request {
		t.Errorf("turn %s: the request handed to Rewrite was changed", turn)
	}
	if again && fingerprint(src) != before {
		t.Errorf("turn %s: what is stored was changed", turn)
	}
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("turn %s refused or unchanged: %+v %+v", turn, rw.Events, rw.Refuse)
	}
	if again {
		if second := r.Rewrite("s", turn, []byte(request)); !bytes.Equal(second.Body, rw.Body) {
			t.Errorf("turn %s: a second rewrite of the same request differs", turn)
		}
	}
	return rw.Body
}

// run stores one turn for each row, the first one's request ending in a tool
// result after pairs more tool calls when pairs is not negative, then rewrites the request of the turn after them.
func (f emptyFormat) run(t *testing.T, rows []emptyRow, pairs int, again bool) []byte {
	t.Helper()
	src := newSource()
	r := f.replayer(src)
	for k := 1; k <= len(rows); k++ {
		turn := fmt.Sprintf("t%d", k)
		from := ""
		if k > 1 {
			from = fmt.Sprintf("t%d", k-1)
		}
		src.addTurn(turn, emptyPrompt(k), from)
		var request string
		switch {
		case k == 1 && pairs >= 0:
			request = f.afterTools(k, emptyPrompt(k), pairs)
		case k == 1:
			request = f.fresh(k, emptyPrompt(k))
		default:
			request = string(f.rewrite(t, src, r, turn, f.fresh(k, emptyPrompt(k)), again))
		}
		stop := rows[k-1].stop
		if stop == "" {
			stop = f.stop
		}
		f.store(t, src, turn, fmt.Sprintf("x%d", k), request, rows[k-1].reply, stop)
		src.setEnd(turn, store.TurnEnd{Closed: true, ExitCode: intp(0)})
	}
	last := fmt.Sprintf("t%d", len(rows)+1)
	src.addTurn(last, emptyPrompt(len(rows)+1), fmt.Sprintf("t%d", len(rows)))
	return f.rewrite(t, src, r, last, f.fresh(len(rows)+1, emptyPrompt(len(rows)+1)), again)
}

// check holds the final request to what the rows ask: one note for each reply
// that said nothing and none for the others, no message the vendor refuses,
// what a row keeps still there, and each plain reply as it was.
func (f emptyFormat) check(t *testing.T, raw []byte, rows []emptyRow) {
	t.Helper()
	notes, plain := 0, 0
	for _, row := range rows {
		if row.silent {
			notes++
		}
		if row.name == "plain" {
			plain++
		}
	}
	s := string(raw)
	if got := strings.Count(s, emptyWords); got != notes {
		t.Errorf("%d empty-reply notes in the request, want %d", got, notes)
	}
	its := f.items(t, raw)
	if why := f.refused(its); why != "" {
		t.Errorf("the vendor refuses this history: %s", why)
	}
	got := 0
	for _, it := range its {
		if f.bare != nil {
			it = f.bare(it)
		}
		if string(it) == f.plainOut {
			got++
		}
	}
	if got != plain {
		t.Errorf("%d plain replies as they were, want %d", got, plain)
	}
	for _, row := range rows {
		for _, w := range row.keep {
			if !strings.Contains(s, w) {
				t.Errorf("%s: the request lacks %s", row.name, w)
			}
		}
		for _, w := range row.gone {
			if strings.Contains(s, w) {
				t.Errorf("%s: the request still has %s", row.name, w)
			}
		}
	}
}

// A reply that said nothing is replayed as a note, in every format, wherever
// it sits in the session: last, first or middle of three, several in a row,
// and right after a tool result. A reply that said something, a tool call or
// a refusal included, is never given a note, and a plain reply stays as it was.
func TestEmptyReplyEveryFormat(t *testing.T) {
	for _, f := range emptyFormats() {
		for _, row := range f.rows {
			scenarios := []struct {
				name  string
				rows  []emptyRow
				pairs int
			}{
				{"last", []emptyRow{row}, -1},
				{"first of three", []emptyRow{row, f.plain, f.plain}, -1},
				{"middle of three", []emptyRow{f.plain, row, f.plain}, -1},
				{"three in a row", []emptyRow{row, row, row}, -1},
				{"after a tool result", []emptyRow{row}, 0},
			}
			for _, sc := range scenarios {
				t.Run(f.name+"/"+row.name+"/"+sc.name, func(t *testing.T) {
					f.check(t, f.run(t, sc.rows, sc.pairs, true), sc.rows)
				})
			}
		}
	}
}

// A turn that ran 500 tool calls before its empty reply: the whole history
// goes back, the tool loop untouched, with one note for the reply.
func TestEmptyReplyAfterALongToolLoop(t *testing.T) {
	for _, f := range emptyFormats() {
		t.Run(f.name, func(t *testing.T) {
			silent := f.rows[0]
			if !silent.silent {
				t.Fatalf("%s: the first row is not an empty reply", f.name)
			}
			rows := []emptyRow{silent}
			raw := f.run(t, rows, 500, false)
			f.check(t, raw, rows)
			for _, id := range []string{"_p0\"", "_p499\""} {
				if !strings.Contains(string(raw), id) {
					t.Errorf("the tool loop lost %s", id)
				}
			}
		})
	}
}
