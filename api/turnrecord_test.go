package api

// GET /api/v0/turns/{id}/record: a turn's two halves in one read. The machine
// half's commands are derived from the turn's tool.requested (the command line)
// and tool.completed (the output), skipping tool.refused. Here without a runner
// (no queue), so there is no checkpoint — a direct-mode turn, which records
// commands only. The both-halves-with-a-file case is an integration test
// (TestTurnRecordBothHalves).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
)

func boolp(b bool) *bool { return &b }

// bashRequested is a Claude Code Bash tool call, as the gateway stored it.
func bashRequested(id, cmd string) event.Event {
	return event.Event{Kind: event.ToolRequested, Session: "s1", Turn: "t_1", Vendor: "anthropic",
		ToolID: id, ToolName: "Bash", ToolInput: json.RawMessage(`{"command":` + jsonStr(cmd) + `}`)}
}

func toolCompleted(id, out string, isErr bool) event.Event {
	return event.Event{Kind: event.ToolCompleted, Session: "s1", Turn: "t_1",
		ToolID: id, ToolOutput: out, ToolError: boolp(isErr)}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// A direct-mode turn (no isolated copy) reads back the commands its tool calls
// ran, with their output, and no checkpoint: files is empty.
func TestTurnRecordDirectCommandsNoCheckpoint(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Text: "go", Harness: "claude"},
		withID("evt_ta", withTime(now.Add(1*time.Second), bashRequested("toolu_a", "echo hi"))),
		withID("evt_ca", withTime(now.Add(2*time.Second), toolCompleted("toolu_a", "hi\n", false))),
		withID("evt_tb", withTime(now.Add(3*time.Second), bashRequested("toolu_b", "pwd"))),
		withID("evt_cb", withTime(now.Add(4*time.Second), toolCompleted("toolu_b", "/tmp\n", false))),
		event.Event{ID: "evt_n", Kind: event.NetworkRefused, Time: now.Add(5 * time.Second),
			Session: "s1", Turn: "t_1", Host: "example.com:443", Error: "not on the allow-list"},
	)

	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if rec.Agent != "claude" || rec.Turn != "t_1" {
		t.Fatalf("header: agent %q turn %q", rec.Agent, rec.Turn)
	}
	if len(rec.Files) != 0 {
		t.Fatalf("direct turn has no checkpoint; files %+v", rec.Files)
	}
	if len(rec.Commands) != 2 {
		t.Fatalf("commands %+v", rec.Commands)
	}
	a, b := rec.Commands[0], rec.Commands[1]
	if a.ID != "toolu_a" || a.Line != "echo hi" || a.Output != "hi\n" || a.Running {
		t.Fatalf("command a: %+v", a)
	}
	if b.ID != "toolu_b" || b.Line != "pwd" || b.Output != "/tmp\n" {
		t.Fatalf("command b: %+v", b)
	}
	if len(rec.Network) != 1 || rec.Network[0].Host != "example.com:443" {
		t.Fatalf("network %+v", rec.Network)
	}
}

// A tool call with no result yet reads as running.
func TestTurnRecordCommandRunning(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "claude"},
		withID("evt_ta", withTime(now.Add(time.Second), bashRequested("toolu_a", "sleep 100"))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 || !rec.Commands[0].Running || rec.Commands[0].Line != "sleep 100" {
		t.Fatalf("commands %+v", rec.Commands)
	}
}

// A tool call the gateway refused never ran, so it is not a command, whether
// the refusal is stored before or after its tool.requested.
func TestTurnRecordRefusedCommandLeftOut(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	refused := func(id string) event.Event {
		return event.Event{Kind: event.ToolRefused, Session: "s1", Turn: "t_1", ToolID: id, ToolName: "Bash", Rule: "git push"}
	}
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "claude"},
		// Refusal before its tool.requested (the gateway's usual order).
		withID("evt_r1", withTime(now.Add(1*time.Second), refused("toolu_push"))),
		withID("evt_t1", withTime(now.Add(2*time.Second), bashRequested("toolu_push", "git push"))),
		// Refusal after its tool.requested (the other order).
		withID("evt_t2", withTime(now.Add(3*time.Second), bashRequested("toolu_rm", "echo x"))),
		withID("evt_r2", withTime(now.Add(4*time.Second), refused("toolu_rm"))),
		// One that ran.
		withID("evt_t3", withTime(now.Add(5*time.Second), bashRequested("toolu_ok", "ls"))),
		withID("evt_c3", withTime(now.Add(6*time.Second), toolCompleted("toolu_ok", "a\nb\n", false))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 || rec.Commands[0].ID != "toolu_ok" {
		t.Fatalf("refused commands not left out: %+v", rec.Commands)
	}
}

