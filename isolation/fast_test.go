package isolation

import (
	"errors"
	"github.com/staysup/agentbus/connector"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// age sets every file and folder under dir to an hour ago, as a project's
// files are: past the racy window, so the Base does not hash them.
func age(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink == 0 {
			os.Chtimes(p, old, old)
		}
		return nil
	})
}

// fast are the backends that record a Base without hashing: copy, and
// overlay where this test can mount one.
func fast(t *testing.T) []string {
	out := []string{Copy}
	if runtime.GOOS == "linux" && overlayWhy(t.TempDir()) == "" {
		out = append(out, Overlay)
	}
	return out
}

// readSnapshot is every file of the checkpoint h, each with the hash
// learned for it since.
func readSnapshot(s *Space, h header) (*workspace.Snapshot, error) {
	tr := s.trees()
	list := []header{h}
	if err := migrate(s, tr, list); err != nil {
		return nil, err
	}
	snap, err := tr.flatten(list[0].Tree.Root)
	if err != nil {
		return nil, err
	}
	fix := learnedFix(readLearned(s))
	for _, rel := range snap.Paths() {
		f, _ := snap.File(rel)
		snap.Set(rel, fix(rel, f))
	}
	return snap, nil
}

func base(t *testing.T, s *Space) *workspace.Snapshot {
	t.Helper()
	list, err := headers(s)
	if err != nil || len(list) == 0 {
		t.Fatalf("no checkpoints: %v", err)
	}
	snap, err := readSnapshot(s, list[0])
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// The Base hashes no file it can trust by size and time: only links and
// files written just before the copy was made.
func TestBaseHashesOnlyRecentFiles(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"old.txt": "old", "sub/old2.txt": "old2"})
			os.Symlink("old.txt", filepath.Join(src, "link"))
			age(t, src)
			fixture(t, src, map[string]string{"new.txt": "new"})
			_, s := open(t, name, src)
			if s.hasShadow() {
				t.Skip("a git-backed copy keeps no Merkle base to hash on demand")
			}
			snap := base(t, s)
			for rel, hashed := range map[string]bool{"old.txt": false, "sub/old2.txt": false, "link": true, "new.txt": true} {
				f, ok := snap.File(rel)
				if !ok || (f.Hash != "") != hashed {
					t.Errorf("%s: in base %v, hash %q, want hashed %v", rel, ok, f.Hash, hashed)
				}
			}
			Close(s)
		})
	}
}

// A file only touched in the copy is no change: its Base content is hashed
// then, from the person's folder (copy) or its layer (overlay).
func TestTouchedFileIsNoChange(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "a1", "b.txt": "b1"})
			age(t, src)
			b, s := open(t, name, src)
			now := time.Now()
			os.Chtimes(filepath.Join(s.Dir, "a.txt"), now, now)
			c, err := b.Checkpoint(s, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if c.Name != Base {
				t.Fatalf("a touched file made checkpoint %s", c.Name)
			}
			Close(s)
		})
	}
}

// An edited file's diff shows its Base content, learned when it changed;
// the apply writes it without reading the person's unchanged file.
func TestLazyBaseDiffAndApply(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "line1\nline2\n", "big.bin": strings.Repeat("x", MaxFileDiff+10), "gone.txt": "g"})
			age(t, src)
			b, s := open(t, name, src)
			fixture(t, s.Dir, map[string]string{"a.txt": "line1\nline2 changed\n", "big.bin": strings.Repeat("y", MaxFileDiff+20)})
			os.Remove(filepath.Join(s.Dir, "gone.txt"))
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
				if f.Path == "a.txt" && (f.Diff == nil || !strings.Contains(*f.Diff, "-line2\n+line2 changed")) {
					t.Errorf("a.txt has no diff from its Base content: %+v", f)
				}
				if f.Path == "gone.txt" && f.SHA256 == "" {
					t.Errorf("a small removed file has no hash: %+v", f)
				}
			}
			want := map[string]string{"a.txt": "modified", "big.bin": "modified", "gone.txt": "deleted"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("diff %v, want %v", got, want)
			}
			if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
				t.Fatal(err)
			}
			if read(t, filepath.Join(src, "a.txt")) != "line1\nline2 changed\n" || read(t, filepath.Join(src, "big.bin")) != strings.Repeat("y", MaxFileDiff+20) ||
				read(t, filepath.Join(src, "gone.txt")) != "<missing>" {
				t.Fatal("not applied")
			}
			Close(s)
		})
	}
}

