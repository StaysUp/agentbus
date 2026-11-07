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

func withoutUnexpected(evs []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind != event.SchemaUnexpected {
			out = append(out, e)
		}
	}
	return out
}

func sse(lines ...string) []byte {
	var b strings.Builder
	for _, l := range lines {
		var c bytes.Buffer
		if json.Compact(&c, []byte(l)) == nil {
			l = c.String() // one event per data line
		}
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(l), &head)
		b.WriteString("event: " + head.Type + "\ndata: " + l + "\n\n")
	}
	return []byte(b.String())
}

func final(output, extra string) string {
	return `{"type":"response.completed","response":{"model":"m","status":"completed","output":[` + output + `],"usage":{"input_tokens":3,"output_tokens":2}` + extra + `}}`
}

func TestUnknownOutputItemType(t *testing.T) {
	evs := reply(t, 200, "text/event-stream", sse(final(
		`{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"`+marker+`"}]},
		 {"type":"hologram","status":"completed","payload":"`+marker+`"},
		 {"type":"web_search_call","status":"completed","action":{"query":"`+marker+`"}}`, "")))
	wantUnexpected(t, evs, event.Unexpected{Where: "response.output[].type", Value: "hologram"})
}

func TestUnknownStreamEvent(t *testing.T) {
	evs := reply(t, 200, "", sse(
		`{"type":"response.created","response":{"model":"m","status":"in_progress","usage":null}}`,
		`{"type":"response.reasoning_summary.delta","delta":"`+marker+`"}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"`+marker+`"}`,
		final(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+marker+`"}]}`, "")))
	wantUnexpected(t, evs, event.Unexpected{Where: "stream.event", Value: "response.reasoning_summary.delta"})
}

func TestUnknownStatusAndIncompleteReason(t *testing.T) {
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"something_new","output":[]}`))
	wantUnexpected(t, evs, event.Unexpected{Where: "response.status", Value: "something_new"})

	evs = reply(t, 200, "application/json", []byte(`{"model":"m","status":"incomplete","incomplete_details":{"reason":"tired"},"output":[]}`))
	wantUnexpected(t, evs, event.Unexpected{Where: "response.incomplete_details.reason", Value: "tired"})

	evs = reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[{"type":"message","role":"assistant","status":"pondering","content":[]}]}`))
	wantUnexpected(t, evs, event.Unexpected{Where: "response.output[].status", Value: "pondering"})
}

func TestUnknownUsageField(t *testing.T) {
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[],
		"usage":{"input_tokens":3,"output_tokens":2,"new_counter":1,"input_tokens_details":{"cached_tokens":0,"audio_tokens":4},"output_tokens_details":{"reasoning_tokens":0,"dream_tokens":1}}}`))
	wantUnexpected(t, evs,
		event.Unexpected{Where: "usage", Field: "new_counter"},
		event.Unexpected{Where: "usage.input_tokens_details", Field: "audio_tokens"},
		event.Unexpected{Where: "usage.output_tokens_details", Field: "dream_tokens"})
}

func TestUnknownRequestItemAndRole(t *testing.T) {
	evs := requestEvents(t, []byte(`{"model":"m","input":[
		{"type":"message","role":"oracle","content":[{"type":"input_text","text":"`+marker+`"}]},
		{"type":"memory_snapshot","data":"`+marker+`"},
		{"type":"telepathy_call","call_id":"c1","arguments":"{\"x\":\"`+marker+`\"}"},
		{"type":"telepathy_call_output","call_id":"c1","output":"`+marker+`"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"`+marker+`"}]}]}`))
	wantUnexpected(t, evs,
		event.Unexpected{Where: "request.input[].role", Value: "oracle"},
		event.Unexpected{Where: "request.input[].type", Value: "memory_snapshot"})
}

// Every captured codex-cli 0.164.0 request and reply in testdata is known.
func TestCapturedFixturesAreKnown(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		body := fixture(t, e.Name())
		var evs []event.Event
		switch {
		case strings.HasSuffix(e.Name(), ".json"):
			evs = requestEvents(t, body)
		case strings.HasSuffix(e.Name(), ".sse"):
			evs = reply(t, 200, "", body)
		default:
			continue
		}
		n++
		if u := unexpected(evs); len(u) != 0 {
			t.Errorf("%s: schema.unexpected %+v", e.Name(), u)
		}
	}
	if n == 0 {
		t.Fatal("no fixtures read")
	}
	// The statuses and reasons the adapter maps are known too.
	for _, body := range []string{
		`{"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
		`{"model":"m","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`,
		`{"model":"m","status":"failed","error":{"code":"x","message":"y"},"output":[]}`,
		`{"model":"m","status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"compaction"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}`,
	} {
		if u := unexpected(reply(t, 200, "application/json", []byte(body))); len(u) != 0 {
			t.Errorf("%s: schema.unexpected %+v", body, u)
		}
	}
}

