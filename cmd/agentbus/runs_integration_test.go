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
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/store"
)

// runService starts the service with the test binary standing in for every
// agent, through its lookPath seam. Own replay is off, so claude's next turns
// use --resume as codex's do: the fake agent makes no real model call, so no
// turn has a reply to replay. replay_test.go starts a service with it on.
func runService(t *testing.T, home string, getenv func(string) string, grace time.Duration) *service {
	return runServiceWith(t, home, getenv, grace, "http://127.0.0.1:1", true)
}

func runServiceWith(t *testing.T, home string, getenv func(string) string, grace time.Duration, upstream string, noReplay bool) *service {
	t.Helper()
	return runServiceVendors(t, home, getenv, grace, upstream, "http://127.0.0.1:1", noReplay)
}

func runServiceVendors(t *testing.T, home string, getenv func(string) string, grace time.Duration, upstream, chatgpt string, noReplay bool) *service {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	// codex's config is read for the person's own developer_instructions;
	// an empty one, so the machine's own config never changes a test.
	t.Setenv("CODEX_HOME", t.TempDir())
	// The service's own environment must not send an agent elsewhere.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL(upstream),
		chatgpt: mustURL(chatgpt), logf: t.Logf, getenv: getenv, runGrace: grace,
		lookPath: func(string) (string, error) { return self, nil }, noReplay: noReplay,
		heartbeat: 100 * time.Millisecond, turnWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	return svc
}

type runEvent struct {
	Kind     string `json:"kind"`
	Session  string `json:"session"`
	Agent    string `json:"agent"`
	Folder   string `json:"folder"`
	Hold     string `json:"hold"`
	ExitCode *int   `json:"exit_code"`
	ResumeID string `json:"resume_id"`
	Stopped  bool   `json:"stopped"`
}

func runEvents(t *testing.T, st *store.Store, session string) []runEvent {
	t.Helper()
	recs, err := st.After(0, store.Filter{Session: session, Only: true}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []runEvent
	for _, r := range recs {
		var e runEvent
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func closedEvent(evs []runEvent) *runEvent {
	for i := range evs {
		if evs[i].Kind == "session.closed" {
			return &evs[i]
		}
	}
	return nil
}

func waitClosed(t *testing.T, st *store.Store, session string) runEvent {
	t.Helper()
	var c *runEvent
	waitFor(t, "session.closed", func() bool { c = closedEvent(runEvents(t, st, session)); return c != nil })
	return *c
}

func readReport(t *testing.T, path string) fakeReport {
	t.Helper()
	var b []byte
	waitFor(t, "the fake agent's report", func() bool {
		var err error
		b, err = os.ReadFile(path)
		return err == nil
	})
	var r fakeReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func envValue(env []string, k string) (string, bool) {
	for _, kv := range env {
		if name, v, _ := strings.Cut(kv, "="); name == k {
			return v, true
		}
	}
	return "", false
}

func TestRunSetsBaseURLInChildOnlyAndClosesWithExitCode(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_EXIT", "3")
	folder := t.TempDir()

	// isolate false: the turn runs in folder itself, which events name.
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": folder, "isolate": false,
		"prompt": "--dangerously-skip-permissions hello", "model": "haiku"})
	if code != http.StatusAccepted {
		t.Fatalf("status %d: %s", code, raw)
	}
	id, _ := out["session"].(string)
	turn, _ := out["turn"].(string)
	base, _ := out["base_url"].(string)
	if id == "" || turn == "" || base != "http://"+svc.addr+"/s/"+id+"/t/"+turn {
		t.Fatalf("reply %s", raw)
	}

	c := waitClosed(t, svc.store, id)
	if c.ExitCode == nil || *c.ExitCode != 3 || c.Hold == "" {
		t.Fatalf("session.closed %+v, want exit_code 3 and a hold", c)
	}
	// Nothing writes session.registered any more: session.opened carries
	// the agent and folder, and the run stores its turn's turn.started.
	evs := runEvents(t, svc.store, id)
	if len(evs) != 4 || evs[0].Kind != "session.opened" || evs[0].Hold != c.Hold || evs[1].Kind != "turn.started" || evs[2].Kind != "context.added" {
		t.Fatalf("events %+v", evs)
	}
	if evs[0].Agent != "claude" || evs[0].Folder != folder {
		t.Fatalf("session.opened %+v, want agent claude and the folder", evs[0])
	}

	r := readReport(t, report)
	if u, _ := envValue(r.Env, "ANTHROPIC_BASE_URL"); u != base {
		t.Fatalf("child ANTHROPIC_BASE_URL %q, want %q", u, base)
	}
	if got := os.Getenv("ANTHROPIC_BASE_URL"); got != "" {
		t.Fatalf("the service's own ANTHROPIC_BASE_URL changed to %q", got)
	}
	// The prompt comes last, after --, so a prompt shaped like a flag stays a prompt.
	want := "-p --output-format json --append-system-prompt HINT --allowedTools EDIT --disallowedTools PROTECT --model haiku -- --dangerously-skip-permissions hello"
	if got := argsOf(t, r.Args, id); got != want {
		t.Fatalf("child args %q, want %q", got, want)
	}
	wantDir, _ := filepath.EvalSymlinks(folder)
	if gotDir, _ := filepath.EvalSymlinks(r.Cwd); gotDir != wantDir {
		t.Fatalf("child folder %q, want %q", gotDir, wantDir)
	}
	all := strings.Join(r.Env, "\n") + strings.Join(r.Args, "\n")
	if strings.Contains(all, tok) {
		t.Fatal("the token reached the child")
	}
	// The turn's own token, and the service's address, for reading its
	// session's history.
	if u, _ := envValue(r.Env, "AGENTBUS_URL"); u != "http://"+svc.addr {
		t.Errorf("child AGENTBUS_URL %q, want http://%s", u, svc.addr)
	}
	if v, _ := envValue(r.Env, "AGENTBUS_TOKEN"); !strings.HasPrefix(v, "abt_") || len(v) != 68 {
		t.Errorf("child AGENTBUS_TOKEN is not a turn token (%d bytes)", len(v))
	}
}

func TestRunHoldsSessionOpenUntilChildExits(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_WAIT", release)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")

	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "codex", "folder": t.TempDir(), "prompt": "p"})
	if code != http.StatusAccepted {
		t.Fatalf("status %d: %s", code, raw)
	}
	id := out["session"].(string)
	time.Sleep(150 * time.Millisecond)
	if closedEvent(runEvents(t, svc.store, id)) != nil {
		t.Fatal("session.closed while the child runs")
	}
	snap, err := svc.store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 1 || !snap.Sessions[0].Open {
		t.Fatalf("snapshot %+v, want one open session", snap.Sessions)
	}
	os.WriteFile(release, nil, 0o600)
	if c := waitClosed(t, svc.store, id); c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("session.closed %+v, want exit_code 0", c)
	}
}

