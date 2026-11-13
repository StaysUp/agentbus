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
			return &queue.Capabilities{OS: "linux", Sandbox: &queue.SandboxReport{Can: false, Why: "bwrap is missing"}}
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

// agentbus login shows a code, waits for the person to approve it, then
// saves the runner identity agentbus runner starts with, and prints the
// one line that starts it. It sets up no service. The machine took no job
// before.
func TestLoginApproved(t *testing.T) {
	srv, js, q := loginControl(t)
	home := filepath.Join(t.TempDir(), "data")
	var out, errb syncBuf
	done := make(chan int)
	go func() {
		done <- runLogin([]string{srv.URL, "-name", "box", "-folder", "/srv/app"}, home,
			&out, &errb, testDeps(srv, context.Background()))
	}()
	code := waitCode(t, &out)
	pend, _ := q.PendingLogins(queue.DefaultTeam)
	if len(pend) != 1 || pend[0].Name != "box" || pend[0].Caps.Sandbox == nil || pend[0].Caps.Sandbox.Why != "bwrap is missing" {
		t.Fatalf("pending: %+v", pend)
	}
	if signedIn(home) {
		t.Fatal("an identity was saved before approval")
	}
	rn, err := js.ApproveLogin(pend[0].ID, code)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-done:
		if c != 0 {
			t.Fatalf("exit %d: %s", c, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("login did not see the approval")
	}
	if !signedIn(home) {
		t.Fatal("no identity saved")
	}
	info, _ := os.Stat(filepath.Join(home, runnerState))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("runner.json mode %v", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), rn.ID) ||
		!strings.Contains(out.String(), "Start the runner with: AGENTBUS_HOME="+home+" /opt/agentbus/agentbus runner -server "+srv.URL+" -name box -folder /srv/app\n") {
		t.Fatalf("out: %s", out.String())
	}
	// The saved token is the runner's: control takes its heartbeat.
	b, _ := os.ReadFile(filepath.Join(home, runnerState))
	tok := regexp.MustCompile(`"token":"([^"]+)"`).FindSubmatch(b)
	req, _ := http.NewRequest("POST", srv.URL+"/api/v0/runner/heartbeat", strings.NewReader(`{"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+string(tok[1]))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("heartbeat with the saved token: %v %v", err, resp)
	}
	resp.Body.Close()
}

// A refused sign-in saves nothing.
func TestLoginRefused(t *testing.T) {
	srv, js, q := loginControl(t)
	home := filepath.Join(t.TempDir(), "data")
	var out, errb syncBuf
	done := make(chan int)
	go func() {
		done <- runLogin([]string{srv.URL}, home, &out, &errb, testDeps(srv, context.Background()))
	}()
	waitCode(t, &out)
	pend, _ := q.PendingLogins(queue.DefaultTeam)
	if err := js.RefuseLogin(pend[0].ID); err != nil {
		t.Fatal(err)
	}
	if c := <-done; c != 1 || !strings.Contains(errb.String(), "refused") {
		t.Fatalf("exit %d: %s", c, errb.String())
	}
	if signedIn(home) {
		t.Fatal("an identity was saved")
	}
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

// agentbus login on a data folder that holds a sign-in starts a new one. It
// never sends the old token, nor shows it; once approved it keeps the old
// file beside the new one and names the old runner. Refused, it changes
// nothing.
func TestLoginReplacesIdentity(t *testing.T) {
	srv, js, q := loginControl(t)
	home := filepath.Join(t.TempDir(), "data")
	login := func(log *tokenLog, approve bool) (int, string) {
		t.Helper()
		var out, errb syncBuf
		d := testDeps(srv, context.Background())
		if log != nil {
			log.next = srv.Client().Transport
			d.http = &http.Client{Transport: log}
		}
		done := make(chan int)
		go func() { done <- runLogin([]string{srv.URL, "-name", "box"}, home, &out, &errb, d) }()
		code := waitCode(t, &out)
		pend, _ := q.PendingLogins(queue.DefaultTeam)
		if len(pend) != 1 {
			t.Fatalf("pending %v", pend)
		}
		if approve {
			if _, err := js.ApproveLogin(pend[0].ID, code); err != nil {
				t.Fatal(err)
			}
		} else if err := js.RefuseLogin(pend[0].ID); err != nil {
			t.Fatal(err)
		}
		return <-done, out.String() + errb.String()
	}
	if c, o := login(nil, true); c != 0 {
		t.Fatalf("first: %d %s", c, o)
	}
	statePath := filepath.Join(home, runnerState)
	before, _ := os.ReadFile(statePath)
	oldID := runner.SavedID(statePath)
	oldTok := regexp.MustCompile(`"token":"([^"]+)"`).FindSubmatch(before)
	if oldID == "" || oldTok == nil {
		t.Fatalf("first identity: %s", before)
	}

	// Refused: nothing changes.
	log := &tokenLog{}
	if c, o := login(log, false); c != 1 {
		t.Fatalf("refused: %d %s", c, o)
	}
	if now, _ := os.ReadFile(statePath); !bytes.Equal(now, before) {
		t.Fatal("a refused sign-in changed runner.json")
	}
	if kept, _ := filepath.Glob(statePath + ".replaced-*"); len(kept) != 0 {
		t.Fatalf("a refused sign-in kept a file aside: %v", kept)
	}

	// Approved: the new identity is saved, the old one kept beside it.
	c, o := login(log, true)
	if c != 0 {
		t.Fatalf("again: %d %s", c, o)
	}
	newID := runner.SavedID(statePath)
	if newID == "" || newID == oldID {
		t.Fatalf("not replaced: %q", newID)
	}
	kept, _ := filepath.Glob(statePath + ".replaced-*")
	if len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
	if b, _ := os.ReadFile(kept[0]); !bytes.Equal(b, before) {
		t.Fatalf("the old identity was not kept whole: %q", b)
	}
	if !strings.Contains(o, "The old runner "+oldID+" may still be listed at control; remove it on the Runners page if so.") {
		t.Fatalf("out: %s", o)
	}
	if strings.Contains(o, string(oldTok[1])) {
		t.Fatal("the old token was shown")
	}
	for _, a := range log.auth {
		if strings.Contains(a, string(oldTok[1])) {
			t.Fatal("a request carried the old token")
		}
	}
	if len(log.auth) == 0 {
		t.Fatal("no request was seen")
	}
}

