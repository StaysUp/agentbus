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
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

const enrollTok = "abe_ENROLL-NEVER-ECHOED"

// newJobsEnv is an API with a job queue behind it, on loopback, every origin
// allowed.
func newJobsEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "folders.json")
	if err := os.WriteFile(p, []byte(`{"connector":"con_0123456789abcdef","folders":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fr, err := folders.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	js, err := jobs.New(jobs.Config{Queue: q, Token: token, EnrollToken: enrollTok, Folders: fr})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, TurnsToken: turnsTok, Port: port, BaseURL: "http://127.0.0.1:" + port, Jobs: js,
		Queue: q, Folders: fr, TurnWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	return &env{srv: srv, api: a, store: st, port: port, q: q}
}

// The runner protocol is on the one address, under /api/v0/runner/. It takes
// the enrollment token and runner tokens, never the main token or the turns
// token. A turn asked for through the client API is the job a runner takes,
// with the same turn id. The jobs API is gone.
func TestRunnerProtocolOnTheOneAPI(t *testing.T) {
	e := newJobsEnv(t)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"/tmp","prompt":"hi"}`)))
	var st Started
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal([]byte(raw), &st) != nil || st.State != "queued" || st.Turn == "" {
		t.Fatalf("POST /api/v0/turns with no runner: %d %s, want queued with its turn", resp.StatusCode, raw)
	}
	noToken(t, resp, raw)
	for _, p := range []string{"/api/v0/jobs", "/api/v0/workspaces", "/api/v0/attempts/t_1/events"} {
		if resp, raw := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d %s, want 404: no such route", p, resp.StatusCode, raw)
		}
	}
	// Register takes the enrollment token, and not the main or turns token.
	enroll := `{"name":"r","capacity":1,"protocol":4,"capabilities":{"connector":"con_0123456789abcdef"}}`
	for _, tok := range []string{token, turnsTok} {
		r := e.req(t, "POST", "/api/v0/runner/register", strings.NewReader(enroll))
		r.Header.Set("Authorization", "Bearer "+tok)
		if resp, _ := do(t, r); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("register with %s: %d, want 401", tok[:4], resp.StatusCode)
		}
	}
	r := e.req(t, "POST", "/api/v0/runner/register", strings.NewReader(enroll))
	r.Header.Set("Authorization", "Bearer "+enrollTok)
	resp, raw = do(t, r)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, raw)
	}
	var en jobs.Enrolled
	if err := json.Unmarshal([]byte(raw), &en); err != nil {
		t.Fatal(err)
	}
	// The main token never takes work.
	if resp, _ := do(t, e.req(t, "POST", "/api/v0/runner/take", strings.NewReader(`{"wait_ms":0,"protocol":4}`))); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("take with the main token: %d, want 401", resp.StatusCode)
	}
	r = e.req(t, "POST", "/api/v0/runner/take", strings.NewReader(`{"wait_ms":0,"protocol":4}`))
	r.Header.Set("Authorization", "Bearer "+en.Token)
	resp, raw = do(t, r)
	var taken jobs.TakenWork
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &taken) != nil || taken.Job == nil || taken.Job.Turn != st.Turn {
		t.Fatalf("take with the runner token: %d %s, want the job of turn %s", resp.StatusCode, raw, st.Turn)
	}
}

// A browser page from another origin may call the API: the preflight is the
// API's own, and every answer allows any origin.
func TestTurnsCORS(t *testing.T) {
	e := newJobsEnv(t)
	r, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v0/turns/t_x/review", nil)
	r.Header.Set("Origin", "http://localhost:5199")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	resp, _ := do(t, r)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	r = e.req(t, "GET", "/api/v0/turns", nil)
	r.Header.Set("Origin", "http://localhost:5199")
	if resp, _ := do(t, r); resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("GET /api/v0/turns from another origin: %d %v", resp.StatusCode, resp.Header)
	}
}

// Without a queue the runner protocol answers 404 with its own words, and so
// do the turn routes.
func TestJobsOff(t *testing.T) {
	e := newEnv(t)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/runner/take", strings.NewReader(`{}`)))
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(raw, "no job queue") {
		t.Fatalf("POST /api/v0/runner/take: %d %s", resp.StatusCode, raw)
	}
	resp, raw = do(t, e.req(t, "POST", "/api/v0/turns/t_1/review", strings.NewReader(`{"agent":"claude"}`)))
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(raw, "no queue") {
		t.Fatalf("POST /api/v0/turns/t_1/review: %d %s", resp.StatusCode, raw)
	}
}
