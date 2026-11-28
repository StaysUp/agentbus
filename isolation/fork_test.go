package isolation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A copy made from another copy at a checkpoint holds the checkpoint's files,
// not the source's live ones; keeps the source's ignored folders; starts with
// one checkpoint, its Base, with nothing changed; records its own turns; and
// leaves the source exactly as it was.
func TestForkHoldsTheCheckpoint(t *testing.T) {
	folder := t.TempDir()
	fixture(t, folder, map[string]string{"a.txt": "a0", "keep.txt": "k", ".gitignore": "cache/\n"})
	b, src := open(t, Copy, folder)
	// Turn t1 edits a.txt and adds b.txt; a build fills an ignored folder.
	fixture(t, src.Dir, map[string]string{"a.txt": "a1", "b.txt": "b1", "cache/obj": "o1", "node_modules/m/index.js": "m"})
	if _, err := b.Checkpoint(src, "t1"); err != nil {
		t.Fatal(err)
	}
	// The source's turn goes on after the checkpoint: these must not reach the fork.
	fixture(t, src.Dir, map[string]string{"a.txt": "a2", "late.txt": "late"})
	if err := os.Remove(filepath.Join(src.Dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}

	fork, err := b.Open(Request{ID: "job_2", Workdir: src.Workdir, From: src, FromCheckpoint: "t1"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	t.Cleanup(func() { b.Close(fork) })

	for p, want := range map[string]string{"a.txt": "a1", "b.txt": "b1", "keep.txt": "k", "late.txt": "<missing>",
		"cache/obj": "o1", "node_modules/m/index.js": "m"} {
		if got := read(t, filepath.Join(fork.Dir, p)); got != want {
			t.Errorf("fork %s = %q, want %q", p, got, want)
		}
	}
	if got := names(t, b, fork); len(got) != 1 || got[0] != Base {
		t.Fatalf("fork checkpoints = %v, want only %s", got, Base)
	}
	if fork.From != src.ID || fork.FromCheckpoint != "t1" || fork.Ref != src.Ref {
		t.Fatalf("fork record = from %q at %q, ref %q", fork.From, fork.FromCheckpoint, fork.Ref)
	}
	// Nothing changed yet: the fork's first turn starts from a clean Base.
	if _, err := b.Checkpoint(fork, "f1"); err != nil {
		t.Fatal(err)
	}
	if c, err := b.Diff(fork, Base, Base); err != nil || len(c.Files) != 0 {
		t.Fatalf("an unchanged fork has changes %v (%v)", c.Files, err)
	}
	// The fork's own turn is its own: its diff is against the checkpoint.
	fixture(t, fork.Dir, map[string]string{"b.txt": "fork"})
	if _, err := b.Checkpoint(fork, "f2"); err != nil {
		t.Fatal(err)
	}
	c, err := b.Diff(fork, Base, "f2")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Files) != 1 || c.Files[0].Path != "b.txt" {
		t.Fatalf("fork diff = %v, want b.txt only", c.Files)
	}
	// The source is untouched: its live files and its checkpoints.
	for p, want := range map[string]string{"a.txt": "a2", "b.txt": "b1", "late.txt": "late", "keep.txt": "<missing>"} {
		if got := read(t, filepath.Join(src.Dir, p)); got != want {
			t.Errorf("source %s = %q, want %q", p, got, want)
		}
	}
	if got := names(t, b, src); len(got) != 2 || got[1] != "t1" {
		t.Fatalf("source checkpoints = %v", got)
	}
	// A retry of the job that made the fork finds it as it is.
	again, err := b.Open(Request{ID: "job_2", Workdir: src.Workdir, From: src, FromCheckpoint: "t1"})
	if err != nil || read(t, filepath.Join(again.Dir, "b.txt")) != "fork" {
		t.Fatalf("a retry made the fork again (%v)", err)
	}
	// It loads like any copy, and closing it leaves the source alone.
	if _, err := LoadID(src.Workdir, "job_2"); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(fork); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src.Dir, "a.txt")) != "a2" {
		t.Fatal("closing the fork changed the source")
	}
}