// A command's output over the cap is kept to the cap, cut on a rune boundary,
// marked truncated, with the whole size in output_bytes.
func TestTurnRecordOutputCapped(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	// Fill to one byte before the cap, then a 3-byte rune straddling it.
	big := strings.Repeat("a", recordOutputCap-1) + "€" + strings.Repeat("b", 100)
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "claude"},
		withID("evt_ta", withTime(now.Add(time.Second), bashRequested("toolu_a", "cat big"))),
		withID("evt_ca", withTime(now.Add(2*time.Second), toolCompleted("toolu_a", big, false))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 {
		t.Fatalf("commands %+v", rec.Commands)
	}
	c := rec.Commands[0]
	if !c.Truncated || c.OutputBytes != int64(len(big)) {
		t.Fatalf("truncated=%v bytes=%d want %d", c.Truncated, c.OutputBytes, len(big))
	}
	if len(c.Output) > recordOutputCap {
		t.Fatalf("output %d over cap %d", len(c.Output), recordOutputCap)
	}
	if !utf8.ValidString(c.Output) {
		t.Fatalf("output is not valid utf-8 (rune split at the cap)")
	}
}

// agy (gemini) runs shell through run_command with a CommandLine argument; it
// reads back as a command too.
func TestTurnRecordAgyRunCommand(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "agy"},
		event.Event{ID: "evt_t", Kind: event.ToolRequested, Time: now.Add(time.Second), Session: "s1", Turn: "t_1",
			Vendor: "gemini", ToolID: "call_18", ToolName: "run_command", ToolInput: json.RawMessage(`{"CommandLine":"cat notes.txt"}`)},
		withID("evt_c", withTime(now.Add(2*time.Second), event.Event{Kind: event.ToolCompleted, Session: "s1", Turn: "t_1",
			ToolID: "call_18", ToolOutput: "the code is 7731\n", ToolError: boolp(false)})),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 || rec.Commands[0].Line != "cat notes.txt" || rec.Commands[0].Output != "the code is 7731\n" {
		t.Fatalf("agy command %+v", rec.Commands)
	}
}

