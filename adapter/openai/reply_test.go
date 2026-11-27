package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Synthetic streams in the shape chatgpt.com sent codex-cli 0.164.0
// (2026-11-06), never saved real replies.

const (
	rReasoning = `{"type":"reasoning","id":"rs_1","metadata":{"a":1},"summary":[],"content":[],"encrypted_content":"RU5DKy89PQ=="}`
	rMessage   = `{"type":"message","id":"msg_1","metadata":{},"role":"assistant","status":"completed","content":[{"type":"output_text","text":"kiwi"}],"phase":"final"}`
)

func replySSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return b.String()
}

func completedEvent(output string) string {
	return `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":` + output + `,"usage":{"input_tokens":3}}}`
}

func TestBuildKeepsItemsByteForByte(t *testing.T) {
	stream := replySSE(`{"type":"response.created","response":{"status":"in_progress","output":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+rReasoning+`}`,
		`{"type":"response.output_item.done","output_index":1,"item":`+rMessage+`}`,
		completedEvent(`[`+rReasoning+`,`+rMessage+`]`)) +
		"data: [DO" // codex stops reading at response.completed
	msg, stop, err := Adapter{}.Build(200, "text/event-stream", []byte(stream))
	if err != nil || stop != "completed" {
		t.Fatalf("Build: %v %q", err, stop)
	}
	if string(msg) != `[`+rReasoning+`,`+rMessage+`]` {
		t.Fatalf("items changed: %s", msg)
	}
}

func TestBuildFromDoneItemsWhenOutputIsEmpty(t *testing.T) {
	stream := replySSE(`{"type":"response.output_item.done","output_index":1,"item":`+rMessage+`}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+rReasoning+`}`,
		completedEvent(`[]`))
	msg, _, err := Adapter{}.Build(200, "text/event-stream", []byte(stream))
	if err != nil || string(msg) != `[`+rReasoning+`,`+rMessage+`]` {
		t.Fatalf("Build: %v %s", err, msg)
	}
}

func TestBuildRefusesPartReplies(t *testing.T) {
	for name, data := range map[string]string{
		"no terminal event": replySSE(`{"type":"response.output_item.done","output_index":0,"item":` + rMessage + `}`),
		"incomplete":        replySSE(`{"type":"response.incomplete","response":{"status":"incomplete","output":[` + rMessage + `]}}`),
		"failed":            replySSE(`{"type":"response.failed","response":{"status":"failed","output":[]}}`),
		"stream error":      replySSE(`{"type":"error","message":"overloaded"}`, completedEvent(`[`+rMessage+`]`)),
		"not json":          "data: {nope\n\n",
	} {
		if _, _, err := (Adapter{}).Build(200, "text/event-stream", []byte(data)); err == nil {
			t.Errorf("%s: kept", name)
		}
	}
	if _, _, err := (Adapter{}).Build(429, "application/json", []byte(`{"error":{}}`)); err == nil {
		t.Error("a 429 was kept")
	}
}

func TestBuildKeepsAnEmptyReply(t *testing.T) {
	for name, tc := range map[string]struct{ contentType, data string }{
		"stream":     {"text/event-stream", replySSE(completedEvent(`[]`))},
		"plain JSON": {"application/json", `{"status":"completed","output":[]}`},
	} {
		msg, stop, err := Adapter{}.Build(200, tc.contentType, []byte(tc.data))
		if err != nil || stop != "completed" || string(msg) != `[]` {
			t.Errorf("%s: Build: %v %q %s", name, err, stop, msg)
		}
	}
}

