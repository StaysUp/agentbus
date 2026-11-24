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

// A turns token may open a session in an existing workspace (then run its
// turns), the same as an ask, but it may not create a workspace: choosing a
// folder and the Sandbox switch stays with the main token (his ruling,
// 2026-11-22).
func TestTurnsTokenCreatesSessionNotWorkspace(t *testing.T) {
	e := turnsEnv(t, nil)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true) // the main token chose the folder and sandbox

	code, out := asTok(t, e, turnsTok, "POST", "/api/v0/sessions", `{"workspace":"`+ws+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("turns token create session in an existing workspace: %d %s", code, out)
	}
	var sc SessionCreated
	if json.Unmarshal([]byte(out), &sc) != nil || sc.Session == "" || sc.Workspace != ws {
		t.Fatalf("session: %s", out)
	}
	// Only an EXISTING workspace: an unknown one is 404, never created.
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/sessions", `{"workspace":"ws_0000000000000000"}`); code != http.StatusNotFound {
		t.Fatalf("turns token session in an unknown workspace: %d %s, want 404", code, out)
	}
	// It cannot create a workspace: that route is the main token's.
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/workspaces", `{"folder_ref":"`+ref+`","machine":"`+machine+`"}`); code != http.StatusForbidden {
		t.Fatalf("turns token create workspace: %d %s, want 403", code, out)
	}
}

// GET /api/v0/turns/{id} answers one turn with its end: exit code, stopped,
// unsettled, error and timings. 404 for a turn id with no turn.started.
func TestGetTurn(t *testing.T) {
	e := turnsEnv(t, nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := 0
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.RunStarted, Time: now, Session: "s1", Turn: "t_a", Hold: "h1", Harness: "claude", Folder: "/x"},
		event.Event{ID: "e2", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_a", Text: "hi", Harness: "claude", Folder: "/x", IdempotencyKey: "job-1"},
		event.Event{ID: "e3", Kind: event.RunEnded, Time: now.Add(time.Second), Session: "s1", Turn: "t_a", Hold: "h1", ExitCode: &code, Unsettled: true},
		event.Event{ID: "e4", Kind: event.RunStarted, Time: now, Session: "s2", Turn: "t_b", Hold: "h2", Harness: "claude", Folder: "/x"},
		event.Event{ID: "e5", Kind: event.TurnStarted, Time: now, Session: "s2", Turn: "t_b", Text: "go", Harness: "claude", Folder: "/x"},
		event.Event{ID: "e6", Kind: event.RunStarted, Time: now, Session: "s3", Turn: "t_c", Hold: "h3", Harness: "codex", Folder: "/x"},
		event.Event{ID: "e7", Kind: event.TurnStarted, Time: now, Session: "s3", Turn: "t_c", Text: "no", Harness: "codex", Folder: "/x"},
		event.Event{ID: "e8", Kind: event.RunEnded, Time: now, Session: "s3", Turn: "t_c", Hold: "h3", Error: "the agent could not be started"},
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

// The turns token starts, stops, reviews and reads turns, and their events,
// and gets 403 everywhere else.
// A wrong token still gets 401.
func TestTurnsTokenScope(t *testing.T) {
	e := turnsEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) { return run.Launch{}, run.ErrNotFound })
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.TurnStarted, Time: time.Now().UTC(), Session: "s1", Turn: "t_a", Text: "hi"})
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v0/turns", "", 200},
		{"GET", "/api/v0/turns?session=s1", "", 200},
		{"GET", "/api/v0/turns/t_a", "", 200},
		{"GET", "/api/v0/sessions/s1/events?turn=t_a", "", 200},
		{"GET", "/api/v0/sessions/s1/events", "", 200},
		{"POST", "/api/v0/turns/t_a/stop", "", 409},
		// Reached the handler: the agent is not found, so 400 and nothing stored.
		{"POST", "/api/v0/turns", `{"agent":"claude","folder":"` + t.TempDir() + `","prompt":"x"}`, 400},
		{"GET", "/api/v0/sessions", "", 403},
		{"GET", "/api/v0/sessions/s1/search?q=hi", "", 403},
		{"GET", "/api/v0/events", "", 403},
		{"POST", "/api/v0/events", `{"kind":"x.p.n","session":"s1"}`, 403},
		{"GET", "/api/v0/agents", "", 403},
		{"GET", "/api/v0/version", "", 403},
		{"GET", "/api/v0/turns/t_a/calls/x1", "", 403},
		{"POST", "/api/v0/workspaces/ws_a/apply", "", 403},
		{"PATCH", "/api/v0/sessions/s1", `{"closed":true}`, 403},
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
	e := turnsEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) { return run.Launch{}, run.ErrNotFound })
	// A valid turn the engine refuses: the runner's setup fails with ErrNotFound,
	// and the POST answers with the engine's own words and reason setup.
	s1 := e.newSession(t, t.TempDir(), true)
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"`+s1+`","agent":"claude","prompt":"x"}`)))
	if resp.StatusCode != 400 || !strings.Contains(out, "is not on the service's PATH") {
		t.Fatalf("engine refusal: %d %s", resp.StatusCode, out)
	}
	// A second valid turn (another session) refuses the same way: two turn jobs,
	// each failed at setup.
	s2 := e.newSession(t, t.TempDir(), true)
	if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"`+s2+`","agent":"claude","prompt":"x"}`))); resp.StatusCode != 400 {
		t.Fatalf("second engine refusal: %d %s", resp.StatusCode, out)
	}
	jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindTurn}, 10)
	if len(jobs) != 2 || jobs[0].Reason != queue.ReasonSetup {
		t.Fatalf("jobs %+v", jobs)
	}
	// An unknown session is refused with 404 and no job.
	if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"nosuch","agent":"claude","prompt":"x"}`))); resp.StatusCode != 404 || !strings.Contains(out, "no such session") {
		t.Fatalf("unknown session: %d %s", resp.StatusCode, out)
	}
	// A request the API refuses (a bad session shape, a missing agent, a missing
	// prompt, a null session) is refused with 4xx and never queued.
	for _, b := range []string{`{"session":"a b","agent":"claude","prompt":"x"}`,
		`{"session":"` + s1 + `","prompt":"x"}`, `{"session":"` + s1 + `","agent":"claude","prompt":""}`,
		`{"session":null,"agent":"claude","prompt":"x"}`} {
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode < 400 || resp.StatusCode > 404 {
			t.Errorf("%s: %d %s, want 4xx", b, resp.StatusCode, out)
		}
	}
	if jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindTurn}, 10); len(jobs) != 2 {
		t.Errorf("%d turn jobs after requests refused before the queue, want 2", len(jobs))
	}
}
