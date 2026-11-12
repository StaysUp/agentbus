package main

import (
	"bytes"
	"context"
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

	// A second login on the same data folder is refused, and keeps the first.
	var b2 bytes.Buffer
	if c := runLogin([]string{srv.URL}, home, &b2, &b2, testDeps(srv, context.Background())); c != 1 ||
		!strings.Contains(b2.String(), "already holds a runner's sign-in") {
		t.Fatalf("second login: %d %s", c, b2.String())
	}
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

// A runner needs no enrollment token when agentbus login saved its identity,
// unless -enroll-token-file names one; without either it says where it
// looked.
func TestRunnerEnrollToken(t *testing.T) {
	home := t.TempDir()
	c := config{home: home, enrollFile: filepath.Join(home, "enroll_token")}
	if _, err := runnerEnrollToken(c); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("neither: %v", err)
	}
	if err := runner.SaveIdentity(filepath.Join(home, runnerState), queue.Runner{ID: "run_1"}, "abr_x", 2000, 30000); err != nil {
		t.Fatal(err)
	}
	if tok, err := runnerEnrollToken(c); err != nil || tok != "" {
		t.Fatalf("signed in: %q %v", tok, err)
	}
	c.enrollFileSet = true
	if _, err := runnerEnrollToken(c); err == nil {
		t.Fatal("a named token file that is not there was passed over")
	}
	c.enrollFileSet = false
	os.WriteFile(c.enrollFile, []byte("abe_tok\n"), 0o600)
	if tok, err := runnerEnrollToken(c); err != nil || tok != "abe_tok" {
		t.Fatalf("the old way: %q %v", tok, err)
	}
}

// Of two logins on one data folder, the one approved second does not replace
// the first one's identity; and a relative data folder is saved, and handed
// to the printed command, whole (review of 2026-11-12, round 1).
func TestLoginKeepsFirstIdentity(t *testing.T) {
	srv, js, q := loginControl(t)
	dir := t.TempDir()
	t.Chdir(dir)
	var out1, out2, err1, err2 syncBuf
	done1, done2 := make(chan int), make(chan int)
	go func() {
		done1 <- runLogin([]string{srv.URL, "-name", "one"}, "data", &out1, &err1, testDeps(srv, context.Background()))
	}()
	code1 := waitCode(t, &out1)
	go func() {
		done2 <- runLogin([]string{srv.URL, "-name", "two"}, "data", &out2, &err2, testDeps(srv, context.Background()))
	}()
	code2 := waitCode(t, &out2)
	pend, _ := q.PendingLogins(queue.DefaultTeam)
	if len(pend) != 2 {
		t.Fatalf("pending %v", pend)
	}
	id := map[string]string{}
	for _, p := range pend {
		id[p.Name] = p.ID
	}
	first, err := js.ApproveLogin(id["one"], code1)
	if err != nil {
		t.Fatal(err)
	}
	if c := <-done1; c != 0 {
		t.Fatalf("first: %d %s", c, err1.String())
	}
	if _, err := js.ApproveLogin(id["two"], code2); err != nil {
		t.Fatal(err)
	}
	if c := <-done2; c != 1 || !strings.Contains(err2.String(), "was kept") {
		t.Fatalf("second: %d %s", c, err2.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "data", runnerState))
	if !strings.Contains(string(b), first.ID) {
		t.Fatalf("the first identity was replaced: %s", b)
	}
	if !strings.Contains(out1.String(), "AGENTBUS_HOME="+filepath.Join(dir, "data")) {
		t.Fatalf("the data folder is not whole in the command: %s", out1.String())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "data", "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}