// An empty completed reply is the model saying nothing only when the stream
// showed no item at all. An item announced and never finished, or any other
// sign of output, means the adapter missed what the model said, and that reply
// is not stored as an empty one.
func TestBuildKeepsAnEmptyReplyOnlyWhenNoItemWasSeen(t *testing.T) {
	quiet := []string{`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.in_progress","response":{"status":"in_progress"}}`,
		`{"type":"codex.rate_limits","plan_type":"plus"}`}
	added := func(i int) string {
		return fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"type":"message","id":"m%d","role":"assistant","content":[]}}`, i, i)
	}
	done := func(i int) string {
		return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, i, rMessage)
	}
	for _, tc := range []struct {
		name   string
		events []string
		want   string
	}{
		{"no item events", nil, `[]`},
		{"an item added and never done", []string{added(0)}, ""},
		{"two added and one done", []string{added(0), added(1), done(0)}, ""},
		{"a content part added", []string{`{"type":"response.content_part.added","output_index":0,"part":{"type":"output_text","text":""}}`}, ""},
		{"a text delta", []string{`{"type":"response.output_text.delta","output_index":0,"delta":"ki"}`}, ""},
		{"a reasoning event", []string{`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"x"}`}, ""},
		{"an item added and done", []string{added(0), done(0)}, `[` + rMessage + `]`},
	} {
		events := append(append([]string{}, quiet...), tc.events...)
		events = append(events, completedEvent(`[]`))
		msg, stop, err := Adapter{}.Build(200, "text/event-stream", []byte(replySSE(events...)))
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: kept as %s", tc.name, msg)
			}
			continue
		}
		if err != nil || stop != "completed" || string(msg) != tc.want {
			t.Errorf("%s: Build: %v %q %s, want %s", tc.name, err, stop, msg, tc.want)
		}
	}
}

func TestBuildPlainJSON(t *testing.T) {
	msg, stop, err := Adapter{}.Build(200, "application/json", []byte(`{"status":"completed","output":[`+rMessage+`]}`))
	if err != nil || stop != "completed" || string(msg) != `[`+rMessage+`]` {
		t.Fatalf("Build: %v %q %s", err, stop, msg)
	}
}

func TestUnanswered(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(rReasoning),
		json.RawMessage(`{"type":"web_search_call","id":"ws","status":"completed"}`),
		json.RawMessage(`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`),
		json.RawMessage(`{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"ls"}`)}
	out, asked, err := Unanswered(items, "not done")
	if err != nil || !asked || len(out) != 2 {
		t.Fatalf("%v %v %d", err, asked, len(out))
	}
	if string(out[0]) != `{"type":"function_call_output","call_id":"c1","output":"not done"}` ||
		string(out[1]) != `{"type":"custom_tool_call_output","call_id":"c2","output":"not done"}` {
		t.Fatalf("outputs: %s %s", out[0], out[1])
	}
	if _, asked, err := Unanswered(items[:2], "x"); err != nil || asked {
		t.Fatalf("a hosted call asked for a result: %v %v", asked, err)
	}
	for _, c := range []string{`{"type":"shell_call","call_id":"c"}`, `{"type":"local_shell_call","call_id":"c"}`,
		`{"type":"tool_search_call","call_id":"c","execution":"client"}`, `{"type":"function_call","name":"x"}`} {
		if _, _, err := Unanswered([]json.RawMessage{json.RawMessage(c)}, "x"); err == nil {
			t.Errorf("%s: a result was guessed", c)
		}
	}
}

func TestPromptAndContextByKinds(t *testing.T) {
	p := json.RawMessage(`{"type":"message","id":"a","role":"user","content":[{"type":"input_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}`)
	env := json.RawMessage(`{"type":"message","id":"b","role":"user","content":[{"type":"input_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]}}`)
	plain := json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	if !IsPrompt(p) || IsPrompt(env) || IsPrompt(plain) {
		t.Fatal("the prompt is not told by its kinds")
	}
	if _, _, ok := ContextKey(p); ok {
		t.Fatal("the prompt counted as context")
	}
	if _, _, ok := ContextKey(plain); ok {
		t.Fatal("a message without kinds counted as context")
	}
	k1, c1, ok1 := ContextKey(env)
	other := json.RawMessage(strings.Replace(string(env), `"id":"b"`, `"id":"z"`, 1))
	k2, c2, ok2 := ContextKey(other)
	if !ok1 || !ok2 || k1 != k2 || string(c1) != string(c2) {
		t.Fatal("the same context under another id is not the same")
	}
}

// A response that sets end_turn false outside its items: codex goes on after
// it, so the kept reply's stop reason says so.
func TestBuildCarriesResponseEndTurnFalse(t *testing.T) {
	stream := replySSE(`{"type":"response.completed","response":{"status":"completed","end_turn":false,"output":[` + rMessage + `]}}`)
	msg, stop, err := Adapter{}.Build(200, "text/event-stream", []byte(stream))
	if err != nil || stop != ContinuesStop || string(msg) != `[`+rMessage+`]` {
		t.Fatalf("Build: %v %q %s", err, stop, msg)
	}
	if _, stop, _ := (Adapter{}).Build(200, "application/json", []byte(`{"status":"completed","end_turn":true,"output":[`+rMessage+`]}`)); stop != "completed" {
		t.Fatalf("end_turn true gave %q", stop)
	}
}
