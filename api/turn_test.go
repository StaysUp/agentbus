package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func TestIDScanner(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	result := `{"type":"result","result":"hi","session_id":"` + id + `"}`
	big := `{"type":"result","result":"` + strings.Repeat("a", MaxStdoutLine) + `","session_id":"` + id + `"}`
	for _, tc := range []struct {
		name, agent string
		writes      []string
		want        string
	}{
		{"claude whole", "claude", []string{result + "\n"}, id},
		{"claude split across writes, no final newline", "claude", []string{result[:10], result[10:30], result[30:]}, id},
		{"claude line over the bound is skipped", "claude", []string{big + "\n"}, ""},
		{"a line after a skipped one still counts", "claude", []string{big + "\n" + result + "\n"}, id},
		{"claude takes only the result type", "claude", []string{result + "\n" + `{"type":"assistant","session_id":"other-1"}` + "\n"}, id},
		{"claude ignores codex's shape", "claude", []string{`{"type":"thread.started","thread_id":"` + id + `"}` + "\n"}, ""},
		{"codex takes the first thread.started", "codex", []string{`{"type":"thread.started","thread_id":"` + id + `"}` + "\n" +
			`{"type":"thread.started","thread_id":"other"}` + "\n"}, id},
		{"codex ignores claude's shape", "codex", []string{result + "\n"}, ""},
		{"an id not shaped like one", "claude", []string{`{"type":"result","session_id":"-x"}` + "\n"}, ""},
		{"an id with a slash", "claude", []string{`{"type":"result","session_id":"a/b"}` + "\n"}, ""},
	} {
		s := &idScanner{agent: tc.agent}
		for _, w := range tc.writes {
			if n, err := s.Write([]byte(w)); n != len(w) || err != nil {
				t.Fatalf("%s: Write %d %v", tc.name, n, err)
			}
		}
		if got := s.ID(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if cap(s.line) > MaxStdoutLine+64<<10 {
			t.Errorf("%s: held %d bytes", tc.name, cap(s.line))
		}
	}
}

func TestVersionToken(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.318 (Claude Code)\n": "2.1.318",
		"codex-cli 0.164.0\n":     "0.164.0",
		"tool v1.2.3-beta.1":      "1.2.3-beta.1",
		"no version here":         "",
		"":                        "",
	} {
		if got := versionToken(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// resumableEnv is an API whose Wire calls wire, with session "s1" started by
// a run of claude and closed with a resume_id.
func resumableEnv(t *testing.T, wire Wire) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Wire: wire})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); a.StopRuns(); st.Close() })
	code := 0
	now := time.Now().UTC()
	for _, e := range []event.Event{
		{ID: "evt_2", Kind: event.SessionOpened, Time: now, Session: "s1", Hold: "hold_1", Agent: "claude", Folder: t.TempDir()},
		{ID: "evt_3", Kind: event.SessionClosed, Time: now, Session: "s1", Hold: "hold_1", ExitCode: &code, ResumeID: "abc-1"},
	} {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	return &env{srv: srv, api: a, store: st, port: port}
}

