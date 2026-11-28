package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
)

// fakeDoctor is a doctor over home where only the programs in onPath are
// installed, env is the environment, and the disk has free bytes.
func fakeDoctor(home string, onPath []string, env map[string]string, free uint64) doctor {
	return doctor{
		home:   home,
		getenv: func(k string) string { return env[k] },
		lookPath: func(name string) (string, error) {
			for _, p := range onPath {
				if p == name {
					return "/usr/local/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
		packed:  true,
		now:     time.Now,
		disk:    func(string) (uint64, error) { return free, nil },
		copyWay: func(string) finding { return finding{statusOK, "Copy clones"} },
	}
}

func doctorReport(t *testing.T, d doctor) (string, int) {
	t.Helper()
	var b bytes.Buffer
	code := d.report(&b)
	return b.String(), code
}

// lineOf is the report's line that holds s, or "".
func lineOf(out, s string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

// A machine ready for a first turn: every line ok or a note, and exit 0.
func TestDoctorReady(t *testing.T) {
	home := t.TempDir()
	connectFake(t, home, "claude")
	out, code := doctorReport(t, fakeDoctor(home, []string{"claude"}, nil, 50<<30))
	if code != 0 || !strings.HasSuffix(out, "\nNo check failed.\n") || strings.Contains(out, "fail ") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"ok    claude is on PATH: /usr/local/bin/claude",
		"ok    the data folder can be written",
		"note  codex is not on PATH: install the Codex CLI to use it",
		"ok    the claude subscription is connected; it works until ",
		"ok    no variable sends an agent elsewhere (ANTHROPIC_BASE_URL)",
		"ok    Copy clones",
		"ok    50.0 GiB free on the data folder's disk",
		"ok    the cockpit is packed in",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	// A harness not installed and not connected has no subscription line.
	if l := lineOf(out, "codex subscription"); l != "" {
		t.Errorf("a line for codex's subscription: %q", l)
	}
	if strings.Contains(out, fakeSubscription) {
		t.Fatal("the report shows the token")
	}
}

// Every failed check names its fix, and any one makes the exit code 1.
func TestDoctorFailsNameTheFix(t *testing.T) {
	home := t.TempDir()
	subs := subscriptionStore(home)
	if err := subs.Put("claude", fakeSubscription, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, subscriptionsDir, "codex.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	const key = "sk-or-v1-doctor-test-key"
	if err := subs.SetKey(credentials.AccessOpenRouter, key); err != nil {
		t.Fatal(err)
	}
	d := fakeDoctor(home, []string{"claude", "codex"}, map[string]string{"ANTHROPIC_BASE_URL": "http://proxy:1"}, 1<<30)
	d.packed = false
	d.appArmor = func() *finding { return &finding{statusFail, "AppArmor refuses; add it, once: curl"} }
	out, code := doctorReport(t, d)
	if code != 1 || !strings.HasSuffix(out, "\n6 checks failed.\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"fail  the claude subscription cannot be used: the token's time is up: connect again in the cockpit's Settings, Subscriptions",
		"fail  the codex subscription cannot be read (the subscription's file cannot be used; connect again): connect it again in the cockpit's Settings, Subscriptions",
		"ok    keys set: openrouter",
		"fail  ANTHROPIC_BASE_URL is set, so every claude turn is refused: unset it in the shell that starts agentbus",
		"fail  AppArmor refuses; add it, once: curl",
		"fail  1.0 GiB free on the data folder's disk, under 2.0 GiB: free some, or set AGENTBUS_HOME to a folder on another disk",
		"fail  the cockpit is not packed in this build: install a release",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	// The key is set, so a turn has something to sign in with.
	if l := lineOf(out, "no subscription is connected"); l != "" {
		t.Errorf("%q", l)
	}
	if strings.Contains(out, key) || strings.Contains(out, fakeSubscription) {
		t.Fatal("the report shows a secret")
	}
}

// Installed but nothing to sign in with: every turn would be refused.
func TestDoctorNothingToSignInWith(t *testing.T) {
	out, code := doctorReport(t, fakeDoctor(t.TempDir(), []string{"claude"}, nil, 50<<30))
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"note  the claude subscription is not connected: connect it in the cockpit's Settings, Subscriptions",
		"fail  no subscription is connected and no key is set, so every turn is refused: start agentbus up, then connect",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// No agent installed: that one fail, not a second about sign-ins.
func TestDoctorNoAgent(t *testing.T) {
	out, code := doctorReport(t, fakeDoctor(t.TempDir(), nil, map[string]string{"ANTHROPIC_BASE_URL": "x"}, 50<<30))
	if code != 1 || !strings.HasSuffix(out, "\n1 check failed.\n") ||
		!strings.Contains(out, "fail  no agent is installed: install claude, codex, agy or opencode") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// A variable for a harness that is not installed changes nothing.
	if strings.Contains(out, "ANTHROPIC_BASE_URL") {
		t.Fatalf("%s", out)
	}
}

// doctor needs no service and makes no data folder: one that does not
// exist yet is reported as new, and still is after. It answers well
// within a second.
func TestDoctorCommand(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agentbus")
	env := func(k string) string {
		if k == "AGENTBUS_HOME" {
			return home
		}
		return os.Getenv(k)
	}
	var b bytes.Buffer
	start := time.Now()
	code := run([]string{"doctor"}, env, &b, &b)
	took := time.Since(start)
	if code != 0 && code != 1 {
		t.Fatalf("exit %d:\n%s", code, b.String())
	}
	if !strings.HasPrefix(b.String(), "Data folder: "+tildePath(home)+" (new)\n") {
		t.Fatalf("%s", b.String())
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor made the data folder: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(home)); len(entries) != 0 {
		t.Fatalf("doctor left %v beside the data folder", entries)
	}
	if took > time.Second {
		t.Fatalf("doctor took %v", took)
	}
	b.Reset()
	if run([]string{"doctor", "-x"}, env, &b, &b) != 2 || !strings.Contains(b.String(), doctorUsage) {
		t.Fatalf("doctor took an argument: %s", b.String())
	}
}

// The data folder: one that exists must be a folder that can be written,
// and one not made yet must have a folder above it that can be.
func TestDoctorDataFolder(t *testing.T) {
	dir := t.TempDir()
	at := func(home string) finding { return fakeDoctor(home, nil, nil, 50<<30).folder() }
	if f := at(filepath.Join(dir, "a", "b")); f.status != statusOK || f.line != "the data folder can be made" {
		t.Fatalf("%+v", f)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if f := at(file); f.status != statusFail || !strings.Contains(f.line, "set AGENTBUS_HOME to a folder") {
		t.Fatalf("%+v", f)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes any folder")
	}
	shut := filepath.Join(dir, "shut")
	if err := os.Mkdir(shut, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(shut, 0o700) })
	if f := at(shut); f.status != statusFail || f.line != "the data folder cannot be written: make it yours, or set AGENTBUS_HOME to a folder you can write" {
		t.Fatalf("%+v", f)
	}
	if f := at(filepath.Join(shut, "agentbus")); f.status != statusFail || !strings.HasSuffix(f.line, " cannot be written: set AGENTBUS_HOME to a folder you can write") {
		t.Fatalf("%+v", f)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("the check left %v", entries)
	}
}

func TestExistingDir(t *testing.T) {
	dir := t.TempDir()
	if got := existingDir(filepath.Join(dir, "a", "b")); got != dir {
		t.Fatalf("%q, want %q", got, dir)
	}
	if got := existingDir(dir); got != dir {
		t.Fatalf("%q", got)
	}
}
