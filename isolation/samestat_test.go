package isolation

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/folders"
)

// treeOf is every file under dir, by slash path: its kind, its mode and its
// content (a link's target), leaving out the copy's own state.
func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[filepath.ToSlash(rel)] = "link " + target
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = fi.Mode().Perm().String() + " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// openFromSnapshot opens a copy of folder the way a runner on another
// machine does: built from the folder's snapshot, with no private git repo,
// so its checkpoints are taken by walking it.
func openFromSnapshot(t *testing.T, folder string) (Backend, *Space) {
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
	conn := &connector.Connector{Folders: reg, Store: st, State: filepath.Join(dir, "state")}
	taken, err := conn.Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	own, err := blobs.Open(filepath.Join(dir, "own"))
	if err != nil {
		t.Fatal(err)
	}
	local := blobs.Local{Store: own, From: st}
	b, _ := Get(Copy)
	s, err := b.Open(Request{ID: "job_1", Workdir: filepath.Join(dir, "worktrees"), Ref: ref,
		Snapshot: &Snapshot{ID: taken.ID, Blobs: local, Times: taken.Times}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.Blobs = local
	t.Cleanup(func() { b.Close(s) })
	return b, s
}

// Two files of one size and one time are common (a checkout writes many in a
// tick, and cp -p, rsync -a and install -p keep times), and a program that
// moves one over the other, or writes a file and puts its time back, leaves
// the file's size, time and mode as they were. The checkpoint must still see
// it: what the person's folder holds after the apply is what the copy holds.
func TestCheckpointSeesAFileWhoseSizeAndTimeDidNotChange(t *testing.T) {
	tick := time.Now().Add(-time.Hour).Truncate(time.Second)
	setTime := func(t *testing.T, p string) {
		t.Helper()
		if err := os.Chtimes(p, tick, tick); err != nil {
			t.Fatal(err)
		}
	}
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ops := []struct {
		name string
		// inPlace: the file keeps its inode, so git, which the copy backend
		// asks, tells it changed by a change time it compares to the second
		// only.
		inPlace bool
		run     func(t *testing.T, dir string)
	}{
		{"a file moved over a twin", false, func(t *testing.T, dir string) {
			must(t, os.Rename(filepath.Join(dir, "new.go"), filepath.Join(dir, "old.go")))
		}},
		{"an edit of the same size with the time put back", true, func(t *testing.T, dir string) {
			p := filepath.Join(dir, "old.go")
			must(t, os.WriteFile(p, []byte("package xyz\n"), 0o644))
			setTime(t, p)
		}},
		{"a copy that keeps the time moved over the file", false, func(t *testing.T, dir string) {
			tmp := filepath.Join(dir, "tmp")
			must(t, os.WriteFile(tmp, []byte("package two\n"), 0o644))
			setTime(t, tmp)
			must(t, os.Rename(tmp, filepath.Join(dir, "old.go")))
		}},
		{"a hard link to the twin put where the file was", false, func(t *testing.T, dir string) {
			p := filepath.Join(dir, "old.go")
			must(t, os.Remove(p))
			must(t, os.Link(filepath.Join(dir, "new.go"), p))
		}},
		{"an empty file moved over an empty twin", false, func(t *testing.T, dir string) {
			must(t, os.Rename(filepath.Join(dir, "e2"), filepath.Join(dir, "e1")))
		}},
		{"a mode change alone", false, func(t *testing.T, dir string) {
			must(t, os.Chmod(filepath.Join(dir, "old.go"), 0o755))
		}},
		{"a link replaced by a link with a target of the same length", false, func(t *testing.T, dir string) {
			must(t, os.Remove(filepath.Join(dir, "l")))
			must(t, os.Symlink("bbbb", filepath.Join(dir, "l")))
		}},
	}
	copies := []struct {
		name string
		open func(t *testing.T, folder string) (Backend, *Space)
	}{
		{Copy, func(t *testing.T, folder string) (Backend, *Space) { return open(t, Copy, folder) }},
		{"copy from a snapshot", openFromSnapshot},
		{Overlay, func(t *testing.T, folder string) (Backend, *Space) {
			if why := overlayWhy(t.TempDir()); why != "" {
				t.Skip(why)
			}
			return open(t, Overlay, folder)
		}},
	}
	for _, c := range copies {
		for _, turn := range []int{1, 2} {
			for _, op := range ops {
				t.Run(c.name+"/turn "+string(rune('0'+turn))+"/"+op.name, func(t *testing.T) {
					src := t.TempDir()
					fixture(t, src, map[string]string{"old.go": "package one\n", "new.go": "package two\n", "x": "x", "e1": "", "e2": ""})
					must(t, os.Symlink("aaaa", filepath.Join(src, "l")))
					for _, p := range []string{"old.go", "new.go", "x", "e1", "e2"} {
						setTime(t, filepath.Join(src, p))
					}
					b, s := c.open(t, src)
					from := Base
					if turn == 2 {
						fixture(t, s.Dir, map[string]string{"x": "y"})
						first, err := b.Checkpoint(s, "t1")
						if err != nil {
							t.Fatal(err)
						}
						if _, err := applyIn(t, s, src, Base, first.Name); err != nil {
							t.Fatal(err)
						}
						from = first.Name
					}
					op.run(t, s.Dir)
					cp, err := b.Checkpoint(s, "t2")
					if err != nil {
						t.Fatal(err)
					}
					if cp.Name == from {
						t.Errorf("the checkpoint saw no change after: %s", op.name)
						return
					}
					if _, err := applyIn(t, s, src, from, cp.Name); err != nil {
						t.Fatalf("apply: %v", err)
					}
					got, want := treeOf(t, src), treeOf(t, s.Dir)
					if !reflect.DeepEqual(got, want) {
						var diff []string
						for p := range want {
							if got[p] != want[p] {
								diff = append(diff, p)
							}
						}
						for p := range got {
							if _, ok := want[p]; !ok {
								diff = append(diff, p)
							}
						}
						sort.Strings(diff)
						t.Errorf("the folder differs from the copy at %v:\nfolder %q\ncopy   %q", diff, got, want)
					}
				})
			}
		}
	}
}
