package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

const turnsTok = "abq_TURNS-TOKEN-MUST-NEVER-APPEAR-0123"

// turnsEnv is an API with a turns token, whose Wire answers wire.
func turnsEnv(t *testing.T, wire Wire) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, TurnsToken: turnsTok, Port: port,
		BaseURL: "http://127.0.0.1:" + port, Wire: wire})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port}
}

func appendAll(t *testing.T, st *store.Store, evs ...event.Event) {
	t.Helper()
	for _, e := range evs {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

func asTok(t *testing.T, e *env, tok, method, path, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = e.req(t, method, path, strings.NewReader(body))
	} else {
		r = e.req(t, method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, out := do(t, r)
	if strings.Contains(out, tok) {
		t.Fatalf("%s %s: the token is in the reply", method, path)
	}
	return resp.StatusCode, out
}

// GET /api/v0/turns/{id} answers one turn with its end: exit code, stopped,
// unsettled, error and timings. 404 for a turn id with no turn.started.
func TestGetTurn(t *testing.T) {
	e := turnsEnv(t, nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := 0
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.SessionOpened, Time: now, Session: "s1", Turn: "t_a", Hold: "h1", Agent: "claude", Folder: "/x"},
		event.Event{ID: "e2", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_a", Text: "hi", Agent: "claude", Folder: "/x", IdempotencyKey: "job-1"},
		event.Event{ID: "e3", Kind: event.SessionClosed, Time: now.Add(time.Second), Session: "s1", Turn: "t_a", Hold: "h1", ExitCode: &code, Unsettled: true},
		event.Event{ID: "e4", Kind: event.SessionOpened, Time: now, Session: "s2", Turn: "t_b", Hold: "h2", Agent: "claude", Folder: "/x"},
		event.Event{ID: "e5", Kind: event.TurnStarted, Time: now, Session: "s2", Turn: "t_b", Text: "go", Agent: "claude", Folder: "/x"},
		event.Event{ID: "e6", Kind: event.SessionOpened, Time: now, Session: "s3", Turn: "t_c", Hold: "h3", Agent: "codex", Folder: "/x"},
		event.Event{ID: "e7", Kind: event.TurnStarted, Time: now, Session: "s3", Turn: "t_c", Text: "no", Agent: "codex", Folder: "/x"},
		event.Event{ID: "e8", Kind: event.SessionClosed, Time: now, Session: "s3", Turn: "t_c", Hold: "h3", Error: "the agent could not be started"},
	)
	get := func(path string) (int, OneTurn) {
		t.Helper()
		resp, out := do(t, e.req(t, "GET", path, nil))
		var one OneTurn
		if resp.StatusCode == 200 {
			if err := json.Unmarshal([]byte(out), &one); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, one
	}
	status, one := get("/api/v0/turns/t_a")
	if status != 200 || one.Store != e.store.ID() || one.Seq != e.store.Last() {
		t.Fatalf("t_a: %d %+v", status, one)
	}
	tr := one.Turn
	if tr.ID != "t_a" || tr.Session != "s1" || tr.State != store.TurnFinished || tr.ExitCode == nil || *tr.ExitCode != 0 ||
		!tr.Unsettled || tr.Stopped || tr.Ended == nil || !tr.Ended.Equal(now.Add(time.Second)) || tr.IdempotencyKey != "job-1" {
		t.Errorf("t_a: %+v", tr)
	}
	if _, one = get("/api/v0/turns/t_b"); one.Turn.State != store.TurnRunning || one.Turn.Ended != nil || one.Turn.Unsettled {
		t.Errorf("t_b: %+v", one.Turn)
	}
	if _, one = get("/api/v0/turns/t_c"); one.Turn.State != store.TurnFailed || one.Turn.Error != "the agent could not be started" {
		t.Errorf("t_c: %+v", one.Turn)
	}
	if status, _ = get("/api/v0/turns/t_a?session=s2"); status != 404 {
		t.Errorf("t_a in another session: %d, want 404", status)
	}
	if status, _ = get("/api/v0/turns/t_none"); status != 404 {
		t.Errorf("an unknown turn: %d, want 404", status)
	}
	if status, _ = get("/api/v0/turns/t_a?session=a%20b"); status != 400 {
		t.Errorf("a bad session: %d, want 400", status)
	}
}

// The turns token starts, stops and reads turns, and gets 403 everywhere else.
// A wrong token still gets 401.
func TestTurnsTokenScope(t *testing.T) {
	e := turnsEnv(t, func(agent, baseURL string) (Launch, error) { return Launch{}, ErrNotFound })
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.TurnStarted, Time: time.Now().UTC(), Session: "s1", Turn: "t_a", Text: "hi"})
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v0/turns", "", 200},
		{"GET", "/api/v0/turns?session=s1", "", 200},
		{"GET", "/api/v0/turns/t_a", "", 200},
		{"GET", "/api/v0/turns/t_a/events", "", 200},
		{"POST", "/api/v0/turns/t_a/stop", "", 409},
		// Reached the handler: the agent is not found, so 400 and nothing stored.
		{"POST", "/api/v0/turns", `{"agent":"claude","folder":"/","prompt":"x"}`, 400},
		{"GET", "/api/v0/sessions", "", 403},
		{"GET", "/api/v0/sessions/s1/events", "", 403},
		{"GET", "/api/v0/sessions/s1/search?q=hi", "", 403},
		{"GET", "/api/v0/events", "", 403},
		{"POST", "/api/v0/events", `{"kind":"x.p.n","session":"s1"}`, 403},
		{"GET", "/api/v0/agents", "", 403},
		{"GET", "/api/v0/version", "", 403},
		{"GET", "/api/v0/calls/x1/context", "", 403},
	} {
		got, out := asTok(t, e, turnsTok, c.method, c.path, c.body)
		if got != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, got, out, c.want)
		}
		if c.want == 403 && !strings.Contains(out, turnsRefused) {
			t.Errorf("%s %s: %s, want the turns token's refusal", c.method, c.path, out)
		}
	}
	if got, _ := asTok(t, e, turnsTok+"x", "GET", "/api/v0/turns", ""); got != 401 {
		t.Errorf("a wrong turns token: %d, want 401", got)
	}
}

