package replay

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// A turn that did not finish never ends the session: the next turn continues
// from it, with what it did made valid for the API and one note saying how it
// ended. These tests follow a session past such a turn for two more turns.

// replayTurn starts turn id after the session's newest turn, as the API does:
// Base names the turn it continues. Its first request (fresh layout) is
// spliced; the spliced request is stored as the turn's call, with reply and
// stop as the vendor answered. It returns the spliced request's messages.
func replayTurn(t *testing.T, src *fakeSource, r *Replayer, id, prompt, reply, stop string, status int) (Plan, []json.RawMessage) {
	t.Helper()
	plan, err := r.Base("s")
	if err != nil {
		t.Fatalf("%s: base: %v", id, err)
	}
	src.addTurn(id, prompt, plan.Turn)
	rw := r.Rewrite("s", id, []byte(t2First(prompt)))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("%s: refused or unchanged: %+v", id, rw.Events)
	}
	x := "x-" + id
	src.addCall(t, id, x, string(rw.Body), reply, stop)
	if status != 200 {
		ft := src.turns[id]
		ft.exchanges[0].Status, ft.exchanges[0].Stop = status, ""
		src.turns[id] = ft
		delete(src.replies, x)
	}
	return plan, mustElements(t, field(rw.Body, "messages"))
}

func msgText(ms []json.RawMessage) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = string(m)
	}
	return out
}

func roles(ms []json.RawMessage) string {
	var rs []string
	for _, m := range ms {
		rs = append(rs, role(m))
	}
	return strings.Join(rs, ",")
}

func countIn(ms []json.RawMessage, s string) int {
	n := 0
	for _, m := range ms {
		n += strings.Count(string(m), s)
	}
	return n
}

