//go:build integration && (darwin || linux)

package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/sandbox"
)

// canSandbox skips where this machine cannot build the wall.
func canSandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	box, err := sandbox.New(sandbox.Spec{Copy: dir, Home: dir, Net: netjail.Spec{Open: true}})
	if err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	box.Close()
}

// A turn with [sandbox] on runs in the wall: it writes its folder and its
// home, and cannot write beside its folder.
func TestSandboxTurn(t *testing.T) {
	canSandbox(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder, outside := filepath.Join(root, "folder"), filepath.Join(root, "outside")
	for _, d := range []string{folder, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `echo in > in.txt; echo home > "$HOME/home.txt"; echo out > ` + filepath.Join(outside, "turn.txt") + `; exit 0`
	e, _ := sandboxEngine(t, "[sandbox]\nmode = \"on\"\n", true, script)
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	if _, err := os.Stat(filepath.Join(folder, "in.txt")); err != nil {
		t.Errorf("the turn could not write its folder")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Homes, turn.Session, "home.txt")); err != nil {
		t.Errorf("the turn could not write its home")
	}
	if _, err := os.Stat(filepath.Join(outside, "turn.txt")); err == nil {
		t.Errorf("the turn wrote outside its folder")
	}
}

// With edit = "none" the wall keeps the folder read only for codex, whose
// own read-only sandbox is off in it, and not for claude, which had none.
func TestSandboxEditNone(t *testing.T) {
	canSandbox(t)
	for _, c := range []struct {
		harness string
		writes  bool
	}{{"claude", true}, {"codex", false}} {
		t.Run(c.harness, func(t *testing.T) {
			e, _ := sandboxEngine(t, "edit = \"none\"\n[sandbox]\nmode = \"on\"\n", true, "echo in > in.txt")
			folder := t.TempDir()
			turn, err := e.Start(Request{Harness: c.harness, Folder: folder, Prompt: "hi"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-turn.Done()
			_, err = os.Stat(filepath.Join(folder, "in.txt"))
			if got := err == nil; got != c.writes {
				t.Errorf("wrote its folder: %t, want %t", got, c.writes)
			}
		})
	}
}
