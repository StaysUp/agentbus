//go:build integration

package isolation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseRepo is repo(t) with a second branch, feature, one commit ahead of
// the first: sub/a.txt changed, sub/feature.txt added. The folder is back
// on the first branch, with its uncommitted edits, an ignored .env, an
// ignored build.log, and an untracked sub/.env.local.
func baseRepo(t *testing.T) (r, feature string) {
	t.Helper()
	r = repo(t)
	gitRun(t, r, "stash", "push", "-q", "-u")
	main := strings.TrimSpace(gitRun(t, r, "rev-parse", "--abbrev-ref", "HEAD"))
	gitRun(t, r, "checkout", "-q", "-b", "feature")
	fixture(t, r, map[string]string{"sub/a.txt": "a-feature", "sub/feature.txt": "f"})
	gitRun(t, r, "add", ".")
	gitRun(t, r, "commit", "-q", "-m", "feature")
	feature = strings.TrimSpace(gitRun(t, r, "rev-parse", "HEAD"))
	gitRun(t, r, "checkout", "-q", main)
	gitRun(t, r, "stash", "pop", "-q")
	fixture(t, r, map[string]string{".gitignore": "*.log\n.env*\n", "sub/.env": "SECRET=1", "sub/.env.local": "L=1"})
	return r, feature
}

// A worktree at a base: the files are the base commit's, none of the
// folder's uncommitted edits, and only the ignored files carry_files names.
// A pattern that names a file the base holds keeps the base's content. The
// person's folder, its HEAD and its branch are not changed.
func TestGitAtABase(t *testing.T) {
	r, feature := baseRepo(t)
	src := filepath.Join(r, "sub")
	head := gitRun(t, r, "rev-parse", "HEAD")
	b, _ := Get(Git)
	w := filepath.Join(t.TempDir(), "w")
	s, err := b.Open(Request{ID: "job_1", Workdir: w, Folder: src, Base: feature, Carry: []string{".env", "a.txt", "*.log.none"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Base != feature || s.Dir != filepath.Join(s.Root, "sub") {
		t.Fatalf("space %+v", s)
	}
	for p, want := range map[string]string{"a.txt": "a-feature", "feature.txt": "f", "keep.txt": "k", "gone.txt": "g",
		"untracked.txt": "<missing>", ".env": "SECRET=1", ".env.local": "<missing>", "build.log": "<missing>"} {
		if got := read(t, filepath.Join(s.Dir, p)); got != want {
			t.Errorf("%s in the worktree: %q, want %q", p, got, want)
		}
	}
	// The carried file is in the base checkpoint, so it is no turn's change.
	end, _ := b.Checkpoint(s, "t1")
	if c, _ := b.Diff(s, Base, end.Name); c.Count != 0 {
		t.Fatalf("diff with no turn %+v", c)
	}
	if gitRun(t, r, "rev-parse", "HEAD") != head || read(t, filepath.Join(src, "a.txt")) != "a-uncommitted" {
		t.Fatal("the person's folder changed")
	}
	if err := b.Close(s); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gitRun(t, r, "worktree", "list"), s.Root) {
		t.Fatal("the worktree is still listed")
	}
}

// A base that is not a full commit id of the repo is refused, and nothing
// is made.
func TestGitAtABaseRefusesAnUnknownCommit(t *testing.T) {
	r, _ := baseRepo(t)
	b, _ := Get(Git)
	for _, base := range []string{"feature", strings.Repeat("a", 40), "--help"} {
		w := filepath.Join(t.TempDir(), "w")
		_, err := b.Open(Request{ID: "job_1", Workdir: w, Folder: r, Base: base})
		var rf *Refusal
		if !errors.As(err, &rf) || rf.Msg != refusedBase {
			t.Fatalf("%s: want the refusal, got %v", base, err)
		}
		if _, err := os.Stat(filepath.Join(w, "job_1")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: a worktree was made", base)
		}
	}
}

// A folder the base commit does not have is refused in its own words.
func TestGitAtABaseRefusesAFolderNotInIt(t *testing.T) {
	r, _ := baseRepo(t)
	first := strings.TrimSpace(gitRun(t, r, "rev-list", "--max-parents=0", "HEAD"))
	fixture(t, r, map[string]string{"later/x.txt": "x"})
	gitRun(t, r, "add", "later/x.txt")
	gitRun(t, r, "commit", "-q", "-m", "later")
	b, _ := Get(Git)
	_, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "w"), Folder: filepath.Join(r, "later"), Base: first})
	var rf *Refusal
	if !errors.As(err, &rf) || rf.Msg != refusedNotInBase {
		t.Fatalf("want the refusal, got %v", err)
	}
}

// carry_files copies regular files only. A symbolic link it names is left
// out, on purpose: its target is a path on the person's machine, which in the
// worktree could point anywhere, or at what the folder holds that the base
// does not.
func TestGitAtABaseSkipsACarriedLink(t *testing.T) {
	r, feature := baseRepo(t)
	src := filepath.Join(r, "sub")
	if err := os.Symlink(".env", filepath.Join(src, ".env.link")); err != nil {
		t.Fatal(err)
	}
	b, _ := Get(Git)
	s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "w"), Folder: src, Base: feature, Carry: []string{".env*"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(s)
	if _, err := os.Lstat(filepath.Join(s.Dir, ".env.link")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link was carried: %v", err)
	}
	if got := read(t, filepath.Join(s.Dir, ".env")); got != "SECRET=1" {
		t.Errorf(".env: %q", got)
	}
}

// carry_files reads the person's folder through a root: a pattern that goes
// through a link to a folder outside it carries nothing from there, while a
// link to a folder inside it still works. The secret is a dummy.
func TestGitAtABaseCarriesNothingFromOutside(t *testing.T) {
	r, feature := baseRepo(t)
	src := filepath.Join(r, "sub")
	outside := t.TempDir()
	fixture(t, outside, map[string]string{"prod.env": "OUTSIDE=dummy"})
	if err := os.Symlink(outside, filepath.Join(src, "config")); err != nil {
		t.Fatal(err)
	}
	fixture(t, src, map[string]string{"real/local.env": "INSIDE=1"})
	if err := os.Symlink("real", filepath.Join(src, "linked")); err != nil {
		t.Fatal(err)
	}
	b, _ := Get(Git)
	s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "w"), Folder: src, Base: feature,
		Carry: []string{"config/*.env", "linked/*.env"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(s)
	if got := read(t, filepath.Join(s.Dir, "config", "prod.env")); got != "<missing>" {
		t.Errorf("a file outside the folder was carried: %q", got)
	}
	if got := read(t, filepath.Join(s.Dir, "linked", "local.env")); got != "INSIDE=1" {
		t.Errorf("a file through a link inside the folder was not carried: %q", got)
	}
}
