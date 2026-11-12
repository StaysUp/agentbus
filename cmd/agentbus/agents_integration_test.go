//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// openRouterService is runService with OpenRouter at upstream.
func openRouterService(t *testing.T, home, upstream string) *service {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	connectFake(t, home)
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), openrouter: mustURL(upstream), logf: t.Logf,
		lookPath: func(string) (string, error) { return self, nil }, noReplay: true,
		heartbeat: 100 * time.Millisecond, turnWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	return svc
}

func sendAPI(t *testing.T, svc *service, tok, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, "http://"+svc.addr+path, r)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// An OpenRouter agent's turn reaches OpenRouter through the gateway: the
// child is pointed at /openrouter of its base URL with a placeholder as its
// key, the call is forwarded to OpenRouter's /api/v1/messages with the key
// from the named variable of control's environment (up's relay adds it, as
// for a runner's), and neither the child nor the record holds the key.
func TestOpenRouterAgentTurnGoesThroughTheGateway(t *testing.T) {
	const keyValue = "test-key-value-0d1f"
	t.Setenv("AB_TEST_OR_KEY", keyValue)
	var mu sync.Mutex
	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"42"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer up.Close()
	home := t.TempDir()
	svc := openRouterService(t, home, up.URL+"/api")
	tok := readToken(t, home)

	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated || ag["key_set"] != true {
		t.Fatalf("create: %d %v", code, ag)
	}
	id := ag["id"].(string)

	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": id, "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	session, base := out["session"].(string), out["base_url"].(string)
	waitClosed(t, svc.store, session)

	r := readReport(t, report)
	for k, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                     base + "/openrouter",
		"ANTHROPIC_API_KEY":                      "",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":         "deepseek/deepseek-v4.1-flash",
		"CLAUDE_CODE_SUBAGENT_MODEL":             "deepseek/deepseek-v4.1-flash",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
	} {
		if v, ok := envValue(r.Env, k); !ok || v != want {
			t.Errorf("child %s = %q (set %v), want %q", k, v, ok, want)
		}
	}
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "AB_TEST_OR_KEY"} {
		if v, _ := envValue(r.Env, k); !strings.HasPrefix(v, jobs.TokenPrefix) {
			t.Errorf("child %s is not the placeholder", k)
		}
	}
	if strings.Contains(strings.Join(r.Env, "\n"), keyValue) {
		t.Error("the child's environment holds the key")
	}
	if !strings.Contains(strings.Join(r.Args, " "), "--model deepseek/deepseek-v4.1-flash -- hi") {
		t.Errorf("args %q", r.Args)
	}
	waitFor(t, "the forwarded call", func() bool { mu.Lock(); defer mu.Unlock(); return gotPath != "" })
	mu.Lock()
	if gotPath != "/api/v1/messages" || gotAuth != "Bearer "+keyValue || !strings.Contains(gotBody, "fake turn") {
		t.Errorf("OpenRouter got %s %q %s", gotPath, gotAuth, gotBody)
	}
	mu.Unlock()

	// The record: the agent and its access, never the key.
	recs, err := svc.store.After(0, store.Filter{Session: session, Only: true}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var opened map[string]any
	for _, rec := range recs {
		if strings.Contains(string(rec.Data), keyValue) {
			t.Errorf("an event holds the key: %s", rec.Data)
		}
		var e map[string]any
		json.Unmarshal(rec.Data, &e)
		if e["kind"] == "session.opened" {
			opened = e
		}
	}
	if opened["agent"] != "claude" || opened["agent_id"] != id || opened["access"] != "openrouter" || opened["key_env"] != "AB_TEST_OR_KEY" {
		t.Errorf("session.opened %v", opened)
	}
	var responded bool
	waitFor(t, "model.responded", func() bool {
		recs, _ := svc.store.After(0, store.Filter{Session: session, Only: true}, 1000)
		for _, rec := range recs {
			responded = responded || strings.Contains(string(rec.Data), `"model.responded"`)
		}
		return responded
	})
	svc.stop()
	logs, _ := os.ReadFile(filepath.Join(home, "logs", "service.log"))
	if strings.Contains(string(logs), keyValue) {
		t.Error("service.log holds the key")
	}
}

