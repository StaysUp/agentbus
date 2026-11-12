package queue

import (
	"errors"
	"testing"
	"time"
)

// A session runs one turn or command at a time, in the order they were
// made, on the session's runner; a closed session takes no command.
func TestCommandWaitsItsTurn(t *testing.T) {
	q, clock := open(t)
	a, b := runner(t, q, 4), runner(t, q, 4)
	first, _, _ := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Folder: "/f", Prompt: "one"}, Links{})
	_, att := take(t, q, a)
	if _, _, err := q.Start(a.ID, att.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	s := first.Session
	if _, err := q.End(a.ID, att.ID, 1, Finish{Status: Finished, Session: s, Workspace: &Workspace{Kind: WorkspaceCopy, Space: "sp1"}}); err != nil {
		t.Fatal(err)
	}
	turn, _, _ := q.Create(DefaultTeam, KindTurn, "", Spec{Session: s, Prompt: "two"}, Links{})
	clock.add(time.Millisecond)
	cmd, _, err := q.Create(DefaultTeam, KindCommand, "", Spec{Session: s, Command: "ls"}, Links{})
	if err != nil || cmd.Turn != "" || cmd.Session != s {
		t.Fatalf("command job: %+v %v", cmd, err)
	}
	if _, _, ok, _, _ := q.Take(b.ID, lease, false); ok {
		t.Fatal("another runner took the session's job")
	}
	j, att2 := take(t, q, a)
	if j.ID != turn.ID {
		t.Fatalf("took %s first, want the turn", j.Kind)
	}
	if _, _, ok, _, _ := q.Take(a.ID, lease, false); ok {
		t.Fatal("the command ran beside the session's turn")
	}
	if _, err := q.End(a.ID, att2.ID, 1, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	j, att3 := take(t, q, a)
	if j.ID != cmd.ID || j.Workspace == nil || j.Workspace.Space != "sp1" || j.Spec.Command != "ls" {
		t.Fatalf("the command job: %+v", j)
	}
	// A turn sent while the command runs waits for it.
	q.Create(DefaultTeam, KindTurn, "", Spec{Session: s, Prompt: "three"}, Links{})
	if _, _, ok, _, _ := q.Take(a.ID, lease, false); ok {
		t.Fatal("a turn ran beside the session's command")
	}
	if busy, _ := q.SessionHasWork(DefaultTeam, s); !busy {
		t.Fatal("a running command is not work")
	}
	if _, err := q.CloseSession(DefaultTeam, s); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("close while a command runs: %v", err)
	}
	if _, err := q.End(a.ID, att3.ID, 1, Finish{Status: Finished, Result: []byte(`{"ran":{"kind":"command.ran"}}`)}); err != nil {
		t.Fatal(err)
	}
	if got := state(t, q, cmd.ID); got.State != Finished {
		t.Fatalf("command: %s", got.State)
	}
	open, err := q.List(DefaultTeam, Filter{Session: s, Open: true}, 10)
	if err != nil || len(open) != 1 || open[0].Spec.Prompt != "three" {
		t.Fatalf("open jobs: %+v %v", open, err)
	}
	q.Cancel(DefaultTeam, open[0].ID)
	if _, err := q.CloseSession(DefaultTeam, s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Create(DefaultTeam, KindCommand, "", Spec{Session: s, Command: "ls"}, Links{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("a command in a closed session: %v", err)
	}
}

// A command and a review of its session never run at once: each waits for
// the other. A turn and a review still do, as before.
func TestCommandLaneWithReviews(t *testing.T) {
	q, clock := open(t)
	a := runner(t, q, 4)
	first, _, _ := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Folder: "/f", Prompt: "one"}, Links{})
	_, att := take(t, q, a)
	q.Start(a.ID, att.ID, 1, lease)
	s := first.Session
	if _, err := q.End(a.ID, att.ID, 1, Finish{Status: Finished, Result: authorResult(), Session: s,
		Workspace: &Workspace{Kind: WorkspaceCopy, Space: "sp1"}}); err != nil {
		t.Fatal(err)
	}
	rev, _, err := q.Review(DefaultTeam, first.ID, "claude", "", "", false)
	if err != nil || rev.Session != s {
		t.Fatalf("review: %+v %v", rev, err)
	}
	clock.add(time.Millisecond)
	cmd, _, _ := q.Create(DefaultTeam, KindCommand, "", Spec{Session: s, CommandID: "cmd_1", Command: "ls"}, Links{})
	j, ratt := take(t, q, a)
	if j.ID != rev.ID {
		t.Fatalf("took %s first, want the review", j.Kind)
	}
	if _, _, ok, _, _ := q.Take(a.ID, lease, false); ok {
		t.Fatal("the command ran beside a review of its session")
	}
	// A turn of the session is not held up by the review.
	clock.add(time.Millisecond)
	q.Create(DefaultTeam, KindTurn, "", Spec{Session: s, Prompt: "two"}, Links{})
	if _, _, ok, _, _ := q.Take(a.ID, lease, false); ok {
		t.Fatal("the turn jumped the command queued before it")
	}
	q.Start(a.ID, ratt.ID, 1, lease)
	if _, err := q.End(a.ID, ratt.ID, 1, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	j, _ = take(t, q, a)
	if j.ID != cmd.ID {
		t.Fatalf("took %s, want the command", j.Kind)
	}
	// A review asked while the command runs waits for it.
	if _, _, err := q.Review(DefaultTeam, first.ID, "claude", "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _, _ := q.Take(a.ID, lease, false); ok {
		t.Fatal("a job ran beside the session's command")
	}
	if got, err := q.JobOfCommand(DefaultTeam, "cmd_1"); err != nil || got.ID != cmd.ID {
		t.Fatalf("JobOfCommand: %v %v", got.ID, err)
	}
}
