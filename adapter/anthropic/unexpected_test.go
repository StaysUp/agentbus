package anthropic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

// marker is placed in text and tool input. It must never reach a
// schema.unexpected event.
const marker = "MARKER-7f3a-secret"

func unexpected(evs []event.Event) []event.Unexpected {
	var out []event.Unexpected
	for _, e := range evs {
		if e.Kind != event.SchemaUnexpected {
			continue
		}
		if e.Unexpected == nil {
			out = append(out, event.Unexpected{Where: "(nil)"})
			continue
		}
		out = append(out, *e.Unexpected)
	}
	return out
}

func wantUnexpected(t *testing.T, evs []event.Event, want ...event.Unexpected) {
	t.Helper()
	got := unexpected(evs)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema.unexpected = %+v, want %+v", got, want)
	}
	for _, e := range evs {
		if e.Kind != event.SchemaUnexpected {
			continue
		}
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), marker) {
			t.Errorf("content reached a schema.unexpected event: %s", b)
		}
		if e.Text != "" || e.ToolInput != nil || e.ToolOutput != "" || e.Error != "" {
			t.Errorf("schema.unexpected carries more than a shape: %+v", e)
		}
	}
}

func respond(t *testing.T, ctype, body string) []event.Event {
	t.Helper()
	evs, err := Adapter{}.ResponseEvents(200, ctype, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func stream(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	return b.String()
}

func TestUnknownContentBlockType(t *testing.T) {
	evs := respond(t, "application/json", `{"model":"m","role":"assistant","content":[
		{"type":"text","text":"`+marker+`"},
		{"type":"server_tool_use_v2","id":"x","name":"web","input":{"q":"`+marker+`"}},
		{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"`+marker+`"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	wantUnexpected(t, evs, event.Unexpected{Where: "response.content[].type", Value: "server_tool_use_v2"})
	// The normal events are still there.
	if evs[0].Kind != event.ModelResponded || evs[1].Kind != event.ToolRequested {
		t.Errorf("kinds = %v", kinds(evs))
	}
}

func TestUnknownStopReason(t *testing.T) {
	evs := respond(t, "application/json", `{"model":"m","content":[{"type":"text","text":"`+marker+`"}],"stop_reason":"something_new","usage":{}}`)
	wantUnexpected(t, evs, event.Unexpected{Where: "response.stop_reason", Value: "something_new"})
}

func TestUnknownStreamEventAndDelta(t *testing.T) {
	evs := respond(t, "text/event-stream", stream(
		`{"type":"message_start","message":{"model":"m","role":"assistant","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+marker+`"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"sparkle_delta","sparkle":"`+marker+`"}}`,
		`{"type":"content_block_flash","index":0,"flash":"`+marker+`"}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`))
	wantUnexpected(t, evs,
		event.Unexpected{Where: "stream.delta.type", Value: "sparkle_delta"},
		event.Unexpected{Where: "stream.event", Value: "content_block_flash"})
}

func TestUnknownStreamBlockAndRole(t *testing.T) {
	evs := respond(t, "text/event-stream", stream(
		`{"type":"message_start","message":{"model":"m","role":"oracle","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"hologram","text":"`+marker+`"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`))
	wantUnexpected(t, evs,
		event.Unexpected{Where: "response.role", Value: "oracle"},
		event.Unexpected{Where: "response.content[].type", Value: "hologram"})
}

func TestUnknownUsageField(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		evs := respond(t, "application/json", `{"model":"m","content":[],"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1,"new_counter":3,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1d_input_tokens":7}}}`)
		wantUnexpected(t, evs,
			event.Unexpected{Where: "usage", Field: "new_counter"},
			event.Unexpected{Where: "usage.cache_creation", Field: "ephemeral_1d_input_tokens"})
	})
	t.Run("stream", func(t *testing.T) {
		evs := respond(t, "text/event-stream", stream(
			`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1,"new_counter":3}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"new_counter":4,"other_counter":1}}`,
			`{"type":"message_stop"}`))
		wantUnexpected(t, evs,
			event.Unexpected{Where: "usage", Field: "new_counter"},
			event.Unexpected{Where: "usage", Field: "other_counter"})
	})
}

func TestUnknownRequestRoleAndBlock(t *testing.T) {
	evs, err := Adapter{}.RequestEvents([]byte(`{"model":"m","messages":[
		{"role":"narrator","content":"` + marker + `"},
		{"role":"user","content":[{"type":"text","text":"` + marker + `"},{"type":"smell","data":"` + marker + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	wantUnexpected(t, evs,
		event.Unexpected{Where: "request.messages[].role", Value: "narrator"},
		event.Unexpected{Where: "request.messages[].content[].type", Value: "smell"})
	if evs[0].Kind != event.TurnStarted {
		t.Errorf("kinds = %v", kinds(evs))
	}
}

// The shapes Claude Code 2.1.318 sent through the gateway on 2026-11-05: the
// request block types and roles from three captured /v1/messages bodies, and
// the usage objects of message_start and message_delta as captured (the
// probe's usage run). None is unexpected.
func TestCapturedShapesAreKnown(t *testing.T) {
	req := `{"model":"claude-haiku-5-5","system":[{"type":"text","text":"s"}],"messages":[
		{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"},{"type":"text","text":"hi"}]},
		{"role":"system","content":[{"type":"text","text":"x"},{"type":"tool_addition","name":"T"},{"type":"tool_removal","name":"U"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"},"caller":{"type":"direct"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]},
		{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"ok","is_error":false,"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	evs, err := Adapter{}.RequestEvents([]byte(req))
	if err != nil {
		t.Fatal(err)
	}
	wantUnexpected(t, evs)

	evs = respond(t, "text/event-stream; charset=utf-8", stream(
		`{"type":"message_start","message":{"model":"claude-haiku-5-5","id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"cache_creation":{"ephemeral_1h_input_tokens":20046,"ephemeral_5m_input_tokens":0},"cache_creation_input_tokens":20046,"cache_read_input_tokens":0,"inference_geo":"global","input_tokens":2,"output_tokens":17,"service_tier":"standard","speed":"standard"}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"s"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"cache_creation_input_tokens":20046,"cache_read_input_tokens":0,"input_tokens":2,"iterations":[{"cache_creation":{"ephemeral_1h_input_tokens":20046,"ephemeral_5m_input_tokens":0},"cache_creation_input_tokens":20046,"cache_read_input_tokens":0,"input_tokens":2,"output_tokens":85,"type":"message"}],"output_tokens":85,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}}`,
		`{"type":"message_stop"}`))
	wantUnexpected(t, evs)

	for _, stop := range []string{"end_turn", "stop_sequence", "max_tokens", "refusal", "pause_turn", "tool_use"} {
		evs = respond(t, "application/json", `{"model":"m","type":"message","role":"assistant","content":[],"stop_reason":"`+stop+`","usage":{"input_tokens":1,"output_tokens":1}}`)
		wantUnexpected(t, evs)
	}
}

func TestUnexpectedDedupedPerReply(t *testing.T) {
	evs := respond(t, "application/json", `{"model":"m","content":[
		{"type":"hologram","text":"a"},{"type":"hologram","text":"b"},{"type":"hologram","text":"c"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	wantUnexpected(t, evs, event.Unexpected{Where: "response.content[].type", Value: "hologram"})
}

func TestUnexpectedCapped(t *testing.T) {
	var bs []string
	for i := 0; i < 40; i++ {
		bs = append(bs, fmt.Sprintf(`{"type":"kind_%d","text":"%s"}`, i, marker))
	}
	evs := respond(t, "application/json", `{"model":"m","content":[`+strings.Join(bs, ",")+`],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	if got := len(unexpected(evs)); got != adapter.MaxUnexpected {
		t.Fatalf("schema.unexpected events = %d, want %d", got, adapter.MaxUnexpected)
	}
}

// A vendor value that is not shaped like a name is never copied: it could be
// text in the wrong place.
func TestUnexpectedValueIsOnlyAName(t *testing.T) {
	evs := respond(t, "application/json", `{"model":"m","content":[{"type":"text with `+marker+` inside","text":"x"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	wantUnexpected(t, evs, event.Unexpected{Where: "response.content[].type", Value: "(not a name)"})
}

func TestIdentifierShapedSecretsAreNotNames(t *testing.T) {
	evs := respond(t, "application/json", `{"model":"m","content":[{"type":"PRIVATE_PAYLOAD_123","text":"x"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1,"sk-ant-api03-abcdef":1}}`)
	wantUnexpected(t, evs,
		event.Unexpected{Where: "response.content[].type", Value: "(not a name)"},
		event.Unexpected{Where: "usage", Field: "(not a name)"})
	for _, e := range evs {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "PRIVATE_PAYLOAD") || strings.Contains(string(b), "sk-ant") {
			t.Errorf("leaked: %s", b)
		}
	}
}

// A field read only to report it never makes parsing stricter.
func TestOddRoleShapeStillParses(t *testing.T) {
	evs := respond(t, "application/json", `{"model":"m","role":{"kind":"assistant"},"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
}

// A stream cut inside the next line keeps what was already seen.
func TestReportsKeptOnStreamParseError(t *testing.T) {
	body := stream(
		`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"new_vendor_event"}`) + `data: {"type":"content_blo`
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(body))
	if err == nil {
		t.Fatal("want a parse error")
	}
	wantUnexpected(t, evs, event.Unexpected{Where: "stream.event", Value: "new_vendor_event"})
	if len(evs) != 1 {
		t.Errorf("events with the error = %v, want only the report", kinds(evs))
	}
}

// OpenRouter's replies carry its charge, usage.cost, and usage.cost_details
// (seen 2026-11-13). Both are known; cost is the call's cost, in the reply
// and in the stream, where message_delta's replaces message_start's.
func TestOpenRouterCost(t *testing.T) {
	const details = `"cost_details":{"upstream_inference_cost":null,"upstream_inference_prompt_cost":0.001,"upstream_inference_completions_cost":0.0002}`
	for name, evs := range map[string][]event.Event{
		"json": respond(t, "application/json", `{"model":"m","content":[],"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1,"cost":0.0012,`+details+`}}`),
		"stream": respond(t, "text/event-stream", stream(
			`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1,"cost":0}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"cost":0.0012,`+details+`}}`,
			`{"type":"message_stop"}`)),
	} {
		wantUnexpected(t, evs)
		if u := evs[0].Usage; u == nil || u.CostUSD == nil || *u.CostUSD != 0.0012 {
			t.Errorf("%s: usage %+v", name, u)
		}
	}
	// Anthropic itself sends no cost: none is made up.
	evs := respond(t, "application/json", `{"model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	if evs[0].Usage.CostUSD != nil {
		t.Errorf("a cost with none sent: %v", *evs[0].Usage.CostUSD)
	}
}
