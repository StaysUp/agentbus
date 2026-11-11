package runner

import (
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/queue"
)

// A copy a build before 0.4 made in <data>/worktrees, named after its job, is
// still found where it is; a new copy goes to Workdir.
func TestCopyInOldWorkdirIsFound(t *testing.T) {
	data := t.TempDir()
	folder := filepath.Join(t.TempDir(), "person")
	write(t, filepath.Join(folder, "a.txt"), "one\n")
	old := filepath.Join(data, "worktrees")
	b, _ := isolation.Get(isolation.Copy)
	s, err := b.Open(isolation.Request{ID: "job_1a746987f1136fbd", Workdir: old, Folder: folder})
	if err != nil {
		t.Fatal(err)
	}
	defer isolation.Close(s)
	r := &Runner{cfg: Config{Workdir: filepath.Join(data, "sessions"), OldWorkdir: old}}
	w := queue.Workspace{Kind: queue.WorkspaceCopy, Space: "job_1a746987f1136fbd"}
	got, err := r.loadSpace(w)
	if err != nil {
		t.Fatalf("a copy in the old workdir: %v", err)
	}
	if got.Root != s.Root {
		t.Fatalf("found %s, want %s", got.Root, s.Root)
	}
	if dir := r.workspaceDir(w); dir != s.Dir {
		t.Fatalf("workspaceDir = %q, want %q", dir, s.Dir)
	}
	// Without OldWorkdir it is not found: only Workdir is read.
	r.cfg.OldWorkdir = ""
	if _, err := r.loadSpace(w); err == nil {
		t.Fatal("a copy outside Workdir was found with no OldWorkdir")
	}
}
