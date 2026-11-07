//go:build integration && (darwin || linux)

package main

import (
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