// codex on OpenRouter gets a provider of its own through -c flags, reading
// the key from the named variable itself, and no ChatGPT base URL.
func TestOpenRouterCodexArgs(t *testing.T) {
	t.Setenv("AB_TEST_OR_KEY", "x")
	home := t.TempDir()
	svc := openRouterService(t, home, "http://127.0.0.1:1")
	tok := readToken(t, home)
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "gpt-oss", "harness": "codex",
		"access": "openrouter", "model": "openai/gpt-oss-120b", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": ag["id"], "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["session"].(string))
	args := strings.Join(readReport(t, report).Args, " ")
	base := out["base_url"].(string)
	want := `-c model_provider=agentbus_openrouter -c model_providers.agentbus_openrouter.name="OpenRouter" ` +
		`-c model_providers.agentbus_openrouter.base_url="` + base + `/openrouter/v1" ` +
		`-c model_providers.agentbus_openrouter.env_key="AB_TEST_OR_KEY" ` +
		`-c model_providers.agentbus_openrouter.wire_api="responses" ` +
		`-c model_providers.agentbus_openrouter.supports_websockets=false exec `
	if !strings.HasPrefix(args, want) || strings.Contains(args, "openai_base_url") || !strings.Contains(args, "--model openai/gpt-oss-120b -- hi") {
		t.Errorf("args\n%s\nwant prefix\n%s", args, want)
	}
}

// An OpenRouter agent whose key variable is not set is refused before
// anything is stored, saying which variable.
func TestOpenRouterAgentWithoutKeyRefused(t *testing.T) {
	t.Setenv("AB_TEST_OR_KEY", "")
	home := t.TempDir()
	svc := openRouterService(t, home, "http://127.0.0.1:1")
	tok := readToken(t, home)
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated || ag["key_set"] != false {
		t.Fatalf("create: %d %v", code, ag)
	}
	before := svc.store.Last()
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": ag["id"], "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "AB_TEST_OR_KEY, which is not set") {
		t.Fatalf("%d %s", code, raw)
	}
	if svc.store.Last() != before {
		t.Fatal("a refused turn stored events")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// isolationSetting writes the isolation setting into home's permissions
// file, before the service starts.
func isolationSetting(t *testing.T, home, value string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "permissions.toml"), []byte("isolation = \""+value+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A new session runs in its own copy of its folder, made as the isolation
// setting says, exactly as the folder is now, uncommitted edits included:
// copy (the default) copies any folder; git makes a worktree of a repo at
// HEAD, in the same folder inside it, and copies a folder outside git;
// isolate false, or the setting none, runs in the folder itself.
func TestNewSessionIsolated(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o700)
	os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("v1"), 0o600)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "one")
	os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("uncommitted"), 0o600)
	plain := t.TempDir()
	os.WriteFile(filepath.Join(plain, "a.txt"), []byte("uncommitted"), 0o600)
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }

	for _, setting := range []string{"", "git", "none"} {
		home := t.TempDir()
		if setting != "" {
			isolationSetting(t, home, setting)
		}
		svc := runService(t, home, nil, 0)
		tok := readToken(t, home)
		// A copy is named after its session.
		copied := func(session string) string { return filepath.Join(home, "sessions", session) }
		inRepo := copied
		if setting == "git" {
			inRepo = func(session string) string { return filepath.Join(home, "sessions", session, "sub") }
		}
		mine := func(f string) func(string) string { return func(string) string { return f } }
		if setting == "none" {
			copied, inRepo = mine(plain), mine(filepath.Join(repo, "sub"))
		}
		for _, tc := range []struct {
			name    string
			folder  string
			isolate any
			want    func(session string) string
		}{
			{"repo subfolder", filepath.Join(repo, "sub"), nil, inRepo},
			{"isolate false", filepath.Join(repo, "sub"), false, mine(filepath.Join(repo, "sub"))},
			{"not a repo", plain, nil, copied},
		} {
			name := "setting " + strconv.Quote(setting) + ", " + tc.name
			report := filepath.Join(t.TempDir(), "report")
			t.Setenv("AGENTBUS_FAKE_OUT", report)
			body := map[string]any{"agent": "claude", "folder": tc.folder, "prompt": "hi"}
			if tc.isolate != nil {
				body["isolate"] = tc.isolate
			}
			code, out, raw := postRun(t, svc, tok, body)
			if code != http.StatusAccepted {
				t.Fatalf("%s: %d %s", name, code, raw)
			}
			waitClosed(t, svc.store, out["session"].(string))
			r := readReport(t, report)
			if real(r.Cwd) != real(tc.want(out["session"].(string))) {
				t.Errorf("%s: ran in %s, want %s", name, r.Cwd, tc.want(out["session"].(string)))
			}
			if b, err := os.ReadFile(filepath.Join(r.Cwd, "a.txt")); err != nil || string(b) != "uncommitted" {
				t.Errorf("%s: the session does not start from the folder as it is: %q %v", name, b, err)
			}
		}
		svc.stop()
	}
	// isolate with session is refused: a session keeps its workspace.
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	code, out, _ := postRun(t, svc, readToken(t, home), map[string]any{"session": "abc", "prompt": "hi", "isolate": true})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "isolate is for a new session") {
		t.Errorf("isolate with session: %d %v", code, out)
	}
}

