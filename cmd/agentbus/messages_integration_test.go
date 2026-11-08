//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// Vendor output shapes, as the real agents print them.
const (
	// claude -p --output-format json (Claude Code 2.1.318): one result object.
	// The reply text holds a decoy id, which must not be taken.
	claudeResultA = `{"type":"result","subtype":"success","is_error":false,"result":"REPLY-TEXT-ONE {\"type\":\"result\",\"session_id\":\"decoy\"}","session_id":"11111111-2222-3333-4444-555555555555","total_cost_usd":0.01}` + "\n"
	claudeResultB = `{"type":"result","subtype":"success","is_error":false,"result":"REPLY-TEXT-TWO","session_id":"66666666-7777-8888-9999-000000000000"}` + "\n"
	// codex exec --json (codex-cli 0.164.0, 2026-11-05): thread.started first.
	codexEventsA = `{"type":"thread.started","thread_id":"01a1ac2b-e655-7e32-8970-e75cb41c20ee"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"REPLY-TEXT-CODEX"}}` + "\n" +
		`{"type":"thread.started","thread_id":"not-the-first"}` + "\n"
)

const (
	idA      = "11111111-2222-3333-4444-555555555555"
	idB      = "66666666-7777-8888-9999-000000000000"
	codexIDA = "01a1ac2b-e655-7e32-8970-e75cb41c20ee"
)

func getAPI(t *testing.T, svc *service, tok, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+svc.addr+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// postMessage starts a session's next turn: POST /api/v0/turns with session.
func postMessage(t *testing.T, svc *service, tok, session string, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	with := map[string]any{"session": session}
	for k, v := range body {
		with[k] = v
	}
	b, _ := json.Marshal(with)
	return postAPI(t, svc, tok, "/api/v0/turns", b)
}

// snapSession is one session of GET /api/v0/sessions, read over HTTP.
func snapSession(t *testing.T, svc *service, tok, id string) map[string]any {
	t.Helper()
	code, raw := getAPI(t, svc, tok, "/api/v0/sessions")
	if code != http.StatusOK {
		t.Fatalf("snapshot %d %s", code, raw)
	}
	var snap struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	for _, s := range snap.Sessions {
		if s["id"] == id {
			return s
		}
	}
	t.Fatalf("session %s not in snapshot %s", id, raw)
	return nil
}

func closedEvents(evs []runEvent) []runEvent {
	var out []runEvent
	for _, e := range evs {
		if e.Kind == "session.closed" {
			out = append(out, e)
		}
	}
	return out
}

func waitClosedN(t *testing.T, svc *service, session string, n int) []runEvent {
	t.Helper()
	var c []runEvent
	waitFor(t, "session.closed", func() bool { c = closedEvents(runEvents(t, svc.store, session)); return len(c) >= n })
	return c
}

// startRun runs agent with the fake printing stdout, waits for it to close,
// and returns the session id and base URL.
func startRun(t *testing.T, svc *service, tok, agent, stdout string) (string, string) {
	t.Helper()
	t.Setenv("AGENTBUS_FAKE_STDOUT", stdout)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": agent, "folder": t.TempDir(), "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %s", code, raw)
	}
	id := out["id"].(string)
	waitClosed(t, svc.store, id)
	return id, out["base_url"].(string)
}

func TestAgentsListsFoundVersionsAndSignIn(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("AGENTBUS_FAKE_VERSION", "2.1.318 (Claude Code)")
	t.Setenv("AGENTBUS_FAKE_AUTH_EXIT", "0")
	home := t.TempDir()
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf,
		lookPath: func(name string) (string, error) {
			if name == "claude" {
				return self, nil
			}
			return "", errors.New("not found")
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)

	code, raw := getAPI(t, svc, tok, "/api/v0/agents")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	want := `{"agents":[{"name":"claude","found":true,"version":"2.1.318","signed_in":true},{"name":"codex","found":false}]}`
	if strings.TrimSpace(raw) != want {
		t.Fatalf("got %s\nwant %s", raw, want)
	}
	if strings.Contains(raw, self) || strings.Contains(raw, "example.com") {
		t.Fatal("a path or the status output reached the reply")
	}
	// Kept for a minute: a changed version is not seen yet.
	t.Setenv("AGENTBUS_FAKE_VERSION", "codex-cli 9.9.9")
	if _, raw2 := getAPI(t, svc, tok, "/api/v0/agents"); raw2 != raw {
		t.Fatalf("not cached: %s", raw2)
	}
}

