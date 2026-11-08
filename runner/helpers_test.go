package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/workspace"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func byPath(c workspace.Changes) map[string]workspace.Change {
	m := map[string]workspace.Change{}
	for _, f := range c.Files {
		m[f.Path] = f
	}
	return m
}
