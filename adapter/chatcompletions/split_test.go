package chatcompletions

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// llama.cpp llama-server b12578's second tool request (2026-11-20): tools,
// then system, user, assistant with tool_calls, and the tool result.
func TestSplitChatCompletionsFixture(t *testing.T) {
	data := fixture(t, "tool_request_2.json")
	req, err := Adapter{}.Split(data)
	if err != nil {
		t.Fatal(err)
	}
	type kl struct{ kind, label string }
	var got []kl
	at := 0
	for i, p := range req.Pieces {
		got = append(got, kl{p.Kind, p.Label})
		j := bytes.Index(data[at:], p.JSON)
		if j < 0 {
			t.Fatalf("piece %d is not the request's bytes after offset %d: %s", i, at, p.JSON)
		}
		at += j + len(p.JSON)
	}
	want := []kl{{"tool", "read_file"}, {"message", "system"}, {"message", "user"},
		{"message", "assistant"}, {"message", "tool"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %v\nwant %v", got, want)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(req.Top, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["messages"]; ok {
		t.Errorf("top still has messages: %s", req.Top)
	}
	if _, ok := top["tools"]; ok {
		t.Errorf("top still has tools: %s", req.Top)
	}
	if string(top["model"]) != `"qwen2.5-0.5b-instruct"` {
		t.Errorf("top = %s", req.Top)
	}
}

func TestSplitChatCompletionsAllKinds(t *testing.T) {
	const in = `{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"bash"}},{"type":"custom","custom":{"name":"exec"}}],` +
		`"functions":[{"name":"legacy"}],"stream":true}`
	req, err := Adapter{}.Split([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	type kl struct{ kind, label string }
	var got []kl
	for _, p := range req.Pieces {
		got = append(got, kl{p.Kind, p.Label})
	}
	want := []kl{{"message", "system"}, {"message", "user"}, {"tool", "bash"}, {"tool", "exec"}, {"tool", "legacy"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pieces = %v\nwant %v", got, want)
	}
	if string(req.Top) != `{"model":"m","stream":true}` {
		t.Errorf("top = %s", req.Top)
	}
}

func TestSplitRefusesNonObject(t *testing.T) {
	for _, b := range []string{``, `[]`, `{"a":1} {}`, `{"a":`} {
		if _, err := (Adapter{}).Split([]byte(b)); err == nil {
			t.Errorf("%q: no error", b)
		}
	}
}
