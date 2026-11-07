package openai

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/staysup/agentbus/body"
)

// codex-cli 0.164.0's second request of a tool turn: no instructions field,
// tools sent as an input item of type additional_tools (2026-11-05).
func TestSplitResponsesFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/tool_request_2.json")
	if err != nil {
		t.Fatal(err)
	}
	req, err := Adapter{}.Split(data)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	at := 0
	for i, p := range req.Pieces {
		if p.Kind != body.InputItem {
			t.Errorf("piece %d kind %s", i, p.Kind)
		}
		labels = append(labels, p.Label)
		j := bytes.Index(data[at:], p.JSON)
		if j < 0 {
			t.Fatalf("piece %d is not the request's bytes after offset %d", i, at)
		}
		at += j + len(p.JSON)
	}
	want := []string{"additional_tools", "developer", "developer", "developer", "developer", "user", "user",
		"custom_tool_call", "custom_tool_call_output"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %v\nwant %v", labels, want)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(req.Top, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["input"]; ok || string(top["model"]) != `"gpt-6.1-sol"` {
		t.Errorf("top = %s", req.Top)
	}
}

func TestSplitResponsesAllKinds(t *testing.T) {
	const in = `{"model":"gpt-6","instructions":"Be brief.","tools":[{"type":"function","name":"shell"},{"type":"web_search"}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}],"stream":true}`
	req, err := Adapter{}.Split([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	type kl struct{ kind, label string }
	var got []kl
	for _, p := range req.Pieces {
		got = append(got, kl{p.Kind, p.Label})
	}
	want := []kl{{"instructions", ""}, {"tool", "shell"}, {"tool", "web_search"}, {"input_item", "user"}, {"input_item", "function_call"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pieces = %v\nwant %v", got, want)
	}
	if string(req.Top) != `{"model":"gpt-6","stream":true}` {
		t.Errorf("top = %s", req.Top)
	}
	// An input given as one string is one item.
	req, err = Adapter{}.Split([]byte(`{"model":"m","input":"hello"}`))
	if err != nil || len(req.Pieces) != 1 || string(req.Pieces[0].JSON) != `"hello"` {
		t.Errorf("string input: %+v %v", req.Pieces, err)
	}
}
