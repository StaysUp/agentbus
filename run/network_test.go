package run

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

func TestReach(t *testing.T) {
	fwd, pass := reach("http://127.0.0.1:8790", "http://127.0.0.1:8790", "http://localhost:9000/x",
		"http://host.lima.internal:8797", "https://control.example", "http://[::1]:7")
	if want := []string{"127.0.0.1:8790", "127.0.0.1:9000", "[::1]:7"}; !slices.Equal(fwd, want) {
		t.Errorf("forward %v, want %v", fwd, want)
	}
	if want := []string{"host.lima.internal:8797", "control.example:443"}; !slices.Equal(pass, want) {
		t.Errorf("pass %v, want %v", pass, want)
	}
}

func TestSetEnv(t *testing.T) {
	got := setEnv([]string{"A=1", "HTTPS_PROXY=old", "B=2", "https_proxy=old"}, []string{"HTTPS_PROXY=new", "https_proxy=new"})
	if want := []string{"A=1", "B=2", "HTTPS_PROXY=new", "https_proxy=new"}; !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
}

// A policy that leaves contain out runs the agent open, on every machine:
// "off" is the default for a runner too.
func TestContainMissingRunsOpen(t *testing.T) {
	p, err := settings.ParsePolicy([]byte("[network]\nallow = [\"github.com\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if on, why := (&Engine{}).contained(p); on || why != "" {
		t.Fatalf("contained = %v %q, want open", on, why)
	}
}

// contain = "on" on a machine that cannot close an agent off refuses the
// turn with why, before anything is stored. Where it can, the turn runs.
func TestContainOnRefusedWhereItCannot(t *testing.T) {
	path := filepath.Join(t.TempDir(), settings.PolicyFile)
	writePerm(t, path, "[network]\ncontain = \"on\"\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: settings.NewFiles(filepath.Dir(path)),
		Wire: func(agent, baseURL string, _ Access) (Launch, error) {
			return Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
	cannot := !netjail.Supported || netjail.Check() != nil
	if !cannot {
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-turn.Done()
		return
	}
	var r *Refusal
	if !errors.As(err, &r) || r.Status != 400 || !strings.Contains(r.Msg, "so the turn did not start") {
		t.Fatalf("Start: %v, want a 400 refusal", err)
	}
	if !netjail.Supported && !strings.Contains(r.Msg, "Linux-only") {
		t.Errorf("refusal: %s", r.Msg)
	}
	if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
		t.Errorf("%d sessions stored, want none", len(snap.Sessions))
	}
}

// A setting the harness cannot keep refuses the turn with why, before
// anything is stored: agy with forbid.
func TestHarnessRefusesSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), settings.PolicyFile)
	writePerm(t, path, "forbid = [\"git push\"]\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: settings.NewFiles(filepath.Dir(path)),
		Wire: func(agent, baseURL string, _ Access) (Launch, error) {
			return Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Start(Request{Harness: "agy", Folder: t.TempDir(), Prompt: "hi"})
	var r *Refusal
	if !errors.As(err, &r) || r.Status != 400 || !strings.Contains(r.Msg, "agy") {
		t.Fatalf("Start: %v, want a 400 refusal", err)
	}
	if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
		t.Errorf("%d sessions stored, want none", len(snap.Sessions))
	}
}

// agy cannot turn its own subagents off. With subagents off, the default
// from 0.5.2, its turn runs, and its turn.started says plainly that the
// setting was not kept. claude keeps it, and records nothing.
func TestHarnessUnkeptSettingRecorded(t *testing.T) {
	for _, h := range []string{"agy", "claude"} {
		st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: settings.NewFiles(t.TempDir()),
			Wire: func(agent, baseURL string, _ Access) (Launch, error) {
				return Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		turn, err := e.Start(Request{Harness: h, Folder: t.TempDir(), Prompt: "hi"})
		if err != nil {
			t.Fatalf("%s: Start: %v", h, err)
		}
		turn.Ended()
		recs, err := st.After(0, store.Filter{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		var unkept []string
		found := false
		for _, rec := range recs {
			if rec.Kind != event.TurnStarted {
				continue
			}
			var ev event.Event
			if err := json.Unmarshal(rec.Data, &ev); err != nil {
				t.Fatal(err)
			}
			found, unkept = true, ev.Unkept
		}
		want := []string(nil)
		if h == "agy" {
			want = []string{harness.AgySubagentsUnkept}
		}
		if !found || !slices.Equal(unkept, want) {
			t.Errorf("%s: turn.started found %v, unkept %q, want %q", h, found, unkept, want)
		}
	}
}
