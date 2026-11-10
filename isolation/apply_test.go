package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/folders"
)

// applyIn applies s's changes from the checkpoint from to to into folder,
// as an apply job does: the runner puts both changesets and the files they
// name in a store, and the connector applies from there. from Base is the
// copy's base.
func applyIn(t testing.TB, s *Space, folder, from, to string) (connector.Applied, error) {
	t.Helper()
	dir := t.TempDir()
	st, err := blobs.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string) string {
		cs, err := ChangesetOf(s, name)
		if err != nil {
			t.Fatalf("changeset of %s: %v", name, err)
		}
		for _, f := range cs.Files {
			// As the runner publishes: each file as the turn left it, and
			// as the Base had it, where that content is still to be had.
			if f.Base != nil && f.Base.Hash != "" && !st.Has(f.Base.Hash) {
				if rc, err := BaseBlob(s, f.Path, *f.Base); err == nil {
					if err := st.Put(context.Background(), f.Base.Hash, rc); err != nil {
						t.Fatal(err)
					}
					rc.Close()
				}
			}
			if f.Now == nil || st.Has(f.Now.Hash) {
				continue
			}
			rc, err := Blob(s, f.Now.Hash)
			if err != nil {
				t.Fatalf("no saved file for %s: %v", f.Path, err)
			}
			if err := st.Put(context.Background(), f.Now.Hash, rc); err != nil {
				t.Fatal(err)
			}
			rc.Close()
		}
		b, _ := json.Marshal(cs)
		h, err := st.PutBytes(b)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	fromSet := ""
	if from != Base {
		fromSet = put(from)
	}
	toSet := put(to)
	reg, err := folders.Open(filepath.Join(dir, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := reg.Register(folder)
	if err != nil {
		t.Fatal(err)
	}
	c := &connector.Connector{Folders: reg, Store: st, State: filepath.Join(dir, "connector")}
	return c.Apply(context.Background(), ref, fromSet, toSet)
}

// lenderFor is the folder side of folder, as a runner on the connector's
// machine reaches it: its reference, and the connector that lends from it.
func lenderFor(t testing.TB, folder string) (string, Lender) {
	t.Helper()
	if folder == "" {
		return "", nil
	}
	reg, err := folders.Open(filepath.Join(t.TempDir(), "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := reg.Register(folder)
	if err != nil {
		t.Fatal(err)
	}
	return ref, &connector.Connector{Folders: reg}
}

// A copy built from a snapshot holds the folder's files, its Base has the
// snapshot's id, and a turn in it applies back to the folder through the
// connector: nothing of the runner's reads the folder.
func TestCopyFromSnapshot(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1\n", "sub/b.txt": "b1\n", "sub/deep/c.txt": "c\n"})
	os.Symlink("a.txt", filepath.Join(src, "link"))
	os.Chmod(filepath.Join(src, "sub", "b.txt"), 0o755)
	dir := t.TempDir()
	reg, _ := folders.Open(filepath.Join(dir, "folders.json"))
	ref, _ := reg.Register(src)
	st, _ := blobs.Open(filepath.Join(dir, "blobs"))
	conn := &connector.Connector{Folders: reg, Store: st, State: filepath.Join(dir, "state")}
	taken, err := conn.Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{Copy, Overlay} {
		t.Run(name, func(t *testing.T) {
			if name == Overlay && OverlayWhy(t.TempDir()) != "" {
				t.Skip("no overlay here")
			}
			b, _ := Get(name)
			// The runner's own store, with the control plane's behind it.
			own, _ := blobs.Open(filepath.Join(t.TempDir(), "blobs"))
			local := blobs.Local{Store: own, From: st}
			s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "worktrees"), Ref: ref,
				Snapshot: &Snapshot{ID: taken.ID, Blobs: local, Times: taken.Times}})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(s)
			s.Blobs = local
			cps, _ := b.Checkpoints(s)
			if len(cps) != 1 || cps[0].Root != taken.ID || s.Snapshot != taken.ID {
				t.Fatalf("base %+v, want snapshot %s", cps, taken.ID)
			}
			for p, want := range map[string]string{"a.txt": "a1\n", "sub/b.txt": "b1\n", "sub/deep/c.txt": "c\n"} {
				if got := read(t, filepath.Join(s.Dir, p)); got != want {
					t.Errorf("%s: %q", p, got)
				}
			}
			if target, _ := os.Readlink(filepath.Join(s.Dir, "link")); target != "a.txt" {
				t.Errorf("link %q", target)
			}
			fa, _ := os.Stat(filepath.Join(src, "a.txt"))
			fc, _ := os.Stat(filepath.Join(s.Dir, "a.txt"))
			if fi, _ := os.Stat(filepath.Join(s.Dir, "sub", "b.txt")); fi.Mode().Perm() != 0o755 || !fa.ModTime().Equal(fc.ModTime()) {
				t.Errorf("mode %v, time %v, want the folder's %v", fi.Mode(), fc.ModTime(), fa.ModTime())
			}
			fixture(t, s.Dir, map[string]string{"a.txt": "a2\n"})
			if _, err := b.Checkpoint(s, "t1"); err != nil {
				t.Fatal(err)
			}
			c, err := b.Diff(s, Base, "t1")
			if err != nil || c.Count != 1 || c.Files[0].Diff == nil || !strings.Contains(*c.Files[0].Diff, "-a1") {
				t.Fatalf("diff %+v %v", c, err)
			}
		})
	}
	// The copy's changes apply back: here through the copy backend's space.
	b, _ := Get(Copy)
	s, err := b.Open(Request{ID: "job_2", Workdir: filepath.Join(t.TempDir(), "worktrees"), Ref: ref,
		Snapshot: &Snapshot{ID: taken.ID, Blobs: st}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(s)
	fixture(t, s.Dir, map[string]string{"sub/deep/c.txt": "c2\n", "new.txt": "n\n"})
	b.Checkpoint(s, "t1")
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src, "sub", "deep", "c.txt"))+read(t, filepath.Join(src, "new.txt")) != "c2\nn\n" {
		t.Fatal("the copy's changes are not in the folder")
	}
	// A snapshot id that its blobs do not have refuses the copy.
	if _, err := b.Open(Request{ID: "job_3", Workdir: filepath.Join(t.TempDir(), "worktrees"),
		Snapshot: &Snapshot{ID: strings.Repeat("a", 64), Blobs: st}}); err == nil {
		t.Fatal("a copy of a snapshot that is not there")
	}
}

// A turn that puts a file and a link back as the Base had them, after an
// earlier turn's change was applied, applies: the Base's content of each is
// kept (a link always, a file read through the connector).
func TestApplyPutsTheBaseBack(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "one\n", "b.txt": "b\n"})
	os.Symlink("a.txt", filepath.Join(src, "link"))
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{"a.txt", "b.txt"} {
		os.Chtimes(filepath.Join(src, p), old, old)
	}
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "two\n"})
	os.Remove(filepath.Join(s.Dir, "link"))
	os.Symlink("b.txt", filepath.Join(s.Dir, "link"))
	b.Checkpoint(s, "t1")
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "one\n"})
	os.Remove(filepath.Join(s.Dir, "link"))
	os.Symlink("a.txt", filepath.Join(s.Dir, "link"))
	b.Checkpoint(s, "t2")
	if _, err := applyIn(t, s, src, "t1", "t2"); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(filepath.Join(src, "link")); target != "a.txt" || read(t, filepath.Join(src, "a.txt")) != "one\n" {
		t.Fatalf("after the second apply: link %q, a.txt %q", target, read(t, filepath.Join(src, "a.txt")))
	}
}

