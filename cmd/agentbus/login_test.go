package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
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
		service: serviceSetup{goos: "test"},
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
// command where it sets up no service. The machine took no job before.
func TestLoginApproved(t *testing.T) {
	srv, js, q := loginControl(t)
	home := filepath.Join(t.TempDir(), "data")
	var out, errb syncBuf
	done := make(chan int)
	go func() {
		done <- runLogin([]string{srv.URL, "-name", "box", "-folder", "/srv/app", "-no-service"}, home, func(string) string { return "" },
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
		!strings.Contains(out.String(), "/opt/agentbus/agentbus runner -server "+srv.URL+" -name box -folder /srv/app") {
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
	if c := runLogin([]string{srv.URL}, home, func(string) string { return "" }, &b2, &b2, testDeps(srv, context.Background())); c != 1 ||
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
		done <- runLogin([]string{srv.URL}, home, func(string) string { return "" }, &out, &errb, testDeps(srv, context.Background()))
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
		if code := runLogin(c.args, c.home, func(string) string { return "" }, &b, &b, testDeps(srv, context.Background())); code == 0 ||
			!strings.Contains(b.String(), c.want) {
			t.Errorf("%v: exit %d, %q; want %q", c.args, code, b.String(), c.want)
		}
	}
	if pend, _ := q.PendingLogins(queue.DefaultTeam); len(pend) != 0 {
		t.Fatalf("asked control: %v", pend)
	}
}

// The systemd unit and the launchd agent run the runner's command line as
// it was given, quoted for each, with the data folder and PATH.
func TestServiceFiles(t *testing.T) {
	cmd := []string{"/opt/a b/agentbus", "runner", "-server", "http://h:8787", "-name", `it's "100%" $HOME`}
	u := unitText(cmd, "/home/me/data", "/usr/bin:/home/me/.local/bin")
	for _, want := range []string{
		`ExecStart="/opt/a b/agentbus" "runner" "-server" "http://h:8787" "-name" "it's \"100%%\" $$HOME"`,
		`Environment="AGENTBUS_HOME=/home/me/data"`, `Environment="PATH=/usr/bin:/home/me/.local/bin"`, "WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %s:\n%s", want, u)
		}
	}
	p := plistText(append(cmd, "<&>"), "/Users/me/data", "/usr/bin", "/Users/me/data/logs/runner-launchd.log")
	var doc struct {
		Strings []string `xml:"dict>array>string"`
	}
	if err := xml.Unmarshal([]byte(p), &doc); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, p)
	}
	if got := strings.Join(doc.Strings, "|"); got != strings.Join(append(cmd, "<&>"), "|") {
		t.Fatalf("ProgramArguments %q", got)
	}
}

// A unit already there, such as one set up by hand, is left as it is, and
// nothing is started; with none, the unit is written and started.
func TestSystemdInstall(t *testing.T) {
	home := t.TempDir()
	var ran []string
	s := serviceSetup{goos: "linux", userHome: home, lookPath: func(string) (string, error) { return "/usr/bin/systemctl", nil },
		run: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil
		}}
	did, err := s.install([]string{"/bin/agentbus", "runner", "-server", "http://h"}, "/d", "/usr/bin")
	if err != nil || !strings.Contains(did, "systemctl --user status agentbus-runner.service") {
		t.Fatalf("install: %v %q", err, did)
	}
	if strings.Join(ran, ";") != "systemctl --user show-environment;systemctl --user daemon-reload;systemctl --user enable --now agentbus-runner.service" {
		t.Fatalf("ran %q", ran)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
	before, _ := os.ReadFile(unit)
	ran = nil
	if _, err := s.install([]string{"/other"}, "/d", ""); err == nil || !strings.Contains(err.Error(), "left as it is") {
		t.Fatalf("a second install: %v", err)
	}
	after, _ := os.ReadFile(unit)
	if !bytes.Equal(before, after) || len(ran) != 1 {
		t.Fatalf("the unit there was changed, or a start ran: %q", ran)
	}
	// No user service manager: nothing written.
	s.userHome = t.TempDir()
	s.run = func(string, ...string) error { return errors.New("no bus") }
	if _, err := s.install([]string{"/bin/agentbus"}, "/d", ""); err == nil {
		t.Fatal("installed with no service manager")
	}
	if _, err := os.Stat(filepath.Join(s.userHome, ".config")); !os.IsNotExist(err) {
		t.Fatal("a unit was written with no service manager")
	}
}

func TestLaunchdInstall(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	var ran []string
	s := serviceSetup{goos: "darwin", userHome: home, uid: 501,
		run: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil
		}}
	if _, err := s.install([]string{"/bin/agentbus", "runner"}, data, "/usr/bin"); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", launchdPlist)
	if len(ran) != 1 || ran[0] != "launchctl bootstrap gui/501 "+plist {
		t.Fatalf("ran %q", ran)
	}
	if _, err := s.install([]string{"/bin/agentbus"}, data, ""); err == nil {
		t.Fatal("an agent there was replaced")
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
