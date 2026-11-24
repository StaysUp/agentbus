package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// anthropicAskStream is a reply with one tool_use call, Bash running "git push".
const anthropicAskStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicTwoAskStream is a reply with two tool_use calls, both ask-matched.
const anthropicTwoAskStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"npm publish\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicAskJSON is an unstreamed (non-SSE) reply with one tool_use call.
const anthropicAskJSON = `{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git push"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`

// guardOut runs the guard over a reply with the ask rules, then closes the
// gateway so its events reach the record, and returns what the agent gets, the
// gateway and its record.
func guardOut(t *testing.T, stream string, ask []string) (string, *Gateway, *event.Memory) {
	t.Helper()
	mem := &event.Memory{}
	g := New(&url.URL{}, anthropic.Adapter{}, mem)
	b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"}, Rules{Ask: ask}, nil, nil,
		io.NopCloser(strings.NewReader(stream)), func() {})
	raw, _ := io.ReadAll(b)
	b.Close()
	return string(raw), g, mem
}

// requested returns the one approval.requested event in the record, after the
// gateway has been closed so its events are flushed.
func requested(t *testing.T, g *Gateway, mem *event.Memory) (event.Event, bool) {
	t.Helper()
	g.Close()
	var out event.Event
	n := 0
	for _, e := range mem.Events() {
		if e.Kind == event.ApprovalRequested {
			out = e
			n++
		}
	}
	return out, n == 1
}

// An ask command is refused to the agent (renamed) with a "waiting for a person"
// message, and recorded as one approval.requested the person can answer. Nothing
// is held, and the gateway keeps no state of its own.
func TestAskRefusesAndRecords(t *testing.T) {
	got, g, mem := guardOut(t, anthropicAskStream, []string{"git push"})
	if !strings.Contains(got, RefusedPrefix+"Bash") {
		t.Fatalf("the ask call was not refused to the agent:\n%s", got)
	}
	if txt := g.refused.text("toolu_1"); !strings.Contains(txt, "waiting for a person to approve") || !strings.Contains(txt, "git push") {
		t.Fatalf("the refusal text is wrong: %q", txt)
	}
	ap, ok := requested(t, g, mem)
	if !ok {
		t.Fatal("no single approval.requested recorded")
	}
	if ap.Approval == "" {
		t.Fatal("approval.requested carries no id")
	}
	if len(ap.Commands) != 1 || ap.Commands[0] != "git push" {
		t.Fatalf("the approval did not record the command: %+v", ap.Commands)
	}
	if ap.ToolID != "toolu_1" || ap.ToolName != "Bash" {
		t.Fatalf("the approval did not carry the call: id=%q name=%q", ap.ToolID, ap.ToolName)
	}
}

// Every ask command in a reply is listed in the one approval.requested: none is
// hidden, and both matched rules are carried for an allow to move.
func TestAskListsEveryCommand(t *testing.T) {
	got, g, mem := guardOut(t, anthropicTwoAskStream, []string{"git push", "npm publish"})
	if n := strings.Count(got, RefusedPrefix+"Bash"); n != 2 {
		t.Fatalf("want both ask calls refused, got %d:\n%s", n, got)
	}
	// Each call's own refusal text names its own command.
	if txt := g.refused.text("toolu_2"); !strings.Contains(txt, "npm publish") {
		t.Fatalf("the second call's refusal did not name its own command: %q", txt)
	}
	ap, ok := requested(t, g, mem)
	if !ok {
		t.Fatal("no single approval.requested recorded")
	}
	if len(ap.Commands) != 2 || ap.Commands[0] != "git push" || ap.Commands[1] != "npm publish" {
		t.Fatalf("the approval did not list both commands: %+v", ap.Commands)
	}
	if len(ap.Rules) != 2 || ap.Rules[0] != "git push" || ap.Rules[1] != "npm publish" {
		t.Fatalf("the approval did not list both rules: %+v", ap.Rules)
	}
}

