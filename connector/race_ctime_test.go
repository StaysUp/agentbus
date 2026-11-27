//go:build linux || darwin

package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/workspace"
)

// must fails the test when err is not nil: a setup step that did not happen
// must not let the test pass.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// touchAndPutBack moves the file's modification time and puts it back: its
// change time moves, and nothing else does.
func touchAndPutBack(t *testing.T, folder string) {
	t.Helper()
	p := filepath.Join(folder, "a.txt")
	fi, err := os.Lstat(p)
	must(t, err)
	must(t, os.Chtimes(p, time.Now(), time.Now()))
	must(t, os.Chtimes(p, fi.ModTime(), fi.ModTime()))
}

// putBack writes content, of the file's own size, into the file p and puts its
// modification time back: what touch -r, cp -p and rsync -t leave. Its change
// time is now, which nothing can put back.
func putBack(t *testing.T, p, content string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(content)) != fi.Size() {
		t.Fatalf("%s holds %d bytes, not the %d of %q", p, fi.Size(), len(content), content)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// agedBy puts the modification time of every file under folder at when from
// now. Setting it moves the change time to now, which nothing can set back, so
// these files are within Racy of the check by change time whatever their
// modification time: a row built on agedBy never reaches the path where Keeps
// trusts a file's stat without reading it. The integration rows, which let the
// files rest for longer than Racy first, do.
func agedBy(t *testing.T, folder string, when time.Duration) {
	t.Helper()
	at := time.Now().Add(when).Truncate(time.Second)
	err := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			err = os.Chtimes(p, at, at)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// statOf is the base's record of the file name of folder by its stat alone.
func statOf(t *testing.T, folder, name string) *workspace.File {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(folder, name))
	if err != nil {
		t.Fatal(err)
	}
	return &workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}
}

