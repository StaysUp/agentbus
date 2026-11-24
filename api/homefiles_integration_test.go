//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// GET /api/v0/folders/{id}/home-files asks the folder machine's runner which
// usual config files its home holds: names, kinds and sizes, a folder's at
// any depth, never a vendor sign-in, and no job is kept once read.
func TestFolderHomeFiles(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	sess := e.newSession(t, folder, true)
	first := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"edit1"}`)

	home := t.TempDir()
	for p, c := range map[string]string{".claude/settings.json": "{}", ".claude/skills/a/SKILL.md": "abc", ".claude/.credentials.json": "x",
		".codex/auth.json": "x", ".codex/config.toml": "", ".gitconfig": "[user]\n", ".claude.json": `{"mcpServers":{}}`} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, p), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)

	j := waitEnded(t, e, first.Turn)
	ref := j.Spec.FolderRef
	if ref == "" {
		t.Fatal("the turn names no folder")
	}
	code, out := as(t, e, token, "GET", "/api/v0/folders/"+ref+"/home-files", "")
	if code != 200 {
		t.Fatalf("home-files: %d %s", code, out)
	}
	var got HomeFileList
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := []queue.HomeFile{
		{Path: "~/.claude/settings.json", Kind: "file", Size: 2},
		{Path: "~/.claude/skills/", Kind: "folder", Size: 3, Files: 1},
		{Path: "~/.claude.json", Kind: "file", Size: 17, Part: "mcpServers"},
		{Path: "~/.codex/config.toml", Kind: "file", Size: 0},
		{Path: "~/.gitconfig", Kind: "file", Size: 7},
	}
	if got.Folder != ref || got.Machine == "" || len(got.Files) != len(want) {
		t.Fatalf("home-files: %s", out)
	}
	for i := range want {
		if got.Files[i] != want[i] {
			t.Errorf("file %d: %+v, want %+v", i, got.Files[i], want[i])
		}
	}
	if code, _ := as(t, e, token, "GET", "/api/v0/folders/fld_0000000000000000/home-files", ""); code != 404 {
		t.Errorf("an unknown folder: %d", code)
	}
	if js, err := e.q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindHome}, 100); err != nil || len(js) != 0 {
		t.Errorf("home jobs kept: %d %v", len(js), err)
	}
}
