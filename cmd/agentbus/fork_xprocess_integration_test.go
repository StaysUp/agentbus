//go:build integration

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A workspace made from another workspace's copy, through control and a
// separate runner: its first turn runs in a copy the runner holding the source
// made from the source's checkpoint (the files the checkpoint recorded, and the
// ignored folders the source holds), never in the source's copy; its turn's
// changes start from that checkpoint; apply refuses it; and once its turn ends
// AgentBus puts its copy away, while the source keeps its own as it was.
func TestXProcWorkspaceFromACopy(t *testing.T) {
	folder := t.TempDir()
	for rel, c := range map[string]string{"a.txt": "base\n", ".gitignore": "cache/\n"} {
		if err := os.WriteFile(filepath.Join(folder, rel), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	// The copy backend on every machine: the test edits the source's copy from
	// outside the runner, which an overlay's mount (Linux) would not show, and
	// it checks a fork holds the checkpoint, not the files as they are.
	if err := os.WriteFile(filepath.Join(x.serverHome, "permissions.toml"), []byte("isolation = \"copy\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connectFake(t, x.serverHome, "claude")
	env := append(os.Environ(), "AGENTBUS_HOME="+x.serverHome)
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"}, env, reListening)
	x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
	if code, raw := x.get("/api/v0/settings/permissions"); code != 200 || !strings.Contains(string(raw), `"isolation":"copy"`) {
		t.Fatalf("settings: %d %s, want isolation copy", code, raw)
	}
	report := filepath.Join(t.TempDir(), "files.jsonl")
	x.setAgentEnv("AGENTBUS_FAKE_WRITE=made.txt=agent\n", "AGENTBUS_FAKE_FILES="+report,
		`AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"fork-test","result":"done"}`)
	runnerHome := t.TempDir()
	signInRunner(t, x.base, x.tok, runnerHome, "far")
	fakeDir := fakeAgentDir(t)
	runnerEnv := append(os.Environ(), "AGENTBUS_HOME="+runnerHome,
		"PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AGENTBUS_FAKE_AGENT=1", "ANTHROPIC_BASE_URL=", "OPENAI_BASE_URL=", "CODEX_HOME="+t.TempDir())
	runnerEnv = append(runnerEnv, x.agentEnv...)
	x.spawn(t, "runner:far", []string{"runner", "-server", x.base, "-name", "far", "-folder", folder, "-replay=false"}, runnerEnv, reRunnerReady)
	x.waitFolders(1)
	f := x.folders()[0]

	type workspace struct {
		Workspace string     `json:"workspace"`
		FolderRef string     `json:"folder_ref"`
		Closed    *time.Time `json:"closed"`
		From      *struct {
			Workspace  string `json:"workspace"`
			Checkpoint string `json:"checkpoint"`
		} `json:"from"`
	}
	readWS := func(id string) workspace {
		t.Helper()
		code, raw := x.get("/api/v0/workspaces/" + id)
		var w workspace
		if code != 200 || json.Unmarshal(raw, &w) != nil {
			t.Fatalf("workspace %s: %d %s", id, code, raw)
		}
		return w
	}
	turnIn := func(ws string) string {
		t.Helper()
		code, raw := x.post("/api/v0/sessions", map[string]any{"workspace": ws})
		var s struct {
			Session string `json:"session"`
		}
		if code != 201 || json.Unmarshal(raw, &s) != nil {
			t.Fatalf("session: %d %s", code, raw)
		}
		code, turn := x.startTurnX(map[string]any{"session": s.Session, "agent": "claude", "prompt": "edit"})
		if code/100 != 2 {
			t.Fatalf("turn: %d %+v", code, turn)
		}
		waitFor(t, "the turn to finish", func() bool {
			code, raw := x.get("/api/v0/turns/" + turn.Turn)
			var st struct {
				Turn struct {
					State string `json:"state"`
				} `json:"turn"`
			}
			if code != 200 || json.Unmarshal(raw, &st) != nil {
				return false
			}
			if st.Turn.State == "failed" {
				t.Fatalf("turn failed: %s", raw)
			}
			return st.Turn.State == "finished"
		})
		return turn.Turn
	}
	type seen struct {
		Cwd   string            `json:"cwd"`
		Files map[string]string `json:"files"`
	}
	reports := func() []seen {
		t.Helper()
		fh, err := os.Open(report)
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		var out []seen
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			var s seen
			if json.Unmarshal(sc.Bytes(), &s) == nil {
				out = append(out, s)
			}
		}
		return out
	}

	code, raw := x.post("/api/v0/workspaces", map[string]any{"machine": f.Machine, "folder_ref": f.Ref})
	var src workspace
	if code != 201 || json.Unmarshal(raw, &src) != nil {
		t.Fatalf("source: %d %s", code, raw)
	}
	t1 := turnIn(src.Workspace)
	srcCopy := reports()[0].Cwd
	// The source's own work goes on after its checkpoint t1: a tracked file
	// changes, and a build fills an ignored folder.
	for rel, c := range map[string]string{"a.txt": "late\n", "cache/obj": "o"} {
		p := filepath.Join(srcCopy, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code, raw = x.post("/api/v0/workspaces", map[string]any{"from": map[string]any{"workspace": src.Workspace, "checkpoint": t1}})
	var fork workspace
	if code != 201 || json.Unmarshal(raw, &fork) != nil || fork.From == nil || fork.From.Checkpoint != t1 || fork.FolderRef != f.Ref {
		t.Fatalf("fork: %d %s", code, raw)
	}
	ft := turnIn(fork.Workspace)
	got := reports()[1]
	if got.Cwd == srcCopy {
		t.Fatal("the fork's turn ran in the source's copy")
	}
	for rel, want := range map[string]string{"a.txt": "base\n", "made.txt": "agent\n", "cache/obj": "o"} {
		if got.Files[rel] != want {
			t.Errorf("the fork's turn found %s = %q, want %q (all: %v)", rel, got.Files[rel], want, got.Files)
		}
	}
	// Its changes start from the checkpoint, which already held made.txt.
	code, raw = x.get("/api/v0/turns/" + ft + "/changes")
	var ch struct {
		Count int `json:"count"`
	}
	if code != 200 || json.Unmarshal(raw, &ch) != nil || ch.Count != 0 {
		t.Fatalf("the fork turn's changes: %d %s, want none", code, raw)
	}
	if code, raw = x.post("/api/v0/workspaces/"+fork.Workspace+"/apply", map[string]any{}); code != 400 {
		t.Fatalf("apply of a fork: %d %s, want 400", code, raw)
	}
	// Its copy is put away once its turn ends; the source's stays as it was.
	for deadline := time.Now().Add(time.Minute); readWS(fork.Workspace).Closed == nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fork's copy was not put away within a minute of its turn's end")
		}
	}
	if _, err := os.Stat(got.Cwd); err == nil {
		t.Error("the fork's copy is still on disk after it was put away")
	}
	if readWS(src.Workspace).Closed != nil {
		t.Fatal("the source was put away")
	}
	if b, err := os.ReadFile(filepath.Join(srcCopy, "a.txt")); err != nil || !strings.HasPrefix(string(b), "late") {
		t.Fatalf("the source's copy changed: %q %v", b, err)
	}
}
