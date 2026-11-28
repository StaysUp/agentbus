//go:build integration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// Close, rebuild, preview and apply through control and a separate runner.
// Files that are already in the rebuilt base must stay in the person's folder.
func TestXProcCloseRebuildPreservesBase(t *testing.T) {
	for _, prior := range []string{"apply", "update"} {
		t.Run(prior, func(t *testing.T) {
			folder := t.TempDir()
			if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// This fake only edits files. Disable replay because it has no model
			// conversation to continue; replay has separate process tests.
			x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
			connectFake(t, x.serverHome, "claude")
			env := append(os.Environ(), "AGENTBUS_HOME="+x.serverHome)
			addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"}, env, reListening)
			x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
			x.setAgentEnv("AGENTBUS_FAKE_WRITE=session.txt=session\n",
				`AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"close-test","result":"done"}`)
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
			code, raw := x.post("/api/v0/workspaces", map[string]any{"machine": f.Machine, "folder_ref": f.Ref})
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
			start := func() turnStart {
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
				return turn
			}
			start()
			if err := os.WriteFile(filepath.Join(folder, "kept.txt"), []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			base := "/api/v0/workspaces/" + ws.Workspace
			// Every workspace action answers at once, queued, and names its
			// outcome; the runner's work shows on the record as that event.
			if e := x.act(ws.Workspace, prior, map[string]any{}); e.Kind == event.WorkspaceFailed {
				t.Fatalf("prior %s: %+v", prior, e.Failed)
			}
			if e := x.act(ws.Workspace, "close", nil); e.Kind != event.WorkspaceClosed {
				t.Fatalf("close: %+v", e)
			}
			waitFor(t, "the workspace to read closed", func() bool {
				code, raw := x.get(base)
				var w struct {
					Closed *time.Time `json:"closed"`
				}
				return code == 200 && json.Unmarshal(raw, &w) == nil && w.Closed != nil
			})
			if got, err := os.ReadFile(filepath.Join(folder, "kept.txt")); err != nil || string(got) != "keep\n" {
				t.Fatalf("folder after close: %q %v", got, err)
			}
			start()
			if code, raw := x.get(base + "/changes?fresh=true"); code != 200 {
				t.Fatalf("preview: %d %s", code, raw)
			}
			if e := x.act(ws.Workspace, "apply", map[string]any{}); e.Applied == nil || e.Applied.Result != "applied" {
				t.Fatalf("apply: %+v", e)
			}
			for name, want := range map[string]string{"a.txt": "base\n", "kept.txt": "keep\n", "session.txt": "session\n"} {
				if got, err := os.ReadFile(filepath.Join(folder, name)); err != nil || string(got) != want {
					t.Fatalf("%s after apply: %q %v", name, got, err)
				}
			}
		})
	}
}
