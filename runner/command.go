package runner

// An exec job (queue.KindExec): a command an agent's turn handed to AgentBus,
// run in the turn's workspace on this runner, the one that holds it (H1).

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// CommandGrace is how long a stopped command has between SIGTERM and SIGKILL,
// as a stopped turn has.
const CommandGrace = 5 * time.Second

// execWork runs the job's command in its workspace's folder: the copy for a
// sandbox workspace (mounted again after a restart), the folder itself for a
// direct one. It records no changes: what the command writes in a copy shows
// in the workspace's next turn's diff. ok is false when the attempt is no
// longer current, and nothing is to be finished.
func (r *Runner) execWork(ctx context.Context, at *attempt) (f queue.Finish, ok bool) {
	refuse := func(why string) (queue.Finish, bool) {
		return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: why, Status: http.StatusBadRequest})}, true
	}
	sp := at.job.Spec
	if sp.Command == "" {
		return refuse("the command is empty")
	}
	w := at.job.Workspace
	if w == nil {
		return refuse("the workspace has no copy or folder on this runner yet")
	}
	var dir string
	switch w.Kind {
	case queue.WorkspaceCopy, queue.WorkspaceWorktree:
		s, err := r.loadSpace(*w)
		if err != nil {
			return refuse("the workspace's copy could not be read")
		}
		if err := isolation.Remount(s); err != nil {
			r.cfg.Logf("the copy for job %s could not be mounted again: %s", at.job.ID, err)
			return refuse("the workspace's copy could not be read")
		}
		dir = s.Dir
	default:
		dir = r.workspaceDir(*w)
		if res := r.remount(at, dir); res != nil {
			return refuse(res.Refused)
		}
	}
	if dir == "" {
		return refuse("the workspace's folder is not known on this runner")
	}
	timeout := time.Duration(sp.CommandTimeout) * time.Second
	at.event("command.started", map[string]any{"command": at.job.Turn})
	at.flush(ctx)
	// A stale attempt stops the command as a cancel does, but finishes nothing:
	// the queue has already settled the job.
	stop, ended := make(chan struct{}), make(chan struct{})
	var stale atomic.Bool
	go func() {
		select {
		case <-at.cancel:
		case <-at.stale:
			stale.Store(true)
		case <-ended:
			return
		}
		close(stop)
	}()
	end := run.RunCommand(ctx, run.Command{Line: sp.Command, Dir: dir, Timeout: timeout, Grace: CommandGrace,
		OutputMax: queue.MaxCommandOutput}, stop)
	close(ended)
	at.event("command.ended", map[string]any{"exit_code": end.ExitCode, "timed_out": end.TimedOut})
	if stale.Load() {
		r.cfg.Logf("job %s attempt %d is no longer current; its command was stopped", at.job.ID, at.a.Number)
		return queue.Finish{}, false
	}
	res := queue.Result{ExitCode: &end.ExitCode, Output: end.Output, OutputBytes: end.OutputBytes,
		TimedOut: end.TimedOut, Stopped: end.Stopped, DurationMS: end.Duration.Milliseconds(), State: queue.Failed}
	if !end.Started {
		res.ExitCode = nil
		res.Error = "the shell could not be started"
	}
	switch {
	case end.Stopped:
		res.State, f.Status = queue.TurnStopped, queue.Cancelled
	case end.Interrupted:
		f.Status, f.Reason = queue.Failed, ReasonRunnerStopped
	case end.TimedOut:
		f.Status, f.Reason = queue.Failed, queue.ReasonTimeout
	case !end.Started:
		f.Status, f.Reason = queue.Failed, queue.ReasonSetup
	case end.ExitCode == 0:
		res.State, f.Status = queue.Finished, queue.Finished
	default:
		f.Status, f.Reason = queue.Failed, queue.ReasonExit
	}
	f.Result = mustJSON(res)
	return f, true
}
