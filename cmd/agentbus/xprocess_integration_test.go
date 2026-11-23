//go:build integration

package main

// Cross-process test harness: a real agentbus server and a real agentbus
// runner, each its own OS process, as a team runs them. The in-process
// server+runner of jobs_integration_test.go (TestServerAndRunner) shares one
// heap, so a part that reads another part's memory instead of control's record
// still passes there. A separate runner process cannot: it reaches control
// only over HTTP, through the relay, the blob transport and the folder jobs.
// This is the test setup the record-on-control and one-gateway-path rules ask
// for (AGENTS.md, "Rules of the code"; audit 2026-11-16, finding 11).
//
// The server and runner are the real built binary, so the modes under test are
// the shipped code. The agent is the test binary, which doubles as the fake
// agent (fake_integration_test.go): a symlink named for the harness on the
// runner's PATH, triggered by AGENTBUS_FAKE_AGENT, which the runner passes to
// the agent through turnEnv. Everything a test reads about a turn it reads from
// control over the API, never from a process's memory.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
)

// agentbusBinary builds the real agentbus binary once per test run and returns
// its path. The build is a couple of seconds; sync.Once shares it across every
// cross-process test. The temp folder is left for the OS to clean: a test run
// cannot remove it until every test that may still spawn from it is done.
var (
	xbinOnce sync.Once
	xbinPath string
	xbinErr  error
)

func agentbusBinary(t *testing.T) string {
	t.Helper()
	xbinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agentbus-xproc-bin-")
		if err != nil {
			xbinErr = err
			return
		}
		out := filepath.Join(dir, "agentbus")
		var errb bytes.Buffer
		cmd := exec.Command("go", "build", "-o", out, "github.com/staysup/agentbus/cmd/agentbus")
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			xbinErr = fmt.Errorf("building agentbus: %v: %s", err, errb.String())
			return
		}
		xbinPath = out
	})
	if xbinErr != nil {
		t.Fatal(xbinErr)
	}
	return xbinPath
}