// shot is every entry under folder as the system lists it.
func shot(t *testing.T, folder string) map[string]fs.FileInfo {
	t.Helper()
	out := map[string]fs.FileInfo{}
	err := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		out[p] = fi
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// untouched fails the test when something under folder is not the very file,
// with the same size, time and mode, that it was in before: a refused apply
// wrote, replaced or removed nothing, and left nothing of its own. A folder's
// own time moves with the staging folder the apply made and removed.
func untouched(t *testing.T, folder string, before map[string]fs.FileInfo) {
	t.Helper()
	now := shot(t, folder)
	for p, b := range before {
		n, ok := now[p]
		switch {
		case !ok:
			t.Errorf("a refused apply removed %s", p)
		case !os.SameFile(b, n) || b.Size() != n.Size() || b.Mode() != n.Mode() || (!b.IsDir() && !b.ModTime().Equal(n.ModTime())):
			t.Errorf("a refused apply wrote %s", p)
		}
	}
	for p := range now {
		if _, ok := before[p]; !ok {
			t.Errorf("a refused apply left %s", p)
		}
	}
}

// refused fails the test unless err is a *Conflict holding exactly want, and
// the apply wrote nothing.
func refused(t *testing.T, got Applied, err error, want []ConflictFile) {
	t.Helper()
	var cf *Conflict
	if !errors.As(err, &cf) {
		t.Fatalf("apply answered %v and wrote %v, want a conflict %v: what the person wrote meanwhile is gone", err, got.Files, want)
	}
	if len(cf.Files) != len(want) {
		t.Fatalf("conflicts %+v, want %+v", cf.Files, want)
	}
	for i := range want {
		if cf.Files[i] != want[i] {
			t.Errorf("conflicts %+v, want %+v", cf.Files, want)
		}
	}
	if len(got.Files) != 0 {
		t.Errorf("a refused apply wrote %v", got.Files)
	}
}

// A write that leaves the file's size, modification time and mode as they
// were is still a write: the look again goes by the change time too
// (workspace.Keeps), measured from when the conflict check began. A refused
// apply leaves every file in the folder as the person left it. One whose bytes
// are as they were, or as the apply writes, still lets it go on.
func TestApplyKeepsAWriteThatPutsTheTimeBack(t *testing.T) {
	changed := workspace.FileChange{Path: "a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("agent\n", 0o644)}
	one := func(*testing.T, string) []workspace.FileChange { return []workspace.FileChange{changed} }
	statChange := func(t *testing.T, folder string) []workspace.FileChange {
		return []workspace.FileChange{{Path: "a.txt", Base: statOf(t, folder, "a.txt"), Now: fileOf("agent\n", 0o644)}}
	}
	three := func(*testing.T, string) []workspace.FileChange {
		return []workspace.FileChange{
			{Path: "a.txt", Base: fileOf("a1\n", 0o644), Now: fileOf("A\n", 0o644)},
			{Path: "b.txt", Base: fileOf("b1\n", 0o644), Now: fileOf("B\n", 0o644)},
			{Path: "c.txt", Base: fileOf("c1\n", 0o644), Now: fileOf("C\n", 0o644)},
		}
	}
	threeFiles := map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "c.txt": "c1\n"}
	big := strings.Repeat("x", 4<<20)
	bigEdit := big[:len(big)-1] + "y"
	bigOf := func(*testing.T, string) []workspace.FileChange {
		return []workspace.FileChange{{Path: "a.txt", Base: fileOf(big, 0o644), Now: fileOf("agent\n", 0o644)}}
	}
	emptyOf := func(*testing.T, string) []workspace.FileChange {
		return []workspace.FileChange{{Path: "a.txt", Base: fileOf("", 0o644), Now: fileOf("agent\n", 0o644)}}
	}
	conflict := func(path, why string) []ConflictFile { return []ConflictFile{{path, why}} }
	hourBack, hourAhead := -time.Hour, time.Hour
	for _, tc := range []struct {
		name  string
		files map[string]string
		// age is the modification time every file is given before the check
		// (agedBy): an hour back, an hour ahead (a clock that ran fast), or the
		// time it was written. Their change time is always just before the check.
		age     time.Duration
		changes func(t *testing.T, folder string) []workspace.FileChange
		blobs   []string
		person  func(t *testing.T, folder string)
		// conflicts is what the apply refuses with; none lets it go on.
		conflicts []ConflictFile
		want      map[string]string
	}{
		{
			name: "a same-size edit with the time put back", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time put back, to a file the turn removes", files: map[string]string{"a.txt": "one\n", "r.txt": "old\n"}, age: hourBack,
			changes: func(*testing.T, string) []workspace.FileChange {
				return []workspace.FileChange{changed, {Path: "r.txt", Base: fileOf("old\n", 0o644)}}
			},
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "r.txt"), "own\n") },
			conflicts: conflict("r.txt", WhyChanged), want: map[string]string{"a.txt": "one\n", "r.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time put back, to a file the base saw by its stat alone", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: statChange,
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time of another file (touch -r)", files: map[string]string{"a.txt": "one\n", "u.txt": "u\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				ref, err := os.Lstat(filepath.Join(f, "u.txt"))
				if err != nil {
					t.Fatal(err)
				}
				write(t, f, map[string]string{"a.txt": "own\n"})
				if err := os.Chtimes(filepath.Join(f, "a.txt"), ref.ModTime(), ref.ModTime()); err != nil {
					t.Fatal(err)
				}
			},
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a twin moved over the file, with the time it was given (cp -p, mv)", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				fi, err := os.Lstat(filepath.Join(f, "a.txt"))
				if err != nil {
					t.Fatal(err)
				}
				write(t, f, map[string]string{"twin": "own\n"})
				if err := os.Chtimes(filepath.Join(f, "twin"), fi.ModTime(), fi.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(f, "twin"), filepath.Join(f, "a.txt")); err != nil {
					t.Fatal(err)
				}
			},
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a hard link to a twin put where the file was", files: map[string]string{"a.txt": "one\n", "u.txt": "own\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				if err := os.Remove(filepath.Join(f, "a.txt")); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(f, "u.txt"), filepath.Join(f, "a.txt")); err != nil {
					t.Fatal(err)
				}
			},
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time put back, to a file written just before the check", files: map[string]string{"a.txt": "one\n"}, changes: one,
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time put back, to a file whose time is ahead of now", files: map[string]string{"a.txt": "one\n"}, age: hourAhead, changes: one,
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time put back, in a subfolder", files: map[string]string{"d/e/a.txt": "one\n"}, age: hourBack,
			changes: func(*testing.T, string) []workspace.FileChange {
				return []workspace.FileChange{{Path: "d/e/a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("agent\n", 0o644)}}
			},
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "d", "e", "a.txt"), "own\n") },
			conflicts: conflict("d/e/a.txt", WhyChanged), want: map[string]string{"d/e/a.txt": "own\n"},
		},
		{
			name: "a same-size edit with the time put back, to a file only its mode changes", files: map[string]string{"a.txt": "one\n", "u.txt": "u\n"}, age: hourBack,
			changes: func(*testing.T, string) []workspace.FileChange {
				return []workspace.FileChange{
					{Path: "a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("one\n", 0o755)},
					{Path: "u.txt", Base: fileOf("u\n", 0o644), Now: fileOf("agent\n", 0o644)},
				}
			},
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "own\n", "u.txt": "u\n"},
		},
		{
			name: "a same-size edit with the time put back, to a file of 4 MiB", files: map[string]string{"a.txt": big}, age: hourBack, changes: bigOf,
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), bigEdit) },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": bigEdit},
		},
		{
			name: "the first of three raced", files: threeFiles, age: hourBack, changes: three, blobs: []string{"A\n", "B\n", "C\n"},
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "me\n") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "me\n", "b.txt": "b1\n", "c.txt": "c1\n"},
		},
		{
			name: "the middle one of three raced", files: threeFiles, age: hourBack, changes: three, blobs: []string{"A\n", "B\n", "C\n"},
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "b.txt"), "me\n") },
			conflicts: conflict("b.txt", WhyChanged), want: map[string]string{"a.txt": "a1\n", "b.txt": "me\n", "c.txt": "c1\n"},
		},
		{
			name: "the last of three raced", files: threeFiles, age: hourBack, changes: three, blobs: []string{"A\n", "B\n", "C\n"},
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "c.txt"), "me\n") },
			conflicts: conflict("c.txt", WhyChanged), want: map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "c.txt": "me\n"},
		},
		{
			name: "two of three raced", files: threeFiles, age: hourBack, changes: three, blobs: []string{"A\n", "B\n", "C\n"},
			person: func(t *testing.T, f string) {
				putBack(t, filepath.Join(f, "a.txt"), "me\n")
				putBack(t, filepath.Join(f, "c.txt"), "me\n")
			},
			conflicts: []ConflictFile{{"a.txt", WhyChanged}, {"c.txt", WhyChanged}}, want: map[string]string{"a.txt": "me\n", "b.txt": "b1\n", "c.txt": "me\n"},
		},
		{
			name: "a mode changed, the bytes and time as they were", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person:    func(t *testing.T, f string) { must(t, os.Chmod(filepath.Join(f, "a.txt"), 0o755)) },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "one\n"},
		},
		{
			name: "a file made where the turn adds one, with an hour-old time", files: map[string]string{"u.txt": "u\n"}, age: hourBack,
			changes: func(*testing.T, string) []workspace.FileChange {
				return []workspace.FileChange{{Path: "n.txt", Now: fileOf("agent\n", 0o644)}}
			},
			person: func(t *testing.T, f string) {
				write(t, f, map[string]string{"n.txt": "mine\n"})
				at := time.Now().Add(-time.Hour)
				must(t, os.Chtimes(filepath.Join(f, "n.txt"), at, at))
			},
			conflicts: conflict("n.txt", WhyAdded), want: map[string]string{"n.txt": "mine\n"},
		},
		{
			name: "a file the turn changed, deleted", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person:    func(t *testing.T, f string) { must(t, os.Remove(filepath.Join(f, "a.txt"))) },
			conflicts: conflict("a.txt", WhyRemoved), want: map[string]string{"a.txt": "<missing>"},
		},
		{
			name: "a file the turn changed, replaced by a folder", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				must(t, os.Remove(filepath.Join(f, "a.txt")))
				write(t, f, map[string]string{"a.txt/inner": "x\n"})
			},
			conflicts: conflict("a.txt", WhyNotFile), want: map[string]string{"a.txt/inner": "x\n"},
		},
		{
			name: "a file the turn changed, replaced by a link", files: map[string]string{"a.txt": "one\n", "u.txt": "u\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				must(t, os.Remove(filepath.Join(f, "a.txt")))
				if err := os.Symlink("u.txt", filepath.Join(f, "a.txt")); err != nil {
					t.Fatal(err)
				}
			},
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "u\n"},
		},
		{
			name: "a file changed before the check, so before any fetch", files: map[string]string{"a.txt": "mine\n"}, age: hourBack, changes: one,
			person:    func(t *testing.T, _ string) { t.Error("a conflict the check finds fetches nothing") },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "mine\n"},
		},
		{
			name: "an empty file given a byte", files: map[string]string{"a.txt": ""}, age: hourBack, changes: emptyOf,
			person:    func(t *testing.T, f string) { write(t, f, map[string]string{"a.txt": "x"}) },
			conflicts: conflict("a.txt", WhyChanged), want: map[string]string{"a.txt": "x"},
		},
		{
			name: "a time moved and put back, the bytes as they were", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person: touchAndPutBack,
			want:   map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "a mode changed and put back", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				p := filepath.Join(f, "a.txt")
				must(t, os.Chmod(p, 0o755))
				must(t, os.Chmod(p, 0o644))
			},
			want: map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "the same bytes written again, with the time put back", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "one\n") },
			want:   map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "the file made again with the same bytes and time", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) {
				p := filepath.Join(f, "a.txt")
				fi, err := os.Lstat(p)
				if err != nil {
					t.Fatal(err)
				}
				must(t, os.Remove(p))
				write(t, f, map[string]string{"a.txt": "one\n"})
				must(t, os.Chtimes(p, fi.ModTime(), fi.ModTime()))
			},
			want: map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "what the turn writes, written with the time put back", files: map[string]string{"a.txt": "old1\n"}, age: hourBack,
			changes: func(*testing.T, string) []workspace.FileChange {
				return []workspace.FileChange{{Path: "a.txt", Base: fileOf("old1\n", 0o644), Now: fileOf("new2\n", 0o644)}}
			},
			blobs:  []string{"new2\n"},
			person: func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "new2\n") },
			want:   map[string]string{"a.txt": "new2\n"},
		},
		{
			name: "what the turn adds, made by the person with an hour-old time", files: map[string]string{"u.txt": "u\n"}, age: hourBack,
			changes: func(*testing.T, string) []workspace.FileChange {
				return []workspace.FileChange{{Path: "n.txt", Now: fileOf("agent\n", 0o644)}}
			},
			person: func(t *testing.T, f string) {
				write(t, f, map[string]string{"n.txt": "agent\n"})
				at := time.Now().Add(-time.Hour)
				must(t, os.Chtimes(filepath.Join(f, "n.txt"), at, at))
			},
			want: map[string]string{"n.txt": "agent\n"},
		},
		{
			name: "a file with an hour-old time the base saw by its stat alone, nobody wrote", files: map[string]string{"a.txt": "one\n"}, age: hourBack, changes: statChange,
			person: func(*testing.T, string) {}, want: map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "a fresh file the base saw by its stat alone, nobody wrote", files: map[string]string{"a.txt": "one\n"}, changes: statChange,
			person: func(*testing.T, string) {}, want: map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "a file the turn did not touch, written", files: map[string]string{"a.txt": "one\n", "u.txt": "u\n"}, age: hourBack, changes: one,
			person: func(t *testing.T, f string) { putBack(t, filepath.Join(f, "u.txt"), "v\n") },
			want:   map[string]string{"a.txt": "agent\n", "u.txt": "v\n"},
		},
		{
			name: "an empty file, its time moved and put back", files: map[string]string{"a.txt": ""}, age: hourBack, changes: emptyOf,
			person: touchAndPutBack,
			want:   map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "an empty file moved over by an empty twin with its time", files: map[string]string{"a.txt": ""}, age: hourBack, changes: emptyOf,
			person: func(t *testing.T, f string) {
				fi, err := os.Lstat(filepath.Join(f, "a.txt"))
				if err != nil {
					t.Fatal(err)
				}
				write(t, f, map[string]string{"twin": ""})
				must(t, os.Chtimes(filepath.Join(f, "twin"), fi.ModTime(), fi.ModTime()))
				if err := os.Rename(filepath.Join(f, "twin"), filepath.Join(f, "a.txt")); err != nil {
					t.Fatal(err)
				}
			},
			want: map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "a file of 4 MiB nobody wrote", files: map[string]string{"a.txt": big}, age: hourBack, changes: bigOf,
			person: func(*testing.T, string) {}, want: map[string]string{"a.txt": "agent\n"},
		},
		{
			name: "a file whose time is ahead of now, nobody wrote", files: map[string]string{"a.txt": "one\n"}, age: hourAhead, changes: one,
			person: func(*testing.T, string) {}, want: map[string]string{"a.txt": "agent\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := t.TempDir()
			write(t, folder, tc.files)
			if tc.age != 0 {
				agedBy(t, folder, tc.age)
			}
			cs := workspace.Changeset{Space: "s", Checkpoint: "t1", Files: tc.changes(t, folder)}
			blobsOf := tc.blobs
			if blobsOf == nil {
				blobsOf = []string{"agent\n"}
			}
			after := shot(t, folder)
			got, err := applyWhile(t, folder, cs, blobsOf, blobsOf[0], func() {
				tc.person(t, folder)
				after = shot(t, folder)
			})
			if tc.conflicts != nil {
				refused(t, got, err, tc.conflicts)
				untouched(t, folder, after)
			} else if err != nil {
				t.Fatalf("apply refused: %v", err)
			}
			for name, w := range tc.want {
				if g := text(t, filepath.Join(folder, name)); g != w {
					t.Errorf("%s holds %q, want %q", name, g, w)
				}
			}
		})
	}
}

