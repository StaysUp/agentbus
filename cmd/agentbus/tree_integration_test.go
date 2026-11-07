//go:build integration && (darwin || linux)

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/store"
)

// alive says pid names a running process. A zombie is not running: kill(2)
// still reaches it, so its state is read with ps.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	return err == nil && len(out) > 0 && out[0] != 'Z'
}

// startToolRun starts a run whose fake agent starts a tool that outlives it,
// and returns the session, the agent's pid and the tool's pid. An unrelated
// `sleep` the test starts itself is returned too: stopping the run must not
// touch it.
func startToolRun(t *testing.T, svc *service, tok string, setsid bool) (string, int, int, int) {
	t.Helper()
	tool := filepath.Join(t.TempDir(), "tool")
	t.Setenv("AGENTBUS_FAKE_TOOL", tool)
	t.Setenv("AGENTBUS_FAKE_TOOL_SETSID", map[bool]string{true: "1"}[setsid])
	t.Setenv("AGENTBUS_FAKE_WAIT", filepath.Join(t.TempDir(), "never"))
	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "codex", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, raw)
	}
	var agent, toolPID int
	waitFor(t, "the tool's pid", func() bool {
		b, err := os.ReadFile(tool)
		return err == nil && func() bool { _, err := fmt.Sscanf(string(b), "%d %d", &agent, &toolPID); return err == nil }()
	})
	t.Cleanup(func() { syscall.Kill(toolPID, syscall.SIGKILL) })
	return out["id"].(string), agent, toolPID, other.Process.Pid
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running", what, pid)
		}
	}
}

// Acceptance soak, 2026-11-06: after SIGTERM to codex, its `sleep 120` kept
// running with parent 1, in a process group of its own; the service
// signalled only the child's group. Stopping a turn now ends the child's
// whole tree, a tool in a session of its own included, and nothing else.
func TestStoppedTurnEndsItsTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setsid bool
		settle time.Duration // how long the tool runs before the stop
	}{
		// A tool in a group of its own stays in the child's session, which
		// counts at once.
		{"own group, stopped at once", false, 0},
		// A tool in a session of its own is found through its parent, by a
		// poll that sees it before its agent stops.
		{"own session", true, 600 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			svc := runService(t, home, nil, time.Second)
			tok := readToken(t, home)
			id, agent, tool, other := startToolRun(t, svc, tok, tc.setsid)
			time.Sleep(tc.settle)
			// Stopped from outside, as the soak stopped codex.
			if err := syscall.Kill(agent, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			c := waitClosed(t, svc.store, id)
			if c.ExitCode == nil || *c.ExitCode != 143 {
				t.Fatalf("session.closed %+v, want exit 143", c)
			}
			// session.closed is stored after the tree has ended.
			if alive(tool) {
				t.Fatalf("the tool (pid %d) outlived its stopped agent", tool)
			}
			if !alive(other) {
				t.Fatal("a process outside the run's tree was stopped")
			}
		})
	}
}

func TestServiceStopEndsToolsOfRuns(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, time.Second)
	tok := readToken(t, home)
	id, _, tool, other := startToolRun(t, svc, tok, true)
	time.Sleep(600 * time.Millisecond)
	svc.stop()
	waitGone(t, "the tool", tool) // stop returned, so this should hold at once
	if !alive(other) {
		t.Fatal("a process outside the run's tree was stopped")
	}
	st, err := store.Open(filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if c := closedEvent(runEvents(t, st, id)); c == nil || c.ExitCode == nil || *c.ExitCode != 143 {
		t.Fatalf("session.closed %+v, want exit 143", c)
	}
}

