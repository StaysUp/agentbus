package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	return postTurn(t, svc, tok, firstTurnBody(t, svc, tok, body))
}

// postTurn posts a turn body and answers the turn's outcome. POST
// /api/v0/turns waits only turnWait for a runner to take the turn, then
// answers 202 queued, with no ahead and no base_url (the Started contract).
// Under load the built-in runner can take longer than a test service's 2s,
// and that answer says nothing yet: not whether the turn runs, nor the
// runner's refusal. So the body carries an idempotency_key (a fresh one
// unless it names its own), and postTurn asks again with it, which answers
// for the same turn and starts no second one, until the reply is anything
// else. A turn queued behind its session's earlier turns (ahead) is an
// answer. A test of the runner-wait answer itself posts with postAPI.
func postTurn(t *testing.T, svc *service, tok string, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	with := map[string]any{"idempotency_key": testKey(t)}
	for k, v := range body {
		with[k] = v
	}
	b, _ := json.Marshal(with)
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", b)
		if code != http.StatusAccepted || out["state"] != "queued" || out["ahead"] != nil || time.Now().After(deadline) {
			return code, out, raw
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testKey is a fresh idempotency_key.
func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "test-" + hex.EncodeToString(b)
}

// firstTurnBody turns a one-call turn body (a folder, no session) into a turn
// body naming a fresh session, running the 3-call flow (POST /workspaces, POST
// /sessions) first. The folder and the sandbox mode (from isolate, default
// true) become the workspace's; the retired read_only is dropped. A body that
// already names a session is a continuation and is left unchanged.
func firstTurnBody(t *testing.T, svc *service, tok string, body map[string]any) map[string]any {
	t.Helper()
	folder, hasFolder := body["folder"].(string)
	if _, hasSession := body["session"]; hasSession || !hasFolder {
		return body
	}
	sandbox := true
	if v, ok := body["isolate"].(bool); ok {
		sandbox = v
	}
	sess := newWorkspaceSession(t, svc, tok, folder, sandbox)
	nb := map[string]any{"session": sess}
	for k, v := range body {
		switch k {
		case "folder", "folder_ref", "machine", "connector", "isolate", "read_only":
			// Workspace fields, or retired turn fields: not a turn's.
		default:
			nb[k] = v
		}
	}
	return nb
}

// newWorkspaceSession creates a workspace in folder (sandbox or direct) and a
// session in it, returning the session id. A first turn now names a session;
// its folder and mode are the workspace's, set by the 3-call flow (POST
// /api/v0/workspaces, POST /api/v0/sessions, then POST /api/v0/turns). A test
// that runs several turns reuses the one session id for each.
func newWorkspaceSession(t *testing.T, svc *service, tok, folder string, sandbox bool) string {
	t.Helper()
	return newSessionOf(t, svc, tok, map[string]any{"folder": folder, "sandbox": sandbox})
}

// newWorkspaceOf creates a workspace from wsBody (folder, or folder_ref +
// connector, plus sandbox) and returns its id, for a test whose workspace is
// not a plain local folder.
func newWorkspaceOf(t *testing.T, svc *service, tok string, wsBody map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(wsBody)
	code, out, raw := postAPI(t, svc, tok, "/api/v0/workspaces", b)
	ws, _ := out["workspace"].(string)
	if code != http.StatusCreated || ws == "" {
		t.Fatalf("POST /api/v0/workspaces %v: %d %s", wsBody, code, raw)
	}
	return ws
}

// newSessionOf creates a workspace from wsBody and a session in it, returning
// the session id.
func newSessionOf(t *testing.T, svc *service, tok string, wsBody map[string]any) string {
	t.Helper()
	ws := newWorkspaceOf(t, svc, tok, wsBody)
	b, _ := json.Marshal(map[string]any{"workspace": ws})
	code, out, raw := postAPI(t, svc, tok, "/api/v0/sessions", b)
	sess, _ := out["session"].(string)
	if code != http.StatusCreated || sess == "" {
		t.Fatalf("POST /api/v0/sessions in %s: %d %s", ws, code, raw)
	}
	return sess
}

// An agent whose program is not on the service's PATH is refused with 400, a
// reply that names the agent and how to install it, a line in service.log,
// and nothing stored in the session's record (turn.ended is control's
// reactor trigger, keyed on the turn and session-less, so it is not a session
// event). Neither the reply nor the log quotes PATH.
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
			tok := readToken(t, home)
			sess := newWorkspaceSession(t, svc, tok, t.TempDir(), true)
			code, out, raw := postRun(t, svc, tok, map[string]any{"session": sess, "agent": tc.agent, "prompt": "hi"})
			msg, _ := out["error"].(string)
			if code != http.StatusBadRequest ||
				!strings.HasPrefix(msg, "agent "+tc.agent+" cannot start: the program "+tc.agent+" is not on the service's PATH.") ||
				!strings.Contains(msg, tc.product) || strings.Contains(raw, "PATH-MARK") {
				t.Fatalf("%d %s, want 400 naming the agent, its program and how to install it", code, raw)
			}
			// The refused turn stores nothing in its session's record; only the
			// session.created of the session it names remains.
			if evs := storedEvents(t, svc.store); len(evs) != 1 || evs[0].Kind != "session.created" {
				t.Fatalf("events %+v, want only the session.created", evs)
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
// closing event. run.started is written before the child starts, so it
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
	sess := newWorkspaceSession(t, svc, tok, t.TempDir(), true)
	code, _, _ := postRun(t, svc, tok, map[string]any{"session": sess, "agent": "claude", "prompt": "hi"})
	if code < 500 {
		t.Fatalf("status %d, want a server error for a child that cannot start", code)
	}
	evs := storedEvents(t, svc.store)
	if len(evs) != 5 || evs[0].Kind != "session.created" || evs[1].Kind != "run.started" || evs[2].Kind != "turn.started" || evs[3].Kind != "context.added" ||
		evs[4].Kind != "run.ended" || evs[1].Hold == "" || evs[4].Hold != evs[1].Hold || evs[4].Error == "" {
		t.Fatalf("events %+v, want session.created, run.started, turn.started, context.added, then its run.ended with the reason", evs)
	}
	if snap, err := svc.store.Snapshot(); err != nil || len(snap.Sessions) != 1 || snap.Sessions[0].Open {
		t.Fatalf("snapshot %+v, %v; want one closed session", snap.Sessions, err)
	}
}
