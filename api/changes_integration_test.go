//go:build integration

package api

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/workspace"
)

func changesOf(t *testing.T, e *env, session string) SessionChanges {
	t.Helper()
	code, out := as(t, e, token, "GET", "/api/v0/sessions/"+session+"/changes", "")
	if code != 200 {
		t.Fatalf("changes of %s: %d %s", session, code, out)
	}
	var c SessionChanges
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// snapshotOf is every file of dir with its bytes.
func snapshotOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return out
}

// The preview of a session's changes is the apply that follows it: the same
// files, each diff the change the apply makes, and it writes nothing. A
// conflict in the preview is the file the apply then refuses. A session
// fully applied reports nothing unapplied.
func TestSessionChangesPreview(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	for p, c := range map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n"} {
		if err := os.WriteFile(filepath.Join(folder, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"edit1"}`)
	waitEnded(t, e, first.Turn)

	before := snapshotOf(t, folder)
	c := changesOf(t, e, first.Session)
	if !reflect.DeepEqual(snapshotOf(t, folder), before) {
		t.Fatal("the preview wrote in the folder")
	}
	if !c.Isolated || !c.Unapplied || c.Turn != first.Turn || !slices.Equal(c.Turns, []string{first.Turn}) ||
		c.Count != 3 || len(c.Conflicts) != 0 {
		t.Fatalf("preview: %+v", c)
	}
	code, st := applyAs(t, e, token, c.Turn)
	if code != 200 {
		t.Fatalf("apply: %d %+v", code, st)
	}
	var paths []string
	for _, f := range c.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, st.Files) {
		t.Fatalf("preview files %v, the apply wrote %v", paths, st.Files)
	}
	after := snapshotOf(t, folder)
	bytesOf := func(m map[string]string, p string) []byte {
		if s, ok := m[p]; ok {
			return []byte(s)
		}
		return nil
	}
	for _, f := range c.Files {
		if f.Diff == nil {
			t.Fatalf("%s: no diff", f.Path)
		}
		if d := workspace.Unified(f.Path, bytesOf(before, f.Path), bytesOf(after, f.Path), f.Change); d != *f.Diff {
			t.Errorf("%s: preview %q, the apply's change %q", f.Path, *f.Diff, d)
		}
	}
	if c := changesOf(t, e, first.Session); c.Unapplied || len(c.Files) != 0 || c.Turn != first.Turn {
		t.Fatalf("preview of a session fully applied: %+v", c)
	}

	// The next turn changes a.txt, and so does the person.
	second := startTurn(t, e, `{"agent":"claude","session":"`+first.Session+`","prompt":"edit2"}`)
	waitEnded(t, e, second.Turn)
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("mine\n"), 0o644)
	before = snapshotOf(t, folder)
	c = changesOf(t, e, first.Session)
	if !reflect.DeepEqual(snapshotOf(t, folder), before) {
		t.Fatal("the preview wrote in the folder")
	}
	if !c.Unapplied || c.Turn != second.Turn || len(c.Conflicts) != 1 || c.Conflicts[0].Path != "a.txt" ||
		len(c.Files) != 1 || c.Files[0].Path != "a.txt" || c.Files[0].Diff == nil ||
		*c.Files[0].Diff != "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1,2 @@\n-mine\n+one\n+three\n" {
		t.Fatalf("preview over the person's edit: %+v", c)
	}
	code, st = applyAs(t, e, token, c.Turn)
	if code != 409 || len(st.Conflicts) != 1 || st.Conflicts[0] != c.Conflicts[0] {
		t.Fatalf("apply after the preview: %d %+v, preview %+v", code, st, c.Conflicts)
	}

	if code, out := as(t, e, token, "GET", "/api/v0/sessions/s_0000000000000000/changes", ""); code != 404 {
		t.Errorf("an unknown session: %d %s", code, out)
	}
}

// A session in the person's folder as it is has nothing to apply, and says
// so.
func TestSessionChangesNotIsolated(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("isolation = \""+settings.IsolationNone+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := diffEnv(t, perm)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	first := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"edit1"}`)
	waitEnded(t, e, first.Turn)
	c := changesOf(t, e, first.Session)
	if c.Isolated || c.Unapplied || c.Why != changesNotIsolated || len(c.Files) != 0 {
		t.Fatalf("preview of a session in the folder: %+v", c)
	}
}

// A session whose first turn has not ended has nothing to apply yet: it is
// not told it works in the folder as it is.
func TestSessionChangesFirstTurnRunning(t *testing.T) {
	e, dir := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
	first := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"hold"}`)
	tokenOf(t, dir, "hold") // the turn is running
	c := changesOf(t, e, first.Session)
	if !c.Isolated || c.Unapplied || c.Why != "" || c.Turn != "" || len(c.Files) != 0 {
		t.Fatalf("preview while the first turn runs: %+v", c)
	}
	release(dir, "hold")
	waitEnded(t, e, first.Turn)
}
