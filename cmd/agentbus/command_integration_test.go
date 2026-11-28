//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// commandEnv is control (a server) and a runner started on its own, which
// reaches control only over HTTP, as a runner on another machine does, with
// one folder the runner holds. The fake agent is claude.
type commandEnv struct {
	server, runner *service
	tok            string
	folder         string
	ref, machine   string
}

func newCommandEnv(t *testing.T) *commandEnv {
	t.Helper()
	return newCommandEnvVia(t, 0)
}

// newCommandEnvVia is newCommandEnv with the runner reaching control through a
// proxy that holds each record it sends (POST .../record) for slow first, as a
// slow link to another machine does. Everything else passes at once.
func newCommandEnvVia(t *testing.T, slow time.Duration) *commandEnv {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return newCommandEnvOf(t, slow, self, nil)
}

// newCommandEnvOf is newCommandEnvVia whose runner starts program for every
// agent. With pass, the proxy hands the nth record the runner sends (from 1)
// to control only when pass(n) says so, and answers 503 to the others, as a
// link that fails part way.
func newCommandEnvOf(t *testing.T, slow time.Duration, program string, pass func(n int) bool) *commandEnv {
	t.Helper()
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	// The vendor's session id, as Claude Code prints it, so the session's next
	// turn (the delivered result, the person's own) continues it.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"command-test"}`+"\n")
	serverHome, runnerHome := t.TempDir(), t.TempDir()
	connectFake(t, serverHome)
	server, err := start(config{mode: modeServer, listen: "127.0.0.1:0", home: serverHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, heartbeat: 100 * time.Millisecond, turnWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.stop)
	tok := readToken(t, serverHome)
	folder := t.TempDir()
	fr, err := folders.Open(filepath.Join(runnerHome, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref, machine := folders.Ref(folder), fr.Machine()
	controlURL := "http://" + server.addr
	if slow > 0 || pass != nil {
		var mu sync.Mutex
		n := 0
		target, _ := url.Parse(controlURL)
		rp := httputil.NewSingleHostReverseProxy(target)
		rp.FlushInterval = -1
		px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/record") {
				time.Sleep(slow)
				mu.Lock()
				n++
				k := n
				mu.Unlock()
				if pass != nil && !pass(k) {
					http.Error(w, "the link failed", http.StatusServiceUnavailable)
					return
				}
			}
			r.Host = target.Host // control answers its own address only
			rp.ServeHTTP(w, r)
		}))
		t.Cleanup(px.Close)
		controlURL = px.URL
	}
	signInRunner(t, controlURL, tok, runnerHome, "far")
	r, err := start(config{mode: modeRunner, passEnv: fakeEnv, listen: "127.0.0.1:0", home: runnerHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, lookPath: func(string) (string, error) { return program, nil }, noReplay: true,
		server: controlURL, runnerName: "far", folders: []string{folder}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.stop)
	return &commandEnv{server: server, runner: r, tok: tok, folder: folder, ref: ref, machine: machine}
}

// askCommand makes the fake agent's turn whose prompt is "run it" call the
// run_command tool with line, and write the call's answer to the file it
// returns.
func askCommand(t *testing.T, line string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "mcp-out")
	args, _ := json.Marshal(map[string]any{"name": "run_command", "arguments": map[string]any{"command": line}})
	t.Setenv("AGENTBUS_FAKE_MCP_PROMPT", "run it")
	t.Setenv("AGENTBUS_FAKE_MCP", string(args))
	t.Setenv("AGENTBUS_FAKE_MCP_OUT", out)
	return out
}

// commandOf reads the command id the run_command call answered with.
func commandOf(t *testing.T, out string) string {
	t.Helper()
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the run_command call left no answer: %v", err)
	}
	// "<status> <JSON-RPC reply>", whose one text content is the app's result.
	_, body, _ := strings.Cut(string(b), " ")
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &rpc) != nil || rpc.Result.IsError || len(rpc.Result.Content) != 1 {
		t.Fatalf("run_command: %s", b)
	}
	var res struct {
		Command, State string
	}
	if json.Unmarshal([]byte(rpc.Result.Content[0].Text), &res) != nil || !strings.HasPrefix(res.Command, "c_") || res.State != "queued" {
		t.Fatalf("run_command's result: %s", rpc.Result.Content[0].Text)
	}
	return res.Command
}

