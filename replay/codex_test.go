package replay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/store"
)

// Synthetic fixtures in codex-cli 0.164.0's layout (2026-11-06), never the
// saved real bodies: the tools item, developer messages, the AGENTS.md and
// environment user message, then the prompt (content kinds user.text), then
// the turn's tool loop. Item ids differ per process; content does not.

const (
	thread1 = "019a0000-0000-7000-8000-000000000001"
	thread2 = "019a0000-0000-7000-8000-000000000002"
	thread3 = "019a0000-0000-7000-8000-000000000003"
	subThr  = "019a0000-0000-7000-8000-0000000000aa"
)

func pass(id string, kinds ...string) string {
	k, _ := json.Marshal(kinds)
	return `"internal_chat_message_metadata_passthrough":{"turn_id":"` + id + `","create_time":1793873160.27,"content_item_kinds":` + string(k) + `}`
}

func devItem(id, text string, kinds ...string) string {
	return `{"type":"message","id":"` + id + `","role":"developer","content":[{"type":"input_text","text":` + jsonQuote(text) + `}],` + pass(id, kinds...) + `}`
}

func userItem(id, text string, kinds ...string) string {
	return `{"type":"message","id":"` + id + `","role":"user","content":[{"type":"input_text","text":` + jsonQuote(text) + `}],` + pass(id, kinds...) + `}`
}

// context is what a fresh codex process puts first; p names its item ids,
// which differ between processes. perm is the permissions text.
func context(p, perm string) []string {
	return []string{
		`{"type":"additional_tools","id":"` + p + `-t","role":"developer","tools":[{"type":"custom","name":"exec"}]}`,
		devItem(p+"-1", "You are codex.", "model.base_instructions"),
		devItem(p+"-2", perm, "permissions.instructions"),
		userItem(p+"-3", "<environment_context>/tmp/x</environment_context>", "agents_md.instructions", "environments.environment_context"),
	}
}

func prompt(p, text string) string { return userItem(p+"-p", text, "user.text") }

// codexReq is a request with these input items under thread.
func codexReq(thread string, items ...string) string {
	return `{"model":"gpt-x","stream":true,"input":[` + strings.Join(items, ",") + `],"store":false,` +
		`"include":["reasoning.encrypted_content"],"prompt_cache_key":"` + thread + `",` +
		`"client_metadata":{"thread_id":"` + thread + `","session_id":"` + thread + `","x-codex-turn-metadata":"{\"thread_id\":\"` + thread + `\",\"turn_id\":\"u1\"}"}}`
}

const (
	reasoning1 = `{"type":"reasoning","id":"rs_1","metadata":{"m":1},"summary":[],"content":[],"encrypted_content":"RU5DLW9uZSsvPQ=="}`
	answer1    = `{"type":"message","id":"msg_1","metadata":{},"role":"assistant","status":"completed","content":[{"type":"output_text","text":"1591"}],"phase":"final"}`
	toolCall2  = `{"type":"custom_tool_call","id":"ctc_2","status":"completed","call_id":"call_2","name":"exec","input":"ls"}`
	toolOut2   = `{"type":"custom_tool_call_output","id":"ctco_2","call_id":"call_2","output":"a.txt"}`
	answer2    = `{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"kiwi"}]}`
)

func items(raws ...string) string { return "[" + strings.Join(raws, ",") + "]" }

// addCodexCall stores one codex call: its request split as the OpenAI
// adapter splits it, its model.responded, and its kept reply, if any.
func (f *fakeSource) addCodexCall(t *testing.T, turn, exchange, request string, c store.Call, reply string) {
	t.Helper()
	req, err := openai.Adapter{}.Split([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	cc := store.CallContext{Exchange: exchange, Vendor: "openai", Request: req.Top}
	for _, p := range req.Pieces {
		cc.Pieces = append(cc.Pieces, store.Piece{Kind: p.Kind, Label: p.Label, JSON: p.JSON})
	}
	f.calls[exchange] = cc
	if reply != "" {
		f.replies[exchange] = body.Reply{Exchange: exchange, Status: 200, Stop: c.Stop, Message: json.RawMessage(reply)}
	}
	c.Exchange = exchange
	ft := f.turns[turn]
	ft.exchanges = append([]store.Call{c}, ft.exchanges...)
	f.turns[turn] = ft
}

var completedCall = store.Call{Responded: true, Status: 200, Stop: "completed"}

const (
	p1 = "Remember the word kiwi. Reply with 37*43."
	p2 = "Which word did I ask you to remember?"
	p3 = "List the files."
)

// t1Req is turn 1's one request, from a fresh process in thread 1.
func t1Req() string {
	return codexReq(thread1, append(context("a", "read-only"), prompt("a", p1))...)
}

func codexTwoTurns(t *testing.T) (*fakeSource, *Replayer) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, items(reasoning1, answer1))
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t2", p2, "t1")
	return src, NewCodex(src)
}

func inputOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	for _, e := range mustElements(t, json.RawMessage(field2(t, raw, "input"))) {
		out = append(out, string(e))
	}
	return out
}

func eqItems(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d items, want %d\ngot  %s\nwant %s", what, len(got), len(want), strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: item %d\ngot  %s\nwant %s", what, i, got[i], want[i])
		}
	}
}

// A replayed turn's first request: turn 1's input and reply items in front
// of the fresh prompt, the fresh context dropped, byte for byte.
func TestCodexSplice(t *testing.T) {
	_, r := codexTwoTurns(t)
	fresh := codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...)
	rw := r.Rewrite("s", "t2", []byte(fresh))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("not spliced: %+v %s", rw.Events, rw.Refuse.Body)
	}
	want := append(append(context("a", "read-only"), prompt("a", p1), reasoning1, answer1), prompt("b", p2))
	eqItems(t, "input", inputOf(t, rw.Body), want)
	if rw.Replayed != 7 {
		t.Fatalf("replayed %d, want 7", rw.Replayed)
	}
	// The encrypted reasoning, its id and metadata as the vendor sent them.
	if !strings.Contains(string(rw.Body), reasoning1) {
		t.Fatal("the reasoning item is not carried byte for byte")
	}
}

// Thread id: the fresh one is replaced by the history's in every field but
// the input, and in every header.
func TestCodexThreadRewrite(t *testing.T) {
	_, r := codexTwoTurns(t)
	// The prompt itself names the fresh thread id: the input is never
	// rewritten.
	p := "Which word? (thread " + thread2 + ")"
	src, _ := codexTwoTurns(t)
	src.turns["t2"] = fakeTurn{prompt: p, from: "t1"}
	r = NewCodex(src)
	fresh := codexReq(thread2, append(context("b", "read-only"), prompt("b", p))...)
	rw := r.Rewrite("s", "t2", []byte(fresh))
	if rw.Body == nil || rw.Header == nil {
		t.Fatalf("no rewrite: %+v", rw)
	}
	if got := field2(t, rw.Body, "prompt_cache_key"); got != `"`+thread1+`"` {
		t.Fatalf("prompt_cache_key %s", got)
	}
	meta := field2(t, rw.Body, "client_metadata")
	if strings.Contains(meta, thread2) || strings.Count(meta, thread1) != 3 {
		t.Fatalf("client_metadata not rewritten: %s", meta)
	}
	in := inputOf(t, rw.Body)
	if in[len(in)-1] != prompt("b", p) {
		t.Fatalf("the input was rewritten: %s", in[len(in)-1])
	}
	h := http.Header{}
	h.Set("Session-Id", thread2)
	h.Set("Thread-Id", thread2)
	h.Set("X-Client-Request-Id", thread2)
	h.Set("X-Codex-Window-Id", thread2+":0")
	h.Set("X-Codex-Turn-Metadata", `{"thread_id":"`+thread2+`","turn_id":"u2"}`)
	h.Set("Authorization", "Bearer not-a-thread")
	rw.Header(h)
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, thread2) {
				t.Fatalf("header %s still names the fresh thread", k)
			}
		}
	}
	if h.Get("X-Codex-Window-Id") != thread1+":0" || h.Get("Authorization") != "Bearer not-a-thread" {
		t.Fatal("headers rewritten wrongly")
	}
}

// A multi-request turn: every request gets the same history, then the turn's
// own tool loop, so the prefix stays the same between requests.
func TestCodexToolLoop(t *testing.T) {
	_, r := codexTwoTurns(t)
	first := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...)))
	loop := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2), toolCall2, toolOut2)...)))
	if first.Body == nil || loop.Body == nil {
		t.Fatal("not spliced")
	}
	a, b := inputOf(t, first.Body), inputOf(t, loop.Body)
	eqItems(t, "prefix", b[:len(a)], a)
	eqItems(t, "loop", b[len(a):], []string{toolCall2, toolOut2})
}

