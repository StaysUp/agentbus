package replay

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// Synthetic fixtures in Claude Code 2.1.319's layout (2026-11-06), never the
// saved real bodies: message 0 is the person's (reminder blocks, then the
// prompt), message 1 Claude Code's own system message.

type fakeTurn struct {
	prompt, from string
	exchanges    []store.Call // newest first
	incomplete   bool
	closed       bool
	end          store.TurnEnd
}

type fakeSource struct {
	turns   map[string]fakeTurn // by turn id
	order   []string            // newest first
	calls   map[string]store.CallContext
	replies map[string]body.Reply
}

func newSource() *fakeSource {
	return &fakeSource{turns: map[string]fakeTurn{}, calls: map[string]store.CallContext{}, replies: map[string]body.Reply{}}
}

func (f *fakeSource) TurnStart(_, turn string) (string, string, bool, error) {
	t, ok := f.turns[turn]
	return t.prompt, t.from, ok, nil
}
func (f *fakeSource) SessionTurns(string) ([]string, error) { return f.order, nil }
func (f *fakeSource) TurnCalls(_, turn string) ([]store.Call, error) {
	return f.turns[turn].exchanges, nil
}
func (f *fakeSource) TurnClosed(_, turn string) (bool, error) { return f.turns[turn].closed, nil }
func (f *fakeSource) TurnEnd(_, turn string) (store.TurnEnd, error) {
	return f.turns[turn].end, nil
}
func (f *fakeSource) TurnIncomplete(_, turn string) (bool, error) {
	return f.turns[turn].incomplete, nil
}
func (f *fakeSource) CallContext(x string, _ bool) (store.CallContext, bool, error) {
	c, ok := f.calls[x]
	return c, ok, nil
}
func (f *fakeSource) Reply(x string) (body.Reply, bool, error) {
	r, ok := f.replies[x]
	return r, ok, nil
}

// addTurn stores a turn, newest first.
func (f *fakeSource) addTurn(id, prompt, from string) {
	f.turns[id] = fakeTurn{prompt: prompt, from: from}
	f.order = append([]string{id}, f.order...)
}

