//go:build integration

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/settings"
)

// keepDefaults makes home a data folder from before new folders started
// blank: its settings database holds no agent rows, so it has every default
// agent (claude, codex, agy) until the person changes or removes one.
func keepDefaults(t *testing.T, home string) {
	t.Helper()
	db, _, err := settings.Upgrade{Dir: home, Lock: filepath.Join(home, "events.db") + ".lock", Agents: agents.ImportRows}.Run()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

// startersService is up (or mode) on home with only the harnesses in found
// on its PATH, each the fake agent.
func startersService(t *testing.T, mode, home string, found ...string) *service {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	look := func(name string) (string, error) {
		if slices.Contains(found, name) {
			return self, nil
		}
		return "", errors.New("not found")
	}
	connectFake(t, home)
	svc, err := start(config{mode: mode, listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, lookPath: look, noReplay: true,
		heartbeat: 100 * time.Millisecond, turnWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func agentIDs(t *testing.T, svc *service, tok string) []string {
	t.Helper()
	code, out := sendAPI(t, svc, tok, "GET", "/api/v0/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("list agents: %d %v", code, out)
	}
	ids := []string{}
	list, _ := out["agents"].([]any)
	for _, a := range list {
		ids = append(ids, a.(map[string]any)["id"].(string))
	}
	return ids
}

// A new data folder of up starts with no agents, whatever its machine has
// (his call, 2026-11-13): the person adds each one. The folder keeps the
// agents the person added on the next start, and a turn that names a
// harness it started without still runs that harness on its own
// subscription, as an old client sends it.
func TestUpStartsWithNoAgents(t *testing.T) {
	home := t.TempDir()
	svc := startersService(t, modeUp, home, "claude", "codex", "agy")
	tok := readToken(t, home)
	if got := agentIDs(t, svc, tok); len(got) != 0 {
		t.Fatalf("agents of a new up folder with every harness found: %v, want none", got)
	}
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "Claude Code", "harness": "claude",
		"access": "subscription"})
	if code != http.StatusCreated {
		t.Fatalf("add an agent: %d %v", code, ag)
	}
	id := ag["id"].(string)
	svc.stop()

	svc = startersService(t, modeUp, home, "claude", "codex", "agy")
	t.Cleanup(svc.stop)
	if got := agentIDs(t, svc, tok); !slices.Equal(got, []string{id}) {
		t.Fatalf("agents of the same folder on the next start: %v, want [%s]", got, id)
	}
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "codex", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("a turn naming codex: %d %s", code, raw)
	}
	if c := waitClosed(t, svc.store, out["session"].(string)); c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("session.closed %+v, want exit_code 0", c)
	}
	job := waitEnded(t, svc, out["turn"].(string))
	if job.Spec.Harness != "codex" || job.Spec.Access != "subscription" {
		t.Fatalf("the turn ran on %q with %q, want codex on its subscription", job.Spec.Harness, job.Spec.Access)
	}
}

// A new data folder of server starts with no agents, whatever its machine
// has: it runs no turns itself.
func TestServerStartsWithNoAgents(t *testing.T) {
	home := t.TempDir()
	svc := startersService(t, modeServer, home, "claude", "codex", "agy")
	t.Cleanup(svc.stop)
	if got := agentIDs(t, svc, readToken(t, home)); len(got) != 0 {
		t.Fatalf("agents of a new server folder: %v, want none", got)
	}
}
