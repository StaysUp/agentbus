package runner

// A command job: the person's shell command, run in the session's
// workspace on this runner (run.Engine.RunCommand). It starts no turn and
// sends no record while it runs: its one event, command.ran, goes to
// control in the job's finish, and control keeps it once, in its own record
// (jobs/outcome.go). The job keeps only the event's id.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// command runs the attempt's command and says how the job ends. A cancel of
// the job, the runner stopping, or an attempt that is no longer current ends
// the command; the job then ends cancelled, or failed as runner_stopped, and
// its result still carries the command.ran of what ran.
func (r *Runner) command(ctx context.Context, at *attempt) queue.Finish {
	refused := func(status int, msg string) queue.Finish {
		return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: msg, Status: status})}
	}
	w := at.job.Workspace
	if w == nil || at.job.Session == "" || w.Runner != r.id {
		return refused(http.StatusConflict, "this session has no copy on this runner, so the command did not run")
	}
	dir := r.workspaceDir(*w)
	if res := r.remount(at, dir); res != nil {
		return refused(http.StatusInternalServerError, res.Refused)
	}
	if info, err := os.Stat(dir); dir == "" || err != nil || !info.IsDir() {
		return refused(http.StatusConflict, "the session's copy is not on its runner any more, so the command did not run")
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-at.stale:
		case <-at.cancel:
		case <-cctx.Done():
		}
		cancel()
	}()
	ev, err := r.cfg.Engine.RunCommand(cctx, run.Command{Session: at.job.Session, Folder: dir, ID: at.job.Spec.CommandID, Line: at.job.Spec.Command,
		Timeout: time.Duration(at.job.Spec.TimeoutMS) * time.Millisecond})
	var rf *run.Refusal
	switch {
	case errors.As(err, &rf):
		return refused(rf.Status, rf.Msg)
	case err != nil:
		return refused(http.StatusInternalServerError, "the command could not be run")
	}
	f := queue.Finish{Status: queue.Finished, Result: mustJSON(queue.Result{Ran: mustJSON(ev)})}
	select {
	case <-at.cancel:
		f.Status = queue.Cancelled
	default:
		if ctx.Err() != nil {
			f.Status, f.Reason = queue.Failed, ReasonRunnerStopped
		}
	}
	return f
}