// addCall stores one call of turn: its request, split as the Anthropic
// adapter splits it, and its reply.
func (f *fakeSource) addCall(t *testing.T, turn, exchange, request, reply, stop string) {
	t.Helper()
	req, err := anthropic.Adapter{}.Split([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	c := store.CallContext{Exchange: exchange, Request: req.Top}
	for _, p := range req.Pieces {
		c.Pieces = append(c.Pieces, store.Piece{Kind: p.Kind, Label: p.Label, JSON: p.JSON})
	}
	f.calls[exchange] = c
	if reply != "" {
		f.replies[exchange] = body.Reply{Exchange: exchange, Status: 200, Stop: stop, Message: json.RawMessage(reply)}
	}
	ft := f.turns[turn]
	ft.exchanges = append([]store.Call{{Exchange: exchange, Responded: true, Status: 200, Stop: stop}}, ft.exchanges...)
	f.turns[turn] = ft
}

const (
	reminder = `{"type":"text","text":"<system-reminder>ctx</system-reminder>"}`
	sys1     = `SessionStart hook additional context: hello\n\n# Environment\nenv one\n\nUserPromptSubmit hook additional context: ups one\n\nToday's date is 2026-11-06.`
	sys2     = `SessionStart hook additional context: hello\n\n# Environment\nenv two\n\nUserPromptSubmit hook additional context: ups two\n\nToday's date is 2026-11-07.`
	cut2     = `SessionStart hook additional context: hello\n\nUserPromptSubmit hook additional context: ups two`
	top      = `"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"Bash","input_schema":{}}]`
	bp1h     = `{"type":"ephemeral","ttl":"1h"}`

	// turn 1's last request: a tool loop, with a thinking block, a tool
	// input holding a key named cache_control (the application's data), and
	// breakpoints on its last two messages and inside the tool_result.
	t1Last = `{"model":"claude-x","messages":[` +
		`{"role":"user","content":[` + reminder + `,{"type":"text","text":"remember the word kiwi"}]},` +
		`{"role":"system","content":"` + sys1 + `"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lHLW9uZQ=="},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo","cache_control":"data"},"caller":{"type":"direct"},"cache_control":` + bp1h + `}]},` +
		`{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"text","text":"out","cache_control":` + bp1h + `}],"is_error":false,"cache_control":` + bp1h + `}]},` +
		`{"role":"system","content":[{"type":"text","text":"a late reminder"},{"type":"text","text":" in two blocks"}]}` +
		`],` + top + `,"safeguards":[{"type":"s","classifier_context":{"v":1,"prior_turn_context":[{"tool_use_ids":["toolu_1"],"context":"c1"}],"classifier_facts":[]}}],"stream":true}`
	t1Reply = `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lHLXR3bw=="},{"type":"text","text":"Noted: kiwi."}]}`

	// The same history, laid out as an earlier part of a request.
	t1History = `{"role":"user","content":[` + reminder + `,{"type":"text","text":"remember the word kiwi"}]},` +
		`{"role":"system","content":"` + sys1 + `"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lHLW9uZQ=="},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo","cache_control":"data"},"caller":{"type":"direct"}}]},` +
		`{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"text","text":"out"}],"is_error":false}]},` +
		`{"role":"system","content":"a late reminder in two blocks"}`
	t1ReplyBP = `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lHLXR3bw=="},{"type":"text","text":"Noted: kiwi.","cache_control":` + bp1h + `}]}`
)

// turn 2's first request from a fresh process: the person's message and the
// system message as a list of one block, with its breakpoint.
func t2First(prompt string) string {
	return `{"model":"claude-x","messages":[` +
		`{"role":"user","content":[` + reminder + `,{"type":"text","text":` + jsonQuote(prompt) + `}]},` +
		`{"role":"system","content":[{"type":"text","text":"` + sys2 + `","cache_control":` + bp1h + `}]}` +
		`],` + top + `,"safeguards":[{"type":"s","classifier_context":{"v":1,"classifier_facts":[]}}],"stream":true}`
}

// turn 2's tool-loop request: the system message is now a plain string, and
// the turn's own tool call and result follow.
const t2Loop = `{"model":"claude-x","messages":[` +
	`{"role":"user","content":[` + reminder + `,{"type":"text","text":"which word?"}]},` +
	`{"role":"system","content":"` + sys2 + `"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{},"caller":{"type":"direct"},"cache_control":` + bp1h + `}]},` +
	`{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":"x","is_error":false,"cache_control":` + bp1h + `}]}` +
	`],` + top + `,"safeguards":[{"type":"s","classifier_context":{"v":1,"prior_turn_context":[{"tool_use_ids":["toolu_2"],"context":"c2"}],"classifier_facts":[]}}],"stream":true}`

func jsonQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func twoTurns(t *testing.T) (*fakeSource, *Replayer) {
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	src.addCall(t, "t1", "x-1", t1Last, t1Reply, "end_turn")
	src.addTurn("t2", "which word?", "t1")
	return src, New(src)
}

func field2(t *testing.T, raw []byte, key string) string {
	t.Helper()
	return string(field(raw, key))
}

// The turn's first request (system message as a list): the history goes in
// front exactly, the prompt becomes a plain-string user message, the system
// message keeps only its hook texts and its own breakpoint, and one
// breakpoint goes on the history's last message with Claude Code's own
// cache_control. Everything else in the request is unchanged.
func TestSpliceFirstRequestListContent(t *testing.T) {
	_, r := twoTurns(t)
	rw := r.Rewrite("s", "t2", []byte(t2First("which word?")))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw.Events)
	}
	want := `[` + t1History + `,` + t1ReplyBP + `,` +
		`{"role":"user","content":"which word?"},` +
		`{"role":"system","content":[{"type":"text","text":"` + cut2 + `","cache_control":` + bp1h + `}]}]`
	if got := field2(t, rw.Body, "messages"); got != want {
		t.Errorf("messages\n got %s\nwant %s", got, want)
	}
	if rw.Replayed != 6 {
		t.Errorf("replayed %d, want 6", rw.Replayed)
	}
	for _, k := range []string{"model", "system", "tools", "stream"} {
		if field2(t, rw.Body, k) != field2(t, []byte(t2First("which word?")), k) {
			t.Errorf("%s changed", k)
		}
	}
	if n := countBreakpoints(mustFields(t, rw.Body), mustElements(t, field(rw.Body, "messages"))); n != 4 {
		t.Errorf("%d breakpoints, want 4", n)
	}
}