// stripped is a message without cache_control, as a later request carries it.
func stripped(t *testing.T, m json.RawMessage) string {
	t.Helper()
	h, err := asHistory(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// checkPrefix asserts that later's messages start with earlier's, once
// cache_control is removed: the history is a stable prefix, so the cache
// keeps working after a recovery.
func checkPrefix(t *testing.T, what string, earlier, later []json.RawMessage, n int) {
	t.Helper()
	if len(later) < n || len(earlier) < n {
		t.Fatalf("%s: %d and %d messages, want at least %d", what, len(earlier), len(later), n)
	}
	for i := 0; i < n; i++ {
		if a, b := stripped(t, earlier[i]), stripped(t, later[i]); a != b {
			t.Fatalf("%s: message %d differs\n%s\n%s", what, i, a, b)
		}
	}
}

const (
	okReply   = `{"role":"assistant","content":[{"type":"text","text":"Done."}]}`
	toolReply = `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lHLXRvb2w="},` +
		`{"type":"text","text":"Running the tests."},` +
		`{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"go test ./..."}},` +
		`{"type":"server_tool_use","id":"srvtoolu_b","name":"web_search","input":{}},` +
		`{"type":"tool_use","id":"toolu_c","name":"Bash","input":{"command":"ls"}}]}`
)

// A turn killed inside its tool loop (exit 143: the child was stopped, or the
// service restarted). The next turn gets that turn's last request, its reply
// with the tool calls, an error tool_result for each client tool_use (not the
// server tool), and the note; it is told plainly. Two more turns after it
// replay all of it as a stable prefix, with the note once.
func TestRecoverKilledInToolLoop(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})

	_, m2 := replayTurn(t, src, r, "t2", "run the tests", toolReply, "tool_use", 200)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(143)})

	plan, m3 := replayTurn(t, src, r, "t3", "what happened?", okReply, "end_turn", 200)
	src.setEnd("t3", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if plan.Turn != "t2" || !strings.Contains(plan.Failed, "it stopped while a tool was running") ||
		!strings.Contains(plan.Failed, "exit 143") {
		t.Fatalf("plan %+v, want t2 failed inside its tool loop", plan)
	}
	// t2's request as history, then its reply, the tool results, the note.
	checkPrefix(t, "t3 after t2's request", m2, m3, len(m2))
	n := len(m2)
	if got := stripped(t, m3[n]); got != toolReply {
		t.Errorf("reply\n got %s\nwant %s", got, toolReply)
	}
	wantResults := `{"role":"user","content":[` +
		`{"tool_use_id":"toolu_a","type":"tool_result","content":"Not finished: the turn was interrupted before this tool's result came back. It may have run in part.","is_error":true},` +
		`{"tool_use_id":"toolu_c","type":"tool_result","content":"Not finished: the turn was interrupted before this tool's result came back. It may have run in part.","is_error":true}]}`
	if got := string(m3[n+1]); got != wantResults {
		t.Errorf("tool results\n got %s\nwant %s", got, wantResults)
	}
	note := stripped(t, m3[n+2])
	wantNote := `{"role":"assistant","content":[{"type":"text","text":"[Note from AgentBus, not written by the model] The turn above did not finish: ` +
		`it stopped while a tool was running; its agent process was stopped (signal 15, exit 143), as when it is stopped or the service restarts. ` +
		`It may have changed files in part. Check the folder before going on."}]}`
	if note != wantNote {
		t.Errorf("note\n got %s\nwant %s", note, wantNote)
	}
	// The note carries the breakpoint of t3's first request: the whole
	// recovered history is read from cache from then on.
	if field(mustElements(t, field(m3[n+2], "content"))[0], "cache_control") == nil {
		t.Error("no breakpoint on the note")
	}
	if got := strings.Join(msgText(m3[n+3:]), ","); !strings.HasPrefix(got, `{"role":"user","content":"what happened?"}`) {
		t.Errorf("t3's own messages %s", got)
	}

	// Two more turns: each replays the last as before, the recovered part
	// unchanged.
	plan4, m4 := replayTurn(t, src, r, "t4", "go on", okReply, "end_turn", 200)
	src.setEnd("t4", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if plan4.Turn != "t3" || plan4.Failed != "" {
		t.Errorf("t4's plan %+v, want t3, finished", plan4)
	}
	checkPrefix(t, "t4 after t3", m3, m4, len(m3))
	plan5, m5 := replayTurn(t, src, r, "t5", "and once more", okReply, "end_turn", 200)
	if plan5.Turn != "t4" || plan5.Failed != "" {
		t.Errorf("t5's plan %+v, want t4, finished", plan5)
	}
	checkPrefix(t, "t5 after t4", m4, m5, len(m4))
	for _, w := range []string{"The turn above did not finish", `"tool_use_id":"toolu_a"`} {
		if c := countIn(m5, w); c != 1 {
			t.Errorf("t5 carries %q %d times, want once", w, c)
		}
	}
	if !strings.Contains(strings.Join(msgText(m5), ""), `"what happened?"`) ||
		!strings.Contains(strings.Join(msgText(m5), ""), `"go on"`) {
		t.Error("t5 lost an earlier prompt")
	}
}

// A turn whose last model call failed (400, 529) or got no reply: its last
// request is replayed with no reply, and the note gives the status. Two more
// turns follow it.
func TestRecoverFailedLastCall(t *testing.T) {
	// 502 too: a vendor the gateway could not reach is still worded as a
	// failed request, unlike a call the agent closed (TestRecoverStoppedMidCall).
	for _, status := range []int{400, 502, 529} {
		src, r := twoTurns(t)
		delete(src.turns, "t2")
		src.order = src.order[1:]
		src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
		_, m2 := replayTurn(t, src, r, "t2", "do it", "", "", status)
		src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})

		plan, m3 := replayTurn(t, src, r, "t3", "what happened?", okReply, "end_turn", 200)
		src.setEnd("t3", store.TurnEnd{Closed: true, ExitCode: intp(0)})
		want := "its last model request failed with status " + itoa(status) + "; its agent process exited with code 1"
		if plan.Turn != "t2" || plan.Failed != want {
			t.Errorf("%d: plan %+v, want t2 failed with %q", status, plan, want)
		}
		checkPrefix(t, "t3 after t2", m2, m3, len(m2))
		if got := stripped(t, m3[len(m2)]); !strings.Contains(got, "The turn above did not finish: "+want+".") || role(m3[len(m2)]) != "assistant" {
			t.Errorf("%d: note %s", status, got)
		}
		// user, system, ..., user, system, assistant (note), user, system:
		// t2's request ended with its system message, then the note.
		if got := roles(m3[len(m2)-2:]); got != "user,system,assistant,user,system" {
			t.Errorf("%d: roles %s", status, got)
		}
		_, m4 := replayTurn(t, src, r, "t4", "go on", okReply, "end_turn", 200)
		src.setEnd("t4", store.TurnEnd{Closed: true, ExitCode: intp(0)})
		checkPrefix(t, "t4 after t3", m3, m4, len(m3))
		_, m5 := replayTurn(t, src, r, "t5", "more", okReply, "end_turn", 200)
		checkPrefix(t, "t5 after t4", m4, m5, len(m4))
		if c := countIn(m5, "The turn above did not finish"); c != 1 {
			t.Errorf("%d: note %d times in t5, want once", status, c)
		}
	}
}

