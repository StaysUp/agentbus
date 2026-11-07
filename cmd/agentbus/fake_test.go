package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/store"
)

type storedEvent struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Folder  string `json:"folder"`
	Hold    string `json:"hold"`
	Error   string `json:"error"`
}

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

// codexWire is the measured codex wiring (codex-cli 0.164.0).
func TestCodexWire(t *testing.T) {
	env, args := codexWire("http://127.0.0.1:8787/s/abc")
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
