//go:build integration

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A turn that does not finish never ends the session (recover_integration_test.go).
// Here the whole service goes while a turn runs, as a crash, the terminal being
// closed or a power cut take it: the real binary, a stand-in claude that calls a
// fake vendor, and a second start on the same data folder. SIGTERM is the way
// the service is asked to stop; its turn is closed with its calls confirmed, and
// the session goes on. SIGKILL gives it no say: it takes up and up's built-in
// runner, a process of its own that outlives up (builtin.go), as a power cut
// does; with up alone gone, its runner would finish the turn whole.
func TestRecoverAfterServiceEndsMidTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(pid int)
	}{
		{"SIGTERM", func(pid int) { syscall.Kill(pid, syscall.SIGTERM) }},
		{"SIGKILL", func(pid int) { syscall.Kill(-pid, syscall.SIGKILL) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := built(t)
			v, url := newVendor(t)
			wait := filepath.Join(t.TempDir(), "go")
			if err := os.WriteFile(wait, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTBUS_FAKE_AGENT", "1")
			t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody("one"))
			t.Setenv("AGENTBUS_FAKE_STDOUT", claudeResultA)
			t.Setenv("AGENTBUS_FAKE_EXIT", "0")
			t.Setenv("AGENTBUS_FAKE_WAIT", wait)
			t.Setenv("AGENTBUS_TEST_UPSTREAM", url)
			t.Setenv("AGENTBUS_USERNS", "off")
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			bins, home := fakeAgentDir(t, "claude"), t.TempDir()
			start := func() (*proc, *client) {
				addr := freeAddr(t)
				p := startProc(t, bin, home, bins, "up", "-listen", addr)
				return p, waitService(t, p, addr, home)
			}
			p, c := start()
			session := c.newWorkspaceSession(map[string]any{"folder": personFolder(t), "isolate": false})
			turn := func(c *client) string {
				t.Helper()
				var s started
				if code, out := c.call("POST", "/api/v0/turns", map[string]any{"session": session, "agent": "claude", "prompt": "one"}, &s); code != 202 || s.Turn == "" {
					t.Fatalf("POST /api/v0/turns: %d %s", code, out)
				}
				return s.Turn
			}
			state := func(c *client, id string) (string, string) {
				var one struct {
					Turn struct{ State, Error string }
				}
				c.call("GET", "/api/v0/turns/"+id, nil, &one)
				return one.Turn.State, one.Turn.Error
			}
			ended := func(c *client, id string) (string, string) {
				t.Helper()
				for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
					if s, e := state(c, id); s == "finished" || s == "failed" || s == "stopped" {
						return s, e
					}
				}
				t.Fatalf("turn %s did not end", id)
				return "", ""
			}

			// Turn 1 ends. Turn 2 asks the vendor, then its agent waits for a
			// file that is not there.
			if s, e := ended(c, turn(c)); s != "finished" {
				t.Fatalf("turn 1 ended %s: %s", s, e)
			}
			if err := os.Remove(wait); err != nil {
				t.Fatal(err)
			}
			t2 := turn(c)
			waitFor(t, "turn 2's call to reach the vendor", func() bool {
				v.mu.Lock()
				defer v.mu.Unlock()
				return len(v.bodies) >= 2
			})
			if s, _ := state(c, t2); s != "running" {
				t.Fatalf("turn 2 is %s, want running", s)
			}

			tc.kill(p.cmd.Process.Pid)
			<-p.done
			if tc.name == "SIGKILL" {
				killBuiltinRunner(t, home)
			}
			if err := os.WriteFile(wait, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			// The service starts again on the same data folder; the person
			// sends the session's next turn.
			_, c = start()
			t3 := turn(c)
			s, e := ended(c, t3)
			if s != "finished" {
				t.Fatalf("the turn after the service was ended mid-turn (%s) ended %s: %s", tc.name, s, e)
			}
			if strings.Contains(e, "cannot be continued") {
				t.Fatalf("the turn after the service was ended mid-turn (%s): %s", tc.name, e)
			}
			// A clean stop keeps replay. After a hard one the turn gets the
			// earlier turns as text, told that their history was not all stored.
			_, events := c.call("GET", "/api/v0/sessions/"+session+"/events?turn="+t3, nil, nil)
			transcript := strings.Contains(events, `"continuity":"transcript"`)
			told := strings.Contains(events, "cannot be sent to you in full")
			if want := tc.name == "SIGKILL"; transcript != want || told != want {
				t.Errorf("the turn after %s: transcript %v, lost-history head %v, want %v: %s", tc.name, transcript, told, want, events)
			}
		})
	}
}

// killBuiltinRunner kills up's built-in runner and its agents (its own process
// group, builtin.go), named by up's service log, and waits until it is gone.
func killBuiltinRunner(t *testing.T, home string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "logs", "service.log"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.LastIndex(string(b), "runner.spawned pid=")
	if i < 0 {
		t.Fatal("up's log names no runner it started")
	}
	var pid int
	if _, err := fmt.Sscanf(string(b)[i+len("runner.spawned pid="):], "%d", &pid); err != nil || pid <= 0 {
		t.Fatalf("up's log names its runner's pid badly: %v", err)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	waitFor(t, "up's runner is gone", func() bool { return !held(runnerLock(home)) })
}
