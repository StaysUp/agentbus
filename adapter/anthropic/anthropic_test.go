package anthropic

import (
	"bytes"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

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

func TestPromptStartsTurn(t *testing.T) {
	evs, err := Adapter{}.RequestEvents([]byte(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Bash"},{"name":"Read"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "hi" {
		t.Errorf("prompt = %q", evs[0].Text)
	}
	if evs[1].Model != "claude-x" || !reflect.DeepEqual(evs[1].ToolsOffered, []string{"Bash", "Read"}) {
		t.Errorf("model.requested = %+v", evs[1])
	}
}

func TestToolResultContinuesTurn(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"run ls"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"a.go"},
			{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"denied by no_timeout"}]},
			{"type":"text","text":"<system-reminder>x</system-reminder>"}]}]}`
	evs, err := Adapter{}.RequestEvents([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ToolCompleted, event.ToolCompleted, event.ModelRequested)
	if evs[0].ToolID != "t1" || evs[0].ToolOutput != "a.go" || *evs[0].ToolError {
		t.Errorf("first result = %+v", evs[0])
	}
	if evs[1].ToolID != "t2" || evs[1].ToolOutput != "denied by no_timeout" || !*evs[1].ToolError {
		t.Errorf("second result = %+v", evs[1])
	}
}

// The shape Claude Code 2.1.318 sends, seen through the gateway on 2026-11-04:
// a system message after the user's message.
func TestTrailingSystemMessage(t *testing.T) {
	start := `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"echo it"}]},
		{"role":"system","content":[{"type":"text","text":"reminder"},{"type":"tool_addition"}]}]}`
	evs, err := Adapter{}.RequestEvents([]byte(start))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "echo it" {
		t.Errorf("prompt = %q, the system message must not be part of it", evs[0].Text)
	}

	cont := `{"model":"m","messages":[
		{"role":"user","content":"echo it"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},
		{"role":"system","content":[{"type":"text","text":"reminder"}]}]}`
	evs, err = Adapter{}.RequestEvents([]byte(cont))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
}

func TestJSONReplies(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		kinds   []event.Kind
		outcome event.Outcome
	}{
		{"end_turn", `{"model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":7}}`,
			[]event.Kind{event.ModelResponded, event.TurnCompleted}, event.Finished},
		{"tool_use", `{"model":"m","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}],"stop_reason":"tool_use","usage":{}}`,
			[]event.Kind{event.ModelResponded, event.ToolRequested}, ""},
		{"max_tokens", `{"model":"m","content":[],"stop_reason":"max_tokens","usage":{}}`,
			[]event.Kind{event.ModelResponded, event.TurnCompleted}, event.Partial},
		{"refusal", `{"model":"m","content":[],"stop_reason":"refusal","usage":{}}`,
			[]event.Kind{event.ModelResponded, event.TurnCompleted}, event.Refused},
		{"pause_turn", `{"model":"m","content":[],"stop_reason":"pause_turn","usage":{}}`,
			[]event.Kind{event.ModelResponded}, ""},
		{"new reason", `{"model":"m","content":[],"stop_reason":"something_new","usage":{}}`,
			[]event.Kind{event.ModelResponded, event.TurnCompleted, event.SchemaUnexpected}, event.Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs, err := Adapter{}.ResponseEvents(200, "application/json", []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			wantKinds(t, evs, c.kinds...)
			for _, e := range evs {
				if e.Kind == event.TurnCompleted && e.Outcome != c.outcome {
					t.Errorf("outcome = %q, want %q", e.Outcome, c.outcome)
				}
			}
		})
	}

	evs, _ := Adapter{}.ResponseEvents(200, "application/json", []byte(cases[0].body))
	if u := evs[0].Usage; u.InputTokens != 10 || u.OutputTokens != 2 || u.CacheReadTokens != 7 {
		t.Errorf("usage = %+v", u)
	}
	evs, _ = Adapter{}.ResponseEvents(200, "application/json", []byte(cases[1].body))
	if evs[1].ToolName != "Bash" || string(evs[1].ToolInput) != `{"command":"ls"}` {
		t.Errorf("tool.requested = %+v", evs[1])
	}
}