// A tool-loop request (system message as a plain string): no breakpoint is
// added, and the turn's own messages after the system message are unchanged.
func TestSpliceToolLoopStringContent(t *testing.T) {
	_, r := twoTurns(t)
	rw := r.Rewrite("s", "t2", []byte(t2Loop))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw.Events)
	}
	loop := mustElements(t, field([]byte(t2Loop), "messages"))
	want := `[` + t1History + `,` + t1Reply + `,` +
		`{"role":"user","content":"which word?"},` +
		`{"role":"system","content":"` + cut2 + `"},` +
		string(loop[2]) + `,` + string(loop[3]) + `]`
	if got := field2(t, rw.Body, "messages"); got != want {
		t.Errorf("messages\n got %s\nwant %s", got, want)
	}
}

// Thinking blocks of the history, with their signatures, are carried byte
// for byte, from the earlier requests and from the stored reply.
func TestThinkingCarriedByteExact(t *testing.T) {
	_, r := twoTurns(t)
	rw := r.Rewrite("s", "t2", []byte(t2Loop))
	for _, b := range []string{`{"type":"thinking","thinking":"","signature":"U0lHLW9uZQ=="}`,
		`{"type":"thinking","thinking":"","signature":"U0lHLXR3bw=="}`} {
		if !strings.Contains(string(rw.Body), b) {
			t.Errorf("the request lacks %s", b)
		}
	}
}

// prior_turn_context: the earlier turn's list goes before classifier_facts
// when the fresh request has none, and in front of the turn's own when it has.
func TestPriorTurnContextCarried(t *testing.T) {
	_, r := twoTurns(t)
	rw := r.Rewrite("s", "t2", []byte(t2First("which word?")))
	want := `[{"type":"s","classifier_context":{"v":1,"prior_turn_context":[{"tool_use_ids":["toolu_1"],"context":"c1"}],"classifier_facts":[]}}]`
	if got := field2(t, rw.Body, "safeguards"); got != want {
		t.Errorf("first request safeguards\n got %s\nwant %s", got, want)
	}
	rw = r.Rewrite("s", "t2", []byte(t2Loop))
	want = `[{"type":"s","classifier_context":{"v":1,"prior_turn_context":[{"tool_use_ids":["toolu_1"],"context":"c1"},{"tool_use_ids":["toolu_2"],"context":"c2"}],"classifier_facts":[]}}]`
	if got := field2(t, rw.Body, "safeguards"); got != want {
		t.Errorf("tool-loop safeguards\n got %s\nwant %s", got, want)
	}
}

// Turn 3 replays turn 2, whose stored request is itself a spliced one: the
// history grows, with both prompts, and prior_turn_context with it.
func TestChainedTurns(t *testing.T) {
	src, r := twoTurns(t)
	rw := r.Rewrite("s", "t2", []byte(t2Loop))
	src.addCall(t, "t2", "x-2", string(rw.Body), `{"role":"assistant","content":[{"type":"text","text":"kiwi"}]}`, "end_turn")
	src.addTurn("t3", "and again?", "t2")
	rw3 := r.Rewrite("s", "t3", []byte(strings.ReplaceAll(t2First("and again?"), "env two", "env three")))
	if rw3.Refuse != nil || rw3.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw3.Events)
	}
	msgs := mustElements(t, field(rw3.Body, "messages"))
	if rw3.Replayed != 11 || len(msgs) != 13 {
		t.Errorf("replayed %d of %d messages, want 11 of 13", rw3.Replayed, len(msgs))
	}
	s := string(rw3.Body)
	for _, w := range []string{`"remember the word kiwi"`, `{"role":"user","content":"which word?"}`,
		`{"role":"user","content":"and again?"}`, `"context":"c1"},{"tool_use_ids":["toolu_2"],"context":"c2"}`} {
		if !strings.Contains(s, w) {
			t.Errorf("turn 3's request lacks %s", w)
		}
	}
	if n := countBreakpoints(mustFields(t, rw3.Body), msgs); n != 4 {
		t.Errorf("%d breakpoints in turn 3's first request, want 4", n)
	}
}

