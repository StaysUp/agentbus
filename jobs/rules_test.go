package jobs

import (
	"net/http/httptest"
	"os"
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
	if code := send(t, srv, r.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion, Lane: queue.LaneInteractive}, &taken); code != 200 || taken.Attempt == nil {
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

// startTwice takes a turn's attempt, starts it, runs edit, and starts it
// again, as a runner does when the first answer was lost.
func startTwice(t *testing.T, st settings.Store, edit func()) (first, again AttemptStarted) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fr, err := testFolders(t)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Settings: st, Folders: fr})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close() })
	r := enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	if code := send(t, srv, r.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion, Lane: queue.LaneInteractive}, &taken); code != 200 || taken.Attempt == nil {
		t.Fatalf("take: %d", code)
	}
	att := "/api/v0/runner/attempts/" + taken.Attempt.ID + "/start"
	if code := send(t, srv, r.Token, "POST", att, AttemptCall{Number: 1}, &first); code != 200 || first.Settings == nil {
		t.Fatalf("start: %d %+v", code, first)
	}
	edit()
	if code := send(t, srv, r.Token, "POST", att, AttemptCall{Number: 1}, &again); code != 200 {
		t.Fatalf("start again: %d", code)
	}
	return first, again
}

// A start sent again after the permissions file was broken gets the rules the
// first start saved: the broken file stops new turns, not this one (review
// t_12c10ac1e9371bd9).
func TestStartAgainAfterBrokenPermissions(t *testing.T) {
	dir := t.TempDir()
	f := settings.NewFiles(dir)
	if err := f.Init(); err != nil {
		t.Fatal(err)
	}
	first, again := startTwice(t, f, func() {
		if err := os.WriteFile(filepath.Join(dir, settings.PolicyFile), []byte("this is [not toml"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if again.RulesRefused != nil || again.Settings == nil {
		t.Fatalf("a start sent again was refused: %+v", again.RulesRefused)
	}
	if again.Settings.PermissionsVersion != first.Settings.PermissionsVersion {
		t.Errorf("rules %s, want the saved %s", again.Settings.PermissionsVersion, first.Settings.PermissionsVersion)
	}
}

// A start sent again after the instructions and the policy changed gets the
// whole snapshot the first start saved: the same policy, instructions and
// settings version, never old permissions with newer instructions (review
// t_12c10ac1e9371bd9).
func TestStartAgainKeepsTheWholeSnapshot(t *testing.T) {
	db, err := settings.OpenDB(filepath.Join(t.TempDir(), settings.DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	first, again := startTwice(t, db, func() {
		if _, err := db.SetInstructions("newer instructions", settings.Any); err != nil {
			t.Fatal(err)
		}
		p, _, _ := db.Policy()
		yes := true
		p.AgentAPI.Apply = &yes
		if _, _, err := db.SetPolicy(p, settings.Any); err != nil {
			t.Fatal(err)
		}
	})
	if again.Settings == nil {
		t.Fatalf("a start sent again was refused: %+v", again.RulesRefused)
	}
	a, b := *first.Settings, *again.Settings
	if b.Instructions != a.Instructions || b.InstructionsVersion != a.InstructionsVersion || b.SettingsVersion != a.SettingsVersion ||
		b.PermissionsVersion != a.PermissionsVersion {
		t.Errorf("a start sent again took newer settings:\n first %s %s %d %s\n again %s %s %d %s",
			a.PermissionsVersion, a.InstructionsVersion, a.SettingsVersion, a.Instructions,
			b.PermissionsVersion, b.InstructionsVersion, b.SettingsVersion, b.Instructions)
	}
}
