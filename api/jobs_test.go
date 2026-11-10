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
	a, err := New(Config{Store: st, Token: token, TurnsToken: turnsTok, Port: port, BaseURL: "http://127.0.0.1:" + port, Jobs: js})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	return &env{srv: srv, api: a, store: st, port: port}
}

// The jobs API is on the one address with the one main token. The turns
// token and a turn token cannot reach it. The runner protocol takes the
// enrollment token and runner tokens, never the main token.
func TestJobsShareTheOneAPI(t *testing.T) {
	e := newJobsEnv(t)
	body := `{"kind":"turn","spec":{"agent":"claude","folder":"/tmp","prompt":"hi"}}`
	resp, raw := do(t, e.req(t, "POST", "/api/v0/jobs", strings.NewReader(body)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v0/jobs with the main token: %d %s", resp.StatusCode, raw)
	}
	noToken(t, resp, raw)
	r := e.req(t, "GET", "/api/v0/jobs", nil)
	r.Header.Set("Authorization", "Bearer "+turnsTok)
	if resp, _ := do(t, r); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/v0/jobs with the turns token: %d, want 403", resp.StatusCode)
	}
	r = e.req(t, "GET", "/api/v0/jobs", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	if resp, _ := do(t, r); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/v0/jobs with a wrong token: %d, want 401", resp.StatusCode)
	}
	// Enroll takes the enrollment token, and not the main token.
	enroll := `{"name":"r","capacity":1,"protocol":1,"capabilities":{"connector":"con_0123456789abcdef"}}`
	if resp, _ := do(t, e.req(t, "POST", "/api/v0/runners", strings.NewReader(enroll))); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("enroll with the main token: %d, want 401", resp.StatusCode)
	}
	r = e.req(t, "POST", "/api/v0/runners", strings.NewReader(enroll))
	r.Header.Set("Authorization", "Bearer "+enrollTok)
	resp, raw = do(t, r)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enroll: %d %s", resp.StatusCode, raw)
	}
	var en jobs.Enrolled
	if err := json.Unmarshal([]byte(raw), &en); err != nil {
		t.Fatal(err)
	}
	r = e.req(t, "POST", "/api/v0/runners/"+en.Runner.ID+"/take", strings.NewReader(`{"wait_ms":0}`))
	r.Header.Set("Authorization", "Bearer "+en.Token)
	resp, raw = do(t, r)
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"attempt"`) {
		t.Fatalf("take with the runner token: %d %s", resp.StatusCode, raw)
	}
}

// A browser page from another origin may call the jobs API: the preflight is
// the API's own, and every answer allows any origin.
func TestJobsCORS(t *testing.T) {
	e := newJobsEnv(t)
	r, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v0/jobs/job_x/review", nil)
	r.Header.Set("Origin", "http://localhost:5199")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	resp, _ := do(t, r)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	r = e.req(t, "GET", "/api/v0/jobs", nil)
	r.Header.Set("Origin", "http://localhost:5199")
	if resp, _ := do(t, r); resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("GET /api/v0/jobs from another origin: %d %v", resp.StatusCode, resp.Header)
	}
}

// Without a queue the job routes answer 404 with their own words.
func TestJobsOff(t *testing.T) {
	e := newEnv(t)
	resp, raw := do(t, e.req(t, "GET", "/api/v0/jobs", nil))
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(raw, "no job queue") {
		t.Fatalf("GET /api/v0/jobs: %d %s", resp.StatusCode, raw)
	}
}
