//go:build integration

package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The file a fake agy keeps its sign-in in, under its HOME, as agy 1.5.5 does.
const fakeAgyFile = ".gemini/antigravity-cli/antigravity-oauth-token"

// agyJSON is a sign-in file as agy writes it.
func agyJSON(access string, exp time.Time) string {
	return `{"token":{"access_token":"` + access + `","token_type":"Bearer","refresh_token":"fake-agy-refresh","expiry":"` +
		exp.UTC().Format(time.RFC3339Nano) + `","expires_in":3599},"auth_method":"consumer","id_token_email":"someone@example.com"}`
}

// script writes an executable shell script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// agyLogin is a fake agy acp --login: it prints a link, reads the pasted
// code, and with "good-code" keeps a sign-in in its HOME. It notes its HOME
// and whether it saw a D-Bus address in marker.
func agyLogin(t *testing.T, access string, exp time.Time, marker string) Login {
	p := script(t, `echo "$HOME ${DBUS_SESSION_BUS_ADDRESS:-nobus}" > `+marker+`
printf 'Open this link in a browser to log in with Google:\r\n\r\n  https://accounts.google.test/o/oauth2/auth?client_id=x&state=y\r\n\r\n'
printf 'Then paste the authorization code, or the full URL of the page you land on, and press Enter:\r\n'
read code
code=$(printf '%s' "$code" | tr -d '\r')
[ "$code" = good-code ] || { echo "login failed: bad code"; exit 1; }
mkdir -p "$HOME/.gemini/antigravity-cli"
umask 077
printf '%s' '`+agyJSON(access, exp)+`' > "$HOME/`+fakeAgyFile+`"
`)
	return Login{Path: p, Args: []string{"acp", "--login"}, Home: "HOME", File: fakeAgyFile, Keep: "agy-home",
		TokenKey: "token.access_token", ExpiryKey: "token.expiry",
		Env: append(os.Environ(), "DBUS_SESSION_BUS_ADDRESS=unix:path=/fake/bus"), Set: []string{"DBUS_SESSION_BUS_ADDRESS=disabled:"}}
}

// ownStore is a store whose agy renews itself by running renew, a fake agy
// models. Each run appends "<HOME> <bus or nobus> <args>" to runs.
func ownStore(t *testing.T, renew string) (s *Subscriptions, runs string) {
	t.Helper()
	runs = filepath.Join(t.TempDir(), "runs")
	p := script(t, `echo "$HOME ${DBUS_SESSION_BUS_ADDRESS:-nobus} $*" >> "$FAKE_RUNS"
`+renew)
	s = &Subscriptions{Dir: filepath.Join(t.TempDir(), "subscriptions"), Own: map[string]Own{"agy": {
		Home: "agy-home", File: fakeAgyFile, TokenKey: "token.access_token", ExpiryKey: "token.expiry",
		Within: 4 * time.Minute, Path: p, Args: []string{"models"},
		Env: append(os.Environ(), "DBUS_SESSION_BUS_ADDRESS=unix:path=/fake/bus", "FAKE_RUNS="+runs),
		Set: []string{"DBUS_SESSION_BUS_ADDRESS=disabled:"}, Refused: regexp.MustCompile(`(?i)please sign in`),
	}}}
	return s, runs
}