// A turn stopped while its model call was in flight: the agent closed the
// connection before the vendor replied, and the gateway stored the call with
// no status (gateway.AgentClosed). The next turn's note says the turn was
// stopped and keeps the call's error text; it names no status, so the model
// does not tell the person the vendor failed (acceptance soak, 2026-11-06:
// "the turn ended with a 502").
func TestRecoverStoppedMidCall(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	_, m2 := replayTurn(t, src, r, "t2", "sleep a while", "", "", 200)
	ft := src.turns["t2"]
	ft.exchanges[0].Status, ft.exchanges[0].Stop, ft.exchanges[0].Error = 0, "", "the agent closed the connection before the vendor replied"
	src.turns["t2"] = ft
	delete(src.replies, "x-t2")
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(143)})

	plan, m3 := replayTurn(t, src, r, "t3", "what happened?", okReply, "end_turn", 200)
	want := "it was stopped while its last model request was in flight (the agent closed the connection before the vendor replied), " +
		"so that request got no reply; its agent process was stopped (signal 15, exit 143), as when it is stopped or the service restarts"
	if plan.Turn != "t2" || plan.Failed != want {
		t.Errorf("plan %+v, want t2 failed with %q", plan, want)
	}
	checkPrefix(t, "t3 after t2", m2, m3, len(m2))
	note := stripped(t, m3[len(m2)])
	if !strings.Contains(note, "The turn above did not finish: "+want+".") || role(m3[len(m2)]) != "assistant" {
		t.Errorf("note %s", note)
	}
	if strings.Contains(note, "status") || strings.Contains(note, "502") {
		t.Errorf("note %s names a status for a call no vendor answered", note)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A session's first turn fails on its only call (529): the second turn
// continues from its request, with the note, and a third and fourth follow.
func TestRecoverFailedFirstTurn(t *testing.T) {
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	src.addCall(t, "t1", "x-1", t2First("remember the word kiwi"), "", "")
	ft := src.turns["t1"]
	ft.exchanges[0].Status, ft.exchanges[0].Stop = 529, ""
	src.turns["t1"] = ft
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(1)})
	r := New(src)

	plan, m2 := replayTurn(t, src, r, "t2", "are you there?", okReply, "end_turn", 200)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if plan.Turn != "t1" || !strings.HasPrefix(plan.Failed, "its last model request failed with status 529") {
		t.Fatalf("plan %+v", plan)
	}
	if got := roles(m2); got != "user,system,assistant,user,system" {
		t.Errorf("roles %s", got)
	}
	if !strings.Contains(string(m2[0]), "remember the word kiwi") {
		t.Errorf("first message %s, want turn 1's", m2[0])
	}
	_, m3 := replayTurn(t, src, r, "t3", "go on", okReply, "end_turn", 200)
	src.setEnd("t3", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	checkPrefix(t, "t3 after t2", m2, m3, len(m2))
	_, m4 := replayTurn(t, src, r, "t4", "more", okReply, "end_turn", 200)
	checkPrefix(t, "t4 after t3", m3, m4, len(m3))
}

