package queue

import (
	"errors"
	"strings"
	"testing"
)

// A session whose runner was removed takes no more work: its queued turn
// ends at the removal with a reason naming the machine, and a new turn of
// it is refused with the same reason. Its apply, the folder's
// machine's job, is left as it is (0.5.1).
func TestRemovedRunnerEndsItsSessionsWork(t *testing.T) {
	q, c := open(t)
	r, err := q.RegisterRunner(DefaultTeam, "studio mac", nil, 4, HashToken(NewID("tok_")), &Capabilities{Connector: conA})
	if err != nil {
		t.Fatal(err)
	}
	other := capsRunner(t, q, nil, nil)
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
	j, a, ok, _, err := q.Take(r.ID, lease, false)
	if err != nil || !ok || j.ID != first.ID {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, a.ID, 1, Finish{Status: Finished, Session: j.Session, Result: []byte(`{"changeset":"` + hexOf('1') + `"}`),
		Workspace: &Workspace{Kind: WorkspaceCopy, Space: j.ID}}); err != nil {
		t.Fatal(err)
	}
	next := makeJob(t, q, Spec{Session: first.Session})
	ap, _, err := q.Apply(DefaultTeam, j.Turn)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	c.add(2 * RunnerFresh) // the runner went offline; the other one is seen again
	if _, _, ok, _, err := q.Take(other.ID, lease, false); err != nil || ok {
		t.Fatalf("the other runner took a job: %v %v", ok, err)
	}
	if err := q.RemoveRunner(DefaultTeam, r.ID); err != nil {
		t.Fatal(err)
	}
	why := RunnerRemovedWhy("studio mac")
	if !strings.Contains(why, `"studio mac"`) {
		t.Fatalf("the reason does not name the machine: %q", why)
	}
	got, err := q.Get(DefaultTeam, next.ID)
	if err != nil || got.State != Failed || got.Ended == nil || got.Reason != ReasonSetup {
		t.Fatalf("the queued turn: %s %s %v", got.State, got.Reason, err)
	}
	if res, err := ReadResult(got.Result); err != nil || res.Refused != why || res.Status != 409 {
		t.Fatalf("the queued turn's result %s, want the reason", got.Result)
	}
	if got, _ := q.Get(DefaultTeam, ap.ID); got.State != Queued {
		t.Fatalf("the apply is %s, want it left queued", got.State)
	}
	var removed *RunnerRemovedError
	if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: first.Session, Prompt: "again"}, Links{}); !errors.As(err, &removed) || err.Error() != why {
		t.Fatalf("a new turn: %v, want the reason", err)
	}
}

// Work that becomes queued after the removal ends too, at the next sweep:
// a next turn the runner had taken, whose lease runs out, and a next turn
// of a session whose first turn's saved finish ends after the removal
// (review of 2026-11-12, codex P1).
func TestRemovedRunnerEndsWorkQueuedLater(t *testing.T) {
	why := RunnerRemovedWhy("studio mac")
	ended := func(t *testing.T, q *Queue, id string) {
		t.Helper()
		got, err := q.Get(DefaultTeam, id)
		if err != nil || got.State != Failed {
			t.Fatalf("job %s is %s, %v; want it ended", id, got.State, err)
		}
		if res, _ := ReadResult(got.Result); res.Refused != why {
			t.Fatalf("job %s result %s", id, got.Result)
		}
	}
	setup := func(t *testing.T) (*Queue, *clock, Runner, Job, Attempt) {
		q, c := open(t)
		r, err := q.RegisterRunner(DefaultTeam, "studio mac", nil, 4, HashToken(NewID("tok_")), &Capabilities{Connector: conA})
		if err != nil {
			t.Fatal(err)
		}
		first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
		j, a, ok, _, err := q.Take(r.ID, lease, false)
		if err != nil || !ok || j.ID != first.ID {
			t.Fatalf("take: %v %v", ok, err)
		}
		if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
			t.Fatal(err)
		}
		return q, c, r, j, a
	}
	fin := func(j Job) Finish {
		return Finish{Status: Finished, Session: j.Session, Result: []byte(`{"changeset":"` + hexOf('1') + `"}`),
			Workspace: &Workspace{Kind: WorkspaceCopy, Space: j.ID}}
	}
	t.Run("taken, then its lease runs out", func(t *testing.T) {
		q, c, r, j, a := setup(t)
		if _, err := q.End(r.ID, a.ID, 1, fin(j)); err != nil {
			t.Fatal(err)
		}
		next := makeJob(t, q, Spec{Session: j.Session})
		if got, _, ok, _, err := q.Take(r.ID, lease, false); err != nil || !ok || got.ID != next.ID {
			t.Fatalf("take next: %v %v", ok, err)
		}
		c.add(2 * RunnerFresh)
		if err := q.RemoveRunner(DefaultTeam, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Sweep(); err != nil {
			t.Fatal(err)
		}
		ended(t, q, next.ID)
	})
	t.Run("the first turn's saved finish ends after the removal", func(t *testing.T) {
		q, c, r, j, a := setup(t)
		if _, _, err := q.Accept(r.ID, a.ID, 1, fin(j)); err != nil {
			t.Fatal(err)
		}
		next := makeJob(t, q, Spec{Session: j.Session})
		c.add(2 * RunnerFresh)
		if err := q.RemoveRunner(DefaultTeam, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.End(r.ID, a.ID, 1, fin(j)); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Sweep(); err != nil {
			t.Fatal(err)
		}
		ended(t, q, next.ID)
	})
}
