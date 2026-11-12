//go:build integration

package api

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A turn records the settings version it started under on its turn.started:
// the version the last change made.
func TestTurnRecordsSettingsVersion(t *testing.T) {
	db, err := settings.OpenDB(filepath.Join(t.TempDir(), settings.DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, text := range []string{"one {session}", "two {session}"} {
		if _, err := db.SetInstructions(text, settings.Any); err != nil {
			t.Fatal(err)
		}
	}
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0", "sh"}}, nil
	}
	e := stackEnv(t, stack{wire: wire, settings: db})
	code, out := asTok(t, e, token, "POST", "/api/v0/turns", `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"hello"}`)
	var started Started
	if code != 202 || json.Unmarshal([]byte(out), &started) != nil {
		t.Fatalf("start: %d %s", code, out)
	}
	waitSessionClosed(t, e.store, started.Session)
	recs, err := e.store.After(0, store.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range recs {
		var ev event.Event
		if json.Unmarshal(r.Data, &ev) == nil && ev.Kind == event.TurnStarted && ev.Turn == started.Turn {
			found = true
			if ev.SettingsVersion != 2 {
				t.Fatalf("turn.started settings_version %d, want 2: %s", ev.SettingsVersion, r.Data)
			}
		}
	}
	if !found {
		t.Fatal("no turn.started")
	}
}
