package anthropic

import (
	"testing"

	"github.com/staysup/agentbus/event"
)

// A client that stops reading after message_stop leaves a partial line in
// the copy. The reply is complete and its events stand.
func TestPartialLineAfterMessageStopIgnored(t *testing.T) {
	body := "event: message_start\n" + `data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n" +
		`data: {"ty`
	evs, err := Adapter{}.ResponseEvents(200, "text/event-stream", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelResponded, event.TurnCompleted)
	if evs[0].Error != "" || evs[0].Text != "hi" {
		t.Fatalf("model.responded = %+v", evs[0])
	}
}
