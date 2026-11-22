package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
func newJobsEnv(t *testing.T) *env { return newJobsEnvAt(t, nil) }

// newJobsEnvAt is newJobsEnv whose queue reads the time from now (nil: the
// clock).
func newJobsEnvAt(t *testing.T, now func() time.Time) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), now)
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
	return &env{srv: srv, api: a, store: st, port: port, q: q, fr: fr}
}

// The runner protocol is on the one address, under /api/v0/runner/. It takes
// the enrollment token and runner tokens, never the main token or the turns
// token. A turn asked for through the client API is the job a runner takes,
// with the same turn id. The jobs API is gone.
// An invalid wait refuses before any turn is queued or its idempotency key
// consumed (found in review 2026-11-19): a later valid create with the same key
// still starts a fresh turn.
func TestTurnWaitInvalidQueuesNothing(t *testing.T) {
	e := newJobsEnv(t)
	sess := e.newSession(t, "/tmp", true)
	body := `{"session":"` + sess + `","agent":"claude","prompt":"hi","idempotency_key":"k-wait-1"}`
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns?wait=bogus", strings.NewReader(body))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST ?wait=bogus: %d %s, want 400", resp.StatusCode, raw)
	}
	// No turn was queued by the refused request.
	resp, raw := do(t, e.req(t, "GET", "/api/v0/turns", nil))
	var list TurnList
	if json.Unmarshal([]byte(raw), &list) != nil || len(list.Turns) != 0 {
		t.Fatalf("turns after a refused create: %s", raw)
	}
	// The key is free: a valid create with the same key starts a fresh turn.
	resp, raw = do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var st Started
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal([]byte(raw), &st) != nil || st.Turn == "" || st.Existing {
		t.Fatalf("valid create reusing the key: %d %s, want a fresh turn", resp.StatusCode, raw)
	}
}

// Archiving a session whose first turn is still queued (no runner has started
// it) SUCCEEDS and hides it: archive is only a view, found through the queue as
// well as the store, and never refused for busy work (his call, 2026-11-19).
// The turn runs on.
func TestArchiveQueuedFirstTurnSucceeds(t *testing.T) {
	e := newJobsEnv(t)
	sess := e.newSession(t, "/tmp", true)
	body := `{"session":"` + sess + `","agent":"claude","prompt":"hi"}`
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var st Started
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal([]byte(raw), &st) != nil || st.Session == "" {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, raw)
	}
	// No runner is enrolled, so the turn stays queued: the session is known to
	// the queue (it has work) but has no record row yet.
	code, out := renameReq(t, e, st.Session, `{"archived":true}`)
	if code != 200 || !strings.Contains(out, `"archived":true`) {
		t.Fatalf("archive of a queued first turn: %d %s, want 200 archived", code, out)
	}
	// It is hidden by default and shown with archived=true; the queued turn is
	// untouched and still waits.
	var def, all Snapshot
	if code := getJSON(t, e, "/api/v0/sessions", &def); code != 200 || len(def.Sessions) != 0 {
		t.Fatalf("default list hides the archived session: %d %+v", code, def.Sessions)
	}
	if code := getJSON(t, e, "/api/v0/sessions?archived=true", &all); code != 200 ||
		len(all.Sessions) != 1 || all.Sessions[0].ID != st.Session || all.Sessions[0].Archived == nil {
		t.Fatalf("archived=true shows the session: %d %+v", code, all.Sessions)
	}
	var turns TurnList
	if code := getJSON(t, e, "/api/v0/turns?session="+st.Session, &turns); code != 200 || len(turns.Turns) != 1 || turns.Turns[0].State != "queued" {
		t.Fatalf("the turn still waits: %d %+v", code, turns.Turns)
	}
	// Un-archive is the same event with archived:false; it returns to the list.
	if code, out := renameReq(t, e, st.Session, `{"archived":false}`); code != 200 || strings.Contains(out, `"archived":true`) {
		t.Fatalf("un-archive: %d %s", code, out)
	}
	if code := getJSON(t, e, "/api/v0/sessions", &def); code != 200 || len(def.Sessions) != 1 || def.Sessions[0].Archived != nil {
		t.Fatalf("un-archived session back in the list: %d %+v", code, def.Sessions)
	}
	// A truly unknown session (not in the store, no queued work) is 404.
	if code, out := renameReq(t, e, "nope", `{"archived":true}`); code != 404 {
		t.Fatalf("archive of an unknown session: %d %s, want 404", code, out)
	}
}

