//go:build integration

package run

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A command past its time limit is stopped and says so; one that leaves a
// child running in the background does not leave it behind; and the output
// kept is its end, with every byte counted.
func TestRunCommandLimitsAndTree(t *testing.T) {
	dir := t.TempDir()
	began := time.Now()
	end := RunCommand(context.Background(), Command{Line: "echo before; sleep 30", Dir: dir, Timeout: 300 * time.Millisecond,
		Grace: time.Second, OutputMax: 1024}, nil)
	if !end.Started || !end.TimedOut || end.Stopped || end.ExitCode != 128+int(syscall.SIGTERM) || time.Since(began) > 10*time.Second ||
		!strings.Contains(end.Output, "before") {
		t.Fatalf("a command past its time: %+v after %s", end, time.Since(began))
	}

	pidFile := filepath.Join(dir, "pid")
	end = RunCommand(context.Background(), Command{Line: "sleep 30 & echo $! > " + pidFile + "; echo done", Dir: dir,
		Grace: time.Second, OutputMax: 1024}, nil)
	if end.ExitCode != 0 || !strings.Contains(end.Output, "done") {
		t.Fatalf("a command with a background child: %+v", end)
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("no child pid: %q", b)
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the background child %d still runs after its command ended", pid)
		}
	}

	end = RunCommand(context.Background(), Command{Line: "yes line | head -n 5000; echo last", Dir: dir, Grace: time.Second, OutputMax: 100}, nil)
	if end.OutputBytes != 5000*5+5 || len(end.Output) != 100 || !strings.HasSuffix(end.Output, "last\n") {
		t.Fatalf("the kept output: %d of %d bytes, %q", len(end.Output), end.OutputBytes, end.Output)
	}
	// The command's folder is its PWD, and AgentBus's own variables are left out.
	t.Setenv("AGENTBUS_TOKEN", "abr_secret")
	end = RunCommand(context.Background(), Command{Line: `echo "pwd=$PWD tok=${AGENTBUS_TOKEN:-none}"`, Dir: dir, Grace: time.Second, OutputMax: 1024}, nil)
	if !strings.Contains(end.Output, "pwd="+dir+" tok=none") {
		t.Fatalf("the command's environment: %q", end.Output)
	}
}
