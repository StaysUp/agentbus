package queue

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/staysup/agentbus/harness"
)

// An apply's key holds a "/", as no caller's key may, so no other job can
// take it first.
func TestApplyKeyIsReserved(t *testing.T) {
	if k := ApplyKey("t_1", 0); !strings.Contains(k, "/") || !strings.Contains(ResultKey("job_1"), "/") {
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

// run takes the next job on r and finishes it with res: the session's
// first turn also records its copy.
func run(t *testing.T, q *Queue, r Runner, res Result) Job {
	t.Helper()
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

// An apply up to a turn starts from the last turn applied, and covers every
// turn before it: asking for an earlier one answers the apply that did it.
func TestApplyUpToATurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, nil)
	conn := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
	t1 := run(t, q, rn, Result{Changeset: hexOf('1')})
	if t1.ID != first.ID {
		t.Fatalf("ran %s", t1.ID)
	}
	var turns []Job
	turns = append(turns, t1)
	for _, c := range []byte{'2', '3'} {
		makeJob(t, q, Spec{Session: first.Session})
		turns = append(turns, run(t, q, rn, Result{Changeset: hexOf(c)}))
	}
	a2, created, err := q.Apply(DefaultTeam, turns[1].Attempt)
	if err != nil || !created || a2.Spec.ApplyFrom != "" || a2.Spec.ApplyTo != hexOf('2') || a2.Spec.Connector != conA || a2.Spec.FolderRef != refX {
		t.Fatalf("apply up to turn 2: %+v %v %v", a2.Spec, created, err)
	}
	// Only the connector takes it; the runner of the session never does.
	if got := takes(t, q, rn); got != "" {
		t.Fatalf("the runner took %s", got)
	}
	// While it runs, an apply of turn 1 is that apply; of turn 3, busy.
	if j, created, err := q.Apply(DefaultTeam, turns[0].Attempt); err != nil || created || j.ID != a2.ID {
		t.Fatalf("turn 1 while turn 2's apply runs: %s %v %v", j.ID, created, err)
	}
	if _, _, err := q.Apply(DefaultTeam, turns[2].Attempt); err != ErrApplyBusy {
		t.Fatalf("turn 3 while turn 2's apply runs: %v", err)
	}
	run(t, q, conn, Result{Apply: &ApplyResult{Files: []string{"a.txt"}}})
	if j, ok, err := q.AppliedBy(DefaultTeam, turns[0].Attempt); err != nil || !ok || j.ID != a2.ID {
		t.Fatalf("turn 1 applied by %s %v %v", j.ID, ok, err)
	}
	if j, created, err := q.Apply(DefaultTeam, turns[0].Attempt); err != nil || created || j.ID != a2.ID {
		t.Fatalf("turn 1 after turn 2's apply: %s %v %v", j.ID, created, err)
	}
	a3, created, err := q.Apply(DefaultTeam, turns[2].Attempt)
	if err != nil || !created || a3.Spec.ApplyFrom != hexOf('2') || a3.Spec.ApplyTo != hexOf('3') {
		t.Fatalf("apply up to turn 3: %+v %v %v", a3.Spec, created, err)
	}
	if _, ok, _ := q.AppliedBy(DefaultTeam, turns[2].Attempt); !ok {
		t.Fatal("turn 3 has no apply")
	}
}

// Snapshot and apply jobs go only to the connector of their folder; a
// connector takes nothing else, and an isolated first turn goes to any
// runner, which builds its copy from a snapshot.
func TestTakeRoutesByRole(t *testing.T) {
	q, _ := open(t)
	plain := capsRunner(t, q, nil, nil)
	connB := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conB})
	connA := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	turn := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
	for _, r := range []Runner{connA, connB} {
		if got := takes(t, q, r); got != "" {
			t.Fatalf("a connector took the turn %s", got)
		}
	}
	if got := takes(t, q, plain); got != turn.ID {
		t.Fatalf("the runner took %q, want the isolated turn %s", got, turn.ID)
	}
	snap, err := q.Snapshot(plain.ID, mustAttempt(t, q, turn.ID), 1)
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.Snapshot(plain.ID, mustAttempt(t, q, turn.ID), 1)
	if err != nil || again.ID != snap.ID || snap.Kind != KindSnapshot {
		t.Fatalf("asked twice: %s and %s (%v)", snap.ID, again.ID, err)
	}
	for _, r := range []Runner{plain, connB} {
		if got := takes(t, q, r); got != "" {
			t.Fatalf("%s took the snapshot", got)
		}
	}
	if got := takes(t, q, connA); got != snap.ID {
		t.Fatalf("the folder's connector took %q, want %s", got, snap.ID)
	}
	if _, err := q.Snapshot(connA.ID, mustAttempt(t, q, turn.ID), 1); err == nil {
		t.Fatal("another runner asked for the turn's snapshot")
	}
	if (Capabilities{Role: "x"}).Check() == "" || (Capabilities{Role: RoleConnector}).Check() == "" ||
		(Capabilities{Role: RoleConnector, Connector: conA}).Check() != "" {
		t.Fatal("a connector's capabilities are not checked")
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
	rn := capsRunner(t, q, nil, nil)
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
	turns := []Job{run(t, q, rn, Result{Changeset: hexOf('1')})}
	for _, c := range []byte{'2', '3'} {
		makeJob(t, q, Spec{Session: first.Session})
		turns = append(turns, run(t, q, rn, Result{Changeset: hexOf(c)}))
	}
	type answer struct {
		j       Job
		created bool
		err     error
	}
	out := make(chan answer, 2)
	for _, tn := range []Job{turns[1], turns[2]} {
		go func() { j, c, err := q.Apply(DefaultTeam, tn.Attempt); out <- answer{j, c, err} }()
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
	applies, _ := q.sessionApplies(DefaultTeam, first.Session)
	if made != 1 || len(applies) != 1 {
		t.Fatalf("%d made, %d applies", made, len(applies))
	}
}

// An isolated first turn goes to a runner with its harness, wherever the
// folder is: a runner on the folder's machine without it does not take it.
func TestTakeIsolatedByHarness(t *testing.T) {
	q, _ := open(t)
	found := func(name string) []harness.Report { return []harness.Report{{Name: name, Found: true}} }
	here := capsRunner(t, q, nil, &Capabilities{Connector: conA, Harnesses: found("claude")})
	away := capsRunner(t, q, nil, &Capabilities{Harnesses: found("codex")})
	j := makeJob(t, q, Spec{Harness: "codex", FolderRef: refX, Connector: conA, Isolate: true})
	if got := takes(t, q, here); got != "" {
		t.Fatalf("the runner without codex took %s", got)
	}
	if got := takes(t, q, away); got != j.ID {
		t.Fatalf("the runner with codex took %q", got)
	}
}

// The snapshot job an attempt asked for ends with it: queued, it is
// cancelled; taken, it is asked to stop.
func TestSnapshotEndsWithItsAttempt(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, nil)
	conn := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	for _, taken := range []bool{false, true} {
		turn := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
		j, a := take(t, q, rn)
		if j.ID != turn.ID {
			t.Fatalf("took %s", j.ID)
		}
		if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
			t.Fatal(err)
		}
		snap, err := q.Snapshot(rn.ID, a.ID, a.Number)
		if err != nil {
			t.Fatal(err)
		}
		if taken && takes(t, q, conn) != snap.ID {
			t.Fatal("the connector did not take the snapshot")
		}
		if _, err := q.End(rn.ID, a.ID, a.Number, Finish{Status: Cancelled}); err != nil {
			t.Fatal(err)
		}
		got := state(t, q, snap.ID)
		if !taken && got.State != Cancelled || taken && !got.CancelRequested {
			t.Fatalf("taken %v: the snapshot job is %s, cancel %v", taken, got.State, got.CancelRequested)
		}
	}
}