// Turn 3 replays turn 2's last request, itself spliced, and its reply; the
// session keeps turn 1's thread id.
func TestCodexChainedTurns(t *testing.T) {
	src, r := codexTwoTurns(t)
	loop := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2), toolCall2, toolOut2)...)))
	src.addCodexCall(t, "t2", "x2", string(loop.Body), completedCall, items(answer2))
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t3", p3, "t2")
	rw := NewCodex(src).Rewrite("s", "t3", []byte(codexReq(thread3, append(context("c", "read-only"), prompt("c", p3))...)))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	want := append(append(inputOf(t, loop.Body), answer2), prompt("c", p3))
	eqItems(t, "input", inputOf(t, rw.Body), want)
	if got := field2(t, rw.Body, "prompt_cache_key"); got != `"`+thread1+`"` {
		t.Fatalf("prompt_cache_key %s, want turn 1's thread", got)
	}
}

// A context item that changed since the history (a new sandbox) goes right
// before the prompt; one that did not is dropped.
func TestCodexChangedContext(t *testing.T) {
	_, r := codexTwoTurns(t)
	ctx := context("b", "workspace-write")
	rw := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(ctx, prompt("b", p2))...)))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	want := append(append(context("a", "read-only"), prompt("a", p1), reasoning1, answer1), ctx[2], prompt("b", p2))
	eqItems(t, "input", inputOf(t, rw.Body), want)
}

func refusedWith(t *testing.T, rw gateway.Rewrite, want string) {
	t.Helper()
	if rw.Refuse == nil || rw.Refuse.Status != 400 || len(rw.Events) != 1 || rw.Events[0].Kind != event.ReplayFailed ||
		!strings.Contains(rw.Events[0].Error, want) {
		t.Fatalf("not refused with %q: %+v", want, rw.Events)
	}
	var e struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if json.Unmarshal(rw.Refuse.Body, &e) != nil || e.Error.Type != "invalid_request_error" ||
		!strings.HasPrefix(e.Error.Message, "agentbus replay: ") {
		t.Fatalf("refusal is not an OpenAI error: %s", rw.Refuse.Body)
	}
}

// Layouts the gateway does not know are refused, never sent without the
// history; another thread (a subagent) passes unchanged.
func TestCodexLayouts(t *testing.T) {
	ctx := context("b", "read-only")
	t.Run("compacted main request", func(t *testing.T) {
		_, r := codexTwoTurns(t)
		if rw := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(ctx, prompt("b", p2))...))); rw.Body == nil {
			t.Fatal("first request not spliced")
		}
		compacted := codexReq(thread2, append(ctx, prompt("b", p2), `{"type":"compaction","encrypted_content":"Q09NUA=="}`)...)
		refusedWith(t, r.Rewrite("s", "t2", []byte(compacted)), "as after a compaction")
	})
	t.Run("model item before the prompt", func(t *testing.T) {
		_, r := codexTwoTurns(t)
		odd := codexReq(thread2, append(append([]string{}, ctx...), answer1, prompt("b", p2))...)
		refusedWith(t, r.Rewrite("s", "t2", []byte(odd)), "in a layout the gateway does not know")
	})
	t.Run("another thread passes", func(t *testing.T) {
		_, r := codexTwoTurns(t)
		if rw := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(ctx, prompt("b", p2))...))); rw.Body == nil {
			t.Fatal("first request not spliced")
		}
		sub := codexReq(subThr, append(ctx, prompt("s", p2))...)
		if rw := r.Rewrite("s", "t2", []byte(sub)); rw.Body != nil || rw.Refuse != nil {
			t.Fatalf("a subagent's request was changed: %+v", rw)
		}
	})
	t.Run("not the codex format", func(t *testing.T) {
		_, r := codexTwoTurns(t)
		if rw := r.Rewrite("s", "t2", []byte(t2First(p2))); rw.Body != nil || rw.Refuse != nil {
			t.Fatalf("a body with no input was changed: %+v", rw)
		}
	})
}

// A turn's identity is not in memory (a new Replayer) while it has stored
// main-thread calls: its stored requests carry the session's thread id, not
// the agent's, so the request is refused, never taken for main or not.
func TestCodexIdentityNotRecovered(t *testing.T) {
	src, r := codexTwoTurns(t)
	first := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...)))
	src.addCodexCall(t, "t2", "x2", string(first.Body), completedCall, items(toolCall2))
	loop := codexReq(thread2, append(context("b", "read-only"), prompt("b", p2), toolCall2, toolOut2)...)
	refusedWith(t, NewCodex(src).Rewrite("s", "t2", []byte(loop)), "cannot be recovered")
}