// A layout the gateway does not know is refused with replay.failed and a
// 400, never sent on without the history.
func TestLayoutChangesAreRefused(t *testing.T) {
	_, r := twoTurns(t)
	for name, req := range map[string]string{
		"no environment marker": strings.ReplaceAll(t2First("which word?"), `# Environment\n`, `# Surroundings\n`),
		"prompt as a string":    `{"messages":[{"role":"user","content":"which word?"},{"role":"system","content":"` + sys2 + `"}],` + top + `}`,
		"no system message":     `{"messages":[{"role":"user","content":[` + reminder + `,{"type":"text","text":"which word?"}]}],` + top + `}`,
	} {
		rw := r.Rewrite("s", "t2", []byte(req))
		if rw.Refuse == nil || rw.Refuse.Status != 400 || rw.Body != nil {
			t.Errorf("%s: not refused: %+v", name, rw)
			continue
		}
		if len(rw.Events) != 1 || rw.Events[0].Kind != event.ReplayFailed || rw.Events[0].Error == "" {
			t.Errorf("%s: events %+v, want one replay.failed with error", name, rw.Events)
		}
		var e struct {
			Type  string `json:"type"`
			Error struct{ Type, Message string }
		}
		if json.Unmarshal(rw.Refuse.Body, &e) != nil || e.Type != "error" || e.Error.Type != "invalid_request_error" ||
			!strings.HasPrefix(e.Error.Message, "agentbus replay: ") {
			t.Errorf("%s: body %s, want an Anthropic error", name, rw.Refuse.Body)
		}
	}
}

// A turn whose earlier turn has no stored reply cannot be continued: refused.
func TestMissingHistoryIsRefused(t *testing.T) {
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	src.addCall(t, "t1", "x-1", t1Last, "", "end_turn")
	src.addTurn("t2", "which word?", "t1")
	rw := New(src).Rewrite("s", "t2", []byte(t2First("which word?")))
	if rw.Refuse == nil || len(rw.Events) != 1 || !strings.Contains(rw.Events[0].Error, "t1") {
		t.Errorf("got %+v, want a refusal naming t1", rw)
	}
}

// Requests that are not the replayed turn's main thread go on unchanged: a
// first turn, a side call with no tools, a subagent's request.
func TestUnchangedRequests(t *testing.T) {
	_, r := twoTurns(t)
	if rw := r.Rewrite("s", "t1", []byte(t1Last)); rw.Body != nil || rw.Refuse != nil {
		t.Error("a first turn's request was changed")
	}
	if rw := r.Rewrite("s", "unknown", []byte(t2Loop)); rw.Body != nil || rw.Refuse != nil {
		t.Error("a turn with no turn.started was changed")
	}
	// Claude Code's quota check, before the main thread's first request.
	quota := `{"model":"claude-x","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`
	if rw := r.Rewrite("s", "t2", []byte(quota)); rw.Body != nil || rw.Refuse != nil {
		t.Error("a side call without tools was changed")
	}
	// A subagent's request, after the main thread's first: its own system
	// prompt, its own prompt.
	if rw := r.Rewrite("s", "t2", []byte(t2First("which word?"))); rw.Body == nil {
		t.Fatal("the main thread's first request was not spliced")
	}
	if rw := r.Rewrite("s", "t2", []byte(subagentBody)); rw.Body != nil || rw.Refuse != nil {
		t.Errorf("a subagent's request was changed: %+v", rw)
	}
}

// A subagent's request: a system prompt of its own, a prompt of its own.
var subagentBody = strings.Replace(t2First("look for the parser in ./src"),
	`{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}`, `{"type":"text","text":"You are a search agent."}`, 1)

