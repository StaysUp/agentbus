package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
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
	_, a, ok, _, err := q.Take(rn.ID, 30*time.Second, false)
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