// Without TurnsToken there is no second token; the same token as the main one
// is refused at New.
func TestTurnsTokenOff(t *testing.T) {
	e := newEnv(t)
	if got, _ := asTok(t, e, turnsTok, "GET", "/api/v0/turns", ""); got != 401 {
		t.Errorf("no turns token configured: %d, want 401", got)
	}
	if _, err := New(Config{Store: e.store, Token: token, TurnsToken: token, Port: e.port}); err == nil {
		t.Error("a turns token equal to the main token was accepted")
	}
}

// An idempotency key that already started a turn answers that turn, with
// existing true, and starts nothing; other fields under it get 409; a bad
// key gets 400. A request that started nothing stores no key.
func TestIdempotencyKey(t *testing.T) {
	wired := 0
	e := turnsEnv(t, func(agent, baseURL string) (Launch, error) { wired++; return Launch{}, ErrNotFound })
	dir := t.TempDir()
	body := `{"agent":"claude","folder":"` + dir + `","prompt":"hello","idempotency_key":"job-7:a1"}`
	var req TurnRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}

	// Nothing stored yet: the agent is not found, so 400, and the key is unused.
	if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body))); resp.StatusCode != 400 {
		t.Fatalf("first: %d %s", resp.StatusCode, out)
	}
	if _, found, _ := e.store.TurnByKey("job-7:a1"); found {
		t.Fatal("a turn that never started stored its key")
	}

	// As if a turn had started with it.
	appendAll(t, e.store, event.Event{ID: "e1", Kind: event.TurnStarted, Time: time.Now().UTC(), Session: "s9", Turn: "t_9",
		Text: "hello", Agent: "claude", Folder: dir, IdempotencyKey: "job-7:a1", RequestHash: requestHash(req, false)})
	wired = 0
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var s Started
	_ = json.Unmarshal([]byte(out), &s)
	if resp.StatusCode != 202 || s.ID != "s9" || s.Turn != "t_9" || !s.Existing || s.BaseURL != e.api.cfg.BaseURL+"/s/s9/t/t_9" {
		t.Fatalf("retry: %d %s", resp.StatusCode, out)
	}
	if wired != 0 {
		t.Fatal("a retry reached the agent's Wire")
	}
	// The turns token gets the same answer.
	if got, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", body); got != 202 || !strings.Contains(out, `"existing":true`) {
		t.Fatalf("retry with the turns token: %d %s", got, out)
	}

	for name, b := range map[string]string{
		"another prompt":    `{"agent":"claude","folder":"` + dir + `","prompt":"other","idempotency_key":"job-7:a1"}`,
		"another model":     `{"agent":"claude","folder":"` + dir + `","prompt":"hello","model":"haiku","idempotency_key":"job-7:a1"}`,
		"read_only":         `{"agent":"claude","folder":"` + dir + `","prompt":"hello","read_only":true,"idempotency_key":"job-7:a1"}`,
		"with a session":    `{"session":"s9","prompt":"hello","idempotency_key":"job-7:a1"}`,
		"with session null": `{"session":null,"agent":"claude","folder":"` + dir + `","prompt":"hello","idempotency_key":"job-7:a1"}`,
	} {
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode != 409 {
			t.Errorf("%s: %d %s, want 409", name, resp.StatusCode, out)
		}
	}
	for _, k := range []string{`""`, `"a b"`, `"` + strings.Repeat("k", 129) + `"`, `"é"`} {
		b := `{"agent":"claude","folder":"` + dir + `","prompt":"hello","idempotency_key":` + k + `}`
		resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b)))
		// An empty key is the same as none: it reaches the Wire, which says 400.
		if resp.StatusCode != 400 {
			t.Errorf("key %s: %d %s, want 400", k, resp.StatusCode, out)
		}
	}
}

// The run limit comes from Config.MaxRuns, and its 429 names it.
func TestMaxRuns(t *testing.T) {
	rs := runs{max: 2}
	for i, s := range []string{"a", "b"} {
		if code, _ := rs.take(s); code != 0 {
			t.Fatalf("take %d: %d", i, code)
		}
	}
	code, msg := rs.take("c")
	if code != http.StatusTooManyRequests || !strings.HasPrefix(msg, "2 runs") {
		t.Fatalf("third: %d %q", code, msg)
	}
	rs.release(nil, "a")
	if code, _ := rs.take("c"); code != 0 {
		t.Fatalf("after a release: %d", code)
	}
	e := newEnv(t)
	if e.api.runs.max != DefaultMaxRuns {
		t.Errorf("default %d, want %d", e.api.runs.max, DefaultMaxRuns)
	}
	if _, err := New(Config{Store: e.store, Token: token, Port: e.port, MaxRuns: -1}); err == nil {
		t.Error("MaxRuns -1 accepted")
	}
}
