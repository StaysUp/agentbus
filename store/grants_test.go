package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFolderGrants(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	notes := filepath.Join(t.TempDir(), "notes")
	other := filepath.Join(t.TempDir(), "other")
	shared := filepath.Join(t.TempDir(), "shared")
	for _, d := range []string{notes, other, shared} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Initially no grants exist.
	has, err := st.HasFolderGrant("s1", filepath.Join(notes, "a.txt"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected false before grant added")
	}

	// Add grant for session s1.
	if err := st.AddFolderGrant("s1", notes); err != nil {
		t.Fatalf("AddFolderGrant: %v", err)
	}

	// Subpath of notes in s1 is granted.
	has, err = st.HasFolderGrant("s1", filepath.Join(notes, "a.txt"), time.Time{})
	if err != nil || !has {
		t.Errorf("expected true for subpath in s1, got %t %v", has, err)
	}

	// Directory itself in s1 is granted.
	has, err = st.HasFolderGrant("s1", notes, time.Time{})
	if err != nil || !has {
		t.Errorf("expected true for directory in s1, got %t %v", has, err)
	}

	// Nested subpath in s1 is granted.
	has, err = st.HasFolderGrant("s1", filepath.Join(notes, "sub", "deep", "b.txt"), time.Time{})
	if err != nil || !has {
		t.Errorf("expected true for nested subpath in s1, got %t %v", has, err)
	}

	// Another folder in s1 is not granted.
	has, err = st.HasFolderGrant("s1", filepath.Join(other, "c.txt"), time.Time{})
	if err != nil || has {
		t.Errorf("expected false for other folder in s1, got %t %v", has, err)
	}

	// Session s2 does not have access to notes.
	has, err = st.HasFolderGrant("s2", filepath.Join(notes, "a.txt"), time.Time{})
	if err != nil || has {
		t.Errorf("expected false for notes in s2, got %t %v", has, err)
	}

	// Add folder grant tied to the folder (empty session = all sessions).
	if err := st.AddFolderGrant("", shared); err != nil {
		t.Fatalf("AddFolderGrant for all sessions: %v", err)
	}

	// Both s1 and s2 have access to shared.
	for _, sess := range []string{"s1", "s2", "s3"} {
		has, err = st.HasFolderGrant(sess, filepath.Join(shared, "doc.md"), time.Time{})
		if err != nil || !has {
			t.Errorf("expected true for shared in %s, got %t %v", sess, has, err)
		}
	}

	// ListFolderGrants lists grants for the session plus folder-wide grants.
	grants, err := st.ListFolderGrants("s1")
	if err != nil {
		t.Fatalf("ListFolderGrants: %v", err)
	}
	if len(grants) != 2 {
		t.Errorf("expected 2 grants for s1, got %d: %+v", len(grants), grants)
	}

	// Granting root "/" covers child paths (Finding 5).
	if err := st.AddFolderGrant("s_root", "/"); err != nil {
		t.Fatal(err)
	}
	has, err = st.HasFolderGrant("s_root", "/tmp/example.txt", time.Time{})
	if err != nil || !has {
		t.Errorf("root grant did not cover /tmp/example.txt: got %t %v, want true", has, err)
	}
}
