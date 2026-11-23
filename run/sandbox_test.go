package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A home_files entry that is not on this runner refuses the turn before
// anything is stored, and the refusal points to Home files in Settings.
func TestHomeFilesRefusedBeforeStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e, st := sandboxEngine(t, "home_files = [\"~/.agentbus-home-files-test-not-there\"]\n", true, "exit 0")
	_, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
	var r *Refusal
	if !errors.As(err, &r) || r.Status != 400 || !strings.Contains(r.Msg, "not on this runner") ||
		!strings.Contains(r.Msg, "so the turn did not start") || !strings.Contains(r.Msg, "Home files in the cockpit's Settings") {
		t.Fatalf("Start: %v, want a 400 refusal pointing to Home files", err)
	}
	if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
		t.Errorf("%d sessions stored, want none", len(snap.Sessions))
	}
}

// A path outside ~ never reaches a turn: the policy refuses it.
func TestHomeFilesOutsideHomeRefused(t *testing.T) {
	for _, policy := range []string{"home_files = [\"/etc/passwd\"]\n", "home_files = [\"~/../x\"]\n"} {
		if _, err := settings.ParsePolicy([]byte(policy)); err == nil || !strings.Contains(err.Error(), "home_files") {
			t.Errorf("%q: %v, want a refusal naming home_files", policy, err)
		}
	}
	if err := sandboxCopy(t, []string{"/etc/passwd"}); err == "" || !strings.Contains(err, "not a path under ~") {
		t.Errorf("copy of /etc/passwd: %q", err)
	}
}

// sandboxCopy is copyHomeFiles for a policy with grants, on a session that
// has a home, even if the policy's own check would have refused them.
func sandboxCopy(t *testing.T, grants []string) string {
	t.Helper()
	e := &Engine{cfg: Config{Homes: t.TempDir(), Logf: func(string, ...any) {}}}
	if err := os.MkdirAll(filepath.Join(e.cfg.Homes, "s1"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := settings.DefaultPolicy()
	p.HomeFiles = grants
	return e.copyHomeFiles(p, "s1")
}

// A session with no home of its own gets no home_files, and is not refused:
// they would land in this user's own home.
func TestHomeFilesNoHome(t *testing.T) {
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	p := settings.DefaultPolicy()
	p.HomeFiles = []string{"~/.agentbus-home-files-test-not-there"}
	for _, e := range []*Engine{{cfg: Config{Logf: logf}}, {cfg: Config{Homes: t.TempDir(), Logf: logf}}} {
		if why := e.copyHomeFiles(p, "s1"); why != "" {
			t.Errorf("no home: %q", why)
		}
	}
	if len(logged) != 2 {
		t.Errorf("logged %q, want two left_out lines", logged)
	}
}

// agentbus-api is readable as one file: never the folder that holds the
// agentbus binary (review of 2026-11-12, round 8).
func TestSandboxAPIProgramIsOneFile(t *testing.T) {
	e := &Engine{cfg: Config{APIDir: "/data/bin"}}
	if got := e.apiProgram(); len(got) != 1 || got[0] != filepath.Join("/data/bin", APIClient) {
		t.Errorf("apiProgram = %v, want the link alone", got)
	}
}