// A turn that failed before its agent made any model call: the next turn
// continues from the turn before it, then the person's message of the failed
// turn and a note that it was not acted on. A first turn with no call has no
// turn before it. Two more turns follow each.
func TestRecoverTurnWithNoCall(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	m1 := mustElements(t, field([]byte(t1Last), "messages"))

	src.addTurn("t2", "fix the parser", "t1")
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})

	plan, m3 := replayTurn(t, src, r, "t3", "did you fix it?", okReply, "end_turn", 200)
	src.setEnd("t3", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if plan.Turn != "t2" || plan.Failed != "it was not acted on: it ended before the model was called (its agent process exited with code 1)" {
		t.Fatalf("plan %+v", plan)
	}
	// t1's whole history, its reply, then t2's message and the note.
	checkPrefix(t, "t3 after t1", m1, m3, len(m1))
	n := len(m1)
	if got := stripped(t, m3[n]); got != t1Reply {
		t.Errorf("t1's reply %s", got)
	}
	if got := string(m3[n+1]); got != `{"role":"user","content":"fix the parser"}` {
		t.Errorf("t2's message %s", got)
	}
	wantNote := `{"role":"assistant","content":[{"type":"text","text":"[Note from AgentBus, not written by the model] The message above was not acted on: it ended before the model was called (its agent process exited with code 1)."}]}`
	if got := stripped(t, m3[n+2]); got != wantNote {
		t.Errorf("note\n got %s\nwant %s", got, wantNote)
	}
	_, m4 := replayTurn(t, src, r, "t4", "go on", okReply, "end_turn", 200)
	src.setEnd("t4", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	checkPrefix(t, "t4 after t3", m3, m4, len(m3))
	_, m5 := replayTurn(t, src, r, "t5", "more", okReply, "end_turn", 200)
	checkPrefix(t, "t5 after t4", m4, m5, len(m4))
	if c := countIn(m5, `"fix the parser"`); c != 1 {
		t.Errorf("t2's message %d times in t5, want once", c)
	}

	// A first turn that could not start, then a second.
	src2 := newSource()
	src2.addTurn("t1", "remember the word kiwi", "")
	src2.setEnd("t1", store.TurnEnd{Closed: true, Error: "the agent could not be started"})
	r2 := New(src2)
	plan, m2 := replayTurn(t, src2, r2, "t2", "are you there?", okReply, "end_turn", 200)
	src2.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if plan.Turn != "t1" {
		t.Fatalf("plan %+v", plan)
	}
	want := `{"role":"user","content":"remember the word kiwi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"[Note from AgentBus, not written by the model] The message above was not acted on: it ended before the model was called (the agent could not be started)."}]}`
	if got := stripped(t, m2[0]) + "," + stripped(t, m2[1]); got != want {
		t.Errorf("history\n got %s\nwant %s", got, want)
	}
	// No earlier turn reached the model, so no history holds the
	// environment: the fresh system message is kept whole, and the next
	// turn still finds the main thread by its layout.
	if len(m2) != 4 || role(m2[3]) != "system" || !strings.Contains(string(m2[3]), `# Environment\nenv two`) {
		t.Errorf("turn 2's system message was cut: %s", strings.Join(msgText(m2), "\n"))
	}
	_, m3b := replayTurn(t, src2, r2, "t3", "go on", okReply, "end_turn", 200)
	checkPrefix(t, "t3 after t2", m2, m3b, len(m2))

	// A turn with no call that ended normally (exit 0) did run: its calls are
	// simply not stored, as in a session from before calls were. There is
	// no history; the API may continue it by the vendor's resume.
	src3 := newSource()
	src3.addTurn("t1", "remember the word kiwi", "")
	src3.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if _, err := New(src3).Base("s"); err == nil || !strings.Contains(err.Error(), "ended normally but none of its model calls are stored") {
		t.Errorf("base err %v, want a refusal", err)
	}
}

// A turn that was killed while its last reply streamed: the reply's stop
// reason is empty, so the cut-off reply is left out, never replayed half.
func TestRecoverCutOffReplyIsLeftOut(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	_, m2 := replayTurn(t, src, r, "t2", "do it", `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}]}`, "", 200)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(143)})
	plan, m3 := replayTurn(t, src, r, "t3", "what happened?", okReply, "end_turn", 200)
	if !strings.HasPrefix(plan.Failed, "its last reply was cut off") {
		t.Errorf("plan %+v", plan)
	}
	if countIn(m3, "toolu_x") != 0 {
		t.Error("the cut-off reply was replayed")
	}
	if got := roles(m3[len(m2)-1 : len(m2)+1]); got != "system,assistant" {
		t.Errorf("roles %s, want the note right after t2's request", got)
	}
}

