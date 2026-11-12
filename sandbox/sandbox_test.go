package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// CopyGrants puts a file and a folder at their place in the session's
// home, in place of what was there, and leaves symlinks inside a folder
// out.
func TestCopyGrants(t *testing.T) {
	user, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	write(t, filepath.Join(user, ".aws", "credentials"), "not granted\n")
	write(t, filepath.Join(user, ".config", "gcloud", "a", "b.json"), "{}")
	if err := os.Symlink(filepath.Join(user, ".aws"), filepath.Join(user, ".config", "gcloud", "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyGrants(home, home+".grants", user, []string{"~/.aws/config", "~/.config/gcloud"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".aws", "config")); err != nil || string(b) != "[profile a]\n" {
		t.Errorf(".aws/config: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "credentials")); err == nil {
		t.Error(".aws/credentials was copied, and only .aws/config was granted")
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "gcloud", "a", "b.json")); err != nil {
		t.Error("a folder's file was not copied")
	}
	if _, err := os.Lstat(filepath.Join(home, ".config", "gcloud", "link")); err == nil {
		t.Error("a symlink inside a granted folder was copied")
	}
}

// A grant that is missing, outside ~, or too big is refused with an Error;
// a symlink the agent left in its home is not followed out of it.
func TestCopyGrantsRefuses(t *testing.T) {
	user, home := t.TempDir(), t.TempDir()
	var e *Error
	for _, g := range []string{"~/not-there", "/etc/passwd", "~/../x", "~/"} {
		if err := CopyGrants(home, home+".grants", user, []string{g}); !errors.As(err, &e) {
			t.Errorf("%s: %v, want an Error", g, err)
		}
	}
	big := filepath.Join(user, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxGrant + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := CopyGrants(home, home+".grants", user, []string{"~/big"}); !errors.As(err, &e) || !strings.Contains(err.Error(), "16 MiB") {
		t.Errorf("too big: %v", err)
	}
	// The agent made .aws a link to a folder outside its home.
	outside := t.TempDir()
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	if err := os.Symlink(outside, filepath.Join(home, ".aws")); err != nil {
		t.Fatal(err)
	}
	_ = CopyGrants(home, home+".grants", user, []string{"~/.aws/config"})
	if _, err := os.Stat(filepath.Join(outside, "config")); err == nil {
		t.Error("the grant was written through a symlink out of the home")
	}
}

// Program grants a program's folder, but never one that holds the home.
func TestProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, "bin", "tool"), "")
	write(t, filepath.Join(home, "tool"), "")
	if got := Program(filepath.Join(home, "bin", "tool")); len(got) != 2 || !strings.HasSuffix(got[1], "bin") {
		t.Errorf("Program: %v, want the file and its folder", got)
	}
	write(t, filepath.Join(home, "lib", "node_modules", "pkg", "package.json"), "{}")
	write(t, filepath.Join(home, "lib", "node_modules", "pkg", "bin", "cli.js"), "")
	if got := Program(filepath.Join(home, "lib", "node_modules", "pkg", "bin", "cli.js")); len(got) != 2 || !strings.HasSuffix(got[1], "pkg") {
		t.Errorf("Program of an npm package's program: %v, want the file and the package", got)
	}
	if got := Program(filepath.Join(home, "tool")); len(got) != 1 {
		t.Errorf("Program of a file in the home: %v, want the file alone", got)
	}
}

// A script's interpreter is readable too: by its path, or found on PATH
// through env, as npm's CLIs name node.
func TestProgramInterpreter(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "nvm", "bin", "node")
	write(t, node, "")
	if err := os.Chmod(node, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(node)+":/usr/bin:/bin")
	for _, line := range []string{"#!/usr/bin/env node\n", "#!/usr/bin/env -S node --flag\n", "#!" + node + "\n"} {
		cli := filepath.Join(dir, "pkg", "cli.js")
		write(t, cli, line+"code\n")
		got := Program(cli)
		if !slices.Contains(got, node) {
			t.Errorf("%q: Program = %v, want the interpreter %s", line, got, node)
		}
	}
	plain := filepath.Join(dir, "plain")
	write(t, plain, "\x7fELF")
	if got := Program(plain); len(got) != 2 {
		t.Errorf("a binary: Program = %v, want the file and its folder", got)
	}
}

