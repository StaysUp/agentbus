//go:build integration

package isolation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/workspace"
)

// secondsOnly makes the copy's disk keep file times to the second only, as
// a Mac folder shared into Docker, a network share or FAT do, for t.
func secondsOnly(t *testing.T) {
	t.Helper()
	setTimes = func(p string, a, m time.Time) error {
		return os.Chtimes(p, a.Truncate(time.Second), m.Truncate(time.Second))
	}
	t.Cleanup(func() { setTimes = os.Chtimes })
}

// A copy on a disk that keeps times to the second only holds no file with
// the folder's time, so the time cannot show that the person's file is as
// the Base saw it. Its content still can: a changed file applies, a big one
// whose size changed and a big one removed included, and a file the person changed meanwhile is
// still refused, with nothing written. Before, every changed file the Base
// left unhashed was refused as changed in the folder (#130).
func TestApplyWhereTheCopyKeepsSecondsOnly(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			secondsOnly(t)
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "big.bin": strings.Repeat("x", MaxFileDiff+10),
				"gone.bin": strings.Repeat("g", MaxFileDiff+10)})
			// An hour ago, and never on a whole second.
			old := time.Now().Add(-time.Hour).Truncate(time.Second).Add(123456789)
			for _, p := range []string{"a.txt", "b.txt", "big.bin", "gone.bin"} {
				if err := os.Chtimes(filepath.Join(src, p), old, old); err != nil {
					t.Fatal(err)
				}
			}
			// Past the racy window, so the files' change times are old
			// and only the whole seconds can fail the time check.
			time.Sleep(workspace.Racy + 500*time.Millisecond)
			b, s := open(t, name, src)
			fixture(t, s.Dir, map[string]string{"a.txt": "a2\n", "big.bin": strings.Repeat("y", MaxFileDiff+20)})
			os.Remove(filepath.Join(s.Dir, "gone.bin"))
			if _, err := b.Checkpoint(s, "t1"); err != nil {
				t.Fatal(err)
			}
			if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if read(t, filepath.Join(src, "a.txt")) != "a2\n" || read(t, filepath.Join(src, "big.bin")) != strings.Repeat("y", MaxFileDiff+20) ||
				read(t, filepath.Join(src, "gone.bin")) != "<missing>" {
				t.Fatal("not applied")
			}

			// A real conflict: the person edits b.txt, which the next turn
			// changes too; a.txt the turn changes again.
			_, s2 := open(t, name, src)
			fixture(t, s2.Dir, map[string]string{"a.txt": "a3\n", "b.txt": "b2\n"})
			if _, err := b.Checkpoint(s2, "t1"); err != nil {
				t.Fatal(err)
			}
			fixture(t, src, map[string]string{"b.txt": "mine\n"})
			var c *connector.Conflict
			if _, err := applyIn(t, s2, src, Base, "t1"); !errors.As(err, &c) || len(c.Files) != 1 || c.Files[0].Path != "b.txt" || c.Files[0].Why != connector.WhyChanged {
				t.Fatalf("want b.txt refused as changed, got %v", err)
			}
			if read(t, filepath.Join(src, "a.txt")) != "a2\n" || read(t, filepath.Join(src, "b.txt")) != "mine\n" {
				t.Fatal("a refused apply wrote a file")
			}
		})
	}
}
