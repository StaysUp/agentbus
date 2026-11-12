package run

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// sandboxEngine is an engine on policy, with homes per session when homes
// is set, whose agent is /bin/sh -c script.
func sandboxEngine(t *testing.T, policy string, homes bool, script string) (*Engine, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), settings.PolicyFile)
	writePerm(t, path, policy)
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: settings.NewFiles(filepath.Dir(path)),
		Wire: func(agent, baseURL string, _ Access) (Launch, error) {
			return Launch{Path: "/bin/sh", Args: []string{"-c", script}}, nil
		}}
	if homes {
		cfg.Homes = filepath.Join(t.TempDir(), "homes")
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.StopAll(); st.Close() })
	return e, st
}

// [sandbox] mode = "on" where the wall cannot be built refuses the turn
// with why, before anything is stored: a runner with no home per session
// (agentbus up), and a grant that is not there.
func TestSandboxRefusedBeforeStart(t *testing.T) {
	for _, c := range []struct {
		name, policy string
		homes        bool
		want         string
	}{
		{"no homes", "[sandbox]\nmode = \"on\"\n", false, "no home of their own"},
		{"missing grant", "[sandbox]\nmode = \"on\"\nread = [\"~/.agentbus-sandbox-test-not-there\"]\n", true, "not on this runner"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, st := sandboxEngine(t, c.policy, c.homes, "exit 0")
			_, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
			var r *Refusal
			if !errors.As(err, &r) || r.Status != 400 || !strings.Contains(r.Msg, c.want) || !strings.Contains(r.Msg, "so the turn did not start") {
				t.Fatalf("Start: %v, want a 400 refusal saying %q", err, c.want)
			}
			if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
				t.Errorf("%d sessions stored, want none", len(snap.Sessions))
			}
		})
	}
}

// A turn in the wall tells its harness so, and codex keeps its own sandbox
// off there.
func TestSandboxTurnSpec(t *testing.T) {
	p, err := settings.ParsePolicy([]byte("[sandbox]\nmode = \"on\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !turnSpec("", "", "", Writes{}, p, "hi").Walled {
		t.Error("Walled is false with [sandbox] on")
	}
	if turnSpec("", "", "", Writes{}, settings.DefaultPolicy(), "hi").Walled {
		t.Error("Walled is true by default")
	}
}

// The wall's network: the allow list when contain is on, the public
// internet when it is off, and for an offline turn (codex that may not
// write) its gateway and the API only, whatever contain says (review of
// 2026-11-11, round 5).
func TestWallNet(t *testing.T) {
	in := netjail.Spec{Forward: []string{"127.0.0.1:1"}, Pass: []string{"api.test:443"}, Allow: []string{"github.com"}}
	on, _ := settings.ParsePolicy([]byte("[network]\ncontain = \"on\"\n"))
	off := settings.DefaultPolicy()
	if got := wallNet(on, false, in); got.Open || len(got.Allow) != 1 {
		t.Errorf("contain on: %+v", got)
	}
	if got := wallNet(off, false, in); !got.Open {
		t.Errorf("contain off: %+v", got)
	}
	for _, p := range []settings.Policy{on, off} {
		got := wallNet(p, true, in)
		if got.Open || len(got.Allow) != 0 || len(got.Forward) != 1 || len(got.Pass) != 1 {
			t.Errorf("offline, contain %s: %+v", p.Network.Contain, got)
		}
	}
}

// agentbus-api is readable in the wall as one file: never the folder that
// holds the agentbus binary (review of 2026-11-12, round 8).
func TestSandboxAPIProgramIsOneFile(t *testing.T) {
	e := &Engine{cfg: Config{APIDir: "/data/bin"}}
	if got := e.apiProgram(); len(got) != 1 || got[0] != filepath.Join("/data/bin", APIClient) {
		t.Errorf("apiProgram = %v, want the link alone", got)
	}
}
