package isolation

import (
	"os"
	"path/filepath"
	"testing"
)

// A file name is bytes, and on Linux they need not be UTF-8: "café.txt" in
// Latin-1 is caf\xe9.txt. An apply acts on that name, not on the name JSON
// makes of it (caf�.txt): a new file lands under its own name, an edit
// is no conflict, a delete removes the file, and two such names stay two
// files. Every backend hands its changes over as the same changeset, so
// each one this machine can open is tried.
func TestApplyKeepsANameThatIsNotUTF8(t *testing.T) {
	const name = "caf\xe9.txt"
	// APFS takes no such name; there the case is skipped.
	write := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Skipf("this disk takes no such name: %v", err)
		}
	}
	for _, c := range []struct {
		what string
		seed bool // the person's folder already holds the file
		turn func(t *testing.T, dir string)
		want map[string]string // the folder's files after the apply
	}{
		{"a new file", false,
			func(t *testing.T, dir string) { write(t, dir, name, "new\n") },
			map[string]string{"keep.txt": "k\n", name: "new\n"}},
		{"an edit", true,
			func(t *testing.T, dir string) { write(t, dir, name, "edited\n") },
			map[string]string{"keep.txt": "k\n", name: "edited\n"}},
		{"a delete", true,
			func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, name)) },
			map[string]string{"keep.txt": "k\n"}},
		{"two names", false,
			func(t *testing.T, dir string) {
				write(t, dir, "n\xe9.txt", "first\n")
				write(t, dir, "n\xff.txt", "second\n")
			},
			map[string]string{"keep.txt": "k\n", "n\xe9.txt": "first\n", "n\xff.txt": "second\n"}},
	} {
		for _, backend := range openBackends(t) {
			t.Run(backend+"/"+c.what, func(t *testing.T) {
				src := t.TempDir()
				fixture(t, src, map[string]string{"keep.txt": "k\n"})
				if c.seed {
					write(t, src, name, "old\n")
				}
				gitInit(t, src) // the git backend needs a repository
				age(t, src)
				b, s := open(t, backend, src)
				c.turn(t, s.Dir)
				if _, err := b.Checkpoint(s, "t1"); err != nil {
					t.Fatal(err)
				}
				applied, err := applyIn(t, s, src, Base, "t1")
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				got := map[string]string{}
				entries, _ := os.ReadDir(src)
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					got[e.Name()] = read(t, filepath.Join(src, e.Name()))
				}
				for p, content := range c.want {
					if got[p] != content {
						t.Errorf("%q in the folder is %q, want %q", p, got[p], content)
					}
				}
				for p := range got {
					if _, ok := c.want[p]; !ok {
						t.Errorf("the folder holds %q, which the copy does not (applied %q)", p, applied.Files)
					}
				}
			})
		}
	}
}
