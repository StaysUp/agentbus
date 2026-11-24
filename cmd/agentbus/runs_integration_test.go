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
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
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
	// agentbus up (the personal runner) now runs sessions in the person's real
	// home, so give the tests a clean temp HOME and clear CLAUDE_CONFIG_DIR
	// (claude.OwnServers reads it before HOME): the machine's own ~/.claude
	// (its MCP servers, config) must never change a test.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// The service's own environment must not send an agent elsewhere.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	connectFake(t, home)
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

// oldToolDefaults sets, in home's permissions, the two keys whose defaults
// 0.5.2 changed to what they were before (subagents on, tools off), for a
// test that pins a child's arguments for something else: the launch golden
// pins the defaults themselves.
func oldToolDefaults(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, settings.PolicyFile), []byte("subagents = true\n\n[agent_api]\ntools = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type runEvent struct {
	Kind      string `json:"kind"`
	Session   string `json:"session"`
	Agent     string `json:"agent"`
	Folder    string `json:"folder"`
	Hold      string `json:"hold"`
	ExitCode  *int   `json:"exit_code"`
	ResumeID  string `json:"resume_id"`
	Stopped   bool   `json:"stopped"`
	ErrorTail string `json:"error_tail"`
	State     string `json:"state"`
	Error     string `json:"error"`
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
		if evs[i].Kind == "run.ended" {
			return &evs[i]
		}
	}
	return nil
}

func waitClosed(t *testing.T, st *store.Store, session string) runEvent {
	t.Helper()
	var c *runEvent
	waitFor(t, "run.ended", func() bool { c = closedEvent(runEvents(t, st, session)); return c != nil })
	return *c
}

// waitEnded waits for turn's job to end and answers it. A turn is done when
// its job is: the job ends only once its outcome (turn.changed) is in the
// record, so a test that waits here reads the whole turn at once. Its
// run.ended, in the record a moment before, is not that point.
func waitEnded(t *testing.T, svc *service, turn string) queue.Job {
	t.Helper()
	var j queue.Job
	waitFor(t, "the turn's job ended", func() bool {
		var err error
		j, err = svc.queue.JobByTurn(queue.DefaultTeam, turn)
		return err == nil && j.Ended != nil
	})
	return j
}

// waitIdle waits until none of session's jobs is open: each has ended, so
// each one's outcome is in the record and nothing more is written for them.
func waitIdle(t *testing.T, svc *service, session string) {
	t.Helper()
	waitFor(t, "the session's jobs ended", func() bool {
		js, err := svc.queue.List(queue.DefaultTeam, queue.Filter{Session: session, Open: true}, 10)
		return err == nil && len(js) == 0
	})
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
	oldToolDefaults(t, home)
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

	waitEnded(t, svc, turn)
	c := waitClosed(t, svc.store, id)
	if c.ExitCode == nil || *c.ExitCode != 3 || c.Hold == "" {
		t.Fatalf("run.ended %+v, want exit_code 3 and a hold", c)
	}
	// Nothing writes session.registered any more: run.started carries
	// the agent and folder, and the run stores its turn's turn.started.
	// The turn's job ended, so what it changed (turn.changed) is in too.
	evs := runEvents(t, svc.store, id)
	kinds := make([]string, len(evs))
	for i, e := range evs {
		kinds[i] = e.Kind
	}
	if got := strings.Join(kinds, " "); got != "session.created run.started turn.started context.added run.ended turn.changed" || evs[1].Hold != c.Hold {
		t.Fatalf("events %s: %+v", got, evs)
	}
	if evs[1].Agent != "claude" || evs[1].Folder != folder {
		t.Fatalf("run.started %+v, want agent claude and the folder", evs[1])
	}

	r := readReport(t, report)
	// The gateway address reaches the child through --settings, not a process
	// env var: a user's own settings.json "env" overrides the env var but not
	// --settings, so recording and review cannot be bypassed
	// (VERIFY_copy_level.md check 2).
	if u, _ := envValue(r.Env, "ANTHROPIC_BASE_URL"); u == base {
		t.Fatalf("child ANTHROPIC_BASE_URL env is the gateway %q, want it only in --settings", u)
	}
	if got := os.Getenv("ANTHROPIC_BASE_URL"); got != "" {
		t.Fatalf("the service's own ANTHROPIC_BASE_URL changed to %q", got)
	}
	if s := settingsArg(r.Args); !strings.Contains(s, `"ANTHROPIC_BASE_URL":"`+base+`"`) {
		t.Fatalf("--settings %q, want it to carry the gateway %q", s, base)
	}
	// The prompt comes last, after --, so a prompt shaped like a flag stays a prompt.
	want := "--settings SETTINGS -p --output-format json --append-system-prompt HINT --allowedTools EDIT,Bash --no-chrome --disallowedTools PROTECT --model haiku -- --dangerously-skip-permissions hello"
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
	// Control's, which its model calls carry to up's relay too.
	if v, _ := envValue(r.Env, "AGENTBUS_TOKEN"); !strings.HasPrefix(v, jobs.TokenPrefix) || v == tok {
		t.Errorf("child AGENTBUS_TOKEN is not a turn token (%d bytes)", len(v))
	}
}