// After a compaction, the main thread's request no longer carries the
// prompt block. It keeps the main system prompt, so it is refused, never
// sent on without the history. A request with tools before any main one
// is refused the same way.
func TestCompactedMainRequestIsRefused(t *testing.T) {
	_, r := twoTurns(t)
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"This session is being continued from a previous conversation. Summary: ..."}]},` +
		`{"role":"system","content":"` + sys2 + `"}],` + top + `,"stream":true}`
	if rw := r.Rewrite("s", "t2", []byte(compacted)); rw.Refuse == nil {
		t.Error("a request before the main thread's first was not refused")
	}
	if rw := r.Rewrite("s", "t2", []byte(t2First("which word?"))); rw.Body == nil {
		t.Fatal("the main thread's first request was not spliced")
	}
	rw := r.Rewrite("s", "t2", []byte(compacted))
	if rw.Refuse == nil || len(rw.Events) != 1 || rw.Events[0].Kind != event.ReplayFailed ||
		!strings.Contains(rw.Events[0].Error, "compaction") {
		t.Errorf("compacted main request: %+v, want replay.failed", rw)
	}
	// The billing header (system[0]) may differ; the identity leaves it out.
	other := strings.Replace(compacted, `"text":"a","cache_control"`, `"text":"a2","cache_control"`, 1)
	if rw := r.Rewrite("s", "t2", []byte(other)); rw.Refuse == nil {
		t.Error("a compacted request with another billing header was not refused")
	}
}

// While the newest finished reply of the earlier turn is not stored, an
// older finished one is never used, and nothing read meanwhile is kept:
// once the newest arrives, it is the one replayed.
func TestPendingReplyIsNotPassedOver(t *testing.T) {
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	src.addCall(t, "t1", "x-0", t1Last, `{"role":"assistant","content":[{"type":"text","text":"cut short"}]}`, "max_tokens")
	src.addCall(t, "t1", "x-1", t1Last, "", "end_turn") // reply not stored yet
	src.addTurn("t2", "which word?", "t1")
	src.order = []string{"t1"} // Base is read before t2 starts
	r := New(src)
	if got, err := r.Base("s"); !errors.Is(err, ErrPending) {
		t.Errorf("base %q %v, want ErrPending", got, err)
	}
	if rw := r.Rewrite("s", "t2", []byte(t2First("which word?"))); rw.Refuse == nil {
		t.Errorf("spliced with an older reply while the newest was pending: %s", rw.Body)
	}
	src.replies["x-1"] = body.Reply{Exchange: "x-1", Status: 200, Stop: "end_turn", Message: json.RawMessage(t1Reply)}
	if got, err := r.Base("s"); err != nil || got.Turn != "t1" || got.Failed != "" {
		t.Errorf("base %+v %v, want t1", got, err)
	}
	rw := r.Rewrite("s", "t2", []byte(t2First("which word?")))
	if rw.Body == nil || !strings.Contains(string(rw.Body), "Noted: kiwi.") || strings.Contains(string(rw.Body), "cut short") {
		t.Errorf("got %s, want the newest reply replayed", rw.Body)
	}
}

// Base names the session's newest turn, never an older one in its place. A
// turn that failed before reaching the model, or stopped inside its tool
// loop, is still the base, with Failed saying how it ended.
func TestBase(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.addTurn("t2", "this one failed to start", "t1")
	src.setEnd("t2", store.TurnEnd{Closed: true, Error: "the agent could not be started"})
	if got, err := r.Base("s"); err != nil || got.Turn != "t2" || !strings.Contains(got.Failed, "not acted on") {
		t.Errorf("base %+v %v, want t2, failed", got, err)
	}
	src2 := newSource()
	src2.addTurn("t1", "remember the word kiwi", "")
	src2.addCall(t, "t1", "x-1", t1Last, `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"b","input":{}}]}`, "tool_use")
	src2.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	if got, err := New(src2).Base("s"); err != nil || got.Turn != "t1" || !strings.Contains(got.Failed, "while a tool was running") {
		t.Errorf("base %+v %v, want t1, failed inside its tool loop", got, err)
	}
	if got, err := New(newSource()).Base("s"); !errors.Is(err, ErrNoBase) {
		t.Errorf("base %+v %v, want ErrNoBase for a session with no turn", got, err)
	}
}

func intp(n int) *int { return &n }

func (f *fakeSource) setEnd(turn string, e store.TurnEnd) {
	ft := f.turns[turn]
	ft.end = e
	ft.closed = e.Closed
	f.turns[turn] = ft
}

// Without a UserPromptSubmit hook the cut keeps what comes before the
// environment; with no hook text at all the system message goes.
func TestCutWithoutHooks(t *testing.T) {
	got, err := cutTurnSystem("SessionStart hook additional context: x\n\n# Environment\nenv\n\nToday's date is d.")
	if err != nil || got != "SessionStart hook additional context: x" {
		t.Errorf("got %q %v", got, err)
	}
	m, err := cutMessage(json.RawMessage(`{"role":"system","content":"# Environment\nenv\n\nToday's date is d."}`))
	if err != nil || m != nil {
		t.Errorf("got %s %v, want the message dropped", m, err)
	}
}

func mustFields(t *testing.T, raw []byte) []body.Field {
	t.Helper()
	f, err := body.Fields(raw)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustElements(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	e, ok := body.Elements(raw)
	if !ok {
		t.Fatal("not an array")
	}
	return e
}

// A history's system message of text blocks becomes a string; one with other
// blocks (MCP tool_addition) stays a list, without cache_control.
func TestHistorySystemMessages(t *testing.T) {
	got, err := asHistory(json.RawMessage(`{"role":"system","content":[{"type":"text","text":"a"},{"type":"tool_addition","tool":{"definition":{"name":"m"}},"cache_control":{"type":"ephemeral"}}]}`))
	if err != nil || string(got) != `{"role":"system","content":[{"type":"text","text":"a"},{"type":"tool_addition","tool":{"definition":{"name":"m"}}}]}` {
		t.Errorf("got %s %v", got, err)
	}
}

// Seen live on 2026-11-06 (Claude Code 2.1.319 with MCP servers): the first
// system message is a text block then tool_addition blocks, and Claude Code's
// 1h breakpoint is on the last of them. The history's breakpoint copies that
// one: a 5m block before a 1h block is refused with 400.
func TestBreakpointFollowsClaudeCodesLifetime(t *testing.T) {
	_, r := twoTurns(t)
	req := `{"model":"claude-x","messages":[` +
		`{"role":"user","content":[` + reminder + `,{"type":"text","text":"which word?"}]},` +
		`{"role":"system","content":[{"type":"text","text":"` + sys2 + `"},{"type":"tool_addition","tool":{"definition":{"name":"m1"}}},{"type":"tool_addition","tool":{"definition":{"name":"m2"}},"cache_control":` + bp1h + `}]}` +
		`],` + top + `,"stream":true}`
	rw := r.Rewrite("s", "t2", []byte(req))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw.Events)
	}
	msgs := mustElements(t, field(rw.Body, "messages"))
	if got := string(msgs[5]); got != t1ReplyBP {
		t.Errorf("history's last message %s\nwant %s", got, t1ReplyBP)
	}
	want := `{"role":"system","content":[{"type":"text","text":"` + cut2 + `"},{"type":"tool_addition","tool":{"definition":{"name":"m1"}}},{"type":"tool_addition","tool":{"definition":{"name":"m2"}},"cache_control":` + bp1h + `}]}`
	if got := string(msgs[7]); got != want {
		t.Errorf("system message %s\nwant %s", got, want)
	}
}

// A claude run with --tools "" sends its main requests with no tools. They are
// told by their layout, not by tools: the first is spliced, a later one
// without the prompt block and with the main system prompt is refused, and a
// side call after them (no tools, its own system prompt) still goes on
// unchanged.
func TestMainRequestsWithoutTools(t *testing.T) {
	src, r := twoTurns(t)
	strip := func(b string) string {
		return strings.ReplaceAll(b, `,"tools":[{"name":"Bash","input_schema":{}}]`, "")
	}
	rw := r.Rewrite("s", "t2", []byte(strip(t2First("which word?"))))
	if rw.Body == nil || rw.Replayed != 6 {
		t.Fatalf("a main request without tools was not spliced: %+v", rw)
	}
	compacted := strip(`{"model":"claude-x","messages":[{"role":"user","content":"Summary: ..."},{"role":"system","content":"` + sys2 + `"}],` + top + `}`)
	if rw := r.Rewrite("s", "t2", []byte(compacted)); rw.Refuse == nil {
		t.Error("a main request without tools or prompt block was not refused")
	}
	title := `{"model":"claude-y","system":[{"type":"text","text":"h"},{"type":"text","text":"Write a title."}],"messages":[{"role":"user","content":"Summary: ..."}]}`
	if rw := r.Rewrite("s", "t2", []byte(title)); rw.Body != nil || rw.Refuse != nil {
		t.Errorf("a side call was changed: %+v", rw)
	}
	// The earlier turn's history is found by layout too: a turn with no
	// tools offered is still a base.
	src.calls["x-1"] = func() store.CallContext {
		c := src.calls["x-1"]
		var kept []store.Piece
		for _, p := range c.Pieces {
			if p.Kind != body.Tool {
				kept = append(kept, p)
			}
		}
		c.Pieces = kept
		return c
	}()
	src.order = []string{"t1"} // Base is read before the next turn starts
	if got, err := New(src).Base("s"); err != nil || got.Turn != "t1" {
		t.Errorf("base %+v %v", got, err)
	}
	if _, err := New(src).history("s", "t1", false); err != nil {
		t.Errorf("a turn without tools has no history: %v", err)
	}
}

// A turn (here a first turn, not replayed) whose last main-thread request was
// compacted has no usable history. Its earlier max_tokens answer never stands
// in: Base gives ErrUnusable and a replayed turn's request is refused.
func TestCompactedLastCallIsUnusable(t *testing.T) {
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	src.addCall(t, "t1", "x-0", t1Last, `{"role":"assistant","content":[{"type":"text","text":"cut short"}]}`, "max_tokens")
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":"Summary: the person asked to remember a word."},` +
		`{"role":"system","content":"` + sys1 + `"}],` + top + `}`
	src.addCall(t, "t1", "x-1", compacted, t1Reply, "end_turn")
	src.addTurn("t2", "which word?", "t1")
	r := New(src)
	if got, err := r.Base("s"); !errors.Is(err, ErrUnusable) {
		t.Errorf("base %+v %v, want ErrUnusable", got, err)
	}
	rw := r.Rewrite("s", "t2", []byte(t2First("which word?")))
	if rw.Refuse == nil || len(rw.Events) != 1 || !strings.Contains(rw.Events[0].Error, "compaction") {
		t.Errorf("got %+v, want a refusal naming the compaction", rw)
	}
}

