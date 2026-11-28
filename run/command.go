package run

// A command an agent handed to AgentBus (H1): one shell line run by /bin/sh -c
// in the workspace's folder, on the runner that holds it, with no agent and no
// model call. It is the runner half of an exec job (queue.KindExec); the
// asking turn has ended by the time it starts, and its end is delivered to the
// asking session as a later turn.
//
// Its process tree is tracked as a turn's is (procTree), so a stop, a time
// limit or the runner stopping reaches what it started, and what is left once
// it exits is ended too. Stopping follows Buildkite's agent (job_runner.go
// Cancel): a signal, a grace period, then a kill; SIGTERM, then SIGKILL after
// the grace, as a stopped turn gets.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/staysup/agentbus/logfile"
)

// Command is one command to run.
type Command struct {
	// Line is the shell command line, run by /bin/sh -c.
	Line string
	// Dir is the folder it runs in: the workspace's copy, or its folder.
	Dir string
	// Timeout is the most it may run; it is then stopped and TimedOut.
	Timeout time.Duration
	// Grace is how long a stopped command has between SIGTERM and SIGKILL.
	Grace time.Duration
	// OutputMax bounds the end of its output that is kept.
	OutputMax int
}

// CommandEnd is how a command ended.
type CommandEnd struct {
	// Started is false when the shell could not be started at all.
	Started bool
	// ExitCode is its exit code, or 128 plus the signal that ended it.
	ExitCode int
	// Output is the end of stdout and stderr together, at most OutputMax
	// bytes; OutputBytes is all it wrote.
	Output      string
	OutputBytes int64
	// TimedOut: it ran past Timeout and was stopped. Stopped: stop closed.
	// Interrupted: ctx ended (the runner is stopping).
	TimedOut, Stopped, Interrupted bool
	Duration                       time.Duration
}

// counted is a Tail that also counts every byte written to it.
type counted struct {
	tail logfile.Tail
	n    atomic.Int64
}

func (c *counted) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.tail.Write(p)
}

// RunCommand runs c until it exits, is stopped (stop closes), passes its time
// limit, or ctx ends, and says how it ended. It returns once the command has
// exited and what it left running has been ended.
func RunCommand(ctx context.Context, c Command, stop <-chan struct{}) CommandEnd {
	out := &counted{tail: logfile.Tail{Max: max(c.OutputMax, 1)}}
	cmd := exec.Command("/bin/sh", "-c", c.Line)
	cmd.Dir = c.Dir
	cmd.Env = commandEnv(os.Environ(), c.Dir)
	cmd.Stdout, cmd.Stderr = out, out
	// Its own session, and so its own process group, as a turn's child: a stop
	// reaches what it started (procTree).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A descendant that keeps the output open must not hold the end up past
	// the grace; it is ended with the rest of the tree below.
	cmd.WaitDelay = c.Grace
	began := time.Now()
	if err := cmd.Start(); err != nil {
		return CommandEnd{ExitCode: -1}
	}
	tree := newProcTree(cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	end := CommandEnd{Started: true}
	var limit <-chan time.Time
	if c.Timeout > 0 {
		t := time.NewTimer(c.Timeout)
		defer t.Stop()
		limit = t.C
	}
	var kill <-chan time.Time
	term := func() {
		if kill != nil {
			return // stopping already
		}
		tree.signal(syscall.SIGTERM)
		kill = time.After(c.Grace)
	}
	quit := ctx.Done()
	var err error
wait:
	for {
		select {
		case err = <-done:
			break wait
		case <-stop:
			stop, end.Stopped = nil, true
			term()
		case <-limit:
			limit, end.TimedOut = nil, true
			term()
		case <-quit:
			quit, end.Interrupted = nil, true
			term()
		case <-kill:
			tree.signal(syscall.SIGKILL)
		}
	}
	tree.exited()
	tree.end(c.Grace)
	end.ExitCode = ExitCode(cmd.ProcessState, err)
	end.Duration = time.Since(began)
	b, _ := out.tail.Bytes()
	end.Output, end.OutputBytes = strings.ToValidUTF8(string(b), ""), out.n.Load()
	return end
}

// commandEnv is the runner's environment for a command, without AgentBus's own
// variables: a command runs after its asking turn ended, so it carries no turn
// token. PWD names the folder it starts in, as a shell sets it.
func commandEnv(parent []string, dir string) []string {
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AGENTBUS_") || k == "PWD" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
}
