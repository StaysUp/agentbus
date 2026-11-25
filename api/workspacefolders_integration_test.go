//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// foldersEnv starts an API whose turns run a shell that edits a workspace of
// two folders, one and two, by prompt.
func foldersEnv(t *testing.T) *env {
	t.Helper()
	script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
		`case "$p" in ` +
		`edit) printf 'A2\n' > one/a.txt; printf 'B2\n' > two/inner/b.txt; echo new > two/new.txt;; ` +
		`esac`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire, permissions: filepath.Join(t.TempDir(), settings.PolicyFile)})
}

// twoFolders makes folders one and two (two holding a committed inner repo),
// and a sandbox workspace of both, and answers the folders and a session in it.
func twoFolders(t *testing.T, e *env) (one, two, session string) {
	t.Helper()
	root := t.TempDir()
	one, two = filepath.Join(root, "one"), filepath.Join(root, "two")
	for p, c := range map[string]string{filepath.Join(one, "a.txt"): "A1\n", filepath.Join(two, "inner", "b.txt"): "B1\n"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inner := filepath.Join(two, "inner")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-q", "-m", "c"}} {
		if out, err := exec.Command("git", append([]string{"-C", inner}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	r1, machine := e.folderRef(t, one)
	r2, _ := e.folderRef(t, two)
	code, out := as(t, e, token, "POST", "/api/v0/workspaces", fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":%q}],"machine":%q}`, r1, r2, machine))
	var ws store.Workspace
	if code != 201 || json.Unmarshal([]byte(out), &ws) != nil {
		t.Fatalf("workspace: %d %s", code, out)
	}
	return one, two, e.makeSession(t, ws.ID)
}

// A turn in a workspace of several folders runs at the top of one copy that
// holds each folder under its name, and its changes are every folder's, a
// nested repo's included, each path starting with its folder's name. The
// person's folders are untouched.
func TestTurnInSeveralFolders(t *testing.T) {
	e := foldersEnv(t)
	one, two, sess := twoFolders(t, e)
	st := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"edit"}`)
	outcomeKept(t, e, st.Turn)
	code, out := as(t, e, token, "GET", "/api/v0/turns/"+st.Turn+"/record", "")
	var rec TurnRecord
	if code != 200 || json.Unmarshal([]byte(out), &rec) != nil {
		t.Fatalf("record: %d %s", code, out)
	}
	var paths []string
	for _, f := range rec.Files {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	if want := []string{"one/a.txt", "two/inner/b.txt", "two/new.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("files %v, want %v (%s)", paths, want, out)
	}
	for p, want := range map[string]string{filepath.Join(one, "a.txt"): "A1\n", filepath.Join(two, "inner", "b.txt"): "B1\n"} {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s in the person's folder: %q", p, b)
		}
	}
	if _, err := os.Stat(filepath.Join(two, "new.txt")); !os.IsNotExist(err) {
		t.Error("the turn wrote in the person's folder")
	}
	// The second turn runs in the same copy.
	st2 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"nothing"}`)
	if j := waitEnded(t, e, st2.Turn); j.State != "finished" {
		res := strings.TrimSpace(string(j.Result))
		t.Fatalf("second turn: %s %s", j.State, res)
	}
}

// Review, Apply, Discard and Update of a workspace of several folders cover
// every folder together. The preview lists each folder's files under its name;
// a conflict in one folder refuses the whole apply and writes neither; once
// clear, the apply writes each folder's part into it. A discard resets every
// folder's copy, and an update brings each folder's own changes in.
func TestSeveralFoldersReviewApply(t *testing.T) {
	e := foldersEnv(t)
	one, two, sess := twoFolders(t, e)
	st := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"edit"}`)
	outcomeKept(t, e, st.Turn)
	ws := wsOfTurn(t, e, st.Turn)

	c := changesOf(t, e, sess)
	var paths []string
	for _, f := range c.Files {
		paths = append(paths, f.Path)
	}
	if want := []string{"one/a.txt", "two/inner/b.txt", "two/new.txt"}; !slices.Equal(paths, want) || !c.Unapplied || len(c.Conflicts) != 0 {
		t.Fatalf("preview %v, want %v: %+v", paths, want, c)
	}

	// The person edits b.txt in the inner repo: the apply is refused, and
	// one/a.txt is not written either.
	os.WriteFile(filepath.Join(two, "inner", "b.txt"), []byte("MINE\n"), 0o644)
	code, ap := applyWS(t, e, token, ws, "")
	if code != 409 || len(ap.Conflicts) != 1 || ap.Conflicts[0].Path != "two/inner/b.txt" {
		t.Fatalf("apply over a change in one folder: %d %+v", code, ap)
	}
	if b, _ := os.ReadFile(filepath.Join(one, "a.txt")); string(b) != "A1\n" {
		t.Fatal("a refused apply wrote in another folder")
	}

	// Put back, the apply writes each folder's part.
	os.WriteFile(filepath.Join(two, "inner", "b.txt"), []byte("B1\n"), 0o644)
	if code, ap = applyWS(t, e, token, ws, ""); code != 200 {
		t.Fatalf("apply: %d %+v", code, ap)
	}
	for p, want := range map[string]string{filepath.Join(one, "a.txt"): "A2\n", filepath.Join(two, "inner", "b.txt"): "B2\n", filepath.Join(two, "new.txt"): "new\n"} {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s after the apply: %q, want %q", p, b, want)
		}
	}
	if _, err := os.Stat(filepath.Join(one, "two")); !os.IsNotExist(err) {
		t.Error("an apply wrote another folder's files into one")
	}

	// The person edits one/a.txt; an update brings it into the copy.
	os.WriteFile(filepath.Join(one, "a.txt"), []byte("PERSON\n"), 0o644)
	code, out := as(t, e, token, "POST", "/api/v0/workspaces/"+ws+"/update", "")
	if code != 200 || !strings.Contains(out, `"updated":true`) {
		t.Fatalf("update: %d %s", code, out)
	}
	if c := changesOf(t, e, sess); c.Unapplied {
		t.Fatalf("after an update with no work of its own: %+v", c)
	}

	// A second turn, then a discard: every folder's copy goes back.
	st2 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"edit"}`)
	outcomeKept(t, e, st2.Turn)
	if c := changesOf(t, e, sess); !c.Unapplied {
		t.Fatalf("the second turn's changes: %+v", c)
	}
	if code, out := as(t, e, token, "POST", "/api/v0/workspaces/"+ws+"/discard", ""); code != 200 {
		t.Fatalf("discard: %d %s", code, out)
	}
	if c := changesOf(t, e, sess); c.Unapplied {
		t.Fatalf("after a discard: %+v", c)
	}
}