// A snapshot of a folder in a git repo carries its .git beside the files:
// the copy built from it is a work tree, and its id is the files' alone.
func TestSnapshotCarriesGit(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a\n"})
	gitIn(t, src, "init", "-q")
	gitIn(t, src, "add", ".")
	gitIn(t, src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "one")
	// Packed refs leave refs/ with no file in it.
	gitIn(t, src, "pack-refs", "--all")
	gitIn(t, src, "gc", "-q")
	plain := t.TempDir()
	fixture(t, plain, map[string]string{"a.txt": "a\n"})
	dir := t.TempDir()
	reg, _ := folders.Open(filepath.Join(dir, "folders.json"))
	st, _ := blobs.Open(filepath.Join(dir, "blobs"))
	conn := &connector.Connector{Folders: reg, Store: st, State: filepath.Join(dir, "state")}
	ref, _ := reg.Register(src)
	pref, _ := reg.Register(plain)
	taken, err := conn.Snapshot(context.Background(), ref)
	if err != nil || taken.Git == "" {
		t.Fatalf("snapshot %+v %v", taken, err)
	}
	if p, err := conn.Snapshot(context.Background(), pref); err != nil || p.ID != taken.ID || p.Git != "" {
		t.Fatalf("the files alone: %+v, want id %s", p, taken.ID)
	}
	b, _ := Get(Copy)
	s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "worktrees"), Ref: ref,
		Snapshot: &Snapshot{ID: taken.ID, Blobs: st, Git: taken.Git}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(s)
	if out, err := exec.Command("git", "-C", s.Dir, "status", "--porcelain").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("git in the copy: %v %s", err, out)
	}
	if c, _ := b.Checkpoints(s); len(c) != 1 || c[0].Files != 1 {
		t.Fatalf("the base holds .git: %+v", c)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// A copy being built from a snapshot stops with its context: a runner that
// stops is not held by a transfer.
func TestCopyFromSnapshotStops(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	dir := t.TempDir()
	reg, _ := folders.Open(filepath.Join(dir, "folders.json"))
	ref, _ := reg.Register(src)
	st, _ := blobs.Open(filepath.Join(dir, "blobs"))
	taken, err := (&connector.Connector{Folders: reg, Store: st}).Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	own, _ := blobs.Open(filepath.Join(t.TempDir(), "blobs"))
	b, _ := Get(Copy)
	work := filepath.Join(t.TempDir(), "worktrees")
	// Fetching from another store, and from a store that holds every blob.
	for i, src := range []BlobSource{blobs.Local{Store: own, From: st}, st} {
		id := fmt.Sprintf("job_%d", i)
		if _, err := b.Open(Request{ID: id, Workdir: work, Ctx: ctx, Snapshot: &Snapshot{ID: taken.ID, Blobs: src}}); err == nil {
			t.Fatalf("%d: a stopped context built the copy", i)
		}
		if _, err := os.Stat(filepath.Join(work, id)); !os.IsNotExist(err) {
			t.Fatalf("%d: a copy was left: %v", i, err)
		}
	}
}

// A new repo, with no commit yet, is still a repo in a copy of its snapshot.
func TestSnapshotOfANewRepo(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a\n"})
	gitIn(t, src, "init", "-q")
	dir := t.TempDir()
	reg, _ := folders.Open(filepath.Join(dir, "folders.json"))
	st, _ := blobs.Open(filepath.Join(dir, "blobs"))
	ref, _ := reg.Register(src)
	taken, err := (&connector.Connector{Folders: reg, Store: st}).Snapshot(context.Background(), ref)
	if err != nil || taken.Git == "" {
		t.Fatalf("%+v %v", taken, err)
	}
	b, _ := Get(Copy)
	s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "worktrees"),
		Snapshot: &Snapshot{ID: taken.ID, Blobs: st, Git: taken.Git}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(s)
	if out, err := exec.Command("git", "-C", s.Dir, "status", "--porcelain").CombinedOutput(); err != nil || !strings.Contains(string(out), "a.txt") {
		t.Fatalf("git in the copy: %v %s", err, out)
	}
}
