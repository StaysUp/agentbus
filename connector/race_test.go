package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/workspace"
)

// slowRemote is the control plane's store, which runs hook while the blob on
// is read from it: the person at work in the folder while an apply fetches
// what it will write.
type slowRemote struct {
	*blobs.Store
	on   string
	hook func()
}

func (r *slowRemote) Open(ctx context.Context, h string) (io.ReadCloser, error) {
	if h == r.on && r.hook != nil {
		r.hook()
		r.hook = nil
	}
	return r.Store.Open(ctx, h)
}

func (r *slowRemote) Missing(ctx context.Context, hs []string) ([]string, error) { return nil, nil }

// applyWhile applies cs to folder, with every blob it needs only in the
// control plane's store, and runs person while the blob holding on is
// fetched: after the conflict check, before anything is written.
func applyWhile(t *testing.T, folder string, cs workspace.Changeset, blobsOf []string, on string, person func()) (Applied, error) {
	t.Helper()
	c, ref := newConnector(t, folder)
	remote, err := blobs.Open(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blobsOf {
		if _, err := remote.PutBytes([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	h, err := remote.PutBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	c.Remote = &slowRemote{Store: remote, on: workspace.TreeHash([]byte(on)), hook: person}
	return c.Apply(context.Background(), ref, "", h)
}

func linkOf(target string) *workspace.File {
	return &workspace.File{Size: int64(len(target)), Mode: os.ModeSymlink | 0o777, Hash: workspace.TreeHash(workspace.LinkContent(target))}
}

// What the person writes in the folder while an apply runs is theirs: a file
// the apply would write or remove, changed since the conflict check, refuses
// the apply as the check would have, and nothing is written. A change to a
// file the apply does not touch, one that leaves the file as it was, and one
// that leaves it as the apply would, let it go on.
func TestApplyKeepsWhatThePersonWritesWhileItRuns(t *testing.T) {
	changed := workspace.FileChange{Path: "a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("agent\n", 0o644)}
	for _, tc := range []struct {
		name  string
		files map[string]string
		links map[string]string
		// changes is the turn's changes to the folder as made.
		changes func(folder string) []workspace.FileChange
		blobs   []string
		person  func(folder string)
		// path and why: the conflict the apply refuses with; no path lets it go on.
		path, why string
		want      map[string]string
	}{
		{
			name:    "an edit to a file the turn changed",
			files:   map[string]string{"a.txt": "one\n"},
			changes: func(string) []workspace.FileChange { return []workspace.FileChange{changed} },
			person:  func(f string) { write(t, f, map[string]string{"a.txt": "mine, typed while the apply ran\n"}) },
			path:    "a.txt", why: WhyChanged,
			want: map[string]string{"a.txt": "mine, typed while the apply ran\n"},
		},
		{
			name:  "an edit to a file the turn removes",
			files: map[string]string{"a.txt": "one\n", "r.txt": "old\n"},
			changes: func(string) []workspace.FileChange {
				return []workspace.FileChange{changed, {Path: "r.txt", Base: fileOf("old\n", 0o644)}}
			},
			person: func(f string) { write(t, f, map[string]string{"r.txt": "kept, i still need it\n"}) },
			path:   "r.txt", why: WhyChanged,
			want: map[string]string{"a.txt": "one\n", "r.txt": "kept, i still need it\n"},
		},
		{
			name:  "a file made where the turn adds one",
			files: map[string]string{"a.txt": "one\n"},
			changes: func(string) []workspace.FileChange {
				return []workspace.FileChange{{Path: "n.txt", Now: fileOf("agent\n", 0o644)}}
			},
			person: func(f string) { write(t, f, map[string]string{"n.txt": "my own n\n"}) },
			path:   "n.txt", why: WhyAdded,
			want: map[string]string{"n.txt": "my own n\n"},
		},
		{
			name:    "a chmod of a file the turn changed",
			files:   map[string]string{"a.txt": "one\n"},
			changes: func(string) []workspace.FileChange { return []workspace.FileChange{changed} },
			person:  func(f string) { os.Chmod(filepath.Join(f, "a.txt"), 0o755) },
			path:    "a.txt", why: WhyChanged,
			want: map[string]string{"a.txt": "one\n"},
		},
		{
			name:    "a file the turn changed, deleted",
			files:   map[string]string{"a.txt": "one\n"},
			changes: func(string) []workspace.FileChange { return []workspace.FileChange{changed} },
			person:  func(f string) { os.Remove(filepath.Join(f, "a.txt")) },
			path:    "a.txt", why: WhyRemoved,
			want: map[string]string{"a.txt": "<missing>"},
		},
		{
			name:  "a link the turn changed, pointed elsewhere",
			files: map[string]string{"x": "x\n", "y": "y\n", "zz": "zz\n"},
			links: map[string]string{"l": "x"},
			changes: func(string) []workspace.FileChange {
				return []workspace.FileChange{{Path: "l", Base: linkOf("x"), Now: linkOf("y")}}
			},
			blobs: []string{"link:y"},
			person: func(f string) {
				os.Remove(filepath.Join(f, "l"))
				os.Symlink("zz", filepath.Join(f, "l"))
			},
			path: "l", why: WhyChanged,
			want: map[string]string{"l": "zz\n"},
		},
		{
			name:  "a file the base saw by its stat alone, edited",
			files: map[string]string{"a.txt": "one\n"},
			changes: func(folder string) []workspace.FileChange {
				fi, err := os.Lstat(filepath.Join(folder, "a.txt"))
				if err != nil {
					t.Fatal(err)
				}
				base := &workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}
				return []workspace.FileChange{{Path: "a.txt", Base: base, Now: fileOf("agent\n", 0o644)}}
			},
			person: func(f string) { write(t, f, map[string]string{"a.txt": "mine\n"}) },
			path:   "a.txt", why: WhyChanged,
			want: map[string]string{"a.txt": "mine\n"},
		},
		{
			name:    "an edit to a file the turn did not touch",
			files:   map[string]string{"a.txt": "one\n", "u.txt": "u\n"},
			changes: func(string) []workspace.FileChange { return []workspace.FileChange{changed} },
			person:  func(f string) { write(t, f, map[string]string{"u.txt": "mine\n"}) },
			want:    map[string]string{"a.txt": "agent\n", "u.txt": "mine\n"},
		},
		{
			name:    "the person saves what the turn wrote",
			files:   map[string]string{"a.txt": "one\n"},
			changes: func(string) []workspace.FileChange { return []workspace.FileChange{changed} },
			person:  func(f string) { write(t, f, map[string]string{"a.txt": "agent\n"}) },
			want:    map[string]string{"a.txt": "agent\n"},
		},
		{
			name:    "a touch that leaves the bytes as they were",
			files:   map[string]string{"a.txt": "one\n"},
			changes: func(string) []workspace.FileChange { return []workspace.FileChange{changed} },
			person: func(f string) {
				later := time.Now().Add(time.Hour)
				os.Chtimes(filepath.Join(f, "a.txt"), later, later)
			},
			want: map[string]string{"a.txt": "agent\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := t.TempDir()
			write(t, folder, tc.files)
			for name, target := range tc.links {
				if err := os.Symlink(target, filepath.Join(folder, name)); err != nil {
					t.Skipf("no symbolic links here: %v", err)
				}
			}
			cs := workspace.Changeset{Space: "s", Checkpoint: "t1", Files: tc.changes(folder)}
			blobsOf, on := tc.blobs, "agent\n"
			if blobsOf == nil {
				blobsOf = []string{on}
			} else {
				on = blobsOf[0]
			}
			got, err := applyWhile(t, folder, cs, blobsOf, on, func() { tc.person(folder) })
			check(t, folder, got, err, tc.path, tc.why, tc.want)
		})
	}
}

// A race on one file of three refuses the whole apply: the other two are not
// written either, as with a conflict the check finds.
func TestApplyRacedOnOneFileWritesNone(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})
	cs := workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: fileOf("a\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "b.txt", Base: fileOf("b\n", 0o644), Now: fileOf("B\n", 0o644)},
		{Path: "c.txt", Base: fileOf("c\n", 0o644), Now: fileOf("C\n", 0o644)},
	}}
	got, err := applyWhile(t, folder, cs, []string{"A\n", "B\n", "C\n"}, "B\n", func() {
		write(t, folder, map[string]string{"b.txt": "my b\n"})
	})
	check(t, folder, got, err, "b.txt", WhyChanged, map[string]string{"a.txt": "a\n", "b.txt": "my b\n", "c.txt": "c\n"})
}

// check says the apply refused with one conflict, at path for why, and wrote
// nothing; or, with no path, that it went on. Either way the folder holds want.
func check(t *testing.T, folder string, got Applied, err error, path, why string, want map[string]string) {
	t.Helper()
	if path == "" {
		if err != nil {
			t.Fatalf("apply refused: %v", err)
		}
	} else {
		var cf *Conflict
		if !errors.As(err, &cf) {
			t.Fatalf("apply answered %v and wrote %v, want a conflict at %s: what the person wrote meanwhile is gone", err, got.Files, path)
		}
		if len(cf.Files) != 1 || cf.Files[0] != (ConflictFile{path, why}) {
			t.Errorf("conflicts %+v, want %s %q", cf.Files, path, why)
		}
		if len(got.Files) != 0 {
			t.Errorf("a refused apply wrote %v", got.Files)
		}
	}
	for name, w := range want {
		if g := text(t, filepath.Join(folder, name)); g != w {
			t.Errorf("%s holds %q, want %q", name, g, w)
		}
	}
}
