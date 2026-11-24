package queue

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/staysup/agentbus/ids"
)

// finishUpdate takes the queued update job and finishes it the way the finisher
// does: computes the folds, records them on the job (MarkUpdated), and ends it
// with the runner's clean result (the folder baseline and the merge changesets).
func finishUpdate(t *testing.T, q *Queue, r Runner, ws ids.WorkspaceID, from, to, theirsCp, mergedCp string, unresolved ...string) Job {
	t.Helper()
	uj, ua := take(t, q, r)
	if uj.Kind != KindUpdate {
		t.Fatalf("took %s (%s), not the update", uj.ID, uj.Kind)
	}
	if _, _, err := q.Start(r.ID, ua.ID, ua.Number, lease); err != nil {
		t.Fatal(err)
	}
	head, folded, err := q.UpdateFolds(DefaultTeam, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.MarkUpdated(DefaultTeam, uj.ID, head, folded); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Result{Update: &UpdateResult{From: from, To: to, TheirsCheckpoint: theirsCp, MergedCheckpoint: mergedCp, Unresolved: unresolved}})
	done, err := q.End(r.ID, ua.ID, ua.Number, Finish{Status: Finished, Result: b})
	if err != nil {
		t.Fatal(err)
	}
	return done
}

// After a clean update, an apply writes only the session's work: its baseline is
// the folder as it is now (theirs) and its target is the merge, so the connector
// writes the delta between them -- the session's changes -- against the folder,
// which now matches theirs. The update's earlier turns are folded (no longer
// applied on their own); the head turn applies as the merge; a turn after the
// update extends it. One apply, discard or update runs per workspace.
func TestUpdateMovesApplyBaseline(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	theirs, merged := hexOf('a'), hexOf('b')

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	t2 := run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	ws := ids.WorkspaceID(first.Session)

	// Update from the folder. Nothing applied yet, so the merge base is the
	// copy's base.
	u, created, err := q.Update(DefaultTeam, ws, conA, nil, false)
	if err != nil || !created || u.Spec.Restore != "" {
		t.Fatalf("update: created=%v restore=%q err=%v", created, u.Spec.Restore, err)
	}
	// While it is in flight, a second update, a discard and an apply are refused.
	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); !errors.Is(err, ErrApplyBusy) {
		t.Fatalf("second update during an update: %v, want ErrApplyBusy", err)
	}
	if _, _, err := q.Discard(DefaultTeam, ws, conA, nil); !errors.Is(err, ErrApplyBusy) {
		t.Fatalf("discard during an update: %v, want ErrApplyBusy", err)
	}
	if _, _, err := q.Apply(DefaultTeam, t2.Turn); !errors.Is(err, ErrApplyBusy) {
		t.Fatalf("apply during an update: %v, want ErrApplyBusy", err)
	}
	finishUpdate(t, q, rn, ws, theirs, merged, "theirscp", "mergedcp")

	// Applying up to the head turn (t2) writes the merge against the folder now:
	// from = theirs (the folder's current state), to = merged.
	aj, created, err := q.Apply(DefaultTeam, t2.Turn)
	if err != nil || !created {
		t.Fatalf("apply head turn after update: created=%v err=%v", created, err)
	}
	if aj.Spec.ApplyFrom != theirs || aj.Spec.ApplyTo != merged {
		t.Fatalf("apply from=%q to=%q, want from=%q to=%q (only the session's work)", aj.Spec.ApplyFrom, aj.Spec.ApplyTo, theirs, merged)
	}

	// The folded earlier turn (t1) is no longer applied on its own.
	if _, _, err := q.Apply(DefaultTeam, t1.Turn); !errors.Is(err, ErrNotApplicable) {
		t.Fatalf("apply of a folded turn: %v, want ErrNotApplicable", err)
	}
}