// A fork of the source's Base holds the folder as the source was made.
func TestForkOfBase(t *testing.T) {
	folder := t.TempDir()
	fixture(t, folder, map[string]string{"a.txt": "a0"})
	b, src := open(t, Copy, folder)
	fixture(t, src.Dir, map[string]string{"a.txt": "a1"})
	fork, err := b.Open(Request{ID: "job_2", Workdir: src.Workdir, From: src, FromCheckpoint: Base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(fork) })
	if got := read(t, filepath.Join(fork.Dir, "a.txt")); got != "a0" {
		t.Fatalf("fork of base a.txt = %q", got)
	}
}

// A checkpoint the source does not hold, a bad name, the source itself, and a
// source with no private git are refused in words, and leave nothing behind.
func TestForkRefusals(t *testing.T) {
	folder := t.TempDir()
	fixture(t, folder, map[string]string{"a.txt": "a0"})
	b, src := open(t, Copy, folder)
	noGit := *src
	noGit.Shadow = false
	cases := map[string]Request{
		"missing":    {ID: "job_2", From: src, FromCheckpoint: "t_nope"},
		"bad name":   {ID: "job_2", From: src, FromCheckpoint: "../x"},
		"itself":     {ID: src.ID, From: src, FromCheckpoint: Base},
		"no history": {ID: "job_2", From: &noGit, FromCheckpoint: Base},
		"no source":  {ID: "job_2", From: &Space{}, FromCheckpoint: Base},
	}
	for name, r := range cases {
		r.Workdir = src.Workdir
		_, err := b.Open(r)
		var rf *Refusal
		if !errors.As(err, &rf) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
		if r.ID != src.ID {
			if _, err := LoadID(src.Workdir, r.ID); err == nil {
				t.Errorf("%s: a refused fork left a copy", name)
			}
			if exists(filepath.Join(src.Workdir, r.ID)) {
				t.Errorf("%s: a refused fork left files", name)
			}
		}
	}
}

// A copy with no private git (here a git worktree; an overlay on Linux is the
// same) is forked as it is when the fork is made, ignored folders included,
// with no link to the person's repo; a checkpoint it does not hold is refused.
func TestForkOfACopyWithNoHistory(t *testing.T) {
	repo := t.TempDir()
	fixture(t, repo, map[string]string{"a.txt": "a0", ".gitignore": "cache/\n"})
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	b, src := open(t, Git, repo)
	if src.hasShadow() {
		t.Skip("the git backend now keeps a private git")
	}
	fixture(t, src.Dir, map[string]string{"a.txt": "a1", "cache/obj": "o"})
	if _, err := b.Checkpoint(src, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(Request{ID: "job_3", Workdir: src.Workdir, From: src, FromCheckpoint: "t_nope"}); err == nil {
		t.Fatal("a checkpoint the source does not hold was not refused")
	}
	cb, _ := Get(Copy)
	fork, err := cb.Open(Request{ID: "job_2", Workdir: src.Workdir, From: src, FromCheckpoint: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cb.Close(fork) })
	if read(t, filepath.Join(fork.Dir, "a.txt")) != "a1" || read(t, filepath.Join(fork.Dir, "cache/obj")) != "o" {
		t.Fatal("the fork does not hold the source's files")
	}
	if fi, err := os.Lstat(filepath.Join(fork.Dir, ".git")); err == nil && !fi.IsDir() {
		t.Fatal("the fork kept the worktree's link to the person's repo")
	}
	fixture(t, fork.Dir, map[string]string{"b.txt": "b"})
	if _, err := cb.Checkpoint(fork, "f1"); err != nil {
		t.Fatal(err)
	}
	if c, err := cb.Diff(fork, Base, "f1"); err != nil || len(c.Files) != 1 || c.Files[0].Path != "b.txt" {
		t.Fatalf("fork diff %v (%v), want b.txt", c.Files, err)
	}
}
