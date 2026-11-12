//go:build integration

package credentials

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// deviceLogin is a fake codex login --device-auth: it prints a link and a
// one-time code, then runs script, with its folder in $FAKE_HOME_VAR.
func deviceLogin(t *testing.T, script string) Login {
	t.Helper()
	p := filepath.Join(t.TempDir(), "login")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+`printf '1. Open this link\n   \033[94mhttps://auth.example.test/codex/device\033[0m\n'
printf '2. Enter this one-time code\n   \033[94mABCD-EFGH2\033[0m\n'
`+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Login{Path: p, Code: regexp.MustCompile(`^[A-Z0-9]{4,8}-[A-Z0-9]{4,8}$`), Home: "FAKE_CODEX_HOME", File: "auth.json",
		TokenKey: "tokens.access_token", RefreshKey: "tokens.refresh_token", AccountKey: "tokens.account_id"}
}

// The sign-in the fake login writes, as codex does.
func authJSON(access string) string {
	return `{"OPENAI_API_KEY":null,"tokens":{"id_token":"fake-id","access_token":"` + access +
		`","refresh_token":"fake-refresh-0","account_id":"acct-1"},"last_refresh":"2026-11-10T12:00:00Z"}`
}

// Connected in the cockpit's way: the person sees the link and the code; the
// login's own folder is control's, inside the subscriptions folder, and is
// gone after; the sign-in is kept and renews.
func TestDeviceLoginKeepsItsSignin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subscriptions")
	access := fakeJWT("access-0", time.Now().Add(time.Hour))
	marker := filepath.Join(t.TempDir(), "home")
	m := &Manager{Store: &Subscriptions{Dir: dir, OAuth: map[string]OAuth{"codex": {URL: "http://127.0.0.1:1", AccountHeader: "ChatGPT-Account-ID"}}},
		Logins: map[string]Login{"codex": deviceLogin(t, `echo "$FAKE_CODEX_HOME" > `+marker+`
sleep 1
printf '%s' '`+authJSON(access)+`' > "$FAKE_CODEX_HOME/auth.json"
echo "Successfully logged in"`)}}
	if _, err := m.Connect("codex"); err != nil {
		t.Fatal(err)
	}
	// While it runs: the link and the code, and nothing to paste.
	waitFor(t, func() bool { v, _ := m.View("codex"); return v.Code != "" || v.State != Running })
	if v, _ := m.View("codex"); v.State != Running || len(v.Output) != 1 || v.Output[0] != "https://auth.example.test/codex/device" ||
		v.Code != "ABCD-EFGH2" || v.Paste {
		t.Fatalf("running: %+v", v)
	}
	// Ended: the code is used up and no longer shown.
	v := waitState(t, m, "codex")
	if v.State != Connected || len(v.Output) != 0 || v.Code != "" {
		t.Fatalf("%+v", v)
	}
	home, _ := os.ReadFile(marker)
	h := strings.TrimSpace(string(home))
	if !strings.HasPrefix(h, filepath.Join(dir, ".login-")) {
		t.Fatalf("the login's folder: %q", h)
	}
	if _, err := os.Stat(h); !os.IsNotExist(err) {
		t.Fatalf("the login's folder is left: %v", err)
	}
	c, err := m.Store.Credential(Need{Access: AccessSubscription, Harness: "codex"})
	if err != nil || c.Token != access || c.Headers["ChatGPT-Account-ID"] != "acct-1" {
		t.Fatalf("%v %v", err, c.Headers)
	}
	if st, _ := m.Store.Status("codex"); !st.Connected || !st.Renews || st.ExpiresAt == nil {
		t.Fatalf("%+v", st)
	}
}

// A login that ends without a sign-in, or with one that names no expiry,
// fails, and leaves no folder.
func TestDeviceLoginWithoutSignin(t *testing.T) {
	for name, script := range map[string]string{
		"none":      "exit 1",
		"no expiry": `printf '%s' '` + authJSON("not-a-jwt") + `' > "$FAKE_CODEX_HOME/auth.json"`,
	} {
		dir := t.TempDir()
		m := &Manager{Store: &Subscriptions{Dir: dir}, Logins: map[string]Login{"codex": deviceLogin(t, script)}}
		if _, err := m.Connect("codex"); err != nil {
			t.Fatal(err)
		}
		if v := waitState(t, m, "codex"); v.State != Failed || !strings.Contains(v.Error, "without a sign-in") {
			t.Fatalf("%s: %+v", name, v)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("%s: left %v", name, left)
		}
	}
}

// A connect stopped midway, at the Manager's Wait or when control stops,
// fails and leaves no folder: the login's folder may hold part of a sign-in.
func TestDeviceLoginStopped(t *testing.T) {
	for _, how := range []string{"wait", "stop"} {
		dir := t.TempDir()
		m := &Manager{Store: &Subscriptions{Dir: dir},
			Logins: map[string]Login{"codex": deviceLogin(t, `echo partial > "$FAKE_CODEX_HOME/auth.json"; sleep 30`)}}
		if how == "wait" {
			m.Wait = 500 * time.Millisecond
		}
		if _, err := m.Connect("codex"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { v, _ := m.View("codex"); return v.Code != "" || v.State != Running })
		if how == "stop" {
			m.Stop()
		}
		start := time.Now()
		if v := waitState(t, m, "codex"); v.State != Failed || v.Code != "" || len(v.Output) != 0 {
			t.Fatalf("%s: %+v", how, v)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("%s: the login was not stopped", how)
		}
		if st, _ := m.Store.Status("codex"); st.Connected {
			t.Fatalf("%s: connected %+v", how, st)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("%s: left %v", how, left)
		}
	}
}
