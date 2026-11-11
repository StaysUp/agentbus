//go:build integration

package main

// The person's "!" command (POST /api/v0/sessions/{id}/commands) on a runner
// that holds no secret, in another process: it runs in the session's copy
// and its home, with no AgentBus token, both records hold its command.ran,
// and the session's next turn is told of it once.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// runCommand sends a command. A queued one (202) is waited for: the answer
// is its end in the server's record, command.ran or command.failed. Any
// other status answers its error.
func runCommand(t *testing.T, svc *service, tok, session string, body map[string]any) (int, event.Event, string) {
	t.Helper()
	code, out := sendAPI(t, svc, tok, "POST", "/api/v0/sessions/"+session+"/commands", body)
	msg, _ := out["error"].(string)
	if code != http.StatusAccepted {
		return code, event.Event{}, msg
	}
	id, _ := out["command"].(string)
	if out["state"] != "queued" || !strings.HasPrefix(id, "cmd_") || out["session"] != session {
		t.Fatalf("queued: %v", out)
	}
	return code, commandEnd(t, svc.store, session, id, 30*time.Second), ""
}

// commandEnd waits for the command's command.ran or command.failed in st.
func commandEnd(t *testing.T, st *store.Store, session, id string, wait time.Duration) event.Event {
	t.Helper()
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		recs, _ := st.After(0, store.Filter{Session: session, Only: true, Kinds: []string{string(event.CommandRan), string(event.CommandFailed)}}, 1000)
		for _, r := range recs {
			var e event.Event
			if json.Unmarshal(r.Data, &e) == nil && e.Command != nil && e.Command.ID == id {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("command %s did not end", id)
		}
	}
}

// commandsIn are the command lines of the session's command.ran events in st.
func commandsIn(t *testing.T, st *store.Store, session string) []string {
	t.Helper()
	recs, err := st.After(0, store.Filter{Session: session, Only: true}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		var e event.Event
		if r.Kind == event.CommandRan && json.Unmarshal(r.Data, &e) == nil && e.Command != nil {
			out = append(out, e.Command.Line)
		}
	}
	return out
}