// What still refuses says why, naming the turn: a turn that may be missing
// events, and a compacted last request.
func TestRefusalsSayWhy(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	ft := src.turns["t1"]
	ft.incomplete = true
	src.turns["t1"] = ft
	_, err := r.Base("s")
	var no *NoBase
	if !errors.As(err, &no) || !errors.Is(err, ErrUnusable) || !strings.Contains(no.Why, "turn t1 may be missing events") {
		t.Errorf("err %v", err)
	}
	src.turns["t1"] = fakeTurn{prompt: ft.prompt}
	src.addCall(t, "t1", "x-c", `{"model":"claude-x","messages":[{"role":"user","content":"Summary: a word."},{"role":"system","content":"`+sys1+`"}],`+top+`}`, t1Reply, "end_turn")
	src.turns["t1"] = fakeTurn{prompt: ft.prompt, exchanges: append(src.turns["t1"].exchanges, ft.exchanges...)}
	if _, err := r.Base("s"); !errors.As(err, &no) || !strings.Contains(no.Why, "turn t1's last main-thread request x-c no longer carries the turn's prompt") {
		t.Errorf("err %v", err)
	}
}

// A stream that ended after message_delta but before message_stop keeps its
// stop reason (end_turn or tool_use), but its model.responded carries an
// error and its reply is never kept. It is no usable reply: the turn is
// recovered with a note, never ErrPending.
func TestRecoverIncompleteStream(t *testing.T) {
	for _, stop := range []string{"end_turn", "tool_use"} {
		src, r := twoTurns(t)
		delete(src.turns, "t2")
		src.order = src.order[1:]
		src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
		_, m2 := replayTurn(t, src, r, "t2", "do it", "", stop, 200)
		ft := src.turns["t2"]
		ft.exchanges[0].Error = "the stream ended before message_stop"
		src.turns["t2"] = ft
		src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})

		plan, err := r.Base("s")
		want := "its last reply did not arrive whole (the stream ended before message_stop), so that reply is left out; its agent process exited with code 1"
		if err != nil || plan.Turn != "t2" || plan.Failed != want {
			t.Fatalf("%s: plan %+v %v, want t2 failed with %q", stop, plan, err, want)
		}
		_, m3 := replayTurn(t, src, r, "t3", "what happened?", okReply, "end_turn", 200)
		checkPrefix(t, "t3 after t2", m2, m3, len(m2))
		if got := stripped(t, m3[len(m2)]); role(m3[len(m2)]) != "assistant" || !strings.Contains(got, "The turn above did not finish: "+want+".") {
			t.Errorf("%s: note %s", stop, got)
		}
		if countIn(m3, "tool_result\",\"content\":\"Not finished") != 0 {
			t.Errorf("%s: a tool_result was made for a reply that was left out", stop)
		}
	}
}