const toolStream = `event: message_start
data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":120,"cache_read_input_tokens":100,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"check."}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"and\":\"go test\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamWithToolCall(t *testing.T) {
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream; charset=utf-8", []byte(toolStream))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	r := evs[0]
	if r.Text != "Let me check." || r.StopReason != "tool_use" || r.Error != "" || r.Model != "claude-x" {
		t.Errorf("model.responded = %+v", r)
	}
	if r.Usage.InputTokens != 120 || r.Usage.CacheReadTokens != 100 || r.Usage.OutputTokens != 42 {
		t.Errorf("usage = %+v", r.Usage)
	}
	if evs[1].ToolID != "toolu_1" || string(evs[1].ToolInput) != `{"command":"go test"}` {
		t.Errorf("tool.requested = %+v", evs[1])
	}
}

func TestStreamCutOffClaimsNoTurnEnd(t *testing.T) {
	cut := toolStream[:strings.Index(toolStream, "event: message_delta")]
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(cut))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == event.TurnCompleted {
			t.Fatal("a cut-off reply must not end the turn")
		}
	}
	if evs[0].Error == "" {
		t.Error("a cut-off reply must say so")
	}
}

func TestStreamErrorEvent(t *testing.T) {
	s := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{}}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Error != "Overloaded" {
		t.Errorf("error = %q", evs[0].Error)
	}
}

func TestHTTPError(t *testing.T) {
	evs, err := Adapter{}.ResponseEvents(529, "application/json", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Status != 529 || evs[0].Error != "Overloaded" {
		t.Errorf("got %+v", evs[0])
	}
	// AgentBus's relay refuses with its own shape: the text is kept.
	evs, err = Adapter{}.ResponseEvents(403, "application/json", []byte(`{"error":"no subscription is connected on control"}`))
	if err != nil || evs[0].Status != 403 || evs[0].Error != "no subscription is connected on control" {
		t.Errorf("relay refusal: %+v %v", evs, err)
	}
}

func TestWatches(t *testing.T) {
	a := Adapter{}
	if !adapter.Watches(a, "POST", "/v1/messages") {
		t.Error("should watch model calls")
	}
	for _, p := range []string{"/v1/messages/count_tokens", "/v1/models"} {
		if adapter.Watches(a, "POST", p) {
			t.Errorf("should not watch %s", p)
		}
	}
}

// A stream that never reached message_stop, or that carried an error,
// claims no turn end.
func TestStreamWithoutMessageStopOrWithErrorClaimsNoTurnEnd(t *testing.T) {
	base := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	for name, s := range map[string]string{
		"no message_stop": base,
		"error after stop reason": base + "data: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(s))
			if err != nil {
				t.Fatal(err)
			}
			wantKinds(t, evs, event.ModelResponded)
			if evs[0].Error == "" {
				t.Error("the problem must be named")
			}
		})
	}
}

// An assistant prefill after the user's words still starts a turn.
func TestAssistantPrefillStartsTurn(t *testing.T) {
	evs, err := Adapter{}.RequestEvents([]byte(`{"model":"m","messages":[{"role":"user","content":"Return JSON"},{"role":"assistant","content":"{"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "Return JSON" {
		t.Errorf("prompt = %q", evs[0].Text)
	}
}

// A tool call cut off mid-arguments is not reported as a tool call.
func TestCutToolCallIsNotReported(t *testing.T) {
	s := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Read\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n"
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelResponded)
}

// A complete reply whose tool input is not JSON reports the call without it.
func TestInvalidToolInputInCompleteReply(t *testing.T) {
	s := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Read\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	if evs[1].ToolInput != nil || evs[1].Error == "" {
		t.Errorf("tool.requested = %+v", evs[1])
	}
}

// Block indexes are keys, not slice positions.
func TestStreamIndexes(t *testing.T) {
	neg := "data: {\"type\":\"content_block_start\",\"index\":-1,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n"
	if _, err := (Adapter{}).ResponseEvents(200, "text/event-stream", []byte(neg)); err == nil {
		t.Error("a negative index must be an error")
	}

	sparse := "data: {\"type\":\"content_block_start\",\"index\":100000,\"content_block\":{\"type\":\"text\",\"text\":\"b\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"text\",\"text\":\"a\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	allocs := testing.AllocsPerRun(1, func() {
		evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(sparse))
		if err != nil || evs[0].Text != "a\nb" {
			t.Fatalf("evs=%+v err=%v", evs, err)
		}
	})
	if allocs > 2000 {
		t.Errorf("a sparse index cost %v allocations", allocs)
	}
}

// Text is built once, not copied per delta.
func TestManyTextDeltas(t *testing.T) {
	const n = 20000
	s := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		strings.Repeat("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"abcdefgh\"}}\n\n", n) +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	var ms1, ms2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms1)
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(s))
	runtime.ReadMemStats(&ms2)
	if err != nil || len(evs[0].Text) != 8*n {
		t.Fatalf("text length = %d, err = %v", len(evs[0].Text), err)
	}
	// Before the fix this input allocated about 1.7 GB.
	if got := ms2.TotalAlloc - ms1.TotalAlloc; got > 100<<20 {
		t.Errorf("allocated %d bytes for %d bytes of text", got, 8*n)
	}
}

