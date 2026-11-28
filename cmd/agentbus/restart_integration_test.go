//go:build integration

package main

// A restart of agentbus up mid-turn, across real processes: up is the built
// binary, its built-in runner the process up starts, the agent the fake one.
// The turn runs while up is stopped and started again, briefly or for longer
// than a lease, and must finish normally with its result in control's record;
// a turn whose agent ends while up is away must finish too, its model call
// made in the gap held until control is back. (TODO-detail "Restart control
// mid-turn", 2026-11-25: before 0.6.7 every running turn ended with exit 143,
// runner_stopped.)

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// upProc is one agentbus up process.
type upProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// startUp starts agentbus up from bin on addr and waits until it listens.
func startUp(t *testing.T, bin, tag, addr string, env []string, args ...string) *upProc {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"up", "-listen", addr}, args...)...)
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &upProc{cmd: cmd, done: make(chan struct{})}
	ready := make(chan struct{}, 1)
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			t.Logf("[%s] %s", tag, sc.Text())
			if reListening.MatchString(sc.Text()) {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { p.stop(t) })
	select {
	case <-ready:
	case <-p.done:
		t.Fatalf("%s exited before it listened", tag)
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not listen within 30s", tag)
	}
	return p
}

// stop sends SIGTERM, as Ctrl-C or a restart does, and waits for up to exit.
func (p *upProc) stop(t *testing.T) {
	if p.cmd.ProcessState != nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}