// connectOwn keeps a sign-in for agy as a finished login leaves it: a folder
// in the subscriptions folder that holds agy's file.
func connectOwn(t *testing.T, s *Subscriptions, access string, exp time.Time) {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(s.Dir, ".login-*")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(home, fakeAgyFile)
	os.MkdirAll(filepath.Dir(f), 0o700)
	if err := os.WriteFile(f, []byte(agyJSON(access, exp)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Keep("agy", Signin{Home: home}); err != nil {
		t.Fatal(err)
	}
}

func runsOf(t *testing.T, runs string) []string {
	b, err := os.ReadFile(runs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

var agyNeed = Need{Access: AccessSubscription, Harness: "agy"}

// Connected in the cockpit's way: the person sees only the link, types the
// code into the login's terminal, and the login's folder becomes agy's home
// on control, as agy wrote it. The login ran with a D-Bus address that
// reaches no bus, in place of the one control has. Nothing
// of the sign-in is copied beside it.
func TestOwnLoginKeepsItsHome(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	marker := filepath.Join(t.TempDir(), "marker")
	s, _ := ownStore(t, "exit 0")
	m := &Manager{Store: s, Logins: map[string]Login{"agy": agyLogin(t, "fake-agy-access-0", exp, marker)}}
	if _, err := m.Connect("agy"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v, _ := m.View("agy"); return len(v.Output) == 1 })
	if err := m.Input("agy", "good-code"); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, m, "agy")
	if v.State != Connected || len(v.Output) != 1 || v.Output[0] != "https://accounts.google.test/o/oauth2/auth?client_id=x&state=y" || v.Code != "" {
		t.Fatalf("%+v", v)
	}
	seen, _ := os.ReadFile(marker)
	f := strings.Fields(string(seen))
	if len(f) != 2 || !strings.HasPrefix(f[0], filepath.Join(s.Dir, ".login-")) || f[1] != "disabled:" {
		t.Fatalf("the login ran with %q", seen)
	}
	home := filepath.Join(s.Dir, "agy-home")
	if b, err := os.ReadFile(filepath.Join(home, fakeAgyFile)); err != nil || string(b) != agyJSON("fake-agy-access-0", exp) {
		t.Fatalf("agy's own file in its home: %v", err)
	}
	left, _ := os.ReadDir(s.Dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "agy-home agy.json" {
		t.Fatalf("the subscriptions folder holds %v", names)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, "agy.json"))
	if strings.Contains(string(b), "fake-agy") {
		t.Fatalf("agy.json holds the sign-in: %s", b)
	}
	c, err := s.Credential(agyNeed)
	if err != nil || c.Token != "fake-agy-access-0" || len(c.Headers) != 0 {
		t.Fatalf("%v %+v", err, c.Headers)
	}
	st, _ := s.Status("agy")
	if !st.Connected || !st.Renews || st.Expired || st.ExpiresAt == nil || !st.ExpiresAt.Equal(exp) {
		t.Fatalf("%+v", st)
	}
}

// In a terminal (agentbus connect agy): the link and the prompt to paste the
// code, and the login's folder is handed on with the sign-in, not removed.
func TestOwnLoginInATerminal(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	l := agyLogin(t, "fake-agy-access-0", exp, filepath.Join(t.TempDir(), "marker"))
	var out strings.Builder
	dir := t.TempDir()
	si, err := Run(context.Background(), l, dir, time.Now(), strings.NewReader("good-code\n"), &out)
	if err != nil || si.Token != "fake-agy-access-0" || si.Refresh != "" || si.Home == "" {
		t.Fatalf("%v %+v", err, si.Home)
	}
	if _, err := os.Stat(filepath.Join(si.Home, fakeAgyFile)); err != nil {
		t.Fatalf("the login's folder was not handed on: %v", err)
	}
	if s := out.String(); s != "Sign in here: https://accounts.google.test/o/oauth2/auth?client_id=x&state=y\n"+
		"Then paste the code the browser shows, and press Enter.\n" {
		t.Fatalf("%q", s)
	}
	// A wrong code: no sign-in, and no folder left.
	dir = t.TempDir()
	if _, err := Run(context.Background(), l, dir, time.Now(), strings.NewReader("bad\n"), &strings.Builder{}); err == nil {
		t.Fatal("a wrong code connected")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

// Less than four minutes left: control runs agy models in agy's home, with
// a D-Bus address that reaches no bus, and agy renews its own file. The renewed token is used,
// and the next call runs nothing.
func TestOwnRenewedByAgy(t *testing.T) {
	next := time.Now().Add(time.Hour).Truncate(time.Second)
	s, runs := ownStore(t, `printf '%s' '`+agyJSON("fake-agy-access-1", next)+`' > "$HOME/`+fakeAgyFile+`"`)
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(2*time.Minute))
	c, err := s.Credential(agyNeed)
	if err != nil || c.Token != "fake-agy-access-1" {
		t.Fatalf("%v %q", err, c.Token)
	}
	r := runsOf(t, runs)
	if len(r) != 1 || r[0] != filepath.Join(s.Dir, "agy-home")+" disabled: models" {
		t.Fatalf("runs %q", r)
	}
	if c, err := s.Credential(agyNeed); err != nil || c.Token != "fake-agy-access-1" || len(runsOf(t, runs)) != 1 {
		t.Fatalf("%v %q %q", err, c.Token, runsOf(t, runs))
	}
	if st, _ := s.Status("agy"); st.Expired || !st.ExpiresAt.Equal(next) {
		t.Fatalf("%+v", st)
	}
}

// More than four minutes left: agy is not run.
func TestOwnNotDue(t *testing.T) {
	s, runs := ownStore(t, "exit 0")
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(30*time.Minute))
	if c, err := s.Credential(agyNeed); err != nil || c.Token != "fake-agy-access-0" {
		t.Fatalf("%v %q", err, c.Token)
	}
	if tried := s.RenewDue(); len(tried) != 0 || len(runsOf(t, runs)) != 0 {
		t.Fatalf("%v %q", tried, runsOf(t, runs))
	}
}

// Between turns, control renews a due sign-in no call used.
func TestOwnRenewDue(t *testing.T) {
	next := time.Now().Add(time.Hour)
	s, runs := ownStore(t, `printf '%s' '`+agyJSON("fake-agy-access-1", next)+`' > "$HOME/`+fakeAgyFile+`"`)
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(time.Minute))
	if tried := s.RenewDue(); len(tried) != 1 || tried["agy"] != nil {
		t.Fatalf("%v", tried)
	}
	if len(runsOf(t, runs)) != 1 {
		t.Fatalf("%q", runsOf(t, runs))
	}
	if tried := s.RenewDue(); len(tried) != 0 {
		t.Fatalf("renewed again: %v", tried)
	}
}

// Google refused: agy says to sign in and leaves its file as it was. The
// sign-in shows expired with "connect again", and agy is not run again.
func TestOwnRenewalRefused(t *testing.T) {
	s, runs := ownStore(t, `echo "Fetching available models..." >&2
echo "Error: Please sign in to view available models. Launch the CLI without arguments to sign in." >&2
exit 1`)
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(2*time.Minute))
	_, err := s.Credential(agyNeed)
	var m *Missing
	if !errors.As(err, &m) || !strings.Contains(m.Why, "refused to renew") || !strings.Contains(m.Why, "connect it again") {
		t.Fatalf("%v", err)
	}
	st, _ := s.Status("agy")
	if !st.Connected || !st.Expired || !strings.Contains(st.Error, "connect again") {
		t.Fatalf("%+v", st)
	}
	s.RenewDue()
	s.Credential(agyNeed)
	if r := runsOf(t, runs); len(r) != 1 {
		t.Fatalf("agy ran %d times", len(r))
	}
	// A new connect clears it.
	connectOwn(t, s, "fake-agy-access-2", time.Now().Add(time.Hour))
	if c, err := s.Credential(agyNeed); err != nil || c.Token != "fake-agy-access-2" {
		t.Fatalf("%v %q", err, c.Token)
	}
}

