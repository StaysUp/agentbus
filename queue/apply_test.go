package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/applied"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/workspace"
)

// An apply's key holds a "/", as no caller's key may, so no other job can
// take it first.
func TestApplyKeyIsReserved(t *testing.T) {
	if k := ApplyKey("t_1", 0); !strings.Contains(k, "/") || !strings.Contains(ResultKey("on_1", "job_1", "s_1"), "/") {
		t.Fatalf("apply key %q", k)
	}
}

// A result with many files fits its budget, keeps the whole count, and says
// it was cut.
func TestApplyResultFit(t *testing.T) {
	a := ApplyResult{}
	for i := range 9000 {
		a.Files = append(a.Files, fmt.Sprintf("folder/with/a/long/name/file-%05d.txt", i))
	}
	a.Count = len(a.Files)
	a.Fit(64 << 10)
	b, _ := json.Marshal(a)
	if len(b) > 64<<10 || !a.Truncated || a.Count != 9000 || len(a.Files) == 0 {
		t.Fatalf("%d bytes, truncated %v, count %d, files %d", len(b), a.Truncated, a.Count, len(a.Files))
	}
	small := ApplyResult{Files: []string{"a.txt"}, Count: 1}
	small.Fit(64 << 10)
	if small.Truncated || len(small.Files) != 1 {
		t.Fatalf("a small result was cut: %+v", small)
	}
}

// A preview cut to fit drops each diff with its line counts, then the files
// no conflict names, then conflicts with their files: every conflict kept
// has its file.
func TestPreviewResultFit(t *testing.T) {
	diff := strings.Repeat("+line\n", 200)
	p := PreviewResult{}
	for i := range 2000 {
		path := fmt.Sprintf("folder/with/a/long/name/file-%05d.txt", i)
		p.Files = append(p.Files, PreviewFile{Path: path, Change: "modified", Added: 200, Removed: 1, Diff: &diff})
		if i%3 == 0 {
			p.Conflicts = append(p.Conflicts, ApplyConflict{Path: path, Why: "changed"})
		}
	}
	p.Count = len(p.Files)
	for _, max := range []int{256 << 10, 64 << 10, 8 << 10} {
		c := p
		c.Files = append([]PreviewFile(nil), p.Files...)
		c.Conflicts = append([]ApplyConflict(nil), p.Conflicts...)
		c.Fit(max)
		b, _ := json.Marshal(c)
		if len(b) > max || !c.Truncated || !c.DiffsCut || c.Count != 2000 || len(c.Conflicts) == 0 {
			t.Fatalf("max %d: %d bytes, truncated %v, cut %v, count %d, conflicts %d", max, len(b), c.Truncated, c.DiffsCut, c.Count, len(c.Conflicts))
		}
		files := map[string]bool{}
		for _, f := range c.Files {
			files[f.Path] = true
			if f.Diff != nil || f.Added != 0 || f.Removed != 0 || !f.DiffLeftOut {
				t.Fatalf("max %d: a cut diff kept its counts: %+v", max, f)
			}
		}
		for _, x := range c.Conflicts {
			if !files[x.Path] {
				t.Fatalf("max %d: conflict %s has no file", max, x.Path)
			}
		}
	}
}

// run takes the next job on r and finishes it with res: the session's
// first turn also records its copy.
func run(t *testing.T, q *Queue, r Runner, res Result) Job {
	t.Helper()
	return runKind(t, q, r, res, false)
}

// runKind is run of a turn (folders false) or of a folder job.
func runKind(t *testing.T, q *Queue, r Runner, res Result, folders bool) Job {
	t.Helper()
	take := take
	if folders {
		take = takeFolder
	}
	j, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	f := Finish{Status: Finished, Result: b}
	if j.Spec.Session == "" && j.Kind == KindTurn {
		f.Session, f.Workspace = j.Session, &Workspace{Kind: WorkspaceCopy, Space: j.ID}
	}
	done, err := q.End(r.ID, a.ID, a.Number, f)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func hexOf(c byte) string { return strings.Repeat(string(c), 64) }

// A discard resets the copy to the LAST APPLIED checkpoint, not base, and drops
// the turns after it. One apply-or-discard runs per workspace: while a discard
// is in flight, a second discard and an apply are both refused with ErrApplyBusy
// (a plain 409 at the API), before they run.
func TestDiscardTargetsAppliedPointAndExcludesOthers(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	conn := rn
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	turns := []Job{run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})}
	for _, c := range []byte{'2', '3'} {
		makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
		turns = append(turns, run(t, q, rn, Result{Changeset: hexOf(c), Checkpoint: "cp" + string(c)}))
	}
	ws := ids.WorkspaceID(first.Session)

	// Apply turn 1, so the applied point is its checkpoint cp1.
	if _, created, err := q.Apply(DefaultTeam, turns[0].Turn); err != nil || !created {
		t.Fatalf("apply turn 1: created %v err %v", created, err)
	}
	runKind(t, q, conn, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}}, true)

	// A discard resets to cp1 (not base) and drops turns 2 and 3.
	d, created, err := q.Discard(DefaultTeam, ws, conA, nil)
	if err != nil || !created || d.Spec.Restore != "cp1" {
		t.Fatalf("discard after apply: restore=%q created=%v err=%v", d.Spec.Restore, created, err)
	}
	// The dropped turns are worked out when the discard RUNS, not frozen on the
	// job; they are turns 2 and 3 (after the applied turn 1).
	if dropped, err := q.DroppedTurns(DefaultTeam, ws); err != nil ||
		fmt.Sprint(dropped) != fmt.Sprint([]string{turns[1].Turn, turns[2].Turn}) {
		t.Fatalf("dropped: %v err %v", dropped, err)
	}

	// While that discard is in flight (queued), a second discard is refused.
	if _, _, err := q.Discard(DefaultTeam, ws, conA, nil); !errors.Is(err, ErrApplyBusy) {
		t.Fatalf("second discard during a discard: %v, want ErrApplyBusy", err)
	}
	// And an apply is refused while a discard is in flight.
	if _, _, err := q.Apply(DefaultTeam, turns[2].Turn); !errors.Is(err, ErrApplyBusy) {
		t.Fatalf("apply during a discard: %v, want ErrApplyBusy", err)
	}
}

