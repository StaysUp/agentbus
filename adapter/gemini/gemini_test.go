package gemini

import (
	"bytes"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
)

// The shapes below are agy 1.5.5's, through Code Assist, measured 2026-11-11
// (ids, signatures and long text shortened).
const startReq = `{"model":"gemini-3.8-flash-medium","project":"p","requestId":"agent/x/1","request":{"contents":[` +
	`{"role":"user","parts":[{"text":"<USER_REQUEST>\nRun cat notes.txt\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-11T04:57:50Z.\n</ADDITIONAL_METADATA>"}]}],` +
	`"systemInstruction":{"role":"user","parts":[{"text":"<identity>..."}]},` +
	`"tools":[{"functionDeclarations":[{"name":"read_file"}]},{"functionDeclarations":[{"name":"run_command"}]}],` +
	`"generationConfig":{"maxOutputTokens":65536}},"userAgent":"antigravity","requestType":"agent"}`

const resultReq = `{"model":"gemini-3.8-flash-medium","request":{"contents":[` +
	`{"role":"user","parts":[{"text":"<USER_REQUEST>\nRun cat notes.txt\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-11T04:57:50Z.\n</ADDITIONAL_METADATA>"}]},` +
	`{"role":"model","parts":[{"functionCall":{"id":"call_18","name":"run_command","args":{"CommandLine":"cat notes.txt"}},"thoughtSignature":"SIGB"}]},` +
	`{"role":"model","parts":[{"functionResponse":{"id":"call_18","name":"run_command","response":{"output":"The code is 7731.\r\n"}}}]}],` +
	`"tools":[{"functionDeclarations":[{"name":"run_command"}]}]}}`

const callReply = "data: {\"response\": {\"candidates\": [{\"content\": {\"role\": \"model\",\"parts\": [{\"thoughtSignature\": \"SIG\",\"functionCall\": {\"name\": \"run_command\",\"args\": {\"CommandLine\": \"cat notes.txt\"},\"id\": \"call_18\"}}]}}],\"usageMetadata\": {\"promptTokenCount\": 10471,\"candidatesTokenCount\": 38,\"totalTokenCount\": 10552,\"thoughtsTokenCount\": 43},\"modelVersion\": \"gemini-3.8-flash-n\"},\"traceId\": \"t\"}\r\n\r\n" +
	"data: {\"response\": {\"candidates\": [{\"content\": {\"role\": \"model\",\"parts\": [{\"text\": \"\"}]},\"finishReason\": \"STOP\"}],\"usageMetadata\": {\"promptTokenCount\": 10471,\"candidatesTokenCount\": 38,\"totalTokenCount\": 10552,\"thoughtsTokenCount\": 43},\"modelVersion\": \"gemini-3.8-flash-n\"},\"traceId\": \"t\"}\r\n\r\n"

const textReply = "data: {\"response\": {\"candidates\": [{\"content\": {\"role\": \"model\",\"parts\": [{\"text\": \"po\"}]}}],\"modelVersion\": \"gemini-3.8-flash-n\"}}\r\n\r\n" +
	"data: {\"response\": {\"candidates\": [{\"content\": {\"role\": \"model\",\"parts\": [{\"text\": \"thinking\",\"thought\": true},{\"text\": \"ng\"}]},\"finishReason\": \"STOP\"}],\"usageMetadata\": {\"promptTokenCount\": 100,\"candidatesTokenCount\": 1,\"thoughtsTokenCount\": 19,\"cachedContentTokenCount\": 60},\"modelVersion\": \"gemini-3.8-flash-n\"}}\r\n\r\n"

func kinds(evs []event.Event) []event.Kind {
	var out []event.Kind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestWatches(t *testing.T) {
	a := Adapter{}
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1internal:streamGenerateContent", true},
		{"POST", "/v1internal:generateContent", true},
		{"POST", "/v1beta/models/gemini-3.8-flash:streamGenerateContent", true},
		{"POST", "/v1internal:loadCodeAssist", false},
		{"GET", "/v1internal:streamGenerateContent", false},
	} {
		if got := a.Watches(c.method, c.path); got != c.want {
			t.Errorf("Watches(%s %s) = %v", c.method, c.path, got)
		}
	}
}

func TestRequestStartsTurn(t *testing.T) {
	evs, err := Adapter{}.RequestEvents([]byte(startReq))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != event.TurnStarted || evs[1].Kind != event.ModelRequested {
		t.Fatalf("events %v", kinds(evs))
	}
	if evs[0].Text != "Run cat notes.txt" {
		t.Errorf("person's words %q", evs[0].Text)
	}
	if evs[1].Model != "gemini-3.8-flash-medium" || strings.Join(evs[1].ToolsOffered, ",") != "read_file,run_command" {
		t.Errorf("model.requested %+v", evs[1])
	}
}