// A turn after a clean update extends it: its apply still starts from the
// folder's current state (theirs), with the turn's own changeset as the target,
// so the delta is the session's work including the new turn.
func TestUpdateThenNewTurnExtends(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	theirs, merged := hexOf('a'), hexOf('b')

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); err != nil {
		t.Fatal(err)
	}
	finishUpdate(t, q, rn, ws, theirs, merged, "theirscp", "mergedcp")

	// A new turn after the update, checkpointing from the merge.
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	t3 := run(t, q, rn, Result{Changeset: hexOf('3'), Checkpoint: "cp3"})

	aj, created, err := q.Apply(DefaultTeam, t3.Turn)
	if err != nil || !created {
		t.Fatalf("apply after update+turn: created=%v err=%v", created, err)
	}
	if aj.Spec.ApplyFrom != theirs || aj.Spec.ApplyTo != hexOf('3') {
		t.Fatalf("apply from=%q to=%q, want from=%q to=%q", aj.Spec.ApplyFrom, aj.Spec.ApplyTo, theirs, hexOf('3'))
	}
}

// A conflicting update (no From/To in its result) changes no baseline: the
// workspace's turns stay appliable as they were, from the copy's base.
func TestUpdateConflictLeavesBaseline(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); err != nil {
		t.Fatal(err)
	}
	// Finish the update as a conflict: no folds, a conflict result.
	uj, ua := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, ua.ID, ua.Number, lease); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Result{Update: &UpdateResult{Conflicts: []string{"a.txt"}}})
	if _, err := q.End(rn.ID, ua.ID, ua.Number, Finish{Status: Finished, Result: b}); err != nil {
		t.Fatal(err)
	}
	_ = uj

	// The turn is still appliable from base: the conflict changed nothing.
	aj, created, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil || !created {
		t.Fatalf("apply after a conflicting update: created=%v err=%v", created, err)
	}
	if aj.Spec.ApplyFrom != "" || aj.Spec.ApplyTo != hexOf('1') {
		t.Fatalf("apply from=%q to=%q, want from='' to=%q", aj.Spec.ApplyFrom, aj.Spec.ApplyTo, hexOf('1'))
	}
}

// An update while a turn runs is not refused: it waits behind the turn in the
// lane (it is queued, not rejected). An apply, by contrast, leaves the lane.
func TestUpdateWaitsBehindRunningTurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	// A second turn is running (taken, not ended).
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	tj, ta := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, ta.ID, ta.Number, lease); err != nil {
		t.Fatal(err)
	}
	// The update queues behind it, not refused.
	u, created, err := q.Update(DefaultTeam, ws, conA, nil, false)
	if err != nil || !created {
		t.Fatalf("update while a turn runs: created=%v err=%v", created, err)
	}
	// It must not be takeable while the turn runs (lane order).
	if got := takes(t, q, rn); got == u.ID {
		t.Fatal("the update was taken while a turn was still running")
	}
	_ = tj
}

// Regression (Codex P1.1): a second update's merge base is the PRIOR update's
// folder snapshot, not the copy's base. Otherwise a second update would re-import
// the earlier folder state over a change the person made since the first update.
func TestSecondUpdateMergesFromPriorFolder(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	// Update #1: nothing applied, so the merge base is the copy's base ("").
	u1, _, err := q.Update(DefaultTeam, ws, conA, nil, false)
	if err != nil || u1.Spec.Restore != "" {
		t.Fatalf("update 1 base: restore=%q err=%v, want empty", u1.Spec.Restore, err)
	}
	finishUpdate(t, q, rn, ws, hexOf('a'), hexOf('b'), "u1.theirs", "u1.merged")

	// Update #2: the merge base is update #1's folder snapshot checkpoint, so the
	// merge is against the folder as #1 imported it, not the copy's base.
	u2, _, err := q.Update(DefaultTeam, ws, conA, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if u2.Spec.Restore != "u1.theirs" {
		t.Fatalf("update 2 base = %q, want u1.theirs (the prior update's folder snapshot)", u2.Spec.Restore)
	}
}

// Regression (Codex P1.2): a discard after an update's merge was applied resets
// the copy to the MERGE the folder holds, not the applied turn's pre-merge
// checkpoint. Otherwise a later turn + apply would revert the folder's imported
// content.
func TestDiscardAfterAppliedMergeResetsToMerge(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	conn := rn
	theirs, merged := hexOf('a'), hexOf('b')

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); err != nil {
		t.Fatal(err)
	}
	finishUpdate(t, q, rn, ws, theirs, merged, "u.theirs", "u.merged")

	// Apply up to the head turn writes the merge; the folder now holds "merged".
	aj, _, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if aj.Spec.ApplyTo != merged {
		t.Fatalf("apply to=%q, want the merge %q", aj.Spec.ApplyTo, merged)
	}
	runKind(t, q, conn, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}}, true)

	// A new turn, then a discard: it must reset to the merge checkpoint the folder
	// holds, not t1's pre-merge checkpoint cp1.
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	d, created, err := q.Discard(DefaultTeam, ws, conA, nil)
	if err != nil || !created {
		t.Fatalf("discard: created=%v err=%v", created, err)
	}
	if d.Spec.Restore != "u.merged" {
		t.Fatalf("discard restore = %q, want the applied merge checkpoint u.merged (not cp1)", d.Spec.Restore)
	}
}

