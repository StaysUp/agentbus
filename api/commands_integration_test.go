//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// commandEnd waits for the command's command.ran or command.failed in st.
func commandEnd(t *testing.T, st *store.Store, session, id string) event.Event {
	t.Helper()
	var out event.Event
	waitUntil(t, "command "+id+" to end", func() bool {
		recs, _ := st.After(0, store.Filter{Session: session, Only: true, Kinds: []string{string(event.CommandRan), string(event.CommandFailed)}}, 1000)
		for _, r := range recs {
			var e event.Event
			if json.Unmarshal(r.Data, &e) == nil && e.Command != nil && e.Command.ID == id {
				out = e
				return true
			}
		}
		return false
	})
	return out
}

// On one machine, as in agentbus up, the runner's record is control's: the
// command.ran the engine stores and the one control keeps from the finish
// are one event. The route answers at once; a turn token cannot run a
// command; a command sent while a turn runs waits behind it in the lane.
func TestCommandOneMachine(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	// The agent keeps its token and its prompt, says its resume id, and
	// waits for release.
	script := `for a; do last=$a; done; printf %s "$AGENTBUS_TOKEN" > "$0/token"; printf %s "$last" > "$0/prompt"
echo '{"type":"result","session_id":"r1"}'
while [ ! -e "$0/release" ]; do sleep 0.05; done`
	e := stackEnv(t, stack{wire: func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, dir}}, nil
	}})
	folder := t.TempDir()
	resp, body := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"`+folder+`","prompt":"hi"}`)))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("turn: %d %s", resp.StatusCode, body)
	}
	var st Started
	json.Unmarshal([]byte(body), &st)
	command := func(tok, line string) (int, CommandQueued, string) {
		t.Helper()
		r := e.req(t, "POST", "/api/v0/sessions/"+st.Session+"/commands", strings.NewReader(`{"command":`+mustQuote(line)+`}`))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, body := do(t, r)
		var q CommandQueued
		json.Unmarshal([]byte(body), &q)
		return resp.StatusCode, q, body
	}
	// The first turn has not ended: the session has no copy yet.
	if code, _, body := command("", "ls"); code != http.StatusConflict || !strings.Contains(body, commandNoCopy) {
		t.Fatalf("before the first turn ended: %d %s", code, body)
	}
	os.WriteFile(release, nil, 0o600)
	waitEnded(t, e, st.Turn)
	os.Remove(release)

	code, q, body := command("", "pwd; echo x > made.txt")
	if code != http.StatusAccepted || q.State != "queued" || !strings.HasPrefix(q.Command, "cmd_") || q.Session != st.Session {
		t.Fatalf("command: %d %s", code, body)
	}
	ran := commandEnd(t, e.store, st.Session, q.Command)
	if ran.Kind != event.CommandRan || ran.Command.ExitCode != 0 {
		t.Fatalf("command: %+v", ran)
	}
	cp := strings.TrimSpace(ran.Command.Stdout)
	if r1, r2 := realPath(t, cp), realPath(t, folder); r1 == r2 || !strings.HasPrefix(r1, realPath(t, e.workdir)) {
		t.Fatalf("it ran in %s, not in the session's copy", cp)
	}
	if _, err := os.Stat(filepath.Join(folder, "made.txt")); err == nil {
		t.Fatal("the command wrote to the person's folder")
	}
	// queued, started, ran: once each.
	recs, _ := e.store.After(0, store.Filter{Session: st.Session, Only: true}, 1000)
	kinds := map[event.Kind]int{}
	for _, r := range recs {
		kinds[r.Kind]++
	}
	if kinds[event.CommandQueued] != 1 || kinds[event.CommandStarted] != 1 || kinds[event.CommandRan] != 1 {
		t.Fatalf("command events: %v", kinds)
	}

	// The next turn runs; a command sent meanwhile waits behind it, and its
	// turn token cannot run one at all.
	resp, body = do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"session":"`+st.Session+`","prompt":"next"}`)))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("next turn: %d %s", resp.StatusCode, body)
	}
	var next Started
	json.Unmarshal([]byte(body), &next)
	var tok string
	waitUntil(t, "the next turn's token", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "prompt"))
		t, _ := os.ReadFile(filepath.Join(dir, "token"))
		tok = string(t)
		return strings.HasSuffix(string(b), "next") && tok != ""
	})
	if code, _, body := command(tok, "ls"); code != http.StatusForbidden {
		t.Fatalf("a turn token: %d %s", code, body)
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt"))
	if strings.Count(string(prompt), "<shell-command>pwd; echo x > made.txt</shell-command>") != 1 {
		t.Fatalf("the next turn's prompt:\n%s", prompt)
	}
	code, q, body = command("", "echo after")
	if code != http.StatusAccepted {
		t.Fatalf("a command while a turn runs: %d %s", code, body)
	}
	time.Sleep(300 * time.Millisecond)
	recs, _ = e.store.After(0, store.Filter{Session: st.Session, Only: true, Kinds: []string{string(event.CommandStarted)}}, 1000)
	if len(recs) != 1 {
		t.Fatalf("the command started beside the turn: %d started", len(recs))
	}
	os.WriteFile(release, nil, 0o600)
	if j := waitEnded(t, e, next.Turn); j.State != queue.Finished {
		t.Fatalf("next turn's job: %s %s", j.State, j.Reason)
	}
	if ran := commandEnd(t, e.store, st.Session, q.Command); ran.Kind != event.CommandRan || ran.Command.Stdout != "after\n" {
		t.Fatalf("the queued command: %+v", ran)
	}
}