// waitCommand waits for command id's job to end.
func waitCommand(t *testing.T, svc *service, id string) queue.Job {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		j, err := svc.queue.JobByCommand(queue.DefaultTeam, id)
		if err != nil {
			t.Fatalf("the job of command %s: %v", id, err)
		}
		if j.Ended != nil {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("command %s did not end: %s", id, j.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitCommandState waits for command id's job to reach state.
func waitCommandState(t *testing.T, svc *service, id, state string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		j, err := svc.queue.JobByCommand(queue.DefaultTeam, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == state {
			return
		}
		if time.Now().After(deadline) || j.Ended != nil {
			t.Fatalf("command %s is %s, want %s", id, j.State, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// deliveredOf waits for the turn job that delivers job j's result, and for it
// to end.
func deliveredOf(t *testing.T, svc *service, j queue.Job) queue.Job {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		list, err := svc.queue.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindTurn}, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range list {
			if r := d.Links.ResultOf; r != nil && r.Job == j.ID {
				return waitJob(t, svc, "", d.ID)
			}
		}
	}
	t.Fatalf("no turn delivered the result of %s", j.ID)
	return queue.Job{}
}

// commandEnded reads command id's command.ended from svc's record.
func commandEnded(t *testing.T, svc *service, id string) event.Event {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		recs, err := svc.store.After(0, store.Filter{Turn: id}, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			var e event.Event
			if r.Kind == event.CommandEnded && json.Unmarshal(r.Data, &e) == nil {
				return e
			}
		}
	}
	t.Fatalf("the record holds no command.ended for %s", id)
	return event.Event{}
}

// H1, across processes: an agent hands a long command to AgentBus and its turn
// ends; the command runs on the runner that holds the workspace once that turn
// has ended; a new turn arrives in the session with its exit code and the end
// of its output, and the record holds command.ended.
func TestRunCommandEndsTheTurnAndWakesTheSession(t *testing.T) {
	e := newCommandEnv(t)
	out := askCommand(t, `echo first-line; echo to-stderr >&2; echo made > made-by-command.txt; exit 3`)
	sess := newSessionOf(t, e.server, e.tok, map[string]any{"folder_ref": e.ref, "machine": e.machine, "sandbox": false})
	code, asking := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": "claude", "prompt": "run it"})
	if code != 202 {
		t.Fatalf("POST /api/v0/turns: %d", code)
	}
	asking = waitJob(t, e.server, e.tok, asking.ID)
	if asking.State != queue.Finished {
		t.Fatalf("the asking turn: %s %s %s", asking.State, asking.Reason, asking.Result)
	}
	id := commandOf(t, out)
	j := waitCommand(t, e.server, id)
	// It ran on the runner, after the turn that asked for it ended.
	if len(j.Attempts) != 1 || j.Attempts[0].Taken.Before(*asking.Ended) {
		t.Fatalf("the command's attempts %+v, the asking turn ended %s", j.Attempts, asking.Ended)
	}
	if j.State != queue.Failed || j.Reason != queue.ReasonExit {
		t.Fatalf("the command: %s %s %s", j.State, j.Reason, j.Result)
	}
	if b, err := os.ReadFile(filepath.Join(e.folder, "made-by-command.txt")); err != nil || string(b) != "made\n" {
		t.Fatalf("the command did not run in the workspace's folder: %q %v", b, err)
	}
	// GET /api/v0/commands/{id} says how it ended.
	var view struct {
		State, Reason, Session, Turn, Output string
		ExitCode                             *int `json:"exit_code"`
	}
	if code := callAPI(t, e.server, e.tok, "GET", "/api/v0/commands/"+id, nil, &view); code != 200 || view.State != "failed" ||
		view.Reason != "exit" || view.ExitCode == nil || *view.ExitCode != 3 || view.Session != sess || view.Turn != asking.Turn ||
		!strings.Contains(view.Output, "first-line") || !strings.Contains(view.Output, "to-stderr") {
		t.Fatalf("GET /api/v0/commands/%s: %d %+v", id, code, view)
	}
	// The record holds its end.
	ce := commandEnded(t, e.server, id)
	if ce.Command == nil || ce.Command.ExitCode != 3 || ce.State != "failed" || ce.Origin == nil || ce.Origin.Turn != asking.Turn ||
		!strings.Contains(ce.Command.Output, "first-line") || ce.Session != "" {
		t.Fatalf("command.ended: %+v %+v", ce, ce.Command)
	}
	// A new turn in the asking session carries the result.
	d := deliveredOf(t, e.server, j)
	if d.Session != sess || d.State != queue.Finished {
		t.Fatalf("the delivered turn: session %s, %s %s", d.Session, d.State, d.Result)
	}
	for _, want := range []string{"[AgentBus]", "Command: echo first-line", "Id: " + id, "Exit code: 3", "first-line", "to-stderr"} {
		if !strings.Contains(d.Spec.Prompt, want) {
			t.Errorf("the delivered prompt lacks %q:\n%s", want, d.Spec.Prompt)
		}
	}
	// Its on fired and is no longer listed as waiting.
	var ons struct {
		Ons []struct{ Turn string } `json:"ons"`
	}
	if code := callAPI(t, e.server, e.tok, "GET", "/api/v0/ons", nil, &ons); code != 200 {
		t.Fatalf("GET /api/v0/ons: %d", code)
	}
	for _, o := range ons.Ons {
		if o.Turn == id {
			t.Fatalf("the command's on is still listed after it delivered: %+v", ons)
		}
	}
}

// A stopped command: its tree ends, it reads stopped, and its result still
// comes back to the asking session, saying so.
func TestRunCommandStopped(t *testing.T) {
	e := newCommandEnv(t)
	out := askCommand(t, `echo started; sleep 60`)
	sess := newSessionOf(t, e.server, e.tok, map[string]any{"folder_ref": e.ref, "machine": e.machine, "sandbox": false})
	code, asking := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": "claude", "prompt": "run it"})
	if code != 202 {
		t.Fatalf("POST /api/v0/turns: %d", code)
	}
	waitJob(t, e.server, e.tok, asking.ID)
	id := commandOf(t, out)
	waitCommandState(t, e.server, id, queue.Running)
	// The waiting on is pending work, listed for the session.
	var ons struct {
		Ons []struct{ Event, Turn, Session string } `json:"ons"`
	}
	if code := callAPI(t, e.server, e.tok, "GET", "/api/v0/ons?session="+sess, nil, &ons); code != 200 || len(ons.Ons) != 1 ||
		ons.Ons[0].Turn != id || ons.Ons[0].Event != queue.OnCommandEnded {
		t.Fatalf("GET /api/v0/ons?session=: %d %+v", code, ons)
	}
	began := time.Now()
	if code := callAPI(t, e.server, e.tok, "POST", "/api/v0/commands/"+id+"/stop", nil, nil); code != 202 {
		t.Fatalf("POST /api/v0/commands/%s/stop: %d", id, code)
	}
	j := waitCommand(t, e.server, id)
	if j.State != queue.Cancelled || time.Since(began) > 15*time.Second {
		t.Fatalf("the stopped command: %s %s after %s", j.State, j.Result, time.Since(began))
	}
	if code := callAPI(t, e.server, e.tok, "POST", "/api/v0/commands/"+id+"/stop", nil, nil); code != 409 {
		t.Fatalf("a second stop of an ended command: %d, want 409", code)
	}
	d := deliveredOf(t, e.server, j)
	if !strings.Contains(d.Spec.Prompt, "State: stopped") || !strings.Contains(d.Spec.Prompt, "started") {
		t.Fatalf("the delivered prompt:\n%s", d.Spec.Prompt)
	}
	if ce := commandEnded(t, e.server, id); ce.State != "stopped" {
		t.Fatalf("command.ended: %+v", ce)
	}
}

// A command whose workspace is busy with a turn waits: it starts only once the
// turn that asked for it, still running, has ended. Once it runs, a turn the
// person sends to the session does not wait for it.
func TestRunCommandWaitsForABusyWorkspace(t *testing.T) {
	e := newCommandEnv(t)
	dir := t.TempDir()
	hold, release := filepath.Join(dir, "hold"), filepath.Join(dir, "release")
	out := askCommand(t, `echo running > `+filepath.Join(dir, "ran")+`; while [ ! -f `+release+` ]; do sleep 0.05; done; echo released`)
	// The asking turn calls run_command, then keeps running until hold exists.
	t.Setenv("AGENTBUS_FAKE_WAIT", hold)
	sess := newSessionOf(t, e.server, e.tok, map[string]any{"folder_ref": e.ref, "machine": e.machine, "sandbox": false})
	code, asking := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": "claude", "prompt": "run it"})
	if code != 202 {
		t.Fatalf("POST /api/v0/turns: %d", code)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(out); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the asking turn never called run_command")
		}
	}
	id := commandOf(t, out)
	// The workspace is busy with the asking turn: the command waits.
	time.Sleep(time.Second)
	if j, err := e.server.queue.JobByCommand(queue.DefaultTeam, id); err != nil || j.State != queue.Queued {
		t.Fatalf("while the asking turn runs, the command is %s (%v), want queued", j.State, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Fatal("the command ran while the asking turn was still running")
	}
	if a := waitState(t, e.server, e.tok, asking.ID); a.Ended != nil {
		t.Fatalf("the asking turn ended early: %s", a.State)
	}
	// The turn ends; the command starts.
	t.Setenv("AGENTBUS_FAKE_WAIT", "")
	os.WriteFile(hold, nil, 0o600)
	asking = waitJob(t, e.server, e.tok, asking.ID)
	waitCommandState(t, e.server, id, queue.Running)
	// A turn the person sends meanwhile runs, and ends, while the command runs.
	t.Setenv("AGENTBUS_FAKE_MCP_PROMPT", "")
	t.Setenv("AGENTBUS_FAKE_MCP", "")
	code, next := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": "claude", "prompt": "hello"})
	if code != 202 {
		t.Fatalf("POST /api/v0/turns while the command runs: %d", code)
	}
	if next = waitJob(t, e.server, e.tok, next.ID); next.State != queue.Finished {
		t.Fatalf("the person's turn: %s %s", next.State, next.Result)
	}
	if j, _ := e.server.queue.JobByCommand(queue.DefaultTeam, id); j.State != queue.Running {
		t.Fatalf("the command is %s once the person's turn ended, want running", j.State)
	}
	os.WriteFile(release, nil, 0o600)
	j := waitCommand(t, e.server, id)
	if j.State != queue.Finished || j.Attempts[0].Taken.Before(*asking.Ended) {
		t.Fatalf("the command: %s %s, taken %s, asking turn ended %s", j.State, j.Result, j.Attempts[0].Taken, asking.Ended)
	}
	if d := deliveredOf(t, e.server, j); !strings.Contains(d.Spec.Prompt, "Exit code: 0") || !strings.Contains(d.Spec.Prompt, "released") {
		t.Fatalf("the delivered prompt:\n%s", d.Spec.Prompt)
	}
}

// A command handed to AgentBus never gets more than the asking turn itself
// may do, through the API and through the tool alike (review P1, 2026-11-27,
// Codex's reproduction: with edits forbidden codex ran read-only, and the
// command was accepted). The runner decides at the turn's launch, from the
// limits its harness started with, and control refuses by that record.
//
// The runner reaches control over a slow link, so its live record comes after
// the child's first call: the decision is there only because the runner sends
// the turn's start to control before the child starts (run.Request.Recorded;
// the gate, 2026-11-27, failed 1 run in 5 without it).
func TestRunCommandNeverGetsMoreThanItsTurn(t *testing.T) {
	e := newCommandEnvVia(t, 700*time.Millisecond)
	setEdit := func(allowed bool) {
		t.Helper()
		p, _, err := e.server.sdb.Policy()
		if err != nil {
			t.Fatal(err)
		}
		p.Allow = slices.DeleteFunc(p.Allow, func(r string) bool { return r == "tool:edit" })
		p.Forbid = slices.DeleteFunc(p.Forbid, func(r string) bool { return r == "tool:edit" })
		if allowed {
			p.Allow = append(p.Allow, "tool:edit")
		} else {
			p.Forbid = append(p.Forbid, "tool:edit")
		}
		if _, _, err := e.server.sdb.SetPolicy(p, settings.Any); err != nil {
			t.Fatal(err)
		}
	}
	line := `echo x > written-by-command.txt`
	// try runs a turn of agent that hands line to AgentBus, over HTTP (as
	// agentbus-api) or over MCP (the run_command tool), and returns the answer.
	try := func(agent string, mcp bool) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "out")
		t.Setenv("AGENTBUS_FAKE_MCP", "")
		t.Setenv("AGENTBUS_FAKE_POST", "")
		if mcp {
			askCommand(t, line)
			t.Setenv("AGENTBUS_FAKE_MCP_OUT", out)
		} else {
			t.Setenv("AGENTBUS_FAKE_MCP_PROMPT", "")
			b, _ := json.Marshal(map[string]any{"command": line})
			t.Setenv("AGENTBUS_FAKE_POST", "/api/v0/commands "+string(b))
			t.Setenv("AGENTBUS_FAKE_POST_OUT", out)
		}
		sess := newSessionOf(t, e.server, e.tok, map[string]any{"folder_ref": e.ref, "machine": e.machine, "sandbox": false})
		code, j := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": agent, "prompt": "run it"})
		if code != 202 {
			t.Fatalf("POST /api/v0/turns: %d", code)
		}
		waitJob(t, e.server, e.tok, j.ID)
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("%s: the call left no answer: %v", agent, err)
		}
		return string(b)
	}
	refused := func(name, got, want string) {
		t.Helper()
		// HTTP: "403 {...}"; MCP: a tool error whose text says why.
		if !strings.Contains(got, want) || (!strings.HasPrefix(got, "403 ") && !strings.Contains(got, `"isError":true`)) {
			t.Errorf("%s: %s, want a refusal saying %q", name, got, want)
		}
	}
	setEdit(false)
	for _, agent := range []string{"codex", "claude"} {
		refused(agent+" read-only over HTTP", try(agent, false), "may not edit")
		refused(agent+" read-only over MCP", try(agent, true), "may not edit")
	}
	// Edits allowed: codex still runs its shell in its own sandbox, and the
	// command would leave it.
	setEdit(true)
	refused("codex over HTTP", try("codex", false), "own sandbox")
	refused("codex over MCP", try("codex", true), "own sandbox")
	if list, err := e.server.queue.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindExec}, 10); err != nil || len(list) != 0 {
		t.Fatalf("refused commands made %d jobs (%v)", len(list), err)
	}
	if _, err := os.Stat(filepath.Join(e.folder, "written-by-command.txt")); err == nil {
		t.Fatal("a refused command wrote in the folder")
	}
	// A turn its own shell runs unwalled for, which may edit, may hand one over.
	if got := try("claude", false); !strings.HasPrefix(got, "202 ") {
		t.Fatalf("claude in a direct folder that may edit: %s, want 202", got)
	}
}