// An agent that exits with an error leaves its last stderr lines on its
// run.ended, with state failed and its own error line as the error, and
// the turn's error says that line: the record says why it failed, not only
// that it did. One that exits 0 leaves none, with state finished. The fake
// agent makes no model call, so the turn fails before any.
func TestRunErrorTailOnRunEnded(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_STDERR", "starting\nError: Eligibility check failed: request failed with status 401\n")
	for _, exit := range []string{"1", "0"} {
		t.Setenv("AGENTBUS_FAKE_EXIT", exit)
		code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "isolate": false, "prompt": "hi"})
		if code != http.StatusAccepted {
			t.Fatalf("status %d: %s", code, raw)
		}
		id, _ := out["session"].(string)
		c := waitClosed(t, svc.store, id)
		want, wantErr, wantState := "", "", store.TurnFinished
		if exit == "1" {
			want = "starting\nError: Eligibility check failed: request failed with status 401"
			wantErr, wantState = "Error: Eligibility check failed: request failed with status 401", store.TurnFailed
		}
		if c.ErrorTail != want || c.State != wantState || c.Error != wantErr {
			t.Fatalf("exit %s: run.ended %+v, want error_tail %q, state %s, error %q", exit, c, want, wantState, wantErr)
		}
		list, err := svc.store.Turns(store.TurnQuery{Session: id, Limit: 1})
		if err != nil || len(list.Turns) != 1 {
			t.Fatalf("turns: %+v %v", list, err)
		}
		if tr := list.Turns[0]; tr.State != wantState || tr.ErrorTail != want || tr.Error != wantErr {
			t.Fatalf("turn %+v", tr)
		}
	}
}

// agy 1.5.9 refusing a --model it does not know prints its error and then
// its 22-line list of models, all to stderr, and exits 1 before any model
// call (live, 2026-11-14). The error is that first line, not the list's last
// item, though the 20-line tail no longer holds it.
func TestRunErrorIsTheErrorLineNotTheList(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	errLine := `error: invalid model selection (--model "gemini-2.5-flash-preview-tts" --effort ""): model gemini-2.5-flash-preview-tts is not recognized as a known model or custom model in settings`
	stderr := errLine + "\nAvailable models:\n"
	for _, m := range []string{"Gemini 4 Carbon (High)", "Gemini 4 Carbon (Medium)", "Gemini 4 Carbon (Low)", "Gemini 4 Hydrogen (High)",
		"Gemini 4 Hydrogen (XHigh)", "Gemini 4 Hydrogen (Medium)", "Gemini 4 Hydrogen (Low)", "Gemini 4 Argon (High)", "Gemini 4 Argon (Medium)",
		"Gemini 4 Argon (Low)", "Gemini 3.8 Flash (High)", "Gemini 3.8 Flash (Medium)", "Gemini 3.8 Flash (Low)", "Gemini 3.7 Flash (High)",
		"Gemini 3.7 Flash (Medium)", "Gemini 3.7 Flash (Low)", "Gemini 3.6 Flash (High)", "Gemini 3.6 Flash (Medium)", "Gemini 3.6 Flash (Low)",
		"Gemini 3.1 Pro (High)", "Gemini 3.1 Pro (Low)"} {
		stderr += "  " + m + "\n"
	}
	t.Setenv("AGENTBUS_FAKE_STDERR", stderr)
	t.Setenv("AGENTBUS_FAKE_EXIT", "1")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "isolate": false, "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("status %d: %s", code, raw)
	}
	id, _ := out["session"].(string)
	c := waitClosed(t, svc.store, id)
	if c.State != store.TurnFailed || c.Error != errLine || strings.Contains(c.ErrorTail, "invalid model") {
		t.Fatalf("run.ended %+v", c)
	}
	list, err := svc.store.Turns(store.TurnQuery{Session: id, Limit: 1})
	if err != nil || len(list.Turns) != 1 || list.Turns[0].Error != errLine || list.Turns[0].ModelCalls != 0 {
		t.Fatalf("turns: %+v %v", list, err)
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
		t.Fatal("run.ended while the child runs")
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
		t.Fatalf("run.ended %+v, want exit_code 0", c)
	}
}

