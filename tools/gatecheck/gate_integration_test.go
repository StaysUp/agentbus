//go:build integration

package gatecheck

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Outside a git repository `git ls-files` lists nothing, and a `gofmt -l` with no
// files reads standard input. The gate must fail at once with a message instead.
func TestGateFailsOutsideGitCheckout(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "gate")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
	// A standard input that never ends: a step that reads it waits here.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	cmd.Stdin = pr
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()

	if ctx.Err() != nil {
		t.Fatalf("make gate did not finish in 30s outside a git checkout:\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("make gate should exit non-zero outside a git checkout, got %v:\n%s", err, out)
	}
	if !strings.Contains(string(out), "inside a git checkout") {
		t.Fatalf("make gate should say to run it inside a git checkout:\n%s", out)
	}
}