// After a discard finishes, the turns it dropped are no longer appliable,
// previewable or droppable: applying one is refused, and a second discard has
// nothing to drop.
func TestDiscardMakesDroppedTurnsUnappliable(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	ws := ids.WorkspaceID(first.Session)

	// Discard to base, then finish it as the finisher does: record its dropped
	// turns on the job (MarkDiscarded) and end it.
	d, _, err := q.Discard(DefaultTeam, ws, conA, nil)
	if err != nil {
		t.Fatal(err)
	}
	dj, da := take(t, q, rn)
	if dj.ID != d.ID {
		t.Fatalf("took %s, not the discard %s", dj.ID, d.ID)
	}
	if _, _, err := q.Start(rn.ID, da.ID, da.Number, lease); err != nil {
		t.Fatal(err)
	}
	dropped, err := q.DroppedTurns(DefaultTeam, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.MarkDiscarded(DefaultTeam, dj.ID, dropped); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(rn.ID, da.ID, da.Number, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}

	// The dropped turn's changes are gone from the copy, so applying it is refused
	// (it writes nothing of the dropped work to the folder).
	if _, _, err := q.Apply(DefaultTeam, t1.Turn); !errors.Is(err, ErrNotApplicable) {
		t.Fatalf("apply of a dropped turn: %v, want ErrNotApplicable", err)
	}
	// And a second discard finds nothing unapplied.
	if _, _, err := q.Discard(DefaultTeam, ws, conA, nil); !errors.Is(err, ErrAllApplied) {
		t.Fatalf("second discard: %v, want ErrAllApplied", err)
	}
}

// A session-less discard is routed by its workspace COPY key to the runner that
// holds the copy: another runner on the same machine, which does not hold it,
// cannot take it (Take keys affinity by the copy key, not `session IS NULL`).
func TestDiscardRoutesToTheCopyRunner(t *testing.T) {
	q, _ := open(t)
	a := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	b := capsRunner(t, q, nil, &Capabilities{Connector: conA}) // same machine, no copy
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, a, Result{Changeset: hexOf('1'), Checkpoint: "cp1"}) // the copy lands on runner a
	ws := ids.WorkspaceID(first.Session)
	if _, created, err := q.Discard(DefaultTeam, ws, conA, nil); err != nil || !created {
		t.Fatalf("discard: created %v err %v", created, err)
	}
	if got := takes(t, q, b); got != "" {
		t.Fatalf("runner b, which holds no copy, took the discard %s", got)
	}
	if got := takes(t, q, a); got == "" {
		t.Fatal("runner a, which holds the copy, did not take the discard")
	}
}

// A discard while only a turn is running -- with no earlier UNAPPLIED ended turn
// -- is not "already" (nothing to do): it queues, waits for the turn, and drops
// it. A running turn's changes count as unapplied even before it ends.
func TestDiscardWhileOnlyTurnRunsIsNotAlready(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	conn := rn
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	// Apply turn 1, so there is NO unapplied ended turn.
	if _, _, err := q.Apply(DefaultTeam, t1.Turn); err != nil {
		t.Fatal(err)
	}
	runKind(t, q, conn, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}}, true)
	// Turn 2 is running; no ended unapplied turn exists.
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	_, a2 := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, a2.ID, a2.Number, lease); err != nil {
		t.Fatal(err)
	}
	// The discard is not refused as "already": it is created, to wait for turn 2.
	if _, created, err := q.Discard(DefaultTeam, ws, conA, nil); err != nil || !created {
		t.Fatalf("discard while only a turn runs: created=%v err=%v, want created with no error", created, err)
	}
}