// A turn whose stored request is not in the main layout (here, no system
// message) but carries the prompt and tools may be a real main call. It is
// refused, never read as "not acted on", which would drop its tool results
// and reply. A turn with only side calls (no tools, no prompt) still never
// reached the model.
func TestUnknownLayoutIsNotNotActedOn(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t2", "fix the parser", "t1")
	noSystem := `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"fix the parser"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_u","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"tool_use_id":"toolu_u","type":"tool_result","content":"ok"}]}],` + top + `}`
	src.addCall(t, "t2", "x-u", noSystem, `{"role":"assistant","content":[{"type":"text","text":"Fixed."}]}`, "end_turn")
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})
	_, err := r.Base("s")
	var no *NoBase
	if !errors.As(err, &no) || !errors.Is(err, ErrUnusable) || !strings.Contains(no.Why, "turn t2's call x-u is in a layout the gateway does not know") {
		t.Fatalf("base err %v, want a refusal naming the layout", err)
	}
	src.addTurn("t3", "did you fix it?", "t2")
	if rw := r.Rewrite("s", "t3", []byte(t2First("did you fix it?"))); rw.Refuse == nil || !strings.Contains(rw.Events[0].Error, "layout the gateway does not know") {
		t.Errorf("t3 spliced %s, want replay.failed", rw.Body)
	}

	// Only a side call (no tools, no prompt), then a failure: not acted on.
	src2, r2 := twoTurns(t)
	delete(src2.turns, "t2")
	src2.order = src2.order[1:]
	src2.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src2.addTurn("t2", "fix the parser", "t1")
	src2.addCall(t, "t2", "x-q", `{"model":"claude-x","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`, "", "end_turn")
	src2.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})
	if plan, err := r2.Base("s"); err != nil || plan.Turn != "t2" || !strings.HasPrefix(plan.Failed, "it was not acted on") {
		t.Errorf("plan %+v %v, want t2 not acted on", plan, err)
	}
}

// A turn the person stopped (session.closed's stopped) continues like one
// stopped any other way, and its note says the person stopped it.
func TestRecoverStoppedByThePerson(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	_, m2 := replayTurn(t, src, r, "t2", "run the tests", toolReply, "tool_use", 200)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(143), Stopped: true})
	plan, m3 := replayTurn(t, src, r, "t3", "what happened?", okReply, "end_turn", 200)
	if plan.Turn != "t2" || plan.Failed != "it stopped while a tool was running; the person stopped this turn" {
		t.Fatalf("plan %+v", plan)
	}
	wantNote := `{"role":"assistant","content":[{"type":"text","text":"[Note from AgentBus, not written by the model] The turn above did not finish: ` +
		`it stopped while a tool was running; the person stopped this turn. ` +
		`It may have changed files in part. Check the folder before going on."}]}`
	if note := stripped(t, m3[len(m2)+2]); note != wantNote {
		t.Errorf("note\n got %s\nwant %s", note, wantNote)
	}
}

// A turn the person stopped before it made any model call, whose agent still
// exited 0, was not acted on: the next turn continues, told so. Without
// stopped, that end is refused as one whose calls are missing.
func TestRecoverStoppedBeforeAnyCall(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.addTurn("t2", "fix the parser", "t1")
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(0), Stopped: true})
	plan, _ := replayTurn(t, src, r, "t3", "did you fix it?", okReply, "end_turn", 200)
	if plan.Turn != "t2" || plan.Failed != "it was not acted on: it ended before the model was called (the person stopped this turn)" {
		t.Fatalf("plan %+v", plan)
	}
}

// A turn stopped after its last reply finished continues as a finished turn:
// its history is whole, with no note.
func TestStoppedAfterFinishedReplyIsWhole(t *testing.T) {
	src, r := twoTurns(t)
	delete(src.turns, "t2")
	src.order = src.order[1:]
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	replayTurn(t, src, r, "t2", "say hi", okReply, "end_turn", 200)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(143), Stopped: true})
	plan, m3 := replayTurn(t, src, r, "t3", "go on", okReply, "end_turn", 200)
	if plan.Turn != "t2" || plan.Failed != "" {
		t.Fatalf("plan %+v, want t2, finished", plan)
	}
	if c := countIn(m3, "Note from AgentBus"); c != 0 {
		t.Errorf("%d notes, want none", c)
	}
}
