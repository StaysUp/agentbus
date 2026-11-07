package gateway

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/staysup/agentbus/event"
)

// closeAfter runs a vendor that sends body and then keeps the stream open, as
// chatgpt.com did after response.completed for codex-cli 0.164.0
// (2026-11-05). The client reads until it has seen marker, then closes.
func closeAfter(t *testing.T, newGW func(*testing.T, string) (*httptest.Server, *event.Memory),
	path, request, body, marker string) []event.Event {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	gw, mem := newGW(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+path, "application/json", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	got := ""
	for !strings.Contains(got, marker) {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("read before close: %v", err)
		}
	}
	resp.Body.Close()
	waitFor(t, mem, 3)
	time.Sleep(100 * time.Millisecond) // let any later event arrive
	return mem.Events()
}

func responded(t *testing.T, evs []event.Event) event.Event {
	t.Helper()
	for _, e := range evs {
		if e.Kind == event.ModelResponded {
			return e
		}
	}
	t.Fatalf("no model.responded in %+v", evs)
	return event.Event{}
}

func has(evs []event.Event, k event.Kind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}

const (
	openaiDone = `data: {"type":"response.created","response":{"model":"gpt-x","status":"in_progress","usage":null}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-x","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n"
	openaiPartial = `data: {"type":"response.created","response":{"model":"gpt-x","status":"in_progress","usage":null}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"ls","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}` + "\n\n"

	anthropicDone = "event: message_start\n" + `data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	anthropicPartial = "event: message_start\n" + `data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"ls","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}` + "\n\n"
)

// Cut rule, 2026-11-05: a client that closes the body after the reply's end
// keeps every event; one that closes before it gets only model.responded,
// with the cut noted.
func TestOpenAIClosedAfterCompletedKeepsTurnEnd(t *testing.T) {
	evs := closeAfter(t, newOpenAIGateway, "/backend-api/codex/responses", codexRequest, openaiDone, "response.completed")
	r := responded(t, evs)
	if r.Error != "" || r.StopReason != "completed" || !has(evs, event.TurnCompleted) {
		t.Fatalf("events = %+v; want turn.completed and no cut error", evs)
	}
}

func TestOpenAIClosedBeforeCompletedIsCut(t *testing.T) {
	evs := closeAfter(t, newOpenAIGateway, "/backend-api/codex/responses", codexRequest, openaiPartial, "arguments.delta")
	if r := responded(t, evs); !strings.Contains(r.Error, "cut off") {
		t.Fatalf("model.responded = %+v; want the cut noted", r)
	}
	if has(evs, event.ToolRequested) || has(evs, event.TurnCompleted) {
		t.Fatalf("events = %+v; a cut reply claims no tool call and no turn end", evs)
	}
}

func TestAnthropicClosedAfterMessageStopKeepsTurnEnd(t *testing.T) {
	evs := closeAfter(t, newGateway, "/v1/messages", okRequest, anthropicDone, "message_stop\"}")
	r := responded(t, evs)
	if r.Error != "" || r.StopReason != "end_turn" || !has(evs, event.TurnCompleted) {
		t.Fatalf("events = %+v; want turn.completed and no cut error", evs)
	}
}

func TestAnthropicClosedBeforeMessageStopIsCut(t *testing.T) {
	evs := closeAfter(t, newGateway, "/v1/messages", okRequest, anthropicPartial, `"stop_reason":"tool_use"`)
	if r := responded(t, evs); !strings.Contains(r.Error, "cut off") {
		t.Fatalf("model.responded = %+v; want the cut noted", r)
	}
	if has(evs, event.ToolRequested) || has(evs, event.TurnCompleted) {
		t.Fatalf("events = %+v; a cut reply claims no tool call and no turn end", evs)
	}
}

// compressedCut runs a vendor that sends body compressed with encoding,
// flushed but with no end (no gzip footer, no zstd frame end), and keeps the
// stream open. The client reads every flushed byte, then closes, as codex-cli
// 0.164.0 does after response.completed.
func compressedCut(t *testing.T, encoding, body string) []event.Event {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		io.Writer
		Flush() error
	}
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "deflate":
		w = zlib.NewWriter(&buf)
	case "zstd":
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		w = zw
	}
	io.WriteString(w, body)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	flushed := append([]byte(nil), buf.Bytes()...)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", encoding)
		w.Write(flushed)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	gw, mem := newOpenAIGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, len(flushed))); err != nil {
		t.Fatalf("read before close: %v", err)
	}
	resp.Body.Close()
	waitFor(t, mem, 3)
	time.Sleep(100 * time.Millisecond)
	return mem.Events()
}

func TestCompressedReplyCutAfterCompletedKeepsTurnEnd(t *testing.T) {
	for _, enc := range []string{"gzip", "deflate", "zstd"} {
		t.Run(enc, func(t *testing.T) {
			evs := compressedCut(t, enc, openaiDone)
			r := responded(t, evs)
			if r.Error != "" || r.StopReason != "completed" || !has(evs, event.TurnCompleted) {
				t.Fatalf("events = %+v; want turn.completed and no error", evs)
			}
		})
	}
}

func TestCompressedReplyCutBeforeCompletedIsCut(t *testing.T) {
	for _, enc := range []string{"gzip", "deflate", "zstd"} {
		t.Run(enc, func(t *testing.T) {
			evs := compressedCut(t, enc, openaiPartial)
			if r := responded(t, evs); !strings.Contains(r.Error, "cut off") {
				t.Fatalf("model.responded = %+v; want the cut noted", r)
			}
			if has(evs, event.TurnCompleted) {
				t.Fatalf("events = %+v; a cut reply claims no turn end", evs)
			}
		})
	}
}

// A complete body that fails to decode is still an error.
func TestCompleteBadGzipReplyIsAnError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Encoding", "gzip")
		w.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff, 'n', 'o', 't', ' ', 'f', 'l', 'a', 't', 'e'})
	}))
	defer upstream.Close()
	gw, mem := newOpenAIGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	evs := waitFor(t, mem, 3)
	if r := responded(t, evs); !strings.Contains(r.Error, "could not be decoded") {
		t.Fatalf("model.responded = %+v; want a decode error", r)
	}
}
