package jobs

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
)

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// loginEnv is a jobs server on a queue file at path, with a clock.
func loginEnv(t *testing.T, path string, c *clock) (*httptest.Server, *Server, *queue.Queue) {
	t.Helper()
	q, err := queue.Open(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close() })
	return srv, s, q
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)} }

var codeShape = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

// startLogin asks to sign a machine in that holds a folder, with no token.
func startLogin(t *testing.T, srv *httptest.Server) LoginStarted {
	t.Helper()
	var l LoginStarted
	body := EnrollRequest{Name: "laptop", Capacity: 2, Protocol: ProtocolVersion, Capabilities: &queue.Capabilities{OS: "linux",
		Connector: testConnector, Folders: []queue.FolderInfo{{Ref: "fld_0123456789abcdef", Name: "app"}},
		Sandbox: &queue.SandboxReport{Can: false, Why: "bwrap is missing"}}}
	if code := send(t, srv, "", "POST", "/api/v0/runner/login", body, &l); code != 201 {
		t.Fatalf("login: %d", code)
	}
	if !codeShape.MatchString(l.Code) || !strings.HasPrefix(l.Token, runnerTokenPrefix) || !strings.HasPrefix(l.Login, "lgn_") {
		t.Fatalf("login answer: %+v", l)
	}
	return l
}

func loginState(t *testing.T, srv *httptest.Server, tok string) string {
	t.Helper()
	var st LoginState
	if code := send(t, srv, tok, "GET", "/api/v0/runner/login", nil, &st); code != 200 {
		t.Fatalf("login state: %d", code)
	}
	return st.State
}

// A machine that signs in holds a token that works for nothing until the
// person approves it with the code the machine shows: it takes no job, sends
// no heartbeat, reads no settings, and is listed nowhere a job is routed by.
func TestLoginNeedsApproval(t *testing.T) {
	c := newClock()
	srv, s, q := loginEnv(t, filepath.Join(t.TempDir(), "jobs.db"), c)
	l := startLogin(t, srv)
	makeJob(t, q, queue.Spec{Harness: "claude", Prompt: "hi"})

	for _, r := range []struct{ method, path string }{
		{"POST", "/api/v0/runner/take"}, {"POST", "/api/v0/runner/heartbeat"}, {"GET", "/api/v0/runner/settings"},
	} {
		if code := send(t, srv, l.Token, r.method, r.path, map[string]any{"wait_ms": 0, "protocol": ProtocolVersion, "capacity": 1}, nil); code != 401 {
			t.Errorf("%s %s before approval: %d, want 401", r.method, r.path, code)
		}
	}
	if st := loginState(t, srv, l.Token); st != queue.LoginPending {
		t.Fatalf("state %q, want pending", st)
	}
	if rs, _ := q.Runners(queue.DefaultTeam); len(rs) != 0 {
		t.Fatalf("a waiting machine is a runner: %v", rs)
	}
	if fs, _ := q.Folders(queue.DefaultTeam); len(fs) != 0 {
		t.Fatalf("a waiting machine's folders are listed: %v", fs)
	}
	pend, _ := q.PendingLogins(queue.DefaultTeam)
	if len(pend) != 1 || pend[0].Name != "laptop" || pend[0].Caps == nil || pend[0].Caps.Sandbox == nil || pend[0].Caps.Sandbox.Why != "bwrap is missing" {
		t.Fatalf("pending: %+v", pend)
	}

	if _, err := s.ApproveLogin(l.Login, "BBBB-BBBB"); !errors.Is(err, queue.ErrWrongCode) && l.Code != "BBBB-BBBB" {
		t.Fatalf("a wrong code: %v", err)
	}
	if _, err := s.ApproveLogin("lgn_0000000000000000", l.Code); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("no such sign-in: %v", err)
	}
	// Typed in lower case, without the dash.
	rn, err := s.ApproveLogin(l.Login, strings.ToLower(strings.ReplaceAll(l.Code, "-", "")))
	if err != nil {
		t.Fatal(err)
	}
	var st LoginState
	if send(t, srv, l.Token, "GET", "/api/v0/runner/login", nil, &st); st.State != queue.LoginApproved || st.Runner == nil || st.Runner.ID != rn.ID || st.Heartbeat == 0 {
		t.Fatalf("approved state: %+v", st)
	}
	var tw TakenWork
	if code := send(t, srv, l.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &tw); code != 200 || tw.Job == nil {
		t.Fatalf("take once approved: %d %+v", code, tw)
	}
	if _, err := s.ApproveLogin(l.Login, l.Code); !errors.Is(err, queue.ErrBadState) {
		t.Fatalf("approved twice: %v", err)
	}
	if pend, _ := q.PendingLogins(queue.DefaultTeam); len(pend) != 0 {
		t.Fatalf("still pending: %v", pend)
	}
}