// A copied session keeps its copy, named after the session, across turns;
// the job's result names the checkpoints each turn ran between; closing the
// session removes the copy and its record; the person's folder is never
// changed.
func TestCopiedSessionKeptAcrossTurnsAndRemovedOnClose(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("v1"), 0o600)
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","session_id":"sess-1"}`+"\n")
	code, j := startTurn(t, svc, tok, map[string]any{"agent": "claude", "folder": folder, "isolate": true, "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	j = waitJob(t, svc, tok, j.ID)
	want := filepath.Join(home, "sessions", j.Session)
	// The control plane knows the copy by its space, never by a path.
	if j.State != queue.Finished || j.Workspace == nil || j.Workspace.Kind != queue.WorkspaceCopy || j.Workspace.Space != j.Session ||
		j.Workspace.Folder != "" {
		t.Fatalf("first job %s, workspace %+v, want a copy %s at %s", j.State, j.Workspace, j.Session, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("no copy at %s: %v", want, err)
	}
	// The second turn writes a file in the copy, through the fake agent.
	t.Setenv("AGENTBUS_FAKE_MARK", filepath.Join(want, "made.txt"))
	code, next := startTurn(t, svc, tok, map[string]any{"session": j.Session, "agent": j.Spec.Harness, "prompt": "two"})
	if code != http.StatusAccepted {
		t.Fatalf("next turn: %d", code)
	}
	if next = waitJob(t, svc, tok, next.ID); next.State != queue.Finished {
		t.Fatalf("next job %s %s %s", next.State, next.Reason, next.Result)
	}
	// A finished job's changes are in the record already: it ends only after.
	c, err := jobs.Changes(svc.store, next)
	if err != nil || c == nil || c.Count != 1 || c.Files[0].Path != "made.txt" || c.From != "base" || c.To != next.Turn {
		t.Fatalf("second turn's changes %+v %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(folder, "made.txt")); err == nil {
		t.Fatal("the turn changed the person's folder")
	}
	if code := callAPI(t, svc, tok, "PATCH", "/api/v0/sessions/"+j.Session, map[string]any{"closed": true}, nil); code/100 != 2 {
		t.Fatalf("close: %d", code)
	}
	waitFor(t, "the copy removed", func() bool {
		_, err1 := os.Stat(want)
		_, err2 := os.Stat(filepath.Join(home, "sessions", ".isolation", j.Session))
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
}

// The saved agents outlast a restart, and a removed default still lets a
// turn name its harness.
func TestSavedAgentsKeptAndDefaultsSeeded(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	code, list := sendAPI(t, svc, tok, "GET", "/api/v0/agents", nil)
	ags, _ := list["agents"].([]any)
	if code != 200 || len(ags) != 3 || ags[0].(map[string]any)["id"] != "claude" || ags[1].(map[string]any)["id"] != "codex" ||
		ags[2].(map[string]any)["id"] != "agy" {
		t.Fatalf("seeded: %d %v", code, list)
	}
	code, made := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "Claude Code · haiku", "harness": "claude",
		"access": "subscription", "model": "haiku"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, made)
	}
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/claude", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	svc.stop()

	svc = runService(t, home, nil, 0)
	_, list = sendAPI(t, svc, tok, "GET", "/api/v0/agents", nil)
	ags, _ = list["agents"].([]any)
	if len(ags) != 3 || ags[0].(map[string]any)["id"] != "codex" || ags[2].(map[string]any)["id"] != made["id"] {
		t.Fatalf("after restart: %v", list)
	}
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": made["id"], "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["session"].(string))
	if args := strings.Join(readReport(t, report).Args, " "); !strings.Contains(args, "--model haiku -- hi") {
		t.Errorf("the agent's model is not used: %s", args)
	}
	code, out, raw = postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("removed default: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["session"].(string))
}

// An isolated session in a repo's subfolder keeps that subfolder: its next
// turn runs there, its job's workspace names it, a review reads it, and
// closing the session removes the whole worktree.
func TestIsolatedSubfolderKeptAcrossTurnsAndReview(t *testing.T) {
	home := t.TempDir()
	isolationSetting(t, home, "git")
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o700)
	os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("v1"), 0o600)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "one")
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }

	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	// A resume id, so the session's next turn can continue (replay is off).
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","session_id":"sess-1"}`+"\n")
	code, j := startTurn(t, svc, tok, map[string]any{"agent": "claude", "folder": filepath.Join(repo, "sub"), "isolate": true, "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	j = waitJob(t, svc, tok, j.ID)
	want := filepath.Join(home, "sessions", j.Session, "sub")
	if j.State != queue.Finished || j.Workspace == nil || j.Workspace.Space != j.Session || j.Workspace.Folder != "" {
		t.Fatalf("first job %s, workspace %+v, want space %s", j.State, j.Workspace, j.Session)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("no worktree folder at %s: %v", want, err)
	}
	// The queue keeps the folder by its reference, and the session its
	// snapshot id; the record has the reference too.
	if j.Spec.Folder != "" || j.Spec.FolderRef != folders.Ref(filepath.Join(repo, "sub")) || j.Spec.Connector == "" ||
		!queue.SnapshotShape.MatchString(j.Workspace.Snapshot) {
		t.Fatalf("first job spec %+v, snapshot %q", j.Spec, j.Workspace.Snapshot)
	}
	if c, err := svc.store.Continuation(j.Session); err != nil || c.FolderRef != j.Spec.FolderRef {
		t.Fatalf("the record's folder_ref %q, want %q (%v)", c.FolderRef, j.Spec.FolderRef, err)
	}
	os.Remove(report)
	code, next := startTurn(t, svc, tok, map[string]any{"session": j.Session, "agent": j.Spec.Harness, "prompt": "two"})
	if code != http.StatusAccepted {
		t.Fatalf("next turn: %d", code)
	}
	if next = waitJob(t, svc, tok, next.ID); next.State != queue.Finished {
		t.Fatalf("next job %s %s %s", next.State, next.Reason, next.Result)
	}
	if r := readReport(t, report); real(r.Cwd) != real(want) {
		t.Fatalf("next turn ran in %s, want %s", r.Cwd, want)
	}
	code, rev := reviewTurn(t, svc, tok, next.Turn, map[string]any{"agent": "codex"})
	if code != http.StatusAccepted {
		t.Fatalf("review: %d", code)
	}
	// The review names no folder: it reads the reviewed session's workspace.
	if rev.Spec.Folder != "" || rev.Workspace == nil || rev.Workspace.Space != j.Session {
		t.Fatalf("review spec folder %q, workspace %+v, want %s", rev.Spec.Folder, rev.Workspace, want)
	}
	waitJob(t, svc, tok, rev.ID)
	if code := callAPI(t, svc, tok, "PATCH", "/api/v0/sessions/"+j.Session, map[string]any{"closed": true}, nil); code/100 != 2 {
		t.Fatalf("close: %d", code)
	}
	waitFor(t, "the worktree removed", func() bool {
		_, err := os.Stat(filepath.Join(home, "sessions", j.Session))
		return os.IsNotExist(err)
	})
}

// A retry with the same idempotency key gets the first job even after the
// saved agent was edited: the request is what was asked, not what the agent
// says now.
func TestRetryAfterAgentEditGetsTheSameTurn(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "fast", "harness": "claude",
		"access": "subscription", "model": "haiku"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	id := ag["id"].(string)
	body := map[string]any{"agent": id, "folder": t.TempDir(), "prompt": "hi", "idempotency_key": "k-1"}
	code, first, raw := postRun(t, svc, tok, body)
	if code != http.StatusAccepted {
		t.Fatalf("first: %d %s", code, raw)
	}
	waitClosed(t, svc.store, first["session"].(string))
	if code, _ := sendAPI(t, svc, tok, "PUT", "/api/v0/agents/"+id, map[string]any{"name": "fast", "harness": "claude",
		"access": "subscription", "model": "sonnet"}); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	code, again, raw := postRun(t, svc, tok, body)
	if code != http.StatusAccepted || again["existing"] != true || again["turn"] != first["turn"] {
		t.Fatalf("retry: %d %s, want the first turn %v", code, raw, first["turn"])
	}
	// And after the agent is removed: the retry still gets the first turn;
	// a new key is refused.
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/"+id, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	code, again, raw = postRun(t, svc, tok, body)
	if code != http.StatusAccepted || again["existing"] != true || again["turn"] != first["turn"] {
		t.Fatalf("retry after remove: %d %s", code, raw)
	}
	body["idempotency_key"] = "k-3"
	if code, _, raw := postRun(t, svc, tok, body); code != http.StatusBadRequest {
		t.Fatalf("new key, removed agent: %d %s", code, raw)
	}
}

// A repo subfolder not in the last commit cannot be isolated: the turn is
// refused with why, and no worktree is left behind.
func TestIsolatedFolderNotInCommitRefused(t *testing.T) {
	home := t.TempDir()
	isolationSetting(t, home, "git")
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("v1"), 0o600)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "one")
	os.MkdirAll(filepath.Join(repo, "new"), 0o700)
	os.WriteFile(filepath.Join(repo, "new", "b.txt"), []byte("x"), 0o600)
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": filepath.Join(repo, "new"), "prompt": "hi"})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "not in its repo's last commit") {
		t.Fatalf("%d %s", code, raw)
	}
	entries, _ := os.ReadDir(filepath.Join(home, "sessions"))
	if len(entries) != 0 {
		t.Fatalf("copies left: %v", entries)
	}
}

