package replay

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter/chatcompletions"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/store"
)

// Synthetic fixtures in OpenCode 1.19.6's Chat Completions layout
// (@ai-sdk/openai-compatible on OpenRouter, 2026-11-21), never the saved
// real bodies: system, then the prompt as a user message, then the turn's
// tool loop. A fresh process has no earlier assistant or tool messages.

const (
	chatSys  = "You are opencode."
	chatSub  = "You are a subagent."
	chatP1   = "Remember the word kiwi."
	chatP2   = "Which word did I ask you to remember?"
	chatP3   = "List the files."
	chatAsst = `{"role":"assistant","content":"Noted: kiwi."}`
	chatCall = `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}`
	chatOut  = `{"role":"tool","tool_call_id":"call_1","content":"a.txt"}`
	chatAns2 = `{"role":"assistant","content":"kiwi"}`
)

func chatSysMsg(s string) string { return `{"role":"system","content":` + jsonQuote(s) + `}` }
func chatUser(s string) string   { return `{"role":"user","content":` + jsonQuote(s) + `}` }

func chatReq(model string, msgs ...string) string {
	return `{"model":"` + model + `","messages":[` + strings.Join(msgs, ",") + `],` +
		`"tools":[{"type":"function","function":{"name":"bash"}}],"stream":true,"stream_options":{"include_usage":true}}`
}

func (f *fakeSource) addChatCall(t *testing.T, turn, exchange, request string, c store.Call, reply string) {
	t.Helper()
	req, err := chatcompletions.Adapter{}.Split([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	cc := store.CallContext{Exchange: exchange, Vendor: "chatcompletions", Request: req.Top}
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

var chatStop = store.Call{Responded: true, Status: 200, Stop: "stop"}
var chatTools = store.Call{Responded: true, Status: 200, Stop: "tool_calls"}

func chatTwoTurns(t *testing.T) (*fakeSource, *Replayer) {
	src := newSource()
	src.addTurn("t1", chatP1, "")
	src.addChatCall(t, "t1", "x1", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1)), chatStop, chatAsst)
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t2", chatP2, "t1")
	return src, NewChatCompletions(src)
}

func messagesOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	for _, e := range mustElements(t, json.RawMessage(field2(t, raw, "messages"))) {
		out = append(out, string(e))
	}
	return out
}

// A replayed turn's first request: turn 1's messages and reply in front of
// the fresh prompt, the fresh system dropped, byte for byte.
func TestChatCompletionsSplice(t *testing.T) {
	_, r := chatTwoTurns(t)
	fresh := chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))
	rw := r.Rewrite("s", "t2", []byte(fresh))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("not spliced: %+v %v", rw.Events, rw.Refuse)
	}
	want := []string{chatSysMsg(chatSys), chatUser(chatP1), chatAsst, chatUser(chatP2)}
	eqItems(t, "messages", messagesOf(t, rw.Body), want)
	if rw.Replayed != 3 {
		t.Fatalf("replayed %d, want 3", rw.Replayed)
	}
}

// A multi-request turn: every request gets the same history, then the turn's
// own tool loop, so the prefix stays the same between requests.
func TestChatCompletionsToolLoop(t *testing.T) {
	_, r := chatTwoTurns(t)
	first := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))))
	loop := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2), chatCall, chatOut)))
	if first.Body == nil || loop.Body == nil {
		t.Fatal("not spliced")
	}
	a, b := messagesOf(t, first.Body), messagesOf(t, loop.Body)
	eqItems(t, "prefix", b[:len(a)], a)
	eqItems(t, "loop", b[len(a):], []string{chatCall, chatOut})
}

// Turn 3 replays turn 2's last request, itself spliced, and its reply.
func TestChatCompletionsChainedTurns(t *testing.T) {
	src, r := chatTwoTurns(t)
	loop := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2), chatCall, chatOut)))
	src.addChatCall(t, "t2", "x2", string(loop.Body), chatStop, chatAns2)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t3", chatP3, "t2")
	rw := NewChatCompletions(src).Rewrite("s", "t3", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP3))))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	want := append(append(messagesOf(t, loop.Body), chatAns2), chatUser(chatP3))
	eqItems(t, "messages", messagesOf(t, rw.Body), want)
}