// A run control saw start always ends there, whatever ends the turn on the
// runner (jobs.closeOpenRuns), so the session's next turn starts. Two ways of
// it, each with control and a separate runner:
//
//   - the agent fails to start: an agent program without execute permission
//     (review P1, 2026-11-27, Codex's reproduction: one open run was left on
//     control, and the next turn got 409);
//   - control never gets the run's end: every record the runner sends after
//     the turn's start is lost on the link.
func TestARunControlSawStartEndsThere(t *testing.T) {
	notAProgram := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(notAProgram, []byte("not a program\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		program string
		pass    func(int) bool
		want    string
	}{
		{"the agent cannot start", notAProgram, nil, queue.Failed},
		{"the run's end is lost on the link", self, func(n int) bool { return n == 1 }, queue.Finished},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCommandEnvOf(t, 0, c.program, c.pass)
			sess := newSessionOf(t, e.server, e.tok, map[string]any{"folder_ref": e.ref, "machine": e.machine, "sandbox": false})
			code, j := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": "claude", "prompt": "hi"})
			if code != 202 {
				t.Fatalf("POST /api/v0/turns: %d", code)
			}
			if j = waitJob(t, e.server, e.tok, j.ID); j.State != c.want {
				t.Fatalf("the turn: %s, want %s", j.State, c.want)
			}
			recs, err := e.server.store.After(0, store.Filter{Turn: j.Turn}, 200)
			if err != nil {
				t.Fatal(err)
			}
			var started, ended int
			for _, r := range recs {
				switch r.Kind {
				case event.RunStarted:
					started++
				case event.RunEnded:
					ended++
				}
			}
			if started != 1 || ended != 1 {
				t.Fatalf("control's record of the turn: %d run.started, %d run.ended; want one each", started, ended)
			}
			if code, _ := startTurn(t, e.server, e.tok, map[string]any{"session": sess, "agent": "claude", "prompt": "again"}); code != 202 {
				t.Fatalf("the session's next turn: %d, want 202 (no run left open on control)", code)
			}
		})
	}
}
