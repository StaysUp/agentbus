package jobs

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

const testLease = 30 * time.Second

func openQS(t *testing.T) (*queue.Queue, *store.Store) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close(); st.Close() })
	return q, st
}

// endTurn creates a turn job, runs it on r, and ends it with status/reason
// through q.End directly -- NOT control's finisher. So the job is ended in
// jobs.db with no turn.ended in the record, which is both the non-finisher end
// (a lost runner, a cancel) and the crash window the reconciler must close.
func endTurn(t *testing.T, q *queue.Queue, r queue.Runner, status, reason string) (turn, session string) {
	t.Helper()
	if _, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	jb, a, ok, _, err := q.Take(r.ID, testLease, queue.LaneInteractive)
	if err != nil || !ok {
		t.Fatalf("take: ok=%v err=%v", ok, err)
	}
	if _, _, err := q.Start(r.ID, a.ID, a.Number, testLease); err != nil {
		t.Fatal(err)
	}
	res := json.RawMessage(`{"session":"` + jb.Session + `","state":"` + queue.TurnState(status) + `"}`)
	if _, err := q.End(r.ID, a.ID, a.Number, queue.Finish{Status: status, Reason: reason, Result: res}); err != nil {
		t.Fatalf("end %s: %v", status, err)
	}
	return jb.Turn, jb.Session
}

func newRunner(t *testing.T, q *queue.Queue) queue.Runner {
	t.Helper()
	r, err := q.RegisterRunner(queue.DefaultTeam, "r", []string{"agent:claude"}, 8, queue.HashToken(queue.NewID("tok_")), nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// RecordTurnEnds writes turn.ended for every ended turn job whatever its
// outcome (finished, failed, stopped from a cancel), the state mapped as the
// turn's; and it does so for jobs ended without the finisher (the crash and
// lost-runner cases), which have no turn.ended until it runs. A second pass
// stores none (idempotent by job id).
func TestRecordTurnEndsEveryStateIdempotent(t *testing.T) {
	q, st := openQS(t)
	r := newRunner(t, q)
	fin, _ := endTurn(t, q, r, queue.Finished, "")
	fail, _ := endTurn(t, q, r, queue.Failed, queue.ReasonRunnerLost)
	canc, _ := endTurn(t, q, r, queue.Cancelled, "")

	// The finisher never ran, so the record holds no turn.ended yet.
	for _, tn := range []string{fin, fail, canc} {
		if ok, err := st.TurnEnded(tn); err != nil || ok {
			t.Fatalf("turn %s has turn.ended before reconcile: ok=%v err=%v", tn, ok, err)
		}
	}
	n, _, err := RecordTurnEnds(q, st, nil, time.Now().Add(time.Hour), 50)
	if err != nil || n != 3 {
		t.Fatalf("reconcile: n=%d err=%v; want 3", n, err)
	}
	for _, tn := range []string{fin, fail, canc} {
		if ok, err := st.TurnEnded(tn); err != nil || !ok {
			t.Fatalf("turn %s missing turn.ended after reconcile: ok=%v err=%v", tn, ok, err)
		}
	}
	if n, _, err := RecordTurnEnds(q, st, nil, time.Now().Add(time.Hour), 50); err != nil || n != 0 {
		t.Fatalf("second reconcile: n=%d err=%v; want 0 (idempotent)", n, err)
	}
	// Each turn.ended carries the turn-facing state: cancelled reads as stopped.
	want := map[string]string{fin: "finished", fail: "failed", canc: "stopped"}
	for tn, state := range want {
		recs, err := st.After(0, store.Filter{Turn: tn}, 10)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, rc := range recs {
			if rc.Kind == event.TurnEnded {
				var e event.Event
				if err := json.Unmarshal(rc.Data, &e); err != nil {
					t.Fatal(err)
				}
				got = e.State
			}
		}
		if got != state {
			t.Fatalf("turn %s turn.ended state = %q, want %q", tn, got, state)
		}
	}
}

// turn.ended is keyed on the turn and carries no session, so a turn refused
// before it ran (recorded only as turn.ended, with no run.started or
// turn.started) does NOT make its job's session "known" and does NOT appear as
// a phantom session in the snapshot (review P1, 2026-11-19).
func TestTurnEndedDoesNotCreateSession(t *testing.T) {
	q, st := openQS(t)
	r := newRunner(t, q)
	_, session := endTurn(t, q, r, queue.Failed, queue.ReasonSetup)
	if _, _, err := RecordTurnEnds(q, st, nil, time.Now().Add(time.Hour), 50); err != nil {
		t.Fatal(err)
	}
	if known, err := st.Known(session); err != nil || known {
		t.Fatalf("Known(%s) = %v, %v; want false (turn.ended must not create a session)", session, known, err)
	}
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snap.Sessions {
		if s.ID == session {
			t.Fatalf("snapshot has a phantom session %s conjured by turn.ended", session)
		}
	}
}

// A turn that ended failed (its runner lost) still delivers its reply: the
// reconciler records turn.ended, and FireOns -- reading the end from the
// record (store.TurnEnded), not from the jobs table -- queues the delivery.
// Nothing delivers until the end is in the record.
func TestFailedTurnStillDelivers(t *testing.T) {
	q, st := openQS(t)
	r := newRunner(t, q)
	turn, session := endTurn(t, q, r, queue.Failed, queue.ReasonRunnerLost)
	q.ReadReplyWith(func(s, tn string) (string, error) {
		if s == session && tn == turn {
			return "it ended: the runner was lost", nil
		}
		return "", nil
	})
	if _, err := q.RegisterOn(queue.DefaultTeam, queue.OnTurnCompleted, turn, "s_target", "", queue.Maker{}); err != nil {
		t.Fatal(err)
	}
	// Before the end is recorded, FireOns reads no end and delivers nothing.
	if n, err := q.FireOns(queue.DefaultTeam, st.TurnEnded, nil, nil, nil); err != nil || n != 0 {
		t.Fatalf("before reconcile: n=%d err=%v; want 0", n, err)
	}
	if _, _, err := RecordTurnEnds(q, st, nil, time.Now().Add(time.Hour), 50); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(queue.DefaultTeam, st.TurnEnded, nil, nil, nil); err != nil || n != 1 {
		t.Fatalf("after reconcile: n=%d err=%v; want 1 (the failed turn delivers)", n, err)
	}
	list, err := q.List(queue.DefaultTeam, queue.Filter{Session: "s_target"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, j := range list {
		if j.Links.ResultOf != nil {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("deliveries queued in s_target = %d, want 1", got)
	}
}