func TestRunReadOnlyFlags(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	for _, tc := range []struct {
		agent    string
		readOnly any
		want     string // with BASE for the session's base URL
	}{
		{"claude", false, "-p --output-format json --append-system-prompt HINT --allowedTools EDIT --disallowedTools PROTECT -- hi"},
		{"claude", true, "-p --output-format json --append-system-prompt HINT --tools Read,Grep,Glob --allowedTools Read,Grep,Glob --strict-mcp-config -- hi"},
		{"claude", nil, "-p --output-format json --append-system-prompt HINT --allowedTools EDIT --disallowedTools PROTECT -- hi"},
		{"codex", nil, `-c openai_base_url="BASE/backend-api/codex" exec --skip-git-repo-check --json --sandbox workspace-write -c developer_instructions=HINT -c sandbox_workspace_write.network_access=true -- hi`},
		{"codex", true, `-c openai_base_url="BASE/backend-api/codex" exec --skip-git-repo-check --json --sandbox read-only -c developer_instructions=HINT -- hi`},
		// False names workspace-write with network on: codex exec's own
		// default is read-only, and its workspace-write has no network
		// (acceptance soak, 2026-11-06). True gets neither.
		{"codex", false, `-c openai_base_url="BASE/backend-api/codex" exec --skip-git-repo-check --json --sandbox workspace-write -c developer_instructions=HINT -c sandbox_workspace_write.network_access=true -- hi`},
	} {
		report := filepath.Join(t.TempDir(), "report")
		t.Setenv("AGENTBUS_FAKE_OUT", report)
		body := map[string]any{"agent": tc.agent, "folder": t.TempDir(), "prompt": "hi"}
		if tc.readOnly != nil {
			body["read_only"] = tc.readOnly
		}
		code, out, raw := postRun(t, svc, tok, body)
		if code != http.StatusAccepted {
			t.Fatalf("%s %v: status %d: %s", tc.agent, tc.readOnly, code, raw)
		}
		want := strings.ReplaceAll(tc.want, "BASE", out["base_url"].(string))
		rep := readReport(t, report)
		if got := argsOf(t, rep.Args, out["session"].(string)); got != want {
			t.Errorf("%s read_only %v: args %q, want %q", tc.agent, tc.readOnly, got, want)
		}
		// Both agents get a turn token, read-only or not.
		if v, _ := envValue(rep.Env, "AGENTBUS_TOKEN"); !strings.HasPrefix(v, "abt_") || v == tok {
			t.Errorf("%s read_only %v: no turn token in the child's environment", tc.agent, tc.readOnly)
		}
		waitClosed(t, svc.store, out["session"].(string))
	}
}