// A session whose turn has no kept reply (as before codex replies were kept)
// has no history: the API then falls back to codex's own resume.
func TestCodexBaseWithoutReply(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, "")
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if _, err := NewCodex(src).Base("s"); !errors.Is(err, ErrPending) {
		t.Fatalf("err %v, want ErrPending", err)
	}
}

const (
	fnCall1 = `{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"}`
	webCall = `{"type":"web_search_call","id":"ws_1","status":"completed"}`
)

// recoverAfter runs turn 2 after turn 1 ended as set up, and returns the
// plan and the spliced input.
func recoverAfter(t *testing.T, src *fakeSource) (Plan, []string) {
	t.Helper()
	r := NewCodex(src)
	plan, err := r.Base("s")
	if err != nil {
		t.Fatalf("Base: %v", err)
	}
	src.addTurn("t2", p2, "t1")
	rw := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...)))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	return plan, inputOf(t, rw.Body)
}

func noteText(t *testing.T, raw string) string {
	t.Helper()
	if body.Label(json.RawMessage(raw), "role") != "developer" {
		t.Fatalf("the note is not a developer message: %s", raw)
	}
	s, _ := openai.PromptText(json.RawMessage(raw))
	if !strings.HasPrefix(s, notePrefix) {
		t.Fatalf("the note does not say it is AgentBus's: %s", s)
	}
	return s
}

// Killed in its tool loop: the last reply asked for two tools and the agent
// was stopped. Each call gets an output marked not finished, a hosted call
// none, then the note, last, before the new prompt.
func TestCodexRecoverKilledInToolLoop(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, items(reasoning1, webCall, fnCall1, toolCall2))
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	plan, in := recoverAfter(t, src)
	if plan.Turn != "t1" || !strings.Contains(plan.Failed, "it stopped while a tool was running") {
		t.Fatalf("plan %+v", plan)
	}
	n := len(in)
	if in[n-1] != prompt("b", p2) {
		t.Fatal("the prompt is not last")
	}
	note := noteText(t, in[n-2])
	if !strings.Contains(note, "signal 15, exit 143") || !strings.Contains(note, "Check the folder") {
		t.Fatalf("note: %s", note)
	}
	want := []string{reasoning1, webCall, fnCall1, toolCall2,
		`{"type":"function_call_output","call_id":"call_1","output":` + jsonQuote(notFinished) + `}`,
		`{"type":"custom_tool_call_output","call_id":"call_2","output":` + jsonQuote(notFinished) + `}`}
	eqItems(t, "reply and results", in[n-2-len(want):n-2], want)
}

// The vendor failed the last call (400 or 529): its request is the history,
// with the note; no reply.
func TestCodexRecoverVendorError(t *testing.T) {
	for _, status := range []int{400, 529} {
		src := newSource()
		src.addTurn("t1", p1, "")
		src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, items(toolCall2))
		second := codexReq(thread1, append(context("a", "read-only"), prompt("a", p1), toolCall2, toolOut2)...)
		src.addCodexCall(t, "t1", "x2", second, store.Call{Responded: true, Status: status, Error: "overloaded"}, "")
		src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(1)})
		plan, in := recoverAfter(t, src)
		if !strings.Contains(plan.Failed, "failed with status "+itoa(status)) {
			t.Fatalf("plan %+v", plan)
		}
		want := append(context("a", "read-only"), prompt("a", p1), toolCall2, toolOut2)
		eqItems(t, "history", in[:len(want)], want)
		if len(in) != len(want)+2 || !strings.Contains(noteText(t, in[len(want)]), "did not finish") {
			t.Fatalf("not history, note, prompt: %d items", len(in))
		}
	}
}

// A stream that broke before response.completed: the reply is left out,
// whatever stop reason came before.
func TestCodexRecoverBrokenStream(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", t1Req(), store.Call{Responded: true, Status: 200,
		Error: "the stream ended before the response completed; the reply was cut off"}, "")
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(1)})
	plan, in := recoverAfter(t, src)
	if !strings.Contains(plan.Failed, "did not arrive whole") {
		t.Fatalf("plan %+v", plan)
	}
	want := append(context("a", "read-only"), prompt("a", p1))
	eqItems(t, "history", in[:len(want)], want)
	if len(in) != len(want)+2 {
		t.Fatalf("%d items, want history, note, prompt", len(in))
	}
}