// Discard reports the copy runner's real state with the right error: a removed
// runner is RunnerRemovedError (not "offline"), a closed workspace is
// ErrSessionClosed -- one query over copies+runners, so they are not conflated.
func TestDiscardReportsRemovedRunner(t *testing.T) {
	q, c := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	c.add(2 * RunnerFresh) // the runner goes offline, so it can be removed
	if err := q.RemoveRunner(DefaultTeam, rn.ID); err != nil {
		t.Fatal(err)
	}
	var removed *RunnerRemovedError
	if _, _, err := q.Discard(DefaultTeam, ids.WorkspaceID(first.Session), conA, nil); !errors.As(err, &removed) {
		t.Fatalf("discard on a removed runner: %v, want RunnerRemovedError", err)
	}
}

func TestDiscardReportsClosedWorkspace(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	if _, err := q.CloseSession(DefaultTeam, ids.WorkspaceID(first.Session)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Discard(DefaultTeam, ids.WorkspaceID(first.Session), conA, nil); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("discard on a closed workspace: %v, want ErrSessionClosed", err)
	}
}

// A discard job names no session: its workspace.discarded event then names only
// the workspace, so a workspace id is never written where a session id belongs
// (no phantom session). Its copy affinity still routes it (CopyKey).
func TestDiscardJobHasNoSession(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)
	d, created, err := q.Discard(DefaultTeam, ws, conA, nil)
	if err != nil || !created {
		t.Fatalf("discard: created %v err %v", created, err)
	}
	if d.Session != "" {
		t.Fatalf("discard job session = %q, want empty", d.Session)
	}
	if d.Spec.Workspace != ws {
		t.Fatalf("discard workspace = %q, want %q", d.Spec.Workspace, ws)
	}
}

// A discard queued while a turn runs is not refused: it waits in the lane, and
// when it runs it drops that turn too -- the dropped turns are read at the
// finish, not frozen when the discard was queued, so nothing is silently wiped.
func TestDiscardQueuedWhileTurnRunsDropsBoth(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws := ids.WorkspaceID(first.Session)

	// Turn 2 is running (taken, started, not ended) when the discard is queued.
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	j2, a2 := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, a2.ID, a2.Number, lease); err != nil {
		t.Fatal(err)
	}
	// A running turn does not refuse the discard; it is queued behind it.
	if _, created, err := q.Discard(DefaultTeam, ws, conA, nil); err != nil || !created {
		t.Fatalf("discard while a turn runs: created %v err %v", created, err)
	}
	// Turn 2 finishes, unapplied.
	b, _ := json.Marshal(Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	if _, err := q.End(rn.ID, a2.ID, a2.Number, Finish{Status: Finished, Result: b}); err != nil {
		t.Fatal(err)
	}
	// When the discard runs, it drops BOTH turns, turn 2 included.
	if dropped, err := q.DroppedTurns(DefaultTeam, ws); err != nil ||
		fmt.Sprint(dropped) != fmt.Sprint([]string{t1.Turn, j2.Turn}) {
		t.Fatalf("dropped %v err %v, want both turns", dropped, err)
	}
}

// With nothing applied, a discard resets the copy to base and drops every turn.
func TestDiscardToBaseDropsEveryTurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	turns := []Job{run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})}
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	turns = append(turns, run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"}))
	ws := ids.WorkspaceID(first.Session)
	d, created, err := q.Discard(DefaultTeam, ws, conA, nil)
	if err != nil || !created || d.Spec.Restore != "" {
		t.Fatalf("discard to base: restore=%q created=%v err=%v", d.Spec.Restore, created, err)
	}
	if dropped, err := q.DroppedTurns(DefaultTeam, ws); err != nil ||
		fmt.Sprint(dropped) != fmt.Sprint([]string{turns[0].Turn, turns[1].Turn}) {
		t.Fatalf("dropped: %v err %v", dropped, err)
	}
}