// Replaying the runner's tool events — the same ids sent again, as a re-flush
// does — is stored once by AppendOnce, so the record shows one command.
func TestTurnRecordReplayNoDuplicate(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	if _, _, err := e.store.Append(event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now,
		Session: "s1", Turn: "t_1", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	batch := []event.Event{
		withID("evt_ta", withTime(now.Add(time.Second), bashRequested("toolu_a", "echo hi"))),
		withID("evt_ca", withTime(now.Add(2*time.Second), toolCompleted("toolu_a", "hi\n", false))),
	}
	for pass := 0; pass < 2; pass++ {
		for _, ev := range batch {
			stored, err := e.store.AppendOnce(ev)
			if err != nil {
				t.Fatal(err)
			}
			if pass == 0 && !stored {
				t.Fatalf("first delivery did not store %s", ev.ID)
			}
			if pass == 1 && stored {
				t.Fatalf("replay stored %s again", ev.ID)
			}
		}
	}

	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 || rec.Commands[0].ID != "toolu_a" || rec.Commands[0].Output != "hi\n" {
		t.Fatalf("replay duplicated the command: %+v", rec.Commands)
	}
}

// A result stored before its tool.requested (a fast in-process result can land
// at a lower seq than the asynchronously written request) still pairs: the
// command reads with its output, not stuck running.
func TestTurnRecordResultBeforeRequest(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "claude"},
		withID("evt_ca", withTime(now.Add(1*time.Second), toolCompleted("toolu_a", "hi\n", false))),
		withID("evt_ta", withTime(now.Add(2*time.Second), bashRequested("toolu_a", "echo hi"))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 || rec.Commands[0].Running || rec.Commands[0].Output != "hi\n" {
		t.Fatalf("out-of-order result not paired: %+v", rec.Commands)
	}
}

// Parallel run_command calls on the plain Gemini API share the tool name as
// their id (no id). Each pairs its own result in order, so no output is lost
// and none is stuck running.
func TestTurnRecordParallelSameID(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	run := func(cmd string) event.Event {
		return event.Event{Kind: event.ToolRequested, Session: "s1", Turn: "t_1", Vendor: "gemini",
			ToolID: "run_command", ToolName: "run_command", ToolInput: json.RawMessage(`{"CommandLine":` + jsonStr(cmd) + `}`)}
	}
	done := func(out string) event.Event {
		return event.Event{Kind: event.ToolCompleted, Session: "s1", Turn: "t_1", ToolID: "run_command",
			ToolOutput: out, ToolError: boolp(false)}
	}
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "agy"},
		withID("evt_t1", withTime(now.Add(1*time.Second), run("echo one"))),
		withID("evt_t2", withTime(now.Add(2*time.Second), run("echo two"))),
		withID("evt_c1", withTime(now.Add(3*time.Second), done("one\n"))),
		withID("evt_c2", withTime(now.Add(4*time.Second), done("two\n"))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 2 {
		t.Fatalf("commands %+v", rec.Commands)
	}
	if rec.Commands[0].Line != "echo one" || rec.Commands[0].Output != "one\n" || rec.Commands[0].Running {
		t.Fatalf("command 0: %+v", rec.Commands[0])
	}
	if rec.Commands[1].Line != "echo two" || rec.Commands[1].Output != "two\n" || rec.Commands[1].Running {
		t.Fatalf("command 1: %+v", rec.Commands[1])
	}
}

// A run_command call with no command (empty CommandLine) still reserves a FIFO
// slot, so its result pairs with it and does not shift onto the next real
// run_command. (Plain Gemini shares the name "run_command" as the id for all.)
func TestTurnRecordEmptyCommandDoesNotShift(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	run := func(cmdline string) event.Event {
		return event.Event{Kind: event.ToolRequested, Session: "s1", Turn: "t_1", Vendor: "gemini",
			ToolID: "run_command", ToolName: "run_command", ToolInput: json.RawMessage(`{"CommandLine":` + jsonStr(cmdline) + `}`)}
	}
	done := func(out string, isErr bool) event.Event {
		return event.Event{Kind: event.ToolCompleted, Session: "s1", Turn: "t_1", ToolID: "run_command",
			ToolOutput: out, ToolError: boolp(isErr)}
	}
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "agy"},
		withID("evt_t1", withTime(now.Add(1*time.Second), run(""))), // no command: a slot, not a command
		withID("evt_c1", withTime(now.Add(2*time.Second), done("error: empty command", true))),
		withID("evt_t2", withTime(now.Add(3*time.Second), run("echo real"))),
		withID("evt_c2", withTime(now.Add(4*time.Second), done("real\n", false))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 {
		t.Fatalf("commands %+v", rec.Commands)
	}
	c := rec.Commands[0]
	if c.Line != "echo real" || c.Output != "real\n" || c.Running {
		t.Fatalf("output shifted off the empty call: %+v", c)
	}
}

// A non-shell tool call (Read) interleaved with a shell command keeps its
// result off the command, and the command keeps its own.
func TestTurnRecordNonShellToolDoesNotPollute(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	read := event.Event{Kind: event.ToolRequested, Session: "s1", Turn: "t_1", Vendor: "anthropic",
		ToolID: "toolu_read", ToolName: "Read", ToolInput: json.RawMessage(`{"path":"a.txt"}`)}
	appendAll(t, e.store,
		event.Event{ID: "evt_s", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Harness: "claude"},
		withID("evt_tr", withTime(now.Add(1*time.Second), read)),
		withID("evt_cr", withTime(now.Add(2*time.Second), toolCompleted("toolu_read", "file body", false))),
		withID("evt_tb", withTime(now.Add(3*time.Second), bashRequested("toolu_b", "ls"))),
		withID("evt_cb", withTime(now.Add(4*time.Second), toolCompleted("toolu_b", "a\nb\n", false))),
	)
	var rec TurnRecord
	if getJSON(t, e, "/api/v0/turns/t_1/record", &rec) != http.StatusOK {
		t.Fatal("record not 200")
	}
	if len(rec.Commands) != 1 || rec.Commands[0].Line != "ls" || rec.Commands[0].Output != "a\nb\n" {
		t.Fatalf("non-shell output polluted the command: %+v", rec.Commands)
	}
}

func withID(id string, e event.Event) event.Event     { e.ID = id; return e }
func withTime(t time.Time, e event.Event) event.Event { e.Time = t; return e }