// Read-only turns retired in 0.6 (B12): an old client may still send read_only
// (the retired step accepts it), but it is ignored — the turn gets the normal
// args, never a restricted tool set. true, false and absent are all the same.
func TestRunReadOnlyRetired(t *testing.T) {
	home := t.TempDir()
	oldToolDefaults(t, home)
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	for _, tc := range []struct {
		agent string
		want  string // with BASE for the session's base URL
	}{
		{"claude", "--settings SETTINGS -p --output-format json --append-system-prompt HINT --allowedTools EDIT,Bash --no-chrome --disallowedTools PROTECT -- hi"},
		{"codex", codexRelayArgs + ` exec --skip-git-repo-check --json --sandbox workspace-write -c developer_instructions=HINT -c sandbox_workspace_write.network_access=true --disable apps --disable plugins -- hi`},
	} {
		for _, readOnly := range []any{nil, true, false} {
			report := filepath.Join(t.TempDir(), "report")
			t.Setenv("AGENTBUS_FAKE_OUT", report)
			body := map[string]any{"agent": tc.agent, "folder": t.TempDir(), "prompt": "hi"}
			if readOnly != nil {
				body["read_only"] = readOnly
			}
			code, out, raw := postRun(t, svc, tok, body)
			if code != http.StatusAccepted {
				t.Fatalf("%s read_only %v: status %d: %s", tc.agent, readOnly, code, raw)
			}
			want := strings.ReplaceAll(tc.want, "BASE", out["base_url"].(string))
			rep := readReport(t, report)
			if got := argsOf(t, rep.Args, out["session"].(string)); got != want {
				t.Errorf("%s read_only %v: args %q, want %q", tc.agent, readOnly, got, want)
			}
			if v, _ := envValue(rep.Env, "AGENTBUS_TOKEN"); !strings.HasPrefix(v, jobs.TokenPrefix) || v == tok {
				t.Errorf("%s read_only %v: no turn token in the child's environment", tc.agent, readOnly)
			}
			waitClosed(t, svc.store, out["session"].(string))
		}
	}
}

