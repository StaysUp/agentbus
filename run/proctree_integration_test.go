//go:build integration

package run

import (
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// With the process table unreadable when a run started, its tree was marked
// as if the child had exited, so a stop sent nothing to the running child
// and StopRuns waited on it for good. Without the table, a stop still
// reaches the child's own group, and later failures to read the table
// change nothing.
func TestStopWithoutProcTableReachesTheChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start bool // the table fails from the start, not only later
	}{{"unreadable at start", true}, {"unreadable later", false}} {
		t.Run(tc.name, func(t *testing.T) {
			// The tree keeps the stand-in it began with; whether it fails is
			// an atomic, so its poll never races the test.
			var failing atomic.Bool
			failing.Store(tc.start)
			defer func(f func(int64) ([]proc, error)) { procTable = f }(procTable)
			procTable = func(min int64) ([]proc, error) {
				if failing.Load() {
					return nil, errNoProcTable
				}
				return readProcs(min)
			}
			cmd := exec.Command("sleep", "30")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			var rs runs
			code, msg, lt := rs.start(cmd, "s", "t_1")
			if code != 0 {
				t.Fatalf("start: %d %s", code, msg)
			}
			defer cmd.Process.Kill()
			defer lt.tree.exited() // stops the poll
			failing.Store(true)
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			rs.signal(syscall.SIGTERM)
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
				t.Fatal("the child got no signal with the process table unreadable")
			}
			if code := ExitCode(cmd.ProcessState, nil); code != 128+15 {
				t.Fatalf("exit %d, want 143", code)
			}
		})
	}
}