// A command whose runner stops answering while it runs ends as
// command.failed once its lease runs out: control's sweep, then
// CloseCommands, as the service's own loop runs them.
func TestCommandRunnerLost(t *testing.T) {
	e := stackEnv(t, stack{wire: func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
	}})
	far, err := e.q.RegisterRunner(queue.DefaultTeam, "far", []string{"far-only"}, 1, queue.HashToken("far-token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Folder: "/f", Prompt: "hi",
		Labels: []string{"far-only"}}, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	_, att, ok, _, err := e.q.Take(far.ID, time.Minute)
	if err != nil || !ok || att.Job != first.ID {
		t.Fatalf("take: %v %v", ok, err)
	}
	e.q.Start(far.ID, att.ID, 1, time.Minute)
	if _, err := e.q.End(far.ID, att.ID, 1, queue.Finish{Status: queue.Finished, Session: first.Session,
		Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: "sp1"}}); err != nil {
		t.Fatal(err)
	}
	e.store.Append(event.Event{ID: "evt_open", Kind: event.SessionOpened, Time: time.Now().UTC(), Session: first.Session, Harness: "claude", Folder: "/f"})
	resp, body := do(t, e.req(t, "POST", "/api/v0/sessions/"+first.Session+"/commands", strings.NewReader(`{"command":"ls"}`)))
	var q CommandQueued
	if json.Unmarshal([]byte(body), &q); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("command: %d %s", resp.StatusCode, body)
	}
	_, ca, ok, _, _ := e.q.Take(far.ID, 200*time.Millisecond)
	if !ok {
		t.Fatal("far did not take the command")
	}
	e.q.Start(far.ID, ca.ID, ca.Number, 200*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	if _, err := e.q.Sweep(); err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.CloseCommands(e.q, e.store); err != nil || n != 1 {
		t.Fatalf("closed %d: %v", n, err)
	}
	if n, _ := jobs.CloseCommands(e.q, e.store); n != 0 {
		t.Fatalf("closed again: %d", n)
	}
	failed := commandEnd(t, e.store, first.Session, q.Command)
	if failed.Kind != event.CommandFailed || failed.Error != "the session's runner stopped answering while the command ran" {
		t.Fatalf("lost command: %+v %q", failed.Kind, failed.Error)
	}
}

func mustQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return r
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
