//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// newestStarted is the turn.started of the session's newest turn.
func newestStarted(t *testing.T, st *store.Store, session string) map[string]any {
	t.Helper()
	recs, err := st.After(0, store.Filter{Session: session, Only: true}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var last map[string]any
	for _, rec := range recs {
		var e map[string]any
		json.Unmarshal(rec.Data, &e)
		if e["kind"] == "turn.started" {
			last = e
		}
	}
	return last
}

// Every message of a session may go to another saved agent, in the
// session's folder. On the same harness the turn continues the session (by
// resume here, replay being off), with the new agent's access; on another
// harness it starts fresh with the earlier turns as text in front of the
// person's words, and its turn.started keeps both apart. A turn that names
// no agent stays on the latest one. A resume id of another harness is never
// used: that turn gets the transcript too. The record names each turn's agent,
// and the session keeps its first.
func TestSwitchAgentPerMessage(t *testing.T) {
	t.Setenv("AB_TEST_OR_KEY", "test-key-switch")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"forty-two"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer up.Close()
	home := t.TempDir()
	keepDefaults(t, home) // turns name the default agents claude and codex
	svc := openRouterService(t, home, up.URL+"/api")
	tok := readToken(t, home)
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	ds := ag["id"].(string)
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","session_id":"sess-1"}`+"\n")
	folder := t.TempDir()

	// Turn 1: Claude Code on OpenRouter, one model call.
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": ds, "folder": folder, "isolate": false, "prompt": "first question"})
	if code != http.StatusAccepted {
		t.Fatalf("turn 1: %d %s", code, raw)
	}
	session := out["session"].(string)
	waitClosedN(t, svc, session, 1)
	t.Setenv("AGENTBUS_FAKE_CALL", "")

	next := func(n int, body map[string]any) (fakeReport, map[string]any) {
		t.Helper()
		os.Remove(report)
		if code, _, raw := postMessage(t, svc, tok, session, body); code != http.StatusAccepted {
			t.Fatalf("turn %d: %d %s", n, code, raw)
		}
		waitClosedN(t, svc, session, n)
		return readReport(t, report), newestStarted(t, svc.store, session)
	}
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }

	// Turn 2: the same harness on its subscription. It resumes the session,
	// with no OpenRouter key.
	r, ts := next(2, map[string]any{"agent": "claude", "prompt": "second"})
	args := strings.Join(r.Args, " ")
	if !strings.Contains(args, "--resume sess-1") || !strings.HasSuffix(args, "-- second") || real(r.Cwd) != real(folder) {
		t.Errorf("turn 2 args %q in %s", args, r.Cwd)
	}
	if v, ok := envValue(r.Env, "ANTHROPIC_AUTH_TOKEN"); ok && v != "" {
		t.Error("turn 2 on the subscription still got the OpenRouter key")
	}
	if ts["agent"] != "claude" || ts["agent_id"] != "claude" || ts["continuity"] != "resume" || ts["context"] != nil {
		t.Errorf("turn 2 started %v", ts)
	}

	// Turn 3: another harness. A fresh codex, the earlier turns as text.
	r, ts = next(3, map[string]any{"agent": "codex", "prompt": "third"})
	args = strings.Join(r.Args, " ")
	last := r.Args[len(r.Args)-1]
	if !strings.Contains(args, "exec") || strings.Contains(args, "resume") || real(r.Cwd) != real(folder) {
		t.Errorf("turn 3 args %q in %s", args, r.Cwd)
	}
	for _, s := range []string{"[AgentBus: this conversation started before you joined it.", "the person to DeepSeek (model claude-x)",
		"first question", "forty-two", "--- Turn 2, the person to Claude Code ---\nsecond"} {
		if !strings.Contains(last, s) {
			t.Errorf("turn 3's prompt lacks %q:\n%s", s, last)
		}
	}
	if !strings.HasSuffix(last, "follows.]\n\nthird") {
		t.Errorf("turn 3's prompt does not end with the person's words: %q", last[max(0, len(last)-80):])
	}
	if ts["agent"] != "codex" || ts["agent_id"] != "codex" || ts["continuity"] != "transcript" || ts["text"] != "third" ||
		ts["context"] != strings.TrimSuffix(last, "third") {
		t.Errorf("turn 3 started %v", ts)
	}

	// Turn 4: no agent named, so codex again. The only resume id is
	// claude's, so it gets the transcript too.
	r, ts = next(4, map[string]any{"prompt": "fourth"})
	args = strings.Join(r.Args, " ")
	if !strings.Contains(args, "exec") || strings.Contains(args, "resume") || !strings.Contains(r.Args[len(r.Args)-1], "--- Turn 3, the person to Codex ---\nthird") {
		t.Errorf("turn 4 args %q", args)
	}
	if ts["agent"] != "codex" || ts["continuity"] != "transcript" {
		t.Errorf("turn 4 started %v", ts)
	}

	// The model goes with the agent: a model sent is that turn's only, and a
	// turn that sends none runs with its named agent's model as it is now
	// (his rule, 2026-11-12), here none, so the harness's default.
	r, _ = next(5, map[string]any{"agent": "claude", "model": "haiku", "prompt": "fifth"})
	if !strings.Contains(strings.Join(r.Args, " "), "--model haiku") {
		t.Errorf("turn 5 args %q", r.Args)
	}
	r, ts = next(6, map[string]any{"prompt": "sixth"})
	if strings.Contains(strings.Join(r.Args, " "), "--model") || ts["model"] != nil {
		t.Errorf("turn 6 args %q, started %v, want the agent's own default", r.Args, ts)
	}
	r, _ = next(7, map[string]any{"agent": "claude", "prompt": "seventh"})
	if strings.Contains(strings.Join(r.Args, " "), "--model") {
		t.Errorf("turn 7 args %q, want the agent's own default", r.Args)
	}
	// A switch to an agent with no model runs on the harness's default.
	r, _ = next(8, map[string]any{"agent": "codex", "prompt": "eighth"})
	if strings.Contains(strings.Join(r.Args, " "), "--model") {
		t.Errorf("turn 8 args %q, want the default model", r.Args)
	}

	// The record: each turn its agent; the session its first and latest.
	var list struct {
		Turns []struct {
			AgentID string `json:"agent_id"`
			Agent   string `json:"agent"`
		} `json:"turns"`
	}
	if code := callAPI(t, svc, tok, "GET", "/api/v0/turns?session="+session, nil, &list); code != http.StatusOK || len(list.Turns) != 8 ||
		list.Turns[0].AgentID != "codex" || list.Turns[4].AgentID != "codex" || list.Turns[6].AgentID != "claude" || list.Turns[7].AgentID != ds {
		t.Errorf("turns %d %+v", code, list.Turns)
	}
	if s := snapSession(t, svc, tok, session); s["agent"] != "codex" || s["first_agent"] != "claude" {
		t.Errorf("session agent %v first %v", s["agent"], s["first_agent"])
	}
}