// A tool's result comes back in a content of role "model".
func TestRequestToolResult(t *testing.T) {
	evs, err := Adapter{}.RequestEvents([]byte(resultReq))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != event.ToolCompleted || evs[1].Kind != event.ModelRequested {
		t.Fatalf("events %v", kinds(evs))
	}
	if evs[0].ToolID != "call_18" || evs[0].ToolOutput != "The code is 7731.\r\n" {
		t.Errorf("tool.completed %+v", evs[0])
	}
}

// Both requests of one turn name the same turn, whatever a signature does.
func TestTurnBasis(t *testing.T) {
	a := Adapter{}
	b1, b2 := a.TurnBasis([]byte(startReq)), a.TurnBasis([]byte(resultReq))
	if b1 == nil || !bytes.Equal(b1, b2) {
		t.Fatalf("bases differ:\n%s\n%s", b1, b2)
	}
	next := strings.Replace(resultReq, `]}],"tools"`, `]},{"role":"user","parts":[{"text":"next"}]}],"tools"`, 1)
	b3 := a.TurnBasis([]byte(next))
	if b3 == nil || bytes.Equal(b1, b3) {
		t.Fatalf("a new request gave the same basis")
	}
	if !bytes.Equal(b3, a.TurnBasis([]byte(strings.ReplaceAll(next, "SIGB", "OTHER")))) {
		t.Errorf("a changed thoughtSignature changed the basis")
	}
	if a.TurnBasis([]byte(`{"request":{"contents":[]}}`)) != nil {
		t.Errorf("no person's words, yet a basis")
	}
}

// A reply that calls a tool ends in STOP too, and the turn goes on.
func TestReplyToolCall(t *testing.T) {
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(callReply))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != event.ModelResponded || evs[1].Kind != event.ToolRequested {
		t.Fatalf("events %v", kinds(evs))
	}
	if evs[1].ToolID != "call_18" || evs[1].ToolName != "run_command" || string(evs[1].ToolInput) != `{"CommandLine": "cat notes.txt"}` {
		t.Errorf("tool.requested %+v", evs[1])
	}
	u := evs[0].Usage
	if u == nil || u.InputTokens != 10471 || u.OutputTokens != 81 || evs[0].Error != "" {
		t.Errorf("model.responded %+v usage %+v", evs[0], u)
	}
}

func TestReplyText(t *testing.T) {
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(textReply))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Kind != event.TurnCompleted || evs[1].Outcome != event.Finished {
		t.Fatalf("events %+v", evs)
	}
	r := evs[0]
	if r.Text != "pong" || r.Model != "gemini-3.8-flash-n" || r.StopReason != "STOP" {
		t.Errorf("model.responded %+v", r)
	}
	if u := r.Usage; u.InputTokens != 40 || u.CacheReadTokens != 60 || u.OutputTokens != 20 {
		t.Errorf("usage %+v", u)
	}
}

func TestReplyCut(t *testing.T) {
	cut := textReply[:strings.Index(textReply, "\r\n")]
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(cut))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Error == "" {
		t.Fatalf("a cut reply: %+v", evs)
	}
}

func TestReplyShapes(t *testing.T) {
	one := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"MAX_TOKENS"}]}`
	evs, err := Adapter{}.ResponseEvents(200, "application/json", []byte(one))
	if err != nil || len(evs) != 2 || evs[0].Text != "hi" || evs[1].Outcome != event.Partial {
		t.Fatalf("one object: %v %+v", err, evs)
	}
	evs, err = Adapter{}.ResponseEvents(200, "application/json", []byte("["+one+"]"))
	if err != nil || len(evs) != 2 || evs[0].Text != "hi" {
		t.Fatalf("a list: %v %+v", err, evs)
	}
	odd := strings.Replace(one, "MAX_TOKENS", "NEW_REASON", 1)
	evs, _ = Adapter{}.ResponseEvents(200, "application/json", []byte(odd))
	if len(evs) != 3 || evs[1].Outcome != event.Unknown || evs[2].Kind != event.SchemaUnexpected {
		t.Fatalf("an unknown reason: %+v", evs)
	}
}

func TestReplyError(t *testing.T) {
	body := `[{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}]`
	evs, err := Adapter{}.ResponseEvents(429, "text/event-stream", []byte(body))
	if err != nil || len(evs) != 1 || evs[0].Status != 429 || evs[0].Error != "Resource has been exhausted (e.g. check quota)." {
		t.Fatalf("%v %+v", err, evs)
	}
	sse := "data: {\"error\":{\"message\":\"overloaded\"}}\n\n"
	evs, _ = Adapter{}.ResponseEvents(200, "text/event-stream", []byte(sse))
	if len(evs) != 1 || evs[0].Error != "overloaded" {
		t.Fatalf("an error in the stream: %+v", evs)
	}
}