// A subagent given the person's words verbatim, in the fresh layout but with
// its own system prompt, is not main: it passes unchanged, does not take
// over the main thread's identity, and a compacted main request after it is
// still refused. In a stored turn, the same subagent's later end_turn call
// is not the turn's last main call.
func TestSubagentWithTheSamePromptIsNotMain(t *testing.T) {
	src, r := twoTurns(t)
	if rw := r.Rewrite("s", "t2", []byte(t2First("which word?"))); rw.Body == nil {
		t.Fatal("the main thread's first request was not spliced")
	}
	sub := strings.Replace(t2First("which word?"), `{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}`,
		`{"type":"text","text":"You are a search agent."}`, 1)
	if rw := r.Rewrite("s", "t2", []byte(sub)); rw.Body != nil || rw.Refuse != nil {
		t.Errorf("a subagent with the person's words was changed: %+v", rw)
	}
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":"Summary: ..."},{"role":"system","content":"` + sys2 + `"}],` + top + `}`
	if rw := r.Rewrite("s", "t2", []byte(compacted)); rw.Refuse == nil {
		t.Error("a compacted main request after a subagent was not refused")
	}

	// Stored: the subagent's end_turn call is newer than the main thread's
	// last call, and is not taken for it.
	subStored := strings.Replace(t1Last, `{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}`,
		`{"type":"text","text":"You are a search agent."}`, 1)
	src.addCall(t, "t1", "x-9", subStored, `{"role":"assistant","content":[{"type":"text","text":"subagent report"}]}`, "end_turn")
	h, err := New(src).history("s", "t1", false)
	if err != nil {
		t.Fatal(err)
	}
	last := string(h.messages[len(h.messages)-1])
	if !strings.Contains(last, "Noted: kiwi.") || strings.Contains(last, "subagent report") {
		t.Errorf("history ends with %s, want the main thread's reply", last)
	}
}

