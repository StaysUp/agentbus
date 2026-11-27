package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"
)

// llama.cpp llama-server b12578 whole and streamed replies (2026-11-20):
// the assistant message OpenCode would send back on the next request.

func TestBuildReadsAJSONReply(t *testing.T) {
	msg, stop, err := Adapter{}.Build(200, "application/json", fixture(t, "whole_response.json"))
	if err != nil || stop != "stop" {
		t.Fatalf("Build: %v %q", err, stop)
	}
	if string(msg) != `{"role":"assistant","content":"Hello!"}` {
		t.Errorf("message %s", msg)
	}
}

func TestBuildReadsAJSONToolReply(t *testing.T) {
	data := fixture(t, "whole_tool_response.json")
	var whole struct {
		Choices []struct {
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &whole); err != nil {
		t.Fatal(err)
	}
	msg, stop, err := Adapter{}.Build(200, "application/json", data)
	if err != nil || stop != "tool_calls" {
		t.Fatalf("Build: %v %q", err, stop)
	}
	if string(msg) != string(whole.Choices[0].Message) {
		t.Errorf("got %s\nwant %s", msg, whole.Choices[0].Message)
	}
}

func TestBuildRebuildsAStreamedReply(t *testing.T) {
	msg, stop, err := Adapter{}.Build(200, "text/event-stream", fixture(t, "plain_response.sse"))
	if err != nil || stop != "stop" {
		t.Fatalf("Build: %v %q", err, stop)
	}
	if string(msg) != `{"role":"assistant","content":"Hello!"}` {
		t.Errorf("message %s", msg)
	}
}

func TestBuildRebuildsAStreamedToolReply(t *testing.T) {
	msg, stop, err := Adapter{}.Build(200, "text/event-stream", fixture(t, "tool_response_1.sse"))
	if err != nil || stop != "tool_calls" {
		t.Fatalf("Build: %v %q", err, stop)
	}
	want := `{"role":"assistant","content":"","tool_calls":[{"id":"u59ffvZ1u1PLK1zFM9h3QTZyHzCmDiRx","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"notes.txt\"}"}}]}`
	if string(msg) != want {
		t.Errorf("got %s\nwant %s", msg, want)
	}
}

func TestBuildKeepsAnEmptyReply(t *testing.T) {
	for name, tc := range map[string]struct{ contentType, data, want string }{
		"stream with no delta": {"text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", `{"role":"assistant","content":""}`},
		"null content":         {"application/json", `{"choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`, `{"role":"assistant","content":null}`},
		"blank content":        {"application/json", `{"choices":[{"index":0,"message":{"role":"assistant","content":"\n"},"finish_reason":"length"}]}`, `{"role":"assistant","content":"\n"}`},
	} {
		msg, _, err := Adapter{}.Build(200, tc.contentType, []byte(tc.data))
		if err != nil || string(msg) != tc.want {
			t.Errorf("%s: Build: %v %s", name, err, msg)
		}
	}
}

func TestBuildRefusesWhatItCannotRebuild(t *testing.T) {
	plain := string(fixture(t, "plain_response.sse"))
	cut, _, _ := strings.Cut(plain, `"finish_reason":"stop"`)
	for name, body := range map[string][]byte{
		"truncated stream": []byte(cut),
		"stream error":     []byte("data: {\"error\":{\"message\":\"overloaded\"}}\n\n"),
	} {
		if msg, _, err := (Adapter{}).Build(200, "text/event-stream", body); err == nil {
			t.Errorf("%s: rebuilt %s, want an error", name, msg)
		}
	}
	if _, _, err := (Adapter{}).Build(429, "application/json", []byte(`{"error":{"message":"busy"}}`)); err == nil {
		t.Error("a 429 was rebuilt")
	}
}
