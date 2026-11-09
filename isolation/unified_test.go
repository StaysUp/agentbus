package isolation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An apply is recorded before its prune, so the same apply again, as after
// a lost answer, answers as it did and writes nothing.
func TestApplyAgainAfterPrune(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "one\n"})
	b, s := open(t, Copy, src)
	defer b.Close(s)
	fixture(t, s.Dir, map[string]string{"a.txt": "two\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	first, err := b.Apply(s, Base, "t1")
	if err != nil || !first.Pruned || len(first.Files) != 1 {
		t.Fatalf("apply: %+v %v", first, err)
	}
	fixture(t, src, map[string]string{"a.txt": "theirs\n"})
	again, err := b.Apply(s, Base, "t1")
	if err != nil || !again.Pruned || len(again.Files) != 1 || again.Files[0] != "a.txt" {
		t.Fatalf("apply again: %+v %v", again, err)
	}
	if read(t, filepath.Join(src, "a.txt")) != "theirs\n" {
		t.Fatal("the second apply wrote again")
	}
}

// Two sessions change the same file: one copied from a folder, one from a
// folder inside it. Applied at once, one writes and the other is refused:
// neither writes over the other.
func TestConcurrentAppliesToOneFolder(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"sub/z.txt": "start\n"})
	work := filepath.Join(t.TempDir(), "worktrees")
	b, _ := Get(Copy)
	var spaces []*Space
	for i, c := range []struct{ id, folder, file string }{
		{"job_a", src, "sub/z.txt"}, {"job_b", filepath.Join(src, "sub"), "z.txt"}} {
		s, err := b.Open(Request{ID: c.id, Workdir: work, Folder: c.folder})
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close(s)
		fixture(t, s.Dir, map[string]string{c.file: fmt.Sprintf("from %d\n", i)})
		if _, err := b.Checkpoint(s, "t1"); err != nil {
			t.Fatal(err)
		}
		spaces = append(spaces, s)
	}
	errs := make(chan error, 2)
	for _, s := range spaces {
		go func() { _, err := b.Apply(s, Base, "t1"); errs <- err }()
	}
	ok, refused := 0, 0
	for range 2 {
		var cf *Conflict
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.As(err, &cf):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("%d applied, %d refused; want one of each", ok, refused)
	}
}

func TestUnified(t *testing.T) {
	for _, c := range []struct {
		name, a, b, change, want string
	}{
		{"one line in the middle", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nfive\n6\n7\n8\n9\n", "modified",
			"--- a/f\n+++ b/f\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n"},
		{"added", "", "x\ny\n", "added", "--- /dev/null\n+++ b/f\n@@ -0,0 +1,2 @@\n+x\n+y\n"},
		{"deleted", "x\n", "", "deleted", "--- a/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"},
		{"no newline at the end", "a\nb", "a\nc", "modified",
			"--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"},
		{"two hunks", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n", "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\ntwelve\n", "modified",
			"--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n-1\n+one\n 2\n 3\n 4\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+twelve\n"},
		{"a line added", "a\nb\n", "a\nnew\nb\n", "modified", "--- a/f\n+++ b/f\n@@ -1,2 +1,3 @@\n a\n+new\n b\n"},
	} {
		if got := unified("f", []byte(c.a), []byte(c.b), c.change); got != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
}

// A middle too big for the matcher is still a whole diff: all removed, then
// all added.
func TestUnifiedPastTheTable(t *testing.T) {
	var a, b strings.Builder
	for i := range 2000 {
		a.WriteString("a" + strings.Repeat("x", i%7) + "\n")
		b.WriteString("b" + strings.Repeat("y", i%5) + "\n")
	}
	d := unified("f", []byte(a.String()), []byte(b.String()), "modified")
	// "\n+" also counts the +++ header line.
	if strings.Count(d, "\n-") != 2000 || strings.Count(d, "\n+") != 2001 || !strings.Contains(d, "@@ -1,2000 +1,2000 @@") {
		t.Fatalf("diff past the table: %d removed, %d added", strings.Count(d, "\n-"), strings.Count(d, "\n+"))
	}
}

// A turn's diff gives each changed text file its own diff, the first turn's
// from the person's folder (base saves nothing), and marks a binary file.
func TestDiffHasFileDiffs(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n", "bin": "x\x00y"})
	b, s := open(t, Copy, src)
	defer b.Close(s)
	fixture(t, s.Dir, map[string]string{"a.txt": "one\n2\n", "new.txt": "hi\n", "bin": "x\x00z"})
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
		switch {
		case f.Diff != nil:
			got[f.Path] = *f.Diff
		case f.DiffLeftOut:
			got[f.Path] = "left out"
		}
	}
	want := map[string]string{
		"a.txt":    "--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+2\n",
		"new.txt":  "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+hi\n",
		"gone.txt": "--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
		"bin":      "left out",
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: %q, want %q", p, got[p], w)
		}
	}

	// The person's file changed since: the first turn's earlier content is
	// no longer kept, so a later diff of it is left out, never wrong.
	fixture(t, src, map[string]string{"a.txt": "theirs\n"})
	c, err = b.Diff(s, Base, "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range c.Files {
		if f.Path == "a.txt" && (f.Diff != nil || !f.DiffLeftOut) {
			t.Fatalf("a.txt after the person changed it: %+v", f)
		}
	}
}