// Calls come from model.requested. A newest main call whose reply event is
// missing, or that failed, is never skipped for the older finished one: its
// own request is replayed, with a note that the turn did not finish. A turn
// that may be missing events has no history at all.
func TestNewestCallWithoutReplyEventIsNeverSkipped(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*fakeSource)
		note string // "" for a refusal
	}{
		"no reply event": {func(f *fakeSource) {
			ft := f.turns["t1"]
			ft.exchanges[0].Responded, ft.exchanges[0].Status, ft.exchanges[0].Stop = false, 0, ""
			f.turns["t1"] = ft
		}, "its last model request got no reply"},
		"failed": {func(f *fakeSource) {
			ft := f.turns["t1"]
			ft.exchanges[0].Status, ft.exchanges[0].Stop = 529, ""
			f.turns["t1"] = ft
		}, "its last model request failed with status 529"},
		"events dropped": {func(f *fakeSource) {
			ft := f.turns["t1"]
			ft.incomplete = true
			f.turns["t1"] = ft
		}, ""},
	} {
		src := newSource()
		src.addTurn("t1", "remember the word kiwi", "")
		src.addCall(t, "t1", "x-0", t1Last, `{"role":"assistant","content":[{"type":"text","text":"older"}]}`, "end_turn")
		src.addCall(t, "t1", "x-1", t1Last, t1Reply, "end_turn")
		src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(1)})
		c.edit(src)
		r := New(src)
		got, err := r.Base("s")
		if c.note == "" {
			if !errors.Is(err, ErrUnusable) {
				t.Errorf("%s: base %+v %v, want ErrUnusable", name, got, err)
			}
		} else if err != nil || got.Turn != "t1" || !strings.HasPrefix(got.Failed, c.note) {
			t.Errorf("%s: base %+v %v, want t1 failed with %q", name, got, err, c.note)
		}
		src.addTurn("t2", "which word?", "t1")
		rw := r.Rewrite("s", "t2", []byte(t2First("which word?")))
		if c.note == "" {
			if rw.Refuse == nil {
				t.Errorf("%s: spliced %s, want a refusal", name, rw.Body)
			}
			continue
		}
		b := string(rw.Body)
		if rw.Refuse != nil || strings.Contains(b, `"text":"older"`) || strings.Contains(b, "Noted: kiwi.") || !strings.Contains(b, c.note) {
			t.Errorf("%s: got %s, want x-1's request with the note, and no reply", name, b)
		}
	}
}

