package queue

import (
	"encoding/json"
	"slices"
	"testing"
)

// A session's snapshot and changesets are roots until it closes; a snapshot
// job belongs to the session whose turn asked for it.
func TestBlobRoots(t *testing.T) {
	q, _ := open(t)
	const con, ref = "con_0123456789abcdef", "fld_0123456789abcdef"
	conn, err := q.RegisterRunner(DefaultTeam, "c", nil, 2, HashToken(NewID("tok_")), &Capabilities{Role: RoleConnector, Connector: con,
		Folders: []FolderInfo{{Ref: ref, Name: "p"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := runner(t, q, 2)
	// One session per folder turn, each with a snapshot and a changeset.
	session := func(cs, tree string) string {
		t.Helper()
		j, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", FolderRef: ref, Connector: con, Isolate: true, Prompt: "p"}, Links{})
		if err != nil {
			t.Fatal(err)
		}
		_, a := take(t, q, r)
		if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Snapshot(r.ID, a.ID, 1); err != nil {
			t.Fatal(err)
		}
		_, sa := take(t, q, conn)
		if _, _, err := q.Start(conn.ID, sa.ID, 1, lease); err != nil {
			t.Fatal(err)
		}
		if _, err := q.End(conn.ID, sa.ID, 1, Finish{Status: Finished,
			Result: mustResult(Result{Snapshot: &SnapshotTaken{ID: tree, Times: tree + "-times", Git: tree + "-git"}})}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.End(r.ID, a.ID, 1, Finish{Status: Finished, Session: j.Session, Result: mustResult(Result{Changeset: cs}),
			Workspace: &Workspace{Kind: WorkspaceCopy, Folder: "s"}}); err != nil {
			t.Fatal(err)
		}
		return j.Session
	}
	s1 := session("cs1", "tree1")
	session("cs2", "tree2")
	roots, err := q.BlobRoots()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   []string
		want []string
	}{{roots.Changesets, []string{"cs1", "cs2"}}, {roots.Trees, []string{"tree1", "tree1-git", "tree2", "tree2-git"}},
		{roots.Blobs, []string{"tree1-times", "tree2-times"}}} {
		for _, w := range c.want {
			if !slices.Contains(c.in, w) {
				t.Fatalf("%s not a root: %+v", w, roots)
			}
		}
	}
	if _, err := q.CloseSession(DefaultTeam, s1); err != nil {
		t.Fatal(err)
	}
	roots, _ = q.BlobRoots()
	for _, gone := range []string{"cs1", "tree1", "tree1-git", "tree1-times"} {
		if slices.Contains(roots.Changesets, gone) || slices.Contains(roots.Trees, gone) || slices.Contains(roots.Blobs, gone) {
			t.Fatalf("%s of a closed session is still a root: %+v", gone, roots)
		}
	}
	if !slices.Contains(roots.Changesets, "cs2") || !slices.Contains(roots.Trees, "tree2") || !slices.Contains(roots.Blobs, "tree2-times") {
		t.Fatalf("the open session's roots went: %+v", roots)
	}
}

func mustResult(r Result) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return b
}
