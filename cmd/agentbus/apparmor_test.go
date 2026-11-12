package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/netjail"
)

// install.sh writes the AppArmor profile from its own copy of
// packaging/apparmor/agentbus: the rules must be the same, but the path.
func TestAppArmorProfileInInstall(t *testing.T) {
	rules := func(file, path string) string {
		b, err := os.ReadFile(filepath.Join(root(t), file))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, "abi <abi/4.0>,")
		j := strings.Index(s[max(i, 0):], "\n}\n")
		if i < 0 || j < 0 {
			t.Fatalf("%s holds no profile", file)
		}
		s = s[i : i+j+3]
		if !strings.Contains(s, `profile agentbus "`+path+`" flags=(unconfined) {`) {
			t.Fatalf("%s: the profile does not name %s:\n%s", file, path, s)
		}
		return strings.Replace(s, path, "BINARY", 1)
	}
	want := rules("packaging/apparmor/agentbus", "/usr/local/bin/agentbus")
	if got := rules("install.sh", "$1"); got != want {
		t.Fatalf("install.sh's profile:\n%s\nwant packaging/apparmor/agentbus's:\n%s", got, want)
	}
	if !strings.Contains(want, "\n  userns,\n") {
		t.Fatal("the profile does not allow userns")
	}
}

// One rule for which paths a profile may name: install.sh's plain_path and
// netjail.ProfilePathPlain answer the same for every path, so the runner's
// advice never offers a profile command that install.sh refuses (review
// round 2), and a path with a space gets none (round 1).
func TestProfilePathShared(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "plain_path() {")
	j := strings.Index(s[max(i, 0):], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("install.sh has no plain_path")
	}
	fn := s[i : i+j+3]
	if !strings.Contains(fn, "*[!"+netjail.ProfilePathChars+"]*") {
		t.Fatalf("install.sh's plain_path does not test netjail.ProfilePathChars:\n%s", fn)
	}
	for _, p := range []string{
		"/usr/local/bin/agentbus", "/opt/agentbus-ci/netjail.test", "/opt/a+b/agent_bus-1.0",
		"/opt/Agent Bus/agentbus", "/opt/it's/agentbus", `/opt/"x"/agentbus`, "/opt/*/agentbus",
		"/opt/{a,b}/agentbus", "/opt/a?b", "/opt/[ab]", "/opt/a^b", "/opt/@{HOME}", "/opt/$x", "/opt/#x",
		`/opt/a\b`, "/opt/tab\there", "/opt/ünï", "",
	} {
		err := exec.Command("sh", "-c", fn+`plain_path "$1"`, "sh", p).Run()
		sh := err == nil
		if _, ok := err.(*exec.ExitError); err != nil && !ok {
			t.Fatal(err)
		}
		if goes := netjail.ProfilePathPlain(p); sh != goes {
			t.Errorf("%q: install.sh says %v, netjail says %v", p, sh, goes)
		}
	}
}

// install.sh names another agentbus found on PATH in quotes, whole, and
// prints no command that acts on it: a path with a space once became two
// arguments to a printed rm. The binary that has the profile is not named.
func TestInstallNamesOtherBinaries(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	fn := func(name string) string {
		i := strings.Index(s, name+"() {")
		if i < 0 {
			t.Fatalf("install.sh has no %s", name)
		}
		// A function on one line, such as say, ends on it.
		line := s[i : i+strings.Index(s[i:], "\n")+1]
		if strings.HasSuffix(line, "}\n") {
			return line
		}
		j := strings.Index(s[i:], "\n}\n")
		if j < 0 {
			t.Fatalf("install.sh's %s does not end", name)
		}
		return s[i : i+j+3]
	}
	dir := filepath.Join(t.TempDir(), "Agent Bus")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "agentbus")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	other, _ = filepath.EvalSymlinks(other)
	run := func(profiled string) string {
		cmd := exec.Command("sh", "-c", fn("say")+fn("other_binaries")+`other_binaries "$1"`, "sh", profiled)
		cmd.Env = []string{"PATH=" + filepath.Dir(other) + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	out := run("/usr/local/bin/agentbus")
	if !strings.Contains(out, "'"+other+"'") {
		t.Errorf("the other binary is not named whole, in quotes:\n%s", out)
	}
	if strings.Contains(out, "rm ") {
		t.Errorf("a command that acts on the path is printed:\n%s", out)
	}
	if out := run(other); out != "" {
		t.Errorf("the profiled binary is named as another:\n%s", out)
	}
}
