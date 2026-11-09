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
	if left, _ := os.ReadDir(filepath.Join(s.Workdir, stateFolder, layerDir)); len(left) != 0 {
		t.Fatalf("layers left after the last Close: %d", len(left))
	}
}

// Copies of an unchanged folder share its layers; a change in the folder
// puts a delta layer on them, which shows the folder as it is now; a Close
// keeps the layers another copy uses.
func TestOverlayLayersShared(t *testing.T) {
	overlayOK(t)
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "d/b.txt": "b1", "d/c.txt": "c1", "old/x.txt": "x"})
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
	s1, s2 := openID("job_1"), openID("job_2")
	if !reflect.DeepEqual(s1.Layers, s2.Layers) || len(s1.Layers) != 1 {
		t.Fatalf("layers %v and %v", s1.Layers, s2.Layers)
	}
	// The person changes their folder.
	fixture(t, src, map[string]string{"a.txt": "a2 longer", "new/n.txt": "n"})
	os.Remove(filepath.Join(src, "d", "c.txt"))
	os.RemoveAll(filepath.Join(src, "old"))
	os.Chmod(filepath.Join(src, "d"), 0o700)
	s3 := openID("job_3")
	if len(s3.Layers) != 2 || s3.Layers[0] != s1.Layers[0] {
		t.Fatalf("no delta on the shared base: %v", s3.Layers)
	}
	for p, want := range map[string]string{"a.txt": "a2 longer", "new/n.txt": "n", "d/b.txt": "b1", "d/c.txt": "<missing>", "old/x.txt": "<missing>"} {
		if got := read(t, filepath.Join(s3.Dir, p)); got != want {
			t.Errorf("%s in the new copy: %q, want %q", p, got, want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(s3.Dir, "d")); fi.Mode().Perm() != 0o700 {
		t.Errorf("a folder's mode in the delta: %v", fi.Mode())
	}
	if read(t, filepath.Join(s1.Dir, "a.txt")) != "a1" {
		t.Fatal("an older copy changed")
	}
	// The new copy's Base is the folder now: nothing to apply, no conflict.
	fixture(t, s3.Dir, map[string]string{"d/b.txt": "b2"})
	b.Checkpoint(s3, "t1")
	if c, _ := b.Diff(s3, Base, "t1"); c.Count != 1 {
		t.Fatalf("diff %+v", c)
	}
	if _, err := b.Apply(s3, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	b.Close(s1)
	if _, err := os.Stat(layerPath(work, s2.Layers[0])); err != nil {
		t.Fatal("a layer in use was removed")
	}
	b.Close(s2)
	b.Close(s3)
	if left, _ := os.ReadDir(filepath.Join(work, stateFolder, layerDir)); len(left) != 0 {
		t.Fatalf("layers left: %d", len(left))
	}
}

// Past maxLayers a copy makes a new base, so a mount never stacks too many
// layers; the options stay short.
func TestOverlayNewBaseWhenDeep(t *testing.T) {
	overlayOK(t)
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "0"})
	work := filepath.Join(t.TempDir(), "w")
	b, _ := Get(Overlay)
	var spaces []*Space
	for i := 0; i <= maxLayers; i++ {
		fixture(t, src, map[string]string{"a.txt": strings.Repeat("x", i+1)})
		s, err := b.Open(Request{ID: "job_" + string(rune('a'+i)), Workdir: work, Folder: src})
		if err != nil {
			t.Fatal(err)
		}
		spaces = append(spaces, s)
		if len(s.Layers) > maxLayers {
			t.Fatalf("%d layers", len(s.Layers))
		}
		if got := read(t, filepath.Join(s.Dir, "a.txt")); got != strings.Repeat("x", i+1) {
			t.Fatalf("copy %d reads %q", i, got)
		}
	}
	last := spaces[len(spaces)-1]
	if len(last.Layers) != 1 || last.Layers[0] == spaces[0].Layers[0] {
		t.Fatalf("no new base past %d layers: %v", maxLayers, last.Layers)
	}
	for _, s := range spaces {
		b.Close(s)
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

// The mount options are relative and short: a stack of maxLayers with the
// longest space id is far under the limit.
func TestOverlayOptionsShort(t *testing.T) {
	layers := make([]string, maxLayers)
	for i := range layers {
		layers[i] = layerDir + "/99999"
	}
	id := strings.Repeat("j", 128)
	opts := "lowerdir=" + strings.Join(layers, ":") + ",upperdir=" + id + "/u,workdir=" + id + "/w," + overlayOptions
	if len(opts) > maxOptions/2 {
		t.Fatalf("options %d bytes", len(opts))
	}
}

// A folder of the person's replaced by a file of the same name, and a
// folder whose mode alone changed: the next copy shows both as they are.
func TestOverlayDeltaFolderChanges(t *testing.T) {
	overlayOK(t)
	src := t.TempDir()
	fixture(t, src, map[string]string{"d/a.txt": "a", "d/sub/b.txt": "b", "m/c.txt": "c"})
	age(t, src)
	work := filepath.Join(t.TempDir(), "w")
	b, _ := Get(Overlay)
	s1, err := b.Open(Request{ID: "job_1", Workdir: work, Folder: src})
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(src, "d"))
	fixture(t, src, map[string]string{"d": "now a file"})
	s2, err := b.Open(Request{ID: "job_2", Workdir: work, Folder: src})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s2.Dir, "d")) != "now a file" {
		t.Fatal("the folder replaced by a file does not read as the file")
	}
	os.Chmod(filepath.Join(src, "m"), 0o700)
	s3, err := b.Open(Request{ID: "job_3", Workdir: work, Folder: src})
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(s3.Dir, "m")); fi.Mode().Perm() != 0o700 || len(s3.Layers) != 3 {
		t.Fatalf("a folder's new mode: %v, layers %v", fi.Mode(), s3.Layers)
	}
	for _, s := range []*Space{s1, s2, s3} {
		b.Close(s)
	}
}
