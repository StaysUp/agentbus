package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

type guardFunc func(session, turn string) []string

func (f guardFunc) Forbid(session, turn string) []string { return f(session, turn) }

// guardGateway is a gateway with forbid = ["git push"] on turn s1/t_1, in
// front of a vendor that answers every model call with reply.
func guardGateway(t *testing.T, a adapter.Adapter, contentType, reply string) (*httptest.Server, *event.Memory, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		io.WriteString(w, reply)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	g := New(u, a, mem)
	g.SetGuard(guardFunc(func(s, turn string) []string {
		if s == "s1" && turn == "t_1" {
			return []string{"git push"}
		}
		return nil
	}))
	srv := httptest.NewServer(g)
	t.Cleanup(func() { srv.Close(); g.Close() })
	return srv, mem, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func post(t *testing.T, url, body string) string {
	t.Helper()
	resp, err := rawClient().Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	return string(b)
}

func refusedEvents(t *testing.T, mem *event.Memory, want int) []event.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var out []event.Event
		for _, e := range mem.Events() {
			if e.Kind == event.ToolRefused {
				out = append(out, e)
			}
		}
		if len(out) >= want || time.Now().After(deadline) {
			if len(out) != want {
				t.Fatalf("got %d tool.refused, want %d", len(out), want)
			}
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var d struct{ Type string }
		_ = json.Unmarshal([]byte(e), &d)
		b.WriteString("event: " + d.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

// Claude Code's chained command (2026-11-08, run c1), its input split across
// deltas, beside a call no rule names.
var claudeStream = sse(
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Pushing."}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_push","name":"Bash","input":{}}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git add notes.txt && git com"}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"mit -m x && git push origin main\"}"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_ls","name":"Bash","input":{}}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
	`{"type":"message_stop"}`,
)

func TestGuardRefusesClaudeStream(t *testing.T) {
	srv, mem, _ := guardGateway(t, anthropic.Adapter{}, "text/event-stream", claudeStream)
	got := post(t, srv.URL+"/s/s1/t/t_1/v1/messages", runBody)
	if !strings.Contains(got, `"id":"toolu_push","input":{},"name":"AgentBusRefused_Bash"`) {
		t.Errorf("the push was not renamed:\n%s", got)
	}
	if !strings.Contains(got, `{"type":"tool_use","id":"toolu_ls","name":"Bash","input":{}}`) {
		t.Errorf("the ls call changed:\n%s", got)
	}
	if strings.Count(got, "AgentBusRefused_") != 1 {
		t.Errorf("want one rename:\n%s", got)
	}
	if want := strings.ReplaceAll(claudeStream, `"name":"Bash","input":{}}}`, ""); len(got) < len(want) {
		t.Errorf("frames lost")
	}
	e := refusedEvents(t, mem, 1)[0]
	if e.ToolID != "toolu_push" || e.ToolName != "Bash" || e.Rule != "git push" || e.Turn != "t_1" ||
		!strings.Contains(string(e.ToolInput), "git push origin main") || !strings.Contains(e.Text, `"git push"`) {
		t.Errorf("tool.refused %+v", e)
	}

	// Other traffic, and a turn without rules, is never guarded.
	for _, p := range []string{"/s/s1/v1/messages", "/s/s1/t/t_2/v1/messages"} {
		if got := post(t, srv.URL+p, runBody); got != claudeStream {
			t.Errorf("%s changed the reply", p)
		}
	}
	refusedEvents(t, mem, 1)
}

// Claude Code re-sends a request unstreamed after a stream idle 300 s
// (2.1.319, 2026-11-08, run c5); that reply is guarded too.
func TestGuardRefusesClaudeUnstreamed(t *testing.T) {
	// Leading white space: still a JSON reply.
	reply := "\n " + `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git -C repo push"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":1}}`
	srv, mem, _ := guardGateway(t, anthropic.Adapter{}, "application/json", reply)
	got := post(t, srv.URL+"/s/s1/t/t_1/v1/messages", runBody)
	if !strings.Contains(got, `"name":"AgentBusRefused_Bash"`) || !json.Valid([]byte(got)) {
		t.Errorf("not renamed: %s", got)
	}
	if e := refusedEvents(t, mem, 1)[0]; e.ToolID != "toolu_1" || e.Rule != "git push" {
		t.Errorf("tool.refused %+v", e)
	}
}

// The next request carries the renamed call and the agent's own error for
// it. The vendor gets the real name and the refusal naming the rule.
func TestGuardFixesClaudeHistory(t *testing.T) {
	srv, mem, upstream := guardGateway(t, anthropic.Adapter{}, "text/event-stream", claudeStream)
	post(t, srv.URL+"/s/s1/t/t_1/v1/messages", runBody)
	refusedEvents(t, mem, 1)
	next := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_push","name":"AgentBusRefused_Bash","input":{"command":"git push"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_push","content":"<tool_use_error>Error: No such tool available: AgentBusRefused_Bash</tool_use_error>","is_error":true}]}]}`
	post(t, srv.URL+"/s/s1/t/t_1/v1/messages", next)
	sent := upstream()[1]
	if strings.Contains(sent, "AgentBusRefused_") || strings.Contains(sent, "No such tool") {
		t.Errorf("history not fixed: %s", sent)
	}
	var m struct {
		Messages []struct {
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(sent), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m.Messages[1].Content), `"name":"Bash"`) ||
		!strings.Contains(string(m.Messages[2].Content), `the forbid rule \"git push\" in permissions.toml`) {
		t.Errorf("history %s", sent)
	}
	// The adapter reads the fixed request: the call completes, with the
	// refusal as its output.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var done *event.Event
		for _, e := range mem.Events() {
			if e.Kind == event.ToolCompleted && e.ToolID == "toolu_push" {
				done = &e
			}
		}
		if done != nil {
			if !strings.Contains(done.ToolOutput, "Refused by the AgentBus gateway") {
				t.Errorf("tool.completed %+v", done)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no tool.completed for the refused call")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// codex's exec call (codex-cli 0.164.0, 2026-11-08, run x1): the push is
// inside the JavaScript, after git's -C option.
var codexStream = sse(
	`{"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}`,
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_push","name":"exec","input":"","status":"in_progress"}}`,
	`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":"const r = await tools.exec_command({cmd:\"git -C repo push origin main\"});"}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_push","name":"exec","input":"const r = await tools.exec_command({cmd:\"git -C repo push origin main\"});","status":"completed"}}`,
	`{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_2","call_id":"call_ls","name":"exec","input":"","status":"in_progress"}}`,
	`{"type":"response.output_item.done","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_2","call_id":"call_ls","name":"exec","input":"tools.exec_command({cmd:\"ls\"})","status":"completed"}}`,
	`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_push","name":"exec","input":"x"},{"type":"custom_tool_call","id":"ctc_2","call_id":"call_ls","name":"exec","input":"y"}],"usage":{"input_tokens":5,"output_tokens":3}}}`,
)

const codexPath = "/s/s1/t/t_1/backend-api/codex/responses"

func TestGuardRefusesCodexStream(t *testing.T) {
	// No Content-Type: codex's replies can come without one (2026-11-05).
	srv, mem, upstream := guardGateway(t, openai.Adapter{}, "", codexStream)
	got := post(t, srv.URL+codexPath, `{"model":"m","stream":true,"input":[]}`)
	if n := strings.Count(got, `"name":"AgentBusRefused_exec"`); n != 3 {
		t.Errorf("want the push renamed in added, done and completed, got %d:\n%s", n, got)
	}
	if n := strings.Count(got, `"name":"exec"`); n != 3 {
		t.Errorf("the ls call changed:\n%s", got)
	}
	e := refusedEvents(t, mem, 1)[0]
	if e.ToolID != "call_push" || e.ToolName != "exec" || e.Rule != "git push" ||
		!strings.Contains(string(e.ToolInput), "git -C repo push") {
		t.Errorf("tool.refused %+v", e)
	}

	// The history: the real name, and the refusal as a plain string.
	next := `{"model":"m","stream":true,"input":[{"type":"custom_tool_call","call_id":"call_push","name":"AgentBusRefused_exec","input":"x"},` +
		`{"type":"custom_tool_call_output","call_id":"call_push","output":[{"type":"input_text","text":"unsupported custom tool call"}]},` +
		`{"type":"custom_tool_call","call_id":"call_ls","name":"exec","input":"y"},{"type":"custom_tool_call_output","call_id":"call_ls","output":"a b"}]}`
	post(t, srv.URL+codexPath, next)
	sent := upstream()[1]
	if strings.Contains(sent, "AgentBusRefused_") || strings.Contains(sent, "unsupported") ||
		!strings.Contains(sent, `"output":"Refused by the AgentBus gateway: the forbid rule \"git push\"`) ||
		!strings.Contains(sent, `"output":"a b"`) {
		t.Errorf("history not fixed: %s", sent)
	}
}

// codex runs a call at its output_item.done, so nothing of the call reaches
// the agent before the gateway has read all of it.
func TestGuardHoldsTheCallUntilItEnds(t *testing.T) {
	pr, pw := io.Pipe()
	g := New(&url.URL{}, openai.Adapter{}, &event.Memory{})
	b := g.guardReply(&exchange{}, []string{"git push"}, pr, nil)
	frames := strings.SplitAfter(codexStream, "\n\n")
	got := make(chan string, 16)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := b.Read(buf)
			if n > 0 {
				got <- string(buf[:n])
			}
			if err != nil {
				close(got)
				return
			}
		}
	}()
	io.WriteString(pw, frames[0])
	if s := <-got; !strings.Contains(s, "response.created") {
		t.Fatalf("first frame %q", s)
	}
	io.WriteString(pw, frames[1]+frames[2])
	select {
	case s := <-got:
		t.Fatalf("a call's frame went on before its end: %q", s)
	case <-time.After(50 * time.Millisecond):
	}
	io.WriteString(pw, frames[3])
	if s := <-got; !strings.Contains(s, "AgentBusRefused_exec") || !strings.Contains(s, "output_item.added") {
		t.Fatalf("held frames %q", s)
	}
	pw.Close()
}

