package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func postAPI(t *testing.T, svc *service, tok, path string, body []byte) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+svc.addr+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), tok) {
		t.Fatal("the token appeared in a reply")
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out, string(b)
}

func postRun(t *testing.T, svc *service, tok string, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	return postAPI(t, svc, tok, "/api/v0/turns", b)
}

// An agent whose program is not on the service's PATH is refused with 400, a
// reply that names the agent and how to install it, a line in service.log,
// and nothing stored. Neither the reply nor the log quotes PATH.
func TestRunMissingAgentIsRefused(t *testing.T) {
	const mark = "/Users/PATH-MARK/bin"
	t.Setenv("PATH", mark+":"+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	for _, tc := range []struct{ agent, product string }{
		{"claude", "Install Claude Code and run claude once to sign in"},
		{"codex", "Install the Codex CLI and run codex once to sign in"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			connectFake(t, home)
			svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL(upstream),
				chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, getenv: os.Getenv,
				lookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(svc.stop)
			code, out, raw := postRun(t, svc, readToken(t, home), map[string]any{"agent": tc.agent, "folder": t.TempDir(), "prompt": "hi"})
			msg, _ := out["error"].(string)
			if code != http.StatusBadRequest ||
				!strings.HasPrefix(msg, "agent "+tc.agent+" cannot start: the program "+tc.agent+" is not on the service's PATH.") ||
				!strings.Contains(msg, tc.product) || strings.Contains(raw, "PATH-MARK") {
				t.Fatalf("%d %s, want 400 naming the agent, its program and how to install it", code, raw)
			}
			if evs := storedEvents(t, svc.store); len(evs) != 0 {
				t.Fatalf("events %+v, want none stored", evs)
			}
			log := waitLog(t, home, "run.refused agent="+tc.agent+" program="+tc.agent+" reason=not_on_path")
			if strings.Contains(log, "PATH-MARK") {
				t.Fatalf("service.log quotes PATH:\n%s", log)
			}
		})
	}
}

// A run whose child cannot start (an executable whose interpreter is missing
// passes lookup but fails to start) left its session registered with no
// closing event. session.opened is written before the child starts, so it
// must be closed, with the reason.
func TestRunThatCannotStartIsClosed(t *testing.T) {
	home := t.TempDir()
	bad := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bad, []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	connectFake(t, home)
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL(upstream),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, getenv: os.Getenv,
		lookPath: func(string) (string, error) { return bad, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)
	code, _, _ := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code < 500 {
		t.Fatalf("status %d, want a server error for a child that cannot start", code)
	}
	evs := storedEvents(t, svc.store)
	if len(evs) != 4 || evs[0].Kind != "session.opened" || evs[1].Kind != "turn.started" || evs[2].Kind != "context.added" ||
		evs[3].Kind != "session.closed" || evs[0].Hold == "" || evs[3].Hold != evs[0].Hold || evs[3].Error == "" {
		t.Fatalf("events %+v, want session.opened, turn.started, context.added, then its session.closed with the reason", evs)
	}
	if snap, err := svc.store.Snapshot(); err != nil || len(snap.Sessions) != 1 || snap.Sessions[0].Open {
		t.Fatalf("snapshot %+v, %v; want one closed session", snap.Sessions, err)
	}
}
