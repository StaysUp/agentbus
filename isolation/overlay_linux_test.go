package isolation

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// overlayOK skips a test where this process cannot mount an overlay.
func overlayOK(t *testing.T) {
	t.Helper()
	if why := overlayWhy(t.TempDir()); why != "" {
		t.Skip(why)
	}
}

// The overlay's own round trip: every kind of change a turn makes shows in
// the diff and applies, a removed folder and a folder made again over a
// removed one included.
func TestOverlayRoundTrip(t *testing.T) {
	overlayOK(t)
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "sub/b.txt": "b1", "dir/x.txt": "x", "dir/y.txt": "y",
		"redo/old.txt": "o", "keep.txt": "k"})
	os.Symlink("a.txt", filepath.Join(src, "link"))
	age(t, src)
	b, s := open(t, Overlay, src)
	if s.Backend != Overlay || s.Method != MethodOverlay || !mounted(s.Root) {
		t.Fatalf("not an overlay: %+v", s)
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "a2", "new/c.txt": "c1"})
	os.Remove(filepath.Join(s.Dir, "sub", "b.txt"))
	os.RemoveAll(filepath.Join(s.Dir, "dir"))
	os.RemoveAll(filepath.Join(s.Dir, "redo"))
	fixture(t, s.Dir, map[string]string{"redo/fresh.txt": "f"})
	os.Remove(filepath.Join(s.Dir, "link"))
	os.Symlink("new/c.txt", filepath.Join(s.Dir, "link"))
	os.Chmod(filepath.Join(s.Dir, "keep.txt"), 0o755)
	if read(t, filepath.Join(src, "a.txt")) != "a1" {
		t.Fatal("an edit of the copy reached the folder")
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	c, err := b.Diff(s, Base, "t1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range c.Files {
		got[f.Path] = f.Change
	}
	want := map[string]string{"a.txt": "modified", "new/c.txt": "added", "sub/b.txt": "deleted", "dir/x.txt": "deleted",
		"dir/y.txt": "deleted", "redo/old.txt": "deleted", "redo/fresh.txt": "added", "link": "modified", "keep.txt": "modified"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff %v, want %v", got, want)
	}
	if _, err := b.Apply(s, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src, "a.txt")) != "a2" || read(t, filepath.Join(src, "redo", "fresh.txt")) != "f" ||
		read(t, filepath.Join(src, "dir", "x.txt")) != "<missing>" || read(t, filepath.Join(src, "redo", "old.txt")) != "<missing>" {
		t.Fatal("the folder does not hold the turn's changes")
	}
	if err := b.Close(s); err != nil {
		t.Fatal(err)
	}
	if mounted(s.Root) {
		t.Fatal("still mounted after Close")
	}
	if _, err := os.Stat(s.state()); !os.IsNotExist(err) {
		t.Fatalf("the space's record is left after Close: %v", err)
	}
}

// Each session owns its snapshot: two sessions of one folder share no file
// of their record, a change in the folder reaches only a session made after
// it, and closing one leaves the other whole.
func TestOverlaySnapshotOwned(t *testing.T) {
	overlayOK(t)
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "d/b.txt": "b1", "old/x.txt": "x"})
	age(t, src)
	work := filepath.Join(t.TempDir(), "w")
	b, _ := Get(Overlay)
	openID := func(id string) *Space {
		s, err := b.Open(Request{ID: id, Workdir: work, Folder: src})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1 := openID("job_1")
	// The person changes their folder: only a later session sees it.
	fixture(t, src, map[string]string{"a.txt": "a2 longer", "new/n.txt": "n"})
	os.RemoveAll(filepath.Join(src, "old"))
	s2 := openID("job_2")
	for p, want := range map[string]string{"a.txt": "a1", "new/n.txt": "<missing>", "old/x.txt": "x"} {
		if got := read(t, filepath.Join(s1.Dir, p)); got != want {
			t.Errorf("%s in the first copy: %q, want %q", p, got, want)
		}
	}
	for p, want := range map[string]string{"a.txt": "a2 longer", "new/n.txt": "n", "old/x.txt": "<missing>"} {
		if got := read(t, filepath.Join(s2.Dir, p)); got != want {
			t.Errorf("%s in the second copy: %q, want %q", p, got, want)
		}
	}
	if s1.lowerDir() == s2.lowerDir() || !strings.HasPrefix(s1.lowerDir(), s1.state()+string(filepath.Separator)) {
		t.Fatalf("snapshots %s and %s", s1.lowerDir(), s2.lowerDir())
	}
	// The new copy's Base is the folder now: nothing to apply, no conflict.
	fixture(t, s2.Dir, map[string]string{"d/b.txt": "b2"})
	b.Checkpoint(s2, "t1")
	if c, _ := b.Diff(s2, Base, "t1"); c.Count != 1 {
		t.Fatalf("diff %+v", c)
	}
	if _, err := b.Apply(s2, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(s1); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s2.Dir, "a.txt")) != "a2 longer" {
		t.Fatal("closing one session changed another")
	}
	b.Close(s2)
	if left, _ := os.ReadDir(filepath.Join(work, stateFolder)); len(left) != 0 {
		t.Fatalf("left in the store after every Close: %d", len(left))
	}
}

// A store a build before session-owned snapshots left: its shared layers go
// once no space record names them.
func TestOverlayOldLayersRemoved(t *testing.T) {
	work := t.TempDir()
	old := filepath.Join(work, stateFolder, oldLayers, "1")
	os.MkdirAll(old, 0o700)
	os.MkdirAll(filepath.Join(work, stateFolder, "job_old"), 0o700)
	os.WriteFile(filepath.Join(work, stateFolder, "job_old", "space.json"), []byte(`{"id":"job_old","layers":[1]}`), 0o600)
	if err := removeOldLayers(work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("layers a space still names were removed")
	}
	os.RemoveAll(filepath.Join(work, stateFolder, "job_old"))
	if err := removeOldLayers(work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, stateFolder, oldLayers)); !os.IsNotExist(err) {
		t.Fatal("old layers left")
	}
}

// After a restart the mount is gone; the next checkpoint mounts it again
// over the same upper layer, and the turn's edits are there.
func TestOverlayRemount(t *testing.T) {
	overlayOK(t)
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Overlay, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if err := unmountAll(s.Root); err != nil {
		t.Fatal(err)
	}
	again, err := Load(s.Workdir, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(again, "t1"); err != nil {
		t.Fatal(err)
	}
	if !mounted(s.Root) || read(t, filepath.Join(s.Dir, "a.txt")) != "a2" {
		t.Fatal("not mounted again with the turn's edit")
	}
	b.Close(again)
}
