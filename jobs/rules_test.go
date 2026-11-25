package jobs

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// A turn's rules are taken once, when its attempt starts: control keeps them,
// pins their version on the attempt, and sends them with the turn's token. A
// start sent again after the policy changed gets the same rules, and the
// turn's token grant names them, so its calls to the API are decided by them
// too (O2.4).
func TestTurnRulesPinnedAtStart(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := settings.OpenDB(filepath.Join(t.TempDir(), settings.DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	fr, err := testFolders(t)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Settings: db, Folders: fr})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); db.Close() })
	r := enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	if code := send(t, srv, r.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 || taken.Attempt == nil {
		t.Fatalf("take: %d", code)
	}
	att := "/api/v0/runner/attempts/" + taken.Attempt.ID + "/start"
	var first AttemptStarted
	if code := send(t, srv, r.Token, "POST", att, AttemptCall{Number: 1}, &first); code != 200 || first.Settings == nil {
		t.Fatalf("start: %d %+v", code, first)
	}
	v := first.Settings.PermissionsVersion
	if g, ok := s.TurnGrant(first.Token); !ok || g.Rules != v || g.RulesAt.IsZero() {
		t.Fatalf("the turn's grant: %+v %v, want rules %s", g, ok, v)
	}
	// The person changes the policy while the turn runs.
	p, _, _ := db.Policy()
	yes := true
	p.AgentAPI.Apply = &yes
	if _, _, err := db.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	var again AttemptStarted
	if code := send(t, srv, r.Token, "POST", att, AttemptCall{Number: 1}, &again); code != 200 || again.Settings == nil {
		t.Fatalf("start again: %d", code)
	}
	if again.Settings.PermissionsVersion != v || again.Settings.Permissions.AgentAPI.Apply == nil || *again.Settings.Permissions.AgentAPI.Apply {
		t.Errorf("a start sent again took newer rules: %s", again.Settings.PermissionsVersion)
	}
	if g, ok := s.TurnGrant(again.Token); !ok || g.Rules != v {
		t.Errorf("the turn's grant after the change: %+v", g)
	}
}
