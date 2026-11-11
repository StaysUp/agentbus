//go:build integration

package run

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/store"
)

func commandEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.StopAll(); st.Close() })
	return e
}

// alive says pid is a process that has not ended. A zombie has ended.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err != nil || !strings.Contains(string(b), ") Z ")
}

func gone(t *testing.T, what string, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); alive(pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) still runs", what, pid)
		}
	}
}

func pidIn(t *testing.T, file string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(file)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", file)
		}
	}
}

// A command ends with everything it started: at its time limit, a process
// that called setsid goes too, and so does what it left running when it
// ended by itself.
func TestCommandEndsItsTree(t *testing.T) {
	e := commandEngine(t)
	dir := t.TempDir()
	escape := `perl -e 'use POSIX; setsid(); open(F, ">", "esc.pid"); print F "$$\n"; close F; sleep 30' & sleep 30`
	ev, err := e.RunCommand(context.Background(), Command{Session: "s1", Folder: dir, Line: escape, Timeout: time.Second})
	if err != nil || !ev.Command.TimedOut {
		t.Fatalf("timed command: %+v %v", ev.Command, err)
	}
	gone(t, "the process that called setsid", pidIn(t, filepath.Join(dir, "esc.pid")))

	ev, err = e.RunCommand(context.Background(), Command{Session: "s1", Folder: dir, Line: `sleep 30 & echo $! > left.pid`})
	if err != nil || ev.Command.ExitCode != 0 || ev.Command.TimedOut {
		t.Fatalf("command: %+v %v", ev.Command, err)
	}
	if ev.Command.DurationMS > 5000 {
		t.Fatalf("a command that ended at once took %dms", ev.Command.DurationMS)
	}
	gone(t, "what the command left running", pidIn(t, filepath.Join(dir, "left.pid")))
}

// A command holds its session's slot while it runs: no turn or other
// command of the session starts on this engine beside it.
func TestCommandHoldsSessionSlot(t *testing.T) {
	e := commandEngine(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.RunCommand(ctx, Command{Session: "s1", Folder: dir, Line: "echo up > up; sleep 30"})
		close(done)
	}()
	pidWait := func() {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "up")); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the command did not start")
			}
		}
	}
	pidWait()
	if code, msg := e.runs.take("s1"); code != http.StatusConflict {
		t.Fatalf("a turn beside the command: %d %s", code, msg)
	}
	_, err := e.RunCommand(context.Background(), Command{Session: "s1", Folder: dir, Line: "true"})
	if rf, ok := err.(*Refusal); !ok || rf.Status != http.StatusConflict {
		t.Fatalf("a second command: %v", err)
	}
	if code, _ := e.runs.take("s2"); code != 0 {
		t.Fatal("another session was refused")
	}
	e.runs.release(nil, "s2")
	cancel()
	<-done
	if code, _ := e.runs.take("s1"); code != 0 {
		t.Fatal("the slot was not freed")
	}
	e.runs.release(nil, "s1")
}
