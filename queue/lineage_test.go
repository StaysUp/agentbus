package queue

import (
	"errors"
	"testing"
	"time"

	"github.com/staysup/agentbus/ids"
)

// closeAndFinish queues a close of ws and runs it to finished, as a runner that
// removed the copy does.
func closeAndFinish(t *testing.T, q *Queue, rn Runner, ws ids.WorkspaceID) {
	t.Helper()
	if _, created, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil); err != nil || !created {
		t.Fatalf("close: created=%v err=%v", created, err)
	}
	finishClose(t, q, rn)
}

// appliable reports whether turn is among the workspace's appliable turns.
func appliable(t *testing.T, q *Queue, ws ids.WorkspaceID, turn string) bool {
	t.Helper()
	rec, err := q.appliedState(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range rec.Turns {
		if x.Turn == turn {
			return true
		}
	}
	return false
}

// The P1 that made this the rule (third finding in the area): after a close,
// a turn is queued, and Close is asked again before that turn starts. The second
// close must not answer "already closed": the queued turn will rebuild the copy.
// It is queued behind the turn in the lane, so it runs after it and removes what
// it built, and the workspace ends closed.
func TestSecondCloseQueuesBehindAQueuedTurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	closeAndFinish(t, q, rn, ws)

	// A turn is queued; it has not started.
	t2 := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	c2, created, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil)
	if err != nil || !created {
		t.Fatalf("second close: created=%v err=%v", created, err)
	}

	// The lane runs the turn first, then the close.
	j, a := take(t, q, rn)
	if j.ID != t2.ID {
		t.Fatalf("took %s (%s) first, want the queued turn %s", j.ID, j.Kind, t2.ID)
	}
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(rn.ID, a.ID, a.Number, Finish{Status: Finished, Result: mustResult(Result{Changeset: hexOf('2'), Checkpoint: "cp2"})}); err != nil {
		t.Fatal(err)
	}
	cj, ca := take(t, q, rn)
	if cj.ID != c2.ID {
		t.Fatalf("took %s (%s), want the second close %s", cj.ID, cj.Kind, c2.ID)
	}
	if _, _, err := q.Start(rn.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(rn.ID, ca.ID, ca.Number, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}

	// Closed again: the turn's copy was removed, so it is not appliable, and a
	// another close still runs safely.
	if appliable(t, q, ws, t2.Turn) {
		t.Fatal("the turn the second close removed is still appliable")
	}
	closeAndFinish(t, q, rn, ws)
}

// Every close runs, including repeated requests and a cancelled turn.
func TestRepeatedCloseStillRuns(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	closeAndFinish(t, q, rn, ws)
	closeAndFinish(t, q, rn, ws)
	t2 := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	if _, err := q.Cancel(DefaultTeam, t2.ID); err != nil {
		t.Fatal(err)
	}
	closeAndFinish(t, q, rn, ws)
}

// The lane runs a workspace's jobs in the order they were made, whatever the
// clock says: a close made after a queued turn runs after it even when both are
// made in one clock tick (ids are random, so the old id tie-break flipped the
// order about half the time) or after the clock stepped back. Without this, the
// second close of the P1 above could run before the queued turn, and the turn
// would rebuild the copy after it.
func TestLaneRunsInTheOrderJobsWereMade(t *testing.T) {
	for name, step := range map[string]time.Duration{"same tick": 0, "clock stepped back": -time.Hour} {
		t.Run(name, func(t *testing.T) {
			for i := range 20 { // fresh random ids each round
				q, clk := open(t)
				rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
				first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
				run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
				ws := ids.WorkspaceID(first.Session)

				turn := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
				clk.add(step)
				c, _, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil)
				if err != nil {
					t.Fatal(err)
				}
				ahead, err := q.TurnsAhead(DefaultTeam, c.ID)
				if err != nil || len(ahead) != 1 || ahead[0].ID != turn.ID {
					t.Fatalf("close's blockers = %+v (%v), want %s", ahead, err, turn.ID)
				}
				if j, _ := take(t, q, rn); j.ID != turn.ID {
					t.Fatalf("round %d: took %s (%s) first; want the turn made before the close (turn %s, close %s)",
						i, j.ID, j.Kind, turn.ID, c.ID)
				}
			}
		})
	}
}

// A prior update is also a baseline. It must reset for apply, preview and
// the next update, even though all old job rows have no lineage stamp.
func TestCloseResetsEveryBaselineAfterUpdate(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); err != nil {
		t.Fatal(err)
	}
	runKind(t, q, rn, Result{Update: &UpdateResult{From: hexOf('a'), To: hexOf('b'), TheirsCheckpoint: "old-theirs", MergedCheckpoint: "old-merge"}}, false)
	closeAndFinish(t, q, rn, ws)
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	reopened := run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	p, _, _, err := q.Preview(DefaultTeam, ws)
	if err != nil || p.Spec.ApplyFrom != "" || p.Spec.ApplyTo != hexOf('2') {
		t.Fatalf("preview after close: from %q to %q err %v", p.Spec.ApplyFrom, p.Spec.ApplyTo, err)
	}
	runKind(t, q, rn, Result{}, true)
	a, _, err := q.Apply(DefaultTeam, reopened.Turn)
	if err != nil || a.Spec.ApplyFrom != "" || a.Spec.ApplyTo != hexOf('2') {
		t.Fatalf("apply after close: from %q to %q err %v", a.Spec.ApplyFrom, a.Spec.ApplyTo, err)
	}
	if _, err := q.Cancel(DefaultTeam, a.ID); err != nil {
		t.Fatal(err)
	}
	u, _, err := q.Update(DefaultTeam, ws, conA, nil, false)
	if err != nil || u.Spec.Restore != "" {
		t.Fatalf("update after close: restore %q err %v", u.Spec.Restore, err)
	}
}

