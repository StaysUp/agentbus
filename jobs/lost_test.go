package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// An apply whose runner was lost (its lease ran out while it ran, with no
// finish) gets its turn.applied, once: failed, naming the lost runner.
// The job then names the event, and a write that fails is tried again on
// the next call (0.5.1).
func TestLostApplyGetsTurnApplied(t *testing.T) {
	e, rec, clock := newFailingEnv(t)
	spec := queue.Spec{Session: "s_1", Connector: testConnector, FolderRef: "fld_0123456789abcdef", ApplyTo: "blob2"}
	links := queue.Links{AppliesTurn: "t_1", AppliesTurns: []string{"t_1"}}
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindApply, "a1", spec, links); err != nil {
		t.Fatal(err)
	}
	e.tok = enrollConnector(t, e.srv)
	j, _ := e.take(t, queue.KindApply)
	if n, err := CloseLostApplies(e.q, rec); err != nil || n != 0 {
		t.Fatalf("a running apply: %d %v", n, err)
	}
	clock.add(time.Hour) // the runner stopped answering
	if _, err := e.q.Sweep(); err != nil {
		t.Fatal(err)
	}
	j, _ = e.q.Get(queue.DefaultTeam, j.ID)
	if j.State != queue.Failed || j.Reason != queue.ReasonRunnerLost {
		t.Fatalf("the lost apply: %s %s", j.State, j.Reason)
	}
	// Before the event is written, the apply reads from its state.
	if a, err := Applied(e.st, j); err != nil || a == nil || a.Result != "failed" {
		t.Fatalf("read before the event: %+v %v", a, err)
	}
	rec.set(1, 0)
	if n, err := CloseLostApplies(e.q, rec); err == nil || n != 0 {
		t.Fatalf("with the record failing: %d %v", n, err)
	}
	if k := e.kinds(t, "s_1"); k[event.TurnApplied] != 0 {
		t.Fatalf("the record holds %v", k)
	}
	if n, err := CloseLostApplies(e.q, rec); err != nil || n != 1 {
		t.Fatalf("closed %d, %v; want the lost apply", n, err)
	}
	if n, err := CloseLostApplies(e.q, rec); err != nil || n != 0 {
		t.Fatalf("closed %d again, %v", n, err)
	}
	if k := e.kinds(t, "s_1"); k[event.TurnApplied] != 1 {
		t.Fatalf("the record holds %v, want one turn.applied", k)
	}
	j, _ = e.q.Get(queue.DefaultTeam, j.ID)
	if res, _ := queue.ReadResult(j.Result); res.AppliedEvent != appliedID(j) || res.OutcomeDue {
		t.Fatalf("the job keeps %s", j.Result)
	}
	a, err := Applied(e.st, j)
	if err != nil || a == nil || a.Result != "failed" || a.Error == "" || len(a.Turns) != 1 || a.Turns[0] != "t_1" {
		t.Fatalf("turn.applied %+v %v", a, err)
	}
	if want := "its runner stopped answering (runner_lost)"; !strings.Contains(a.Error, want) {
		t.Errorf("error %q, want it to say %q", a.Error, want)
	}
}

// Due outcomes are read a page at a time, so a write that keeps failing
// holds up none after it.
func TestLostAppliesReadPastFailures(t *testing.T) {
	old := lostPage
	lostPage = 1
	t.Cleanup(func() { lostPage = old })
	e, rec, clock := newFailingEnv(t)
	e.tok = enrollConnector(t, e.srv)
	var ids []string
	for _, key := range []string{"a1", "a2", "a3"} {
		spec := queue.Spec{Session: "s_1", Connector: testConnector, FolderRef: "fld_0123456789abcdef", ApplyTo: "blob2"}
		if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindApply, key, spec, queue.Links{AppliesTurn: "t_1"}); err != nil {
			t.Fatal(err)
		}
		j, _ := e.take(t, queue.KindApply)
		clock.add(time.Hour)
		if _, err := e.q.Sweep(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	rec.set(2, 0) // the first two writes fail
	if n, err := CloseLostApplies(e.q, rec); err == nil || n != 1 {
		t.Fatalf("closed %d, %v; want the third", n, err)
	}
	last, _ := e.q.Get(queue.DefaultTeam, ids[2])
	if res, _ := queue.ReadResult(last.Result); res.AppliedEvent == "" {
		t.Fatalf("the third apply waits behind the failed ones: %s", last.Result)
	}
	if n, err := CloseLostApplies(e.q, rec); err != nil || n != 2 {
		t.Fatalf("closed %d, %v; want the other two", n, err)
	}
}
