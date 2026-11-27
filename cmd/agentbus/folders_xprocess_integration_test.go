//go:build integration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// A workspace of several folders across processes: control and a separate
// runner that holds the folders, as a team runs them. The folder list reaches
// the runner on the job (the runner has no copy of control's record), the
// turn's changes come back under each folder's name, nested repo included,
// and an apply over HTTP writes each folder's part into that folder.
func TestXProcSeveralFolders(t *testing.T) {
	root := t.TempDir()
	one, two := filepath.Join(root, "one"), filepath.Join(root, "two")
	for p, c := range map[string]string{filepath.Join(one, "a.txt"): "A1\n", filepath.Join(two, "inner", "b.txt"): "B1\n"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-q", "-m", "c"}} {
		if out, err := exec.Command("git", append([]string{"-C", filepath.Join(two, "inner")}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	x := startServer(t, "claude")
	x.setAgentEnv("AGENTBUS_FAKE_WRITE=one/a.txt=A2\n;two/inner/b.txt=B2\n")
	x.addRunner("far", one, two)
	fs := x.folders()
	refs := map[string]string{}
	for _, f := range fs {
		refs[f.Name] = f.Ref
	}
	code, b := x.post("/api/v0/workspaces", map[string]any{"machine": fs[0].Machine,
		"folders": []map[string]string{{"folder_ref": refs["one"]}, {"folder_ref": refs["two"]}}})
	var ws struct {
		Workspace string `json:"workspace"`
	}
	if code != 201 || json.Unmarshal(b, &ws) != nil {
		t.Fatalf("workspace: %d %s", code, b)
	}
	code, b = x.post("/api/v0/sessions", map[string]any{"workspace": ws.Workspace})
	var sc struct {
		Session string `json:"session"`
	}
	if code != 201 || json.Unmarshal(b, &sc) != nil {
		t.Fatalf("session: %d %s", code, b)
	}
	code, st := x.startTurnX(map[string]any{"session": sc.Session, "agent": "claude", "prompt": "edit"})
	if code/100 != 2 {
		t.Fatalf("turn: %d %+v", code, st)
	}
	waitFor(t, "the turn's changes in control's record", func() bool { return hasKind(x.recordKinds(sc.Session), "turn.changed") })

	var page struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	var paths []string
	waitFor(t, "the preview", func() bool {
		code, b := x.get("/api/v0/workspaces/" + ws.Workspace + "/changes")
		if code != 200 || json.Unmarshal(b, &page) != nil {
			return false
		}
		paths = paths[:0]
		for _, f := range page.Files {
			paths = append(paths, f.Path)
		}
		return len(paths) > 0
	})
	if want := []string{"one/a.txt", "two/inner/b.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("preview %v, want %v", paths, want)
	}
	if e := x.act(ws.Workspace, "apply", map[string]any{}); e.Applied == nil || e.Applied.Result != "applied" {
		t.Fatalf("apply: %+v", e)
	}
	for p, want := range map[string]string{filepath.Join(one, "a.txt"): "A2\n", filepath.Join(two, "inner", "b.txt"): "B2\n"} {
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Errorf("%s after the apply: %q, want %q", p, got, want)
		}
	}
}