// turnOf is GET /api/v0/turns/{id}: its state and error.
func (x *xproc) turnOf(id string) (state, errText string) {
	x.t.Helper()
	code, b := x.get("/api/v0/turns/" + id)
	if code != 200 {
		return "", ""
	}
	var v struct {
		Turn struct {
			State string `json:"state"`
			Error string `json:"error"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Turn.State, v.Turn.Error
}

// runnerLock is the lock up's built-in runner holds while it runs.
func runnerLock(home string) string { return filepath.Join(home, builtinDir, "events.db.lock") }

// waitRunnerGone waits for up's built-in runner to leave, as it does once up
// is stopped and it runs nothing.
func waitRunnerGone(t *testing.T, home string) {
	t.Helper()
	waitForN(t, "the built-in runner leaves", 20*time.Second, func() bool { return !held(runnerLock(home)) })
}

func waitForN(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

type restartCase struct {
	down     time.Duration // how long up is away
	endsAway bool          // the agent ends, after one model call, while up is away
	newBuild bool          // up comes back from another build
}

func runRestart(t *testing.T, rc restartCase) {
	bin := agentbusBinary(t)
	home := t.TempDir()
	connectFake(t, home, "claude")
	v, vendorURL := newVendor(t)
	scratch := t.TempDir()
	release := filepath.Join(scratch, "go")
	callOut := filepath.Join(scratch, "call")
	env := append(os.Environ(),
		"AGENTBUS_HOME="+home,
		"PATH="+fakeAgentDir(t, "claude")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AGENTBUS_FAKE_AGENT=1",
		"ANTHROPIC_BASE_URL=", "OPENAI_BASE_URL=",
		"AGENTBUS_TEST_UPSTREAM="+vendorURL,
		"AGENTBUS_FAKE_WAIT="+release, "AGENTBUS_FAKE_WAIT_S=300",
		"AGENTBUS_FAKE_STDOUT=done",
		// The turn's own prompt, so own replay can continue the session.
		"AGENTBUS_FAKE_CALL_AFTER="+freshClaudeBody("hello"),
		"AGENTBUS_FAKE_CALL_OUT="+callOut,
	)
	addr := freeAddr(t)
	up1 := startUp(t, bin, "up1", addr, env)
	x := &xproc{t: t, bin: bin, serverHome: home, base: "http://" + addr, tok: readToken(t, home)}
	t.Cleanup(func() { waitRunnerGone(t, home) })
	t.Cleanup(func() {
		if t.Failed() {
			for _, f := range []string{"service.log", "runner.log", "runner.out"} {
				b, _ := os.ReadFile(filepath.Join(home, "logs", f))
				t.Logf("%s:\n%s", f, b)
			}
		}
	})

	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hello", "isolate": false})
	if code/100 != 2 || st.Turn == "" {
		t.Fatalf("start: %d %+v", code, st)
	}
	waitForN(t, "the turn runs", 30*time.Second, func() bool { s, _ := x.turnOf(st.Turn); return s == "running" })

	up1.stop(t)
	if !held(runnerLock(home)) {
		t.Fatal("the built-in runner left with up: its turn was stopped")
	}
	away := time.Now()
	if rc.endsAway {
		time.Sleep(time.Second)
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(rc.down - time.Since(away))

	upBin := bin
	if rc.newBuild {
		// Another build: the same program with one byte more, so its content,
		// which names a build, differs.
		upBin = filepath.Join(t.TempDir(), "agentbus")
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(upBin, append(b, 0), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	startUp(t, upBin, "up2", addr, env)
	t.Logf("up back after %s", time.Since(away).Round(time.Second))
	if !rc.endsAway {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var state, why string
	waitForN(t, "the turn ends", 90*time.Second, func() bool {
		state, why = x.turnOf(st.Turn)
		return state != "running" && state != "queued" && state != ""
	})
	if state != "finished" {
		t.Fatalf("the turn ended %s (%s), want finished", state, why)
	}
	if b, _ := os.ReadFile(callOut); string(b) != "200" {
		t.Fatalf("the agent's model call got %q, want 200", b)
	}
	kinds := x.recordKinds(st.Session)
	for _, k := range []string{"turn.started", "model.responded", "run.ended"} {
		if !hasKind(kinds, k) {
			t.Fatalf("control's record of the session lacks %s: %v", k, kinds)
		}
	}
	reached := false
	v.mu.Lock()
	bodies := append([]string(nil), v.bodies...)
	v.mu.Unlock()
	for _, b := range bodies {
		reached = reached || strings.Contains(b, `"text":"hello"`)
	}
	if !reached {
		t.Fatal("the call made around the restart never reached the vendor")
	}

	if rc.newBuild {
		// The runner of the earlier build retired once its turn was done; the
		// next turn runs on one up started from its own build.
		waitForN(t, "the earlier runner retires", 30*time.Second, func() bool {
			b, _ := os.ReadFile(filepath.Join(home, "logs", "runner.log"))
			return strings.Contains(string(b), "runner.retiring why=new_build")
		})
	}
	// The session goes on after the restart.
	{
		code, st2 := x.startTurnX(map[string]any{"session": st.Session, "agent": "claude", "prompt": "again"})
		if code/100 != 2 || st2.Turn == "" {
			t.Fatalf("second turn: %d %+v", code, st2)
		}
		waitForN(t, "the second turn ends", 60*time.Second, func() bool {
			state, why = x.turnOf(st2.Turn)
			return state != "running" && state != "queued" && state != ""
		})
		if state != "finished" {
			t.Fatalf("the second turn ended %s (%s), want finished", state, why)
		}
	}
}

// up away 5 seconds while the agent works: the turn goes on.
func TestRestartUpShort(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	runRestart(t, restartCase{down: 5 * time.Second})
}

// up away a minute, two leases: the turn goes on, and control does not call
// it lost when it is back (queue.RenewLeases).
func TestRestartUpMinute(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	runRestart(t, restartCase{down: 60 * time.Second})
}

// The agent ends while up is away, after a model call made in the gap: the
// call waits for control, and the turn's end reaches the record once up is
// back, never left running.
func TestRestartUpEndsAway(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	runRestart(t, restartCase{down: 10 * time.Second, endsAway: true})
}

// The same for longer than a lease.
func TestRestartUpEndsAwayLong(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	runRestart(t, restartCase{down: 40 * time.Second, endsAway: true})
}

// up comes back from another build, as an upgrade: the running turn finishes
// on the earlier build's runner, which then retires, and the next turn runs on
// the new build's.
func TestRestartUpNewBuild(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	runRestart(t, restartCase{down: 3 * time.Second, newBuild: true})
}

// upFixture is one agentbus up's data folder and environment, the fake agent
// on its PATH waiting for release.
type upFixture struct {
	bin, home, addr, release string
	env                      []string
}

func newUpFixture(t *testing.T, extra ...string) *upFixture {
	t.Helper()
	f := &upFixture{bin: agentbusBinary(t), home: t.TempDir(), addr: freeAddr(t)}
	connectFake(t, f.home, "claude")
	_, vendorURL := newVendor(t)
	f.release = filepath.Join(t.TempDir(), "go")
	f.env = append(append(os.Environ(),
		"AGENTBUS_HOME="+f.home,
		"PATH="+fakeAgentDir(t, "claude")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AGENTBUS_FAKE_AGENT=1",
		"ANTHROPIC_BASE_URL=", "OPENAI_BASE_URL=",
		"AGENTBUS_TEST_UPSTREAM="+vendorURL,
		"AGENTBUS_FAKE_WAIT="+f.release, "AGENTBUS_FAKE_WAIT_S=300",
		"AGENTBUS_FAKE_STDOUT=done"), extra...)
	t.Cleanup(func() { waitRunnerGone(t, f.home) })
	t.Cleanup(func() {
		if t.Failed() {
			for _, n := range []string{"service.log", "runner.log", "runner.out"} {
				b, _ := os.ReadFile(filepath.Join(f.home, "logs", n))
				t.Logf("%s:\n%s", n, b)
			}
		}
	})
	return f
}

func (f *upFixture) up(t *testing.T, tag string, args ...string) (*upProc, *xproc) {
	t.Helper()
	p := startUp(t, f.bin, tag, f.addr, f.env, args...)
	return p, &xproc{t: t, bin: f.bin, serverHome: f.home, base: "http://" + f.addr, tok: readToken(t, f.home)}
}

func (f *upFixture) runnerLog() string {
	b, _ := os.ReadFile(filepath.Join(f.home, "logs", "runner.log"))
	return string(b)
}

// kept counts the turns' ends waiting on disk for control.
func (f *upFixture) kept() int {
	n := 0
	es, _ := os.ReadDir(filepath.Join(f.home, finishesDir))
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func waitEndedState(t *testing.T, x *xproc, turn string, d time.Duration) (state, why string) {
	t.Helper()
	waitForN(t, "the turn ends", d, func() bool {
		state, why = x.turnOf(turn)
		return state != "running" && state != "queued" && state != ""
	})
	return state, why
}

// up stopped for good, not restarted: its turn's agent ends while up is away,
// and the built-in runner leaves at once, its end kept on disk, so a stopped
// up leaves no process behind. When up is started again, the next runner
// sends the end and the turn reads finished. (Review evt_69f6f1c76f03b195 P1:
// the runner waited in the finish for control, so it never left.)
func TestStopUpLeavesNoRunner(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	ran := filepath.Join(t.TempDir(), "ran")
	f := newUpFixture(t, "AGENTBUS_FAKE_MARK="+ran)
	up1, x := f.up(t, "up1")
	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hello", "isolate": true})
	if code/100 != 2 || st.Turn == "" {
		t.Fatalf("start: %d %+v", code, st)
	}
	// The agent itself runs: the turn reads running from its start in
	// control's record, before the agent (run.Request.Recorded).
	waitForN(t, "the turn's agent runs", 30*time.Second, func() bool { _, err := os.Stat(ran); return err == nil })
	up1.stop(t)
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForN(t, "the runner leaves with up stopped", 30*time.Second, func() bool { return !held(runnerLock(f.home)) })
	if n := f.kept(); n != 1 {
		t.Fatalf("%d ends kept on disk, want 1", n)
	}
	_, x = f.up(t, "up2")
	if state, why := waitEndedState(t, x, st.Turn, 60*time.Second); state != "finished" {
		t.Fatalf("the turn ended %s (%s), want finished", state, why)
	}
	waitForN(t, "the kept end is sent", 10*time.Second, func() bool { return f.kept() == 0 })
	if !strings.Contains(f.runnerLog(), "sending the end a runner before kept") {
		t.Fatal("the end did not come from the kept file")
	}
}

// A turn taken in the moment before up stops, with up back a minute later,
// longer than a lease: the turn runs as soon as up is back. Nothing was spent
// on the taken attempt, so control queues it again when it starts
// (queue.RenewLeases), and a runner takes it at once. (Review P2: the turn
// waited for that attempt's lease to run out on control, 30 s, before a
// runner took it again.)
func TestRestartUpBetweenTakeAndStart(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	f := newUpFixture(t, "AGENTBUS_TEST_START_DELAY_MS=3000")
	up1, x := f.up(t, "up1")
	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hello", "isolate": true})
	if code/100 != 2 || st.Turn == "" {
		t.Fatalf("start: %d %+v", code, st)
	}
	waitForN(t, "the runner takes the turn", 30*time.Second, func() bool { return strings.Contains(f.runnerLog(), "test.start_delay") })
	up1.stop(t)
	time.Sleep(60 * time.Second)
	_, x = f.up(t, "up2")
	back := time.Now()
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, why := waitEndedState(t, x, st.Turn, 90*time.Second); state != "finished" {
		t.Fatalf("the turn ended %s (%s), want finished", state, why)
	}
	if d := time.Since(back); d > 20*time.Second {
		t.Fatalf("the turn ended %s after up was back: it waited for its old lease", d.Round(time.Second))
	}
}

// up stopped for good between a turn's take and its start: the built-in
// runner drops the start it could not send and leaves, and when up is started
// again control queues the turn again at once (queue.RenewLeases), so it runs
// then. (Found merging onto H1, 2026-11-27: the runner retried the start for
// as long as up was away, so it never left.)
func TestStopUpBetweenTakeAndStartLeavesNoRunner(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	f := newUpFixture(t, "AGENTBUS_TEST_START_DELAY_MS=3000")
	up1, x := f.up(t, "up1")
	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hello", "isolate": true})
	if code/100 != 2 || st.Turn == "" {
		t.Fatalf("start: %d %+v", code, st)
	}
	waitForN(t, "the runner takes the turn", 30*time.Second, func() bool { return strings.Contains(f.runnerLog(), "test.start_delay") })
	up1.stop(t)
	waitForN(t, "the runner leaves with up stopped", 30*time.Second, func() bool { return !held(runnerLock(f.home)) })
	_, x = f.up(t, "up2")
	back := time.Now()
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, why := waitEndedState(t, x, st.Turn, 90*time.Second); state != "finished" {
		t.Fatalf("the turn ended %s (%s), want finished", state, why)
	}
	if d := time.Since(back); d > 20*time.Second {
		t.Fatalf("the turn ended %s after up was back: it waited for its old lease", d.Round(time.Second))
	}
}

// up started again, the same build, with another option for its runner
// (-replay=false): the runner it finds, kept by a running turn, finishes that
// turn and retires, and up starts one with the new option. (Review
// t_f1724c4b98986fcb P2: the runner kept replay.)
func TestRestartUpNewOptions(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	ran := filepath.Join(t.TempDir(), "ran")
	f := newUpFixture(t, "AGENTBUS_FAKE_MARK="+ran)
	up1, x := f.up(t, "up1")
	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hello", "isolate": true})
	if code/100 != 2 || st.Turn == "" {
		t.Fatalf("start: %d %+v", code, st)
	}
	waitForN(t, "the turn's agent runs", 30*time.Second, func() bool { _, err := os.Stat(ran); return err == nil })
	up1.stop(t)
	_, x = f.up(t, "up2", "-replay=false")
	waitForN(t, "the earlier runner retires for the new options", 30*time.Second, func() bool {
		return strings.Contains(f.runnerLog(), "runner.retiring why=new_build")
	})
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, why := waitEndedState(t, x, st.Turn, 90*time.Second); state != "finished" {
		t.Fatalf("the turn ended %s (%s), want finished", state, why)
	}
	log := func() string { b, _ := os.ReadFile(filepath.Join(f.home, "logs", "service.log")); return string(b) }
	waitForN(t, "a runner of the new options runs", 30*time.Second, func() bool {
		return strings.Count(log(), "runner.spawned pid=") >= 2 && held(runnerLock(f.home))
	})
	if b, _ := os.ReadFile(filepath.Join(f.home, builtinDir, buildFile)); !strings.HasSuffix(string(b), "+replay=false") {
		t.Fatalf("the build up wants: %q", b)
	}
}
