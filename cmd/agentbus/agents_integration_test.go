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
	"strings"
	"sync"
	"testing"
	"time"

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
// child is pointed at /openrouter of its base URL with the key from the
// named variable, the call is forwarded to OpenRouter's /api/v1/messages
// with that key, and the record names the agent and its access but never
// holds the key.
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
	session, base := out["id"].(string), out["base_url"].(string)
	waitClosed(t, svc.store, session)

	r := readReport(t, report)
	for k, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                     base + "/openrouter",
		"ANTHROPIC_AUTH_TOKEN":                   keyValue,
		"ANTHROPIC_API_KEY":                      "",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":         "deepseek/deepseek-v4.1-flash",
		"CLAUDE_CODE_SUBAGENT_MODEL":             "deepseek/deepseek-v4.1-flash",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
	} {
		if v, ok := envValue(r.Env, k); !ok || v != want {
			t.Errorf("child %s = %q (set %v), want %q", k, v, ok, want)
		}
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
	waitClosed(t, svc.store, out["id"].(string))
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

// A new session in a folder of a git repo runs in its own worktree of that
// repo at HEAD, in the same folder inside it; isolate false, or a folder
// outside git, runs in the folder itself.
func TestNewSessionIsolatedInAWorktree(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o700)
	os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("v1"), 0o600)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "one")
	plain := t.TempDir()
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }

	for _, tc := range []struct {
		name    string
		folder  string
		isolate any
		want    func(job string) string
	}{
		{"repo subfolder", filepath.Join(repo, "sub"), nil, func(job string) string {
			return filepath.Join(home, "worktrees", job, "sub")
		}},
		{"isolate false", filepath.Join(repo, "sub"), false, func(string) string { return filepath.Join(repo, "sub") }},
		{"not a repo", plain, nil, func(string) string { return plain }},
	} {
		report := filepath.Join(t.TempDir(), "report")
		t.Setenv("AGENTBUS_FAKE_OUT", report)
		body := map[string]any{"agent": "claude", "folder": tc.folder, "prompt": "hi"}
		if tc.isolate != nil {
			body["isolate"] = tc.isolate
		}
		code, out, raw := postRun(t, svc, tok, body)
		if code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", tc.name, code, raw)
		}
		waitClosed(t, svc.store, out["id"].(string))
		r := readReport(t, report)
		if real(r.Cwd) != real(tc.want(out["job"].(string))) {
			t.Errorf("%s: ran in %s, want %s", tc.name, r.Cwd, tc.want(out["job"].(string)))
		}
		if tc.name == "repo subfolder" {
			if b, err := os.ReadFile(filepath.Join(r.Cwd, "a.txt")); err != nil || string(b) != "v1" {
				t.Errorf("the worktree does not hold the repo's HEAD: %q %v", b, err)
			}
		}
	}
	// isolate with session is refused: a session keeps its workspace.
	code, out, _ := postRun(t, svc, tok, map[string]any{"session": "abc", "prompt": "hi", "isolate": true})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "isolate is for a new session") {
		t.Errorf("isolate with session: %d %v", code, out)
	}
}

