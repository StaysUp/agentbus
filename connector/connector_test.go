package connector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/workspace"
)

func write(t *testing.T, dir string, files map[string]string) {
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

func text(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return "<missing>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newConnector owns folder, with its own store.
func newConnector(t *testing.T, folder string) (*Connector, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := folders.Open(filepath.Join(dir, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := reg.Register(folder)
	if err != nil {
		t.Fatal(err)
	}
	st, err := blobs.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return &Connector{Folders: reg, Store: st, State: filepath.Join(dir, "state")}, ref
}

// Equal files are an equal snapshot id, on any machine and whatever their
// times; one changed byte is another. Every file and tree is in the store.
func TestSnapshotIsContentAddressed(t *testing.T) {
	files := map[string]string{"a.txt": "a", "sub/b.txt": "b", "sub/deep/c.txt": "c"}
	one, two := t.TempDir(), t.TempDir()
	write(t, one, files)
	write(t, two, files)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(two, "a.txt"), old, old)
	if err := os.Symlink("a.txt", filepath.Join(one, "link")); err != nil {
		t.Fatal(err)
	}
	os.Symlink("a.txt", filepath.Join(two, "link"))
	c1, r1 := newConnector(t, one)
	c2, r2 := newConnector(t, two)
	s1, err := c1.Snapshot(context.Background(), r1)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := c2.Snapshot(context.Background(), r2)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != s2.ID || s1.Files != 4 {
		t.Fatalf("snapshots %+v and %+v of equal files", s1, s2)
	}
	// Every tree, down from the top, and every file is a blob.
	var walk func(h string)
	walk = func(h string) {
		b, err := blobs.ReadAll(context.Background(), c1.Store, h, 1<<20)
		if err != nil {
			t.Fatalf("tree %s: %v", h, err)
		}
		es, err := workspace.DecodeTree(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Dir {
				walk(e.Hash)
			} else if !c1.Store.Has(e.Hash) {
				t.Errorf("no blob of %s", e.Name)
			}
		}
	}
	walk(s1.ID)
	write(t, two, map[string]string{"sub/deep/c.txt": "C"})
	if s3, err := c2.Snapshot(context.Background(), r2); err != nil || s3.ID == s1.ID {
		t.Fatalf("a changed file kept the id: %+v %v", s3, err)
	}
}

// changeset puts cs in c's store and answers its blob, with a blob of each
// file it names now.
func putChangeset(t *testing.T, c *Connector, cs workspace.Changeset, contents map[string]string) string {
	t.Helper()
	for _, b := range contents {
		if _, err := c.Store.PutBytes([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(cs)
	h, err := c.Store.PutBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func fileOf(content string, mode os.FileMode) *workspace.File {
	return &workspace.File{Size: int64(len(content)), Mode: mode, Hash: workspace.TreeHash([]byte(content))}
}

// Apply goes from one changeset to another: up to turn 2 from the base
// applies both turns at once; from turn 1 to turn 2 applies turn 2 alone.
func TestApplyUpToATurn(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "one\n", "b.txt": "b\n"})
	c, ref := newConnector(t, folder)
	base := map[string]*workspace.File{"a.txt": fileOf("one\n", 0o644), "b.txt": fileOf("b\n", 0o644)}
	// Turn 1 changed a.txt and added n.txt; turn 2 changed a.txt again,
	// removed n.txt and b.txt.
	t1 := putChangeset(t, c, workspace.Changeset{Space: "job_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: base["a.txt"], Now: fileOf("one\np1\n", 0o644)},
		{Path: "n.txt", Now: fileOf("n\n", 0o644)},
	}}, map[string]string{"a1": "one\np1\n", "n": "n\n"})
	t2 := putChangeset(t, c, workspace.Changeset{Space: "job_1", Checkpoint: "t2", Files: []workspace.FileChange{
		{Path: "a.txt", Base: base["a.txt"], Now: fileOf("one\np1\np2\n", 0o644)},
		{Path: "b.txt", Base: base["b.txt"]},
	}}, map[string]string{"a2": "one\np1\np2\n"})
	got, err := c.Apply(context.Background(), ref, "", t2)
	if err != nil || len(got.Files) != 2 {
		t.Fatalf("apply up to turn 2: %+v %v", got, err)
	}
	if text(t, filepath.Join(folder, "a.txt"))+text(t, filepath.Join(folder, "b.txt"))+text(t, filepath.Join(folder, "n.txt")) !=
		"one\np1\np2\n<missing><missing>" {
		t.Fatal("the folder is not as turn 2 left the copy")
	}
	// From turn 1: a folder as turn 1 left it gets turn 2's changes.
	folder2 := t.TempDir()
	write(t, folder2, map[string]string{"a.txt": "one\np1\n", "b.txt": "b\n", "n.txt": "n\n"})
	c2, ref2 := newConnector(t, folder2)
	c2.Store = c.Store
	got, err = c2.Apply(context.Background(), ref2, t1, t2)
	if err != nil || len(got.Files) != 3 {
		t.Fatalf("apply from turn 1 to turn 2: %+v %v", got, err)
	}
	if text(t, filepath.Join(folder2, "a.txt"))+text(t, filepath.Join(folder2, "n.txt")) != "one\np1\np2\n<missing>" {
		t.Fatal("turn 2 is not applied over turn 1")
	}
}

// All or nothing: one file changed in the folder refuses every file, with
// why; and a file whose content the store lacks writes nothing either.
func TestApplyAllOrNothing(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "one\n", "b.txt": "b\n"})
	c, ref := newConnector(t, folder)
	cs := workspace.Changeset{Space: "job_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "b.txt", Base: fileOf("b\n", 0o644), Now: fileOf("B\n", 0o644)},
		{Path: "new/n.txt", Now: fileOf("n\n", 0o644)},
	}}
	h := putChangeset(t, c, cs, map[string]string{"a": "A\n", "b": "B\n", "n": "n\n"})
	write(t, folder, map[string]string{"b.txt": "mine\n"})
	_, err := c.Apply(context.Background(), ref, "", h)
	var cf *Conflict
	if !errors.As(err, &cf) || len(cf.Files) != 1 || cf.Files[0].Path != "b.txt" || cf.Files[0].Why != WhyChanged {
		t.Fatalf("apply over a change: %v", err)
	}
	if text(t, filepath.Join(folder, "a.txt")) != "one\n" || text(t, filepath.Join(folder, "new", "n.txt")) != "<missing>" {
		t.Fatal("a refused apply wrote")
	}
	// The person's change undone; the store loses n.txt's content.
	write(t, folder, map[string]string{"b.txt": "b\n"})
	os.Remove(c.Store.Path(workspace.TreeHash([]byte("n\n"))))
	if _, err := c.Apply(context.Background(), ref, "", h); !errors.Is(err, ErrChangeset) {
		t.Fatalf("apply with a file missing from the store: %v", err)
	}
	if text(t, filepath.Join(folder, "a.txt")) != "one\n" || text(t, filepath.Join(folder, "b.txt")) != "b\n" {
		t.Fatal("an apply that could not stage every file wrote")
	}
	entries, _ := os.ReadDir(folder)
	for _, e := range entries {
		if e.Name() != "a.txt" && e.Name() != "b.txt" {
			t.Errorf("left in the folder: %s", e.Name())
		}
	}
}

// Lend reads a file only while it is as the copy's base recorded it.
func TestLend(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "one\n"})
	c, ref := newConnector(t, folder)
	fi, _ := os.Lstat(filepath.Join(folder, "a.txt"))
	f := workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}
	r, same, ok := c.Lend(ref, "a.txt", f)
	if !ok {
		t.Fatal("not lent")
	}
	r.Close()
	if !same() {
		t.Fatal("not the same")
	}
	write(t, folder, map[string]string{"a.txt": "two!\n"})
	if _, _, ok := c.Lend(ref, "a.txt", f); ok {
		t.Fatal("lent a changed file")
	}
	if _, _, ok := c.Lend("fld_0000000000000000", "a.txt", f); ok {
		t.Fatal("lent from a folder it does not own")
	}
	if _, _, ok := c.Lend(ref, "../a.txt", f); ok {
		t.Fatal("lent from outside the folder")
	}
}