// A child that ends by itself keeps what it left running, as before: only a
// stop ends the tree.
func TestFinishedTurnKeepsItsTools(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, time.Second)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	tool := filepath.Join(t.TempDir(), "tool")
	t.Setenv("AGENTBUS_FAKE_TOOL", tool)
	t.Setenv("AGENTBUS_FAKE_TOOL_SETSID", "1")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "codex", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, raw)
	}
	waitClosed(t, svc.store, out["id"].(string))
	var agent, pid int
	b, _ := os.ReadFile(tool)
	if _, err := fmt.Sscanf(string(b), "%d %d", &agent, &pid); err != nil {
		t.Fatalf("tool file %q", b)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if !alive(pid) {
		t.Fatal("a finished turn's tool was stopped")
	}
}

// turnOf is the one turn of session, from GET /api/v0/turns.
func turnOf(t *testing.T, svc *service, tok, session string) store.Turn {
	t.Helper()
	code, out := getAPI(t, svc, tok, "/api/v0/turns?session="+session)
	var list store.TurnList
	if err := json.Unmarshal([]byte(out), &list); code != http.StatusOK || err != nil || len(list.Turns) != 1 {
		t.Fatalf("GET /turns: %d %s", code, out)
	}
	return list.Turns[0]
}

// POST /api/v0/turns/{id}/stop ends the turn's whole tree, a tool in a
// session of its own included, and nothing else. Its session.closed and the
// turn list say the person stopped it. A child that ignores SIGTERM gets
// SIGKILL after the grace period. Once it has ended, a stop gets 409; a turn
// no event names gets 404.
func TestStopRouteEndsTheTurnsTree(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ignoreTerm bool
		want       int
	}{{"agent stops on SIGTERM", false, 143}, {"agent ignores SIGTERM", true, 137}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			svc := runService(t, home, nil, time.Second)
			tok := readToken(t, home)
			if tc.ignoreTerm {
				t.Setenv("AGENTBUS_FAKE_IGNORE_TERM", "1")
			}
			id, agent, tool, other := startToolRun(t, svc, tok, true)
			time.Sleep(600 * time.Millisecond) // the poll sees the tool through its parent
			turn := turnOf(t, svc, tok, id).ID
			// Two stops at once: both get 202, and the turn stops once.
			stoppedAt := time.Now()
			codes := make(chan int, 2)
			for i := 0; i < 2; i++ {
				go func() {
					code, _, _ := postAPI(t, svc, tok, "/api/v0/turns/"+turn+"/stop", nil)
					codes <- code
				}()
			}
			for i := 0; i < 2; i++ {
				if code := <-codes; code != http.StatusAccepted {
					t.Errorf("stop: %d, want 202", code)
				}
			}
			c := waitClosed(t, svc.store, id)
			// One deadline for the whole tree: the tool, which ignores
			// SIGTERM too (it inherits the agent's ignored signal), gets
			// SIGKILL with the agent, not a second grace period later.
			if took := time.Since(stoppedAt); took > 1700*time.Millisecond {
				t.Errorf("the turn closed %s after the stop, want about the 1s grace", took)
			}
			if c.ExitCode == nil || *c.ExitCode != tc.want || !c.Stopped {
				t.Fatalf("session.closed %+v, want exit %d and stopped", c, tc.want)
			}
			if alive(agent) || alive(tool) {
				t.Fatalf("the agent (pid %d) or its tool (pid %d) outlived the stop", agent, tool)
			}
			if !alive(other) {
				t.Fatal("a process outside the turn's tree was stopped")
			}
			if tr := turnOf(t, svc, tok, id); !tr.Stopped || tr.State != store.TurnFailed {
				t.Errorf("listed turn %+v, want stopped and failed", tr)
			}
			if code, _, raw := postAPI(t, svc, tok, "/api/v0/turns/"+turn+"/stop", nil); code != http.StatusConflict {
				t.Errorf("stop after it ended: %d %s, want 409", code, raw)
			}
			if code, _, raw := postAPI(t, svc, tok, "/api/v0/turns/t_0000000000000000/stop", nil); code != http.StatusNotFound {
				t.Errorf("stop of an unknown turn: %d %s, want 404", code, raw)
			}
		})
	}
}