// The saved agents outlast a restart, and a removed default still lets a
// turn name its harness.
func TestSavedAgentsKeptAndDefaultsSeeded(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	code, list := sendAPI(t, svc, tok, "GET", "/api/v0/agents", nil)
	ags, _ := list["agents"].([]any)
	if code != 200 || len(ags) != 2 || ags[0].(map[string]any)["id"] != "claude" || ags[1].(map[string]any)["id"] != "codex" {
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
	if len(ags) != 2 || ags[0].(map[string]any)["id"] != "codex" || ags[1].(map[string]any)["id"] != made["id"] {
		t.Fatalf("after restart: %v", list)
	}
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": made["id"], "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["id"].(string))
	if args := strings.Join(readReport(t, report).Args, " "); !strings.Contains(args, "--model haiku -- hi") {
		t.Errorf("the agent's model is not used: %s", args)
	}
	code, out, raw = postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("removed default: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["id"].(string))
}

// An isolated session in a repo's subfolder keeps that subfolder: its next
// turn runs there, its job's workspace names it, a review reads it, and
// closing the workspace removes the whole worktree.
func TestIsolatedSubfolderKeptAcrossTurnsAndReview(t *testing.T) {
	home := t.TempDir()
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
	var j queue.Job
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"agent": "claude", "folder": filepath.Join(repo, "sub"), "isolate": true, "prompt": "one"}}, &j); code != http.StatusCreated {
		t.Fatalf("make job: %d", code)
	}
	j = waitJob(t, svc, tok, j.ID)
	want := filepath.Join(home, "worktrees", j.ID, "sub")
	if j.State != queue.Finished || j.Workspace == nil || real(j.Workspace.Folder) != real(want) {
		t.Fatalf("first job %s, workspace %+v, want folder %s", j.State, j.Workspace, want)
	}
	os.Remove(report)
	var next queue.Job
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"session": j.Session, "prompt": "two"}}, &next); code != http.StatusCreated {
		t.Fatalf("next job: %d", code)
	}
	if next = waitJob(t, svc, tok, next.ID); next.State != queue.Finished {
		t.Fatalf("next job %s %s %s", next.State, next.Reason, next.Result)
	}
	if r := readReport(t, report); real(r.Cwd) != real(want) {
		t.Fatalf("next turn ran in %s, want %s", r.Cwd, want)
	}
	var rev queue.Job
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs/"+next.ID+"/review", map[string]any{"agent": "codex"}, &rev); code != http.StatusCreated {
		t.Fatalf("review: %d", code)
	}
	if real(rev.Spec.Folder) != real(want) {
		t.Fatalf("review folder %s, want %s", rev.Spec.Folder, want)
	}
	waitJob(t, svc, tok, rev.ID)
	if code := callAPI(t, svc, tok, "POST", "/api/v0/workspaces/"+j.Session+"/close", nil, nil); code/100 != 2 {
		t.Fatalf("close: %d", code)
	}
	waitFor(t, "the worktree removed", func() bool {
		_, err := os.Stat(filepath.Join(home, "worktrees", j.ID))
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
	waitClosed(t, svc.store, first["id"].(string))
	if code, _ := sendAPI(t, svc, tok, "PUT", "/api/v0/agents/"+id, map[string]any{"name": "fast", "harness": "claude",
		"access": "subscription", "model": "sonnet"}); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	code, again, raw := postRun(t, svc, tok, body)
	if code != http.StatusAccepted || again["existing"] != true || again["turn"] != first["turn"] {
		t.Fatalf("retry: %d %s, want the first turn %v", code, raw, first["turn"])
	}
	// Jobs too.
	jb := map[string]any{"kind": "turn", "idempotency_key": "k-2", "spec": map[string]any{"agent": id, "folder": t.TempDir(), "prompt": "hi"}}
	var j1, j2, j3 queue.Job
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", jb, &j1); code != http.StatusCreated || j1.Spec.Model != "sonnet" {
		t.Fatalf("job: %d %+v", code, j1.Spec)
	}
	sendAPI(t, svc, tok, "PUT", "/api/v0/agents/"+id, map[string]any{"name": "fast", "harness": "claude", "access": "subscription"})
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", jb, &j2); code != http.StatusOK || j2.ID != j1.ID {
		t.Fatalf("job retry: %d %s, want %s", code, j2.ID, j1.ID)
	}
	waitJob(t, svc, tok, j1.ID)

	// And after the agent is removed: the retry still gets the first turn
	// and job; a new key is refused.
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/"+id, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	code, again, raw = postRun(t, svc, tok, body)
	if code != http.StatusAccepted || again["existing"] != true || again["turn"] != first["turn"] {
		t.Fatalf("retry after remove: %d %s", code, raw)
	}
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", jb, &j3); code != http.StatusOK || j3.ID != j1.ID {
		t.Fatalf("job retry after remove: %d %s, want %s", code, j3.ID, j1.ID)
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
	entries, _ := os.ReadDir(filepath.Join(home, "worktrees"))
	if len(entries) != 0 {
		t.Fatalf("worktrees left: %v", entries)
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
	var j queue.Job
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "one"}}, &j); code != http.StatusCreated {
		t.Fatalf("job: %d", code)
	}
	waitJob(t, svc, tok, j.ID)
	var rev queue.Job
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs/"+j.ID+"/review", map[string]any{"agent": ag["id"]}, &rev); code != http.StatusCreated {
		t.Fatalf("review: %d", code)
	}
	if rev.Spec.Harness != "codex" || rev.Spec.AgentID != ag["id"] || rev.Spec.Access != "subscription" || rev.Spec.Model != "gpt-5.5" {
		t.Fatalf("review spec %+v", rev.Spec)
	}
	if rev = waitJob(t, svc, tok, rev.ID); rev.State != queue.Finished {
		t.Fatalf("review %s %s %s", rev.State, rev.Reason, rev.Result)
	}
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs/"+j.ID+"/review", map[string]any{"agent": "a_nobody"}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown reviewer: %d", code)
	}
	// A keyed review, its reviewer removed, then the same request: the first review.
	var k1, k2 queue.Job
	req := map[string]any{"agent": ag["id"], "idempotency_key": "rev-1"}
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs/"+j.ID+"/review", req, &k1); code != http.StatusCreated {
		t.Fatalf("keyed review: %d", code)
	}
	waitJob(t, svc, tok, k1.ID)
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/"+ag["id"].(string), nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code := callAPI(t, svc, tok, "POST", "/api/v0/jobs/"+j.ID+"/review", req, &k2); code != http.StatusOK || k2.ID != k1.ID {
		t.Fatalf("retry after remove: %d %s, want %s", code, k2.ID, k1.ID)
	}
}

// A subfolder the commit records as a symlink out of the repo is refused,
// even when the folder itself is a real folder now: the turn never runs
// outside its worktree.
func TestIsolatedSymlinkOutRefused(t *testing.T) {
	home := t.TempDir()
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
	if entries, _ := os.ReadDir(filepath.Join(home, "worktrees")); len(entries) != 0 {
		t.Fatalf("worktrees left: %v", entries)
	}
}
