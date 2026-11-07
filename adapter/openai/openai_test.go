package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

var _ adapter.Adapter = Adapter{}

// The fixtures are codex-cli 0.164.0 traffic to
// https://chatgpt.com/backend-api/codex/responses, captured 2026-11-05 with a
// ChatGPT sign-in and model gpt-6.1-sol. Long instructions and tool
// descriptions are trimmed and the installation id and safety identifier are
// replaced; every field the adapter reads is as captured.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func kinds(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func wantKinds(t *testing.T, evs []event.Event, want ...event.Kind) {
	t.Helper()
	if got := kinds(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func requestEvents(t *testing.T, body []byte) []event.Event {
	t.Helper()
	evs, err := Adapter{}.RequestEvents(body)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// The captured replies came with no Content-Type at all, so the tests pass "".
func reply(t *testing.T, status int, ctype string, body []byte) []event.Event {
	t.Helper()
	evs, err := Adapter{}.ResponseEvents(status, ctype, body)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

var codexTools = []string{"exec", "wait", "request_user_input", "request_user_input_async", "sleep",
	"followup_task", "interrupt_agent", "list_agents", "send_message", "spawn_agent", "wait_agent"}

func TestWatches(t *testing.T) {
	a := Adapter{}
	if a.Vendor() != "openai" {
		t.Errorf("vendor = %q", a.Vendor())
	}
	if !a.Watches("POST", "/backend-api/codex/responses") {
		t.Error("should watch codex's model calls")
	}
	for _, c := range [][2]string{
		{"GET", "/backend-api/codex/responses"}, // the WebSocket upgrade
		{"GET", "/backend-api/codex/models"},
		{"POST", "/backend-api/codex/responses/compact"},
	} {
		if a.Watches(c[0], c[1]) {
			t.Errorf("should not watch %s %s", c[0], c[1])
		}
	}
}

func TestCapturedPlainTurn(t *testing.T) {
	evs := requestEvents(t, fixture(t, "plain_request.json"))
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "Reply with exactly the word: pong" {
		t.Errorf("prompt = %q", evs[0].Text)
	}
	if evs[1].Model != "gpt-6.1-sol" || !reflect.DeepEqual(evs[1].ToolsOffered, codexTools) {
		t.Errorf("model.requested = %+v", evs[1])
	}

	evs = reply(t, 200, "", fixture(t, "plain_response.sse"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	r := evs[0]
	if r.Text != "pong" || r.Model != "gpt-6.1-sol" || r.StopReason != "completed" || r.Error != "" || r.Status != 200 {
		t.Errorf("model.responded = %+v", r)
	}
	want := event.Usage{InputTokens: 18349 - 12288, OutputTokens: 5, CacheReadTokens: 12288, ServiceTier: "default"}
	if r.Usage == nil || !reflect.DeepEqual(*r.Usage, want) {
		t.Errorf("usage = %+v, want %+v", r.Usage, want)
	}
	if evs[1].Outcome != event.Finished || evs[1].StopReason != "completed" {
		t.Errorf("turn.completed = %+v", evs[1])
	}
}

func TestCapturedToolTurn(t *testing.T) {
	const callID = "call_1269105a62d84323a056e461cfb64e52"
	const code = "const r = await tools.exec_command({cmd:\"echo hello123\",max_output_tokens:100});text(r.output);\n"

	evs := requestEvents(t, fixture(t, "tool_request_1.json"))
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)

	evs = reply(t, 200, "", fixture(t, "tool_response_1.sse"))
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	if u := evs[0].Usage; u == nil || u.InputTokens != 18358-12288 || u.OutputTokens != 34 || u.CacheReadTokens != 12288 {
		t.Errorf("usage = %+v", u)
	}
	tr := evs[1]
	var input string
	if err := json.Unmarshal(tr.ToolInput, &input); err != nil || input != code {
		t.Errorf("tool input = %s (%v), want the code as a JSON string", tr.ToolInput, err)
	}
	if tr.ToolID != callID || tr.ToolName != "exec" || tr.Error != "" {
		t.Errorf("tool.requested = %+v", tr)
	}

	evs = requestEvents(t, fixture(t, "tool_request_2.json"))
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
	tc := evs[0]
	if tc.ToolID != callID || tc.ToolOutput != "Script completed\nWall time 0.2 seconds\nOutput:\nhello123\n" {
		t.Errorf("tool.completed = %+v", tc)
	}
	if tc.ToolError != nil {
		t.Errorf("the Responses API sends no error flag, so none is claimed; got %v", *tc.ToolError)
	}

	evs = reply(t, 200, "", fixture(t, "tool_response_2.sse"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Text != "hello123" || evs[1].Outcome != event.Finished {
		t.Errorf("events = %+v", evs)
	}
	if u := evs[0].Usage; u == nil || u.InputTokens != 18418-18176 || u.CacheReadTokens != 18176 || u.OutputTokens != 6 {
		t.Errorf("usage = %+v", u)
	}
}

// A function call, the Responses API's other tool shape: arguments arrive as a
// JSON string, and the output as a plain string.
func TestFunctionCall(t *testing.T) {
	body := `{"model":"m","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"}],"usage":{"input_tokens":3,"output_tokens":1}}`
	evs := reply(t, 200, "application/json", []byte(body))
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	if evs[1].ToolID != "c1" || evs[1].ToolName != "shell" || string(evs[1].ToolInput) != `{"cmd":"ls"}` {
		t.Errorf("tool.requested = %+v", evs[1])
	}

	req := `{"model":"m","input":[{"type":"message","role":"user","content":"list"},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"a.go"},
		{"type":"function_call_output","call_id":"c2","output":"b.go"}],
		"tools":[{"type":"function","name":"shell"}]}`
	evs = requestEvents(t, []byte(req))
	wantKinds(t, evs, event.ToolCompleted, event.ToolCompleted, event.ModelRequested)
	if evs[0].ToolOutput != "a.go" || evs[1].ToolID != "c2" || !reflect.DeepEqual(evs[2].ToolsOffered, []string{"shell"}) {
		t.Errorf("events = %+v", evs)
	}
}

func TestCutStreamClaimsNoTurnEnd(t *testing.T) {
	full := string(fixture(t, "plain_response.sse"))
	cut := full[:strings.Index(full, "event: response.completed")]
	evs := reply(t, 200, "", []byte(cut))
	wantKinds(t, evs, event.ModelResponded)
	r := evs[0]
	if r.Error == "" {
		t.Error("a cut stream must say so")
	}
	if r.Text != "pong" || r.Model != "gpt-6.1-sol" {
		t.Errorf("partial reply = %+v", r)
	}
	// Usage arrives only in the final event, so a cut stream has no counts.
	// Zeros would claim a known zero input; absent keeps the cost incomplete.
	if r.Usage != nil {
		t.Errorf("usage = %+v, want nil", r.Usage)
	}

	evs = reply(t, 200, "", []byte(full))
	if evs[0].Usage.OutputIncomplete {
		t.Error("a stream with response.completed has its final count")
	}
}

func TestMissingUsageStaysMissing(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\",\"status\":\"in_progress\",\"usage\":null}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":null}}\n\n"
	for _, c := range []struct {
		name, ctype, body string
		wantNil           bool
	}{
		{"json without usage", "application/json", `{"model":"m","status":"completed","output":[]}`, true},
		{"json with zero usage", "application/json", `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`, false},
		{"stream with null usage", "text/event-stream", stream, true},
	} {
		evs := reply(t, 200, c.ctype, []byte(c.body))
		if got := evs[0].Usage == nil; got != c.wantNil {
			t.Errorf("%s: usage nil = %v, want %v (usage %+v)", c.name, got, c.wantNil, evs[0].Usage)
		}
	}
}

// OpenAI's input_tokens include the cached tokens; the framework's do not.
func TestCachedTokensSubtracted(t *testing.T) {
	body := `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":5},"output_tokens":3}}`
	evs := reply(t, 200, "application/json", []byte(body))
	want := event.Usage{InputTokens: 20, OutputTokens: 3, CacheReadTokens: 80, CacheWriteTokens: 5}
	if u := evs[0].Usage; u == nil || !reflect.DeepEqual(*u, want) {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestErrorStatuses(t *testing.T) {
	evs := reply(t, 429, "application/json", []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Status != 429 || evs[0].Error != "The usage limit has been reached" {
		t.Errorf("got %+v", evs[0])
	}

	failed := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"model\":\"m\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"Something went wrong\"},\"output\":[],\"usage\":null}}\n\n"
	evs = reply(t, 200, "", []byte(failed))
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Error != "Something went wrong" || evs[0].StopReason != "failed" {
		t.Errorf("got %+v", evs[0])
	}

	streamErr := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"Slow down\"}\n\n"
	evs = reply(t, 200, "", []byte(streamErr))
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Error != "Slow down" {
		t.Errorf("got %+v", evs[0])
	}
}

func TestStatusOutcomes(t *testing.T) {
	for _, c := range []struct {
		name, body string
		outcome    event.Outcome
		stop       string
	}{
		{"max output tokens", `{"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, event.Partial, "incomplete:max_output_tokens"},
		{"content filter", `{"model":"m","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`, event.Refused, "incomplete:content_filter"},
		{"new incomplete reason", `{"model":"m","status":"incomplete","incomplete_details":{"reason":"something_new"},"output":[]}`, event.Unknown, "incomplete:something_new"},
		{"cancelled", `{"model":"m","status":"cancelled","output":[]}`, event.Unknown, "cancelled"},
		{"new status", `{"model":"m","status":"something_new","output":[]}`, event.Unknown, "something_new"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A new status or reason is also reported as schema.unexpected;
			// unexpected_test.go checks that.
			evs := withoutUnexpected(reply(t, 200, "application/json", []byte(c.body)))
			wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
			if evs[1].Outcome != c.outcome || evs[1].StopReason != c.stop {
				t.Errorf("turn.completed = %+v, want %s / %s", evs[1], c.outcome, c.stop)
			}
		})
	}
}

func basis(t *testing.T, body []byte) []byte {
	t.Helper()
	b := Adapter{}.TurnBasis(body)
	if b == nil {
		t.Fatal("no turn basis")
	}
	return b
}

// A tool-output request names the same turn as the request that started it.
// From the captures: tool_request_2 repeats the first seven items of
// tool_request_1 byte for byte, then adds the call and its output.
func TestTurnBasisSameForToolOutputRequest(t *testing.T) {
	first := basis(t, fixture(t, "tool_request_1.json"))
	second := basis(t, fixture(t, "tool_request_2.json"))
	if !bytes.Equal(first, second) {
		t.Errorf("the tool-output request names another turn:\n%s\n%s", first, second)
	}

	// The basis ends at the real prompt, not at codex's AGENTS.md message,
	// which is also a user message and comes just before it.
	var items []map[string]any
	if err := json.Unmarshal(first, &items); err != nil {
		t.Fatal(err)
	}
	last, _ := json.Marshal(items[len(items)-1]["content"])
	if len(items) != 7 || !strings.Contains(string(last), "Run the shell command: echo hello123") {
		t.Errorf("basis has %d items, last content %s", len(items), last)
	}
}

// The next prompt in the same conversation starts a new turn.
func TestTurnBasisNewForNextPrompt(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal(fixture(t, "tool_request_2.json"), &req); err != nil {
		t.Fatal(err)
	}
	req["input"] = append(req["input"].([]any),
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hello123"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "now say bye"}}})
	next, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(basis(t, fixture(t, "tool_request_2.json")), basis(t, next)) {
		t.Error("the next prompt must name a new turn")
	}
}

// Key order and spacing do not change the basis; a request with no person's
// words names no turn.
func TestTurnBasisCanonical(t *testing.T) {
	a := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	b := `{"input":[ {"content":[{"text":"hi","type":"input_text"}], "role":"user","type":"message"} ],"model":"m2"}`
	if !bytes.Equal(basis(t, []byte(a)), basis(t, []byte(b))) {
		t.Errorf("basis depends on key order or spacing: %s vs %s", basis(t, []byte(a)), basis(t, []byte(b)))
	}
	none := `{"model":"m","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]}]}`
	if got := (Adapter{}).TurnBasis([]byte(none)); got != nil {
		t.Errorf("basis = %s, want nil", got)
	}
}

// A request carries every earlier call and output of the turn. Only the
// outputs after the last model output are new.
func TestOnlyNewToolOutputsReported(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal(fixture(t, "tool_request_2.json"), &req); err != nil {
		t.Fatal(err)
	}
	req["input"] = append(req["input"].([]any),
		map[string]any{"type": "custom_tool_call", "call_id": "call_2", "name": "exec", "input": "text('again')", "status": "completed"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_2", "output": []any{map[string]any{"type": "input_text", "text": "again"}}})
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	evs := requestEvents(t, body)
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
	if evs[0].ToolID != "call_2" || evs[0].ToolOutput != "again" {
		t.Errorf("tool.completed = %+v", evs[0])
	}
}

// OpenAI documents a refusal as status "completed" with a content part
// {"type":"refusal","refusal":"..."}, streamed as response.refusal.delta
// (structured-outputs guide, read 2026-11-05).
const refusalText = "I'm sorry, I cannot assist with that request."

func TestRefusal(t *testing.T) {
	msg := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"` + refusalText + `"}]}`
	final := `{"model":"m","status":"completed","output":[` + msg + `],"usage":{"input_tokens":10,"output_tokens":9}}`
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\",\"status\":\"in_progress\",\"usage\":null}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"refusal\",\"refusal\":\"\"}}\n\n" +
		"event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"I'm sorry, I cannot \"}\n\n" +
		"event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"assist with that request.\"}\n\n" +
		"event: response.refusal.done\ndata: {\"type\":\"response.refusal.done\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"refusal\":\"" + refusalText + "\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + msg + "}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + final + "}\n\n"
	for _, c := range []struct{ name, ctype, body string }{
		{"json", "application/json", final},
		{"sse", "text/event-stream", stream},
	} {
		t.Run(c.name, func(t *testing.T) {
			evs := reply(t, 200, c.ctype, []byte(c.body))
			wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
			if evs[0].Text != refusalText {
				t.Errorf("text = %q", evs[0].Text)
			}
			if evs[1].Outcome != event.Refused {
				t.Errorf("outcome = %q, want refused", evs[1].Outcome)
			}
		})
	}

	// A stream cut before response.completed keeps the refusal text so far.
	cut := stream[:strings.Index(stream, "event: response.refusal.done")]
	evs := reply(t, 200, "text/event-stream", []byte(cut))
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Text != refusalText || evs[0].Error == "" {
		t.Errorf("cut refusal = %+v", evs[0])
	}
}

// The new outputs are every output after the model's last item, whatever
// else comes between; earlier outputs are not new.
func TestToolOutputsAfterLastModelItem(t *testing.T) {
	const head = `{"type":"message","role":"user","content":"run it"},` +
		`{"type":"reasoning","id":"rs_1","summary":[]},` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},`
	for _, c := range []struct {
		name, items string
		want        []event.Kind
		ids         []string
	}{
		{"context after the output",
			head + `{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"message","role":"developer","content":"note"}`,
			[]event.Kind{event.ToolCompleted, event.ModelRequested}, []string{"c1"}},
		{"user follow-up after the output",
			head + `{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"message","role":"user","content":"next prompt"}`,
			[]event.Kind{event.ToolCompleted, event.TurnStarted, event.ModelRequested}, []string{"c1"}},
		{"parallel calls",
			head + `{"type":"function_call","call_id":"c2","name":"shell","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"c1","output":"a"},{"type":"function_call_output","call_id":"c2","output":"b"}`,
			[]event.Kind{event.ToolCompleted, event.ToolCompleted, event.ModelRequested}, []string{"c1", "c2"}},
		{"next turn",
			head + `{"type":"function_call_output","call_id":"c1","output":"ok"},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"message","role":"user","content":"again"}`,
			[]event.Kind{event.TurnStarted, event.ModelRequested}, nil},
		// The Responses API lets an input message omit type.
		{"next turn, assistant message without type",
			head + `{"type":"function_call_output","call_id":"c1","output":"ok"},` +
				`{"role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"message","role":"user","content":"again"}`,
			[]event.Kind{event.TurnStarted, event.ModelRequested}, nil},
		// After compaction the retained prompt is followed by a compaction
		// item, which the model produced; it is not a new turn.
		{"compaction after the retained prompt",
			`{"type":"message","role":"user","content":"run it"},{"type":"compaction","id":"cmp_1","encrypted_content":"x"}`,
			[]event.Kind{event.ModelRequested}, nil},
		{"a built-in tool call is model output",
			`{"type":"message","role":"user","content":"look it up"},{"type":"web_search_call","id":"ws_1","status":"completed"}`,
			[]event.Kind{event.ModelRequested}, nil},
		{"developer after an assistant message without type",
			head + `{"type":"function_call_output","call_id":"c1","output":"ok"},` +
				`{"role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"message","role":"developer","content":"note"}`,
			[]event.Kind{event.ModelRequested}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			evs := requestEvents(t, []byte(`{"model":"m","input":[`+c.items+`]}`))
			wantKinds(t, evs, c.want...)
			var ids []string
			for _, e := range evs {
				if e.Kind == event.ToolCompleted {
					ids = append(ids, e.ToolID)
				}
			}
			if !reflect.DeepEqual(ids, c.ids) {
				t.Errorf("tool ids = %v, want %v", ids, c.ids)
			}
		})
	}
}

// A person item after the model's last item starts a turn, even when
// context follows it.
func TestTrailingContextDoesNotHideTurnStart(t *testing.T) {
	evs := requestEvents(t, []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"run it"},{"type":"message","role":"developer","content":"context"}]}`))
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "run it" {
		t.Errorf("prompt = %q", evs[0].Text)
	}

	evs = requestEvents(t, []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"run it"},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"message","role":"developer","content":"context"}]}`))
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
}

// RequestEvents and TurnBasis agree: turn.started exactly when the basis's
// starting item comes after the model's last item.
func TestTurnStartAgreesWithTurnBasis(t *testing.T) {
	bodies := map[string][]byte{}
	for _, f := range []string{"plain_request.json", "tool_request_1.json", "tool_request_2.json"} {
		bodies[f] = fixture(t, f)
	}
	for i, s := range []string{
		`[{"type":"message","role":"user","content":"run it"},{"type":"message","role":"developer","content":"context"}]`,
		`[{"type":"message","role":"user","content":"a"},{"type":"function_call","call_id":"c1","name":"x","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"message","role":"developer","content":"d"}]`,
		`[{"type":"message","role":"user","content":"a"},{"type":"function_call","call_id":"c1","name":"x","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"message","role":"user","content":"b"}]`,
		`[{"type":"message","role":"user","content":"a"},{"type":"message","role":"assistant","content":"done"},{"type":"message","role":"user","content":"b"},{"type":"additional_tools","role":"developer","tools":[]}]`,
		`[{"type":"message","role":"developer","content":"rules"}]`,
	} {
		bodies[fmt.Sprintf("case %d", i)] = []byte(`{"model":"m","input":` + s + `}`)
	}
	for name, body := range bodies {
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		lastModel := -1
		for i, it := range req.Input {
			if isModelItem(it) {
				lastModel = i
			}
		}
		start := -1
		if b := (Adapter{}).TurnBasis(body); b != nil {
			var items []json.RawMessage
			if err := json.Unmarshal(b, &items); err != nil {
				t.Fatal(err)
			}
			start = len(items) - 1
		}
		started := false
		for _, e := range requestEvents(t, body) {
			if e.Kind == event.TurnStarted {
				started = true
			}
		}
		if want := start >= 0 && start > lastModel; started != want {
			t.Errorf("%s: turn.started = %v, but basis start %d and last model item %d", name, started, start, lastModel)
		}
	}
}

// Codex compacts its history through the Responses stream; a completed
// response holding a compaction item is not the turn's end.
func TestCompactionKeepsTurnOpen(t *testing.T) {
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[{"type":"compaction","id":"cmp_1","encrypted_content":"x"}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	wantKinds(t, evs, event.ModelResponded)
}

// An end_turn of false on an output message means codex continues.
func TestEndTurnFalseKeepsTurnOpen(t *testing.T) {
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[{"type":"message","role":"assistant","end_turn":false,"content":[{"type":"output_text","text":"working"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	wantKinds(t, evs, event.ModelResponded)
}

// A tool_search_call carries its arguments as an object, which must not stop
// parsing of the response or of later requests that keep the item.
func TestToolSearchArgumentsObject(t *testing.T) {
	item := `{"type":"tool_search_call","call_id":"ts_1","arguments":{"query":"files"}}`
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[`+item+`],"usage":{"input_tokens":10,"output_tokens":2}}`))
	if len(evs) == 0 || evs[0].Kind != event.ModelResponded || evs[0].Error != "" {
		t.Fatalf("reply events = %+v", evs)
	}
	req := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"find"},` + item + `,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"found"}]},{"type":"message","role":"user","content":"next"}]}`)
	wantKinds(t, requestEvents(t, req), event.TurnStarted, event.ModelRequested)
	if (Adapter{}).TurnBasis(req) == nil {
		t.Error("TurnBasis returned nil for a request that keeps a tool_search_call")
	}
}

// A tool call the client runs (tool_search_call with execution
// "client") is a tool request and keeps the turn open; its
// tool_search_output in the next request is a tool result.
func TestClientToolSearch(t *testing.T) {
	call := `{"type":"tool_search_call","call_id":"ts_1","execution":"client","arguments":{"query":"files"}}`
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[`+call+`],"usage":{"input_tokens":10,"output_tokens":2}}`))
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	if evs[1].ToolID != "ts_1" || string(evs[1].ToolInput) != `{"query":"files"}` {
		t.Errorf("tool.requested = %+v", evs[1])
	}
	req := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"find"},` + call +
		`,{"type":"tool_search_output","call_id":"ts_1","execution":"client","tools":[{"type":"function","name":"read"}]}]}`)
	got := requestEvents(t, req)
	wantKinds(t, got, event.ToolCompleted, event.ModelRequested)
	if got[0].ToolID != "ts_1" {
		t.Errorf("tool.completed = %+v", got[0])
	}
}

// A stream event whose delta is an object (hosted shell output) must
// not stop parsing of the rest of the stream.
func TestObjectDeltaDoesNotStopStream(t *testing.T) {
	s := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\",\"status\":\"in_progress\",\"output\":[],\"usage\":null}}\n\n" +
		"event: response.shell_call_output_content.delta\ndata: {\"type\":\"response.shell_call_output_content.delta\",\"output_index\":0,\"delta\":{\"stdout\":\"hi\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"
	evs := reply(t, 200, "text/event-stream", []byte(s))
	// The hosted shell event is not one this adapter knows, so it is reported.
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted, event.SchemaUnexpected)
	if evs[0].Text != "done" || evs[0].Usage == nil {
		t.Errorf("model.responded = %+v", evs[0])
	}
}

// A tool_search_output's result is in tools; it becomes the tool result,
// and the loaded tools count as offered.
func TestToolSearchOutputTools(t *testing.T) {
	req := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"find"},` +
		`{"type":"tool_search_call","call_id":"ts_1","execution":"client","arguments":{"query":"files"}},` +
		`{"type":"tool_search_output","call_id":"ts_1","execution":"client","tools":[{"type":"function","name":"read_file"}]}]}`)
	evs := requestEvents(t, req)
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
	if !strings.Contains(evs[0].ToolOutput, "read_file") {
		t.Errorf("tool output = %q, want the loaded tools", evs[0].ToolOutput)
	}
	if !reflect.DeepEqual(evs[1].ToolsOffered, []string{"read_file"}) {
		t.Errorf("tools offered = %v, want [read_file]", evs[1].ToolsOffered)
	}
}

// Tool definitions from a tool search stay whole, and a structured
// output the text read cannot use is kept as JSON.
func TestToolResultsKeepTheirData(t *testing.T) {
	search := requestEvents(t, []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"go"},`+
		`{"type":"tool_search_call","call_id":"ts_1","execution":"client","arguments":{}},`+
		`{"type":"tool_search_output","call_id":"ts_1","execution":"client","tools":[{"type":"function","name":"get_eta","description":"ETA","parameters":{"type":"object"}}]}]}`))
	wantKinds(t, search, event.ToolCompleted, event.ModelRequested)
	if !strings.Contains(search[0].ToolOutput, `"parameters"`) || !strings.Contains(search[0].ToolOutput, `"description"`) {
		t.Errorf("tool search output lost the definitions: %s", search[0].ToolOutput)
	}
	shell := requestEvents(t, []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"go"},`+
		`{"type":"shell_call","call_id":"sh_1","execution":"client","action":{"commands":["ls"]}},`+
		`{"type":"shell_call_output","call_id":"sh_1","output":[{"stdout":"a.txt\n","stderr":"","outcome":{"type":"exit","exit_code":0}}]}]}`))
	wantKinds(t, shell, event.ToolCompleted, event.ModelRequested)
	if !strings.Contains(shell[0].ToolOutput, "a.txt") {
		t.Errorf("shell output = %q, want its stdout kept", shell[0].ToolOutput)
	}
}

// Codex reads end_turn on the response object too.
func TestResponseEndTurnFalseKeepsTurnOpen(t *testing.T) {
	body := `{"model":"m","status":"completed","end_turn":false,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
	wantKinds(t, reply(t, 200, "application/json", []byte(body)), event.ModelResponded)
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
	wantKinds(t, reply(t, 200, "text/event-stream", []byte(sse)), event.ModelResponded)
}