func TestRunRefusals(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	folder := t.TempDir()
	file := filepath.Join(folder, "f")
	os.WriteFile(file, nil, 0o600)
	ok := func(m map[string]any) []byte {
		b := map[string]any{"agent": "claude", "folder": folder, "prompt": "hi"}
		for k, v := range m {
			if v == nil {
				delete(b, k)
			} else {
				b[k] = v
			}
		}
		out, _ := json.Marshal(b)
		return out
	}
	const (
		badAgent  = "agent \"opencode\" is not a saved agent; GET /api/v0/agents lists them"
		noAgent   = "agent must be a saved agent's id; GET /api/v0/agents lists them"
		badFolder = "folder must be an absolute path to an existing directory, at most 4096 bytes"
		badPrompt = "prompt must be from 1 byte to 256 KiB, with no NUL byte"
		badModel  = "model must match ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$"
		badBody   = "the body must be one JSON object with prompt, agent and folder (or session, and optionally agent), and optionally model, read_only, isolate and idempotency_key, at most 2 MiB"
	)
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"unknown agent", ok(map[string]any{"agent": "opencode"}), badAgent},
		{"no agent", ok(map[string]any{"agent": nil}), noAgent},
		{"relative folder", ok(map[string]any{"folder": "x/y"}), badFolder},
		{"missing folder", ok(map[string]any{"folder": filepath.Join(folder, "nope")}), badFolder},
		{"folder is a file", ok(map[string]any{"folder": file}), badFolder},
		{"no folder", ok(map[string]any{"folder": nil}), badFolder},
		{"empty prompt", ok(map[string]any{"prompt": ""}), badPrompt},
		{"prompt over 256 KiB", ok(map[string]any{"prompt": strings.Repeat("a", 256<<10+1)}), badPrompt},
		{"prompt with NUL", ok(map[string]any{"prompt": "a\x00b"}), badPrompt},
		{"model as a flag", ok(map[string]any{"model": "--dangerously-skip-permissions"}), badModel},
		{"model with a space", ok(map[string]any{"model": "haiku --tools Bash"}), badModel},
		{"model with =", ok(map[string]any{"model": "a=b"}), badModel},
		{"unknown field", []byte(`{"agent":"claude","folder":"` + folder + `","prompt":"hi","readonly":false}`), badBody},
		{"not an object", []byte(`["claude"]`), badBody},
		{"trailing data", append(ok(nil), []byte(`{}`)...), badBody},
		{"read_only not a bool", ok(map[string]any{"read_only": "no"}), badBody},
		{"over 2 MiB", []byte(`{"agent":"claude","folder":"` + folder + `","prompt":"` + strings.Repeat("a", 2<<20) + `"}`), badBody},
	} {
		before := svc.store.Last()
		code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", tc.body)
		if code != http.StatusBadRequest || out["error"] != tc.want {
			t.Errorf("%s: %d %.200s, want 400 %q", tc.name, code, raw, tc.want)
		}
		if svc.store.Last() != before {
			t.Errorf("%s: a refused run stored events", tc.name)
		}
	}
	// The prompt shaped as a flag is accepted and stays the prompt (the first
	// test checks where it lands); here only that it is not refused.
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	if code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", ok(map[string]any{"prompt": "-x"})); code != http.StatusAccepted {
		t.Fatalf("a prompt starting with - was refused: %d %s", code, raw)
	} else {
		waitClosed(t, svc.store, out["session"].(string))
	}
}

// A person who already points an agent at their own gateway would have that
// gateway's credentials sent to the vendor. A run refuses, storing nothing.
func TestRunRefusesWhenServiceSendsAgentElsewhere(t *testing.T) {
	for _, h := range harness.All() {
		agent, v := h.Info().Name, h.Info().OwnEndpointEnv
		home := t.TempDir()
		env := map[string]string{v: "https://gateway.example"}
		svc := runService(t, home, func(k string) string { return env[k] }, 0)
		tok := readToken(t, home)
		code, out, raw := postRun(t, svc, tok, map[string]any{"agent": agent, "folder": t.TempDir(), "prompt": "hi"})
		if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "another endpoint") {
			t.Fatalf("%s with %s set: %d %s, want 400", agent, v, code, raw)
		}
		if evs := storedEvents(t, svc.store); len(evs) != 0 {
			t.Fatalf("%s: a refused run stored events: %+v", agent, evs)
		}
	}
}

