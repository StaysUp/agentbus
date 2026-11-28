package main

import (
	"encoding/json"
	"net/url"
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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// fakeEnv are the variables the fake agent reads (fake_integration_test.go).
// A runner runs each session in a home of its own, which keeps only the
// variables it is told to (-pass-env), so a test's runner passes these on.
var fakeEnv = []string{
	"AGENTBUS_FAKE_AGENT", "AGENTBUS_FAKE_AGY_BODY", "AGENTBUS_FAKE_AGY_MODELS", "AGENTBUS_FAKE_APP_OUT",
	"AGENTBUS_FAKE_APP_SERVER", "AGENTBUS_FAKE_ASK", "AGENTBUS_FAKE_ASKED", "AGENTBUS_FAKE_AUTH_EXIT",
	"AGENTBUS_FAKE_BODY", "AGENTBUS_FAKE_CALL", "AGENTBUS_FAKE_CODEX_BODY", "AGENTBUS_FAKE_EXIT",
	"AGENTBUS_FAKE_HOME", "AGENTBUS_FAKE_IGNORE_TERM", "AGENTBUS_FAKE_LOGIN_EXIT", "AGENTBUS_FAKE_MARK",
	"AGENTBUS_FAKE_MCP", "AGENTBUS_FAKE_MCP_OUT", "AGENTBUS_FAKE_MODELS_CACHE", "AGENTBUS_FAKE_NET",
	"AGENTBUS_FAKE_OPENCODE_BODY", "AGENTBUS_FAKE_OTHER", "AGENTBUS_FAKE_OUT", "AGENTBUS_FAKE_POST",
	"AGENTBUS_FAKE_POST_OUT", "AGENTBUS_FAKE_STDERR", "AGENTBUS_FAKE_STDERR_REPEAT", "AGENTBUS_FAKE_STDOUT",
	"AGENTBUS_FAKE_THREAD", "AGENTBUS_FAKE_TOOL", "AGENTBUS_FAKE_TOOL_SETSID", "AGENTBUS_FAKE_VERSION",
	"AGENTBUS_FAKE_BODIES", "AGENTBUS_FAKE_CALL_AFTER", "AGENTBUS_FAKE_CALL_OUT", "AGENTBUS_FAKE_MCP_PROMPT",
	"AGENTBUS_FAKE_TERM_MARK", "AGENTBUS_FAKE_WAIT", "AGENTBUS_FAKE_WAIT_S", "AGENTBUS_FAKE_WRITE",
}