// service_tier is recorded from the final response. response.created and
// response.in_progress echo the request's "auto"; response.completed says
// which tier served the call (codex 0.164.0 streams, captured). codex's fast
// tier comes back as "priority".
func TestServiceTierRecorded(t *testing.T) {
	evs := reply(t, 200, "text/event-stream", fixture(t, "plain_response.sse"))
	if u := evs[0].Usage; u == nil || u.ServiceTier != "default" {
		t.Errorf("captured stream: usage %+v, want service_tier default", u)
	}
	body := `{"model":"m","status":"completed","service_tier":"priority","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	evs = reply(t, 200, "application/json", []byte(body))
	if u := evs[0].Usage; u == nil || u.ServiceTier != "priority" {
		t.Errorf("json: usage %+v, want service_tier priority", u)
	}
}

// Hosted tools OpenAI runs itself (web_search_call and the like) are counted
// by item type, so cost can see them. Calls the client runs are not.
func TestHostedToolCallsCounted(t *testing.T) {
	body := `{"model":"m","status":"completed","service_tier":"default","output":[` +
		`{"type":"web_search_call","id":"ws_1","status":"completed"},{"type":"web_search_call","id":"ws_2","status":"completed"},` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"tool_search_call","call_id":"c2","execution":"client","arguments":{}}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	evs := reply(t, 200, "application/json", []byte(body))
	if u := evs[0].Usage; u == nil || !reflect.DeepEqual(u.ServerToolUse, map[string]int{"web_search_call": 2}) {
		t.Errorf("usage = %+v, want two web_search_call", u)
	}
}