// Regression (agy finding P1): after a clean update the next turn diffs from the
// merge, not the pre-update checkpoint. Otherwise the folder's own edits that the
// update merged in would show in the next turn's turn.changed as the agent's.
func TestUpdateMovesNextTurnDiffFrom(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); err != nil {
		t.Fatal(err)
	}
	finishUpdate(t, q, rn, ws, hexOf('a'), hexOf('b'), "u.theirs", "u.merged")

	// The next turn of the workspace must diff from the update's merge checkpoint.
	next := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	w, err := workspaceOf(q.db, next)
	if err != nil || w == nil {
		t.Fatalf("workspaceOf: %v", err)
	}
	if w.DiffFrom != "u.merged" {
		t.Fatalf("next turn DiffFrom = %q, want the update's merge checkpoint u.merged (not the pre-update cp1)", w.DiffFrom)
	}
}

// A merge is refused while a turn of the workspace runs or waits, so its
// conflict markers land only between turns; once the turn ends it queues with
// Merge set, and the baseline moves as on a clean update.
func TestMergeRefusedDuringTurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	theirs, merged := hexOf('a'), hexOf('b')

	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	ws := ids.WorkspaceID(first.Session)
	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, true); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("merge while a turn waits: %v, want ErrTurnBusy", err)
	}
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})

	u, _, err := q.Update(DefaultTeam, ws, conA, nil, true)
	if err != nil || !u.Spec.Merge {
		t.Fatalf("merge after the turn: merge=%v err=%v", u.Spec.Merge, err)
	}
	finishUpdate(t, q, rn, ws, theirs, merged, "theirscp", "mergedcp", "a.txt")

	aj, _, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if aj.Spec.ApplyFrom != theirs || aj.Spec.ApplyTo != merged {
		t.Fatalf("apply from=%q to=%q, want from=%q to=%q", aj.Spec.ApplyFrom, aj.Spec.ApplyTo, theirs, merged)
	}
}

// Apply and preview check for merge markers only while a merge left them:
// none before any merge, on after it (also after a later plain update), and
// off again once an apply finished (review finding, 2026-11-24).
func TestCheckMarkersOnlyAfterAMerge(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	ws := ids.WorkspaceID(first.Session)
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})

	if rec, err := q.appliedState(ws); err != nil || rec.Markers {
		t.Fatalf("before any merge: %v %v, want no check", rec.Markers, err)
	}
	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, true); err != nil {
		t.Fatal(err)
	}
	finishUpdate(t, q, rn, ws, hexOf('a'), hexOf('b'), "theirs1", "merged1", "a.txt")
	if _, _, err := q.Update(DefaultTeam, ws, conA, nil, false); err != nil {
		t.Fatal(err)
	}
	finishUpdate(t, q, rn, ws, hexOf('c'), hexOf('d'), "theirs2", "merged2")
	aj, _, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil || !aj.Spec.CheckMarkers {
		t.Fatalf("apply after a merge and a plain update: check=%v err=%v, want the check on", aj.Spec.CheckMarkers, err)
	}
	// The apply finishes: the merge is behind it, and the check is off.
	endApply(t, q, rn, Finished, "", Result{Apply: &ApplyResult{Files: []string{"a.txt"}}})
	if rec, err := q.appliedState(ws); err != nil || rec.Markers {
		t.Fatalf("after a finished apply: %v %v, want no check", rec.Markers, err)
	}
}