func TestRunStoresResumeIDAndNothingElseFromStdout(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	for _, tc := range []struct{ agent, stdout, want string }{
		// A noise line first, and a line of another type with its own
		// session_id after the result.
		{"claude", "warming up\n" + claudeResultA + `{"type":"assistant","session_id":"not-the-result"}` + "\n", idA},
		{"codex", codexEventsA, codexIDA},
		{"claude", "no json here\n", ""},
		// The result's id must be shaped like an id.
		{"claude", `{"type":"result","session_id":"--dangerously-skip-permissions"}` + "\n", ""},
	} {
		id, _ := startRun(t, svc, tok, tc.agent, tc.stdout)
		c := closedEvent(runEvents(t, svc.store, id))
		if c.ResumeID != tc.want {
			t.Errorf("%s: resume_id %q, want %q", tc.agent, c.ResumeID, tc.want)
		}
	}
	recs, err := svc.store.After(0, store.Filter{}, 100000)
	if err != nil || len(recs) == 0 {
		t.Fatal(err)
	}
	for _, r := range recs {
		d := string(r.Data)
		if strings.Contains(d, "REPLY-TEXT") || strings.Contains(d, "decoy") || strings.Contains(d, "warming") ||
			strings.Contains(d, "total_cost_usd") || strings.Contains(d, "not-the-") {
			t.Fatalf("stdout beyond the id was stored: %s", r.Data)
		}
	}
}

