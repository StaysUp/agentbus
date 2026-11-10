package isolation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// fixture writes files under dir: path -> content.
func fixture(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func open(t *testing.T, name, folder string) (Backend, *Space) {
	t.Helper()
	b, _ := Get(name)
	s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "worktrees"), Folder: folder})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// An overlay copy left mounted would keep its temp folder from going.
	t.Cleanup(func() { b.Close(s) })
	return b, s
}

func names(t *testing.T, b Backend, s *Space) []string {
	t.Helper()
	list, err := b.Checkpoints(s)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

// The copy is the folder as it is, a turn's edits stay in the copy, the
// diff names each one with its content, and an apply puts them all in the
// person's folder.
func TestCopyRoundTrip(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "sub/b.txt": "b1", ".hidden": "h", "keep.txt": "k"})
	os.Chmod(filepath.Join(src, "keep.txt"), 0o600)
	os.Symlink("a.txt", filepath.Join(src, "link"))
	b, s := open(t, Copy, src)
	if s.Dir == src || read(t, filepath.Join(s.Dir, "sub", "b.txt")) != "b1" || read(t, filepath.Join(s.Dir, ".hidden")) != "h" {
		t.Fatalf("the copy is not the folder: %+v", s)
	}
	if target, _ := os.Readlink(filepath.Join(s.Dir, "link")); target != "a.txt" {
		t.Fatalf("the link was not copied as a link: %q", target)
	}
	t.Logf("copy method: %s", s.Method)

	// The turn.
	fixture(t, s.Dir, map[string]string{"a.txt": "a2", "new/c.txt": "c1"})
	os.Remove(filepath.Join(s.Dir, "sub", "b.txt"))
	os.Remove(filepath.Join(s.Dir, "link"))
	os.Symlink("new/c.txt", filepath.Join(s.Dir, "link"))
	os.Chmod(filepath.Join(s.Dir, "keep.txt"), 0o755)
	if read(t, filepath.Join(src, "a.txt")) != "a1" {
		t.Fatal("an edit of the copy reached the folder")
	}
	end, err := b.Checkpoint(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Diff(s, Base, end.Name)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range c.Files {
		got[f.Path] = f.Change
	}
	want := map[string]string{"a.txt": "modified", "new/c.txt": "added", "sub/b.txt": "deleted", "link": "modified", "keep.txt": "modified"}
	if !reflect.DeepEqual(got, want) || c.From != Base || c.To != "t1" {
		t.Fatalf("diff %v from %s to %s, want %v", got, c.From, c.To, want)
	}
	for _, f := range c.Files {
		if f.Path == "new/c.txt" && (f.Content == nil || *f.Content != "c1") {
			t.Fatalf("no content for an added file: %+v", f)
		}
	}

	a, err := b.Apply(s, Base, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Files) != 5 || !a.Pruned {
		t.Fatalf("applied %+v", a)
	}
	if read(t, filepath.Join(src, "a.txt")) != "a2" || read(t, filepath.Join(src, "new", "c.txt")) != "c1" ||
		read(t, filepath.Join(src, "sub", "b.txt")) != "<missing>" {
		t.Fatal("the folder does not hold the turn's changes")
	}
	if target, _ := os.Readlink(filepath.Join(src, "link")); target != "new/c.txt" {
		t.Fatalf("link %q", target)
	}
	if fi, _ := os.Stat(filepath.Join(src, "keep.txt")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// The apply started at the oldest checkpoint: the ones before t1 went.
	if n := names(t, b, s); !reflect.DeepEqual(n, []string{"t1"}) {
		t.Fatalf("checkpoints after apply %v", n)
	}
	if err := b.Close(s); err != nil {
		t.Fatal(err)
	}
}

// A checkpoint of a space nothing changed is the latest one: a turn's start
// and an empty turn record nothing.
func TestCheckpointUnchangedIsTheLatest(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	c, err := b.Checkpoint(s, "t1.start")
	if err != nil || c.Name != Base {
		t.Fatalf("start of the first turn: %+v %v", c, err)
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	c1, _ := b.Checkpoint(s, "t1")
	c2, _ := b.Checkpoint(s, "t2.start")
	c3, _ := b.Checkpoint(s, "t2")
	if c1.Name != "t1" || c2.Name != "t1" || c3.Name != "t1" {
		t.Fatalf("%v %v %v", c1, c2, c3)
	}
	// The same name again is the checkpoint already taken.
	fixture(t, s.Dir, map[string]string{"a.txt": "a3"})
	if again, _ := b.Checkpoint(s, "t1"); again.Seq != c1.Seq {
		t.Fatalf("again %+v", again)
	}
	if _, err := b.Checkpoint(s, "../x"); !errors.Is(err, ErrBadName) {
		t.Fatalf("bad name: %v", err)
	}
	if c, err := b.Diff(s, "t1", Base); !errors.Is(err, ErrOrder) {
		t.Fatalf("backwards: %+v %v", c, err)
	}
}

// A file of the person's folder changed since the checkpoint refuses the
// whole apply, with the reason and each file, and nothing is written. A
// file already as the turn left it is no conflict.
func TestApplyConflictRefused(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "b.txt": "b1", "c.txt": "c1", "d.txt": "d1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2", "b.txt": "b2", "new.txt": "n", "same.txt": "s", "d.txt": "d2"})
	os.Remove(filepath.Join(s.Dir, "c.txt"))
	b.Checkpoint(s, "t1")
	// The person, meanwhile.
	fixture(t, src, map[string]string{"a.txt": "mine", "new.txt": "mine too", "same.txt": "s", "c.txt": "c-mine"})
	os.Remove(filepath.Join(src, "d.txt"))

	_, err := b.Apply(s, Base, "t1")
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatalf("want a conflict, got %v", err)
	}
	got := map[string]string{}
	for _, f := range c.Files {
		got[f.Path] = f.Why
	}
	want := map[string]string{"a.txt": whyChanged, "new.txt": whyAdded, "c.txt": whyChanged, "d.txt": whyRemoved}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conflicts %v, want %v", got, want)
	}
	if !strings.Contains(err.Error(), "4 file(s)") || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("message %q", err)
	}
	if read(t, filepath.Join(src, "b.txt")) != "b1" || read(t, filepath.Join(src, "a.txt")) != "mine" {
		t.Fatal("a refused apply wrote a file")
	}
	// Nothing was pruned: the turn can still be applied once resolved.
	if n := names(t, b, s); !reflect.DeepEqual(n, []string{Base, "t1"}) {
		t.Fatalf("checkpoints %v", n)
	}
	// Resolved: the person takes back their edits.
	fixture(t, src, map[string]string{"a.txt": "a1", "c.txt": "c1", "d.txt": "d1"})
	os.Remove(filepath.Join(src, "new.txt"))
	if _, err := b.Apply(s, Base, "t1"); err != nil {
		t.Fatalf("after resolving: %v", err)
	}
	if read(t, filepath.Join(src, "a.txt")) != "a2" || read(t, filepath.Join(src, "same.txt")) != "s" {
		t.Fatal("not applied")
	}
}