// agy failed for now (no network): with time left the token is still used,
// and the next call tries again; once its time is up the call is refused,
// and the status says to connect again.
func TestOwnRenewalFailsForNow(t *testing.T) {
	s, runs := ownStore(t, "echo 'Error: network is unreachable' >&2; exit 1")
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(2*time.Minute))
	if c, err := s.Credential(agyNeed); err != nil || c.Token != "fake-agy-access-0" {
		t.Fatalf("%v %q", err, c.Token)
	}
	s.Credential(agyNeed)
	if r := runsOf(t, runs); len(r) != 2 {
		t.Fatalf("agy ran %d times", len(r))
	}
	s2, _ := ownStore(t, "exit 1")
	connectOwn(t, s2, "fake-agy-access-0", time.Now().Add(-time.Minute))
	_, err := s2.Credential(agyNeed)
	var m *Missing
	if !errors.As(err, &m) || !strings.Contains(m.Why, "expired and could not be renewed just now") {
		t.Fatalf("%v", err)
	}
	if st, _ := s2.Status("agy"); !st.Expired || !strings.Contains(st.Error, "connect again") {
		t.Fatalf("%+v", st)
	}
}

// One renewal at a time: calls that all find the sign-in due run agy once.
func TestOwnOneRenewalAtATime(t *testing.T) {
	next := time.Now().Add(time.Hour)
	s, runs := ownStore(t, `sleep 0.3; printf '%s' '`+agyJSON("fake-agy-access-1", next)+`' > "$HOME/`+fakeAgyFile+`"`)
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(time.Minute))
	var wg sync.WaitGroup
	got := make([]string, 6)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := s.Credential(agyNeed)
			got[i] = c.Token
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g != "fake-agy-access-1" {
			t.Fatalf("%q", got)
		}
	}
	if r := runsOf(t, runs); len(r) != 1 {
		t.Fatalf("agy ran %d times", len(r))
	}
}

// A renewal that hangs is stopped at its limit, and counts as failed for now.
func TestOwnRenewalStopped(t *testing.T) {
	s, _ := ownStore(t, "sleep 30")
	o := s.Own["agy"]
	o.Wait = 300 * time.Millisecond
	s.Own["agy"] = o
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(time.Minute))
	start := time.Now()
	if c, err := s.Credential(agyNeed); err != nil || c.Token != "fake-agy-access-0" {
		t.Fatalf("%v %q", err, c.Token)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the renewal was not stopped")
	}
}

// A new connect replaces agy's home; a disconnect removes it.
func TestOwnReconnectAndDisconnect(t *testing.T) {
	s, _ := ownStore(t, "exit 0")
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(time.Hour))
	connectOwn(t, s, "fake-agy-access-9", time.Now().Add(time.Hour))
	if c, err := s.Credential(agyNeed); err != nil || c.Token != "fake-agy-access-9" {
		t.Fatalf("%v %q", err, c.Token)
	}
	left, _ := os.ReadDir(s.Dir)
	if len(left) != 2 {
		t.Fatalf("left %v", left)
	}
	if err := s.Remove("agy"); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(s.Dir); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
	if st, _ := s.Status("agy"); st.Connected {
		t.Fatalf("%+v", st)
	}
}

// A home whose file agy left unusable is not used: connect again.
func TestOwnFileUnusable(t *testing.T) {
	s, _ := ownStore(t, "exit 0")
	connectOwn(t, s, "fake-agy-access-0", time.Now().Add(time.Hour))
	os.WriteFile(filepath.Join(s.Dir, "agy-home", fakeAgyFile), []byte(`{"token":{}}`), 0o600)
	_, err := s.Credential(agyNeed)
	var m *Missing
	if !errors.As(err, &m) || !strings.Contains(m.Why, "connect it again") {
		t.Fatalf("%v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
