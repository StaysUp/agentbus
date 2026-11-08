package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := git(dir, nil, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func byPath(c Changes) map[string]Change {
	m := map[string]Change{}
	for _, f := range c.Files {
		m[f.Path] = f
	}
	return m
}

// A plain folder: added, modified and deleted files, with size and hash; an
// unchanged file is not listed; a file touched with the same bytes is not a
// change.
func TestSnapshotPlainFolder(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep.txt"), "same")
	write(t, filepath.Join(dir, "edit.txt"), "old")
	write(t, filepath.Join(dir, "gone.txt"), "bye")
	write(t, filepath.Join(dir, "touch.txt"), "x")
	before, err := Take(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "edit.txt"), "new!")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, filepath.Join(dir, "sub", "new.txt"), "hello")
	write(t, filepath.Join(dir, "touch.txt"), "x")
	after, err := Take(dir, before)
	if err != nil {
		t.Fatal(err)
	}
	c := Diff(dir, before, after)
	m := byPath(c)
	if c.Kind != "snapshot" || c.Count != 3 || len(m) != 3 {
		t.Fatalf("%+v", c)
	}
	if e := m["edit.txt"]; e.Change != "modified" || e.Size != 4 || e.SHA256 != hashBytes([]byte("new!")) || e.Git != "" {
		t.Errorf("edit: %+v", e)
	}
	if e := m["gone.txt"]; e.Change != "deleted" || e.SHA256 != hashBytes([]byte("bye")) {
		t.Errorf("gone: %+v", e)
	}
	if e := m["sub/new.txt"]; e.Change != "added" || e.Size != 5 {
		t.Errorf("new: %+v", e)
	}
}

// A git worktree made at a base: tracked, untracked and git-ignored changes
// are all listed and labelled; .git is never listed; a commit shows in
// head_before and head_after. RemoveWorktree refuses a folder outside the
// workdir and removes one inside it, ignored files and all.
func TestWorktreeChanges(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, ".gitignore"), "*.log\nbuild/\n")
	write(t, filepath.Join(repo, "main.go"), "package main\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")

	workdir := t.TempDir()
	dir := filepath.Join(workdir, "job_1")
	if err := MakeWorktree(repo, "main", dir); err != nil {
		t.Fatal(err)
	}
	// Made again, as a second attempt of the same job would: used as it is.
	if err := MakeWorktree(repo, "main", dir); err != nil {
		t.Fatalf("again: %v", err)
	}
	if err := MakeWorktree(repo, "no-such-branch", filepath.Join(workdir, "job_2")); err == nil {
		t.Fatal("a worktree of a missing base was made")
	}
	before, err := Take(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !before.git || before.head == "" {
		t.Fatalf("not seen as git: %+v", before)
	}
	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(dir, "notes.md"), "draft")
	write(t, filepath.Join(dir, "run.log"), "output")
	write(t, filepath.Join(dir, "build", "out.bin"), "bin")
	mustGit(t, dir, "add", "notes.md")
	mustGit(t, dir, "commit", "-q", "-m", "two")
	write(t, filepath.Join(dir, "scratch.txt"), "untracked")
	after, err := Take(dir, before)
	if err != nil {
		t.Fatal(err)
	}
	c := Diff(dir, before, after)
	m := byPath(c)
	want := map[string][2]string{
		"main.go":       {"modified", "tracked"},
		"notes.md":      {"added", "tracked"},
		"run.log":       {"added", "ignored"},
		"build/out.bin": {"added", "ignored"},
		"scratch.txt":   {"added", "untracked"},
	}
	if c.Kind != "git" || c.Count != len(want) || c.HeadBefore == c.HeadAfter || c.HeadAfter == "" {
		t.Fatalf("%+v", c)
	}
	for p, w := range want {
		if e := m[p]; e.Change != w[0] || e.Git != w[1] {
			t.Errorf("%s: %+v, want %v", p, e, w)
		}
	}
	for p := range m {
		if p == ".git" {
			t.Error(".git is listed")
		}
	}

	if err := RemoveWorktree(workdir, repo, repo); err == nil {
		t.Fatal("removed a folder outside the workdir")
	}
	if err := RemoveWorktree(workdir, repo, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "main.go")); err != nil {
		t.Fatal("the repo itself was touched")
	}
	// Removing it again is not an error.
	if err := RemoveWorktree(workdir, repo, dir); err != nil {
		t.Fatalf("again: %v", err)
	}
}