func TestCloseWaitsForRunningTurnAcrossClockStep(t *testing.T) {
	q, clk := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	j, a := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	clk.add(-time.Hour)
	ws := ids.WorkspaceID(first.Session)
	c, _, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ahead, err := q.TurnsAhead(DefaultTeam, c.ID); err != nil || len(ahead) != 1 || ahead[0].ID != j.ID {
		t.Fatalf("running blockers: %+v %v", ahead, err)
	}
	if _, err := q.End(rn.ID, a.ID, a.Number, Finish{Status: Finished, Session: j.Session,
		Workspace: &Workspace{Kind: WorkspaceCopy, Space: j.ID}, Result: mustResult(Result{Changeset: hexOf('1'), Checkpoint: "cp1"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.appliedState(ws); err != nil {
		t.Fatal(err)
	}
	finishClose(t, q, rn)
	if _, _, err := q.Apply(DefaultTeam, j.Turn); !errors.Is(err, ErrNotApplicable) {
		t.Fatalf("apply of removed copy: %v", err)
	}
}

func TestCloseExcludesOtherWorkspaceOperations(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	j := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	if _, _, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil); err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]func() error{
		"apply":   func() error { _, _, err := q.Apply(DefaultTeam, j.Turn); return err },
		"preview": func() error { _, _, _, err := q.Preview(DefaultTeam, ws); return err },
		"discard": func() error { _, _, err := q.Discard(DefaultTeam, ws, conA, nil); return err },
		"update":  func() error { _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); return err },
	} {
		if err := request(); !errors.Is(err, ErrApplyBusy) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCloseNeedsARecordBeforeQueuing(t *testing.T) {
	q, _ := open(t)
	q.ReadAppliedWith(nil)
	if _, created, err := q.CloseWorkspace(DefaultTeam, "ws_test", conA, nil); !errors.Is(err, ErrNoRecord) || created {
		t.Fatalf("without record: created %v err %v", created, err)
	}
}

func TestQueueDisplayListsBlockersInExecutionOrder(t *testing.T) {
	q, clk := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	one := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	clk.add(-time.Minute)
	two := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	clk.add(-time.Minute)
	c, _, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []Job{one, two} {
		ahead, err := q.TurnsAhead(DefaultTeam, c.ID)
		if err != nil || len(ahead) == 0 || ahead[0].ID != expected.ID {
			t.Fatalf("display: %+v %v, want %s first", ahead, err, expected.ID)
		}
		got := run(t, q, rn, Result{})
		if got.ID != expected.ID {
			t.Fatalf("execution %s, display %s", got.ID, expected.ID)
		}
	}
	if ahead, err := q.TurnsAhead(DefaultTeam, c.ID); err != nil || len(ahead) != 0 {
		t.Fatalf("remaining blockers: %+v %v", ahead, err)
	}
	finishClose(t, q, rn)
}

// A turn queued after the close runs on the rebuilt copy. Its recorded
// changes stay appliable.
func TestTurnAfterCloseStaysAppliable(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	// The close is made first, then a turn; the lane runs the close first.
	if _, _, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil); err != nil {
		t.Fatal(err)
	}
	t2 := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	finishClose(t, q, rn)
	run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})

	if !appliable(t, q, ws, t2.Turn) {
		t.Fatal("a turn that started after the close (on the rebuilt copy) is not appliable")
	}

}

// A close resets a prior apply baseline, and a later apply becomes the new
// update baseline.
func TestCloseResetsApplyAndUpdateBaselines(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	_, _, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil {
		t.Fatal(err)
	}
	runKind(t, q, rn, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}}, true)
	closeAndFinish(t, q, rn, ws)

	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	t2 := run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	a2, _, err := q.Apply(DefaultTeam, t2.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Spec.ApplyFrom != "" {
		t.Fatalf("apply after close starts at %q, want the rebuilt base", a2.Spec.ApplyFrom)
	}
	runKind(t, q, rn, Result{Apply: &ApplyResult{Files: []string{"b.txt"}}}, true)
	u, _, err := q.Update(DefaultTeam, ws, conA, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if u.Spec.Restore != "cp2" {
		t.Fatalf("update after apply restores %q, want cp2", u.Spec.Restore)
	}
}

// Queue-only tests publish outcomes through withRecord, as the finisher does.
func finishClose(t *testing.T, q *Queue, rn Runner) {
	t.Helper()
	j := runKind(t, q, rn, Result{}, false)
	if j.Kind != KindClose {
		t.Fatalf("finished %s, want close", j.Kind)
	}
	if _, err := q.appliedState(CopyKey(j.Spec.Workspace, j.Session)); err != nil {
		t.Fatal(err)
	}
}