func TestRunWithSessionRunsNextTurnInSameSession(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	for _, tc := range []struct {
		agent, first, firstID string
		readOnly              any
		model                 string
		want                  string // BASE is the session's base URL, ID the first resume id
	}{
		{"claude", claudeResultA, idA, nil, "haiku",
			"-p --output-format json --append-system-prompt HINT --resume ID --model haiku -- --next"},
		{"claude", claudeResultA, idA, true, "haiku",
			"-p --output-format json --append-system-prompt HINT --resume ID --tools Read,Grep,Glob --allowedTools Read,Grep,Glob --strict-mcp-config --model haiku -- --next"},
		{"claude", claudeResultA, idA, false, "",
			"-p --output-format json --append-system-prompt HINT --resume ID -- --next"},
		{"codex", codexEventsA, codexIDA, nil, "",
			`-c openai_base_url="BASE/backend-api/codex" exec --skip-git-repo-check --sandbox workspace-write -c developer_instructions=HINT -c sandbox_workspace_write.network_access=true resume --json -- ID --next`},
		{"codex", codexEventsA, codexIDA, true, "",
			`-c openai_base_url="BASE/backend-api/codex" exec --skip-git-repo-check --sandbox read-only -c developer_instructions=HINT resume --json -- ID --next`},
		{"codex", codexEventsA, codexIDA, false, "gpt-5.5",
			`-c openai_base_url="BASE/backend-api/codex" exec --skip-git-repo-check --sandbox workspace-write -c developer_instructions=HINT -c sandbox_workspace_write.network_access=true resume --json --model gpt-5.5 -- ID --next`},
	} {
		name := tc.agent + " read_only " + strings.TrimSpace(strings.ReplaceAll(jsonOf(tc.readOnly), "null", "default"))
		id, base := startRun(t, svc, tok, tc.agent, tc.first)
		if !snapSession(t, svc, tok, id)["resumable"].(bool) {
			t.Fatalf("%s: a closed run with a resume_id is not resumable", name)
		}
		report := filepath.Join(t.TempDir(), "report")
		t.Setenv("AGENTBUS_FAKE_OUT", report)
		t.Setenv("AGENTBUS_FAKE_STDOUT", claudeResultB)
		if tc.agent == "codex" {
			t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"thread.started","thread_id":"`+idB+`"}`+"\n")
		}
		t.Setenv("AGENTBUS_FAKE_EXIT", "4")
		body := map[string]any{"prompt": "--next"}
		if tc.readOnly != nil {
			body["read_only"] = tc.readOnly
		}
		if tc.model != "" {
			body["model"] = tc.model
		}
		code, out, raw := postMessage(t, svc, tok, id, body)
		next, _ := out["turn"].(string)
		job, _ := out["job"].(string)
		session := strings.Split(base, "/t/")[0]
		if code != http.StatusAccepted || next == "" || job == "" || strings.HasSuffix(base, "/t/"+next) ||
			strings.TrimSpace(raw) != `{"base_url":"`+session+`/t/`+next+`","id":"`+id+`","turn":"`+next+`","job":"`+job+`"}` {
			t.Fatalf("%s: %d %s, want 202 with the same id, a new turn, and the session's base URL with it", name, code, raw)
		}
		base = out["base_url"].(string) // this turn's own
		closed := waitClosedN(t, svc, id, 2)
		if c := closed[1]; c.ExitCode == nil || *c.ExitCode != 4 || c.ResumeID != idB || c.Hold == "" || c.Hold == closed[0].Hold {
			t.Fatalf("%s: second session.closed %+v, want exit 4, resume_id %s, a new hold", name, c, idB)
		}
		r := readReport(t, report)
		want := strings.ReplaceAll(strings.ReplaceAll(tc.want, "BASE", base), "ID", tc.firstID)
		if got := argsOf(t, r.Args, id); got != want {
			t.Errorf("%s: args %q\nwant %q", name, got, want)
		}
		if tc.agent == "claude" {
			if u, _ := envValue(r.Env, "ANTHROPIC_BASE_URL"); u != base {
				t.Errorf("%s: base URL %q, want this turn's %q", name, u, base)
			}
		}
		if strings.Contains(strings.Join(r.Env, "\n")+strings.Join(r.Args, "\n"), tok) {
			t.Fatal("the token reached the child")
		}
		var kinds []string
		for _, e := range runEvents(t, svc.store, id) {
			kinds = append(kinds, e.Kind)
		}
		if got := strings.Join(kinds, " "); got != "session.opened turn.started session.closed session.opened turn.started session.closed" {
			t.Fatalf("%s: events %s", name, got)
		}
		t.Setenv("AGENTBUS_FAKE_OUT", "")
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestRunWithSessionRefusals(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)

	// Unknown session.
	if code, _, raw := postMessage(t, svc, tok, "nosuchsession", map[string]any{"prompt": "hi"}); code != http.StatusNotFound {
		t.Errorf("unknown session: %d %s, want 404", code, raw)
	}
	// A session seen only through the gateway (pointed at it by hand) has no
	// agent and no resume_id.
	const byHand = "byhand"
	if _, _, err := svc.store.Append(event.Event{ID: "evt_byhand", Kind: event.TurnStarted, Time: time.Now(),
		Session: byHand, Turn: "turn_1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	before := svc.store.Last()
	if code, out, raw := postMessage(t, svc, tok, byHand, map[string]any{"prompt": "hi"}); code != http.StatusConflict ||
		out["error"] != "this session cannot be continued from here" {
		t.Errorf("hand-pointed session: %d %s, want 409 cannot be continued", code, raw)
	}
	if svc.store.Last() != before {
		t.Error("a refused turn stored events")
	}
	// A run whose output named no session.
	noID, _ := startRun(t, svc, tok, "claude", "plain text\n")
	if code, out, raw := postMessage(t, svc, tok, noID, map[string]any{"prompt": "hi"}); code != http.StatusConflict ||
		out["error"] != "this session cannot be continued from here" {
		t.Errorf("run without resume_id: %d %s, want 409 cannot be continued", code, raw)
	}
	if snapSession(t, svc, tok, noID)["resumable"].(bool) {
		t.Error("a session without resume_id is resumable")
	}

	// While a turn runs: 409, and not resumable.
	id, _ := startRun(t, svc, tok, "claude", claudeResultA)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_WAIT", release)
	t.Setenv("AGENTBUS_FAKE_STDOUT", claudeResultB)
	if code, _, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "two"}); code != http.StatusAccepted {
		t.Fatalf("second turn: %d %s", code, raw)
	}
	waitFor(t, "the turn's session.opened", func() bool { return snapSession(t, svc, tok, id)["open"].(bool) })
	if snapSession(t, svc, tok, id)["resumable"].(bool) {
		t.Error("resumable while a turn runs")
	}
	before = svc.store.Last()
	if code, out, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "three"}); code != http.StatusConflict ||
		out["error"] != "a turn is already running in this session" {
		t.Errorf("while running: %d %s, want 409 already running", code, raw)
	}
	if svc.store.Last() != before {
		t.Error("a refused turn stored events")
	}
	os.WriteFile(release, nil, 0o600)
	waitClosedN(t, svc, id, 2)
	if !snapSession(t, svc, tok, id)["resumable"].(bool) {
		t.Error("not resumable after the turn closed")
	}

	// The first run of a session is still running: already running, not
	// "cannot be continued".
	release2 := filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_WAIT", release2)
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("run %d %s", code, raw)
	}
	running := out["id"].(string)
	if code, out, raw := postMessage(t, svc, tok, running, map[string]any{"prompt": "x"}); code != http.StatusConflict ||
		out["error"] != "a turn is already running in this session" {
		t.Errorf("first run running: %d %s, want 409 already running", code, raw)
	}
	os.WriteFile(release2, nil, 0o600)
	waitClosed(t, svc.store, running)
	t.Setenv("AGENTBUS_FAKE_WAIT", "")

	// Body refusals, as runs.
	const badPrompt = "prompt must be from 1 byte to 256 KiB, with no NUL byte"
	const badBody = "the body must be one JSON object with prompt, agent and folder (or session), and optionally model, read_only and idempotency_key, at most 2 MiB"
	folder := closedFolder(t, svc, id)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"empty prompt", `{"prompt":""}`, badPrompt},
		{"no prompt", `{"read_only":true}`, badPrompt},
		{"NUL", `{"prompt":"a\u0000b"}`, badPrompt},
		{"over 256 KiB", `{"prompt":"` + strings.Repeat("a", 256<<10+1) + `"}`, badPrompt},
		{"model as flag", `{"prompt":"a","model":"--x"}`, "model must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"},
		{"unknown field", `{"prompt":"a","bogus":1}`, badBody},
		{"another agent", `{"prompt":"a","agent":"codex"}`,
			`agent "codex" is not this session's agent "claude"; leave agent out to use the session's`},
		{"another folder", `{"prompt":"a","folder":"/"}`,
			"folder is not this session's folder " + strconv.Quote(folder) + "; leave folder out to use the session's"},
		{"another agent, same folder", `{"prompt":"a","agent":"codex","folder":` + jsonOf(folder) + `}`,
			`agent "codex" is not this session's agent "claude"; leave agent out to use the session's`},
	} {
		before := svc.store.Last()
		code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", []byte(`{"session":"`+id+`",`+strings.TrimPrefix(tc.body, "{")))
		if code != http.StatusBadRequest || out["error"] != tc.want {
			t.Errorf("%s: %d %.200s, want 400 %q", tc.name, code, raw, tc.want)
		}
		if svc.store.Last() != before {
			t.Errorf("%s: stored events", tc.name)
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"not an object", `["a"]`},
		{"session not a string", `{"session":1,"prompt":"a"}`},
	} {
		if code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", []byte(tc.body)); code != http.StatusBadRequest || out["error"] != badBody {
			t.Errorf("%s: %d %.200s, want 400 %q", tc.name, code, raw, badBody)
		}
	}
	// A session sent empty or null is refused, even with agent, folder and
	// prompt that would start a new session: only an absent session does.
	for _, sess := range []string{`""`, `null`} {
		before := svc.store.Last()
		b := `{"session":` + sess + `,"agent":"claude","folder":` + jsonOf(t.TempDir()) + `,"prompt":"a"}`
		if code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", []byte(b)); code != http.StatusBadRequest ||
			out["error"] != "session must match ^[A-Za-z0-9_-]{1,64}$" {
			t.Errorf("session %s: %d %s, want 400", sess, code, raw)
		}
		if svc.store.Last() != before {
			t.Errorf("session %s: stored events", sess)
		}
	}
	// A session id of the wrong shape is refused before the store is read.
	if code, out, raw := postMessage(t, svc, tok, "a/b", map[string]any{"prompt": "a"}); code != http.StatusBadRequest ||
		out["error"] != "session must match ^[A-Za-z0-9_-]{1,64}$" {
		t.Errorf("bad session shape: %d %s, want 400", code, raw)
	}

	// The session's own agent and folder, sent again, are accepted.
	t.Setenv("AGENTBUS_FAKE_STDOUT", claudeResultB)
	if code, _, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "same", "agent": "claude",
		"folder": folder + "/"}); code != http.StatusAccepted {
		t.Fatalf("own agent and folder sent: %d %s, want 202", code, raw)
	}
	waitClosedN(t, svc, id, 3)
}

// closedFolder is the folder of session's first session.opened.
func closedFolder(t *testing.T, svc *service, session string) string {
	t.Helper()
	for _, e := range runEvents(t, svc.store, session) {
		if e.Kind == "session.opened" && e.Folder != "" {
			return e.Folder
		}
	}
	t.Fatalf("no session.opened with a folder in %s", session)
	return ""
}

// Two next turns sent at once: exactly one starts.
func TestRunWithSessionOneTurnAtATime(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	id, _ := startRun(t, svc, tok, "claude", claudeResultA)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_WAIT", release)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = postMessage(t, svc, tok, id, map[string]any{"prompt": "two"})
		}(i)
	}
	wg.Wait()
	if !(codes[0] == 202 && codes[1] == 409 || codes[0] == 409 && codes[1] == 202) {
		t.Fatalf("codes %v, want one 202 and one 409", codes)
	}
	os.WriteFile(release, nil, 0o600)
	waitClosedN(t, svc, id, 2)
	time.Sleep(50 * time.Millisecond)
	if n := len(closedEvents(runEvents(t, svc.store, id))); n != 2 {
		t.Fatalf("%d session.closed, want 2", n)
	}
}
