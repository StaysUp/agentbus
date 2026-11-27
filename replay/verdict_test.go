package replay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// The compaction verdict is one test (Replayer.compacted), read from the
// request itself. Review of cbb539f: history trusted only a recorded
// history.compacted, so a record from before it, or a call the gateway could
// not judge, was read as uncompacted and the next turn carried the harness's
// summary.

// oldRecord is a first turn per format: one fresh call, then one its harness
// compacted, stored straight into the record as a build before 0.6.7 stored
// them, with no history.compacted.
type oldRecord struct {
	name    string
	newR    func(Source) *Replayer
	store   func(t *testing.T, src *fakeSource)
	next    string // the next turn's fresh first request
	summary string // a word only the harness's summary holds
}

var oldRecords = []oldRecord{
	{name: "claude", newR: New, summary: "HARNESS SUMMARY",
		store: func(t *testing.T, src *fakeSource) {
			src.addTurn("t1", "remember the word kiwi", "")
			src.addCall(t, "t1", "x-0", t1Last, toolReply, "tool_use")
			src.addCall(t, "t1", "x-1", `{"model":"claude-x","messages":[{"role":"user","content":"HARNESS SUMMARY: a word."},{"role":"system","content":"`+sys1+`"}],`+top+`}`, t1Reply, "end_turn")
		},
		next: t2First("which word?")},
	{name: "codex", newR: NewCodex, summary: "Q09NUA==",
		store: func(t *testing.T, src *fakeSource) {
			src.addTurn("t1", p1, "")
			call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{\"command\":\"ls\"}"}`
			src.addCodexCall(t, "t1", "x-0", t1Req(), completedCall, items(call))
			src.addCodexCall(t, "t1", "x-1", codexReq(thread1, append(context("a", "read-only"), prompt("a", p1),
				`{"type":"compaction","encrypted_content":"Q09NUA=="}`)...), completedCall, items(answer2))
		},
		next: codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...)},
	{name: "opencode", newR: NewChatCompletions, summary: "HARNESS SUMMARY",
		store: func(t *testing.T, src *fakeSource) {
			src.addTurn("t1", chatP1, "")
			src.addChatCall(t, "t1", "x-0", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1)), chatTools, chatCall)
			src.addChatCall(t, "t1", "x-1", chatReq("glm", chatSysMsg(chatSys), chatUser("HARNESS SUMMARY"), chatUser(chatP1)), chatStop, chatAns2)
		},
		next: chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))},
}

func (o oldRecord) nextHistory(t *testing.T, src *fakeSource, r *Replayer) string {
	t.Helper()
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	src.order = []string{"t1"}
	if p, err := r.Base("s"); err != nil || p.Turn != "t1" {
		t.Fatalf("%s: base %+v %v", o.name, p, err)
	}
	prompts := map[string]string{"claude": "which word?", "codex": p2, "opencode": chatP2}
	src.addTurn("t2", prompts[o.name], "t1")
	rw := r.Rewrite("s", "t2", []byte(o.next))
	if rw.Body == nil {
		t.Fatalf("%s: not spliced: %+v", o.name, rw.Events)
	}
	return string(rw.Body)
}

// A record from before history.compacted: the compacted call has no verdict
// recorded, and the next turn is still rebuilt from before it.
func TestOldRecordCompactionIsRebuilt(t *testing.T) {
	for _, o := range oldRecords {
		src := newSource()
		o.store(t, src)
		r := o.newR(src)
		body := o.nextHistory(t, src, r)
		if strings.Contains(body, o.summary) || !strings.Contains(body, "compacted its context") {
			t.Errorf("%s: the next turn carries the harness's summary or no note:\n%s", o.name, body)
		}
	}
}

// A call the gateway could not judge: the service restarted inside a first
// turn, so the compacted request came with no main identity in memory and
// no history.compacted was recorded. The next turn is still rebuilt.
func TestUnjudgedCompactionIsRebuilt(t *testing.T) {
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	before := New(src)
	src.through(t, before, "t1", "x-0", t1Last, func(b string) { src.addCall(t, "t1", "x-0", b, toolReply, "tool_use") })
	after := New(src) // the restart: nothing in memory
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":"HARNESS SUMMARY: a word."},{"role":"system","content":"` + sys1 + `"}],` + top + `}`
	rw := src.through(t, after, "t1", "x-1", compacted, func(b string) { src.addCall(t, "t1", "x-1", b, t1Reply, "end_turn") })
	if judgedCompacted(rw) {
		t.Fatal("the restarted gateway judged it: this test needs the unjudged case")
	}
	body := oldRecords[0].nextHistory(t, src, after)
	if strings.Contains(body, "HARNESS SUMMARY") || !strings.Contains(body, "compacted its context") {
		t.Errorf("the next turn carries the harness's summary:\n%s", body)
	}
}

// The gateway's recorded verdict and the history's reading of the record
// agree on every call, in every format: one test, applied to the same bytes.
func TestRecordedVerdictMatchesTheRecord(t *testing.T) {
	type sent struct{ turn, x, req string }
	cases := []struct {
		name string
		newR func(Source) *Replayer
		add  func(t *testing.T, src *fakeSource, turn, x, b string)
		reqs []sent
	}{
		{"claude", New, func(t *testing.T, src *fakeSource, turn, x, b string) {
			src.addCall(t, turn, x, b, okReply, "end_turn")
		},
			[]sent{{"t1", "a", t1Last}, {"t1", "b", `{"model":"claude-x","messages":[{"role":"user","content":"Summary"},{"role":"system","content":"` + sys1 + `"}],` + top + `}`},
				{"t2", "c", t2First("which word?")}, {"t2", "d", t2Loop},
				{"t2", "e", `{"model":"claude-x","messages":[{"role":"user","content":"Summary"},{"role":"system","content":"` + sys2 + `"}],` + top + `}`}}},
		{"codex", NewCodex, func(t *testing.T, src *fakeSource, turn, x, b string) {
			src.addCodexCall(t, turn, x, b, completedCall, items(answer1))
		},
			[]sent{{"t1", "a", t1Req()}, {"t1", "b", codexReq(thread1, append(context("a", "read-only"), prompt("a", p1), `{"type":"compaction","encrypted_content":"QQ=="}`)...)},
				{"t2", "c", codexReq(thread2, append(context("b", "read-only"), prompt("b", "which word?"))...)},
				{"t2", "d", codexReq(thread2, append(context("b", "read-only"), prompt("b", "which word?"), `{"type":"compaction","encrypted_content":"QQ=="}`)...)}}},
		{"opencode", NewChatCompletions, func(t *testing.T, src *fakeSource, turn, x, b string) {
			src.addChatCall(t, turn, x, b, chatStop, chatAsst)
		},
			[]sent{{"t1", "a", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1))}, {"t1", "b", chatReq("glm", chatSysMsg(chatSys), chatUser("S"), chatUser(chatP1))},
				{"t2", "c", chatReq("glm", chatSysMsg(chatSys), chatUser("which word?"))}, {"t2", "d", chatReq("glm", chatSysMsg(chatSys), chatUser("S"), chatUser("which word?"))}}},
	}
	for _, c := range cases {
		src := newSource()
		prompts := map[string]string{"t1": map[string]string{"claude": "remember the word kiwi", "codex": p1, "opencode": chatP1}[c.name], "t2": "which word?"}
		src.addTurn("t1", prompts["t1"], "")
		r := c.newR(src)
		verdict := map[string]bool{}
		for _, s := range c.reqs {
			if s.turn == "t2" && src.turns["t2"].prompt == "" {
				src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
				src.addTurn("t2", prompts["t2"], "t1")
			}
			rw := src.through(t, r, s.turn, s.x, s.req, func(b string) { c.add(t, src, s.turn, s.x, b) })
			verdict[s.x] = judgedCompacted(rw)
		}
		for _, turn := range []string{"t1", "t2"} {
			for _, sc := range src.turns[turn].exchanges {
				cc := src.calls[sc.Exchange]
				msgs, id, tools := r.f.read(cc)
				ti, _ := r.turn("s", turn)
				got := r.compacted(call{a: sc, c: cc, msgs: msgs, id: id, tools: tools}, ti)
				if got != verdict[sc.Exchange] {
					t.Errorf("%s %s: the record reads compacted=%v, the gateway recorded %v", c.name, sc.Exchange, got, verdict[sc.Exchange])
				}
			}
		}
		if !verdict["b"] {
			t.Errorf("%s: the compacted request was not judged", c.name)
		}
	}
}

// fit records no step that changed nothing: a history over the budget whose
// turns cannot be found by their person's message is returned as it is,
// with no history.shortened.
func TestFitWithoutTurnsRecordsNothing(t *testing.T) {
	src := newSource()
	src.addTurn("t1", "a prompt the history does not hold", "")
	r := New(src)
	r.SetBudget(func() float64 { return 0.000001 })
	h := &history{messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":` + jsonQuote(bigOut) + `}`)}}
	got, step := r.fit("s", "t1", h, "claude-x")
	if step != nil || got != h {
		t.Errorf("step %+v, want none", step)
	}
}