// An entry taken out of the list is gone from the home at the next call,
// and so is every entry when the list is empty (review of 2026-11-11, round
// 3). The record lives beside the home, not in it.
func TestCopyGrantsRevokes(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	write(t, filepath.Join(user, ".gitconfig"), "[user]\n")
	if err := CopyGrants(home, record, user, []string{"~/.aws/config", "~/.gitconfig"}); err != nil {
		t.Fatal(err)
	}
	if err := CopyGrants(home, record, user, []string{"~/.gitconfig"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "config")); err == nil {
		t.Error("an entry taken out is still in the home")
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err != nil {
		t.Error("an entry kept is gone")
	}
	if err := CopyGrants(home, record, user, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err == nil {
		t.Error("with no entries left, one is still in the home")
	}
	// A record not of this shape is refused, never followed.
	write(t, record, "../outside")
	if err := CopyGrants(home, record, user, nil); err == nil {
		t.Error("a record naming a path out of the home was used")
	}
}

// A grant writes files beside what the session has and never replaces one
// of its own; a refused call leaves the home as it was; the files it wrote
// are replaced and, taken out, removed, and what the session wrote beside
// them stays (reviews of 2026-11-11/12, rounds 3, 4, 6 and 7).
func TestCopyGrantsFilesOnly(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	write(t, filepath.Join(home, ".claude", "projects", "s.jsonl"), "{}")
	write(t, filepath.Join(user, ".claude", "settings.json"), "{}")
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	write(t, filepath.Join(user, ".aws", "sso", "cache.json"), "{}")
	// Beside the session's own folder: merged, its file stays.
	if err := CopyGrants(home, record, user, []string{"~/.claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "projects", "s.jsonl")); err != nil {
		t.Error("the session's own file is gone")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Error("the granted file was not copied")
	}
	// Over one of the session's own files: refused, nothing changes.
	write(t, filepath.Join(user, ".claude", "projects", "s.jsonl"), "theirs")
	var e *Error
	if err := CopyGrants(home, record, user, []string{"~/.aws/config", "~/.claude"}); !errors.As(err, &e) || !strings.Contains(err.Error(), "would replace") {
		t.Fatalf("a grant over the session's own file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", "s.jsonl")); string(b) != "{}" {
		t.Error("the session's own file changed")
	}
	if _, err := os.Stat(filepath.Join(home, ".aws")); err == nil {
		t.Error("a refused call copied part of its grants")
	}
	// Widened from a file to its folder, then the session writes beside it.
	for _, step := range [][]string{{"~/.aws/config"}, {"~/.aws"}} {
		if err := CopyGrants(home, record, user, step); err != nil {
			t.Fatalf("%v: %v", step, err)
		}
	}
	write(t, filepath.Join(home, ".aws", "config"), "changed by the agent\n")
	write(t, filepath.Join(home, ".aws", "sso", "login.json"), "the session's own")
	if err := CopyGrants(home, record, user, []string{"~/.aws"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".aws", "config")); string(b) != "[profile a]\n" {
		t.Errorf(".aws/config: %q, want the original", b)
	}
	// Taken out: its files go, the session's own stay.
	if err := CopyGrants(home, record, user, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".aws/config", ".aws/sso/cache.json", ".claude/settings.json"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s stays once its grant is taken out", f)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "sso", "login.json")); err != nil {
		t.Error("a file the session wrote beside a grant is gone")
	}
}

// A grant that is a named pipe is refused, never waited on (review of
// 2026-11-12, round 7).
func TestCopyGrantsPipe(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home := filepath.Join(dir, "S")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(user, "pipe")); err != nil {
		t.Skip("no named pipes here")
	}
	done := make(chan error, 1)
	go func() { done <- CopyGrants(home, home+".grants", user, []string{"~/pipe"}) }()
	select {
	case err := <-done:
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("a pipe: %v, want an Error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CopyGrants waits on a named pipe")
	}
}