// A wait=false retry of a turn stopped before it started reports the stop, not
// a stale "queued" (found in review 2026-11-19): the async reply reads the
// turn's real state.
func TestTurnWaitFalseRetryAfterStop(t *testing.T) {
	e := newJobsEnv(t)
	sess := e.newSession(t, "/tmp", true)
	body := `{"session":"` + sess + `","agent":"claude","prompt":"hi","idempotency_key":"k-stop-1"}`
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns?wait=false", strings.NewReader(body)))
	var st Started
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal([]byte(raw), &st) != nil || st.State != "queued" || st.Turn == "" {
		t.Fatalf("wait=false create: %d %s, want queued", resp.StatusCode, raw)
	}
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns/"+st.Turn+"/stop", strings.NewReader(`{}`))); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop the queued turn: %d %s", resp.StatusCode, raw)
	}
	// The retry must not say queued: the turn was stopped before it started.
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns?wait=false", strings.NewReader(body))); resp.StatusCode != http.StatusConflict {
		t.Fatalf("wait=false retry after stop: %d %s, want 409 (stopped before it started)", resp.StatusCode, raw)
	}
}

func TestRunnerProtocolOnTheOneAPI(t *testing.T) {
	e := newJobsEnv(t)
	sess := e.newSession(t, "/tmp", true)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"`+sess+`","agent":"claude","prompt":"hi"}`)))
	var st Started
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal([]byte(raw), &st) != nil || st.State != "queued" || st.Turn == "" {
		t.Fatalf("POST /api/v0/turns with no runner: %d %s, want queued with its turn", resp.StatusCode, raw)
	}
	noToken(t, resp, raw)
	for _, p := range []string{"/api/v0/jobs", "/api/v0/attempts/t_1/events"} {
		if resp, raw := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d %s, want 404: no such route", p, resp.StatusCode, raw)
		}
	}
	// Register takes the enrollment token, and not the main or turns token.
	enroll := `{"name":"r","capacity":1,"protocol":5,"capabilities":{"connector":"con_0123456789abcdef"}}`
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
	if resp, _ := do(t, e.req(t, "POST", "/api/v0/runner/take", strings.NewReader(`{"wait_ms":0,"protocol":5}`))); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("take with the main token: %d, want 401", resp.StatusCode)
	}
	r = e.req(t, "POST", "/api/v0/runner/take", strings.NewReader(`{"wait_ms":0,"protocol":5}`))
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
	r, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v0/turns/t_x/stop", nil)
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
	resp, raw = do(t, e.req(t, "POST", "/api/v0/turns/t_1/stop", strings.NewReader(`{}`)))
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(raw, "no queue") {
		t.Fatalf("POST /api/v0/turns/t_1/stop: %d %s", resp.StatusCode, raw)
	}
}

// A session whose runner was removed takes no more work: the API answers a
// next turn with 409 and the reason, which names the machine
// (queue/removed.go; 0.5.1 F1 note). A review goes through the same
// queue.create, which queue/removed_test.go covers.
func TestRemovedRunnerRefusesNextTurnAndReview(t *testing.T) {
	var later atomic.Int64 // how far the queue's clock is ahead
	e := newJobsEnvAt(t, func() time.Time { return time.Now().Add(time.Duration(later.Load())) })
	const machine = "con_0123456789abcdef"
	r, err := e.q.RegisterRunner(queue.DefaultTeam, "studio mac", nil, 4, queue.HashToken(queue.NewID("tok_")), &queue.Capabilities{Connector: machine})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "p",
		FolderRef: "fld_0123456789abcdef", Machine: machine, Isolate: true}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, a, ok, _, err := e.q.Take(r.ID, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := e.q.Start(r.ID, a.ID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.End(r.ID, a.ID, 1, queue.Finish{Status: queue.Finished, Session: j.Session,
		Result: []byte(`{"changeset":"` + strings.Repeat("1", 64) + `"}`), Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.ID}}); err != nil {
		t.Fatal(err)
	}
	later.Store(int64(2 * queue.RunnerFresh)) // the runner went offline
	if err := e.q.RemoveRunner(queue.DefaultTeam, r.ID); err != nil {
		t.Fatal(err)
	}
	// The session exists in the store (as POST /api/v0/sessions would have left
	// it): its workspace is a sandbox copy on the removed machine.
	seedWorkspaceOn(t, e.store, j.Session, "", "fld_0123456789abcdef", machine, true)
	want := queue.RunnerRemovedWhy("studio mac")
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","session":"`+j.Session+`","prompt":"again"}`)))
	var got struct{ Error string }
	if resp.StatusCode != http.StatusConflict || json.Unmarshal([]byte(raw), &got) != nil || got.Error != want {
		t.Errorf("POST /api/v0/turns: %d %s, want 409 %q", resp.StatusCode, raw, want)
	}
}
