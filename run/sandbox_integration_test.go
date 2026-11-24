//go:build integration && (darwin || linux)

package run

import (
	"os"
	"path/filepath"
	"testing"
)

// home_files are copied into a session's own home at the start of each turn.
// A file it does not list stays out (TestCopyGrants* cover the rest of the
// copy's rules).
func TestHomeFilesTurn(t *testing.T) {
	user, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", user)
	if err := os.MkdirAll(filepath.Join(user, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".aws", "config"), []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".aws", "credentials"), []byte("not granted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := sandboxEngine(t, "home_files = [\"~/.aws/config\"]\n", true, `cat "$HOME/.aws/config" > "$HOME/got.txt"; exit 0`)
	turn, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	home := filepath.Join(e.cfg.Homes, turn.Session)
	if b, err := os.ReadFile(filepath.Join(home, "got.txt")); err != nil || string(b) != "[default]\n" {
		t.Fatalf("the turn read %q %v from its home", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "credentials")); err == nil {
		t.Error("a file not listed was copied")
	}
}
