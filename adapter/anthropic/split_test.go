package anthropic

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/staysup/agentbus/body"
)

// The shape Claude Code 2.1.318 sends: system blocks, tools, messages, and
// top-level fields around them (field names from the 2026-11-05 census).
const splitSample = `{"model":"claude-sonnet-5-5","max_tokens":32000,
 "system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.318"},
           {"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
 "tools":[{"name":"Read","description":"Reads a file.","input_schema":{"type":"object"}},
          {"name":"Edit","description":"Edits a file.","input_schema":{"type":"object"}}],
 "messages":[{"role":"user","content":[{"type":"text","text":"Fix the bug"}]},
             {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/a.go"}}]},
             {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package a"}]},
             {"role":"system","content":[{"type":"text","text":"reminder"}]}],
 "metadata":{"user_id":"u"},"stream":true,"thinking":{"type":"adaptive"}}`

func TestSplitMessages(t *testing.T) {
	req, err := Adapter{}.Split([]byte(splitSample))
	if err != nil {
		t.Fatal(err)
	}
	type kl struct{ kind, label string }
	var got []kl
	for _, p := range req.Pieces {
		got = append(got, kl{p.Kind, p.Label})
	}
	want := []kl{{"system", "text"}, {"system", "text"}, {"tool", "Read"}, {"tool", "Edit"},
		{"message", "user"}, {"message", "assistant"}, {"message", "user"}, {"message", "system"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %v\nwant %v", got, want)
	}
	// Each piece is the request's own bytes, in the request's order.
	at := 0
	for i, p := range req.Pieces {
		j := bytes.Index([]byte(splitSample)[at:], p.JSON)
		if j < 0 {
			t.Fatalf("piece %d is not the request's bytes after offset %d: %s", i, at, p.JSON)
		}
		at += j + len(p.JSON)
	}
	// Top keeps every other field, in order.
	fields, err := body.Fields(req.Top)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range fields {
		keys = append(keys, f.Key)
	}
	if want := []string{"model", "max_tokens", "metadata", "stream", "thinking"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("top keys = %v, want %v", keys, want)
	}
	// Top and the pieces together are the request.
	if !sameJSON(t, rebuild(t, req), []byte(splitSample)) {
		t.Errorf("top and pieces do not rebuild the request")
	}
}

func TestSplitSystemString(t *testing.T) {
	req, err := Adapter{}.Split([]byte(`{"model":"m","system":"Be brief.","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Pieces) != 1 || req.Pieces[0].Kind != body.System || string(req.Pieces[0].JSON) != `"Be brief."` {
		t.Fatalf("pieces = %+v", req.Pieces)
	}
	if string(req.Top) != `{"model":"m"}` {
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

// rebuild puts the pieces back into Top, as the request held them.
func rebuild(t *testing.T, req body.Request) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(req.Top, &m); err != nil {
		t.Fatal(err)
	}
	for _, p := range req.Pieces {
		var v any
		if err := json.Unmarshal(p.JSON, &v); err != nil {
			t.Fatal(err)
		}
		key := map[string]string{body.System: "system", body.Tool: "tools", body.Message: "messages"}[p.Kind]
		if s, ok := v.(string); ok && p.Kind == body.System {
			m[key] = s
			continue
		}
		list, _ := m[key].([]any)
		m[key] = append(list, v)
	}
	out, _ := json.Marshal(m)
	return out
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
