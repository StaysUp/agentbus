//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
)

// A sandbox turn that ran commands and changed a file reads back both halves in
// one call: the files it changed (an isolated checkpoint), the commands it ran
// with their output (from its tool calls), and the network it was refused,
// beside the agent half's model calls. The file half is the turn's real
// changeset; the tool and network events are the record as the gateway writes
// them, joined to the turn by its id.
func TestTurnRecordBothHalves(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	e, _ := diffEnv(t, perm)
	folder := t.TempDir()
	for p, c := range map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n"} {
		if err := os.WriteFile(filepath.Join(folder, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	turn := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"edit1"}`)
	waitEnded(t, e, turn.Turn)

	// The machine half's tool calls and a network refusal, as the gateway
	// records them, joined to the turn by its id.
	now := time.Now().UTC()
	ev := func(id string, at time.Duration, e event.Event) event.Event {
		e.ID, e.Time, e.Session, e.Turn = id, now.Add(at), turn.Session, turn.Turn
		return e
	}
	bash := func(id, cmd string) event.Event {
		return event.Event{Kind: event.ToolRequested, Vendor: "anthropic", ToolID: id, ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":` + jsonStr(cmd) + `}`)}
	}
	done := func(id, out string) event.Event {
		return event.Event{Kind: event.ToolCompleted, ToolID: id, ToolOutput: out, ToolError: boolp(false)}
	}
	appendAll(t, e.store,
		ev("evt_ta", 1*time.Second, bash("toolu_a", "echo hi")),
		ev("evt_ca", 2*time.Second, done("toolu_a", "hi\n")),
		ev("evt_tb", 3*time.Second, bash("toolu_b", "cat a.txt")),
		ev("evt_cb", 4*time.Second, done("toolu_b", "one\nTWO\n")),
		ev("evt_net", 5*time.Second, event.Event{Kind: event.NetworkRefused, Host: "example.com:443", Error: "not on the allow-list"}),
	)

	code, out := as(t, e, token, "GET", "/api/v0/turns/"+turn.Turn+"/record", "")
	if code != 200 {
		t.Fatalf("record: %d %s", code, out)
	}
	var rec TurnRecord
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	// Machine half, files first: the turn's real changeset (3 files, isolated).
	if len(rec.Files) != 3 {
		t.Fatalf("files %+v", rec.Files)
	}
	paths := map[string]bool{}
	for _, f := range rec.Files {
		paths[f.Path] = true
		if f.Diff != nil || f.Content != nil {
			t.Errorf("record should not embed per-file diffs: %s", f.Path)
		}
	}
	for _, p := range []string{"a.txt", "b.txt", "gone.txt"} {
		if !paths[p] {
			t.Errorf("changeset missing %s", p)
		}
	}
	// Then the commands it ran, with their output.
	if len(rec.Commands) != 2 || rec.Commands[0].Line != "echo hi" || rec.Commands[0].Output != "hi\n" ||
		rec.Commands[1].Line != "cat a.txt" || rec.Commands[1].Output != "one\nTWO\n" {
		t.Fatalf("commands %+v", rec.Commands)
	}
	// Then the network it was refused.
	if len(rec.Network) != 1 || rec.Network[0].Host != "example.com:443" {
		t.Fatalf("network %+v", rec.Network)
	}
	// And the agent half: its model calls and state.
	if rec.State != "finished" || rec.Agent != "claude" {
		t.Fatalf("header: state %q agent %q", rec.State, rec.Agent)
	}
}

// While the turn's job has not ended, the record reads state running with empty
// files — never finished with empty files, which would contradict /diff and
// /turns/{id} while the runner is still checkpointing.
func TestTurnRecordRunningState(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	e, dir := diffEnv(t, perm)
	turn := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"hold"}`)
	tokenOf(t, dir, "hold") // the turn is running, paused in its copy
	defer release(dir, "hold")

	code, out := as(t, e, token, "GET", "/api/v0/turns/"+turn.Turn+"/record", "")
	if code != 200 {
		t.Fatalf("record: %d %s", code, out)
	}
	var rec TurnRecord
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.State != "running" {
		t.Fatalf("state %q, want running", rec.State)
	}
	if len(rec.Files) != 0 {
		t.Fatalf("a running turn has no checkpoint yet; files %+v", rec.Files)
	}
}
