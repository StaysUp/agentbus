//go:build integration

package credentials

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// lines keeps what a Manager logs.
type lines struct {
	mu  sync.Mutex
	got []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, strings.Fields(strings.ReplaceAll(format, "%s", "X"))[0])
}

func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.got...)
}

// The login reads its code from a terminal, as claude setup-token does: a
// pipe as stdin or stdout makes it fail. The connect reaches it, the code
// typed is never echoed back, and each step is logged without a value.
func TestConnectRunsTheLoginInATerminal(t *testing.T) {
	var log lines
	var ended []string
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, Logf: log.logf,
		Ended: func(h, state, why string) { ended = append(ended, h+" "+state) },
		Logins: map[string]Login{"claude": fakeLogin(t, `[ -t 0 ] && [ -t 1 ] || exit 4
echo "Open https://example.test/auth"
read code
[ "$code" = "c0de" ] || exit 3
echo "Token: `+fakeToken+`"`)}}
	if _, err := m.Connect("claude"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if v, _ := m.View("claude"); len(v.Output) == 1 || v.State != Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no link")
		}
	}
	if err := m.Input("claude", "c0de"); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, m, "claude")
	if v.State != Connected || len(v.Output) != 0 {
		t.Fatalf("%+v", v)
	}
	want := []string{"subscription.login_started", "subscription.link_shown", "subscription.input_received", "subscription.connected"}
	if got := log.all(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("log %q", got)
	}
	if len(ended) != 1 || ended[0] != "claude connected" {
		t.Fatalf("ended %q", ended)
	}
}

// A wrong code ends the connect as failed, with the reason: a login that
// exits, and one that says so and waits to be asked again, as claude
// setup-token 2.1.322 does (2026-11-10).
func TestConnectWrongCodeFails(t *testing.T) {
	for name, c := range map[string]struct{ script, want string }{
		"exits": {`echo "Open https://example.test/auth"
read code
[ "$code" = "c0de" ] || { echo "bad code"; exit 1; }`, "exit 1"},
		"waits": {`echo "Open https://example.test/auth"
read code
printf '\rOAuth error: That code was not accepted.\r\rPress Enter to retry.\r'
sleep 60 &
read again`, "did not accept the code"},
	} {
		t.Run(name, func(t *testing.T) {
			l := fakeLogin(t, c.script)
			l.Refused = regexp.MustCompile(`OAuth\s*error`)
			m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, Logins: map[string]Login{"claude": l}}
			if _, err := m.Connect("claude"); err != nil {
				t.Fatal(err)
			}
			if err := m.Input("claude", "wrong"); err != nil {
				t.Fatal(err)
			}
			// A failed connect keeps why, and no link.
			if v := waitState(t, m, "claude"); v.State != Failed || !strings.Contains(v.Error, c.want) || len(v.Output) != 0 || v.Paste {
				t.Fatalf("%+v", v)
			}
		})
	}
}

// A login left waiting for a code is stopped after the Manager's Wait, with
// whatever it started, and the connect fails saying so.
func TestConnectWaitsTooLong(t *testing.T) {
	var log lines
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, Wait: 300 * time.Millisecond, Logf: log.logf,
		Logins: map[string]Login{"claude": fakeLogin(t, `echo "Open https://example.test/auth"
sleep 60 &
read code`)}}
	if _, err := m.Connect("claude"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	v := waitState(t, m, "claude")
	if v.State != Failed || !strings.Contains(v.Error, "waited too long") {
		t.Fatalf("%+v", v)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	if got := log.all(); got[len(got)-1] != "subscription.failed" {
		t.Fatalf("log %q", got)
	}
}

// A login program that is not there is named, with the PATH searched.
func TestConnectNotOnPath(t *testing.T) {
	t.Setenv("PATH", "/nowhere/bin")
	l := Login{Path: "agentbus-no-such-login", Token: fakeLogin(t, "").Token}
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, Logins: map[string]Login{"claude": l}}
	_, err := m.Connect("claude")
	var missing *Missing
	want := "agentbus-no-such-login is not on PATH (searched: /nowhere/bin); add the folder that holds agentbus-no-such-login to PATH"
	if !errors.As(err, &missing) || missing.Why != want {
		t.Fatalf("%v", err)
	}
}
