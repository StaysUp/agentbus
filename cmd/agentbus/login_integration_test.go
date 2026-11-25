//go:build integration

package main

// agentbus login against a control: these wait on real time (the login
// polls control, and waitCode polls its output), so they run in make gate
// only. Their helpers are in login_test.go.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/runner"
)

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
	if len(pend) != 1 || pend[0].Name != "box" || pend[0].Caps == nil || pend[0].Caps.OS != "linux" {
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

// A sign-in approved over an old one whose new identity cannot be written
// puts the old one back, so the machine is never left with none (review of
// 0.5.1 L1, 815ce20a).
func TestLoginPutsTheOldIdentityBack(t *testing.T) {
	srv, js, q := loginControl(t)
	home := filepath.Join(t.TempDir(), "data")
	login := func(d loginDeps) (int, string) {
		t.Helper()
		var out, errb syncBuf
		done := make(chan int)
		go func() { done <- runLogin([]string{srv.URL, "-name", "box"}, home, &out, &errb, d) }()
		code := waitCode(t, &out)
		pend, _ := q.PendingLogins(queue.DefaultTeam)
		if len(pend) != 1 {
			t.Fatalf("pending %v", pend)
		}
		if _, err := js.ApproveLogin(pend[0].ID, code); err != nil {
			t.Fatal(err)
		}
		return <-done, out.String() + errb.String()
	}
	if c, o := login(testDeps(srv, context.Background())); c != 0 {
		t.Fatalf("first: %d %s", c, o)
	}
	statePath := filepath.Join(home, runnerState)
	before, _ := os.ReadFile(statePath)
	failing := testDeps(srv, context.Background())
	failing.save = func(string, queue.Runner, string, int64, int64) error { return errors.New("disk full") }
	c, o := login(failing)
	if c != 1 || !strings.Contains(o, "The sign-in that was there is back in place.") {
		t.Fatalf("failed write: %d %s", c, o)
	}
	if now, err := os.ReadFile(statePath); err != nil || !bytes.Equal(now, before) {
		t.Fatalf("runner.json after a failed write: %q %v", now, err)
	}
	if kept, _ := filepath.Glob(statePath + ".replaced-*"); len(kept) != 0 {
		t.Fatalf("left aside: %v", kept)
	}
}