// A refused machine never becomes a runner; nor does one that waited too
// long.
func TestLoginRefusedOrExpired(t *testing.T) {
	c := newClock()
	srv, s, q := loginEnv(t, filepath.Join(t.TempDir(), "jobs.db"), c)
	a, b := startLogin(t, srv), startLogin(t, srv)
	if err := s.RefuseLogin(a.Login); err != nil {
		t.Fatal(err)
	}
	if st := loginState(t, srv, a.Token); st != queue.LoginRefused {
		t.Fatalf("state %q, want refused", st)
	}
	if _, err := s.ApproveLogin(a.Login, a.Code); !errors.Is(err, queue.ErrBadState) {
		t.Fatalf("approve after refuse: %v", err)
	}
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, nil); code != 401 {
		t.Fatalf("take after refuse: %d", code)
	}
	c.add(queue.LoginTTL)
	if st := loginState(t, srv, b.Token); st != queue.LoginExpired {
		t.Fatalf("state %q, want expired", st)
	}
	if _, err := s.ApproveLogin(b.Login, b.Code); !errors.Is(err, queue.ErrBadState) {
		t.Fatalf("approve once expired: %v", err)
	}
	if pend, _ := q.PendingLogins(queue.DefaultTeam); len(pend) != 0 {
		t.Fatalf("pending: %v", pend)
	}
	if rs, _ := q.Runners(queue.DefaultTeam); len(rs) != 0 {
		t.Fatalf("runners: %v", rs)
	}
	if code := send(t, srv, "abr_not-a-sign-in", "GET", "/api/v0/runner/login", nil, nil); code != 401 {
		t.Fatalf("an unknown token: %d", code)
	}
}

// At most MaxPendingLogins wait at once.
func TestLoginBounded(t *testing.T) {
	srv, _, _ := loginEnv(t, filepath.Join(t.TempDir(), "jobs.db"), newClock())
	for range queue.MaxPendingLogins {
		startLogin(t, srv)
	}
	if code := send(t, srv, "", "POST", "/api/v0/runner/login", EnrollRequest{Name: "x", Capacity: 1, Protocol: ProtocolVersion}, nil); code != 429 {
		t.Fatalf("one more: %d, want 429", code)
	}
	for _, name := range []string{"bad\nname", "bell\x07"} {
		if code := send(t, srv, "", "POST", "/api/v0/runner/login", EnrollRequest{Name: name, Capacity: 1, Protocol: ProtocolVersion}, nil); code != 400 {
			t.Fatalf("name %q: %d, want 400", name, code)
		}
	}
}

// An approved machine keeps working when control restarts.
func TestLoginSurvivesRestart(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "jobs.db")
	srv, s, q := loginEnv(t, path, c)
	l := startLogin(t, srv)
	waiting := startLogin(t, srv)
	if _, err := s.ApproveLogin(l.Login, l.Code); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	q.Close()

	srv2, s2, _ := loginEnv(t, path, c)
	if code := send(t, srv2, l.Token, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 1}, nil); code != 200 {
		t.Fatalf("heartbeat after restart: %d", code)
	}
	// One still waiting waits on, and can be approved after the restart.
	if _, err := s2.ApproveLogin(waiting.Login, waiting.Code); err != nil {
		t.Fatal(err)
	}
}

// A removed runner's token works no more and it leaves the list; its row,
// and so its jobs' history, stays. An online runner is not removed.
func TestRemoveRunner(t *testing.T) {
	c := newClock()
	srv, s, q := loginEnv(t, filepath.Join(t.TempDir(), "jobs.db"), c)
	e := enroll(t, srv, 1)
	l := startLogin(t, srv)
	rn, err := s.ApproveLogin(l.Login, l.Code)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRunner(e.Runner.ID); !errors.Is(err, queue.ErrRunnerOnline) {
		t.Fatalf("remove online: %v", err)
	}
	c.add(2 * queue.RunnerFresh)
	for _, id := range []string{e.Runner.ID, rn.ID} {
		if err := s.RemoveRunner(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveRunner(e.Runner.ID); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	for _, tok := range []string{e.Token, l.Token} {
		if code := send(t, srv, tok, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 1}, nil); code != 401 {
			t.Fatalf("heartbeat after removal: %d", code)
		}
	}
	if st := loginState(t, srv, l.Token); st != "removed" {
		t.Fatalf("state %q, want removed", st)
	}
	if rs, _ := q.Runners(queue.DefaultTeam); len(rs) != 0 {
		t.Fatalf("listed: %v", rs)
	}
	if _, err := q.GetRunner(queue.DefaultTeam, e.Runner.ID); err != nil {
		t.Fatalf("the removed runner's row is gone: %v", err)
	}
}

// The enrollment token still adds a runner at once, with no approval.
func TestEnrollTokenStillWorks(t *testing.T) {
	srv, _, q := loginEnv(t, filepath.Join(t.TempDir(), "jobs.db"), newClock())
	e := enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Prompt: "hi"})
	var tw TakenWork
	if code := send(t, srv, e.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &tw); code != 200 || tw.Job == nil {
		t.Fatalf("take: %d", code)
	}
	if code := send(t, srv, e.Token, "GET", "/api/v0/runner/login", nil, nil); code != 401 {
		t.Fatalf("an enrolled runner's token is no sign-in: %d", code)
	}
}

func TestCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := ShowCode(newCode())
		if !codeShape.MatchString(c) {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 199 {
		t.Fatalf("codes repeat: %d of 200", len(seen))
	}
	if normalCode(" bcdf-ghjk ") != "BCDFGHJK" {
		t.Fatal("normalCode")
	}
}
