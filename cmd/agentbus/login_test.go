package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/runner"
)

// syncBuf is a buffer two goroutines share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// loginControl is a control's runner protocol, on a queue of its own.
func loginControl(t *testing.T) (*httptest.Server, *jobs.Server, *queue.Queue) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	js, err := jobs.New(jobs.Config{Queue: q, Token: "abm_main", EnrollToken: "abe_enroll"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(js)
	t.Cleanup(func() { srv.Close(); q.Close() })
	return srv, js, q
}

func testDeps(srv *httptest.Server, ctx context.Context) loginDeps {
	return loginDeps{http: srv.Client(), self: "/opt/agentbus/agentbus", poll: 20 * time.Millisecond,
		caps: func() *queue.Capabilities {
			return &queue.Capabilities{OS: "linux"}
		},
		signals: func(context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }}
}

var shownCode = regexp.MustCompile(`\n    ([A-Z]{4}-[A-Z]{4})\n`)

// waitCode waits for agentbus login to show its code.
func waitCode(t *testing.T, out *syncBuf) string {
	t.Helper()
	for range 200 {
		if m := shownCode.FindStringSubmatch(out.String()); m != nil {
			return m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no code shown: %q", out.String())
	return ""
}

// What agentbus login refuses before it asks control anything.
func TestLoginRefusesLines(t *testing.T) {
	srv, _, q := loginControl(t)
	control := t.TempDir()
	os.WriteFile(filepath.Join(control, "jobs.db"), nil, 0o600)
	for _, c := range []struct {
		args []string
		home string
		want string
	}{
		{nil, t.TempDir(), "needs control's address"},
		{[]string{"-name", "x"}, t.TempDir(), "needs control's address"},
		{[]string{"ftp://x"}, t.TempDir(), "-server must be the server's URL"},
		{[]string{srv.URL, "-server", "http://y"}, t.TempDir(), "first argument"},
		{[]string{srv.URL, "-enroll-token-file", "/x"}, t.TempDir(), "needs none"},
		{[]string{srv.URL}, control, "a control's data folder"},
	} {
		var b bytes.Buffer
		if code := runLogin(c.args, c.home, &b, &b, testDeps(srv, context.Background())); code == 0 ||
			!strings.Contains(b.String(), c.want) {
			t.Errorf("%v: exit %d, %q; want %q", c.args, code, b.String(), c.want)
		}
	}
	if pend, _ := q.PendingLogins(queue.DefaultTeam); len(pend) != 0 {
		t.Fatalf("asked control: %v", pend)
	}
}

// tokenLog is a transport that keeps the Authorization header of every
// request it carries.
type tokenLog struct {
	mu   sync.Mutex
	auth []string
	next http.RoundTripper
}

func (l *tokenLog) RoundTrip(r *http.Request) (*http.Response, error) {
	l.mu.Lock()
	l.auth = append(l.auth, r.Header.Get("Authorization"))
	l.mu.Unlock()
	return l.next.RoundTrip(r)
}

// signInRunner signs a machine with data folder home in to the control at
// base, as agentbus login and the person on the Runners page do together:
// the sign-in, its approval with the main token, and the identity saved.
func signInRunner(t *testing.T, base, mainToken, home, name string) {
	t.Helper()
	send := func(method, path, tok string, body, out any) {
		t.Helper()
		var rd *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		} else {
			rd = strings.NewReader("")
		}
		req, _ := http.NewRequest(method, base+path, rd)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: HTTP %d", method, path, resp.StatusCode)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
	}
	var started jobs.LoginStarted
	send("POST", "/api/v0/runner/login", "", jobs.EnrollRequest{Name: name, Capacity: 1, Protocol: jobs.ProtocolVersion}, &started)
	send("POST", "/api/v0/runners/pending/"+started.Login+"/approve", mainToken, map[string]string{"code": started.Code}, nil)
	var st jobs.LoginState
	send("GET", "/api/v0/runner/login", started.Token, nil, &st)
	if st.State != queue.LoginApproved || st.Runner == nil {
		t.Fatalf("sign-in of %s: %+v", name, st)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runner.SaveIdentity(filepath.Join(home, runnerState), *st.Runner, started.Token, st.Heartbeat, st.Lease); err != nil {
		t.Fatal(err)
	}
}
