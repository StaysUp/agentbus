//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServiceLogHasAPIErrorsAndNoTokenQueryOrBody(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	const prompt = "PROMPT-NEVER-IN-SERVICE-LOG"

	code, _, _ := postRun(t, svc, tok, map[string]any{"agent": "nope", "folder": t.TempDir(), "prompt": prompt})
	if code != http.StatusBadRequest {
		t.Fatalf("bad agent: %d", code)
	}
	if code, _ := getAPI(t, svc, tok, "/api/v0/sessions/abc/events?store=st_QUERYMARK&after=0"); code != http.StatusConflict {
		t.Fatalf("wrong store: %d", code)
	}
	if code := rawGet(t, svc.addr, "/api/v0/sessions", "Bearer wrong-token-mark"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code := rawGet(t, svc.addr, "/nowhere", ""); code != http.StatusNotFound {
		t.Fatalf("unrouted path: %d", code)
	}

	s := waitLog(t, home,
		`request method=POST path=/api/v0/turns status=400 `,
		`is not a saved agent; GET /api/v0/agents lists them"`,
		`request method=GET path=/api/v0/sessions/abc/events status=409 `,
		`error="the cursor belongs to another store; start over from the snapshot"`,
		`request method=GET path=/api/v0/sessions status=401 `,
		`request method=GET path=/nowhere status=404 `,
		`error="agentbus: no vendor is routed for this path"`,
		"service.start ")
	for _, never := range []string{tok, "wrong-token-mark", "Bearer", "Authorization", prompt, "QUERYMARK", "store=", `"folder"`} {
		if strings.Contains(s, never) {
			t.Errorf("service.log holds %q:\n%s", never, s)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if !strings.HasPrefix(line, "20") {
			t.Errorf("a line without its time: %q", line)
		}
	}
	wantMode(t, filepath.Join(home, "logs"), 0o700)
	wantMode(t, filepath.Join(home, "logs", "runs"), 0o700)
	wantMode(t, filepath.Join(home, "logs", "service.log"), 0o600)
}

func TestRunKeepsChildOutputInItsOwnLogAndItsExitInServiceLog(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	const prompt = "PROMPT-MARK-7f3a"
	const failure = "Error: not signed in; run claude login\n"
	t.Setenv("AGENTBUS_FAKE_STDERR", failure)
	t.Setenv("AGENTBUS_FAKE_EXIT", "1")

	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": prompt})
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %s", code, raw)
	}
	id := out["session"].(string)
	waitClosed(t, svc.store, id)

	runLog := filepath.Join(home, "logs", "runs", id+".log")
	got := readFile(t, runLog)
	if !strings.Contains(got, failure) {
		t.Errorf("the run log lacks the child's stderr:\n%s", got)
	}
	header, _, _ := strings.Cut(got, "\n")
	if !strings.HasPrefix(header, "=== 20") || !strings.Contains(header, " agent=claude exit=1 ") ||
		!strings.Contains(header, `"-p" "--output-format" "json"`) || !strings.HasSuffix(header, " <prompt>]") {
		t.Errorf("header: %q", header)
	}
	if strings.Contains(header, prompt) {
		t.Errorf("the header holds the prompt: %q", header)
	}
	wantMode(t, runLog, 0o600)

	s := waitLog(t, home, "run.start session="+id+" agent=claude ", "run.exit session="+id+" agent=claude ", " exit=1 ")
	for _, never := range []string{prompt, "not signed in", tok} {
		if strings.Contains(s, never) {
			t.Errorf("service.log holds %q:\n%s", never, s)
		}
	}
	// The output stays out of the events too, but for the last stderr
	// lines of an agent that failed, on its session.closed (error_tail).
	recs, err := svc.store.After(0, store.Filter{}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	tails := 0
	for _, r := range recs {
		if strings.Contains(string(r.Data), tok) {
			t.Errorf("an event holds the token: %s", r.Data)
		}
		if !strings.Contains(string(r.Data), "not signed in") {
			continue
		}
		var e event.Event
		if json.Unmarshal(r.Data, &e) != nil || e.Kind != event.SessionClosed || !strings.Contains(e.ErrorTail, "not signed in") ||
			strings.Contains(strings.Replace(string(r.Data), e.ErrorTail, "", 1), "not signed in") {
			t.Errorf("an event holds the child's stderr outside session.closed's error_tail: %s", r.Data)
		}
		tails++
	}
	if tails != 1 {
		t.Errorf("%d events hold the failed agent's last stderr lines, want its session.closed", tails)
	}

	// A second turn of the session appends under its own header.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","session_id":"sess-1"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	t.Setenv("AGENTBUS_FAKE_STDERR", "second turn\n")
	code, out, raw = postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "x"})
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %s", code, raw)
	}
	id2 := out["session"].(string)
	waitClosed(t, svc.store, id2)
	if code, _, raw := postMessage(t, svc, tok, id2, map[string]any{"prompt": "MESSAGE-MARK"}); code != http.StatusAccepted {
		t.Fatalf("message: %d %s", code, raw)
	}
	waitClosedN(t, svc, id2, 2)
	got2 := readFile(t, filepath.Join(home, "logs", "runs", id2+".log"))
	if n := strings.Count(got2, "=== "); n != 2 {
		t.Errorf("two turns, %d headers:\n%s", n, got2)
	}
	if !strings.Contains(got2, `"--resume" "sess-1"`) {
		t.Errorf("the second header lacks its resume id:\n%s", got2)
	}
	waitLog(t, home, "message.exit session="+id2+" ")
	if s := readFile(t, filepath.Join(home, "logs", "service.log")); strings.Contains(s, "MESSAGE-MARK") {
		t.Errorf("service.log holds a message's prompt:\n%s", s)
	}
}

func TestRunLogKeepsOnlyTheLastMiB(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_STDERR", strings.Repeat("x", 1023)+"\n")
	t.Setenv("AGENTBUS_FAKE_STDERR_REPEAT", "1600") // 1.6 MiB
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "p"})
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %s", code, raw)
	}
	id := out["session"].(string)
	waitClosed(t, svc.store, id)
	fi, err := os.Stat(filepath.Join(home, "logs", "runs", id+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1<<20 || fi.Size() < 1<<19 {
		t.Errorf("run log is %d bytes, want at most 1 MiB and most of it kept", fi.Size())
	}
}