func TestRunRefusals(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	folder := t.TempDir()
	file := filepath.Join(folder, "f")
	os.WriteFile(file, nil, 0o600)

	// The folder is the workspace's, so folder refusals belong to POST
	// /api/v0/workspaces, before any turn, and store nothing.
	const (
		badFolder    = "folder must be an absolute path to an existing directory, at most 4096 bytes"
		badWorkspace = "the body must be one JSON object with folder (an absolute path) or folder_ref, and optionally machine, sandbox and idempotency_key"
	)
	for _, tc := range []struct{ name, body, want string }{
		{"relative folder", `{"folder":"x/y"}`, badFolder},
		{"missing folder", `{"folder":` + jsonStr(filepath.Join(folder, "nope")) + `}`, "the folder does not exist on the chosen machine"},
		{"folder is a file", `{"folder":` + jsonStr(file) + `}`, "the folder is a file, not a directory"},
		{"neither folder nor folder_ref", `{}`, "send folder or folder_ref, not both and not neither"},
		{"not an object", `["x"]`, badWorkspace},
		{"unknown field", `{"folder":` + jsonStr(folder) + `,"bogus":1}`, badWorkspace},
	} {
		before := svc.store.Last()
		code, out, raw := postAPI(t, svc, tok, "/api/v0/workspaces", []byte(tc.body))
		if code != http.StatusBadRequest || out["error"] != tc.want {
			t.Errorf("workspace %s: %d %.200s, want 400 %q", tc.name, code, raw, tc.want)
		}
		if evs, err := svc.store.After(before, store.Filter{}, 10); err != nil {
			t.Fatal(err)
		} else if len(evs) != 0 {
			t.Errorf("workspace %s: a refused workspace stored events", tc.name)
		}
	}

	// Turn refusals need a session; its workspace holds the (valid) folder.
	sess := newWorkspaceSession(t, svc, tok, folder, true)
	turn := func(m map[string]any) []byte {
		b := map[string]any{"session": sess, "agent": "claude", "prompt": "hi"}
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
		noAgent   = "agent is required: name the saved agent this turn runs on (GET /api/v0/agents lists them)"
		badPrompt = "prompt must be from 1 byte to 256 KiB, with no NUL byte"
		badModel  = "model must match ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$"
		badBody   = "the body must be one JSON object with session, agent and prompt, and optionally model, delivery and idempotency_key, at most 2 MiB"
	)
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"unknown agent", turn(map[string]any{"agent": "opencode"}), badAgent},
		{"no agent", turn(map[string]any{"agent": nil}), noAgent},
		{"empty prompt", turn(map[string]any{"prompt": ""}), badPrompt},
		{"prompt over 256 KiB", turn(map[string]any{"prompt": strings.Repeat("a", 256<<10+1)}), badPrompt},
		{"prompt with NUL", turn(map[string]any{"prompt": "a\x00b"}), badPrompt},
		{"model as a flag", turn(map[string]any{"model": "--dangerously-skip-permissions"}), badModel},
		{"model with a space", turn(map[string]any{"model": "haiku --tools Bash"}), badModel},
		{"model with =", turn(map[string]any{"model": "a=b"}), badModel},
		// A turn carries no folder; it is an unknown field.
		{"a folder", turn(map[string]any{"folder": folder}), badBody},
		{"unknown field", []byte(`{"session":"` + sess + `","agent":"claude","prompt":"hi","readonly":false}`), badBody},
		{"not an object", []byte(`["claude"]`), badBody},
		{"trailing data", append(turn(nil), []byte(`{}`)...), badBody},
		{"over 2 MiB", []byte(`{"session":"` + sess + `","agent":"claude","prompt":"` + strings.Repeat("a", 2<<20) + `"}`), badBody},
	} {
		before := svc.store.Last()
		code, out, raw := postAPI(t, svc, tok, "/api/v0/turns", tc.body)
		if code != http.StatusBadRequest || out["error"] != tc.want {
			t.Errorf("turn %s: %d %.200s, want 400 %q", tc.name, code, raw, tc.want)
		}
		if evs, err := svc.store.After(before, store.Filter{}, 10); err != nil {
			t.Fatal(err)
		} else {
			for _, e := range evs {
				if e.Kind != "turn.ended" { // control's session-less reactor trigger
					t.Errorf("%s: a refused turn stored events", tc.name)
				}
			}
		}
	}
	// The prompt shaped as a flag is accepted and stays the prompt (the first
	// test checks where it lands); here only that it is not refused.
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	if code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "-x"}); code != http.StatusAccepted {
		t.Fatalf("a prompt starting with - was refused: %d %s", code, raw)
	} else {
		waitClosed(t, svc.store, out["session"].(string))
	}
}