func TestCommandOnRunner(t *testing.T) {
	ns := startNoSecrets(t)
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-1"}`+"\n")
	code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	first := waitJob(t, ns.server, ns.tok, queueTurn(t, ns.server, ns.tok,
		map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "isolate": true, "prompt": "hello"}).ID)
	if first.State != queue.Finished {
		t.Fatalf("first turn: %s %s %s", first.State, first.Reason, first.Result)
	}
	session := first.Session
	copyDir := reportOf(t, ns.report).Cwd
	person, ok := ns.runner.folders.Path(ns.ref)
	if !ok || mustReal(t, person) == mustReal(t, copyDir) {
		t.Fatalf("the turn ran in %s, the person's folder is %s", copyDir, person)
	}

	// It runs in the session's copy and home, with no AgentBus variable.
	code, e, msg := runCommand(t, ns.server, ns.tok, session, map[string]any{
		"command": `echo hi; echo oops >&2; pwd; echo "home=$HOME"; echo "token=$AGENTBUS_TOKEN"; echo made > made.txt; exit 3`})
	if code != http.StatusAccepted || e.Kind != event.CommandRan || e.Command == nil || e.Session != session || e.Turn != "" {
		t.Fatalf("command: %d %s %+v", code, msg, e)
	}
	c := e.Command
	lines := strings.Split(strings.TrimSpace(c.Stdout), "\n")
	if c.ExitCode != 3 || c.TimedOut || c.Truncated || c.Stderr != "oops\n" || len(lines) != 4 || lines[0] != "hi" {
		t.Fatalf("command ran: %+v", c)
	}
	if real, _ := filepath.EvalSymlinks(lines[1]); real != mustReal(t, copyDir) {
		t.Fatalf("it ran in %s, the copy is %s", lines[1], copyDir)
	}
	if want := "home=" + filepath.Join(ns.runnerHome, homesDir, session); lines[2] != want {
		t.Fatalf("its home: %q, want %q", lines[2], want)
	}
	if lines[3] != "token=" {
		t.Fatalf("it got an AgentBus token: %q", lines[3])
	}
	if c.OutputBytes != int64(len(c.Stdout)+len(c.Stderr)) {
		t.Fatalf("output_bytes %d", c.OutputBytes)
	}
	// The copy changed; the person's folder did not.
	if _, err := os.Stat(filepath.Join(copyDir, "made.txt")); err != nil {
		t.Fatalf("the copy has no made.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(person, "made.txt")); err == nil {
		t.Fatal("the command wrote to the person's folder")
	}
	// Both records hold it, once.
	waitFor(t, "the command in both records", func() bool {
		return len(commandsIn(t, ns.server.store, session)) == 1 && len(commandsIn(t, ns.runner.store, session)) == 1
	})

	// The time limit ends the command and what it started.
	begun := time.Now()
	code, e, msg = runCommand(t, ns.server, ns.tok, session, map[string]any{"command": "sleep 30 & sleep 30", "timeout_seconds": 1})
	if code != http.StatusAccepted || e.Command == nil || !e.Command.TimedOut || e.Command.ExitCode != 128+9 {
		t.Fatalf("timed command: %d %s %+v", code, msg, e.Command)
	}
	if d := time.Since(begun); d > 10*time.Second {
		t.Fatalf("the time limit took %s", d)
	}

	// The output cap keeps the start of each stream and counts it all.
	code, e, msg = runCommand(t, ns.server, ns.tok, session, map[string]any{"command": "head -c 200000 /dev/zero | tr '\\0' a"})
	if code != http.StatusAccepted || e.Command == nil || len(e.Command.Stdout) != 64<<10 || !e.Command.Truncated || e.Command.OutputBytes != 200000 {
		t.Fatalf("capped command: %d %s len %d %+v", code, msg, len(e.Command.Stdout), e.Command.Truncated)
	}

	// A cancel of the running command's job ends it and what it started;
	// the job ends cancelled, and the record keeps what ran.
	type answer struct {
		code int
		e    event.Event
	}
	got := make(chan answer, 1)
	go func() {
		code, e, _ := runCommand(t, ns.server, ns.tok, session, map[string]any{"command": "sleep 30 & sleep 30"})
		got <- answer{code, e}
	}()
	var running queue.Job
	waitFor(t, "the command running", func() bool {
		js, _ := ns.server.queue.List(queue.DefaultTeam, queue.Filter{Session: session, Kind: queue.KindCommand, Open: true}, 10)
		if len(js) == 1 && js[0].State == queue.Running {
			running = js[0]
			return true
		}
		return false
	})
	begun = time.Now()
	if _, err := ns.server.queue.Cancel(queue.DefaultTeam, running.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.code != http.StatusAccepted || a.e.Kind != event.CommandRan || a.e.Command.ExitCode != 128+9 || a.e.Command.TimedOut {
			t.Fatalf("cancelled command: %d %+v", a.code, a.e.Command)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the cancelled command did not end")
	}
	if d := time.Since(begun); d > 10*time.Second {
		t.Fatalf("the cancel took %s", d)
	}
	if j, _ := ns.server.queue.Get(queue.DefaultTeam, running.ID); j.State != queue.Cancelled {
		t.Fatalf("the cancelled command's job: %s", j.State)
	}

	// Refusals: a bad body, a turn token, an unknown session.
	if code, _, _ := runCommand(t, ns.server, ns.tok, session, map[string]any{"command": "  "}); code != http.StatusBadRequest {
		t.Fatalf("a blank command: %d", code)
	}
	if code, _, _ := runCommand(t, ns.server, ns.tok, "nosuchsession", map[string]any{"command": "ls"}); code != http.StatusNotFound {
		t.Fatalf("an unknown session: %d", code)
	}

	// A client reads them back by kind.
	code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/sessions/"+session+"/events?kind=command.ran,command.queued")
	var page struct{ Events []event.Event }
	if json.Unmarshal([]byte(raw), &page); code != 200 || len(page.Events) != 8 || page.Events[0].Kind != event.CommandQueued ||
		page.Events[1].Command.ExitCode != 3 {
		t.Fatalf("events by kind: %d %d", code, len(page.Events))
	}
	if code, _ := getAPI(t, ns.server, ns.tok, "/api/v0/sessions/"+session+"/events?kind=no%20such"); code != http.StatusBadRequest {
		t.Fatalf("a bad kind: %d", code)
	}

	// The next turn is told of the commands, once, before the person's words.
	next := waitJob(t, ns.server, ns.tok, queueTurn(t, ns.server, ns.tok, map[string]any{"session": session, "prompt": "what did I run?"}).ID)
	if next.State != queue.Finished {
		t.Fatalf("next turn: %s %s %s", next.State, next.Reason, next.Result)
	}
	prompt := lastArg(t, ns.report)
	if strings.Count(prompt, "<shell-command>") != 4 || strings.Count(prompt, "<shell-command>echo hi;") != 1 ||
		!strings.Contains(prompt, "<shell-stderr>\noops\n</shell-stderr>") || !strings.Contains(prompt, "ended at its time limit") ||
		!strings.HasSuffix(prompt, "what did I run?") {
		t.Fatalf("the next turn's prompt:\n%s", prompt)
	}
	// The record says what it was given: context.added of kind commands.
	waitFor(t, "context.added with the commands", func() bool {
		recs, _ := ns.server.store.After(0, store.Filter{Turn: next.Turn}, 1000)
		for _, r := range recs {
			var ev event.Event
			if r.Kind == event.ContextAdded && json.Unmarshal(r.Data, &ev) == nil {
				for _, a := range ev.Added {
					if a.Kind == event.AddedCommands && strings.Contains(prompt, a.Text) && a.Bytes == len(a.Text) {
						return true
					}
				}
			}
		}
		return false
	})
	// The turn after it is not told again.
	third := waitJob(t, ns.server, ns.tok, queueTurn(t, ns.server, ns.tok, map[string]any{"session": session, "prompt": "and now?"}).ID)
	if third.State != queue.Finished {
		t.Fatalf("third turn: %s %s %s", third.State, third.Reason, third.Result)
	}
	if p := lastArg(t, ns.report); strings.Contains(p, "<shell-command>") {
		t.Fatalf("the third turn was told again:\n%s", p)
	}

	// While a turn runs, a command waits in the session's lane behind it,
	// and runs once the turn has ended.
	t.Setenv("AGENTBUS_FAKE_WAIT", ns.release)
	busy := queueTurn(t, ns.server, ns.tok, map[string]any{"session": session, "prompt": "wait"})
	waitFor(t, "the turn running", func() bool {
		j, _ := ns.server.queue.Get(queue.DefaultTeam, busy.ID)
		return j.State == queue.Running
	})
	code, out := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/sessions/"+session+"/commands", map[string]any{"command": "echo after"})
	if code != http.StatusAccepted || out["state"] != "queued" {
		t.Fatalf("a command while a turn runs: %d %v", code, out)
	}
	id, _ := out["command"].(string)
	time.Sleep(500 * time.Millisecond)
	if js, _ := ns.server.queue.List(queue.DefaultTeam, queue.Filter{Session: session, Kind: queue.KindCommand, Open: true}, 10); len(js) != 1 ||
		js[0].State != queue.Queued {
		t.Fatalf("the command did not wait for the turn: %+v", js)
	}
	os.WriteFile(ns.release, nil, 0o600)
	if e := commandEnd(t, ns.server.store, session, id, 30*time.Second); e.Kind != event.CommandRan || e.Command.Stdout != "after\n" {
		t.Fatalf("the queued command: %+v", e)
	}
	if j := waitJob(t, ns.server, ns.tok, busy.ID); j.State != queue.Finished {
		t.Fatalf("the turn: %s", j.State)
	}

	// A closed session refuses.
	if code, out := sendAPI(t, ns.server, ns.tok, "PATCH", "/api/v0/sessions/"+session, map[string]any{"closed": true}); code != 200 {
		t.Fatalf("close: %d %v", code, out)
	}
	if code, _, msg := runCommand(t, ns.server, ns.tok, session, map[string]any{"command": "ls"}); code != http.StatusConflict ||
		msg != "this session is closed; it runs no more commands" {
		t.Fatalf("a command in a closed session: %d %q", code, msg)
	}
}

func reportOf(t *testing.T, report string) fakeReport {
	t.Helper()
	var rep fakeReport
	b, err := os.ReadFile(report)
	if err != nil || json.Unmarshal(b, &rep) != nil {
		t.Fatalf("no report: %v", err)
	}
	return rep
}

// lastArg is the last argument the fake agent got: its prompt.
func lastArg(t *testing.T, report string) string {
	t.Helper()
	rep := reportOf(t, report)
	if len(rep.Args) == 0 {
		t.Fatal("the agent got no arguments")
	}
	return rep.Args[len(rep.Args)-1]
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Under contain = "on" a command runs closed off, as its session's turns
// do: in a network namespace with loopback only (its /proc/net/dev; sysfs
// still shows the runner's), given the runner's proxy.
func TestCommandClosedOff(t *testing.T) {
	if !netjail.Supported {
		t.Skip("network control is Linux-only")
	}
	if err := netjail.Check(); err != nil {
		t.Skipf("this machine cannot close an agent off: %v", err)
	}
	ns := startNoSecrets(t)
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-1"}`+"\n")
	if err := os.WriteFile(filepath.Join(ns.serverHome, "permissions.toml"), []byte("[network]\ncontain = \"on\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	first := waitJob(t, ns.server, ns.tok, queueTurn(t, ns.server, ns.tok,
		map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "isolate": true, "prompt": "hello"}).ID)
	if first.State != queue.Finished {
		t.Fatalf("first turn: %s %s %s", first.State, first.Reason, first.Result)
	}
	code, e, msg := runCommand(t, ns.server, ns.tok, first.Session, map[string]any{"command": `tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' '; echo "proxy=$HTTPS_PROXY"`})
	if code != http.StatusAccepted || e.Kind != event.CommandRan || e.Command.ExitCode != 0 {
		t.Fatalf("command: %d %s %+v", code, msg, e)
	}
	lines := strings.Split(strings.TrimSpace(e.Command.Stdout), "\n")
	if len(lines) != 2 || lines[0] != "lo" || !strings.HasPrefix(lines[1], "proxy=http://127.0.0.1:") {
		t.Fatalf("the command's network: %q", e.Command.Stdout)
	}
}
