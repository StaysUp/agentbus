package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/workspace"
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

// An apply up to a turn starts from the last turn applied, and covers every
// turn before it: asking for an earlier one answers the apply that did it.
func TestApplyUpToATurn(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	conn := rn
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
	a2, created, err := q.Apply(DefaultTeam, turns[1].Turn)
	if err != nil || !created || a2.Spec.ApplyFrom != "" || a2.Spec.ApplyTo != hexOf('2') || a2.Spec.Connector != conA || a2.Spec.FolderRef != refX {
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
	turn := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
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
	makeJob(t, q, Spec{Session: turn.Session})
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
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
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
	next := makeJob(t, q, Spec{Session: first.Session})
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

// Folder jobs have places of their own: an apply is taken while every turn
// place is busy, and at most FolderCapacity folder jobs run at once.
func TestFolderPlacesBesideTurns(t *testing.T) {
	q, _ := open(t)
	r, err := q.RegisterRunner(DefaultTeam, "r", nil, 1, HashToken(NewID("tok_")), &Capabilities{Connector: conA})
	if err != nil {
		t.Fatal(err)
	}
	makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA})
	makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA})
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
	applies, _ := q.sessionApplies(DefaultTeam, first.Session)
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
	j := makeJob(t, q, Spec{Harness: "codex", FolderRef: refX, Connector: conA, Isolate: true})
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
	turn := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA, Isolate: true})
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
	if applies, _ := q.sessionApplies(DefaultTeam, j.Session); len(applies) != 0 {
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
	makeJob(t, q, Spec{Session: first.Session})
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
	makeJob(t, q, Spec{Session: first.Session})
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
			next, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: first.Session, Prompt: "next"}, Links{})
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
			if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: first.Session, Prompt: "again"}, Links{}); !errors.Is(err, ErrSessionEnded) {
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
	if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: first.Session, Prompt: "next"}, Links{}); err != nil {
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