func TestRunConcurrencyBound(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_WAIT", release)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	body := map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"}
	var ids []string
	for i := 0; i < 4; i++ {
		code, out, raw := postRun(t, svc, tok, body)
		if code != http.StatusAccepted {
			t.Fatalf("run %d: %d %s", i+1, code, raw)
		}
		ids = append(ids, out["session"].(string))
	}
	// A fifth waits in the queue: 202 queued, with its session and its
	// turn's id, and nothing stored for it yet. The turn reads queued.
	before := svc.store.Last()
	code, out, raw := postRun(t, svc, tok, body)
	if code != http.StatusAccepted || out["state"] != "queued" || out["turn"] == "" || out["session"] == "" {
		t.Fatalf("fifth run: %d %s, want 202 queued", code, raw)
	}
	if code, one := sendAPI(t, svc, tok, "GET", "/api/v0/turns/"+out["turn"].(string), nil); code != 200 ||
		one["turn"].(map[string]any)["state"] != "queued" {
		t.Fatalf("the queued turn: %d %v", code, one)
	}
	if svc.store.Last() != before {
		t.Fatal("the queued fifth run stored events")
	}
	os.WriteFile(release, nil, 0o600)
	for _, id := range ids {
		waitClosed(t, svc.store, id)
	}
	// It runs once a slot is free, in the session its answer named.
	waitClosed(t, svc.store, out["session"].(string))
}

func TestServiceStopEndsRuns(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ignoreTerm string
		want       int
	}{
		{"SIGTERM", "", 128 + 15},
		{"SIGKILL after SIGTERM is ignored", "1", 128 + 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			svc := runService(t, home, nil, 300*time.Millisecond)
			tok := readToken(t, home)
			report := filepath.Join(t.TempDir(), "report")
			t.Setenv("AGENTBUS_FAKE_OUT", report)
			t.Setenv("AGENTBUS_FAKE_WAIT", filepath.Join(t.TempDir(), "never"))
			t.Setenv("AGENTBUS_FAKE_IGNORE_TERM", tc.ignoreTerm)
			code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
			if code != http.StatusAccepted {
				t.Fatalf("%d %s", code, raw)
			}
			readReport(t, report) // the child is running and has set up its signals
			stopped := time.Now()
			svc.stop()
			if d := time.Since(stopped); d > 5*time.Second {
				t.Fatalf("stop took %v", d)
			}
			st, err := store.Open(filepath.Join(home, "events.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			c := closedEvent(runEvents(t, st, out["session"].(string)))
			if c == nil || c.ExitCode == nil || *c.ExitCode != tc.want {
				t.Fatalf("session.closed %+v, want exit_code %d", c, tc.want)
			}
		})
	}
}

// Every turn.started of a run's session names the run's agent and folder,
// and GET /api/v0/sessions reads them. A session pointed at the gateway by hand has neither.
func TestRunTurnCarriesAgentAndFolder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-x","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	home := t.TempDir()
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL(upstream.URL),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, getenv: os.Getenv,
		lookPath: func(string) (string, error) { return self, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)

	// Before the run's child reaches a model call, the run's own record
	// already names the agent and folder.
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_WAIT", release)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	folder := t.TempDir()
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": folder, "isolate": false, "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("run %d %s", code, raw)
	}
	waiting := out["session"].(string)
	if s := snapSession(t, svc, tok, waiting); s["agent"] != "claude" || s["folder"] != folder || s["open"] != true {
		t.Fatalf("a running child's session before any model call: %v", s)
	}
	os.WriteFile(release, nil, 0o600)
	waitClosed(t, svc.store, waiting)
	t.Setenv("AGENTBUS_FAKE_WAIT", "")

	// A run whose child makes one model call through the gateway.
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	code, out, raw = postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": folder, "isolate": false, "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("run %d %s", code, raw)
	}
	id := out["session"].(string)
	waitClosed(t, svc.store, id)
	var turn *storedEvent
	waitFor(t, "the run's turn.started", func() bool {
		for _, e := range storedEvents(t, svc.store) {
			if e.Kind == "turn.started" && e.Session == id {
				turn = &e
				return true
			}
		}
		return false
	})
	if turn.Agent != "claude" || turn.Folder != folder {
		t.Fatalf("turn.started %+v, want agent claude and folder %s", *turn, folder)
	}
	if s := snapSession(t, svc, tok, id); s["agent"] != "claude" || s["folder"] != folder || s["open"] != false {
		t.Fatalf("session %v, want claude, the folder, and not open", s)
	}

	// The same call pointed at the gateway by hand: no run, so no agent and
	// no folder, and not open.
	resp, err := http.Post("http://"+svc.addr+"/s/byhand/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"by hand"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	waitFor(t, "the hand-pointed turn.started", func() bool {
		for _, e := range storedEvents(t, svc.store) {
			if e.Kind == "turn.started" && e.Session == "byhand" {
				turn = &e
				return true
			}
		}
		return false
	})
	if turn.Agent != "" || turn.Folder != "" {
		t.Fatalf("a hand-pointed turn.started named %q %q", turn.Agent, turn.Folder)
	}
	if s := snapSession(t, svc, tok, "byhand"); s["agent"] != "" || s["folder"] != "" || s["open"] != false {
		t.Fatalf("hand-pointed session %v, want no agent, no folder, not open", s)
	}
}