func refusedChat(t *testing.T, rw gateway.Rewrite, want string) {
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
// history; a main request the harness compacted passes as sent, recorded
// once; another system prompt (a subagent) passes unchanged.
func TestChatCompletionsLayouts(t *testing.T) {
	t.Run("compacted main request", func(t *testing.T) {
		_, r := chatTwoTurns(t)
		if rw := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2)))); rw.Body == nil {
			t.Fatal("first request not spliced")
		}
		compacted := chatReq("glm", chatSysMsg(chatSys), chatUser("summary of earlier turns"))
		passedCompacted(t, r.Rewrite("s", "t2", []byte(compacted)))
		passedCompacted(t, r.Rewrite("s", "t2", []byte(compacted)))
	})
	t.Run("another system prompt passes", func(t *testing.T) {
		_, r := chatTwoTurns(t)
		if rw := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2)))); rw.Body == nil {
			t.Fatal("first request not spliced")
		}
		sub := chatReq("glm", chatSysMsg(chatSub), chatUser(chatP2))
		if rw := r.Rewrite("s", "t2", []byte(sub)); rw.Body != nil || rw.Refuse != nil {
			t.Fatalf("a subagent's request was changed: %+v", rw)
		}
	})
	t.Run("not the chat completions format", func(t *testing.T) {
		_, r := chatTwoTurns(t)
		if rw := r.Rewrite("s", "t2", []byte(`{"model":"gpt-x","input":[]}`)); rw.Body != nil || rw.Refuse != nil {
			t.Fatalf("a body with no messages was changed: %+v", rw)
		}
	})
}

// Killed in its tool loop: the last reply asked for a tool and the agent was
// stopped. The call gets a tool result marked not finished, then the note,
// last, before the new prompt.
func TestChatCompletionsRecoverKilledInToolLoop(t *testing.T) {
	src := newSource()
	src.addTurn("t1", chatP1, "")
	src.addChatCall(t, "t1", "x1", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1)), chatTools, chatCall)
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	r := NewChatCompletions(src)
	plan, err := r.Base("s")
	if err != nil {
		t.Fatalf("Base: %v", err)
	}
	src.addTurn("t2", chatP2, "t1")
	rw := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	if plan.Turn != "t1" || !strings.Contains(plan.Failed, "it stopped while a tool was running") {
		t.Fatalf("plan %+v", plan)
	}
	got := messagesOf(t, rw.Body)
	n := len(got)
	if got[n-1] != chatUser(chatP2) {
		t.Fatal("the prompt is not last")
	}
	note := got[n-2]
	if body.Label(json.RawMessage(note), "role") != "user" || !strings.Contains(note, notePrefix) ||
		!strings.Contains(note, "signal 15, exit 143") || !strings.Contains(note, "Check the folder") {
		t.Fatalf("note: %s", note)
	}
	want := []string{chatCall, `{"role":"tool","tool_call_id":"call_1","content":` + jsonQuote(notFinished) + `}`}
	eqItems(t, "reply and results", got[n-2-len(want):n-2], want)
}

// A request to another model drops the history's reasoning fields, which
// only the model that made them can read.
func TestChatCompletionsSpliceForAnotherModel(t *testing.T) {
	src := newSource()
	withReason := `{"role":"assistant","content":"Noted: kiwi.","reasoning_content":"secret"}`
	src.addTurn("t1", chatP1, "")
	src.addChatCall(t, "t1", "x1", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1)), chatStop, withReason)
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t2", chatP2, "t1")
	r := NewChatCompletions(src)
	fresh := strings.Replace(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2)), `"model":"glm"`, `"model":"kimi"`, 1)
	rw := r.Rewrite("s", "t2", []byte(fresh))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	got := messagesOf(t, rw.Body)
	for _, m := range got {
		if strings.Contains(m, "reasoning_content") {
			t.Fatalf("reasoning kept for another model: %s", m)
		}
	}
	if !strings.Contains(strings.Join(got, "\n"), `"content":"Noted: kiwi."`) {
		t.Fatalf("conversation dropped: %s", got)
	}
}

func TestChatCompletionsIdentityRecovered(t *testing.T) {
	src, r := chatTwoTurns(t)
	first := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))))
	src.addChatCall(t, "t2", "x2", string(first.Body), chatTools, chatCall)
	loop := chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2), chatCall, chatOut)
	rw := NewChatCompletions(src).Rewrite("s", "t2", []byte(loop))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("identity not recovered: %+v", rw.Events)
	}
}

func TestChatCompletionsHistoryHasNoSystem(t *testing.T) {
	src := newSource()
	src.addTurn("t1", chatP1, "")
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(1), Error: "the program exited"})
	src.addTurn("t2", chatP2, "t1")
	r := NewChatCompletions(src)
	rw := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	got := messagesOf(t, rw.Body)
	if got[0] != chatSysMsg(chatSys) {
		t.Fatalf("fresh system dropped when history had none: %s", got)
	}
}

func TestChatCompletionsHeaderUnchanged(t *testing.T) {
	_, r := chatTwoTurns(t)
	rw := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))))
	if rw.Body == nil {
		t.Fatal("not spliced")
	}
	if rw.Header != nil {
		h := http.Header{}
		rw.Header(h)
		t.Fatalf("headers rewritten: %v", h)
	}
}