// An older checkpoint applies as it was, from its saved files, though a
// later turn changed the file again; then the later one applies on top.
func TestApplyOneTurnAtATime(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "v1", "b.txt": "b1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "v2"})
	b.Checkpoint(s, "t1")
	fixture(t, s.Dir, map[string]string{"a.txt": "v3", "b.txt": "b2"})
	b.Checkpoint(s, "t2")
	// A turn's own diff is from its start.
	c, _ := b.Diff(s, "t1", "t2")
	if c.Count != 2 || c.Files[0].Content == nil || *c.Files[0].Content != "v3" {
		t.Fatalf("t2 diff %+v", c)
	}
	c, _ = b.Diff(s, Base, "t1")
	if c.Count != 1 || *c.Files[0].Content != "v2" {
		t.Fatalf("t1 diff %+v", c)
	}
	if _, err := b.Apply(s, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src, "a.txt")) != "v2" || read(t, filepath.Join(src, "b.txt")) != "b1" {
		t.Fatal("t1 is not applied as it was")
	}
	if _, err := b.Apply(s, "t1", "t2"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src, "a.txt")) != "v3" || read(t, filepath.Join(src, "b.txt")) != "b2" {
		t.Fatal("t2 is not applied")
	}
	objects, _ := os.ReadDir(s.objectDir())
	if n := names(t, b, s); !reflect.DeepEqual(n, []string{"t2"}) || len(objects) != 0 {
		t.Fatalf("after both applies: checkpoints %v, saved files %d", n, len(objects))
	}
}

// Close removes the copy and its record; Load then finds nothing.
func TestCloseRemovesTheCopy(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	os.MkdirAll(filepath.Join(s.Dir, "ro"), 0o755)
	fixture(t, s.Dir, map[string]string{"ro/x": "x"})
	os.Chmod(filepath.Join(s.Dir, "ro"), 0o555) // as a copied module cache is
	b.Checkpoint(s, "t1")
	if got, err := Load(s.Workdir, s.Dir); err != nil || got.Root != s.Root || got.Source != src {
		t.Fatalf("load %+v %v", got, err)
	}
	if err := Close(s); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Root, s.state()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", p, err)
		}
	}
	if _, err := Load(s.Workdir, s.Dir); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("load after close: %v", err)
	}
	if read(t, filepath.Join(src, "a.txt")) != "a1" {
		t.Fatal("close changed the folder")
	}
	if _, err := Load(s.Workdir, "/elsewhere"); !errors.Is(err, ErrOutside) {
		t.Fatalf("outside: %v", err)
	}
}