// applyPlacesWhile is applyWhile for a workspace of folders one and two, named
// as such, with the blobs on the control plane's store.
func applyPlacesWhile(t *testing.T, one, two string, cs workspace.Changeset, blobsOf []string, on string, person func()) (Applied, error) {
	t.Helper()
	c, r1 := newConnector(t, one)
	r2, err := c.Folders.Register(two)
	if err != nil {
		t.Fatal(err)
	}
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
	places := []workspace.Place{{Name: "one", Ref: r1}, {Name: "two", Ref: r2}}
	return c.ApplyPlaces(context.Background(), places, "", h, false)
}

// In a workspace of two folders, a write made while the apply fetches refuses
// it as in one folder, and a refusal in the first folder writes nothing in
// either. The folders are checked together only before the first is written,
// so what the person writes in the second after that is refused there, with
// the first already written: ErrPartial, which says so, and the second as the
// person left it.
func TestApplyPlacesKeepsAWriteThatPutsTheTimeBack(t *testing.T) {
	cs := workspace.Changeset{Space: "ws_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "one/a.txt", Base: fileOf("a1\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "two/b.txt", Base: fileOf("b1\n", 0o644), Now: fileOf("B\n", 0o644)},
		{Path: "two/n.txt", Now: fileOf("N\n", 0o644)},
	}}
	for _, tc := range []struct {
		name string
		// on is the blob whose fetch the person writes during.
		on     string
		person func(t *testing.T, one, two string)
		// err: a conflict at this path (refused, nothing written) or ErrPartial.
		conflict string
		partial  bool
		want     map[string]string // by folder name and path
	}{
		{
			name: "in the first folder", on: "A\n",
			person:   func(t *testing.T, one, two string) { putBack(t, filepath.Join(one, "a.txt"), "me\n") },
			conflict: "one/a.txt",
			want:     map[string]string{"one/a.txt": "me\n", "two/b.txt": "b1\n", "two/n.txt": "<missing>"},
		},
		{
			name: "in the second folder, while the second is fetched", on: "B\n",
			person:  func(t *testing.T, one, two string) { putBack(t, filepath.Join(two, "b.txt"), "me\n") },
			partial: true,
			want:    map[string]string{"one/a.txt": "A\n", "two/b.txt": "me\n", "two/n.txt": "<missing>"},
		},
		{
			name: "in the second folder, while the first is fetched", on: "A\n",
			person:  func(t *testing.T, one, two string) { putBack(t, filepath.Join(two, "b.txt"), "me\n") },
			partial: true,
			want:    map[string]string{"one/a.txt": "A\n", "two/b.txt": "me\n", "two/n.txt": "<missing>"},
		},
		{
			name: "nothing changes while it fetches", on: "A\n", person: func(*testing.T, string, string) {},
			want: map[string]string{"one/a.txt": "A\n", "two/b.txt": "B\n", "two/n.txt": "N\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			one, two := t.TempDir(), t.TempDir()
			write(t, one, map[string]string{"a.txt": "a1\n"})
			write(t, two, map[string]string{"b.txt": "b1\n"})
			agedBy(t, one, -time.Hour)
			agedBy(t, two, -time.Hour)
			var afterOne, afterTwo map[string]fs.FileInfo
			got, err := applyPlacesWhile(t, one, two, cs, []string{"A\n", "B\n", "N\n"}, tc.on, func() {
				tc.person(t, one, two)
				afterOne, afterTwo = shot(t, one), shot(t, two)
			})
			switch {
			case tc.conflict != "":
				refused(t, got, err, []ConflictFile{{tc.conflict, WhyChanged}})
				untouched(t, one, afterOne)
				untouched(t, two, afterTwo)
			case tc.partial:
				if !errors.Is(err, ErrPartial) || len(got.Files) != 1 || got.Files[0] != "one/a.txt" {
					t.Errorf("apply answered %v and wrote %v, want ErrPartial with one/a.txt", err, got.Files)
				}
			case err != nil:
				t.Fatalf("apply refused: %v", err)
			}
			for name, w := range tc.want {
				dir, rel, _ := strings.Cut(name, "/")
				folder := one
				if dir == "two" {
					folder = two
				}
				if g := text(t, filepath.Join(folder, rel)); g != w {
					t.Errorf("%s holds %q, want %q", name, g, w)
				}
			}
		})
	}
}
