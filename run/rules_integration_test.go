//go:build integration

package run

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A turn of control's runs under the rules control took for it when its
// attempt started (Request.Settings), never the engine's own read, and its
// turn.started names them (O2.4).
func TestTurnRunsUnderControlsRules(t *testing.T) {
	newEngine := func(t *testing.T) (*Engine, *store.Store) {
		st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		// The engine's own settings forbid nothing.
		e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: settings.NewFiles(t.TempDir()),
			Wire: func(agent, baseURL string, _ Access) (Launch, error) {
				return Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return e, st
	}
	control := settings.DefaultPolicy()
	control.Forbid = append(control.Forbid, "git push")
	ctl := &settings.RunnerSettings{Permissions: control, PermissionsVersion: "v-control", Instructions: settings.DefaultInstructions}

	// agy cannot keep a forbid list: under control's rules it is refused,
	// though the engine's own would let it start.
	e, _ := newEngine(t)
	_, err := e.Start(Request{Harness: "agy", Folder: t.TempDir(), Prompt: "hi", Settings: ctl})
	var r *Refusal
	if !errors.As(err, &r) || r.Status != 400 {
		t.Fatalf("agy under control's forbid: %v, want a 400 refusal", err)
	}
	// Control that has no rules to give refuses the turn with its reason.
	e, _ = newEngine(t)
	_, err = e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi",
		RulesError: &settings.Error{Setting: "permissions", Where: "the rules this turn started under", Why: "control no longer keeps them", Fix: "Start the turn again"}})
	if !errors.As(err, &r) || !strings.Contains(r.Msg, "the rules this turn started under") {
		t.Fatalf("no rules from control: %v", err)
	}
	// The turn.started names the rules the turn ran under.
	e, st := newEngine(t)
	turn, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi", Settings: ctl})
	if err != nil {
		t.Fatal(err)
	}
	turn.Ended()
	recs, err := st.After(0, store.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Kind != event.TurnStarted {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal(rec.Data, &ev); err != nil || ev.RulesVersion != "v-control" {
			t.Errorf("turn.started rules_version %q, want v-control (%v)", ev.RulesVersion, err)
		}
		return
	}
	t.Fatal("no turn.started")
}