// A regular file holding a link's bytes is not that link: the apply makes
// the link.
func TestApplyLinkOverItsBytes(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"l": "link:a.txt"})
	c, ref := newConnector(t, folder)
	h := workspace.TreeHash(workspace.LinkContent("a.txt"))
	link := &workspace.File{Hash: h, Mode: os.ModeSymlink | 0o777, Size: 5}
	cs := putChangeset(t, c, workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "l", Base: &workspace.File{Hash: h, Mode: 0o644, Size: 10}, Now: link}}}, map[string]string{"l": "link:a.txt"})
	if _, err := c.Apply(context.Background(), ref, "", cs); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(folder, "l")); err != nil || target != "a.txt" {
		t.Fatalf("l is not the link: %q %v", target, err)
	}
}

// A stopped job stops: a snapshot takes nothing, an apply writes nothing.
func TestStoppedJobsStop(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "one\n"})
	c, ref := newConnector(t, folder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Snapshot(ctx, ref); err == nil {
		t.Fatal("a stopped snapshot answered")
	}
	cs := putChangeset(t, c, workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("two\n", 0o644)}}}, map[string]string{"a": "two\n"})
	if _, err := c.Apply(ctx, ref, "", cs); err == nil || text(t, filepath.Join(folder, "a.txt")) != "one\n" {
		t.Fatalf("a stopped apply: %v, a.txt %q", err, text(t, filepath.Join(folder, "a.txt")))
	}
}