// An unstreamed (JSON) reply is refused and recorded the same way.
func TestAskUnstreamedRefusesAndRecords(t *testing.T) {
	got, g, mem := guardOut(t, anthropicAskJSON, []string{"git push"})
	if !strings.Contains(got, RefusedPrefix+"Bash") {
		t.Fatalf("the unstreamed ask call was not refused:\n%s", got)
	}
	ap, ok := requested(t, g, mem)
	if !ok || len(ap.Commands) != 1 || ap.Commands[0] != "git push" {
		t.Fatalf("the unstreamed reply did not record the command: ok=%v %+v", ok, ap.Commands)
	}
}

// The approval records the matched ask RULE (a valid permissions entry), not
// the raw command with its arguments, so moving it to allow removes exactly the
// entry that refused and the continuation is not refused again.
func TestAskRecordsMatchedRuleNotRawCommand(t *testing.T) {
	mem := &event.Memory{}
	g := New(&url.URL{}, anthropic.Adapter{}, mem)
	b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"}, Rules{Ask: []string{"git push"}}, nil, nil,
		io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
	item := map[string]any{"name": "Bash", "input": map[string]any{"command": "git push origin main"}}
	b.decide("0", "toolu_1", "Bash", json.RawMessage(`{"command":"git push origin main"}`), item)
	b.registerApproval()
	ap, ok := requested(t, g, mem)
	if !ok {
		t.Fatal("want one approval.requested")
	}
	if len(ap.Rules) != 1 || ap.Rules[0] != "git push" {
		t.Fatalf("the approval did not record the matched rule: %+v", ap.Rules)
	}
	if len(ap.Commands) != 1 || ap.Commands[0] != "git push origin main" {
		t.Fatalf("the approval lost the raw command: %+v", ap.Commands)
	}
}

// A codex local_shell_call refused by an ask rule is told the ask message, not
// the forbid message (it is refused in place, not through fixHistory).
func TestAskLocalShellCallUsesAskText(t *testing.T) {
	g := New(&url.URL{}, openai.Adapter{}, &event.Memory{})
	b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"}, Rules{Ask: []string{"git push"}}, nil, nil,
		io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
	item := map[string]any{"type": "local_shell_call", "call_id": "c1",
		"action": map[string]any{"type": "exec", "command": []any{"/bin/zsh", "-lc", "git push"}}}
	b.decide("c1", "c1", "", nil, item)
	txt := b.refused["c1"]
	if !strings.Contains(txt, "waiting for a person to approve") {
		t.Fatalf("the ask call got the wrong refusal text: %q", txt)
	}
	refuseItem(item, txt)
	cmd := item["action"].(map[string]any)["command"].([]any)
	if cmd[len(cmd)-1].(string) != txt {
		t.Fatalf("the local_shell_call did not embed the ask text: %v", cmd)
	}
	g.Close()
}

// anthropicAskPartial is the ask reply without its terminal message_stop: the
// tool_use block completed, but the reply never ended.
const anthropicAskPartial = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`

// A reply that never reached its terminal event records no approval, whether it
// ended by a clean EOF or a connection error: the agent discards it and the
// gateway drops its tool.requested, so a command it never really asked must not
// appear.
func TestAskNoTerminalEventNoApproval(t *testing.T) {
	for name, src := range map[string]io.ReadCloser{
		"clean EOF": io.NopCloser(strings.NewReader(anthropicAskPartial)),
		"cut":       &cutReader{data: []byte(anthropicAskPartial)},
	} {
		mem := &event.Memory{}
		g := New(&url.URL{}, anthropic.Adapter{}, mem)
		b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"}, Rules{Ask: []string{"git push"}}, nil, nil, src, func() {})
		io.ReadAll(b)
		b.Close()
		g.Close()
		for _, e := range mem.Events() {
			if e.Kind == event.ApprovalRequested {
				t.Fatalf("%s: a reply with no terminal event recorded an approval", name)
			}
		}
	}
}

// cutReader yields its bytes, then a non-EOF error, like a connection reset
// before the reply's clean end.
type cutReader struct {
	data []byte
	pos  int
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errReset
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *cutReader) Close() error { return nil }

var errReset = fmt.Errorf("connection reset")
