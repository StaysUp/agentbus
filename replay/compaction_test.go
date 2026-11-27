package replay

import (
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// A harness that compacts inside one turn is let through, and the next turn
// is built from the record, never from the harness's summary: the newest
// request from before the compaction whose reply asked for tools, those
// tools' results as "ran, in the record", then a note with the model's last
// words. One test per format (Claude Code's is
// TestCompactedTurnIsRebuiltFromTheRecord).

func TestCodexCompactedTurnIsRebuiltFromTheRecord(t *testing.T) {
	src := newSource()
	src.addTurn("t1", p1, "")
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{\"command\":\"ls\"}"}`
	r := NewCodex(src)
	src.through(t, r, "t1", "x0", t1Req(), func(b string) { src.addCodexCall(t, "t1", "x0", b, completedCall, items(call)) })
	compacted := codexReq(thread1, append(context("a", "read-only"), prompt("a", p1),
		`{"type":"compaction","encrypted_content":"Q09NUA=="}`)...)
	src.through(t, r, "t1", "x1", compacted, func(b string) { src.addCodexCall(t, "t1", "x1", b, completedCall, items(answer2)) })
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if p, err := r.Base("s"); err != nil || p.Turn != "t1" || p.Failed != "" {
		t.Fatalf("base %+v %v", p, err)
	}
	src.addTurn("t2", p2, "t1")
	rw := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...)))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	in := strings.Join(inputOf(t, rw.Body), "\n")
	if strings.Contains(in, `"compaction"`) || !strings.Contains(in, `"call_id":"call_1"`) ||
		!strings.Contains(in, compactedResult) || !strings.Contains(in, "compacted its context") || !strings.Contains(in, "kiwi") {
		t.Errorf("input:\n%s", in)
	}
	codexSession.paired(t, mustElements(t, field(rw.Body, "input")))
}

func TestChatCompletionsCompactedTurnIsRebuiltFromTheRecord(t *testing.T) {
	src := newSource()
	src.addTurn("t1", chatP1, "")
	r := NewChatCompletions(src)
	src.through(t, r, "t1", "x0", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1)), func(b string) { src.addChatCall(t, "t1", "x0", b, chatTools, chatCall) })
	src.through(t, r, "t1", "x1", chatReq("glm", chatSysMsg(chatSys), chatUser("summary of the turn so far")), func(b string) {
		src.addChatCall(t, "t1", "x1", b, chatStop, chatAns2)
	})
	src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
	if p, err := r.Base("s"); err != nil || p.Turn != "t1" || p.Failed != "" {
		t.Fatalf("base %+v %v", p, err)
	}
	src.addTurn("t2", chatP2, "t1")
	rw := r.Rewrite("s", "t2", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP2))))
	if rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	msgs := messagesOf(t, rw.Body)
	all := strings.Join(msgs, "\n")
	if strings.Contains(all, "summary of the turn so far") || !strings.Contains(all, compactedResult) ||
		!strings.Contains(all, "compacted its context") || !strings.Contains(all, "kiwi") {
		t.Errorf("messages:\n%s", all)
	}
	chatSession.paired(t, mustElements(t, field(rw.Body, "messages")))
}

// A main request that is not in the fresh layout before any request of the
// turn carried the history is refused: sent on, the model would see a turn
// with no past.
func TestNotFreshBeforeTheHistoryIsRefused(t *testing.T) {
	_, r := twoTurns(t)
	noEnv := strings.ReplaceAll(t2First("which word?"), `# Environment\n`, `# Surroundings\n`)
	if rw := r.Rewrite("s", "t2", []byte(noEnv)); rw.Refuse == nil {
		t.Fatal("a first request in an unknown layout was not refused")
	}
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":"Summary: ..."},{"role":"system","content":"` + sys2 + `"}],` + top + `}`
	if rw := r.Rewrite("s", "t2", []byte(compacted)); rw.Refuse == nil {
		t.Errorf("a request not in the fresh layout went on before the history was carried: %+v", rw)
	}
}

// A full memo drops only closed turns: a running turn that carried the
// history keeps its fitted bytes and still has a compaction let through.
func TestFullMemoKeepsOpenTurnsHistory(t *testing.T) {
	src, r := twoTurns(t)
	first := r.Rewrite("s", "t2", []byte(t2First("which word?")))
	if first.Body == nil {
		t.Fatal("the main thread's first request was not spliced")
	}
	for i := 0; i < memoMax; i++ {
		id := "c" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		src.turns[id] = fakeTurn{prompt: "p", closed: true}
		r.mu.Lock()
		r.carried[[2]string{"s", id}] = true
		r.fits[[2]string{"s", id}] = &history{}
		r.mu.Unlock()
	}
	src.addTurn("t3", "and again?", "t1")
	if rw := r.Rewrite("s", "t3", []byte(t2First("and again?"))); rw.Body == nil {
		t.Fatalf("t3's first request: %+v", rw)
	}
	r.mu.Lock()
	_, fitted := r.fits[[2]string{"s", "t2"}]
	nf, nc := len(r.fits), len(r.carried)
	r.mu.Unlock()
	if !fitted || nf != 2 || nc != 2 {
		t.Errorf("t2 fitted %v; %d fits and %d carried left, want only the open turns", fitted, nf, nc)
	}
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":"Summary: ..."},{"role":"system","content":"` + sys2 + `"}],` + top + `}`
	passedCompacted(t, r.Rewrite("s", "t2", []byte(compacted)))
}

// Codex compacts inside a replayed turn, whose fresh thread id differs from
// the session's. The turn's requests, the compacted ones too, are recorded
// under the session's one thread id, so the next turn sees the compaction
// and the turn's final answer (review t_9f7ff3c2866b0f64: the compacted
// requests kept the fresh id, were read as another thread, and the final
// answer was lost).
func TestCodexCompactionInAReplayedTurn(t *testing.T) {
	src, r := codexTwoTurns(t)
	ctx := context("b", "read-only")
	call := `{"type":"function_call","id":"fc_2","call_id":"call_2","name":"shell","arguments":"{\"command\":\"ls\"}"}`
	first := r.Rewrite("s", "t2", []byte(codexReq(thread2, append(ctx, prompt("b", p2))...)))
	if first.Body == nil {
		t.Fatalf("not spliced: %+v", first.Events)
	}
	src.addCodexCall(t, "t2", "y0", string(first.Body), completedCall, items(call))
	compacted := codexReq(thread2, append(ctx, prompt("b", p2), `{"type":"compaction","encrypted_content":"Q09NUA=="}`)...)
	src.through(t, r, "t2", "y1", compacted, func(b string) { src.addCodexCall(t, "t2", "y1", b, completedCall, items(answer2)) })
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(0)})

	if p, err := r.Base("s"); err != nil || p.Turn != "t2" {
		t.Fatalf("base %+v %v", p, err)
	}
	src.addTurn("t3", p3, "t2")
	next := r.Rewrite("s", "t3", []byte(codexReq(thread3, append(context("c", "read-only"), prompt("c", p3))...)))
	if next.Body == nil {
		t.Fatalf("t3 not spliced: %+v", next.Events)
	}
	in := strings.Join(inputOf(t, next.Body), "\n")
	if !strings.Contains(in, "compacted its context") || !strings.Contains(in, compactedResult) ||
		!strings.Contains(in, `last words in the turn were:\nkiwi`) {
		t.Errorf("t3's input lost t2's compaction or final answer:\n%s", in)
	}
}
