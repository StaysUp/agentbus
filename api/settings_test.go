package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

const settingsTurnsToken = "turns-THIS-MUST-NEVER-APPEAR-0123456789"

// settingsEnv is an API whose settings are the files of a data folder.
func settingsEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, TurnsToken: settingsTurnsToken, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Settings: settings.NewFiles(dir)})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port}, dir
}

// send is one settings call with the main token, and If-Match when match is
// not empty.
func send(t *testing.T, e *env, method, path, match, body string) (*http.Response, string) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = e.req(t, method, path, strings.NewReader(body))
	} else {
		r = e.req(t, method, path, nil)
	}
	if match != "" {
		r.Header.Set("If-Match", match)
	}
	resp, out := do(t, r)
	noToken(t, resp, out)
	return resp, out
}

// A person reads the policy, changes it with the version they read, and the
// file holds the change in the starter's layout. A write with an old version
// is refused, so a hand edit in between is never undone, and the hand edit
// shows on the next read.
func TestSettingsPolicyReadWrite(t *testing.T) {
	e, dir := settingsEnv(t)
	path := filepath.Join(dir, settings.PolicyFile)

	// No file: every default, and a version for "no file".
	resp, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if resp.StatusCode != 200 || !strings.Contains(out, `"edit":"session"`) || !strings.Contains(out, `"read_folders":[]`) {
		t.Fatalf("GET with no file: %d %s", resp.StatusCode, out)
	}
	v0 := resp.Header.Get("ETag")
	if v0 == "" || resp.Header.Get("Access-Control-Expose-Headers") != "ETag" {
		t.Fatal("no ETag, or not exposed to a page on another origin")
	}
	// A cockpit on another origin may send If-Match.
	pre, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v0/settings/permissions", nil)
	pre.Header.Set("Origin", "http://localhost:5173")
	pre.Header.Set("Access-Control-Request-Method", "PUT")
	if resp, _ := do(t, pre); !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "If-Match") {
		t.Fatalf("preflight allows %q", resp.Header.Get("Access-Control-Allow-Headers"))
	}

	// Without If-Match: 428.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", "", `{"forbid":["git push"]}`); resp.StatusCode != 428 {
		t.Fatalf("PUT without If-Match: %d %s", resp.StatusCode, out)
	}
	// A value out of range names its key.
	for _, tc := range []struct{ body, want string }{
		{`{"edit":"everywhere"}`, "edit must be"},
		{`{"limits":{"max_depth":-1}}`, "max_depth must be 0 or more"},
		{`{"forbid":"git push"}`, "forbid has the wrong type"},
		{`{"allow":["go test:*"]}`, `allow entry "go test:*" has a star`},
		{`{"read_folders":["notes"]}`, `read_folders entry "notes" must be an absolute path`},
		{`{"agent_api":{"scope":"all"}}`, "scope must be"},
		{`{"allowed":[]}`, `unknown key "allowed"`},
		{`[]`, "one JSON object"},
		{`null`, "one JSON object"},
		{``, "one JSON object"},
	} {
		resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", v0, tc.body)
		var refused struct{ Error string }
		_ = json.Unmarshal([]byte(out), &refused)
		if resp.StatusCode != 400 || !strings.Contains(refused.Error, tc.want) {
			t.Errorf("PUT %s: %d %s, want 400 with %q", tc.body, resp.StatusCode, out, tc.want)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused PUT wrote the file")
	}

	// A good write: the reply is the policy as stored, with a new version.
	resp, out = send(t, e, "PUT", "/api/v0/settings/permissions", v0, `{"read_folders":["~/notes"],"forbid":["git push"],"limits":{"max_depth":1}}`)
	if resp.StatusCode != 200 || !strings.Contains(out, `"forbid":["git push"]`) || !strings.Contains(out, `"max_depth":1`) ||
		!strings.Contains(out, `"max_jobs_per_turn":10`) || !strings.Contains(out, `"read_folders":["~/notes"]`) {
		t.Fatalf("PUT: %d %s", resp.StatusCode, out)
	}
	v1 := resp.Header.Get("ETag")
	if v1 == "" || v1 == v0 {
		t.Fatalf("ETag after a write %q, before %q", v1, v0)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `forbid = ["git push"]`) || !strings.Contains(string(b), "max_depth = 1 ") ||
		!strings.Contains(string(b), "# What AgentBus turns may do without asking.") {
		t.Errorf("the file:\n%s", b)
	}
	if resp, _ := send(t, e, "GET", "/api/v0/settings/permissions", "", ""); resp.Header.Get("ETag") != v1 {
		t.Errorf("GET after PUT: ETag %q, want %q", resp.Header.Get("ETag"), v1)
	}

	// The old version is refused: 412.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", v0, `{}`); resp.StatusCode != 412 {
		t.Fatalf("PUT with a stale version: %d %s", resp.StatusCode, out)
	}
	// A hand edit changes the version, so the version read before it is
	// refused, and the edit is what the next GET reads.
	if err := os.WriteFile(path, []byte("forbid = [\"rm\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", v1, `{}`); resp.StatusCode != 412 {
		t.Fatalf("PUT over a hand edit: %d %s", resp.StatusCode, out)
	}
	resp, out = send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if resp.StatusCode != 200 || !strings.Contains(out, `"forbid":["rm"]`) || resp.Header.Get("ETag") == v1 {
		t.Fatalf("GET after a hand edit: %d %s %s", resp.StatusCode, resp.Header.Get("ETag"), out)
	}
	// * replaces whatever is there.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", "*", `{"allow":["go test"]}`); resp.StatusCode != 200 || !strings.Contains(out, `"allow":["go test"]`) {
		t.Fatalf("PUT with *: %d %s", resp.StatusCode, out)
	}

	// A broken file: 503 with what is wrong, and its version, so a PUT can
	// replace it.
	if err := os.WriteFile(path, []byte("allow = go test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, out = send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if resp.StatusCode != 503 || !strings.Contains(out, "line 1") || resp.Header.Get("ETag") == "" {
		t.Fatalf("GET of a broken file: %d %s", resp.StatusCode, out)
	}
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", resp.Header.Get("ETag"), `{}`); resp.StatusCode != 200 {
		t.Fatalf("PUT over a broken file: %d %s", resp.StatusCode, out)
	}
}

