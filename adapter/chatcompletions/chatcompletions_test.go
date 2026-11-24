package chatcompletions

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

var _ adapter.Adapter = Adapter{}

// The fixtures are llama.cpp llama-server b12578 traffic, captured 2026-11-20
// with Qwen2.5-0.5B-Instruct (Q4_K_M, --jinja, --alias qwen2.5-0.5b-instruct,
// temperature 0): each *_request.json is the body posted to
// /v1/chat/completions, each reply the body as it came back. Nothing is
// trimmed. tool_request_2.json sends the result of the call in
// tool_response_1.sse.

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

func reply(t *testing.T, status int, ctype string, body []byte) []event.Event {
	t.Helper()
	evs, err := Adapter{}.ResponseEvents(status, ctype, body)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func wantUsage(t *testing.T, got *event.Usage, want event.Usage) {
	t.Helper()
	if got == nil {
		t.Fatal("usage is nil")
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("usage = %+v, want %+v", *got, want)
	}
}

func TestWatches(t *testing.T) {
	a := Adapter{}
	if a.Vendor() != "chatcompletions" {
		t.Errorf("vendor = %q", a.Vendor())
	}
	if !adapter.Watches(a, "POST", "/v1/chat/completions") {
		t.Error("should watch the model call")
	}
	for _, c := range [][2]string{
		{"GET", "/v1/chat/completions"},
		{"POST", "/v1/completions"},
		{"POST", "/v1/responses"},
		{"POST", "/v1/chat/completions/x"},
	} {
		if adapter.Watches(a, c[0], c[1]) {
			t.Errorf("should not watch %s %s", c[0], c[1])
		}
	}
}

func TestPlainRequestStartsATurn(t *testing.T) {
	evs := requestEvents(t, fixture(t, "plain_request.json"))
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if evs[0].Text != "Say hello in three words." {
		t.Errorf("prompt = %q", evs[0].Text)
	}
	if evs[1].Model != "qwen2.5-0.5b-instruct" || len(evs[1].ToolsOffered) != 0 {
		t.Errorf("model.requested = %+v", evs[1])
	}
}

func TestPlainStreamedReply(t *testing.T) {
	evs := reply(t, 200, "text/event-stream", fixture(t, "plain_response.sse"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	r := evs[0]
	if r.Text != "Hello!" || r.StopReason != "stop" || r.Model != "qwen2.5-0.5b-instruct" || r.Error != "" || r.Status != 200 {
		t.Fatalf("model.responded = %+v", r)
	}
	wantUsage(t, r.Usage, event.Usage{InputTokens: 25, OutputTokens: 3})
	if evs[1].Outcome != event.Finished || evs[1].StopReason != "stop" {
		t.Fatalf("turn.completed = %+v", evs[1])
	}
}

// llama-server reused 24 of the 25 prompt tokens from its cache: they are
// cache reads, and the input tokens are the rest.
func TestWholeReplyCountsCachedTokensAsCacheReads(t *testing.T) {
	evs := reply(t, 200, "application/json; charset=utf-8", fixture(t, "whole_response.json"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Text != "Hello!" || evs[0].StopReason != "stop" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
	wantUsage(t, evs[0].Usage, event.Usage{InputTokens: 1, OutputTokens: 3, CacheReadTokens: 24})
	if evs[1].Outcome != event.Finished {
		t.Fatalf("outcome = %q", evs[1].Outcome)
	}
}

func TestToolRequestOffersTools(t *testing.T) {
	evs := requestEvents(t, fixture(t, "tool_request_1.json"))
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if !reflect.DeepEqual(evs[1].ToolsOffered, []string{"read_file"}) {
		t.Fatalf("tools offered = %v", evs[1].ToolsOffered)
	}
}

// The tool call's arguments came in seven deltas; they are joined into one
// JSON input, and the reply does not end the turn.
func TestStreamedToolCall(t *testing.T) {
	evs := reply(t, 200, "text/event-stream", fixture(t, "tool_response_1.sse"))
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	if evs[0].StopReason != "tool_calls" || evs[0].Text != "" || evs[0].Error != "" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
	wantUsage(t, evs[0].Usage, event.Usage{InputTokens: 174, OutputTokens: 21})
	call := evs[1]
	if call.ToolID != "u59ffvZ1u1PLK1zFM9h3QTZyHzCmDiRx" || call.ToolName != "read_file" || call.Error != "" {
		t.Fatalf("tool.requested = %+v", call)
	}
	if string(call.ToolInput) != `{"path": "notes.txt"}` {
		t.Fatalf("tool input = %s", call.ToolInput)
	}
}

func TestWholeToolCall(t *testing.T) {
	evs := reply(t, 200, "application/json; charset=utf-8", fixture(t, "whole_tool_response.json"))
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested)
	if evs[1].ToolID != "ggwyzYGodcAyG6UeRCPVfME0nhWt95yL" || evs[1].ToolName != "read_file" ||
		string(evs[1].ToolInput) != `{"path": "notes.txt"}` {
		t.Fatalf("tool.requested = %+v", evs[1])
	}
	wantUsage(t, evs[0].Usage, event.Usage{InputTokens: 1, OutputTokens: 21, CacheReadTokens: 173})
}

// The request that sends the tool's result continues the turn: it reports the
// result, tied to the call by its id, and starts no turn.
func TestToolResultContinuesTheTurn(t *testing.T) {
	evs := requestEvents(t, fixture(t, "tool_request_2.json"))
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
	if evs[0].ToolID != "u59ffvZ1u1PLK1zFM9h3QTZyHzCmDiRx" || evs[0].ToolOutput != "buy milk; call the plumber" ||
		evs[0].ToolError != nil {
		t.Fatalf("tool.completed = %+v", evs[0])
	}
}

func TestReplyAfterToolEndsTheTurn(t *testing.T) {
	evs := reply(t, 200, "text/event-stream", fixture(t, "tool_response_2.sse"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if !strings.HasPrefix(evs[0].Text, "The tool response is: buy milk; call the plumber.") {
		t.Fatalf("text = %q", evs[0].Text)
	}
	wantUsage(t, evs[0].Usage, event.Usage{InputTokens: 25, OutputTokens: 32, CacheReadTokens: 194})
	if evs[1].Outcome != event.Finished {
		t.Fatalf("outcome = %q", evs[1].Outcome)
	}
}

// max_tokens 4 cut the reply: finish_reason "length" is a partial turn.
func TestLengthIsPartial(t *testing.T) {
	evs := reply(t, 200, "text/event-stream", fixture(t, "length_response.sse"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Text != "The sea, a" || evs[0].StopReason != "length" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
	wantUsage(t, evs[0].Usage, event.Usage{InputTokens: 37, OutputTokens: 4})
	if evs[1].Outcome != event.Partial || evs[1].StopReason != "length" {
		t.Fatalf("turn.completed = %+v", evs[1])
	}
}

// The first request of the turn and the one that sends the tool result name
// the same turn, and a request with a new user message names another.
func TestTurnBasis(t *testing.T) {
	a := Adapter{}
	first := a.TurnBasis(fixture(t, "tool_request_1.json"))
	second := a.TurnBasis(fixture(t, "tool_request_2.json"))
	if first == nil || string(first) != string(second) {
		t.Fatalf("basis differs:\n%s\n%s", first, second)
	}
	var ms []map[string]any
	if err := json.Unmarshal(first, &ms); err != nil || len(ms) != 2 || ms[1]["role"] != "user" {
		t.Fatalf("basis = %s", first)
	}
	if string(a.TurnBasis(fixture(t, "plain_request.json"))) == string(first) {
		t.Fatal("another conversation gave the same basis")
	}
	if a.TurnBasis([]byte(`{"messages":[{"role":"system","content":"x"}]}`)) != nil {
		t.Fatal("a request with no user message names no turn")
	}
	if a.TurnBasis([]byte(`not json`)) != nil {
		t.Fatal("an unreadable request names no turn")
	}
}

// Without stream_options.include_usage a stream carries no usage: Usage stays
// nil, not zeros.
func TestStreamWithoutUsageKeepsUsageNil(t *testing.T) {
	var kept []string
	for _, line := range strings.Split(string(fixture(t, "plain_response.sse")), "\n") {
		if !strings.Contains(line, `"usage"`) {
			kept = append(kept, line)
		}
	}
	evs := reply(t, 200, "text/event-stream", []byte(strings.Join(kept, "\n")))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Usage != nil || evs[0].Text != "Hello!" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
}

// A stream cut before finish_reason is a problem: the text so far is kept,
// and no turn end is claimed.
func TestStreamCutBeforeFinish(t *testing.T) {
	body := string(fixture(t, "tool_response_2.sse"))
	cut := body[:strings.Index(body, `"finish_reason":"stop"`)]
	cut = cut[:strings.LastIndex(cut, "\n")]
	evs := reply(t, 200, "text/event-stream", []byte(cut))
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Error != "the stream ended before the reply finished" || evs[0].Usage != nil ||
		!strings.HasPrefix(evs[0].Text, "The tool response is") {
		t.Fatalf("model.responded = %+v", evs[0])
	}
}

// A stream with no Content-Type is still read as a stream.
func TestStreamWithoutContentType(t *testing.T) {
	evs := reply(t, 200, "", fixture(t, "plain_response.sse"))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
}

func TestErrorReplies(t *testing.T) {
	for _, c := range []struct {
		body, want string
	}{
		// llama-server's shape, as OpenAI's.
		{`{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`,
			"the request exceeds the available context size"},
		{`{"error":"no credential for this vendor"}`, "no credential for this vendor"},
		{`upstream timed out`, "upstream timed out"},
	} {
		evs := reply(t, 400, "application/json", []byte(c.body))
		wantKinds(t, evs, event.ModelResponded)
		if evs[0].Status != 400 || evs[0].Error != c.want {
			t.Errorf("%s: model.responded = %+v", c.body, evs[0])
		}
	}
	long := strings.Repeat("x", 2000)
	evs := reply(t, 500, "", []byte(long))
	if !strings.HasSuffix(evs[0].Error, " (cut)") || len(evs[0].Error) > maxError+len(" (cut)") {
		t.Fatalf("a long error is kept whole: %d bytes", len(evs[0].Error))
	}
}

// An error sent inside a stream is a problem, and the turn's end is not
// claimed.
func TestErrorInsideStream(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}],"model":"m"}` + "\n\n" +
		`data: {"error":{"message":"the model crashed"}}` + "\n\n"
	evs := reply(t, 200, "text/event-stream", []byte(body))
	wantKinds(t, evs, event.ModelResponded)
	if evs[0].Error != "the model crashed" || evs[0].Text != "Hel" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
}

func TestFinishReasons(t *testing.T) {
	for _, c := range []struct {
		finish  string
		outcome event.Outcome
		ends    bool
	}{
		{"stop", event.Finished, true},
		{"length", event.Partial, true},
		{"content_filter", event.Refused, true},
		{"tool_calls", "", false},
		{"function_call", "", false},
		{"something_new", event.Unknown, true},
	} {
		body := `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` +
			c.finish + `"}]}`
		evs := reply(t, 200, "application/json", []byte(body))
		var end *event.Event
		for i := range evs {
			if evs[i].Kind == event.TurnCompleted {
				end = &evs[i]
			}
		}
		if (end != nil) != c.ends || (end != nil && end.Outcome != c.outcome) {
			t.Errorf("%s: events = %+v", c.finish, evs)
		}
	}
}

// An unknown finish_reason, role or usage field is reported by name.
func TestUnknownNamesReported(t *testing.T) {
	body := `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"something_new"}],` +
		`"usage":{"prompt_tokens":2,"completion_tokens":1,"new_count":1,"prompt_tokens_details":{"cached_tokens":0,"other":1}}}`
	evs := reply(t, 200, "application/json", []byte(body))
	var got []event.Unexpected
	for _, e := range evs {
		if e.Kind == event.SchemaUnexpected {
			got = append(got, *e.Unexpected)
		}
	}
	want := []event.Unexpected{
		{Where: "choices[].finish_reason", Value: "something_new"},
		{Where: "usage", Field: "new_count"},
		{Where: "usage.prompt_tokens_details", Field: "other"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected = %+v, want %+v", got, want)
	}
	evs = requestEvents(t, []byte(`{"model":"m","messages":[{"role":"critic","content":"x"},{"role":"user","content":"go"}]}`))
	if last := evs[len(evs)-1]; last.Kind != event.SchemaUnexpected || last.Unexpected.Value != "critic" {
		t.Fatalf("events = %+v", evs)
	}
}

// Content parts: text parts are joined, and an image part is kept as its JSON
// after the text, so nothing the person sent is lost.
func TestContentParts(t *testing.T) {
	image := `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`
	evs := requestEvents(t, []byte(`{"model":"m","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"what is"},{"type":"text","text":"this?"},`+image+`]}]}`))
	if evs[0].Kind != event.TurnStarted || evs[0].Text != "what is\nthis?\n"+image {
		t.Fatalf("turn.started = %+v", evs[0])
	}
}

// Tools offered the older way (functions) and as custom tools are named too.
func TestToolsOfferedEveryWay(t *testing.T) {
	evs := requestEvents(t, []byte(`{"model":"m","messages":[{"role":"user","content":"go"}],`+
		`"tools":[{"type":"function","function":{"name":"read_file"}},{"type":"custom","custom":{"name":"apply_patch"}}],`+
		`"functions":[{"name":"legacy"}]}`))
	if got := evs[1].ToolsOffered; !reflect.DeepEqual(got, []string{"read_file", "apply_patch", "legacy"}) {
		t.Fatalf("tools offered = %v", got)
	}
}

// Arguments that are not JSON are reported on the call, not dropped
// silently; a custom tool's free-text input is kept as a JSON string.
func TestToolInputs(t *testing.T) {
	body := `{"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"read_file","arguments":"{not json"}},` +
		`{"id":"b","type":"custom","custom":{"name":"apply_patch","input":"*** Begin Patch"}}]}}]}`
	evs := reply(t, 200, "application/json", []byte(body))
	wantKinds(t, evs, event.ModelResponded, event.ToolRequested, event.ToolRequested)
	if evs[1].Error != "the tool input is not valid JSON" || evs[1].ToolInput != nil {
		t.Fatalf("bad arguments: %+v", evs[1])
	}
	if evs[2].ToolName != "apply_patch" || string(evs[2].ToolInput) != `"*** Begin Patch"` {
		t.Fatalf("custom call: %+v", evs[2])
	}
}

// A refusal ends the turn as refused, its text kept as the reply.
func TestRefusal(t *testing.T) {
	body := `{"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`
	evs := reply(t, 200, "application/json", []byte(body))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Text != "I can't help with that." || evs[1].Outcome != event.Refused {
		t.Fatalf("events = %+v", evs)
	}
}

// OpenRouter's usage.cost is kept as the vendor's own charge.
func TestOpenRouterCost(t *testing.T) {
	body := `{"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0.00042,"is_byok":false,"cost_details":{}}}`
	evs := reply(t, 200, "application/json", []byte(body))
	if u := evs[0].Usage; u == nil || u.CostUSD == nil || *u.CostUSD != 0.00042 {
		t.Fatalf("usage = %+v", evs[0].Usage)
	}
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
}

func TestBadBodies(t *testing.T) {
	if _, err := (Adapter{}).RequestEvents([]byte(`{`)); err == nil {
		t.Error("a request that is not JSON should fail")
	}
	if _, err := (Adapter{}).ResponseEvents(200, "application/json", []byte(`{`)); err == nil {
		t.Error("a reply that is not JSON should fail")
	}
	if _, err := (Adapter{}).ResponseEvents(200, "text/event-stream", []byte("data: {\n\n")); err == nil {
		t.Error("a stream chunk that is not JSON should fail")
	}
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","choices":[]}`))
	if evs[0].Error != "the reply has no choices" || len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
}

// xAI's usage.cost_in_usd_ticks is kept as the vendor's own charge, in units
// of 1e-10 dollars (litellm's xai test: 37756000 ticks = $0.0037756). A
// negative count is no charge reported.
func TestXAICostTicks(t *testing.T) {
	for _, tc := range []struct {
		ticks string
		want  *float64
	}{{"37756000", ptr(0.0037756)}, {"0", ptr(0)}, {"-37756000", nil}} {
		body := `{"model":"grok-4.7","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],` +
			`"usage":{"prompt_tokens":100,"completion_tokens":200,"total_tokens":300,"cost_in_usd_ticks":` + tc.ticks + `}}`
		evs := reply(t, 200, "application/json", []byte(body))
		u := evs[0].Usage
		if u == nil || (tc.want == nil) != (u.CostUSD == nil) || (tc.want != nil && *u.CostUSD != *tc.want) {
			t.Errorf("ticks %s: usage = %+v", tc.ticks, u)
		}
		wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	}
}

func ptr(f float64) *float64 { return &f }
