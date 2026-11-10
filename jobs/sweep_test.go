package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// Live reaches every file and tree under a snapshot, and every file a
// changeset names; a sweep then removes only what none reaches.
func TestLiveAndSweep(t *testing.T) {
	ctx := context.Background()
	st, _ := blobs.Open(t.TempDir())
	put := func(b []byte) string {
		h, err := st.PutBytes(b)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a, b, c, other := put([]byte("a")), put([]byte("b")), put([]byte("c")), put([]byte("nobody's"))
	sub := put(workspace.EncodeTree([]workspace.TreeEntry{{Name: "b.txt", Hash: b, Mode: 0o644, Size: 1}}))
	top := put(workspace.EncodeTree([]workspace.TreeEntry{{Name: "a.txt", Hash: a, Mode: 0o644, Size: 1},
		{Name: "sub", Dir: true, Hash: sub, Count: 1}}))
	csb, _ := json.Marshal(workspace.Changeset{Space: "s", Checkpoint: "t", Files: []workspace.FileChange{
		{Path: "c.txt", Now: &workspace.File{Hash: c, Size: 1, Mode: 0o644}}}})
	cs := put(csb)
	times := put([]byte("times"))
	missing := "ab" + top[2:] // a root no longer held reaches nothing
	live, err := Live(ctx, st, queue.BlobRoots{Trees: []string{top, missing}, Changesets: []string{cs}, Blobs: []string{times}})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{a, b, c, sub, top, cs, times} {
		if !live[h] {
			t.Fatalf("%s not live", h)
		}
	}
	if live[other] {
		t.Fatal("a blob nothing names is live")
	}
	// Everything old: only the one nothing names goes.
	then := time.Now().Add(-2 * time.Hour)
	filepath.Walk(st.Dir(), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			os.Chtimes(p, then, then)
		}
		return nil
	})
	if _, removed, err := st.Sweep(ctx, func(h string) bool { return live[h] }, time.Hour); err != nil || removed != 1 || st.Has(other) {
		t.Fatalf("removed %d: %v", removed, err)
	}
	// A changeset held but not one: nothing is swept.
	bad := put([]byte("not json"))
	if _, err := Live(ctx, st, queue.BlobRoots{Changesets: []string{bad}}); err == nil {
		t.Fatal("a changeset that is not one was read")
	}
}

// A file whose bytes are a tree's does not stop that tree being read.
func TestLiveTreeThatIsAlsoAFile(t *testing.T) {
	ctx := context.Background()
	st, _ := blobs.Open(t.TempDir())
	leaf, _ := st.PutBytes([]byte("leaf"))
	subBytes := workspace.EncodeTree([]workspace.TreeEntry{{Name: "leaf.txt", Hash: leaf, Mode: 0o644, Size: 4}})
	sub, _ := st.PutBytes(subBytes)
	// The file comes first in the walk: a.txt holds sub's bytes.
	top, _ := st.PutBytes(workspace.EncodeTree([]workspace.TreeEntry{{Name: "a.txt", Hash: sub, Mode: 0o644, Size: int64(len(subBytes))},
		{Name: "z", Dir: true, Hash: sub, Count: 1}}))
	other, _ := st.PutBytes(workspace.EncodeTree([]workspace.TreeEntry{{Name: "a.txt", Hash: sub, Mode: 0o644, Size: int64(len(subBytes))}}))
	live, err := Live(ctx, st, queue.BlobRoots{Trees: []string{other, top}})
	if err != nil || !live[leaf] {
		t.Fatalf("the leaf under a tree that is also a file is not live: %v", err)
	}
}
