//go:build integration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A copy whose work is applied is put away by itself, through control and a
// separate runner: control's built-in on queues the put-away when the apply is
// recorded, it comes due after the quiet time ([workspaces]
// put_away_after_minutes, 1 here), control reads the record and lets the
// runner holding the copy remove it, and the record says so. The person's
// folder keeps the applied work, and the next turn makes a fresh copy.
func TestXProcSettledCopyIsPutAway(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	// The quiet time is the person's setting, imported from the settings file
	// at control's first start.
	if err := os.WriteFile(filepath.Join(x.serverHome, "permissions.toml"), []byte("[workspaces]\nput_away_after_minutes = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connectFake(t, x.serverHome, "claude")
	env := append(os.Environ(), "AGENTBUS_HOME="+x.serverHome)
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"}, env, reListening)
	x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
	x.setAgentEnv("AGENTBUS_FAKE_WRITE=session.txt=session\n",
		`AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"putaway-test","result":"done"}`)
	code, raw := x.get("/api/v0/settings/permissions")
	var p struct {
		Workspaces struct {
			Minutes int `json:"put_away_after_minutes"`
		} `json:"workspaces"`
	}
	if code != 200 || json.Unmarshal(raw, &p) != nil || p.Workspaces.Minutes != 1 {
		t.Fatalf("settings: %d %s", code, raw)
	}
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
	code, raw = x.post("/api/v0/workspaces", map[string]any{"machine": f.Machine, "folder_ref": f.Ref})
	var ws struct {
		Workspace string `json:"workspace"`
	}
	if code != 201 || json.Unmarshal(raw, &ws) != nil {
		t.Fatalf("workspace: %d %s", code, raw)
	}
	code, raw = x.post("/api/v0/sessions", map[string]any{"workspace": ws.Workspace})
	var session struct {
		Session string `json:"session"`
	}
	if code != 201 || json.Unmarshal(raw, &session) != nil {
		t.Fatalf("session: %d %s", code, raw)
	}
	start := func() {
		t.Helper()
		code, turn := x.startTurnX(map[string]any{"session": session.Session, "agent": "claude", "prompt": "edit"})
		if code/100 != 2 {
			t.Fatalf("turn: %d %+v", code, turn)
		}
		waitFor(t, "the turn to finish", func() bool {
			code, raw := x.get("/api/v0/turns/" + turn.Turn)
			var state struct {
				Turn struct {
					State string `json:"state"`
				} `json:"turn"`
			}
			if code != 200 || json.Unmarshal(raw, &state) != nil {
				return false
			}
			if state.Turn.State == "failed" {
				t.Fatalf("turn failed: %s", raw)
			}
			return state.Turn.State == "finished"
		})
	}
	closed := func() bool {
		code, raw := x.get("/api/v0/workspaces/" + ws.Workspace)
		var w struct {
			Closed *time.Time `json:"closed"`
		}
		if code != 200 || json.Unmarshal(raw, &w) != nil {
			t.Fatalf("workspace: %d %s", code, raw)
		}
		return w.Closed != nil
	}
	start()
	base := "/api/v0/workspaces/" + ws.Workspace
	if code, raw := x.post(base+"/apply", map[string]any{}); code != 200 {
		t.Fatalf("apply: %d %s", code, raw)
	}
	if closed() {
		t.Fatal("put away before its quiet time")
	}
	for deadline := time.Now().Add(2 * time.Minute); !closed(); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("the settled copy was not put away within two minutes")
		}
	}
	if got, err := os.ReadFile(filepath.Join(folder, "session.txt")); err != nil || string(got) != "session\n" {
		t.Fatalf("folder after the put-away: %q %v", got, err)
	}
	start()
	if closed() {
		t.Fatal("the next turn did not make a fresh copy")
	}
}
