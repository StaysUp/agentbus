package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

const settingsTurnsToken = "turns-THIS-MUST-NEVER-APPEAR-0123456789"

// settingsEnv is an API whose settings are a settings database, its changes
// recorded in the store as control records them.
func settingsEnv(t *testing.T) (*env, *settings.DB) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := settings.OpenDB(filepath.Join(dir, settings.DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	db.SetSink(RecordSettings(st), t.Logf)
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, TurnsToken: settingsTurnsToken, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Settings: db})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		db.Close()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port}, db
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

// settingsEvents are the settings.changed events in the record.
func settingsEvents(t *testing.T, st *store.Store) []event.Event {
	t.Helper()
	recs, err := st.After(0, store.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Event
	for _, r := range recs {
		var ev event.Event
		if err := json.Unmarshal(r.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == event.SettingsChanged {
			out = append(out, ev)
		}
	}
	return out
}

// A person reads the policy, changes it with the version they read, and the
// next read has the change. A write with an old version is refused, so a
// change by someone else in between is never undone. Each change is in the
// record by the keys it changed, never their values.
func TestSettingsPolicyReadWrite(t *testing.T) {
	e, _ := settingsEnv(t)

	// Nothing changed yet: every default.
	resp, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if resp.StatusCode != 200 || !strings.Contains(out, `"edit":"session"`) || !strings.Contains(out, `"read_folders":[]`) {
		t.Fatalf("GET with nothing changed: %d %s", resp.StatusCode, out)
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
	if evs := settingsEvents(t, e.store); len(evs) != 0 {
		t.Fatalf("a refused PUT made %d events", len(evs))
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
	if resp, _ := send(t, e, "GET", "/api/v0/settings/permissions", "", ""); resp.Header.Get("ETag") != v1 {
		t.Errorf("GET after PUT: ETag %q, want %q", resp.Header.Get("ETag"), v1)
	}
	// The change is in the record when the reply comes: keys, not values.
	evs := settingsEvents(t, e.store)
	if len(evs) != 1 || evs[0].Setting != "permissions" || strings.Join(evs[0].Keys, ",") != "forbid,limits.max_depth,read_folders" ||
		evs[0].SettingsVersion != 1 || evs[0].Session != "" {
		t.Fatalf("events %+v", evs)
	}
	recs, _ := e.store.After(0, store.Filter{}, 100)
	for _, r := range recs {
		if strings.Contains(string(r.Data), "git push") || strings.Contains(string(r.Data), "~/notes") {
			t.Fatalf("a value is in the record: %s", r.Data)
		}
	}

	// The old version is refused: 412.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", v0, `{}`); resp.StatusCode != 412 {
		t.Fatalf("PUT with a stale version: %d %s", resp.StatusCode, out)
	}
	// The same policy again changes nothing: no event, no new version.
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/permissions", v1, `{"read_folders":["~/notes"],"forbid":["git push"],"limits":{"max_depth":1}}`); resp.StatusCode != 200 || resp.Header.Get("ETag") != v1 {
		t.Fatalf("the same PUT: %d %s", resp.StatusCode, resp.Header.Get("ETag"))
	}
	if evs := settingsEvents(t, e.store); len(evs) != 1 {
		t.Fatalf("%d events after a PUT that changed nothing", len(evs))
	}
	// * replaces whatever is there.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", "*", `{"allow":["go test"]}`); resp.StatusCode != 200 || !strings.Contains(out, `"allow":["go test"]`) {
		t.Fatalf("PUT with *: %d %s", resp.StatusCode, out)
	}
}

// The instructions: the default text until changed; a write takes the
// version; text a turn cannot carry is refused; the record names the change,
// never the text.
func TestSettingsInstructionsReadWrite(t *testing.T) {
	e, _ := settingsEnv(t)
	resp, out := send(t, e, "GET", "/api/v0/settings/instructions", "", "")
	var got InstructionsBody
	if resp.StatusCode != 200 || json.Unmarshal([]byte(out), &got) != nil || got.Text != settings.DefaultInstructions {
		t.Fatalf("GET with nothing changed: %d %s", resp.StatusCode, out)
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
	if resp, out := send(t, e, "GET", "/api/v0/settings/instructions", "", ""); !strings.Contains(out, "Be brief") || resp.Header.Get("ETag") == v0 {
		t.Errorf("GET after PUT: %s", out)
	}
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/instructions", v0, `{"text":"x"}`); resp.StatusCode != 412 {
		t.Errorf("PUT with a stale version: %d", resp.StatusCode)
	}
	recs, _ := e.store.After(0, store.Filter{}, 10)
	if len(recs) != 1 || !strings.Contains(string(recs[0].Data), `"kind":"settings.changed"`) ||
		!strings.Contains(string(recs[0].Data), `"setting":"instructions"`) || strings.Contains(string(recs[0].Data), "Be brief") {
		t.Fatalf("%d events: %v", len(recs), recs)
	}
}

// GET /api/v0/settings is the settings' state; its ETag is the settings
// version, which a change of the defaults version names in If-Match.
func TestSettingsStateAndDefaults(t *testing.T) {
	e, _ := settingsEnv(t)
	resp, out := send(t, e, "GET", "/api/v0/settings", "", "")
	var in SettingsInfo
	if resp.StatusCode != 200 || json.Unmarshal([]byte(out), &in) != nil || in.Version != 0 || in.Defaults != settings.LatestDefaults() ||
		in.LatestDefaults != settings.LatestDefaults() || in.ImportedFiles == nil || resp.Header.Get("ETag") != `"0"` {
		t.Fatalf("GET: %d %s %s", resp.StatusCode, resp.Header.Get("ETag"), out)
	}
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/instructions", "*", `{"text":"x"}`); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := send(t, e, "GET", "/api/v0/settings", "", ""); resp.Header.Get("ETag") != `"1"` {
		t.Fatalf("ETag after a change %q", resp.Header.Get("ETag"))
	}
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/defaults", `"0"`, `{"version":1}`); resp.StatusCode != 412 {
		t.Errorf("a stale version: %d", resp.StatusCode)
	}
	if resp, out := send(t, e, "PUT", "/api/v0/settings/defaults", `"1"`, `{"version":99}`); resp.StatusCode != 400 || !strings.Contains(out, "version must be") {
		t.Errorf("no such version: %d %s", resp.StatusCode, out)
	}
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/defaults", "", `{"version":1}`); resp.StatusCode != 428 {
		t.Errorf("no If-Match: %d", resp.StatusCode)
	}
	// The version it runs under already: nothing changes.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/defaults", `"1"`, `{"version":1}`); resp.StatusCode != 200 || resp.Header.Get("ETag") != `"1"` {
		t.Errorf("the same version: %d %s", resp.StatusCode, out)
	}
}

// The turns token, which may only start, stop, review and read turns, never reads or
// writes settings. (A turn's own token: TestAskPolicyFromPermissions.)
func TestSettingsTurnsTokenRefused(t *testing.T) {
	e, db := settingsEnv(t)
	for _, r := range [][2]string{
		{"GET", "/api/v0/settings"}, {"PUT", "/api/v0/settings/defaults"},
		{"GET", "/api/v0/settings/permissions"}, {"PUT", "/api/v0/settings/permissions"},
		{"GET", "/api/v0/settings/instructions"}, {"PUT", "/api/v0/settings/instructions"},
	} {
		req := e.req(t, r[0], r[1], strings.NewReader(`{"text":"x","forbid":[],"version":1}`))
		req.Header.Set("Authorization", "Bearer "+settingsTurnsToken)
		req.Header.Set("If-Match", "*")
		resp, out := do(t, req)
		if resp.StatusCode != 403 || strings.Contains(out, settingsTurnsToken) {
			t.Errorf("%s %s with the turns token: %d %s", r[0], r[1], resp.StatusCode, out)
		}
	}
	if in, _ := db.Info(); in.Version != 0 {
		t.Errorf("the settings changed: version %d", in.Version)
	}
}