// The instructions: a missing file reads as the default text; a write takes
// the version; text a turn cannot carry is refused.
func TestSettingsInstructionsReadWrite(t *testing.T) {
	e, dir := settingsEnv(t)
	resp, out := send(t, e, "GET", "/api/v0/settings/instructions", "", "")
	var got InstructionsBody
	if resp.StatusCode != 200 || json.Unmarshal([]byte(out), &got) != nil || got.Text != settings.DefaultInstructions {
		t.Fatalf("GET with no file: %d %s", resp.StatusCode, out)
	}
	v0 := resp.Header.Get("ETag")
	if resp, out := send(t, e, "PUT", "/api/v0/settings/instructions", v0, `{"text":"a\u0000b"}`); resp.StatusCode != 400 || !strings.Contains(out, "NUL") {
		t.Errorf("PUT with a NUL: %d %s", resp.StatusCode, out)
	}
	if resp, out := send(t, e, "PUT", "/api/v0/settings/instructions", v0, `{"text":"`+strings.Repeat("a", settings.MaxInstructions+1)+`"}`); resp.StatusCode != 400 || !strings.Contains(out, "64 KiB") {
		t.Errorf("PUT over 64 KiB: %d %.80s", resp.StatusCode, out)
	}
	resp, out = send(t, e, "PUT", "/api/v0/settings/instructions", v0, `{"text":"Be brief. Session {session}.\n"}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "Be brief") {
		t.Fatalf("PUT: %d %s", resp.StatusCode, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, settings.InstructionsFile)); string(b) != "Be brief. Session {session}.\n" {
		t.Errorf("the file: %q", b)
	}
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/instructions", v0, `{"text":"x"}`); resp.StatusCode != 412 {
		t.Errorf("PUT with a stale version: %d", resp.StatusCode)
	}
}

// The turns token, which may only start, stop and read turns, never reads or
// writes settings. (A turn's own token: TestAskPolicyFromPermissions.)
func TestSettingsTurnsTokenRefused(t *testing.T) {
	e, dir := settingsEnv(t)
	for _, r := range [][2]string{
		{"GET", "/api/v0/settings/permissions"}, {"PUT", "/api/v0/settings/permissions"},
		{"GET", "/api/v0/settings/instructions"}, {"PUT", "/api/v0/settings/instructions"},
	} {
		req := e.req(t, r[0], r[1], strings.NewReader(`{"text":"x","forbid":[]}`))
		req.Header.Set("Authorization", "Bearer "+settingsTurnsToken)
		req.Header.Set("If-Match", "*")
		resp, out := do(t, req)
		if resp.StatusCode != 403 || strings.Contains(out, settingsTurnsToken) {
			t.Errorf("%s %s with the turns token: %d %s", r[0], r[1], resp.StatusCode, out)
		}
	}
	for _, f := range []string{settings.PolicyFile, settings.InstructionsFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s was written", f)
		}
	}
}