// A reply that asked for a call whose result shape is not known, such as a
// shell_call codex 0.164.0 sends without execution "client": no result is
// guessed, and the session cannot be continued by replay.
func TestCodexRecoverUnknownCallRefused(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, items(`{"type":"shell_call","id":"sc_1","call_id":"call_9","action":{}}`))
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	_, err := NewCodex(src).Base("s")
	var no *NoBase
	if !errors.As(err, &no) || !errors.Is(err, ErrUnusable) || !strings.Contains(no.Why, "shell_call") {
		t.Fatalf("err %v", err)
	}
}

// A turn whose last reply said the turn goes on (end_turn false) did not
// finish, though nothing was asked of a tool.
func TestCodexRecoverEndTurnFalse(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	msg := `{"type":"message","id":"msg_9","role":"assistant","content":[{"type":"output_text","text":"working"}],"end_turn":false}`
	src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, items(msg))
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	plan, in := recoverAfter(t, src)
	if !strings.Contains(plan.Failed, "before the model finished its work") || in[len(in)-3] != msg {
		t.Fatalf("plan %+v", plan)
	}
}

// A first turn that never reached the model: its prompt and a note, then the
// fresh context goes first, since no history holds any.
func TestCodexRecoverFirstTurnNotReached(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.setEnd("t1", store.TurnEnd{Closed: true, Error: "the agent could not be started"})
	plan, in := recoverAfter(t, src)
	if !strings.Contains(plan.Failed, "not acted on") {
		t.Fatalf("plan %+v", plan)
	}
	ctx := context("b", "read-only")
	eqItems(t, "context first", in[:len(ctx)], ctx)
	if in[len(ctx)] != string(openai.UserMessage(p1)) || !strings.Contains(noteText(t, in[len(ctx)+1]), "not acted on") {
		t.Fatalf("not prompt then note: %s", in[len(ctx)])
	}
}

// The response, not its message, set end_turn false (the kept reply's stop
// reason says so), and the child stopped after it: the turn did not finish,
// so the next turn is a recovery with the note.
func TestCodexRecoverResponseEndTurnFalse(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", t1Req(), completedCall, items(answer1))
	rep := src.replies["x1"]
	rep.Stop = openai.ContinuesStop
	src.replies["x1"] = rep
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	plan, in := recoverAfter(t, src)
	if plan.Turn != "t1" || !strings.Contains(plan.Failed, "before the model finished its work") {
		t.Fatalf("plan %+v, want a recovery", plan)
	}
	if in[len(in)-3] != answer1 || !strings.Contains(noteText(t, in[len(in)-2]), "did not finish") {
		t.Fatal("not reply, note, prompt")
	}
}

// A continuation request after an end_turn false reply ends with the model's
// own item, with no new user item. If its reply is cut off, the history is
// that request's items, then the note: a valid layout, never a 409. Every
// valid ending is taken; a client call with no output in the request is not.
func TestCodexRecoverEndsOfAnUnkeptRequest(t *testing.T) {
	cont := `{"type":"message","id":"msg_9","role":"assistant","content":[{"type":"output_text","text":"working"}],"end_turn":false}`
	broken := store.Call{Responded: true, Status: 200, Error: "the stream ended before the response completed"}
	for name, tail := range map[string][]string{
		"assistant message":       {cont},
		"reasoning":               {reasoning1},
		"function_call_output":    {fnCall1, `{"type":"function_call_output","call_id":"call_1","output":"ok"}`},
		"custom_tool_call_output": {toolCall2, toolOut2},
	} {
		src := newSource()
		src.addTurn("t1", p1, "")
		req := codexReq(thread1, append(append(context("a", "read-only"), prompt("a", p1)), tail...)...)
		src.addCodexCall(t, "t1", "x1", req, broken, "")
		src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
		plan, in := recoverAfter(t, src)
		if !strings.Contains(plan.Failed, "did not arrive whole") {
			t.Fatalf("%s: plan %+v", name, plan)
		}
		if in[len(in)-3] != tail[len(tail)-1] || !strings.Contains(noteText(t, in[len(in)-2]), "did not finish") {
			t.Fatalf("%s: not request, note, prompt", name)
		}
	}
	src := newSource()
	src.addTurn("t1", p1, "")
	src.addCodexCall(t, "t1", "x1", codexReq(thread1, append(context("a", "read-only"), prompt("a", p1), fnCall1)...), broken, "")
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	if _, err := NewCodex(src).Base("s"); !errors.Is(err, ErrUnusable) {
		t.Fatalf("a call with no output: %v, want ErrUnusable", err)
	}
}