func postNext(t *testing.T, e *env) (int, string) {
	t.Helper()
	resp, body := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"s1","prompt":"next"}`)))
	return resp.StatusCode, body
}

// A turn counts as running from the moment it is accepted, before its child
// starts and its session.opened is stored: a second turn sent in that window
// is refused, not started beside it.
func TestNextTurnRefusedWhileAnotherIsStarting(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	// Only the first caller waits, so a second turn that is wrongly let
	// through answers 400 at once instead of hanging the test.
	var once sync.Once
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) {
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
		return Launch{}, ErrNotFound
	})
	// Runs before the server closes, so a failure below cannot leave the
	// first request waiting.
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(free)
	first := make(chan int)
	go func() { code, _ := postNext(t, e); first <- code }()
	<-entered
	if code, body := postNext(t, e); code != http.StatusConflict || !strings.Contains(body, "a turn is already running in this session") {
		t.Fatalf("second turn while the first starts: %d %s, want 409", code, body)
	}
	free()
	if code := <-first; code != http.StatusBadRequest {
		t.Fatalf("first turn: %d, want 400 (its binary was not found)", code)
	}
	// The slot and the session are free again.
	if code, body := postNext(t, e); code != http.StatusBadRequest {
		t.Fatalf("after the first ended: %d %s, want 400 again", code, body)
	}
}

// A child the store shows running keeps the session busy too, even one this
// process did not start (such as one stored just before a restart).
func TestNextTurnRefusedWhileAChildIsOpen(t *testing.T) {
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) { return Launch{}, ErrNotFound })
	if _, _, err := e.store.Append(event.Event{ID: "evt_4", Kind: event.SessionOpened, Time: time.Now().UTC(),
		Session: "s1", Hold: "hold_2"}); err != nil {
		t.Fatal(err)
	}
	if code, body := postNext(t, e); code != http.StatusConflict || !strings.Contains(body, "a turn is already running in this session") {
		t.Fatalf("%d %s, want 409", code, body)
	}
}

// The write after the child started was ignored, so with a store that could
// not write the API answered 202 and the child ran with no record.
// session.opened is now written first: a failed write answers 500 and starts
// nothing.
func TestRunRefusedWhenStoreCannotWrite(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "ran")
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) {
		return Launch{Path: "/bin/sh", Args: []string{"-c", `touch "$0"`, mark}}, nil
	})
	e.store.Close() // every Append now fails
	resp, body := do(t, e.req(t, "POST", "/api/v0/turns",
		strings.NewReader(`{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"hi"}`)))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("%d %s, want 500", resp.StatusCode, body)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the child started although nothing could be stored")
	}
}

// The API kept every session's agent and folder in memory forever. TurnSink
// now reads them from the store's session.opened, so the API keeps nothing,
// and a turn.started that arrives after its child exited is still named.
func TestTurnSinkNamesTurnsAfterChildExits(t *testing.T) {
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) {
		return Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
	})
	folder := t.TempDir()
	var ids []string
	for i := 0; i < 3; i++ {
		resp, body := do(t, e.req(t, "POST", "/api/v0/turns",
			strings.NewReader(`{"agent":"codex","folder":"`+folder+`","prompt":"hi"}`)))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		var out Started
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, out.ID)
	}
	for _, id := range ids {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if c, _ := e.store.Continuation(id); c.Known && !c.Open {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never closed", id)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	sink := e.api.TurnSink(e.store)
	sink.Emit(event.Event{ID: "evt_late", Kind: event.TurnStarted, Time: time.Now(), Session: ids[0], Turn: "t1", Text: "late"})
	sink.Emit(event.Event{ID: "evt_hand", Kind: event.TurnStarted, Time: time.Now(), Session: "byhand", Turn: "t1", Text: "hand"})
	for session, want := range map[string][2]string{ids[0]: {"codex", folder}, "byhand": {"", ""}} {
		recs, err := e.store.After(0, store.Filter{Session: session, Only: true}, 100)
		if err != nil {
			t.Fatal(err)
		}
		var got *event.Event
		for _, r := range recs {
			var ev event.Event
			if json.Unmarshal(r.Data, &ev) == nil && ev.Kind == event.TurnStarted {
				got = &ev
			}
		}
		if got == nil || got.Agent != want[0] || got.Folder != want[1] {
			t.Fatalf("%s: turn.started %+v, want agent %q folder %q", session, got, want[0], want[1])
		}
	}
}

// GET /turns/{id}/events?session= keeps one session's events of a turn id
// that two sessions share; without it both are sent, and a session that is
// not an id shape is refused.
func TestTurnEventsSessionQuery(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{"s1", "s2"} {
		for _, k := range []event.Kind{event.TurnStarted, event.ModelRequested} {
			if _, _, err := e.store.Append(event.Event{ID: s + string(k), Kind: k, Session: s, Turn: "t_same", Time: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sessions := func(query string) []string {
		t.Helper()
		resp, body := do(t, e.req(t, "GET", "/api/v0/turns/t_same/events"+query, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", query, resp.StatusCode, body)
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
	if got := strings.Join(sessions(""), " "); got != "s1 s1 s2 s2" {
		t.Errorf("no session: %s", got)
	}
	if got := strings.Join(sessions("?session=s2"), " "); got != "s2 s2" {
		t.Errorf("session=s2: %s", got)
	}
	if got := sessions("?session=nobody"); len(got) != 0 {
		t.Errorf("session=nobody: %v", got)
	}
	if resp, body := do(t, e.req(t, "GET", "/api/v0/turns/t_same/events?session=a.b", nil)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("session=a.b: %d %s, want 400", resp.StatusCode, body)
	}
}
