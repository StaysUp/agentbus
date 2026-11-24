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
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c"}} {
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
