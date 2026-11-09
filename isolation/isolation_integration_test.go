//go:build integration

package isolation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

// repo is a git repo with sub/ committed, then uncommitted edits: a changed
// tracked file, a removed one, an untracked one and an ignored one.
func repo(t *testing.T) string {
	t.Helper()
	r := t.TempDir()
	gitRun(t, r, "init", "-q")
	fixture(t, r, map[string]string{".gitignore": "*.log\n", "sub/a.txt": "a1", "sub/gone.txt": "g", "sub/keep.txt": "k", "top.txt": "t"})
	gitRun(t, r, "add", ".")
	gitRun(t, r, "commit", "-q", "-m", "one")
	fixture(t, r, map[string]string{"sub/a.txt": "a-uncommitted", "sub/untracked.txt": "u", "sub/build.log": "ignored"})
	os.Remove(filepath.Join(r, "sub", "gone.txt"))
	return r
}

// The git backend: a worktree at HEAD with the folder's uncommitted edits
// carried in and its ignored files left out; a turn's diff from git; an
// apply into the folder; close removes the worktree from the repo.
func TestGitRoundTrip(t *testing.T) {
	r := repo(t)
	src := filepath.Join(r, "sub")
	b, s := open(t, Git, src)
	if s.Dir != filepath.Join(s.Root, "sub") || s.Source != src {
		t.Fatalf("space %+v", s)
	}
	for p, want := range map[string]string{"a.txt": "a-uncommitted", "untracked.txt": "u", "gone.txt": "<missing>",
		"build.log": "<missing>", "keep.txt": "k"} {
		if got := read(t, filepath.Join(s.Dir, p)); got != want {
			t.Errorf("%s in the worktree: %q, want %q", p, got, want)
		}
	}
	fixture(t, s.Dir, map[string]string{"keep.txt": "k2", "made.txt": "m"})
	b.Checkpoint(s, "t1")
	c, err := b.Diff(s, Base, "t1")
	if err != nil || c.Count != 2 {
		t.Fatalf("diff %+v %v", c, err)
	}
	if _, err := b.Apply(s, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src, "keep.txt")) != "k2" || read(t, filepath.Join(src, "made.txt")) != "m" ||
		read(t, filepath.Join(src, "a.txt")) != "a-uncommitted" || read(t, filepath.Join(src, "build.log")) != "ignored" {
		t.Fatal("the folder after apply is wrong")
	}
	if err := b.Close(s); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gitRun(t, r, "worktree", "list"), s.Root) {
		t.Fatal("the worktree is still listed")
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree left: %v", err)
	}
}

// A worktree at the repo's top is a git work tree, so its diff is git's:
// tracked files labelled and a unified diff.
func TestGitDiffAtTheTop(t *testing.T) {
	r := repo(t)
	b, s := open(t, Git, r)
	fixture(t, s.Dir, map[string]string{"top.txt": "t2"})
	b.Checkpoint(s, "t1")
	c, err := b.Diff(s, Base, "t1")
	if err != nil || c.Kind != "git" || c.Count != 1 || c.Files[0].Git != "tracked" || !strings.Contains(c.Diff, "+t2") {
		t.Fatalf("diff %+v %v", c, err)
	}
	b.Close(s)
}

// The copy backend in a repo: the copy has everything, the ignored file and
// a .git of its own, so git works there and a commit stays there.
func TestCopyOfARepo(t *testing.T) {
	r := repo(t)
	b, s := open(t, Copy, r)
	if read(t, filepath.Join(s.Dir, "sub", "build.log")) != "ignored" || read(t, filepath.Join(s.Dir, "sub", "a.txt")) != "a-uncommitted" {
		t.Fatal("the copy is not the folder as it is")
	}
	before := gitRun(t, r, "rev-parse", "HEAD")
	fixture(t, s.Dir, map[string]string{"top.txt": "t2"})
	gitRun(t, s.Dir, "commit", "-q", "-am", "in the copy")
	if gitRun(t, r, "rev-parse", "HEAD") != before {
		t.Fatal("a commit in the copy reached the repo")
	}
	end, _ := b.Checkpoint(s, "t1")
	c, _ := b.Diff(s, Base, end.Name)
	if c.Kind != "git" || c.HeadBefore == c.HeadAfter {
		t.Fatalf("diff %+v", c)
	}
	b.Close(s)
}

// A folder the last commit does not have cannot get a worktree, and none is
// left behind.
func TestGitRefusesAFolderNotInTheCommit(t *testing.T) {
	r := repo(t)
	fixture(t, r, map[string]string{"new/x.txt": "x"})
	b, _ := Get(Git)
	w := filepath.Join(t.TempDir(), "w")
	_, err := b.Open(Request{ID: "job_1", Workdir: w, Folder: filepath.Join(r, "new")})
	var rf *Refusal
	if !errors.As(err, &rf) || !strings.Contains(rf.Msg, "last commit") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if entries, _ := os.ReadDir(w); len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		// The record folder may be there, empty.
		if len(names) != 1 || names[0] != stateFolder {
			t.Fatalf("left behind: %v", names)
		}
	}
}

// The setting: copy by default; git in a repo, copy elsewhere; none is no
// backend.
func TestForTheSetting(t *testing.T) {
	r, plain := repo(t), t.TempDir()
	for _, c := range []struct {
		setting, folder, want string
	}{{"", r, Copy}, {"copy", r, Copy}, {"git", r, Git}, {"git", filepath.Join(r, "sub"), Git}, {"git", plain, Copy}, {"none", r, ""}} {
		b, ok := For(c.setting, c.folder)
		got := ""
		if ok {
			got = b.Name()
		}
		if got != c.want {
			t.Errorf("For(%q, %s) = %q, want %q", c.setting, c.folder, got, c.want)
		}
	}
}
