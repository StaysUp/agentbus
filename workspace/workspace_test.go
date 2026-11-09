package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byPath(c Changes) map[string]Change {
	m := map[string]Change{}
	for _, f := range c.Files {
		m[f.Path] = f
	}
	return m
}

// A plain folder: added, modified and deleted files, with size and hash; an
// unchanged file is not listed; a file touched with the same bytes is not a
// change.
func TestSnapshotPlainFolder(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep.txt"), "same")
	write(t, filepath.Join(dir, "edit.txt"), "old")
	write(t, filepath.Join(dir, "gone.txt"), "bye")
	write(t, filepath.Join(dir, "touch.txt"), "x")
	before, err := Walk(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "edit.txt"), "new!")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, filepath.Join(dir, "sub", "new.txt"), "hello")
	write(t, filepath.Join(dir, "touch.txt"), "x")
	after, err := Walk(dir, before, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := Diff(before, after, nil)
	m := byPath(c)
	if c.Kind != "snapshot" || c.Count != 3 || len(m) != 3 {
		t.Fatalf("%+v", c)
	}
	if e := m["edit.txt"]; e.Change != "modified" || e.Size != 4 || e.SHA256 != hashBytes([]byte("new!")) || e.Content != nil {
		t.Errorf("edit: %+v", e)
	}
	if e := m["gone.txt"]; e.Change != "deleted" || e.SHA256 != hashBytes([]byte("bye")) {
		t.Errorf("gone: %+v", e)
	}
	if e := m["sub/new.txt"]; e.Change != "added" || e.Size != 5 {
		t.Errorf("new: %+v", e)
	}
}

// A job that writes many small files still gets a result that can be sent:
// the contents share one budget, and Fit shrinks the rest to a bound.
func TestManyFilesFit(t *testing.T) {
	root := t.TempDir()
	before, err := Walk(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		write(t, filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), strings.Repeat("a", 8<<10))
	}
	after, err := Walk(root, before, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := Diff(before, after, func(rel string, max int) ([]byte, bool) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		return b, err == nil && len(b) <= max
	})
	total := 0
	for _, f := range c.Files {
		if f.Content != nil {
			total += len(*f.Content)
		}
	}
	if c.Count != 400 || total > MaxContents || total == 0 || !c.ContentLeftOut {
		t.Fatalf("count %d, content %d bytes, left out %v", c.Count, total, c.ContentLeftOut)
	}
	c.Fit(16 << 10)
	b, _ := json.Marshal(c)
	if len(b) > 16<<10 || !c.Truncated || c.Count != 400 || len(c.Files) == 0 {
		t.Fatalf("fit: %d bytes, truncated %v, %d of %d files", len(b), c.Truncated, len(c.Files), c.Count)
	}
	for _, f := range c.Files {
		if f.Content != nil {
			t.Fatalf("%s kept its content", f.Path)
		}
	}
}
