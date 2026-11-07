package openai

import (
	"reflect"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
)

// A built-in tool has a type and no name, and is offered under its type.
func TestBuiltInToolsOfferedByType(t *testing.T) {
	evs := requestEvents(t, []byte(`{"model":"m","tools":[{"type":"web_search"},{"type":"function","name":"read_file"}],`+
		`"input":[{"type":"message","role":"user","content":"go"}]}`))
	wantKinds(t, evs, event.TurnStarted, event.ModelRequested)
	if got := evs[1].ToolsOffered; !reflect.DeepEqual(got, []string{"web_search", "read_file"}) {
		t.Fatalf("tools offered = %v, want [web_search read_file]", got)
	}
}

// A stream may open with a ":" comment line; with no Content-Type it is still
// read as a stream.
func TestCommentFirstStreamDetected(t *testing.T) {
	body := ": keep-alive\n\n" +
		`data: {"type":"response.completed","response":{"model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":4,"output_tokens":1}}}` + "\n\n"
	evs := reply(t, 200, "", []byte(body))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Text != "hi" || evs[0].Error != "" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
}

// A tool result with text and an image kept only the text. The image part is
// kept too, as its JSON.
func TestToolResultKeepsNonTextParts(t *testing.T) {
	image := `{"type":"input_image","image_url":"data:image/png;base64,AAAA"}`
	evs := requestEvents(t, []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"look"},`+
		`{"type":"function_call","call_id":"c1","name":"view","arguments":"{}"},`+
		`{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"a screenshot"},`+image+`]}]}`))
	wantKinds(t, evs, event.ToolCompleted, event.ModelRequested)
	out := evs[0].ToolOutput
	if !strings.HasPrefix(out, "a screenshot") || !strings.Contains(out, image) {
		t.Fatalf("tool output = %q, want the text and the image part", out)
	}
}

// A client that stops reading after response.completed leaves a partial line
// in the copy. The reply is complete and its events stand.
func TestPartialLineAfterCompletedIgnored(t *testing.T) {
	body := `data: {"type":"response.completed","response":{"model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":4,"output_tokens":1}}}` +
		"\n\ndata: [DO"
	evs := reply(t, 200, "", []byte(body))
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Error != "" || evs[0].Text != "hi" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
}