// An apply up to a turn starts from the last turn applied, and covers every
// turn before it: asking for an earlier one answers the apply that did it.
func TestApplyUpToATurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	conn := rn
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1')})
	if t1.ID != first.ID {
		t.Fatalf("ran %s", t1.ID)
	}
	var turns []Job
	turns = append(turns, t1)
	for _, c := range []byte{'2', '3'} {
		makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
		turns = append(turns, run(t, q, rn, Result{Changeset: hexOf(c)}))
	}
	a2, created, err := q.Apply(DefaultTeam, turns[1].Turn)
	if err != nil || !created || a2.Spec.ApplyFrom != "" || a2.Spec.ApplyTo != hexOf('2') || a2.Spec.Machine != conA || a2.Spec.FolderRef != refX {
		t.Fatalf("apply up to turn 2: %+v %v %v", a2.Spec, created, err)
	}
	// Only a folder place takes it; a turn place never does.
	if got := takes(t, q, rn); got != "" {
		t.Fatalf("a turn place took %s", got)
	}
	// While it runs, an apply of turn 1 is that apply; of turn 3, busy.
	if j, created, err := q.Apply(DefaultTeam, turns[0].Turn); err != nil || created || j.ID != a2.ID {
		t.Fatalf("turn 1 while turn 2's apply runs: %s %v %v", j.ID, created, err)
	}
	if _, _, err := q.Apply(DefaultTeam, turns[2].Turn); err != ErrApplyBusy {
		t.Fatalf("turn 3 while turn 2's apply runs: %v", err)
	}
	runKind(t, q, conn, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}}, true)
	if j, ok, err := q.AppliedBy(DefaultTeam, turns[0].Turn); err != nil || !ok || j.ID != a2.ID {
		t.Fatalf("turn 1 applied by %s %v %v", j.ID, ok, err)
	}
	if j, created, err := q.Apply(DefaultTeam, turns[0].Turn); err != nil || created || j.ID != a2.ID {
		t.Fatalf("turn 1 after turn 2's apply: %s %v %v", j.ID, created, err)
	}
	a3, created, err := q.Apply(DefaultTeam, turns[2].Turn)
	if err != nil || !created || a3.Spec.ApplyFrom != hexOf('2') || a3.Spec.ApplyTo != hexOf('3') {
		t.Fatalf("apply up to turn 3: %+v %v %v", a3.Spec, created, err)
	}
	if _, ok, _ := q.AppliedBy(DefaultTeam, turns[2].Turn); !ok {
		t.Fatal("turn 3 has no apply")
	}
}

// A session runs on the machine that holds its folder: its first turn, an
// isolated copy included, goes only to that machine's runner, never to
// another's or to a runner of no machine. Folder jobs (a snapshot, an apply)
// go only to that runner's folder places, and turns only to its turn places.
// A connector of a build before 0.5 takes nothing, and none can report
// itself one now.
func TestTakeRoutesByMachine(t *testing.T) {
	q, _ := open(t)
	plain := capsRunner(t, q, nil, nil)
	onB := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	legacy := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	onA := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	turn := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	for _, r := range []Runner{plain, onB, legacy} {
		if got := takes(t, q, r); got != "" {
			t.Fatalf("a runner off the folder's machine took the turn %s", got)
		}
	}
	if got := takesFolder(t, q, onA); got != "" {
		t.Fatalf("a folder place took the turn %s", got)
	}
	if got := takes(t, q, onA); got != turn.ID {
		t.Fatalf("the folder's machine took %q, want %s", got, turn.ID)
	}
	// A next turn of the session stays on the session's runner.
	makeJob(t, q, Spec{Session: ids.SessionID(turn.Session)})
	if got := takes(t, q, onB); got != "" {
		t.Fatalf("another machine took the next turn %s", got)
	}
	if (Capabilities{Role: "x"}).Check() == "" || (Capabilities{Role: RoleConnector, Connector: conA}).Check() == "" ||
		(Capabilities{Connector: conA}).Check() != "" {
		t.Fatal("a role is not refused")
	}
}

