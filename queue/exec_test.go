package queue

import (
	"strings"
	"testing"

	"github.com/staysup/agentbus/ids"
)

// commandIn makes a command in the workspace ws, asked by turn asking of
// session, as POST /api/v0/commands does.
func commandIn(t *testing.T, q *Queue, ws ids.WorkspaceID, session, asking, line string) Job {
	t.Helper()
	c, created, err := q.CreateCommand(DefaultTeam, "", Spec{Session: ids.SessionID(session), Workspace: ws, Machine: conA, Isolate: true,
		Command: line, CommandTimeout: 60},
		Links{Chain: &Chain{Root: session, Depth: 1, By: asking}}, Limits{MaxDepth: 3, MaxPerTurn: 5, MaxPerTree: 10},
		&Delivery{Session: session, Maker: Maker{Session: session, Turn: asking}})
	if err != nil || !created {
		t.Fatalf("CreateCommand: %v", err)
	}
	return c
}

// startTaken takes the next job and starts it, as a runner does.
func startTaken(t *testing.T, q *Queue, rn Runner) (Job, Attempt) {
	t.Helper()
	j, a := take(t, q, rn)
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	return j, a
}

// H1's lane: a command waits for the turn that asked for it (and anything made
// before it); once it runs, a turn the person sends does not wait for it, but a
// close does; and a second command waits for the first.
func TestCommandLane(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws, sess := ids.WorkspaceID(first.Session), first.Session

	asking := makeJob(t, q, Spec{Session: ids.SessionID(sess)})
	aj, aa := startTaken(t, q, rn)
	if aj.ID != asking.ID {
		t.Fatalf("took %s, want the asking turn", aj.ID)
	}
	c := commandIn(t, q, ws, sess, asking.Turn, "make gate")
	if !strings.HasPrefix(c.Turn, "c_") || c.Session != "" || c.Kind != KindExec {
		t.Fatalf("the command's job: kind %s turn %s session %q", c.Kind, c.Turn, c.Session)
	}
	if ahead, err := q.TurnsAhead(DefaultTeam, c.ID); err != nil || len(ahead) != 1 || ahead[0].ID != asking.ID {
		t.Fatalf("the command waits for %+v (%v), want the asking turn", ahead, err)
	}
	if got := takes(t, q, rn); got != "" {
		t.Fatalf("took %s while the asking turn runs", got)
	}
	// JobByTurn never names a command; JobByCommand does.
	if _, err := q.JobByTurn(DefaultTeam, c.Turn); err == nil {
		t.Fatal("JobByTurn found the command")
	}
	if j, err := q.JobByCommand(DefaultTeam, c.Turn); err != nil || j.ID != c.ID {
		t.Fatalf("JobByCommand: %v", err)
	}
	if _, err := q.End(rn.ID, aa.ID, aa.Number, Finish{Status: Finished, Result: mustResult(Result{Changeset: hexOf('2'), Checkpoint: "cp2"})}); err != nil {
		t.Fatal(err)
	}
	cj, ca := startTaken(t, q, rn)
	if cj.ID != c.ID {
		t.Fatalf("took %s (%s), want the command", cj.ID, cj.Kind)
	}
	// While it runs: the person's next turn does not wait for it.
	next := makeJob(t, q, Spec{Session: ids.SessionID(sess)})
	if ahead, err := q.TurnsAhead(DefaultTeam, next.ID); err != nil || len(ahead) != 0 {
		t.Fatalf("the person's turn waits for %+v (%v), want nothing", ahead, err)
	}
	if got := takes(t, q, rn); got != next.ID {
		t.Fatalf("took %q while the command runs, want the person's turn %s", got, next.ID)
	}
	// A second command waits for the first, and for that turn, made before it.
	c2 := commandIn(t, q, ws, sess, next.Turn, "make test")
	if ahead, err := q.TurnsAhead(DefaultTeam, c2.ID); err != nil || len(ahead) != 2 {
		t.Fatalf("the second command waits for %+v (%v), want the first command and the turn", ahead, err)
	}
	// A close waits for the running command.
	cl, _, err := q.CloseWorkspace(DefaultTeam, ws, conA, nil)
	if err != nil {
		t.Fatal(err)
	}
	ahead, err := q.TurnsAhead(DefaultTeam, cl.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitsFor := map[string]bool{}
	for _, j := range ahead {
		waitsFor[j.ID] = true
	}
	if !waitsFor[c.ID] || !waitsFor[c2.ID] {
		t.Fatalf("the close waits for %+v, want both commands among them", ahead)
	}
	if _, err := q.End(rn.ID, ca.ID, ca.Number, Finish{Status: Failed, Reason: ReasonExit,
		Result: mustResult(Result{ExitCode: ptr(2), Output: "FAIL TestX\n", OutputBytes: 9000, DurationMS: 61000})}); err != nil {
		t.Fatal(err)
	}
	ended, err := q.JobByCommand(DefaultTeam, c.Turn)
	if err != nil {
		t.Fatal(err)
	}
	// Its delivery on fires once the record says it ended.
	n, err := q.FireOns(DefaultTeam, func(s string) (bool, error) { return s == c.Turn, nil }, nil, nil, nil)
	if err != nil || n != 1 {
		t.Fatalf("FireOns: %d %v, want one delivery", n, err)
	}
	list, err := q.List(DefaultTeam, Filter{Kind: KindTurn}, 50)
	if err != nil {
		t.Fatal(err)
	}
	var delivered *Job
	for i := range list {
		if r := list[i].Links.ResultOf; r != nil && r.Job == ended.ID {
			delivered = &list[i]
		}
	}
	if delivered == nil || string(delivered.Spec.Session) != sess || delivered.Links.ResultOf.Kind != KindExec {
		t.Fatalf("no delivery of the command to %s: %+v", sess, delivered)
	}
	for _, want := range []string{"Command: make gate", "Id: " + c.Turn, "State: failed (exit)", "Exit code: 2", "Ran for: 1m1s",
		"the last 11 of 9000 bytes", "FAIL TestX"} {
		if !strings.Contains(delivered.Spec.Prompt, want) {
			t.Errorf("the delivered prompt lacks %q:\n%s", want, delivered.Spec.Prompt)
		}
	}
	// Fired once: a second sweep delivers nothing more.
	if n, err := q.FireOns(DefaultTeam, func(s string) (bool, error) { return s == c.Turn, nil }, nil, nil, nil); err != nil || n != 0 {
		t.Fatalf("a second sweep: %d %v", n, err)
	}
}

// A command counts as work its turn asked for: the per-turn cap holds it.
func TestCommandCountsAsAskedWork(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws, sess := ids.WorkspaceID(first.Session), first.Session
	lim := Limits{MaxDepth: 3, MaxPerTurn: 1, MaxPerTree: 10}
	mk := func() error {
		_, _, err := q.CreateCommand(DefaultTeam, "", Spec{Session: ids.SessionID(sess), Workspace: ws, Command: "true"},
			Links{Chain: &Chain{Root: sess, Depth: 1, By: first.Turn}}, lim, &Delivery{Session: sess, Maker: Maker{Session: sess, Turn: first.Turn}})
		return err
	}
	if err := mk(); err != nil {
		t.Fatal(err)
	}
	var limit *LimitError
	if err := mk(); err == nil || !asLimit(err, &limit) {
		t.Fatalf("a second command past one per turn: %v, want a LimitError", err)
	}
}

func asLimit(err error, out **LimitError) bool {
	l, ok := err.(*LimitError)
	if ok {
		*out = l
	}
	return ok
}

func ptr[T any](v T) *T { return &v }

// An on the main token made over a command (no asking turn of its own)
// delivers on the agent of the turn that asked for the command: a command runs
// no agent (review P2, 2026-11-27).
func TestOwnerOnOverACommandDelivers(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", AgentID: "a_1", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws, sess := ids.WorkspaceID(first.Session), first.Session
	c := commandIn(t, q, ws, sess, first.Turn, "make gate")
	if _, err := q.RegisterOn(DefaultTeam, OnCommandEnded, c.Turn, sess, "", Maker{}); err != nil {
		t.Fatal(err)
	}
	cj, ca := startTaken(t, q, rn)
	if cj.ID != c.ID {
		t.Fatalf("took %s, want the command", cj.ID)
	}
	if _, err := q.End(rn.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: mustResult(Result{ExitCode: ptr(0)})}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, func(s string) (bool, error) { return s == c.Turn, nil }, nil, nil, nil); err != nil || n != 2 {
		t.Fatalf("FireOns: %d %v, want two deliveries (the command's own on and the owner's)", n, err)
	}
	list, err := q.List(DefaultTeam, Filter{Kind: KindTurn}, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range list {
		if r := d.Links.ResultOf; r != nil && r.Job == c.ID {
			n++
			if d.Spec.Harness != "claude" || d.Spec.AgentID != "a_1" {
				t.Errorf("a delivery runs on %q/%q, want the asking turn's claude/a_1", d.Spec.Harness, d.Spec.AgentID)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d deliveries of the command, want 2", n)
	}
}

// Under the lanes rule a command is delegated work (an agent asked for it): a
// turn the person sends while the command still waits goes ahead of it, and the
// command does not start beside that turn, though the turn was made after it.
func TestCommandIsDelegatedInTheLane(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	ws, sess := ids.WorkspaceID(first.Session), first.Session

	asking := makeJob(t, q, Spec{Session: ids.SessionID(sess)})
	_, aa := startTaken(t, q, rn)
	c := commandIn(t, q, ws, sess, asking.Turn, "make gate")
	person := makeJob(t, q, Spec{Session: ids.SessionID(sess)})
	if _, err := q.End(rn.ID, aa.ID, aa.Number, Finish{Status: Finished, Result: mustResult(Result{Changeset: hexOf('2'), Checkpoint: "cp2"})}); err != nil {
		t.Fatal(err)
	}
	if ahead, err := q.TurnsAhead(DefaultTeam, c.ID); err != nil || len(ahead) != 1 || ahead[0].ID != person.ID {
		t.Fatalf("the queued command waits for %+v (%v), want the person's later turn", ahead, err)
	}
	pj, pa := startTaken(t, q, rn)
	if pj.ID != person.ID {
		t.Fatalf("took %s (%s), want the person's turn first", pj.ID, pj.Kind)
	}
	if got := takes(t, q, rn); got != "" {
		t.Fatalf("took %s beside the person's running turn, want the command to wait", got)
	}
	if _, err := q.End(rn.ID, pa.ID, pa.Number, Finish{Status: Finished, Result: mustResult(Result{Changeset: hexOf('3'), Checkpoint: "cp3"})}); err != nil {
		t.Fatal(err)
	}
	if cj, _ := startTaken(t, q, rn); cj.ID != c.ID {
		t.Fatalf("took %s, want the command once the person's turn ended", cj.ID)
	}
}
