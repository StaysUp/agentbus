//go:build integration && darwin

package sandbox

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretsHiddenUnderGrant grants the agent a folder that holds the
// person's secrets and a plain file, and shows the wall hides every secret
// path and the .env outside the copy, while the plain file and the copy's
// own .env stay readable. Without this the whole folder would be readable:
// the grant allows it, and the deny is what the wall's defaults add.
func TestSecretsHiddenUnderGrant(t *testing.T) {
	needWall(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(root, "user")
	copyd := filepath.Join(root, "copy")
	home := filepath.Join(root, "home")
	for _, d := range []string{
		filepath.Join(user, ".ssh"), filepath.Join(user, ".aws"), filepath.Join(user, ".gnupg"),
		filepath.Join(user, ".config", "gh"), filepath.Join(user, ".docker"),
		filepath.Join(user, "sub"), copyd, home,
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(user, ".ssh", "config"):         "Host example\n",
		filepath.Join(user, ".aws", "config"):         "[profile a]\n",
		filepath.Join(user, ".gnupg", "secring"):      "key\n",
		filepath.Join(user, ".config", "gh", "hosts"): "token\n",
		filepath.Join(user, ".docker", "config.json"): "{}\n",
		filepath.Join(user, ".npmrc"):                 "//r/:_authToken=x\n",
		filepath.Join(user, ".netrc"):                 "machine x login y\n",
		filepath.Join(user, ".env"):                   "SECRET=1\n",
		filepath.Join(user, "sub", ".env.local"):      "SECRET=2\n",
		filepath.Join(user, "ok.txt"):                 "fine\n",
		filepath.Join(copyd, ".env"):                  "PROJECT=1\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Point the wall's secret set at this fake home, so no real secret is
	// ever named, and grant the whole folder for reading.
	old := homeDir
	homeDir = func() (string, error) { return user, nil }
	defer func() { homeDir = old }()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(Spec{Copy: copyd, Home: home, Read: append([]string{user}, Program(self)...)})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	cases := []probeCase{
		{"grant.ok", "read", filepath.Join(user, "ok.txt"), true, true},
		{"copy.env", "read", filepath.Join(copyd, ".env"), true, true},
		{"ssh", "read", filepath.Join(user, ".ssh", "config"), false, true},
		{"aws", "read", filepath.Join(user, ".aws", "config"), false, true},
		{"gnupg", "read", filepath.Join(user, ".gnupg", "secring"), false, true},
		{"gh", "read", filepath.Join(user, ".config", "gh", "hosts"), false, true},
		{"docker", "read", filepath.Join(user, ".docker", "config.json"), false, true},
		{"npmrc", "read", filepath.Join(user, ".npmrc"), false, true},
		{"netrc", "read", filepath.Join(user, ".netrc"), false, true},
		{"env", "read", filepath.Join(user, ".env"), false, true},
		{"env.nested", "read", filepath.Join(user, "sub", ".env.local"), false, true},
	}

	run := func(walled bool) map[string]bool {
		var list strings.Builder
		for _, c := range cases {
			fmt.Fprintf(&list, "%s %s %s\n", c.name, c.op, c.arg)
		}
		cmd := exec.Command(self)
		cmd.Dir = copyd
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, probeEnv + "=" + list.String()}
		start := cmd.Start
		if walled {
			cmd.Env = append(cmd.Env, box.Env()...)
			start = func() error { return box.Start(cmd) }
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := start(); err != nil {
			t.Fatalf("start (walled %t): %v", walled, err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("probe (walled %t): %v\n%s", walled, err, out.String())
		}
		got := map[string]bool{}
		sc := bufio.NewScanner(&out)
		for sc.Scan() {
			name, res, _ := strings.Cut(sc.Text(), " ")
			got[name] = res == "allowed"
		}
		return got
	}

	open := run(false)
	walled := run(true)
	for _, c := range cases {
		if c.open && !open[c.name] {
			t.Errorf("%s: not readable even without the wall, so the probe shows nothing", c.name)
		}
		if walled[c.name] != c.allowed {
			t.Errorf("%s: allowed %t in the sandbox, want %t", c.name, walled[c.name], c.allowed)
		}
	}
}

// TestSecretsHiddenViaSymlink covers a dotfile-manager layout: the secrets
// live in ~/dotfiles, and ~/.config and ~/.netrc are symlinks into it.
// Seatbelt matches a read against the resolved path, so the wall must deny
// both the link and its target; reading either, by either path, fails.
func TestSecretsHiddenViaSymlink(t *testing.T) {
	needWall(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(root, "user")
	copyd := filepath.Join(root, "copy")
	home := filepath.Join(root, "home")
	for _, d := range []string{filepath.Join(user, "dotfiles", "config", "gh"), copyd, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{
		filepath.Join(user, "dotfiles", "config", "gh", "hosts"): "token\n",
		filepath.Join(user, "dotfiles", "netrc"):                 "machine x login y\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// ~/.config -> ~/dotfiles/config (a symlinked parent), ~/.netrc ->
	// ~/dotfiles/netrc (a symlinked leaf).
	if err := os.Symlink(filepath.Join(user, "dotfiles", "config"), filepath.Join(user, ".config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(user, "dotfiles", "netrc"), filepath.Join(user, ".netrc")); err != nil {
		t.Fatal(err)
	}

	old := homeDir
	homeDir = func() (string, error) { return user, nil }
	defer func() { homeDir = old }()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(Spec{Copy: copyd, Home: home, Read: append([]string{user}, Program(self)...)})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	cases := []probeCase{
		{"gh.link", "read", filepath.Join(user, ".config", "gh", "hosts"), false, true},
		{"gh.target", "read", filepath.Join(user, "dotfiles", "config", "gh", "hosts"), false, true},
		{"netrc.link", "read", filepath.Join(user, ".netrc"), false, true},
		{"netrc.target", "read", filepath.Join(user, "dotfiles", "netrc"), false, true},
	}
	run := func(walled bool) map[string]bool {
		var list strings.Builder
		for _, c := range cases {
			fmt.Fprintf(&list, "%s %s %s\n", c.name, c.op, c.arg)
		}
		cmd := exec.Command(self)
		cmd.Dir = copyd
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, probeEnv + "=" + list.String()}
		start := cmd.Start
		if walled {
			cmd.Env = append(cmd.Env, box.Env()...)
			start = func() error { return box.Start(cmd) }
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := start(); err != nil {
			t.Fatalf("start (walled %t): %v", walled, err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("probe (walled %t): %v\n%s", walled, err, out.String())
		}
		got := map[string]bool{}
		sc := bufio.NewScanner(&out)
		for sc.Scan() {
			name, res, _ := strings.Cut(sc.Text(), " ")
			got[name] = res == "allowed"
		}
		return got
	}
	open := run(false)
	walled := run(true)
	for _, c := range cases {
		if c.open && !open[c.name] {
			t.Errorf("%s: not readable even without the wall", c.name)
		}
		if walled[c.name] != c.allowed {
			t.Errorf("%s: allowed %t in the sandbox, want %t", c.name, walled[c.name], c.allowed)
		}
	}
}
