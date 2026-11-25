package connector

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/workspace"
)

// A changeset of a workspace of two folders applies each folder's part (its
// files, under its name) into that folder. A conflict in one folder refuses
// the whole apply, and neither folder is written. The preview names every
// file under its folder's name, its conflicts too.
func TestApplyPlaces(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	write(t, one, map[string]string{"a.txt": "a\n"})
	write(t, two, map[string]string{"b.txt": "b\n"})
	c, r1 := newConnector(t, one)
	r2, err := c.Folders.Register(two)
	if err != nil {
		t.Fatal(err)
	}
	places := []workspace.Place{{Name: "one", Ref: r1}, {Name: "two", Ref: r2}}
	h := putChangeset(t, c, workspace.Changeset{Space: "ws_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "one/a.txt", Base: fileOf("a\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "two/b.txt", Base: fileOf("b\n", 0o644), Now: fileOf("B\n", 0o644)},
		{Path: "two/sub/n.txt", Now: fileOf("n\n", 0o644)},
	}}, map[string]string{"a": "A\n", "b": "B\n", "n": "n\n"})

	// The person changed b.txt: the apply is refused in both folders.
	write(t, two, map[string]string{"b.txt": "mine\n"})
	p, err := c.PreviewPlaces(context.Background(), places, "", h, false)
	if err != nil || len(p.Files) != 3 || p.Files[0].Path != "one/a.txt" || p.Files[2].Path != "two/sub/n.txt" ||
		len(p.Conflicts) != 1 || p.Conflicts[0].Path != "two/b.txt" || p.Count != 3 {
		t.Fatalf("preview: %+v %v", p, err)
	}
	_, err = c.ApplyPlaces(context.Background(), places, "", h, false)
	var cf *Conflict
	if !errors.As(err, &cf) || len(cf.Files) != 1 || cf.Files[0].Path != "two/b.txt" {
		t.Fatalf("apply over a change in one folder: %v", err)
	}
	if text(t, filepath.Join(one, "a.txt")) != "a\n" || text(t, filepath.Join(two, "sub", "n.txt")) != "<missing>" {
		t.Fatal("a refused apply wrote in a folder")
	}

	// Undone: both folders get their part.
	write(t, two, map[string]string{"b.txt": "b\n"})
	got, err := c.ApplyPlaces(context.Background(), places, "", h, false)
	if err != nil || len(got.Files) != 3 || got.Files[0] != "one/a.txt" || got.Files[1] != "two/b.txt" {
		t.Fatalf("apply: %+v %v", got, err)
	}
	if text(t, filepath.Join(one, "a.txt")) != "A\n" || text(t, filepath.Join(two, "b.txt")) != "B\n" ||
		text(t, filepath.Join(two, "sub", "n.txt")) != "n\n" {
		t.Fatal("the folders are not as the turn left the copy")
	}
	if text(t, filepath.Join(one, "two", "b.txt")) != "<missing>" {
		t.Fatal("a file went into the wrong folder")
	}
}

// A file a turn made beside the folders, in none of them, refuses the apply,
// and the preview names it: no folder can hold it, and it is not dropped
// without a word.
func TestApplyPlacesBeside(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	write(t, one, map[string]string{"a.txt": "a\n"})
	c, r1 := newConnector(t, one)
	r2, err := c.Folders.Register(two)
	if err != nil {
		t.Fatal(err)
	}
	places := []workspace.Place{{Name: "one", Ref: r1}, {Name: "two", Ref: r2}}
	h := putChangeset(t, c, workspace.Changeset{Space: "ws_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "notes.md", Now: fileOf("n\n", 0o644)},
		{Path: "one/a.txt", Base: fileOf("a\n", 0o644), Now: fileOf("A\n", 0o644)},
	}}, map[string]string{"a": "A\n", "n": "n\n"})
	p, err := c.PreviewPlaces(context.Background(), places, "", h, false)
	if err != nil || len(p.Conflicts) != 1 || p.Conflicts[0].Path != "notes.md" || p.Conflicts[0].Why != WhyBeside || p.Count != 2 {
		t.Fatalf("preview: %+v %v", p, err)
	}
	_, err = c.ApplyPlaces(context.Background(), places, "", h, false)
	var cf *Conflict
	if !errors.As(err, &cf) || len(cf.Files) != 1 || cf.Files[0].Path != "notes.md" {
		t.Fatalf("apply with a file beside the folders: %v", err)
	}
	if text(t, filepath.Join(one, "a.txt")) != "a\n" {
		t.Fatal("a refused apply wrote")
	}
}

// After a merge left conflict markers, an apply of several folders refuses a
// file that still holds them, in whichever folder it is, and writes no folder.
func TestApplyPlacesMarkers(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	write(t, one, map[string]string{"a.txt": "a\n"})
	write(t, two, map[string]string{"b.txt": "b\n"})
	c, r1 := newConnector(t, one)
	r2, err := c.Folders.Register(two)
	if err != nil {
		t.Fatal(err)
	}
	places := []workspace.Place{{Name: "one", Ref: r1}, {Name: "two", Ref: r2}}
	marked := "<<<<<<< " + workspace.MergeOurs + "\nX\n=======\nY\n>>>>>>> " + workspace.MergeTheirs + "\n"
	h := putChangeset(t, c, workspace.Changeset{Space: "ws_1", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "one/a.txt", Base: fileOf("a\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "two/b.txt", Base: fileOf("b\n", 0o644), Now: fileOf(marked, 0o644)},
	}}, map[string]string{"a": "A\n", "m": marked})
	_, err = c.ApplyPlaces(context.Background(), places, "", h, true)
	var cf *Conflict
	if !errors.As(err, &cf) || len(cf.Files) != 1 || cf.Files[0].Path != "two/b.txt" || cf.Files[0].Why != WhyMarkers {
		t.Fatalf("apply of a marked file: %v", err)
	}
	if text(t, filepath.Join(one, "a.txt")) != "a\n" {
		t.Fatal("a refused apply wrote another folder")
	}
}
