package isolation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A folder used as it is gets the same checkpoints and diff as a copy, with
// no from and to, since nothing can be applied. A file added gets its diff
// and content, read from the folder; a modified file's earlier content is
// not kept, so its diff is left out and its content is there; a touched
// file is no change. Close removes the record and never the folder.
func TestNoneChangesInPlace(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, map[string]string{"keep.txt": "k", "edit.txt": "one\n", "gone.txt": "g", "touch.txt": "x"})
	b, s := open(t, None, dir)
	if s.Dir != dir || s.Root != dir || s.Source != "" {
		t.Fatalf("space %+v", s)
	}
	fixture(t, dir, map[string]string{"edit.txt": "two\n", "new.txt": "n\n", "touch.txt": "x"})
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(dir, "touch.txt"), later, later)
	os.Remove(filepath.Join(dir, "gone.txt"))
	end, err := b.Checkpoint(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Diff(s, Base, end.Name)
	if err != nil || c.From != "" || c.To != "" || c.Count != 3 || c.Kind != "snapshot" {
		t.Fatalf("diff %+v %v", c, err)
	}
	m := map[string]int{}
	for i, f := range c.Files {
		m[f.Path] = i
	}
	if f := c.Files[m["new.txt"]]; f.Change != "added" || f.Diff == nil || !strings.Contains(*f.Diff, "+n") || f.Content == nil || *f.Content != "n\n" {
		t.Errorf("new.txt: %+v", f)
	}
	if f := c.Files[m["edit.txt"]]; f.Change != "modified" || f.Diff != nil || !f.DiffLeftOut || f.Content == nil || *f.Content != "two\n" {
		t.Errorf("edit.txt: %+v", f)
	}
	if f := c.Files[m["gone.txt"]]; f.Change != "deleted" {
		t.Errorf("gone.txt: %+v", f)
	}
	if _, err := b.Apply(s, Base, end.Name); !errors.Is(err, ErrNoSource) {
		t.Fatalf("apply: %v", err)
	}
	if _, err := Load(s.Workdir, dir); err == nil {
		t.Fatal("a folder used as it is loads as a copy")
	}
	state := s.state()
	if err := Close(s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("the record is still there: %v", err)
	}
	if read(t, filepath.Join(dir, "edit.txt")) != "two\n" || read(t, filepath.Join(dir, "keep.txt")) != "k" {
		t.Fatal("close touched the folder")
	}
}

// A file whose content changes after the end checkpoint has no content in
// the diff: what is read must still have the checkpoint's hash.
func TestNoneReadsOnlyWhatTheCheckpointSaw(t *testing.T) {
	dir := t.TempDir()
	b, s := open(t, None, dir)
	defer Close(s)
	fixture(t, dir, map[string]string{"a.txt": "first\n"})
	end, _ := b.Checkpoint(s, "t1")
	fixture(t, dir, map[string]string{"a.txt": "second\n"})
	c, err := b.Diff(s, Base, end.Name)
	if err != nil || c.Count != 1 || c.Files[0].Content != nil || c.Files[0].Diff != nil || !c.Files[0].DiffLeftOut {
		t.Fatalf("diff %+v %v", c, err)
	}
}

// A changed symlink is listed, but its target's content is never read into
// the result.
func TestNoneSymlinkNotFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	fixture(t, outside, map[string]string{"private.txt": "outside secret"})
	b, s := open(t, None, dir)
	defer Close(s)
	if err := os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	end, _ := b.Checkpoint(s, "t1")
	c, _ := b.Diff(s, Base, end.Name)
	if c.Count != 1 || c.Files[0].Content != nil || c.Files[0].Diff != nil {
		t.Fatalf("diff %+v", c)
	}
}

// A folder that holds the workdir keeps its record in a temporary folder of
// its own, removed by Close, so the record is never among its files.
func TestNoneRecordOutsideTheFolder(t *testing.T) {
	dir := t.TempDir()
	b, _ := Get(None)
	s, err := b.Open(Request{ID: "t_1", Workdir: filepath.Join(dir, "data", "worktrees"), Folder: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !s.temp || strings.HasPrefix(s.Workdir, dir) {
		t.Fatalf("record under the folder: %s", s.Workdir)
	}
	fixture(t, dir, map[string]string{"a.txt": "a"})
	end, _ := b.Checkpoint(s, "t1")
	if c, _ := b.Diff(s, Base, end.Name); c.Count != 1 {
		t.Fatalf("diff %+v", c)
	}
	wd := s.Workdir
	Close(s)
	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Fatalf("the temporary folder is still there: %v", err)
	}
}