// Resuming after pause_turn sends the paused assistant content back. That
// continues the turn; it does not start one.
func TestPauseTurnContinuationStartsNothing(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"search the web for X"},
		{"role":"assistant","content":[{"type":"text","text":"Searching."},{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"X"}}]}]}`
	evs, err := Adapter{}.RequestEvents([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelRequested)
}

// A vendor error is capped and copied, so an event never holds the whole
// reply.
func TestVendorErrorIsCapped(t *testing.T) {
	body := strings.Repeat(" ", 8<<20) + strings.Repeat("é", 2000)
	evs, err := Adapter{}.ResponseEvents(502, "text/plain", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	msg := evs[0].Error
	if len(msg) > maxError+len(" (cut)") || !utf8.ValidString(msg) || !strings.HasSuffix(msg, " (cut)") {
		t.Errorf("error is %d bytes, valid UTF-8 %v: %q…", len(msg), utf8.ValidString(msg), msg[:20])
	}
}

// A stream that ends before message_delta has only message_start's placeholder
// output count, so the count is marked incomplete. Input and cache counts come
// from message_start and are real.
func TestCutStreamMarksOutputCountIncomplete(t *testing.T) {
	cut := toolStream[:strings.Index(toolStream, "event: message_delta")]
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(cut))
	if err != nil {
		t.Fatal(err)
	}
	u := evs[0].Usage
	if !u.OutputIncomplete || u.OutputTokens != 1 || u.InputTokens != 120 || u.CacheReadTokens != 100 {
		t.Errorf("usage = %+v", u)
	}
	if evs[0].Text != "Let me check." {
		t.Errorf("partial text = %q", evs[0].Text)
	}

	evs, err = Adapter{}.ResponseEvents(200, "text/event-stream", []byte(toolStream))
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Usage.OutputIncomplete {
		t.Error("a stream with message_delta has its final output count")
	}
}

// message_delta without message_stop still carries the final count.
func TestStreamWithoutStopKeepsFinalCount(t *testing.T) {
	cut := toolStream[:strings.Index(toolStream, "event: message_stop")]
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(cut))
	if err != nil {
		t.Fatal(err)
	}
	if u := evs[0].Usage; u.OutputIncomplete || u.OutputTokens != 42 {
		t.Errorf("usage = %+v", u)
	}
}

// A reply without usage keeps Usage nil; explicit zeros stay zeros. Absent is
// not the same as zero.
func TestMissingUsageStaysMissing(t *testing.T) {
	for _, c := range []struct {
		name, ctype, body string
		wantNil           bool
	}{
		{"json without usage", "application/json", `{"model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`, true},
		{"json with zero usage", "application/json", `{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`, false},
		{"stream without usage", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", true},
	} {
		evs, err := Adapter{}.ResponseEvents(200, c.ctype, []byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := evs[0].Usage == nil; got != c.wantNil {
			t.Errorf("%s: usage nil = %v, want %v (usage %+v)", c.name, got, c.wantNil, evs[0].Usage)
		}
	}
}

// The shape Claude Code 2.1.318 sends as the first user message (seen through
// the gateway, 2026-11-05): two text blocks that are each one whole
// <system-reminder>, the second ending in a newline, then the person's words
// in a block of their own. turn.started keeps only the person's words.
func TestPromptTextIsThePersonsWords(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":[
			{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# userEmail\nThe user's email address is ana@example.com.\n</system-reminder>"},
			{"type":"text","text":"<system-reminder>\nAttribution for git commits: Ana Example\n</system-reminder>\n"},
			{"type":"text","text":"Say hi in three words."}]},
		{"role":"system","content":[{"type":"text","text":"# Environment","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	evs, err := Adapter{}.RequestEvents([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "Say hi in three words." {
		t.Errorf("prompt = %q, want only the person's words", evs[0].Text)
	}
}

// historyWith builds a request whose history holds one tool call and its
// result before the person's next message, so both are inside the turn basis.
func historyWith(toolInput, toolResultContent string) []byte {
	return []byte(`{"model":"m","messages":[
	{"role":"user","content":"first"},
	{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Fetch","input":` + toolInput + `}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + toolResultContent + `}]},
	{"role":"assistant","content":[{"type":"text","text":"done"}]},
	{"role":"user","content":"second"}]}`)
}

// A key named cache_control inside a tool's input is the application's data,
// not a cache marker, so it must change the turn.
func TestTurnBasisKeepsCacheControlInsideToolInput(t *testing.T) {
	a := Adapter{}.TurnBasis(historyWith(`{"cache_control":"no-cache"}`, `"ok"`))
	b := Adapter{}.TurnBasis(historyWith(`{"cache_control":"max-age=3600"}`, `"ok"`))
	if a == nil || bytes.Equal(a, b) {
		t.Errorf("different tool inputs gave the same basis:\n%s", a)
	}
}

// A tool_result's content may be a string or a list of blocks; both forms
// of the same result are the same turn.
func TestTurnBasisToolResultStringOrBlocks(t *testing.T) {
	a := Adapter{}.TurnBasis(historyWith(`{}`, `"ok"`))
	b := Adapter{}.TurnBasis(historyWith(`{}`, `[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]`))
	if a == nil || !bytes.Equal(a, b) {
		t.Errorf("string and block forms of one tool result differ:\n%s\n%s", a, b)
	}
}

// usage.cache_creation splits cache_creation_input_tokens by lifetime. Claude
// Code 2.1.318 writes one-hour cache (seen 2026-11-05), and the split is kept
// because each lifetime has its own price. A reply without cache_creation
// leaves the split zero: the lifetime is unknown.
func TestCacheWriteLifetimesKept(t *testing.T) {
	for _, c := range []struct {
		name, ctype, body string
		want              event.Usage
	}{
		{"json", "application/json",
			`{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5,` +
				`"cache_read_input_tokens":100,"cache_creation_input_tokens":700,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":500}}}`,
			event.Usage{InputTokens: 3, OutputTokens: 5, CacheReadTokens: 100, CacheWriteTokens: 700,
				CacheWrite5mTokens: 200, CacheWrite1hTokens: 500}},
		{"json without split", "application/json",
			`{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5,"cache_creation_input_tokens":700}}`,
			event.Usage{InputTokens: 3, OutputTokens: 5, CacheWriteTokens: 700}},
		{"stream", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1," +
				"\"cache_creation_input_tokens\":900,\"cache_creation\":{\"ephemeral_5m_input_tokens\":0,\"ephemeral_1h_input_tokens\":900}}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			event.Usage{InputTokens: 3, OutputTokens: 8, CacheWriteTokens: 900, CacheWrite1hTokens: 900}},
		{"stream with split in message_delta", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8," +
				"\"cache_creation_input_tokens\":40,\"cache_creation\":{\"ephemeral_5m_input_tokens\":40,\"ephemeral_1h_input_tokens\":0}}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			event.Usage{InputTokens: 3, OutputTokens: 8, CacheWriteTokens: 40, CacheWrite5mTokens: 40}},
	} {
		evs, err := Adapter{}.ResponseEvents(200, c.ctype, []byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if u := evs[0].Usage; u == nil || !reflect.DeepEqual(*u, c.want) {
			t.Errorf("%s: usage = %+v, want %+v", c.name, u, c.want)
		}
	}
}

// usage.service_tier and usage.speed say how the call was served. Fast mode
// reports speed "fast". In a stream either may arrive in message_start or in
// message_delta (LiteLLM's stream assembly reads speed from both).
func TestServiceTierAndSpeedRecorded(t *testing.T) {
	for _, c := range []struct {
		name, ctype, body string
		tier, speed       string
	}{
		{"json", "application/json",
			`{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5,"service_tier":"standard","speed":"fast"}}`,
			"standard", "fast"},
		{"json without either", "application/json",
			`{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`,
			"", ""},
		{"stream, both in message_start", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"service_tier\":\"standard\",\"speed\":\"fast\"}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			"standard", "fast"},
		{"stream, both in message_delta", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8,\"service_tier\":\"standard\",\"speed\":\"fast\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			"standard", "fast"},
	} {
		evs, err := Adapter{}.ResponseEvents(200, c.ctype, []byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if u := evs[0].Usage; u == nil || u.ServiceTier != c.tier || u.Speed != c.speed {
			t.Errorf("%s: usage = %+v, want tier %q speed %q", c.name, u, c.tier, c.speed)
		}
	}
}

// usage.server_tool_use counts the tools Anthropic ran itself, and
// usage.inference_geo says where the call ran ("global" on Claude Code
// 2.1.318 replies, seen 2026-11-05 in message_start's usage). Both are kept.
// In a stream the counts may come in message_delta.
func TestServerToolUseAndGeoRecorded(t *testing.T) {
	for _, c := range []struct {
		name, ctype, body string
		geo               string
		tools             map[string]int
	}{
		{"json", "application/json",
			`{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5,"inference_geo":"us",` +
				`"server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}`,
			"us", map[string]int{"web_search_requests": 2, "web_fetch_requests": 1}},
		{"json without either", "application/json",
			`{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`,
			"", nil},
		{"stream", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"inference_geo\":\"global\"}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8,\"server_tool_use\":{\"web_search_requests\":1}}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			"global", map[string]int{"web_search_requests": 1}},
	} {
		evs, err := Adapter{}.ResponseEvents(200, c.ctype, []byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		u := evs[0].Usage
		if u == nil || u.InferenceGeo != c.geo || !reflect.DeepEqual(u.ServerToolUse, c.tools) {
			t.Errorf("%s: usage = %+v, want geo %q tools %v", c.name, u, c.geo, c.tools)
		}
	}
}
