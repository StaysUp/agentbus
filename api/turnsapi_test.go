package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

const turnsTok = "abq_TURNS-TOKEN-MUST-NEVER-APPEAR-0123"

func appendAll(t *testing.T, st *store.Store, evs ...event.Event) {
	t.Helper()
	for _, e := range evs {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// turnsEnv is an API with a whole stack and a turns token, whose engine's
// Wire answers wire.
func turnsEnv(t *testing.T, wire run.Wire) *env {
	t.Helper()
	return stackEnv(t, stack{wire: wire, turnsToken: true})
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
	e := turnsEnv(t, func(agent, baseURL string) (run.Launch, error) { return run.Launch{}, run.ErrNotFound })
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
		{"POST", "/api/v0/turns", `{"agent":"claude","folder":"` + t.TempDir() + `","prompt":"x"}`, 400},
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

// A turn the engine refuses answers with the engine's status and words: the
// job failed with reason setup. A request the API refuses is never queued.
func TestTurnRefusalsComeFromTheJob(t *testing.T) {
	e := turnsEnv(t, func(agent, baseURL string) (run.Launch, error) { return run.Launch{}, run.ErrNotFound })
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"x"}`)))
	if resp.StatusCode != 400 || !strings.Contains(out, "is not on the service's PATH") {
		t.Fatalf("missing agent: %d %s", resp.StatusCode, out)
	}
	resp, out = do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"nosuch","prompt":"x"}`)))
	if resp.StatusCode != 404 || !strings.Contains(out, "no such session") {
		t.Fatalf("unknown session: %d %s", resp.StatusCode, out)
	}
	jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{}, 10)
	if len(jobs) != 2 || jobs[0].Reason != queue.ReasonSetup {
		t.Fatalf("jobs %+v", jobs)
	}
	for _, b := range []string{`{"agent":"x","folder":"/","prompt":"x"}`, `{"agent":"claude","folder":"rel","prompt":"x"}`,
		`{"agent":"claude","folder":"/","prompt":""}`, `{"session":"a b","prompt":"x"}`, `{"session":null,"prompt":"x"}`} {
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode != 400 {
			t.Errorf("%s: %d %s, want 400", b, resp.StatusCode, out)
		}
	}
	if jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{}, 10); len(jobs) != 2 {
		t.Errorf("%d jobs after requests refused before the queue, want 2", len(jobs))
	}
}
