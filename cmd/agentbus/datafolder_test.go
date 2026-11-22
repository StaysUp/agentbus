package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A data folder is new when it is missing or holds nothing.
func TestFreshFolder(t *testing.T) {
	dir := t.TempDir()
	if !freshFolder(filepath.Join(dir, "missing")) {
		t.Errorf("a missing folder is not new")
	}
	if !freshFolder(dir) {
		t.Errorf("an empty folder is not new")
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if freshFolder(dir) {
		t.Errorf("a folder that holds a file is new")
	}
}

// A folder under the person's home is shown from ~, any other as it is.
func TestTildePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, c := range []struct{ in, want string }{
		{filepath.Join(home, ".local", "share", "agentbus"), "~/.local/share/agentbus"},
		{"/srv/agentbus", "/srv/agentbus"},
		{home + "-other/agentbus", home + "-other/agentbus"},
	} {
		if got := tildePath(c.in); got != c.want {
			t.Errorf("tildePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCounted(t *testing.T) {
	for n, want := range map[int]string{0: "0 agents", 1: "1 agent", 3: "3 agents"} {
		if got := counted(n, "agent"); got != want {
			t.Errorf("counted(%d) = %q, want %q", n, got, want)
		}
	}
}