// A function tool with a cmd argument, and an unstreamed Responses reply.
func TestGuardRefusesCodexFunctionCallUnstreamed(t *testing.T) {
	reply := `{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"cd repo && git push\"}"}]}`
	srv, mem, _ := guardGateway(t, openai.Adapter{}, "application/json", reply)
	got := post(t, srv.URL+codexPath, `{"model":"m","input":[]}`)
	if !strings.Contains(got, `"name":"AgentBusRefused_exec_command"`) {
		t.Errorf("not renamed: %s", got)
	}
	if e := refusedEvents(t, mem, 1)[0]; e.ToolID != "call_1" || string(e.ToolInput) != `{"cmd":"cd repo && git push"}` {
		t.Errorf("tool.refused %+v", e)
	}
}

// A local_shell_call has no name to rename, so its command is replaced with
// one that prints the refusal and fails.
func TestGuardRefusesLocalShellCall(t *testing.T) {
	reply := `{"id":"resp_1","status":"completed","output":[{"type":"local_shell_call","id":"lsc_1","call_id":"call_1","action":{"type":"exec","command":["bash","-lc","git push"]}}]}`
	srv, mem, _ := guardGateway(t, openai.Adapter{}, "application/json", reply)
	got := post(t, srv.URL+codexPath, `{"model":"m","input":[]}`)
	var m struct {
		Output []struct {
			Action struct{ Command []string }
		}
	}
	if err := json.Unmarshal([]byte(got), &m); err != nil || len(m.Output) != 1 {
		t.Fatalf("%v %s", err, got)
	}
	cmd := m.Output[0].Action.Command
	if len(cmd) != 5 || cmd[0] != "sh" || !strings.Contains(cmd[4], `"git push"`) || strings.Contains(strings.Join(cmd[:4], " "), "push") {
		t.Errorf("command %q", cmd)
	}
	refusedEvents(t, mem, 1)
}

// holdRefusals blocks the delivery of tool.refused until release is closed.
type holdRefusals struct {
	release chan struct{}
	mem     event.Memory
}

func (h *holdRefusals) Emit(e event.Event) {
	if e.Kind == event.ToolRefused {
		<-h.release
	}
	h.mem.Emit(e)
}

// The turn is not settled while a refusal is still on its way to the store,
// for an unstreamed reply too, whose copy ends before the guard reads it.
func TestSettleWaitsForRefusals(t *testing.T) {
	reply := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git push"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	sink := &holdRefusals{release: make(chan struct{})}
	g := New(u, anthropic.Adapter{}, sink)
	g.SetGuard(guardFunc(func(string, string) []string { return []string{"git push"} }))
	srv := httptest.NewServer(g)
	t.Cleanup(func() { srv.Close(); g.Close() })
	post(t, srv.URL+"/s/s1/t/t_1/v1/messages", runBody)
	if g.Settle("s1", "t_1", 100*time.Millisecond) {
		t.Fatal("settled with a tool.refused not yet delivered")
	}
	close(sink.release)
	if !g.Settle("s1", "t_1", 2*time.Second) {
		t.Fatal("not settled after the refusal was delivered")
	}
}
