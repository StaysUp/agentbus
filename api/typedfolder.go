package api

// A folder a person typed for a new session, on the machine they chose.
// Control never reads a folder, so the runner of that machine checks it
// through a folder job it takes (queue.KindFolder): agentbus up's own machine
// through its built-in runner, any other through that machine's runner. The
// folder is registered there, the session goes on by its reference, and runs
// on that machine. GET /api/v0/folders lists it from then on.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/staysup/agentbus/queue"
)

// typedFolder is the reference of the folder at path, and the machine whose
// runner registered it. Which machine: the one named; else this service's
// own; else the only one online. status and msg say why not, when msg is not
// empty; status 0 means the caller went away.
func (a *API) typedFolder(ctx context.Context, path, machine string) (ref, conn string, status int, msg string) {
	ref, conn, _, status, msg = a.checkFolder(ctx, machine, queue.FolderCheck{Path: path})
	return ref, conn, status, msg
}

// checkFolder runs the folder job c on the folder's machine (chosen as
// typedFolder chooses it) and answers the folder's reference, the machine,
// and, when c names a base, the commit it resolved to.
func (a *API) checkFolder(ctx context.Context, machine string, c queue.FolderCheck) (ref, conn, commit string, status int, msg string) {
	res, conn, status, msg := a.folderJob(ctx, machine, c)
	return res.FolderRef, conn, res.BaseCommit, status, msg
}

// folderJob runs the folder job c on the folder's machine, chosen as
// typedFolder chooses it, and answers its result and the machine.
func (a *API) folderJob(ctx context.Context, machine string, c queue.FolderCheck) (res queue.Result, conn string, status int, msg string) {
	// agentbus up's own machine takes the folder job through its built-in
	// runner, the same KindFolder job a remote runner takes: control never
	// reads the folder on its own thread. The folder's shape is already
	// checked synchronously before here (api/runs.go), so a bad path got its
	// 400 without the queue; existence is the runner's, through the job.
	if own := a.cfg.Folders; own != nil && (machine == "" || machine == own.Machine()) {
		machine = own.Machine()
	} else if machine != "" {
		if !queue.ConnectorShape.MatchString(machine) {
			return queue.Result{}, "", http.StatusBadRequest, "machine must be con_ and 16 hex digits: a machine's id, from GET /api/v0/runners"
		}
		seen, err := a.cfg.Queue.MachineSeen(queue.DefaultTeam, machine)
		if err != nil {
			return queue.Result{}, "", http.StatusInternalServerError, "internal"
		}
		if !seen {
			return queue.Result{}, "", http.StatusServiceUnavailable, "machine " + machine + " is offline; start agentbus runner there, or name another"
		}
	} else {
		all, err := a.cfg.Queue.Machines(queue.DefaultTeam)
		if err != nil {
			return queue.Result{}, "", http.StatusInternalServerError, "internal"
		}
		switch len(all) {
		case 0:
			return queue.Result{}, "", http.StatusServiceUnavailable, "no machine is online, so the folder cannot be reached; start agentbus runner on the folder's machine"
		case 1:
			machine = all[0]
		default:
			return queue.Result{}, "", http.StatusBadRequest, strconv.Itoa(len(all)) +
				" machines are online; name the folder's machine as machine, from GET /api/v0/runners"
		}
	}
	job, err := a.cfg.Queue.CheckFolder(queue.DefaultTeam, machine, c)
	switch {
	case errors.Is(err, queue.ErrFull):
		return queue.Result{}, "", http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end and retry"
	case err != nil:
		return queue.Result{}, "", http.StatusInternalServerError, "internal"
	}
	// The path goes from the job whatever happens, once it is read.
	defer a.cfg.Queue.ForgetFolder(queue.DefaultTeam, job.ID)
	wait := queue.FolderWait
	if c.Remote != "" || c.Branches {
		wait += queue.FetchWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil {
			return queue.Result{}, "", http.StatusInternalServerError, "internal"
		}
		if j.Ended != nil {
			res, _ := queue.ReadResult(j.Result)
			switch {
			case j.State == queue.Finished && queue.FolderRefShape.MatchString(res.FolderRef):
				// folder.registered is written by the finisher as the folder job
				// ends (jobs.keepOutcomes), so it is in the record whether or not
				// this request is still waiting.
				return res, machine, 0, ""
			case res.Refused != "":
				return queue.Result{}, "", http.StatusBadRequest, res.Refused
			}
			return queue.Result{}, "", http.StatusServiceUnavailable, "the machine's runner could not check the folder; send the turn again"
		}
		select {
		case <-ctx.Done():
			return queue.Result{}, "", 0, "gone"
		case <-deadline.C:
			if c.Branches {
				return queue.Result{}, "", http.StatusServiceUnavailable, "the machine's runner did not list the branches within 75 seconds; check that it runs and that git can reach the remote there, and try again"
			}
			if c.Remote != "" {
				return queue.Result{}, "", http.StatusServiceUnavailable, "the machine's runner did not fetch the branch within 75 seconds; check that git can reach the remote there, and try again"
			}
			return queue.Result{}, "", http.StatusServiceUnavailable, "the machine's runner did not check the folder within 15 seconds; check that it runs, and send the turn again"
		case <-tick.C:
		}
	}
}