// jsonStr quotes s as a JSON string (with its surrounding quotes).
func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// A person who already points an agent at their own gateway would have that
// gateway's credentials sent to the vendor. A run refuses, storing nothing.
func TestRunRefusesWhenServiceSendsAgentElsewhere(t *testing.T) {
	for _, h := range harness.All() {
		agent, v := h.Info().Name, h.Info().OwnEndpointEnv
		home := t.TempDir()
		env := map[string]string{v: "https://gateway.example"}
		svc := runService(t, home, func(k string) string { return env[k] }, 0)
		tok := readToken(t, home)
		// A harness with no subscription of its own (opencode, gemini) has
		// no default agent: its turn runs as a saved agent on an access it
		// offers.
		if !harness.Offers(h.Info(), harness.Subscription) {
			body := map[string]any{"name": agent, "harness": agent, "access": "openrouter", "model": "z-ai/glm-5.3-flash"}
			if !harness.Offers(h.Info(), harness.OpenRouter) {
				body = map[string]any{"name": agent, "harness": agent, "access": "api"}
			}
			code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", body)
			if code != http.StatusCreated {
				t.Fatalf("%s: create: %d %v", agent, code, ag)
			}
			agent = ag["id"].(string)
		}
		code, out, raw := postRun(t, svc, tok, map[string]any{"agent": agent, "folder": t.TempDir(), "prompt": "hi"})
		if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "another endpoint") {
			t.Fatalf("%s with %s set: %d %s, want 400", agent, v, code, raw)
		}
		// The refused turn stores nothing in its session's record; only the
		// session.created of the session it names remains.
		if evs := storedEvents(t, svc.store); len(evs) != 1 || evs[0].Kind != "session.created" {
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
	// turn's id, and nothing stored for it yet. The turn reads queued. Its
	// workspace and session are made first, so only the turn is measured.
	fifth := newWorkspaceSession(t, svc, tok, t.TempDir(), true)
	before := svc.store.Last()
	code, out, raw := postRun(t, svc, tok, map[string]any{"session": fifth, "agent": "claude", "prompt": "hi"})
	if code != http.StatusAccepted || out["state"] != "queued" || out["turn"] == "" || out["session"] == "" {
		t.Fatalf("fifth run: %d %s, want 202 queued", code, raw)
	}
	if code, one := sendAPI(t, svc, tok, "GET", "/api/v0/turns/"+out["turn"].(string), nil); code != 200 ||
		one["turn"].(map[string]any)["state"] != "queued" {
		t.Fatalf("the queued turn: %d %v", code, one)
	}
	if evs, err := svc.store.After(before, store.Filter{}, 10); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range evs {
			if e.Kind != "turn.ended" { // control's session-less reactor trigger
				t.Fatal("the queued fifth run stored events")
			}
		}
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
				t.Fatalf("run.ended %+v, want exit_code %d", c, tc.want)
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
	connectFake(t, home)
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

	// The same call pointed at the gateway by hand, with no turn's token:
	// in agentbus up a call's turn comes from its token, so it is refused
	// and recorded nowhere.
	resp, err := http.Post("http://"+svc.addr+"/s/byhand/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"by hand"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a hand-pointed call: %d, want 401", resp.StatusCode)
	}
	for _, e := range storedEvents(t, svc.store) {
		if e.Session == "byhand" {
			t.Fatalf("a hand-pointed call was recorded: %+v", e)
		}
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
		case i > 0 && out[i-1] == "--settings":
			// The gateway URL and the per-turn stand-in travel here; both are
			// dynamic. TestRunSetsBaseURLInChildOnlyAndClosesWithExitCode
			// checks them directly.
			out[i] = "SETTINGS"
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

// settingsArg returns the value of the child's --settings argument, or "".
func settingsArg(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--settings" {
			return args[i+1]
		}
	}
	return ""
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
	// agentbus up (the personal runner) runs in the person's real home, so the
	// person's own codex config IS read: its developer_instructions leaves the
	// hint out (codex.OwnInstructions), as it would in their terminal. claude
	// has no such config here, so it still gets the hint.
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

// A sandbox (Copy) workspace's FIRST turn runs through the per-command sandbox,
// though the workspace's queue row is written only when a turn finishes: the
// runner reads the prepared kind, not that row. Without that, the first and most
// important turn would run unsandboxed (agy review evt_3daaa8a056a9d8ec, P1.1).
func TestFirstCopyTurnIsSandboxed(t *testing.T) {
	home := t.TempDir()
	oldToolDefaults(t, home)
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	// No isolate: it defaults true, so this first turn runs in a sandbox copy.
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["session"].(string))
	s := settingsArg(readReport(t, report).Args)
	if !strings.Contains(s, `"sandbox":`) || !strings.Contains(s, `"allowUnsandboxedCommands":false`) {
		t.Fatalf("the first Copy turn is not sandboxed: --settings %q", s)
	}
	// A Direct turn (isolate false) is not sandboxed: no sandbox block.
	direct := filepath.Join(t.TempDir(), "direct")
	t.Setenv("AGENTBUS_FAKE_OUT", direct)
	code, out, raw = postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "isolate": false, "prompt": "one"})
	if code != http.StatusAccepted {
		t.Fatalf("direct run: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["session"].(string))
	if d := settingsArg(readReport(t, direct).Args); strings.Contains(d, `"sandbox":`) {
		t.Fatalf("a Direct turn carries a sandbox block: --settings %q", d)
	}
}