// Two logins on one data folder at once: the second is refused before it
// asks control anything; and a relative data folder is saved, and handed
// to the printed command, whole (review of 2026-11-12, round 1).
func TestLoginOneAtATime(t *testing.T) {
	srv, js, q := loginControl(t)
	dir := t.TempDir()
	t.Chdir(dir)
	var out1, err1 syncBuf
	done1 := make(chan int)
	go func() {
		done1 <- runLogin([]string{srv.URL, "-name", "one"}, "data", &out1, &err1, testDeps(srv, context.Background()))
	}()
	code1 := waitCode(t, &out1)
	var b bytes.Buffer
	if c := runLogin([]string{srv.URL, "-name", "two"}, "data", &b, &b, testDeps(srv, context.Background())); c != 1 ||
		!strings.Contains(b.String(), "another login is running here") {
		t.Fatalf("second: %d %s", c, b.String())
	}
	pend, _ := q.PendingLogins(queue.DefaultTeam)
	if len(pend) != 1 || pend[0].Name != "one" {
		t.Fatalf("pending %v", pend)
	}
	first, err := js.ApproveLogin(pend[0].ID, code1)
	if err != nil {
		t.Fatal(err)
	}
	if c := <-done1; c != 0 {
		t.Fatalf("first: %d %s", c, err1.String())
	}
	if id := runner.SavedID(filepath.Join(dir, "data", runnerState)); id != first.ID {
		t.Fatalf("saved %q, want %q", id, first.ID)
	}
	if !strings.Contains(out1.String(), "AGENTBUS_HOME="+filepath.Join(dir, "data")) {
		t.Fatalf("the data folder is not whole in the command: %s", out1.String())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "data", "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// The lock ends with the login: the next one may start.
	b.Reset()
	if _, err := lockLogin(filepath.Join(dir, "data")); err != nil {
		t.Fatalf("the lock outlived the login: %v", err)
	}
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