// A session made before 0.5 whose first turn another machine's runner built
// from a snapshot keeps going there: its next turn goes to the session's
// runner, though its folder is elsewhere. Its apply goes to the folder's
// machine only.
func TestSessionFromBeforeKeepsItsRunner(t *testing.T) {
	q, _ := open(t)
	far := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	home := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	// As 0.4 took it: bound to the far runner.
	j, a, ok, _, err := q.Take(home.ID, lease, false)
	if err != nil || !ok || j.ID != first.ID {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, err := q.db.Exec(`UPDATE attempts SET runner = ? WHERE id = ?`, far.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Start(far.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(far.ID, a.ID, 1, Finish{Status: Finished, Session: j.Session, Result: []byte(`{"changeset":"` + hexOf('1') + `"}`),
		Workspace: &Workspace{Kind: WorkspaceCopy, Space: j.ID}}); err != nil {
		t.Fatal(err)
	}
	next := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	if got := takes(t, q, home); got != "" {
		t.Fatalf("the folder's machine took the next turn of a session another runner holds: %s", got)
	}
	if got := takes(t, q, far); got != next.ID {
		t.Fatalf("the session's runner took %q, want %s", got, next.ID)
	}
	ap, created, err := q.Apply(DefaultTeam, j.Turn)
	if err != nil || !created {
		t.Fatalf("apply: %v %v", created, err)
	}
	if got := takesFolder(t, q, far); got != "" {
		t.Fatalf("the session's far runner took the apply %s", got)
	}
	if got := takesFolder(t, q, home); got != ap.ID {
		t.Fatalf("the folder's machine took %q, want the apply %s", got, ap.ID)
	}
}

// W3: apply is NOT in the workspace lane -- it reads finished checkpoints and
// the folder, never the live copy -- so an apply of an ended turn is taken at
// once while a later turn of the same workspace is still running (his call,
// 2026-11-22). Only turns and discards (which change the copy) share the lane.
func TestApplyDoesNotWaitForRunningTurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1')})
	if t1.ID != first.ID {
		t.Fatalf("ran %s", t1.ID)
	}
	// A second turn of the same workspace is running (taken and started).
	next := makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	nj, na := take(t, q, rn)
	if nj.ID != next.ID {
		t.Fatalf("took %s, want the next turn %s", nj.ID, next.ID)
	}
	if _, _, err := q.Start(rn.ID, na.ID, na.Number, lease); err != nil {
		t.Fatal(err)
	}
	ap, created, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil || !created {
		t.Fatalf("apply: %v %v", created, err)
	}
	if got := takesFolder(t, q, rn); got != ap.ID {
		t.Fatalf("the apply waited behind a running turn: took %q, want the apply %s", got, ap.ID)
	}
}

// Folder jobs have places of their own: an apply is taken while every turn
// place is busy, and at most FolderCapacity folder jobs run at once.
func TestFolderPlacesBesideTurns(t *testing.T) {
	q, _ := open(t)
	r, err := q.RegisterRunner(DefaultTeam, "r", nil, 1, HashToken(NewID("tok_")), &Capabilities{Connector: conA})
	if err != nil {
		t.Fatal(err)
	}
	makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA})
	makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA})
	if takes(t, q, r) == "" || takes(t, q, r) != "" {
		t.Fatal("a runner of capacity 1 did not take exactly one turn")
	}
	for i := range FolderCapacity + 1 {
		if _, err := q.RegisterFolder(DefaultTeam, conA, "/p/"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range FolderCapacity {
		if takesFolder(t, q, r) == "" {
			t.Fatalf("folder job %d was not taken while the turn runs", i+1)
		}
	}
	if got := takesFolder(t, q, r); got != "" {
		t.Fatalf("a folder job past FolderCapacity was taken: %s", got)
	}
}

func mustAttempt(t *testing.T, q *Queue, job string) string {
	t.Helper()
	j, err := q.Get(DefaultTeam, job)
	if err != nil || j.Attempt == "" {
		t.Fatalf("job %s has no attempt: %v", job, err)
	}
	return j.Attempt
}

// Two applies of one session asked at once: one is made, and the other is
// that apply or refused as busy; never two from the same starting point.
func TestApplyAtOnce(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	turns := []Job{run(t, q, rn, Result{Changeset: hexOf('1')})}
	for _, c := range []byte{'2', '3'} {
		makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
		turns = append(turns, run(t, q, rn, Result{Changeset: hexOf(c)}))
	}
	type answer struct {
		j       Job
		created bool
		err     error
	}
	out := make(chan answer, 2)
	for _, tn := range []Job{turns[1], turns[2]} {
		go func() { j, c, err := q.Apply(DefaultTeam, tn.Turn); out <- answer{j, c, err} }()
	}
	made := 0
	for range 2 {
		a := <-out
		switch {
		case a.err == nil && a.created:
			made++
		case a.err == nil, a.err == ErrApplyBusy:
		default:
			t.Fatal(a.err)
		}
	}
	applies, _ := q.sessionApplies(DefaultTeam, ids.WorkspaceID(first.Session))
	if made != 1 || len(applies) != 1 {
		t.Fatalf("%d made, %d applies", made, len(applies))
	}
}

// An isolated first turn stays on its folder's machine whatever harness
// another runner has: the folder's runner takes it, and its engine says how
// to install the program.
func TestTakeIsolatedStaysOnItsMachine(t *testing.T) {
	q, _ := open(t)
	found := func(name string) []harness.Report { return []harness.Report{{Name: name, Found: true}} }
	here := capsRunner(t, q, nil, &Capabilities{Connector: conA, Harnesses: found("claude")})
	away := capsRunner(t, q, nil, &Capabilities{Connector: conB, Harnesses: found("codex")})
	j := makeJob(t, q, Spec{Harness: "codex", FolderRef: refX, Machine: conA, Isolate: true})
	if got := takes(t, q, away); got != "" {
		t.Fatalf("the runner of another machine took %s", got)
	}
	if got := takes(t, q, here); got != j.ID {
		t.Fatalf("the folder's runner took %q", got)
	}
}

// An apply of a folder whose machine's runner has not been seen for
// RunnerFresh is refused: nothing would take it. It makes no job. A
// connector of a build before 0.5 on that machine does not count.
func TestApplyNeedsItsMachine(t *testing.T) {
	q, c := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	turn := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	j, a := take(t, q, rn)
	if j.ID != turn.ID {
		t.Fatalf("took %s", j.ID)
	}
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(rn.ID, a.ID, a.Number, Finish{Status: Finished, Session: j.Session,
		Workspace: &Workspace{Kind: WorkspaceCopy, Space: j.ID}, Result: []byte(`{"changeset":"` + hexOf('1') + `"}`)}); err != nil {
		t.Fatal(err)
	}
	c.add(RunnerFresh + time.Second)
	capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	if _, _, err := q.Apply(DefaultTeam, j.Turn); !errors.Is(err, ErrMachineOffline) {
		t.Fatalf("apply with its machine offline: %v", err)
	}
	if applies, _ := q.sessionApplies(DefaultTeam, ids.WorkspaceID(j.Session)); len(applies) != 0 {
		t.Fatalf("%d applies made", len(applies))
	}
	if _, err := q.RunnerBeat(rn.ID, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, created, err := q.Apply(DefaultTeam, j.Turn); err != nil || !created {
		t.Fatalf("apply with its machine back: %v %v", created, err)
	}
}

// A later turn's workspace names where its diff starts: the end of the
// session's latest turn whose changeset control holds. A turn whose finish
// was refused after its lease ran out holds none, so the diff after it
// starts before it.
func TestDiffFromIsTheLastTurnControlHolds(t *testing.T) {
	q, c := open(t)
	rn := capsRunner(t, q, nil, nil)
	first := makeJob(t, q, Spec{Harness: "claude", Folder: "/x", Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Changes: &workspace.Changes{To: "t_end1"}})
	if t1.ID != first.ID {
		t.Fatalf("ran %s", t1.ID)
	}
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	_, a := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	c.add(lease + time.Second)
	if _, err := q.Sweep(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Result{Changeset: hexOf('2'), Changes: &workspace.Changes{To: "t_end2"}})
	if _, err := q.End(rn.ID, a.ID, a.Number, Finish{Status: Finished, Result: b}); err == nil {
		t.Fatal("the finish after the lease ran out was taken")
	}
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	j, _ := take(t, q, rn)
	if j.Workspace == nil || j.Workspace.DiffFrom != "t_end1" {
		t.Fatalf("workspace %+v", j.Workspace)
	}
}

// A session whose first turn ends without an accepted finish is ended,
// however that turn ended and whatever its isolation: the turn queued
// behind it ends with SessionEnded, no runner gets it, and a new turn is
// refused. One rule (sessionEnded) decides every case.
func TestSessionEndsWithItsFirstTurn(t *testing.T) {
	for name, c := range map[string]struct {
		spec Spec
		end  func(t *testing.T, q *Queue, c *clock, rn Runner, first Job)
	}{
		"lease lost, isolated":        {Spec{Harness: "claude", Folder: "/x", Isolate: true}, loseLease},
		"lease lost, folder as it is": {Spec{Harness: "claude", Folder: "/x"}, loseLease},
		"finish refused after the lease": {Spec{Harness: "claude", Folder: "/x", Isolate: true}, func(t *testing.T, q *Queue, c *clock, rn Runner, first Job) {
			_, a := take(t, q, rn)
			if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
				t.Fatal(err)
			}
			c.add(lease + time.Second)
			if _, err := q.End(rn.ID, a.ID, a.Number, Finish{Status: Finished, Session: first.Session,
				Workspace: &Workspace{Kind: WorkspaceCopy, Space: first.ID}}); err == nil {
				t.Fatal("a finish after the lease was taken")
			}
		}},
		"cancelled while queued": {Spec{Harness: "claude", Folder: "/x", Isolate: true}, func(t *testing.T, q *Queue, c *clock, rn Runner, first Job) {
			if _, err := q.Cancel(DefaultTeam, first.ID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			q, clk := open(t)
			rn := capsRunner(t, q, nil, nil)
			c.spec.Prompt = "hi"
			first, _, err := q.Create(DefaultTeam, KindTurn, "", c.spec, Links{})
			if err != nil {
				t.Fatal(err)
			}
			clk.add(time.Millisecond) // made after the first, as on a real clock
			next, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: ids.SessionID(first.Session), Prompt: "next"}, Links{})
			if err != nil {
				t.Fatalf("behind the first turn: %v", err)
			}
			c.end(t, q, clk, rn, first)
			if j, _, ok, _, err := q.Take(rn.ID, lease, false); err != nil || ok {
				t.Fatalf("a runner took a turn: %v %v: %s (first %s, next %s)", ok, err, j.ID, first.ID, next.ID)
			}
			got := state(t, q, next.ID)
			r, _ := ReadResult(got.Result)
			if got.State != Failed || got.Reason != ReasonSetup || r.Refused != SessionEnded {
				t.Fatalf("the queued next turn: %s %s %q", got.State, got.Reason, r.Refused)
			}
			if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: ids.SessionID(first.Session), Prompt: "again"}, Links{}); !errors.Is(err, ErrSessionEnded) {
				t.Fatalf("a new turn: %v", err)
			}
		})
	}
}

