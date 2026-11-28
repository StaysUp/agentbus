package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A turn token control minted works while its attempt is current; after the
// attempt ends it reads as ended, so the API can say so. A token control did
// not sign never does.
func TestTurnTokenEnded(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := q.RegisterRunner(queue.DefaultTeam, "r", nil, 1, queue.HashToken("tok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	_, a, ok, _, err := q.Take(rn.ID, 30*time.Second, queue.LaneInteractive)
	if err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	tok := s.mint(rn.ID, a.ID, a.Number)
	if _, live := s.TurnGrant(tok); !live || s.TurnTokenEnded(tok) {
		t.Fatalf("a running turn's token: live %v, ended %v", live, s.TurnTokenEnded(tok))
	}
	if _, err := q.End(rn.ID, a.ID, a.Number, queue.Finish{Status: queue.Finished}); err != nil {
		t.Fatal(err)
	}
	if _, live := s.TurnGrant(tok); live || !s.TurnTokenEnded(tok) {
		t.Errorf("the token after its attempt ended: live %v, ended %v", live, s.TurnTokenEnded(tok))
	}
	for _, other := range []string{"", mainTok, "abr_" + rn.ID + "." + a.ID + ".1.forged", tok[:len(tok)-1], "abr_" + queue.NewID("")} {
		if s.TurnTokenEnded(other) {
			t.Errorf("%q reads as an ended turn token", other)
		}
	}
}

// A turn's control-minted token (what up and every runner use) stops working
// once the turn's child has exited -- when its run.ended is in the record --
// not only when the runner finishes the attempt. Between the two the runner
// uploads the turn's changes, which takes seconds for a large changeset; a
// process the agent left behind must not keep the token, or the stand-in,
// working in that window. The engine revokes its own-minted token at child
// exit (run/slots.go, TestTurnTokenRevokedAtProcessExitNotPipeClose), but in
// up and runner mode the token is control-minted, so control alone must end
// it here.
func TestTurnTokenEndsWhenChildExits(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Record: st})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := q.RegisterRunner(queue.DefaultTeam, "r", nil, 1, queue.HashToken("tok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	_, a, ok, _, err := q.Take(rn.ID, 30*time.Second, queue.LaneInteractive)
	if err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := q.Start(rn.ID, a.ID, a.Number, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	tok := s.mint(rn.ID, a.ID, a.Number)
	standIn := s.standIn(rn.ID, a.ID, a.Number)
	j, _, live := s.turnJob(tok)
	if !live {
		t.Fatal("a running turn's token is not live")
	}
	if _, ok := s.standInJob(standIn); !ok {
		t.Fatal("a running turn's stand-in is not live")
	}
	// The child exits: the engine stores the turn's run.ended. The runner has
	// not finished the attempt yet -- it still has the changeset to upload --
	// so the attempt is still current.
	if _, _, err := st.Append(event.Event{ID: "evt_end", Kind: event.RunEnded, Session: j.Session, Turn: j.Turn}); err != nil {
		t.Fatal(err)
	}
	if _, live := s.TurnGrant(tok); live {
		t.Error("the turn token still works after the turn's child exited (run.ended is in the record)")
	}
	if !s.TurnTokenEnded(tok) {
		t.Error("TurnTokenEnded is false after the child exited, so the API cannot tell a left-behind process its token ended")
	}
	if _, ok := s.standInJob(standIn); ok {
		t.Error("the turn's stand-in credential still works after the child exited, so a left-behind process keeps spending the person's model credential")
	}
}