func TestUnexpectedDedupedAndCapped(t *testing.T) {
	evs := reply(t, 200, "", sse(
		`{"type":"response.mystery"}`, `{"type":"response.mystery"}`, `{"type":"response.mystery"}`,
		final(`{"type":"hologram"},{"type":"hologram"}`, "")))
	wantUnexpected(t, evs,
		event.Unexpected{Where: "stream.event", Value: "response.mystery"},
		event.Unexpected{Where: "response.output[].type", Value: "hologram"})

	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"response.mystery_%d","delta":"%s"}`, i, marker))
	}
	lines = append(lines, final("", ""))
	if got := len(unexpected(reply(t, 200, "", sse(lines...)))); got != adapter.MaxUnexpected {
		t.Fatalf("schema.unexpected events = %d, want %d", got, adapter.MaxUnexpected)
	}
}

// Reporting never makes parsing stricter: an item whose status is not a
// string still parses.
func TestOddStatusShapeStillParses(t *testing.T) {
	evs := reply(t, 200, "application/json", []byte(`{"model":"m","status":"completed","output":[
		{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"the answer"}]},
		{"type":"new_metadata","status":{"phase":"ready"}}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	if evs[0].Kind != event.ModelResponded || evs[0].Text != "the answer" || evs[0].Error != "" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
	wantKinds(t, withoutUnexpected(evs), event.ModelResponded, event.TurnCompleted)
	wantUnexpected(t, evs, event.Unexpected{Where: "response.output[].type", Value: "new_metadata"})
}

func TestReportsKeptOnStreamParseError(t *testing.T) {
	body := append(sse(`{"type":"response.created","response":{"model":"m","status":"in_progress","usage":null}}`,
		`{"type":"new_vendor_event"}`), []byte(`data: {"type":"response.compl`)...)
	evs, err := Adapter{}.ResponseEvents(200, "", body)
	if err == nil {
		t.Fatal("want a parse error")
	}
	wantUnexpected(t, evs, event.Unexpected{Where: "stream.event", Value: "new_vendor_event"})
	if len(evs) != 1 {
		t.Errorf("events with the error = %v, want only the report", kinds(evs))
	}
}

func TestHostedToolProgressAndInterruptedKnown(t *testing.T) {
	evs := reply(t, 200, "", sse(
		`{"type":"response.web_search_call.in_progress","output_index":0}`,
		`{"type":"response.web_search_call.searching","output_index":0}`,
		`{"type":"response.web_search_call.completed","output_index":0}`,
		`{"type":"response.file_search_call.searching","output_index":0}`,
		`{"type":"response.code_interpreter_call.interpreting","output_index":0}`,
		`{"type":"response.image_generation_call.partial_image","output_index":0}`,
		`{"type":"response.mcp_call.in_progress","output_index":0}`,
		`{"type":"response.incomplete","response":{"model":"m","status":"incomplete","incomplete_details":{"reason":"interrupted"},"output":[]}}`))
	wantUnexpected(t, evs)
}