// fakeAgentDir returns a directory holding an executable named for each harness
// (claude, codex) that is the test binary: run as the agent, it is the fake
// agent (AGENTBUS_FAKE_AGENT). It goes on the runner's PATH, so the runner's
// own exec.LookPath finds it, as it would find a real install.
func fakeAgentDir(t *testing.T, harnesses ...string) string {
	t.Helper()
	if len(harnesses) == 0 {
		harnesses = []string{"claude", "codex"}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, h := range harnesses {
		if err := os.Symlink(self, filepath.Join(dir, h)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// xproc is a control process and the runners signed in to it, each a separate
// OS process. Its fields are the client's view: the base URL and token a team
// uses, never a process's internals.
type xproc struct {
	t          *testing.T
	bin        string
	serverHome string
	base       string // http://127.0.0.1:PORT
	tok        string // control's main token
	// agentEnv is added to every runner's environment, for the fake agent:
	// AGENTBUS_FAKE_* knobs a test sets before it adds a runner.
	agentEnv []string
}

// startServer builds the binary, starts agentbus server in its own process and
// waits until it is listening. connectFake gives control a subscription, as the
// cockpit's Connect would, so a runner with -gateway server can relay its
// agents' calls.
func startServer(t *testing.T, harnesses ...string) *xproc {
	t.Helper()
	return startServerEnv(t, nil, harnesses...)
}

// startServerEnv is startServer with more variables in control's environment,
// such as a HOME of its own, so control and a runner stand for two machines.
func startServerEnv(t *testing.T, extra []string, harnesses ...string) *xproc {
	t.Helper()
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	connectFake(t, x.serverHome, harnesses...)
	env := append(append(os.Environ(), "AGENTBUS_HOME="+x.serverHome), extra...)
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0"}, env, reListening)
	x.base = "http://" + addr
	x.tok = readToken(t, x.serverHome)
	return x
}

// startServerUpstream is startServer with control's model upstream pointed at a
// loopback fake vendor (AGENTBUS_TEST_UPSTREAM), so a two-process test can make
// the gateway guard fire on a real reply — the one thing a server with the real
// vendor cannot do in a test.
func startServerUpstream(t *testing.T, upstream string, harnesses ...string) *xproc {
	t.Helper()
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	connectFake(t, x.serverHome, harnesses...)
	env := append(os.Environ(), "AGENTBUS_HOME="+x.serverHome, "AGENTBUS_TEST_UPSTREAM="+upstream)
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0"}, env, reListening)
	x.base = "http://" + addr
	x.tok = readToken(t, x.serverHome)
	return x
}

// startServerTwoUpstreams is startServerUpstream with both vendor upstreams
// pointed at loopback fakes (AGENTBUS_TEST_UPSTREAM for Anthropic,
// AGENTBUS_TEST_CHATGPT_UPSTREAM for ChatGPT), so a two-process test can prove
// each vendor's calls reach only that vendor.
func startServerTwoUpstreams(t *testing.T, anthropic, chatgpt string, harnesses ...string) *xproc {
	t.Helper()
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	connectFake(t, x.serverHome, harnesses...)
	env := append(os.Environ(), "AGENTBUS_HOME="+x.serverHome,
		"AGENTBUS_TEST_UPSTREAM="+anthropic, "AGENTBUS_TEST_CHATGPT_UPSTREAM="+chatgpt)
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0"}, env, reListening)
	x.base = "http://" + addr
	x.tok = readToken(t, x.serverHome)
	return x
}

// addRunner signs a runner in to control (as agentbus login and the Runners
// page do) and starts agentbus runner in its own process, owning folders and
// running the fake agent off its PATH. It waits until the runner is listening,
// then until control lists the runner's folders, so a turn sent after it
// returns has a machine to run on.
func (x *xproc) addRunner(name string, folders ...string) {
	t := x.t
	t.Helper()
	home := t.TempDir()
	signInRunner(t, x.base, x.tok, home, name)
	bin := fakeAgentDir(t)
	env := append(os.Environ(),
		"AGENTBUS_HOME="+home,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AGENTBUS_FAKE_AGENT=1",
		// The runner's own environment must not send an agent elsewhere.
		"ANTHROPIC_BASE_URL=",
		"OPENAI_BASE_URL=",
		"CODEX_HOME="+t.TempDir(),
	)
	env = append(env, x.agentEnv...)
	args := []string{"runner", "-server", x.base, "-name", name}
	for _, f := range folders {
		args = append(args, "-folder", f)
	}
	x.spawn(t, "runner:"+name, args, env, reRunnerReady)
	if len(folders) > 0 {
		x.waitFolders(len(folders))
	}
}

// setAgentEnv adds AGENTBUS_FAKE_* knobs that each later runner passes to its
// agent. Call it before addRunner.
func (x *xproc) setAgentEnv(kv ...string) { x.agentEnv = append(x.agentEnv, kv...) }

var reListening = regexp.MustCompile(`listening on http://([^,\s]+)`)

// reRunnerReady is a runner's one start line.
var reRunnerReady = regexp.MustCompile(`runner ready, taking jobs from (\S+)`)

// spawn starts the binary in a mode, streams its stderr to the test log with a
// tag, and waits until a line matches ready, returning its first capture. It
// stops the process on cleanup: SIGTERM, then a wait, then Kill.
func (x *xproc) spawn(t *testing.T, tag string, args, env []string, ready *regexp.Regexp) string {
	t.Helper()
	cmd := exec.Command(x.bin, args...)
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	got := make(chan string, 1)
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			t.Logf("[%s] %s", tag, line)
			if m := ready.FindStringSubmatch(line); m != nil {
				select {
				case got <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case addr := <-got:
		return addr
	case <-done:
		t.Fatalf("%s exited before it was ready", tag)
	case <-time.After(30 * time.Second):
		t.Fatalf("%s was not ready within 30s", tag)
	}
	return ""
}

// get sends GET path with the main token and returns the status and body.
func (x *xproc) get(path string) (int, []byte) {
	x.t.Helper()
	return x.do("GET", path, nil)
}

// post sends POST path with a JSON body (a map or nil) and the main token.
func (x *xproc) post(path string, body any) (int, []byte) {
	x.t.Helper()
	return x.do("POST", path, body)
}

func (x *xproc) do(method, path string, body any) (int, []byte) {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, x.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+x.tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), x.tok) {
		x.t.Fatal("the token appeared in a reply")
	}
	return resp.StatusCode, b
}

// xFolder is one folder in GET /api/v0/folders.
type xFolder struct {
	Ref     string `json:"ref"`
	Machine string `json:"machine"`
	Name    string `json:"name"`
}

func (x *xproc) folders() []xFolder {
	x.t.Helper()
	code, b := x.get("/api/v0/folders")
	if code != 200 {
		x.t.Fatalf("GET /api/v0/folders: %d %s", code, b)
	}
	var out struct {
		Folders []xFolder `json:"folders"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		x.t.Fatalf("GET /api/v0/folders: %v: %s", err, b)
	}
	return out.Folders
}

// waitFolders waits until control lists at least n folders: a runner reports
// them to control as it comes up, so a turn by reference has a machine.
func (x *xproc) waitFolders(n int) {
	x.t.Helper()
	waitFor(x.t, fmt.Sprintf("control lists %d folders", n), func() bool {
		return len(x.folders()) >= n
	})
}

// turnStart is POST /api/v0/turns' answer: the session and turn, whether the
// turn runs at once or waits in the queue.
type turnStart struct {
	Session string `json:"session"`
	Turn    string `json:"turn"`
	State   string `json:"state"`
}

// startTurnX starts a turn. A first turn (a folder or folder_ref, no session)
// runs the 3-call flow first: create the workspace (its folder and sandbox
// mode) and a session in it, then post the turn naming the session. A body that
// already names a session is posted as-is. The turn runs on whichever runner
// owns its folder.
func (x *xproc) startTurnX(body map[string]any) (int, turnStart) {
	x.t.Helper()
	if body["session"] == nil && (body["folder_ref"] != nil || body["folder"] != nil) {
		wsBody := map[string]any{"sandbox": true}
		if v, ok := body["isolate"].(bool); ok {
			wsBody["sandbox"] = v
		}
		for from, to := range map[string]string{"folder": "folder", "folder_ref": "folder_ref", "machine": "machine", "connector": "machine"} {
			if v, ok := body[from]; ok {
				wsBody[to] = v
			}
		}
		code, wb := x.post("/api/v0/workspaces", wsBody)
		if code/100 != 2 {
			return code, turnStart{}
		}
		var ws struct {
			Workspace string `json:"workspace"`
		}
		json.Unmarshal(wb, &ws)
		code, sb := x.post("/api/v0/sessions", map[string]any{"workspace": ws.Workspace})
		if code/100 != 2 {
			return code, turnStart{}
		}
		var sc struct {
			Session string `json:"session"`
		}
		json.Unmarshal(sb, &sc)
		nb := map[string]any{"session": sc.Session}
		for k, v := range body {
			switch k {
			case "folder", "folder_ref", "machine", "connector", "isolate", "read_only":
			default:
				nb[k] = v
			}
		}
		body = nb
	}
	code, b := x.post("/api/v0/turns", body)
	var st turnStart
	_ = json.Unmarshal(b, &st)
	return code, st
}

// recordKinds is the kinds in control's record of a session, read over the API.
func (x *xproc) recordKinds(session string) []string {
	x.t.Helper()
	code, b := x.get("/api/v0/sessions/" + session + "/events")
	if code != 200 {
		x.t.Fatalf("GET events: %d %s", code, b)
	}
	var page struct {
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		x.t.Fatalf("events: %v: %s", err, b)
	}
	var kinds []string
	for _, e := range page.Events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func hasKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// A job made on control runs on a separate runner process, and the turn's
// record reaches control over HTTP. The runner's gateway and record are its
// own memory; control holds only what the runner forwards. This is the
// record-forwarding test (TestServerAndRunner) across real processes.
func TestXProcRunnerForwardsRecord(t *testing.T) {
	folder := t.TempDir()
	x := startServer(t, "claude")
	x.setAgentEnv("AGENTBUS_FAKE_MARK=" + filepath.Join(folder, "made.txt"))
	x.addRunner("far", folder)

	fs := x.folders()
	if len(fs) != 1 || fs[0].Name != filepath.Base(folder) {
		t.Fatalf("control's folders: %+v", fs)
	}
	// The server never learns the folder's path, only its reference.
	if _, b := x.get("/api/v0/folders"); strings.Contains(string(b), folder) {
		t.Fatalf("control holds the folder's path:\n%s", b)
	}

	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref,
		"machine": fs[0].Machine, "prompt": "hello", "isolate": false})
	if code/100 != 2 || st.Session == "" {
		t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
	}

	// The runner wrote the turn's record and forwarded it; control holds its
	// start and close, and, at the job's end, control's own turn.changed.
	waitFor(t, "the runner's turn in control's record", func() bool {
		k := x.recordKinds(st.Session)
		return hasKind(k, "session.closed") && hasKind(k, "turn.changed")
	})
	k := x.recordKinds(st.Session)
	for _, want := range []string{"session.opened", "turn.started", "session.closed", "turn.changed"} {
		if !hasKind(k, want) {
			t.Fatalf("control's record lacks %s: %v", want, k)
		}
	}
	// The fake agent ran on the runner: it wrote the mark in the folder's copy,
	// and the turn's one change came back to control.
	if _, err := os.Stat(filepath.Join(folder, "made.txt")); err != nil {
		t.Fatalf("the agent did not run on the runner: %v", err)
	}
}

// sessionIDs is the ids of control's sessions, over the API.
func (x *xproc) sessionIDs() []string {
	x.t.Helper()
	code, b := x.get("/api/v0/sessions")
	if code != 200 {
		x.t.Fatalf("GET /api/v0/sessions: %d %s", code, b)
	}
	var page struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		x.t.Fatalf("sessions: %v: %s", err, b)
	}
	var ids []string
	for _, s := range page.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

// An agent on a separate runner process asks another agent through control's
// API, with the token control minted for its turn. The ask reaches control over
// HTTP, control starts the asked turn on the runner, and control's record holds
// both sessions. This is TestRunnerAgentAsksAnother across real processes.
func TestXProcRunnerAgentAsksAnother(t *testing.T) {
	folder := t.TempDir()
	asked := filepath.Join(t.TempDir(), "asked")
	x := startServer(t, "claude")
	x.setAgentEnv(
		"AGENTBUS_FAKE_CALL=",
		`AGENTBUS_FAKE_ASK={"agent":"claude","prompt":"answer me"}`,
		"AGENTBUS_FAKE_ASKED="+asked,
		// A resume id, so the asking session could take a next turn.
		`AGENTBUS_FAKE_STDOUT={"type":"result","subtype":"success","result":"done","session_id":"fake-resume-1"}`,
	)
	x.addRunner("far", folder)
	fs := x.folders()

	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref,
		"machine": fs[0].Machine, "prompt": "ask someone", "isolate": false})
	if code/100 != 2 || st.Session == "" {
		t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
	}

	// The agent's ask reached control and control accepted the asked turn.
	waitFor(t, "the agent's ask recorded", func() bool {
		_, err := os.Stat(asked)
		return err == nil
	})
	b, err := os.ReadFile(asked)
	if err != nil || !strings.HasPrefix(string(b), "202 ") {
		t.Fatalf("the ask through control's API: %v %s", err, b)
	}

	// Control's record holds the asking session and the asked one.
	waitFor(t, "both sessions in control's record", func() bool {
		return len(x.sessionIDs()) >= 2
	})
}

// An ask rule refused on a separate runner process reaches control's record,
// lists from GET /api/v0/approvals, and answers from POST — control holding no
// in-memory state for it. This is the 2026-11-16 bug class, across real
// processes: the guard on the runner's gateway holds the command, records
// approval.requested, and the runner forwards it to control, which lists and
// decides it from the record alone. The fake upstream (AGENTBUS_TEST_UPSTREAM)
// returns a reply whose tool call the policy holds for a person.
func TestXProcApprovalFromRunnerReachesControl(t *testing.T) {
	folder := t.TempDir()
	// A reply that asks to run a Bash command the policy holds (ask).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",`+
			`"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git push"}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	x := startServerUpstream(t, upstream.URL, "claude")
	// The policy holds "git push" for a person: the runner's gateway guard
	// holds the call and records approval.requested.
	{
		body, _ := json.Marshal(map[string]any{"ask": []string{"git push"}})
		req, _ := http.NewRequest("PUT", x.base+"/api/v0/settings/permissions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+x.tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", "*") // replace whatever is there
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("set the ask rule: %d", resp.StatusCode)
		}
	}
	x.setAgentEnv("AGENTBUS_FAKE_CALL=1") // the agent makes one model call
	x.addRunner("far", folder)
	fs := x.folders()

	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref,
		"machine": fs[0].Machine, "prompt": "do a thing", "isolate": false})
	if code/100 != 2 || st.Session == "" {
		t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
	}

	// The approval the runner's guard raised reaches control's record and lists
	// there, with the command — control held no state for it.
	var id string
	waitFor(t, "the runner's approval listed on control", func() bool {
		code, b := x.get("/api/v0/approvals")
		if code != 200 {
			return false
		}
		var page struct {
			Approvals []struct {
				ID       string   `json:"id"`
				Commands []string `json:"commands"`
			} `json:"approvals"`
		}
		if json.Unmarshal(b, &page) != nil {
			return false
		}
		for _, a := range page.Approvals {
			for _, c := range a.Commands {
				if c == "git push" {
					id = a.ID
					return true
				}
			}
		}
		return false
	})

	// Answered from control's record; allow moves the command to the allow list.
	if code, b := x.post("/api/v0/approvals/"+id, map[string]any{"decision": "allow"}); code != 204 {
		t.Fatalf("decide allow: %d %s", code, b)
	}
	if code, b := x.get("/api/v0/settings/permissions"); code != 200 || !strings.Contains(string(b), `"git push"`) {
		t.Fatalf("after allow, permissions: %d %s", code, b)
	}
	// The answered approval no longer lists.
	if _, b := x.get("/api/v0/approvals"); strings.Contains(string(b), id) {
		t.Fatalf("the answered approval still lists:\n%s", b)
	}
}

// Vendor isolation end to end across processes (rule 3): an agent's call
// carries a stand-in the relay swaps for control's own sign-in and sends to
// the one vendor the path names. A claude turn's model call reaches only the
// Anthropic upstream and a codex turn's only the ChatGPT upstream, each with
// control's credential (never the stand-in abs_ or the turn token abr_), and
// neither reaches the other. Covers the isolation the retired -gateway local
// codex tests checked, now on the one gateway path.
func TestXProcEachVendorPathReachesOnlyItsVendor(t *testing.T) {
	var mu sync.Mutex
	var anthHits, cgHits []string // "path auth" per request
	rec := func(dst *[]string, reply string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.ReadAll(r.Body)
			mu.Lock()
			*dst = append(*dst, r.URL.Path+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, reply)
		}))
	}
	anth := rec(&anthHits, claudeReply)
	cg := rec(&cgHits, codexReply)
	defer anth.Close()
	defer cg.Close()
	snap := func(dst *[]string) []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), *dst...) }

	folder := t.TempDir()
	x := startServerTwoUpstreams(t, anth.URL, cg.URL, "claude", "codex")
	// A claude turn's agent makes one claude call (AGENTBUS_FAKE_CALL), a codex
	// turn's one codex call (AGENTBUS_FAKE_CODEX_BODY). Each knob fires only for
	// the harness whose vendor base URL the runner set, so neither agent makes
	// the other's call.
	x.setAgentEnv("AGENTBUS_FAKE_CALL=1",
		`AGENTBUS_FAKE_CODEX_BODY={"model":"gpt-x","input":[{"type":"message","role":"user","content":"hi"}]}`)
	x.addRunner("r", folder)
	fs := x.folders()
	run := func(agent string) {
		code, st := x.startTurnX(map[string]any{"agent": agent, "folder_ref": fs[0].Ref,
			"machine": fs[0].Machine, "prompt": "hi", "isolate": false})
		if code/100 != 2 || st.Session == "" {
			t.Fatalf("%s turn: %d %+v", agent, code, st)
		}
	}

	run("claude")
	waitFor(t, "the claude call reached the Anthropic upstream", func() bool { return len(snap(&anthHits)) >= 1 })
	run("codex")
	waitFor(t, "the codex call reached the ChatGPT upstream", func() bool { return len(snap(&cgHits)) >= 1 })

	a, c := snap(&anthHits), snap(&cgHits)
	if len(a) != 1 || !strings.HasPrefix(a[0], "/v1/messages ") {
		t.Fatalf("Anthropic upstream saw %v, want one /v1/messages", a)
	}
	if len(c) != 1 || !strings.Contains(c[0], "/backend-api/codex/responses") {
		t.Fatalf("ChatGPT upstream saw %v, want one codex call", c)
	}
	// Each carried control's credential, never the stand-in or the turn token.
	for _, got := range []string{a[0], c[0]} {
		auth := strings.TrimPrefix(strings.Fields(got + " ")[1], "Bearer ")
		if strings.HasPrefix(auth, jobs.StandInPrefix) || strings.HasPrefix(auth, "abr_") || auth == "" {
			t.Fatalf("vendor saw %q: want control's credential, not a stand-in or turn token", got)
		}
	}
}
