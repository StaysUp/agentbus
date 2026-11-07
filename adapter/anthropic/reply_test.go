package anthropic

import (
	"strings"
	"testing"
)

func sse(events ...string) []byte {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return []byte(b.String())
}

// A streamed reply with thinking (signature), text and a tool_use with a
// field the gateway does not know (caller, seen in Claude Code 2.1.319) is
// rebuilt block by block, each block's fields in the vendor's order.
func TestBuildRebuildsEveryBlockExactly(t *testing.T) {
	stream := sse(
		`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-x","content":[],"usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2lnLTE="}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Reading <a> & "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"b."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{},"caller":{"type":"direct"}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": "}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"/a.go\", \"z\": 1, \"a\": 2}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	)
	msg, stop, err := Adapter{}.Build(200, "text/event-stream; charset=utf-8", stream)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"","signature":"c2lnLTE="},` +
		`{"type":"text","text":"Reading <a> & b."},` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/a.go","z":1,"a":2},"caller":{"type":"direct"}}]}`
	if string(msg) != want || stop != "tool_use" {
		t.Errorf("got %s stop %q\nwant %s stop tool_use", msg, stop, want)
	}
}

// A reply the gateway cannot rebuild whole is an error, never a guess.
func TestBuildRefusesWhatItCannotRebuild(t *testing.T) {
	start := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	for name, stream := range map[string][]byte{
		"no message_stop": sse(start, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		"unknown delta": sse(start, `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{}}}`,
			`{"type":"message_stop"}`),
		"stream error":     sse(start, `{"type":"error","error":{"message":"overloaded"}}`),
		"delta first":      sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, `{"type":"message_stop"}`),
		"bad tool input":   sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"x","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`, `{"type":"message_stop"}`),
		"block started 2x": sse(start, start, `{"type":"message_stop"}`),
	} {
		if msg, _, err := (Adapter{}).Build(200, "text/event-stream", stream); err == nil {
			t.Errorf("%s: rebuilt %s, want an error", name, msg)
		}
	}
	if _, _, err := (Adapter{}).Build(429, "application/json", []byte(`{"type":"error"}`)); err == nil {
		t.Error("a 429 was rebuilt")
	}
}

// A reply that is not a stream is the message's content as sent.
func TestBuildReadsAJSONReply(t *testing.T) {
	msg, stop, err := Adapter{}.Build(200, "application/json",
		[]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	if err != nil || stop != "end_turn" || string(msg) != `{"role":"assistant","content":[{"type":"text","text":"hi"}]}` {
		t.Errorf("got %s %q %v", msg, stop, err)
	}
}