// A review names its reviewer as a saved agent, and the job carries what
// the agent says: its harness, id, access and model.
func TestReviewBySavedAgent(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "reviewer", "harness": "codex",
		"access": "subscription", "model": "gpt-5.5"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	code, j := startTurn(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "isolate": false, "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d", code)
	}
	waitJob(t, svc, tok, j.ID)
	code, rev := reviewTurn(t, svc, tok, j.Turn, map[string]any{"agent": ag["id"]})
	if code != http.StatusAccepted {
		t.Fatalf("review: %d", code)
	}
	if rev.Spec.Harness != "codex" || rev.Spec.AgentID != ag["id"] || rev.Spec.Access != "subscription" || rev.Spec.Model != "gpt-5.5" {
		t.Fatalf("review spec %+v", rev.Spec)
	}
	if rev = waitJob(t, svc, tok, rev.ID); rev.State != queue.Finished {
		t.Fatalf("review %s %s %s", rev.State, rev.Reason, rev.Result)
	}
	if code, _ := reviewTurn(t, svc, tok, j.Turn, map[string]any{"agent": "a_nobody"}); code != http.StatusBadRequest {
		t.Fatalf("unknown reviewer: %d", code)
	}
	// A keyed review, its reviewer removed, then the same request: the first review.
	req := map[string]any{"agent": ag["id"], "idempotency_key": "rev-1"}
	code, k1 := reviewTurn(t, svc, tok, j.Turn, req)
	if code != http.StatusAccepted {
		t.Fatalf("keyed review: %d", code)
	}
	waitJob(t, svc, tok, k1.ID)
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/"+ag["id"].(string), nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code, k2 := reviewTurn(t, svc, tok, j.Turn, req); code != http.StatusAccepted || k2.ID != k1.ID {
		t.Fatalf("retry after remove: %d %s, want %s", code, k2.ID, k1.ID)
	}
}

// A subfolder the commit records as a symlink out of the repo is refused,
// even when the folder itself is a real folder now: the turn never runs
// outside its worktree.
func TestIsolatedSymlinkOutRefused(t *testing.T) {
	home := t.TempDir()
	isolationSetting(t, home, "git")
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	repo, outside := t.TempDir(), t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.Symlink(outside, filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "one")
	os.Remove(filepath.Join(repo, "sub"))
	os.MkdirAll(filepath.Join(repo, "sub"), 0o700)
	t.Setenv("AGENTBUS_FAKE_MARK", filepath.Join(outside, "ran"))
	code, _, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": filepath.Join(repo, "sub"), "prompt": "hi"})
	if code != http.StatusBadRequest {
		t.Fatalf("%d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(outside, "ran")); err == nil {
		t.Fatal("the agent ran outside the worktree")
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "sessions")); len(entries) != 0 {
		t.Fatalf("copies left: %v", entries)
	}
}
