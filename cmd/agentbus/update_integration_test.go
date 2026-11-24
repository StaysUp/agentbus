//go:build integration

package main

// Update from the person's folder, end to end over HTTP in agentbus up: a
// sandbox session's copy falls behind the folder (the person edited it), an
// apply is refused, an update brings the folder in by a three-way merge, and
// the apply then writes only the session's own work. A stand-in claude edits
// the files, so no model is called.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type updateAnswer struct {
	State     string
	Updated   bool
	Conflicts []string
	Error     string
}

func (c *client) update(ws string) (int, updateAnswer) {
	var u updateAnswer
	code, _ := c.call("POST", "/api/v0/workspaces/"+ws+"/update", map[string]any{}, &u)
	return code, u
}

// A clean update: the person edits a.txt where the session did not, so the
// merge is clean. The apply, refused before, then writes only the session's
// work against the folder as it is now.
func TestUpdateFromFolderCleanThenApply(t *testing.T) {
	bin, bins := built(t), standInDir(t)
	home, addr := t.TempDir(), freeAddr(t)
	p := startProc(t, bin, home, bins, "up", "-listen", addr, "-replay=false")
	c := waitService(t, p, addr, home)
	folder := personFolder(t)

	// The session's unapplied work: a.txt gains a line, new.txt is created.
	t1 := c.turn(map[string]any{"folder": folder, "agent": "claude", "prompt": "p1"})
	ws := c.ws[t1.Session]
	if ws == "" {
		t.Fatal("no workspace for the session")
	}

	// The person edits a.txt in a different region (prepends a line), so the
	// copy is now behind the folder.
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("ZERO\none\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply is refused: a.txt changed in the folder since the checkpoint.
	if code, a := c.apply(t1); code != http.StatusConflict || a.State != "refused" ||
		len(a.Conflicts) != 1 || a.Conflicts[0].Path != "a.txt" {
		t.Fatalf("apply before update: %d %+v, want 409 refused on a.txt", code, a)
	}

	// Update merges the folder into the copy.
	if code, u := c.update(ws); code != 200 || u.State != "updated" || !u.Updated {
		t.Fatalf("update: %d %+v, want 200 updated", code, u)
	}

	// Apply now writes only the session's work, against the folder as it is now.
	if code, a := c.apply(t1); code != 200 || a.State != "applied" {
		t.Fatalf("apply after update: %d %+v, want 200 applied", code, a)
	}
	if got := fileText(t, filepath.Join(folder, "a.txt")); got != "ZERO\none\np1\n" {
		t.Fatalf("a.txt after update+apply: %q, want the person's line and the session's", got)
	}
	if got := fileText(t, filepath.Join(folder, "new.txt")); got != "p1\n" {
		t.Fatalf("new.txt after update+apply: %q", got)
	}
	// b.txt was the folder's and the session never touched it: still as it was.
	if got := fileText(t, filepath.Join(folder, "b.txt")); got != "keep\n" {
		t.Fatalf("b.txt should be untouched: %q", got)
	}
}

// A real conflict (the person edited the same region the session did) changes
// nothing: the update names the file, the copy is untouched, and the apply is
// still refused.
func TestUpdateFromFolderConflictChangesNothing(t *testing.T) {
	bin, bins := built(t), standInDir(t)
	home, addr := t.TempDir(), freeAddr(t)
	p := startProc(t, bin, home, bins, "up", "-listen", addr, "-replay=false")
	c := waitService(t, p, addr, home)
	folder := personFolder(t)

	t1 := c.turn(map[string]any{"folder": folder, "agent": "claude", "prompt": "p1"})
	ws := c.ws[t1.Session]

	// The person appends a conflicting line in the same region as the session's.
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\nHUMAN\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, u := c.update(ws); code != 200 || u.State != "conflict" ||
		len(u.Conflicts) != 1 || u.Conflicts[0] != "a.txt" {
		t.Fatalf("conflicting update: %d %+v, want 200 conflict on a.txt", code, u)
	}
	// Nothing changed in the folder, and the apply is still refused.
	if got := fileText(t, filepath.Join(folder, "a.txt")); got != "one\nHUMAN\n" {
		t.Fatalf("folder a.txt changed on a conflict: %q", got)
	}
	if code, a := c.apply(t1); code != http.StatusConflict || a.State != "refused" {
		t.Fatalf("apply after a conflicting update: %d %+v, want still refused", code, a)
	}
}

// A direct (sandbox false) workspace has no copy, so an update is refused.
func TestUpdateDirectWorkspaceRefused(t *testing.T) {
	bin, bins := built(t), standInDir(t)
	home, addr := t.TempDir(), freeAddr(t)
	p := startProc(t, bin, home, bins, "up", "-listen", addr, "-replay=false")
	c := waitService(t, p, addr, home)
	folder := personFolder(t)

	var ws struct {
		Workspace string `json:"workspace"`
	}
	if code, out := c.call("POST", "/api/v0/workspaces", map[string]any{"folder": folder, "sandbox": false}, &ws); code/100 != 2 || ws.Workspace == "" {
		t.Fatalf("create direct workspace: %d %s", code, out)
	}
	if code, u := c.update(ws.Workspace); code != http.StatusBadRequest {
		t.Fatalf("update of a direct workspace: %d %+v, want 400", code, u)
	}
}