// A full identity memo drops only closed turns' identities, so an open turn
// keeps its own and a same-prompt subagent arriving next is still not main.
func TestFullMemoKeepsOpenTurnsIdentity(t *testing.T) {
	src, r := twoTurns(t)
	if rw := r.Rewrite("s", "t2", []byte(t2First("which word?"))); rw.Body == nil {
		t.Fatal("the main thread's first request was not spliced")
	}
	// Fill the memo with closed turns' identities, then one more turn.
	for i := 0; i < memoMax; i++ {
		id := "c" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		src.turns[id] = fakeTurn{prompt: "p", closed: true}
		r.mu.Lock()
		r.mains[[2]string{"s", id}] = "closed"
		r.mu.Unlock()
	}
	src.addTurn("t3", "and again?", "t1")
	if rw := r.Rewrite("s", "t3", []byte(t2First("and again?"))); rw.Body == nil {
		t.Fatalf("t3's first request: %+v", rw)
	}
	r.mu.Lock()
	_, kept := r.mains[[2]string{"s", "t2"}]
	n := len(r.mains)
	r.mu.Unlock()
	if !kept || n != 2 {
		t.Errorf("open turn t2 kept %v, %d identities left, want t2 kept and only open turns left", kept, n)
	}
	sub := strings.Replace(t2First("which word?"), `{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}`,
		`{"type":"text","text":"You are a search agent."}`, 1)
	if rw := r.Rewrite("s", "t2", []byte(sub)); rw.Body != nil || rw.Refuse != nil {
		t.Errorf("a same-prompt subagent was taken as main after the memo filled: %+v", rw)
	}
}

// An open turn whose identity is not in memory gets it back from its oldest
// stored main-layout call, never from the request at hand; with a call whose
// request is not stored, it is refused.
func TestIdentityRecoveredFromTheStore(t *testing.T) {
	src, _ := twoTurns(t)
	// t2's first main request is already stored (spliced, as it was sent).
	first := New(src).Rewrite("s", "t2", []byte(t2First("which word?")))
	src.addCall(t, "t2", "x-20", string(first.Body), "", "tool_use")
	sub := strings.Replace(t2First("which word?"), `{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}`,
		`{"type":"text","text":"You are a search agent."}`, 1)
	r := New(src) // a fresh memo: t2's identity is not in it
	if rw := r.Rewrite("s", "t2", []byte(sub)); rw.Body != nil || rw.Refuse != nil {
		t.Errorf("a same-prompt subagent became main with the identity not in memory: %+v", rw)
	}
	r2 := New(src)
	delete(src.calls, "x-20") // its request was not stored
	if rw := r2.Rewrite("s", "t2", []byte(sub)); rw.Refuse == nil {
		t.Errorf("with the first call unread, got %+v, want a refusal", rw)
	}
}
