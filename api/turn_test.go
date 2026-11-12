package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// resumableEnv is an API with a whole stack, whose engine's Wire calls wire,
// with session "s1" started by a run of claude and closed with a resume_id.
func resumableEnv(t *testing.T, wire run.Wire) *env {
	t.Helper()
	folder := t.TempDir()
	return stackEnv(t, stack{wire: wire, before: func(st *store.Store) {
		code := 0
		now := time.Now().UTC()
		for _, e := range []event.Event{
			{ID: "evt_2", Kind: event.SessionOpened, Time: now, Session: "s1", Hold: "hold_1", Harness: "claude", Folder: folder},
			{ID: "evt_3", Kind: event.SessionClosed, Time: now, Session: "s1", Hold: "hold_1", ExitCode: &code, ResumeID: "abc-1"},
		} {
			if _, _, err := st.Append(e); err != nil {
				t.Fatal(err)
			}
		}
	}})
}

func postNext(t *testing.T, e *env) (int, string) {
	t.Helper()
	resp, body := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","session":"s1","prompt":"next"}`)))
	return resp.StatusCode, body
}

// A turn sent while the session's turn is starting is queued behind it, not
// refused and not started beside it: the reply says queued at once, with one
// turn ahead, and the job runs once the first has ended.
func TestNextTurnQueuesWhileAnotherIsStarting(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	// Only the first caller waits; the queued one is refused at once (its
	// binary is not found) when its turn comes.
	var once sync.Once
	var starts atomic.Int32
	e := resumableEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		starts.Add(1)
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
		return run.Launch{}, run.ErrNotFound
	})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(free)
	first := make(chan int)
	go func() { code, _ := postNext(t, e); first <- code }()
	<-entered
	code, body := postNext(t, e)
	var out Started
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != http.StatusAccepted || out.State != "queued" || out.Ahead != 1 ||
		out.Turn == "" || out.Session != "s1" || out.BaseURL != "" {
		t.Fatalf("second turn while the first starts: %d %s, want 202 queued with ahead 1 and its turn id", code, body)
	}
	// The queued turn is known by its id from now on, and listed first.
	var one OneTurn
	if resp, b := do(t, e.req(t, "GET", "/api/v0/turns/"+out.Turn, nil)); resp.StatusCode != 200 || json.Unmarshal([]byte(b), &one) != nil ||
		one.Turn.State != "queued" || one.Turn.Session != "s1" || one.Turn.Prompt != "next" || one.Turn.Ahead != 1 || one.Turn.Kind != "turn" || one.Turn.Asked == nil {
		t.Fatalf("GET the queued turn: %d %s", resp.StatusCode, b)
	}
	var list TurnList
	if resp, b := do(t, e.req(t, "GET", "/api/v0/turns?session=s1", nil)); resp.StatusCode != 200 || json.Unmarshal([]byte(b), &list) != nil ||
		len(list.Turns) == 0 || list.Turns[0].ID != out.Turn || list.Turns[0].State != "queued" {
		t.Fatalf("GET /turns: %d %s, want the queued turn first", resp.StatusCode, b)
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("%d starts while the first runs, want 1", n)
	}
	free()
	if code := <-first; code != http.StatusBadRequest {
		t.Fatalf("first turn: %d, want 400 (its binary was not found)", code)
	}
	// The queued job runs next, and is refused the same way.
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := e.q.JobByTurn(queue.DefaultTeam, out.Turn)
		if err != nil {
			t.Fatal(err)
		}
		if j.Ended != nil {
			if j.State != queue.Failed || j.Reason != queue.ReasonSetup {
				t.Fatalf("queued job ended %s %s, want failed setup", j.State, j.Reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued job still %s", j.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := starts.Load(); n != 2 {
		t.Fatalf("%d starts, want 2", n)
	}
	// A turn that ended before it started says so, and why.
	if resp, b := do(t, e.req(t, "GET", "/api/v0/turns/"+out.Turn, nil)); resp.StatusCode != 200 || json.Unmarshal([]byte(b), &one) != nil ||
		one.Turn.State != "failed" || one.Turn.Error == "" || one.Turn.Ended == nil {
		t.Fatalf("GET the refused turn: %d %s", resp.StatusCode, b)
	}
	// Nothing is ahead now: a third turn waits for its own start.
	if code, body := postNext(t, e); code != http.StatusBadRequest {
		t.Fatalf("after both ended: %d %s, want 400 again", code, body)
	}
}

// A child the store shows running keeps the session busy too, even one this
// process did not start (such as one stored just before a restart).
func TestNextTurnRefusedWhileAChildIsOpen(t *testing.T) {
	e := resumableEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) { return run.Launch{}, run.ErrNotFound })
	if _, _, err := e.store.Append(event.Event{ID: "evt_4", Kind: event.SessionOpened, Time: time.Now().UTC(),
		Session: "s1", Hold: "hold_2"}); err != nil {
		t.Fatal(err)
	}
	if code, body := postNext(t, e); code != http.StatusConflict || !strings.Contains(body, "a turn is already running in this session") {
		t.Fatalf("%d %s, want 409", code, body)
	}
}

// GET /sessions/{id}/events?turn= keeps one session's events of a turn id
// that two sessions share, and a turn that is not an id shape is refused.
func TestSessionEventsTurnQuery(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{"s1", "s2"} {
		for _, k := range []event.Kind{event.TurnStarted, event.ModelRequested} {
			if _, _, err := e.store.Append(event.Event{ID: s + string(k), Kind: k, Session: s, Turn: "t_same", Time: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sessions := func(session string) []string {
		t.Helper()
		resp, body := do(t, e.req(t, "GET", "/api/v0/sessions/"+session+"/events?turn=t_same", nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", session, resp.StatusCode, body)
		}
		var page struct{ Events []event.Event }
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ev := range page.Events {
			out = append(out, ev.Session)
		}
		return out
	}
	if got := strings.Join(sessions("s1"), " "); got != "s1 s1" {
		t.Errorf("s1: %s", got)
	}
	if got := strings.Join(sessions("s2"), " "); got != "s2 s2" {
		t.Errorf("s2: %s", got)
	}
	if got := sessions("nobody"); len(got) != 0 {
		t.Errorf("nobody: %v", got)
	}
	if resp, body := do(t, e.req(t, "GET", "/api/v0/sessions/s1/events?turn=a.b", nil)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("turn=a.b: %d %s, want 400", resp.StatusCode, body)
	}
}