// A file the person changed since the copy was made is still a conflict
// though the Base never hashed it: its size or time changed, so it is
// hashed, and it is not as the turn left it. A big file, the same.
func TestConflictWithoutBaseHash(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "a1", "b.txt": "b1", "big.bin": strings.Repeat("x", MaxFileDiff+10)})
			age(t, src)
			b, s := open(t, name, src)
			fixture(t, s.Dir, map[string]string{"a.txt": "a2", "b.txt": "b2", "big.bin": strings.Repeat("x", MaxFileDiff+11)})
			b.Checkpoint(s, "t1")
			// The person, meanwhile: the same size, a new time.
			fixture(t, src, map[string]string{"a.txt": "zz", "big.bin": strings.Repeat("z", MaxFileDiff+10)})
			_, err := applyIn(t, s, src, Base, "t1")
			var c *connector.Conflict
			if !errors.As(err, &c) {
				t.Fatalf("want a conflict, got %v", err)
			}
			got := map[string]string{}
			for _, f := range c.Files {
				got[f.Path] = f.Why
			}
			if want := map[string]string{"a.txt": connector.WhyChanged, "big.bin": connector.WhyChanged}; !reflect.DeepEqual(got, want) {
				t.Fatalf("conflicts %v, want %v", got, want)
			}
			if read(t, filepath.Join(src, "b.txt")) != "b1" {
				t.Fatal("a refused apply wrote a file")
			}
			// The person's edit made as the turn's: no conflict.
			fixture(t, src, map[string]string{"a.txt": "a2", "big.bin": strings.Repeat("x", MaxFileDiff+11)})
			if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
				t.Fatalf("after resolving: %v", err)
			}
			Close(s)
		})
	}
}

// A legacy shared copy a pre-0.6 install left is removed once no space
// references it, and one a remaining space still references is kept.
func TestGCLegacyShared(t *testing.T) {
	work := t.TempDir()
	shared := filepath.Join(work, stateFolder, legacySharedFolder)
	orphan := filepath.Join(shared, "orphan")
	used := filepath.Join(shared, "used")
	for _, d := range []string{orphan, used} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sp := filepath.Join(work, stateFolder, "job_old")
	if err := os.MkdirAll(sp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp, "space.json"),
		[]byte(`{"id":"job_old","backend":"copy","root":"x","dir":"x","shared":["`+used+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gcLegacyShared(work)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("an unreferenced legacy shared copy was kept: %v", err)
	}
	if _, err := os.Stat(used); err != nil {
		t.Fatalf("a referenced legacy shared copy was removed: %v", err)
	}
}

// auto is the platform's fastest backend; overlay falls back to copy where
// it cannot mount, and says why.
func TestAutoChoice(t *testing.T) {
	b, ok := For(Auto, t.TempDir())
	if !ok {
		t.Fatal("auto is not a choice")
	}
	if runtime.GOOS == "darwin" && b.Name() != Copy {
		t.Fatalf("auto on macOS is %s", b.Name())
	}
	if runtime.GOOS == "linux" && b.Name() != Overlay {
		t.Fatalf("auto on Linux tries %s first", b.Name())
	}
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	ob, _ := For(Overlay, src)
	s, err := ob.Open(Request{ID: "job_1", Workdir: filepath.Join(t.TempDir(), "w"), Folder: src})
	if err != nil {
		t.Fatal(err)
	}
	defer Close(s)
	if s.Backend == Copy && !strings.Contains(s.Fallback, "overlay cannot be used here") {
		t.Fatalf("a fallback that does not say why: %+v", s)
	}
	if s.Backend == Overlay && s.Fallback != "" {
		t.Fatalf("an overlay with a fallback note: %+v", s)
	}
	again, err := Load(s.Workdir, s.Dir)
	if err != nil || again.Fallback != s.Fallback {
		t.Fatalf("the note is not kept: %+v %v", again, err)
	}
}

// A file touched in one turn and edited in the next still has a diff from
// its Base content: what was learned at the touch is kept.
func TestTouchedThenEditedHasDiff(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "one\n"})
			age(t, src)
			b, s := open(t, name, src)
			now := time.Now().Add(-time.Minute)
			os.Chtimes(filepath.Join(s.Dir, "a.txt"), now, now)
			if c, _ := b.Checkpoint(s, "t1"); c.Name != Base {
				t.Fatalf("a touch made %s", c.Name)
			}
			fixture(t, s.Dir, map[string]string{"a.txt": "two\n"})
			b.Checkpoint(s, "t2")
			c, err := b.Diff(s, Base, "t2")
			if err != nil || c.Count != 1 || c.Files[0].Diff == nil || !strings.Contains(*c.Files[0].Diff, "-one\n+two") {
				t.Fatalf("diff %+v %v", c, err)
			}
			Close(s)
		})
	}
}

// A Base content learned at a touch survives later checkpoints that record
// nothing: the edit after them, with the person's file changed too, still
// has its diff.
func TestLearnedKeptAcrossUnchangedCheckpoints(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "one\n"})
			age(t, src)
			b, s := open(t, name, src)
			then := time.Now().Add(-time.Minute)
			os.Chtimes(filepath.Join(s.Dir, "a.txt"), then, then)
			b.Checkpoint(s, "t1")
			b.Checkpoint(s, "t2")
			fixture(t, s.Dir, map[string]string{"a.txt": "two\n"})
			fixture(t, src, map[string]string{"a.txt": "theirs\n"})
			b.Checkpoint(s, "t3")
			c, err := b.Diff(s, Base, "t3")
			if err != nil || c.Count != 1 || c.Files[0].Diff == nil || !strings.Contains(*c.Files[0].Diff, "-one\n+two") {
				t.Fatalf("diff %+v %v", c, err)
			}
			Close(s)
		})
	}
}
