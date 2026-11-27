package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// twinFolder is a person's folder holding old.go and new.go, one size and one
// time an hour ago, as a checkout writes them, and old.go as a Base recorded
// it by size, time and mode only (an overlay copy's Base).
func twinFolder(t *testing.T) (string, workspace.File) {
	t.Helper()
	folder := t.TempDir()
	write(t, folder, map[string]string{"old.go": "package one\n", "new.go": "package two\n"})
	tick := time.Now().Add(-time.Hour)
	for _, p := range []string{"old.go", "new.go"} {
		if err := os.Chtimes(filepath.Join(folder, p), tick, tick); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Lstat(filepath.Join(folder, "old.go"))
	if err != nil {
		t.Fatal(err)
	}
	return folder, workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}
}

// A turn's edit of old.go, against a Base that recorded it unhashed, as of a
// Base that began reading the folder at taken.
func editOfOld(t *testing.T, c *Connector, base workspace.File, taken int64) string {
	t.Helper()
	return putChangeset(t, c, workspace.Changeset{Space: "job_1", Checkpoint: "t1", BaseTaken: taken, Files: []workspace.FileChange{
		{Path: "old.go", Base: &base, Now: fileOf("package agent\n", 0o644)},
	}}, map[string]string{"a": "package agent\n"})
}

// The person moves new.go over old.go after the copy read the folder: old.go
// keeps the size, time and mode the Base recorded, and only its change time
// says it is another file. The apply refuses it as changed and writes
// nothing. Before, the stat matched, the apply wrote the agent's old.go over
// the person's, and the person's content was gone.
func TestApplyRefusesATwinMovedOverAFile(t *testing.T) {
	folder, base := twinFolder(t)
	c, ref := newConnector(t, folder)
	taken := time.Now().UnixNano() // the copy reads the folder now
	if err := os.Rename(filepath.Join(folder, "new.go"), filepath.Join(folder, "old.go")); err != nil {
		t.Fatal(err)
	}
	var cf *Conflict
	if _, err := c.Apply(context.Background(), ref, "", editOfOld(t, c, base, taken)); !errors.As(err, &cf) || cf.Files[0].Why != WhyChanged {
		t.Fatalf("apply over a twin moved in: %v", err)
	}
	if got := text(t, filepath.Join(folder, "old.go")); got != "package two\n" {
		t.Fatalf("the person's old.go: %q", got)
	}
}

// A file nobody touched since well before the Base read the folder keeps the
// fast path: it applies without being read. A changeset with no Base time (one
// made before 0.6.7) proves nothing, so the same file refuses.
func TestApplyTrustsAFileUntouchedSinceTheBase(t *testing.T) {
	folder, base := twinFolder(t)
	c, ref := newConnector(t, folder)
	var cf *Conflict
	if _, err := c.Apply(context.Background(), ref, "", editOfOld(t, c, base, 0)); !errors.As(err, &cf) {
		t.Fatalf("a changeset with no Base time was trusted: %v", err)
	}
	// As if the Base began reading a minute from now: every change time here
	// is well before it.
	if _, err := c.Apply(context.Background(), ref, "", editOfOld(t, c, base, time.Now().Add(time.Minute).UnixNano())); err != nil {
		t.Fatal(err)
	}
	if got := text(t, filepath.Join(folder, "old.go")); got != "package agent\n" {
		t.Fatalf("old.go: %q", got)
	}
}

// Lend lends no twin moved over a file since the Base read the folder: its
// bytes are not the Base's, and a copy that learned them as its Base content
// would show and apply the wrong change. Before, the stat matched and it lent.
func TestLendRefusesATwinMovedOverAFile(t *testing.T) {
	folder, base := twinFolder(t)
	c, ref := newConnector(t, folder)
	taken := time.Now().UnixNano()
	if err := os.Rename(filepath.Join(folder, "new.go"), filepath.Join(folder, "old.go")); err != nil {
		t.Fatal(err)
	}
	if r, _, ok := c.Lend(ref, "old.go", base, taken); ok {
		r.Close()
		t.Fatal("lent a twin moved over the file")
	}
	if _, _, ok := c.Lend(ref, "old.go", base, 0); ok {
		t.Fatal("lent with no Base time")
	}
}
