package connector

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/staysup/agentbus/workspace"
)

// folderBytes is every file of dir, by its path, with its mode.
func folderBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, _ := d.Info()
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fi.Mode().String() + ":" + string(b)
		return nil
	})
	return out
}

// The preview is the apply's dry run: it writes nothing, it lists the files
// the apply then writes, and each diff is the one from the folder before
// the apply to the folder after it.
func TestPreviewIsTheApply(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "one\ntwo\n", "b.txt": "b\n", "same.txt": "s\n", "bin": "x\x00y"})
	c, ref := newConnector(t, folder)
	cs := workspace.Changeset{Space: "job_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: fileOf("one\ntwo\n", 0o644), Now: fileOf("one\nTWO\nthree\n", 0o644)},
		{Path: "b.txt", Base: fileOf("b\n", 0o644)},
		{Path: "bin", Base: fileOf("x\x00y", 0o644), Now: fileOf("x\x00z", 0o644)},
		{Path: "new/n.txt", Now: fileOf("n\n", 0o644)},
		// The person already made same.txt as the turn left it: no write.
		{Path: "same.txt", Base: fileOf("old\n", 0o644), Now: fileOf("s\n", 0o644)},
	}}
	h := putChangeset(t, c, cs, map[string]string{"a": "one\nTWO\nthree\n", "bin": "x\x00z", "n": "n\n", "s": "s\n"})
	before := folderBytes(t, folder)
	p, err := c.Preview(context.Background(), ref, "", h)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(folderBytes(t, folder), before) {
		t.Fatal("the preview wrote in the folder")
	}
	if len(p.Conflicts) != 0 || p.Count != 4 {
		t.Fatalf("preview: %+v", p)
	}
	got, err := c.Apply(context.Background(), ref, "", h)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range p.Files {
		paths = append(paths, f.Path)
	}
	if !reflect.DeepEqual(paths, got.Files) {
		t.Fatalf("preview files %v, apply wrote %v", paths, got.Files)
	}
	after := folderBytes(t, folder)
	strip := func(s string) []byte {
		if s == "" {
			return nil
		}
		return []byte(s[strings.IndexByte(s, ':')+1:])
	}
	want := map[string]string{"a.txt": "modified +2 -1", "b.txt": "deleted +0 -1", "bin": "modified left out", "new/n.txt": "added +1 -0"}
	for _, f := range p.Files {
		rel := filepath.FromSlash(f.Path)
		desc := f.Change + " left out"
		if f.Diff != nil {
			desc = f.Change + " +" + strconv.Itoa(f.Added) + " -" + strconv.Itoa(f.Removed)
			if d := workspace.Unified(f.Path, strip(before[rel]), strip(after[rel]), f.Change); d != *f.Diff {
				t.Errorf("%s: preview diff %q, the apply's change %q", f.Path, *f.Diff, d)
			}
		} else if !f.DiffLeftOut {
			t.Errorf("%s: no diff and not left out", f.Path)
		}
		if desc != want[f.Path] {
			t.Errorf("%s: %s, want %s", f.Path, desc, want[f.Path])
		}
	}
}

// A conflict is in the preview as the apply then refuses it, and the file is
// listed with its diff against the person's edit.
func TestPreviewConflictIsTheApplysRefusal(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"a.txt": "one\n", "b.txt": "b\n"})
	c, ref := newConnector(t, folder)
	cs := workspace.Changeset{Space: "job_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: fileOf("one\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "b.txt", Base: fileOf("b\n", 0o644), Now: fileOf("B\n", 0o644)},
	}}
	h := putChangeset(t, c, cs, map[string]string{"a": "A\n", "b": "B\n"})
	write(t, folder, map[string]string{"b.txt": "mine\n"})
	before := folderBytes(t, folder)
	p, err := c.Preview(context.Background(), ref, "", h)
	if err != nil || len(p.Conflicts) != 1 || p.Conflicts[0].Path != "b.txt" || p.Conflicts[0].Why != WhyChanged || p.Count != 2 {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if b := p.Files[1]; b.Path != "b.txt" || b.Diff == nil || *b.Diff != "--- a/b.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-mine\n+B\n" {
		t.Fatalf("conflicting file: %+v", b)
	}
	if !reflect.DeepEqual(folderBytes(t, folder), before) {
		t.Fatal("the preview wrote in the folder")
	}
	_, err = c.Apply(context.Background(), ref, "", h)
	cf, ok := err.(*Conflict)
	if !ok || len(cf.Files) != 1 || cf.Files[0].Path != p.Conflicts[0].Path || cf.Files[0].Why != p.Conflicts[0].Why {
		t.Fatalf("apply: %v, preview %+v", err, p.Conflicts)
	}
}