// argsOf joins a child's arguments as the tests compare them: the history
// hint, which names the session and is long, becomes HINT, once it is checked
// to name session.
func argsOf(t *testing.T, args []string, session string) string {
	t.Helper()
	out := append([]string(nil), args...)
	// The hint names the session; with the agentbus MCP tools it names no
	// route, so the session's own words are what is checked.
	search := "in session " + session + "."
	for i, a := range out {
		switch {
		case i > 0 && out[i-1] == "--mcp-config":
			if !strings.Contains(a, `"Bearer ${AGENTBUS_TOKEN}"`) || !strings.Contains(a, "/api/v0/mcp") {
				t.Errorf("claude's tool server: %q", a)
			}
			out[i] = "MCP"
		case strings.HasPrefix(a, "mcp_servers.agentbus.url="):
			if !strings.HasSuffix(a, `/api/v0/mcp"`) {
				t.Errorf("codex's tool server: %q", a)
			}
			out[i] = "mcp_servers.agentbus.url=TOOLS"
		case i > 0 && out[i-1] == "--append-system-prompt":
			if !strings.Contains(a, search) {
				t.Errorf("claude's hint does not name session %s: %q", session, a)
			}
			out[i] = "HINT"
		case strings.HasPrefix(a, "developer_instructions="):
			if !strings.Contains(a, search) {
				t.Errorf("codex's hint does not name session %s: %q", session, a)
			}
			out[i] = "developer_instructions=HINT"
		case i > 0 && (out[i-1] == "--allowedTools" || out[i-1] == "--disallowedTools"):
			out[i] = editRulesShort(a)
		}
	}
	return strings.Join(out, " ")
}

// editRulesShort shows a rule list with the edit rule for the turn's folder
// as EDIT and the deny rules on the data folder's files as PROTECT: their
// paths are temporary folders, and run's own tests check them exactly.
func editRulesShort(list string) string {
	var out []string
	for _, r := range strings.Split(list, ",") {
		short := r
		switch {
		case strings.HasPrefix(r, "Edit(//") && strings.HasSuffix(r, "/**)"):
			short = "EDIT"
		case strings.HasPrefix(r, "Edit(//"):
			short = "PROTECT"
		}
		if len(out) > 0 && out[len(out)-1] == short && (short == "EDIT" || short == "PROTECT") {
			continue
		}
		out = append(out, short)
	}
	return strings.Join(out, ",")
}

// A developer_instructions the person set for codex wins: the turn gets no
// hint, and claude is unaffected.
func TestCodexOwnInstructionsLeaveOutTheHint(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte("developer_instructions = \"mine\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	for agent, want := range map[string]bool{"codex": false, "claude": true} {
		report := filepath.Join(t.TempDir(), "report")
		t.Setenv("AGENTBUS_FAKE_OUT", report)
		code, out, raw := postRun(t, svc, tok, map[string]any{"agent": agent, "folder": t.TempDir(), "prompt": "hi"})
		if code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", agent, code, raw)
		}
		got := argsOf(t, readReport(t, report).Args, out["session"].(string))
		if strings.Contains(got, "HINT") != want {
			t.Errorf("%s: args %q, want the hint %v", agent, got, want)
		}
		waitClosed(t, svc.store, out["session"].(string))
	}
}
