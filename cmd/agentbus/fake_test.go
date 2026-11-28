package main

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/store"
)

type storedEvent struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Folder  string `json:"folder"`
	Hold    string `json:"hold"`
	Error   string `json:"error"`
	Call    string `json:"call"`
	Status  int    `json:"status"`
}

// storedEvents are the store's events of sessions, and its drops. Control's
// own events, in no session (such as runner.enrolled), are left out.
func storedEvents(t *testing.T, st *store.Store) []storedEvent {
	t.Helper()
	recs, err := st.After(0, store.Filter{}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []storedEvent
	for _, r := range recs {
		var e storedEvent
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatal(err)
		}
		if e.Session == "" && e.Kind != "events.dropped" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// waitFor polls cond every 10 ms until it holds, and fails the test once
// waitDeadline(5 s) has passed without it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(waitDeadline(5 * time.Second)); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ciWaitScale is how many times longer a test waits for something to happen
// on CI. CI machines are shared and slower than a laptop: a delivered turn
// that takes a second here took over 5 s on macOS CI (2026-11-27). It scales
// how long a test waits before it gives up, never a bound a test checks.
const ciWaitScale = 4

// waitDeadline is d, or d times ciWaitScale on CI (CI is set, as GitHub
// Actions sets it).
func waitDeadline(d time.Duration) time.Duration {
	if os.Getenv("CI") != "" {
		return d * ciWaitScale
	}
	return d
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// codex's wire is the measured codex wiring (codex-cli 0.164.0).
func TestCodexWire(t *testing.T) {
	h, _ := harness.Get("codex")
	env, args := h.Wire("http://127.0.0.1:8787/s/abc")
	if env != nil || strings.Join(args, " ") != `-c openai_base_url="http://127.0.0.1:8787/s/abc/backend-api/codex"` {
		t.Fatalf("env %v, args %v", env, args)
	}
}

func TestChildEnvReplacesOnlyNamed(t *testing.T) {
	got := childEnv([]string{"A=1", "ANTHROPIC_BASE_URL=old", "B=2"}, []string{"ANTHROPIC_BASE_URL=new"})
	if strings.Join(got, " ") != "A=1 B=2 ANTHROPIC_BASE_URL=new" {
		t.Fatalf("%v", got)
	}
}
