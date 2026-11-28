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

// Apply, update and discard answer at once through control, while a separate
// runner process does the work (async by default, his rule 2026-11-19, as
// Close since 6bce6a8). What a waiting reply used to carry reaches the caller
// through the record: a refused apply's files with their paths and why, an
// update's result, a discard's dropped turns, and a discard the runner could
// not do, as workspace.failed with why.
func TestXProcWorkspaceActionsAnswerAtOnce(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	connectFake(t, x.serverHome, "claude")
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"},
		append(os.Environ(), "AGENTBUS_HOME="+x.serverHome), reListening)
	x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
	x.setAgentEnv("AGENTBUS_FAKE_WRITE=a.txt=session\n",
		`AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"actions","result":"done"}`)
	rn := x.startCopyRunner(t, "far", folder)
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
	run := func() string {
		t.Helper()
		code, turn := x.startTurnX(map[string]any{"session": session.Session, "agent": "claude", "prompt": "edit"})
		if code/100 != 2 {
			t.Fatalf("turn: %d %+v", code, turn)
		}
		if !waitLong(30*time.Second, func() bool { return x.turnState(turn.Turn) == "finished" }) {
			t.Fatalf("the turn reads %q", x.turnState(turn.Turn))
		}
		return turn.Turn
	}
	first := run()

	// The person edits a.txt too: the apply is refused. The reply came before
	// the runner looked; the refusal, with the file and why, is on the record.
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ap := x.act(ws.Workspace, "apply", map[string]any{})
	if ap.Kind != event.WorkspaceApplied || ap.Applied == nil || ap.Applied.Result != "refused" ||
		len(ap.Applied.Conflicts) != 1 || ap.Applied.Conflicts[0].Path != "a.txt" ||
		ap.Applied.Conflicts[0].Why != "changed in your folder since the checkpoint" || ap.Applied.Error == "" {
		t.Fatalf("refused apply: %+v %+v", ap, ap.Applied)
	}
	if got, _ := os.ReadFile(filepath.Join(folder, "a.txt")); string(got) != "mine\n" {
		t.Fatalf("a refused apply wrote a.txt: %q", got)
	}

	// The update's conflict is on the record, with its file.
	up := x.act(ws.Workspace, "update", map[string]any{})
	if up.Kind != event.WorkspaceUpdated || up.Updated == nil || up.Updated.Result != "conflict" ||
		len(up.Updated.Conflicts) != 1 || up.Updated.Conflicts[0] != "a.txt" {
		t.Fatalf("update: %+v %+v", up, up.Updated)
	}

	// The discard's dropped turns are on the record.
	d := x.act(ws.Workspace, "discard", nil)
	if d.Kind != event.WorkspaceDiscarded || d.Discarded == nil || len(d.Discarded.DroppedTurns) != 1 || d.Discarded.DroppedTurns[0] != first {
		t.Fatalf("discard: %+v %+v", d, d.Discarded)
	}
	// The discard replaced the update as last_action, yet the update's caller
	// still reads its own outcome by its event (Codex review t_c0aec2284cfc9c6f,
	// P2: a caller polling last_action could miss it).
	var w struct {
		LastAction *event.Event `json:"last_action"`
	}
	if code, raw := x.get("/api/v0/workspaces/" + ws.Workspace); code != 200 || json.Unmarshal(raw, &w) != nil || w.LastAction == nil || w.LastAction.ID != d.ID {
		t.Fatalf("last_action after the discard: %d %s", code, raw)
	}
	var again event.Event
	if code, raw := x.get("/api/v0/workspaces/" + ws.Workspace + "/outcome?event=" + up.ID); code != 200 || json.Unmarshal(raw, &again) != nil ||
		again.Kind != event.WorkspaceUpdated || again.ID != up.ID {
		t.Fatalf("the update's outcome after a later discard: %d %s", code, raw)
	}
	if code, _ := x.get("/api/v0/workspaces/" + ws.Workspace + "/outcome?event=evt_wd_job_0000000000000000"); code != 404 {
		t.Fatalf("an outcome not on the record: %d, want 404", code)
	}

	// A discard the runner cannot do: its copy is gone from its disk. Before,
	// a discard that failed recorded nothing; with the reply sent at once,
	// nothing would have said why.
	run()
	if err := os.RemoveAll(filepath.Join(rn.copies(), ws.Workspace)); err != nil {
		t.Fatal(err)
	}
	failed := x.act(ws.Workspace, "discard", nil)
	if failed.Kind != event.WorkspaceFailed || failed.Failed == nil || failed.Failed.Action != "discard" ||
		failed.Failed.Error == "" || failed.Failed.By == nil || failed.Failed.By.Kind != "person" {
		t.Fatalf("failed discard: %+v %+v", failed, failed.Failed)
	}
}
