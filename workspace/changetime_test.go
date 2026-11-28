//go:build linux || darwin

package workspace

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// counter is a Hasher that records the paths it was asked to hash; blank
// answers no hash, as a Base recorded by size, time and mode does.
type counter struct {
	mu    sync.Mutex
	paths []string
	blank bool
}

func (c *counter) hash(rel, path string, info fs.FileInfo) (string, error) {
	c.mu.Lock()
	c.paths = append(c.paths, rel)
	c.mu.Unlock()
	if c.blank {
		return "", nil
	}
	return Hash(rel, path, info)
}

var walkers = map[string]func(string, *Snapshot, Hasher) (*Snapshot, error){
	"Walk": Walk,
	"WalkParallel": func(root string, prev *Snapshot, h Hasher) (*Snapshot, error) {
		return WalkParallel(root, prev, h, 3)
	},
}

// ageFiles puts every file under dir at mtime, as a project's files are old.
func ageFiles(t *testing.T, dir string, mtime time.Time) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			err = os.Chtimes(p, mtime, mtime)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A file keeps its earlier hash only while nothing touched it since that
// snapshot: a rename, a write whose time is put back (touch -r, cp -p,
// rsync -t) or a new link change the file's change time, which no program
// can set back, so the file is hashed again even when its size, time and
// mode are the same as before.
func TestWalkHashesAFileTouchedSinceTheLastWalk(t *testing.T) {
	tick := time.Now().Add(-time.Hour).Truncate(time.Second)
	twins := func(t *testing.T, dir string) {
		write(t, filepath.Join(dir, "old.go"), "package one\n")
		write(t, filepath.Join(dir, "new.go"), "package two\n")
		write(t, filepath.Join(dir, "x"), "x")
	}
	rows := []struct {
		name    string
		age     time.Time
		blank   bool
		setup   func(t *testing.T, dir string)
		mutate  func(t *testing.T, dir string)
		rehash  []string
		taken   func(prev *Snapshot) time.Time
		noCalls bool
	}{
		{name: "a file moved over a twin of the same size and time", setup: twins, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, "new.go"), filepath.Join(dir, "old.go")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "an edit of the same size with the time put back", setup: twins, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "old.go")
				write(t, p, "package xyz\n")
				if err := os.Chtimes(p, tick, tick); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a copy that keeps the time moved over the file", setup: twins, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				tmp := filepath.Join(dir, "tmp")
				write(t, tmp, "package two\n")
				if err := os.Chtimes(tmp, tick, tick); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, filepath.Join(dir, "old.go")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a hard link to the twin put where the file was", setup: twins, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "old.go")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(dir, "new.go"), p); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a time put back on a file whose content did not change", setup: twins, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "old.go")
				if err := os.Chtimes(p, time.Now(), time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, tick, tick); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a file recorded without a hash, moved over a twin", setup: twins, blank: true, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, "new.go"), filepath.Join(dir, "old.go")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a mode change alone", setup: twins, rehash: []string{"old.go"},
			mutate: func(t *testing.T, dir string) {
				if err := os.Chmod(filepath.Join(dir, "old.go"), 0o755); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "an empty file moved over an empty twin",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "e1"), "")
				write(t, filepath.Join(dir, "e2"), "")
			},
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, "e2"), filepath.Join(dir, "e1")); err != nil {
					t.Fatal(err)
				}
			}, rehash: []string{"e1"}},
		{name: "a link replaced by a link with a target of the same length",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "x"), "x")
				if err := os.Symlink("aaaa", filepath.Join(dir, "l")); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "l")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("bbbb", filepath.Join(dir, "l")); err != nil {
					t.Fatal(err)
				}
			}, rehash: []string{"l"}},
		{name: "a file replaced by a folder",
			setup: twins,
			mutate: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "old.go")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(p, "inner"), "package two\n")
			}, rehash: []string{"old.go/inner"}},
		{name: "a folder replaced by a file",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "d", "inner"), "package one\n")
				write(t, filepath.Join(dir, "x"), "x")
			},
			mutate: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, "d"), "package one\n")
			}, rehash: []string{"d"}},
		{name: "a file whose time is after the last walk", setup: twins, age: time.Now().Add(time.Hour),
			mutate: func(t *testing.T, dir string) {}, rehash: []string{"old.go", "new.go", "x"}},
		{name: "a file nobody touched since a walk long after its change", setup: twins,
			taken:   func(prev *Snapshot) time.Time { return time.Now().Add(time.Hour) },
			mutate:  func(t *testing.T, dir string) {},
			noCalls: true},
		{name: "a file recorded without a hash that nobody touched", setup: twins, blank: true,
			taken:   func(prev *Snapshot) time.Time { return time.Now().Add(time.Hour) },
			mutate:  func(t *testing.T, dir string) {},
			noCalls: true},
		{name: "a snapshot that does not know when it was taken trusts the times", setup: twins,
			taken:   func(prev *Snapshot) time.Time { return time.Unix(0, 0) },
			mutate:  func(t *testing.T, dir string) {},
			noCalls: true},
	}
	for name, walk := range walkers {
		for _, row := range rows {
			t.Run(name+"/"+row.name, func(t *testing.T) {
				dir := t.TempDir()
				row.setup(t, dir)
				age := tick
				if !row.age.IsZero() {
					age = row.age
				}
				ageFiles(t, dir, age)
				prev, err := walk(dir, nil, (&counter{blank: row.blank}).hash)
				if err != nil {
					t.Fatal(err)
				}
				if row.taken != nil {
					prev.SetTaken(row.taken(prev))
				}
				row.mutate(t, dir)
				c := &counter{}
				snap, err := walk(dir, prev, c.hash)
				if err != nil {
					t.Fatal(err)
				}
				if row.noCalls && len(c.paths) != 0 {
					t.Errorf("hashed %v, want none: nothing was touched", c.paths)
				}
				for _, p := range row.rehash {
					if !slices.Contains(c.paths, p) {
						t.Errorf("%s was not hashed again (hashed %v)", p, c.paths)
					}
				}
				for _, p := range snap.Paths() {
					f, _ := snap.File(p)
					want, _, err := HashOf(filepath.Join(dir, filepath.FromSlash(p)))
					if err != nil {
						t.Fatal(err)
					}
					if f.Hash == "" && row.blank && row.noCalls {
						continue
					}
					if f.Hash != want {
						t.Errorf("%s has hash %q, its content hashes to %q", p, f.Hash, want)
					}
				}
			})
		}
	}
}
