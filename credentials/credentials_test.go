package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A fake token: the shape a login prints, no one's credential.
const fakeToken = "sk-fake-oat01-AAAABBBBCCCCDDDDEEEEFFFF0000"

func fakeLogin(t *testing.T, script string) Login {
	t.Helper()
	p := filepath.Join(t.TempDir(), "login")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Login{Path: p, Prefix: "sk-fake-", Token: regexp.MustCompile(`sk-fake-[A-Za-z0-9_-]{16,}`), Valid: time.Hour}
}

func waitState(t *testing.T, m *Manager, h string) View {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if v, _ := m.View(h); v.State != Running {
			return v
		}
	}
	t.Fatal("the login did not end")
	return View{}
}

// The login asks for a code, then prints the token: the person sees the
// link only, never the token, and the store keeps it.
func TestConnectKeepsTokenOutOfOutput(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Store: &Subscriptions{Dir: filepath.Join(dir, "subscriptions")},
		Logins: map[string]Login{"claude": fakeLogin(t, `echo "Open https://example.test/auth?x=1"
printf 'Paste code here: '
read code
[ "$code" = "c0de" ] || exit 3
printf '\033[32mYour token:\033[0m `+fakeToken+`\n'
echo done`)}}
	v, err := m.Connect("claude")
	if err != nil || v.State != Running {
		t.Fatalf("%v %+v", err, v)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		v, _ = m.View("claude")
		if len(v.Output) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no prompt: %+v", v)
		}
	}
	if err := m.Input("claude", "c0de"); err != nil {
		t.Fatal(err)
	}
	v = waitState(t, m, "claude")
	if v.State != Connected || len(v.Output) != 1 || v.Output[0] != "https://example.test/auth?x=1" {
		t.Fatalf("%+v", v)
	}
	got, err := m.Store.Credential(Need{Access: AccessSubscription, Harness: "claude"})
	if err != nil || got.Token != fakeToken || got.Headers != nil {
		t.Fatalf("credential: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "subscriptions", "claude.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", err, fi)
	}
	st, _ := m.Store.Status("claude")
	if !st.Connected || st.Expired || st.ExpiresAt == nil {
		t.Fatalf("%+v", st)
	}
}

func TestConnectWithoutTokenFails(t *testing.T) {
	m := &Manager{Store: &Subscriptions{Dir: t.TempDir()}, Logins: map[string]Login{"claude": fakeLogin(t, "echo nope; exit 1")}}
	if _, err := m.Connect("claude"); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, m, "claude"); v.State != Failed || !strings.Contains(v.Error, "exit 1") {
		t.Fatalf("%+v", v)
	}
	if _, err := m.Connect("codex"); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("codex: %v", err)
	}
}

func TestExpiredAndMissing(t *testing.T) {
	now := time.Now()
	s := &Subscriptions{Dir: t.TempDir(), Now: func() time.Time { return now }}
	var m *Missing
	if _, err := s.Credential(Need{Harness: "claude"}); !errors.As(err, &m) || !strings.Contains(m.Why, "connect") {
		t.Fatalf("%v", err)
	}
	if err := s.Put("claude", fakeToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.Credential(Need{Harness: "claude"}); !errors.As(err, &m) || !strings.Contains(m.Why, "expired") {
		t.Fatalf("%v", err)
	}
	if st, _ := s.Status("claude"); !st.Expired {
		t.Fatalf("%+v", st)
	}
	if err := s.Remove("claude"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("claude"); st.Connected {
		t.Fatalf("%+v", st)
	}
	if err := s.Put("../x", fakeToken, time.Minute); err == nil {
		t.Fatal("a path as a harness name was taken")
	}
}

func TestEnvByName(t *testing.T) {
	e := Env{Getenv: func(k string) string {
		if k == "AB_KEY" {
			return "fake-key"
		}
		return ""
	}}
	p := Plug{AccessOpenRouter: e}
	if v, err := p.Credential(Need{Access: AccessOpenRouter, KeyEnv: "AB_KEY"}); err != nil || v.Token != "fake-key" {
		t.Fatalf("%q %v", v, err)
	}
	for _, name := range []string{"", "AB_UNSET", "1X", "A-B"} {
		if _, err := p.Credential(Need{Access: AccessOpenRouter, KeyEnv: name}); err == nil {
			t.Fatalf("%q gave a key", name)
		}
	}
	if _, err := p.Credential(Need{Access: AccessSubscription}); err == nil {
		t.Fatal("no source, a credential")
	}
}

// Only links leave the login: a token cut across reads, styled with escape
// sequences, or printed twice is never shown, in the terminal or the view,
// and it is still found.
func TestOnlyLinksLeaveTheLogin(t *testing.T) {
	l := fakeLogin(t, `echo "Open https://example.test/auth"
printf 'Token: sk-fa'
sleep 0.2
printf 'ke-oat01-AAAABBBBCCCCDDDDEEEEFFFF0000 again `+fakeToken+`\n'
printf 'x sk-fa\033(Bke-oat01-AAAA\033[1mBBBBCCCCDDDDEEEEFFFF0000 y\n'
printf 'https://example.test/x?t=`+fakeToken+`\n'`)
	var out strings.Builder
	si, err := Run(context.Background(), l, t.TempDir(), time.Now(), strings.NewReader(""), &out)
	if err != nil || si.Token != fakeToken {
		t.Fatalf("%q %v", si.Token, err)
	}
	if s := out.String(); s != "Sign in here: https://example.test/auth\nThen paste the code the browser shows, and press Enter.\n" {
		t.Fatalf("%q", s)
	}
	c := &connecting{login: l}
	c.take(strings.NewReader("a https://example.test/a "+fakeToken+"\nx sk-fa\x1b(Bke-oat01-AAAABBBBCCCCDDDDEEEEFFFF0000\n"), func(string) {})
	if si, _ := l.signin(c.text, "", time.Now()); len(c.view().Output) != 1 || c.view().Output[0] != "https://example.test/a" || si.Token != fakeToken {
		t.Fatalf("%q %q", c.view().Output, si.Token)
	}
}