// A folder inside a repo, not its top, is a plain Snapshot.
func TestFolderInsideRepoIsPlain(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	sub := filepath.Join(repo, "sub")
	write(t, filepath.Join(sub, "a.txt"), "a")
	s, err := Take(sub, nil)
	if err != nil || s.git {
		t.Fatalf("git %v err %v", s.git, err)
	}
}

// A job that writes many small files still gets a result that can be sent:
// the contents share one budget, and Fit shrinks the rest to a bound.
func TestManyFilesFit(t *testing.T) {
	root := t.TempDir()
	before, err := Take(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		write(t, filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), strings.Repeat("a", 8<<10))
	}
	after, err := Take(root, before)
	if err != nil {
		t.Fatal(err)
	}
	c := Diff(root, before, after)
	total := 0
	for _, f := range c.Files {
		if f.Content != nil {
			total += len(*f.Content)
		}
	}
	if c.Count != 400 || total > MaxContents || total == 0 || !c.ContentLeftOut {
		t.Fatalf("count %d, content %d bytes, left out %v", c.Count, total, c.ContentLeftOut)
	}
	c.Fit(16 << 10)
	b, _ := json.Marshal(c)
	if len(b) > 16<<10 || !c.Truncated || c.Count != 400 || len(c.Files) == 0 {
		t.Fatalf("fit: %d bytes, truncated %v, %d of %d files", len(b), c.Truncated, len(c.Files), c.Count)
	}
	for _, f := range c.Files {
		if f.Content != nil {
			t.Fatalf("%s kept its content", f.Path)
		}
	}
}

// A changed symlink is listed, but its target's content is never read into
// the result: a link to a file outside the workspace cannot carry it out.
func TestSymlinkContentNotFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "private.txt"), "outside secret")
	before, err := Take(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "plain.txt"), "kept")
	after, err := Take(dir, before)
	if err != nil {
		t.Fatal(err)
	}
	m := byPath(Diff(dir, before, after))
	l, ok := m["link.txt"]
	if !ok || l.Change != "added" {
		t.Fatalf("the link is not listed as added: %+v", m)
	}
	if l.Content != nil {
		t.Fatalf("the link's target was read into the result: %q", *l.Content)
	}
	if p := m["plain.txt"]; p.Content == nil || *p.Content != "kept" {
		t.Fatalf("a plain file's content is missing: %+v", p)
	}
}

// A folder swapped for a symlink to an outside folder after the first
// snapshot does not carry the outside files' content into the result either.
func TestSymlinkedFolderContentNotFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dir, "sub", "notes.txt"), "inside")
	write(t, filepath.Join(outside, "notes.txt"), "outside secret")
	before, err := Take(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	after, err := Take(dir, before)
	if err != nil {
		t.Fatal(err)
	}
	c := Diff(dir, before, after)
	// Whatever the snapshot lists, no file of the result may hold the outside text.
	for _, f := range c.Files {
		if f.Content != nil && strings.Contains(*f.Content, "outside") {
			t.Fatalf("%s carried outside content: %q", f.Path, *f.Content)
		}
	}
	// The reader itself refuses the path through the linked folder.
	b, ok := readInside(mustRoot(t, dir), "sub/notes.txt", 1<<20)
	if ok {
		t.Fatalf("read through a symlinked folder: %q", b)
	}
}

func mustRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// RepoOf finds the repo a folder is in, the folder's place in it and HEAD's
// commit; a folder outside git or a repo with no commit is not isolated.
func TestRepoOf(t *testing.T) {
	plain := t.TempDir()
	if _, _, _, ok := RepoOf(plain); ok {
		t.Fatal("a plain folder is a repo")
	}
	empty := t.TempDir()
	mustGit(t, empty, "init", "-q")
	if _, _, _, ok := RepoOf(empty); ok {
		t.Fatal("a repo with no commit counts")
	}
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "a", "b", "f.txt"), "x")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")
	real, _ := filepath.EvalSymlinks(repo)
	for dir, wantRel := range map[string]string{repo: ".", filepath.Join(repo, "a", "b"): filepath.Join("a", "b")} {
		top, rel, head, ok := RepoOf(dir)
		if !ok || top != real || rel != wantRel || len(head) != 40 {
			t.Errorf("%s: %q %q %q %v", dir, top, rel, head, ok)
		}
	}
}

// A folder whose name starts with two dots is inside the repo.
func TestRepoOfDotDotName(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "..data", "f.txt"), "x")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")
	if _, rel, _, ok := RepoOf(filepath.Join(repo, "..data")); !ok || rel != "..data" {
		t.Fatalf("%q %v", rel, ok)
	}
}
