//go:build integration && (darwin || linux)

package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A turn and a ! command with [sandbox] on run in the wall: each writes its
// folder and its home, and cannot write beside its folder.
func TestSandboxTurnAndCommand(t *testing.T) {
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
	ran, err := e.RunCommand(context.Background(), Command{Session: turn.Session, Folder: folder, ID: "cmd_1",
		Line: "echo c > c.txt; echo out > " + filepath.Join(outside, "cmd.txt")})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if ran.Command == nil {
		t.Fatal("no command.ran")
	}
	if _, err := os.Stat(filepath.Join(folder, "c.txt")); err != nil {
		t.Errorf("the command could not write its folder")
	}
	if _, err := os.Stat(filepath.Join(outside, "cmd.txt")); err == nil {
		t.Errorf("the command wrote outside its folder")
	}
}

// A ! command sent while a turn of its session runs is refused before its
// sandbox is built: the grants are not copied again into the running
// turn's home.
func TestSandboxCommandLeavesRunningTurnsHome(t *testing.T) {
	canSandbox(t)
	user := t.TempDir()
	t.Setenv("HOME", user)
	if err := os.MkdirAll(filepath.Join(user, ".grant"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".grant", "g"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The turn changes its copy of the grant, then marks that it did.
	e, _ := sandboxEngine(t, "[sandbox]\nmode = \"on\"\nread = [\"~/.grant\"]\n", true,
		`echo changed > "$HOME/.grant/g"; echo mine > "$HOME/mine"; sleep 3`)
	folder := t.TempDir()
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	mine := filepath.Join(e.cfg.Homes, turn.Session, "mine")
	g := filepath.Join(e.cfg.Homes, turn.Session, ".grant", "g")
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(mine); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err = e.RunCommand(context.Background(), Command{Session: turn.Session, Folder: folder, ID: "cmd_1", Line: "true"})
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Msg, TurnRunning) {
		t.Fatalf("RunCommand: %v, want %q", err, TurnRunning)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("the turn did not run")
	}
	if b, _ := os.ReadFile(g); string(b) != "changed\n" {
		t.Error("the refused command copied the grants over the running turn's home")
	}
	<-turn.Done()
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