// A retried job gets the copy its first attempt made, edits and all.
func TestOpenAgainIsTheSameSpace(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "edited"})
	again, err := b.Open(Request{ID: s.ID, Workdir: s.Workdir, Folder: src})
	if err != nil || again.Root != s.Root || read(t, filepath.Join(again.Dir, "a.txt")) != "edited" {
		t.Fatalf("again %+v %v", again, err)
	}
	if _, err := b.Open(Request{ID: "job_2", Workdir: s.Workdir, Folder: filepath.Join(src, "nope")}); err == nil {
		t.Fatal("a missing folder opened")
	}
}

// A folder that holds the workdir, such as a home folder that holds the
// data folder, is refused: the copy would be inside what it copies.
func TestCopyRefusesAFolderHoldingTheWorkdir(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	b, _ := Get(Copy)
	_, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(src, ".local", "agentbus", "worktrees"), Folder: src})
	var rf *Refusal
	if !errors.As(err, &rf) || !strings.Contains(rf.Msg, "data folder") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if !holds(src, src) || holds(filepath.Join(src, "x"), src) {
		t.Fatal("holds")
	}
}

// A .git file at the folder's top, as a linked worktree has, names the
// person's own repo: the copy leaves it out.
func TestCopyLeavesOutAGitFile(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{".git": "gitdir: /their/repo/.git/worktrees/x", "a.txt": "a"})
	_, s := open(t, Copy, src)
	if _, err := os.Lstat(filepath.Join(s.Dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".git file copied: %v", err)
	}
	if read(t, filepath.Join(src, ".git")) == "<missing>" {
		t.Fatal("the person's .git file was removed")
	}
}

// copyTree, the copy where no clone of a whole folder is possible, keeps
// files, links, modes and the folders' own modes.
func TestCopyTreeByFiles(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "c")
	fixture(t, src, map[string]string{"x/y.sh": "#!/bin/sh", "z": "z"})
	os.Chmod(filepath.Join(src, "x", "y.sh"), 0o755)
	os.Symlink("../z", filepath.Join(src, "x", "l"))
	method, err := copyTree(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("method %s", method)
	a, _ := workspace.Walk(src, nil, nil)
	b, _ := workspace.Walk(dst, nil, nil)
	if ch := a.Changed(b); len(ch) != 0 {
		t.Fatalf("the copy differs: %v", ch)
	}
}

// A folder of a few thousand files: how long each step takes. The figures
// are logged, not asserted, since they are the disk's.
func TestBigFolderTimes(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	src := t.TempDir()
	const n = 3000
	body := strings.Repeat("x", 4096)
	for i := range n {
		fixture(t, src, map[string]string{fmt.Sprintf("d%02d/f%04d.txt", i%30, i): body})
	}
	b, _ := Get(Copy)
	start := time.Now()
	s, err := b.Open(Request{ID: "job_big", Workdir: filepath.Join(t.TempDir(), "w"), Folder: src})
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Since(start)
	start = time.Now()
	b.Checkpoint(s, "t1.start")
	unchanged := time.Since(start)
	fixture(t, s.Dir, map[string]string{"d00/f0000.txt": "changed", "d01/new.txt": "new"})
	start = time.Now()
	b.Checkpoint(s, "t1")
	changed := time.Since(start)
	start = time.Now()
	if _, err := b.Apply(s, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	applied := time.Since(start)
	start = time.Now()
	if err := b.Close(s); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d files of 4 KiB, %s: open %v, checkpoint unchanged %v, checkpoint after 2 edits %v, apply %v, close %v",
		n, s.Method, opened, unchanged, changed, applied, time.Since(start))
}

// A checkpoint's root is its snapshot id: two copies of one folder have the
// same one, an edit changes it, and reading the checkpoint again gives the
// root it was taken with. The tree holds each file's size, mode and
// modification time, so the id names this state of the files, not their
// content alone.
func TestCheckpointRootIsTheSnapshotID(t *testing.T) {
	a := t.TempDir()
	fixture(t, a, map[string]string{"x.txt": "1", "d/y.txt": "2"})
	ba, sa := open(t, Copy, a)
	bb, sb := open(t, Copy, a)
	ca, err1 := ba.Checkpoint(sa, Base)
	cb, err2 := bb.Checkpoint(sb, Base)
	if err1 != nil || err2 != nil || len(ca.Root) != 64 || ca.Root != cb.Root {
		t.Fatalf("roots %q %q (%v %v)", ca.Root, cb.Root, err1, err2)
	}
	fixture(t, sa.Dir, map[string]string{"x.txt": "changed"})
	c1, err := ba.Checkpoint(sa, "t1")
	if err != nil || c1.Root == "" || c1.Root == ca.Root {
		t.Fatalf("after an edit: %+v %v", c1, err)
	}
	list, err := ba.Checkpoints(sa)
	if err != nil || len(list) != 2 || list[0].Root != ca.Root || list[1].Root != c1.Root {
		t.Fatalf("read back: %+v %v", list, err)
	}
}
