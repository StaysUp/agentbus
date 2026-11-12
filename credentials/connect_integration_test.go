//go:build integration

package credentials

import (
	"errors"
	"regexp"
	"slices"
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
	// Only the silent login waits out AnswerWait: the others end by
	// themselves first, however slow the machine.
	for name, c := range map[string]struct {
		script, want string
		rejected     bool
		answer       time.Duration
	}{
		"exits": {`echo "Open https://example.test/auth"
read code
[ "$code" = "c0de" ] || { echo "bad code"; exit 1; }`, "exit 1", false, time.Minute},
		"waits": {`echo "Open https://example.test/auth"
read code
printf '\rOAuth error: That code was not accepted.\r\rPress Enter to retry.\r'
sleep 60 &
read again`, "code was rejected", true, time.Minute},
		// claude setup-token 2.1.321 given the right state and a wrong code
		// (2026-11-12): nothing more is printed, and it keeps running.
		"silent": {`echo "Open https://example.test/auth"
read code
sleep 60 &
read again`, "gave no token within", true, 300 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			l := fakeLogin(t, c.script)
			l.Refused = regexp.MustCompile(`OAuth\s*error`)
			m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, AnswerWait: c.answer, Logins: map[string]Login{"claude": l}}
			if _, err := m.Connect("claude"); err != nil {
				t.Fatal(err)
			}
			if err := m.Input("claude", "wrong"); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			// A failed connect keeps why, and no link.
			if v := waitState(t, m, "claude"); v.State != Failed || !strings.Contains(v.Error, c.want) || len(v.Output) != 0 || v.Paste || v.Rejected != c.rejected {
				t.Fatalf("%+v", v)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %v", d)
			}
		})
	}
}

// Cancel stops a running login: the connect fails as cancelled, not as
// rejected, and a second Cancel finds none running.
func TestConnectCancel(t *testing.T) {
	var log lines
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, Logf: log.logf,
		Logins: map[string]Login{"claude": fakeLogin(t, `echo "Open https://example.test/auth"
sleep 60 &
read code`)}}
	if err := m.Cancel("claude"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel with none: %v", err)
	}
	if _, err := m.Connect("claude"); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("claude"); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, m, "claude")
	if v.State != Failed || !v.Cancelled || v.Rejected || !strings.Contains(v.Error, "cancelled") {
		t.Fatalf("%+v", v)
	}
	if err := m.Cancel("claude"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second cancel: %v", err)
	}
	if got := log.all(); !slices.Contains(got, "subscription.cancelled") {
		t.Fatalf("log %q", got)
	}
}

// A cancel that lands after the login printed its token keeps nothing: a
// Cancel that succeeded never leaves the subscription connected.
func TestConnectCancelAfterTokenKeepsNothing(t *testing.T) {
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()},
		Logins: map[string]Login{"claude": fakeLogin(t, `echo "Open https://example.test/auth"
echo "Token: `+fakeToken+`"
sleep 60 &
read code`)}}
	if _, err := m.Connect("claude"); err != nil {
		t.Fatal(err)
	}
	// The token is printed; the login still runs.
	time.Sleep(300 * time.Millisecond)
	if err := m.Cancel("claude"); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, m, "claude"); v.State != Failed || !v.Cancelled {
		t.Fatalf("%+v", v)
	}
	if st, _ := m.Store.Status("claude"); st.Connected {
		t.Fatalf("a cancelled connect kept its token: %+v", st)
	}
}

// A login that takes the code and prints its token after a moment, under
// AnswerWait, connects.
func TestConnectAnswerInTime(t *testing.T) {
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, AnswerWait: 2 * time.Second,
		Logins: map[string]Login{"claude": fakeLogin(t, `echo "Open https://example.test/auth"
read code
sleep 0.3
echo "Token: `+fakeToken+`"`)}}
	if _, err := m.Connect("claude"); err != nil {
		t.Fatal(err)
	}
	if err := m.Input("claude", "c0de"); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, m, "claude"); v.State != Connected {
		t.Fatalf("%+v", v)
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