// A session whose first turn finished with its workspace goes on.
func TestSessionGoesOnAfterItsFirstFinish(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, nil)
	first := makeJob(t, q, Spec{Harness: "claude", Folder: "/x"})
	run(t, q, rn, Result{})
	if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: ids.SessionID(first.Session), Prompt: "next"}, Links{}); err != nil {
		t.Fatal(err)
	}
	if j, _ := take(t, q, rn); j.Workspace == nil {
		t.Fatal("the next turn has no workspace")
	}
}

// loseLease starts the first turn and lets its lease run out.
func loseLease(t *testing.T, q *Queue, c *clock, rn Runner, first Job) {
	t.Helper()
	_, a := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	c.add(lease + time.Second)
}

// A preview's job goes once read (DropPreview), never before it ends, and
// one the API gave up on (cancelled) goes with the session's next preview:
// the jobs table keeps no finished preview.
func TestDropPreviews(t *testing.T) {
	q, _ := open(t)
	mk := func(session string) Job {
		t.Helper()
		j, _, err := q.create(DefaultTeam, KindPreview, "", Spec{Session: ids.SessionID(session)}, Links{}, nil, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	exists := func(j Job) bool {
		_, err := q.Get(DefaultTeam, j.ID)
		return err == nil
	}
	open, read, gaveUp, other := mk("s1"), mk("s1"), mk("s1"), mk("s2")
	for _, j := range []Job{read, gaveUp, other} {
		if _, err := q.Cancel(DefaultTeam, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.DropPreview(DefaultTeam, open.ID); err != nil || !exists(open) {
		t.Fatalf("a preview not ended was dropped: %v", err)
	}
	if err := q.DropPreview(DefaultTeam, read.ID); err != nil || exists(read) {
		t.Fatalf("a read preview stayed: %v", err)
	}
	if err := q.dropPreviews(DefaultTeam, `session = ? AND cancel = 1`, "s1"); err != nil {
		t.Fatal(err)
	}
	if exists(gaveUp) || !exists(open) || !exists(other) {
		t.Fatalf("after s1's next preview: gave up %v, open %v, s2's %v", exists(gaveUp), exists(open), exists(other))
	}
	// A turn job is never a preview's to drop.
	turn := job(t, q, "")
	q.Cancel(DefaultTeam, turn.ID)
	if err := q.DropPreview(DefaultTeam, turn.ID); err != nil || !exists(turn) {
		t.Fatalf("a turn job was dropped: %v", err)
	}
}

// AppliableCursor is the latest appliable turn and the applied count, and it
// moves only for what changes an apply: a new appliable turn or an apply. A
// turn that has not ended (R2) and an ended turn with no changeset -- a failed
// upload (R4) -- are not appliable and leave the cursor where it was, so a
// stored preview keyed to the cursor is still reused.
func TestAppliableCursor(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	if _, _, ok, err := q.AppliableCursor(DefaultTeam, "nope"); err != nil || ok {
		t.Fatalf("no turns: ok %v err %v", ok, err)
	}
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1')})
	turn, applied, ok, err := q.AppliableCursor(DefaultTeam, ids.WorkspaceID(first.Session))
	if err != nil || !ok || turn != t1.Turn || applied != 0 {
		t.Fatalf("after t1: turn %q applied %d ok %v err %v", turn, applied, ok, err)
	}
	// A second turn that has not ended is not appliable: cursor unchanged (R2).
	makeJob(t, q, Spec{Session: ids.SessionID(first.Session)})
	if turn2, applied2, _, _ := q.AppliableCursor(DefaultTeam, ids.WorkspaceID(first.Session)); turn2 != t1.Turn || applied2 != 0 {
		t.Fatalf("a not-ended turn moved the cursor: turn %q applied %d", turn2, applied2)
	}
	// End that turn with no changeset -- a failed upload: still not appliable,
	// cursor unchanged (R4).
	run(t, q, rn, Result{})
	if turn3, applied3, _, _ := q.AppliableCursor(DefaultTeam, ids.WorkspaceID(first.Session)); turn3 != t1.Turn || applied3 != 0 {
		t.Fatalf("a no-changeset turn moved the cursor: turn %q applied %d", turn3, applied3)
	}
}

// endApply takes the session's pending apply and ends it with a crafted
// outcome, so a test can make a finished, refused, or partial apply.
func endApply(t *testing.T, q *Queue, r Runner, status, reason string, res Result) {
	t.Helper()
	_, a := takeFolder(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if _, err := q.End(r.ID, a.ID, a.Number, Finish{Status: status, Reason: reason, Result: b}); err != nil {
		t.Fatal(err)
	}
}

// appliableCursor's applies count moves for every apply job of the session --
// pending (P1), finished, refused, or failed part way (P2) -- because any
// apply may have changed the folder and the stored preview is retaken; what it
// did is read fresh on the re-run.
func TestAppliableCursorCountsApplies(t *testing.T) {
	setup := func(t *testing.T) (*Queue, Runner, string, string) {
		q, _ := open(t)
		rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
		first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
		t1 := run(t, q, rn, Result{Changeset: hexOf('1')})
		return q, rn, first.Session, t1.Turn
	}
	count := func(t *testing.T, q *Queue, session string) int {
		_, n, ok, err := q.AppliableCursor(DefaultTeam, ids.WorkspaceID(session))
		if err != nil || !ok {
			t.Fatalf("cursor: ok %v err %v", ok, err)
		}
		return n
	}
	t.Run("pending apply counts", func(t *testing.T) {
		q, _, session, turn := setup(t)
		if _, _, err := q.Apply(DefaultTeam, turn); err != nil {
			t.Fatal(err)
		}
		if n := count(t, q, session); n != 1 {
			t.Fatalf("a pending apply: applies %d, want 1", n)
		}
	})
	t.Run("finished apply counts", func(t *testing.T) {
		q, rn, session, turn := setup(t)
		if _, _, err := q.Apply(DefaultTeam, turn); err != nil {
			t.Fatal(err)
		}
		endApply(t, q, rn, Finished, "", Result{Apply: &ApplyResult{Files: []string{"a.txt"}}})
		if n := count(t, q, session); n != 1 {
			t.Fatalf("a finished apply: applies %d, want 1", n)
		}
	})
	t.Run("refused apply counts", func(t *testing.T) {
		q, rn, session, turn := setup(t)
		if _, _, err := q.Apply(DefaultTeam, turn); err != nil {
			t.Fatal(err)
		}
		endApply(t, q, rn, Failed, ReasonConflict, Result{Apply: &ApplyResult{Files: []string{}}})
		if n := count(t, q, session); n != 1 {
			t.Fatalf("a refused apply: applies %d, want 1", n)
		}
	})
	t.Run("failed apply that wrote a file counts", func(t *testing.T) {
		q, rn, session, turn := setup(t)
		if _, _, err := q.Apply(DefaultTeam, turn); err != nil {
			t.Fatal(err)
		}
		endApply(t, q, rn, Failed, ReasonSetup, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}})
		if n := count(t, q, session); n != 1 {
			t.Fatalf("a partial apply: applies %d, want 1", n)
		}
	})
}

// What is applied and what is appliable has one owner, the record: the queue
// reads it there (ReadAppliedWith) and keeps no fold of its own job rows, so
// the two cannot disagree. Here the job rows hold an ended turn with a
// changeset, but the record says nothing is appliable: the queue follows the
// record. Then the record names the turn, with its own changeset, and the apply
// goes to that changeset, not to the job result's (his call, 2026-11-24).
func TestAppliedStateIsTheRecords(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	ws := ids.WorkspaceID(first.Session)
	t1 := run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})

	rec := applied.Empty()
	q.ReadAppliedWith(func(ids.WorkspaceID) (applied.State, error) { return rec, nil })
	if _, _, ok, err := q.AppliableCursor(DefaultTeam, ws); err != nil || ok {
		t.Fatalf("cursor with an empty record: ok %v err %v", ok, err)
	}
	if _, _, err := q.Apply(DefaultTeam, t1.Turn); !errors.Is(err, ErrNotApplicable) {
		t.Fatalf("apply of a turn the record does not hold: %v", err)
	}
	if _, _, err := q.Discard(DefaultTeam, ws, conA, nil); !errors.Is(err, ErrAllApplied) {
		t.Fatalf("discard with an empty record: %v", err)
	}
	if dropped, err := q.DroppedTurns(DefaultTeam, ws); err != nil || len(dropped) != 0 {
		t.Fatalf("dropped with an empty record: %v %v", dropped, err)
	}

	var f applied.Fold
	f.Changed(t1.Turn, hexOf('9'), "cp9")
	rec = f.State()
	if turn, _, ok, err := q.AppliableCursor(DefaultTeam, ws); err != nil || !ok || turn != t1.Turn {
		t.Fatalf("cursor: %q %v %v", turn, ok, err)
	}
	aj, created, err := q.Apply(DefaultTeam, t1.Turn)
	if err != nil || !created || aj.Spec.ApplyTo != hexOf('9') {
		t.Fatalf("apply: to %q created %v err %v, want the record's changeset", aj.Spec.ApplyTo, created, err)
	}
}

// Without a record the queue cannot say what is appliable, and says so.
func TestAppliedStateNeedsARecord(t *testing.T) {
	q, _ := open(t)
	q.ReadAppliedWith(nil)
	if _, _, _, err := q.AppliableCursor(DefaultTeam, "ws_x"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("no record: %v", err)
	}
}
