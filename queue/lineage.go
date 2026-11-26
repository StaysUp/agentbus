package queue

import "github.com/staysup/agentbus/ids"

// CloseWorkspace always queues a close in the workspace lane. The runner
// removes the copy, including unapplied work. It succeeds when no copy exists.
// A pending turn therefore cannot rebuild a copy after an "already closed"
// shortcut. A later turn rebuilds from the folder.
//
// The workspace.closed event resets applied.Fold, the same state apply,
// preview, update and the workspace view read. Jobs need no lineage stamps.
// A missing record refuses the close before anything is queued.
func (q *Queue) CloseWorkspace(team string, ws ids.WorkspaceID, machine string, by *By) (job Job, created bool, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	busy, err := q.jobsInFlight(team, ws, `'apply','discard','update','close'`)
	if err != nil {
		return Job{}, false, err
	}
	if busy {
		return Job{}, false, ErrApplyBusy
	}
	// Closing needs the same record as apply and update. A scheduler without
	// one cannot publish the reset, so it must not remove the copy.
	if _, err := q.appliedState(ws); err != nil {
		return Job{}, false, err
	}
	// The close runs on the runner holding the copy (copy affinity routes it), so
	// its state -- not the folder machine's -- decides the close, with the right
	// reason. Fall back to the folder machine only when no copy row exists yet:
	// there is nothing on disk to remove, and the close still records its event.
	if closed, removed, fresh, hasCopy, name, err := q.copyRunner(team, ws); err != nil {
		return Job{}, false, err
	} else if hasCopy {
		switch {
		case closed:
			return Job{}, false, ErrSessionClosed
		case removed:
			return Job{}, false, &RunnerRemovedError{Machine: name}
		case !fresh:
			return Job{}, false, ErrMachineOffline
		}
	} else if ok, err := q.MachineSeen(team, machine); err != nil {
		return Job{}, false, err
	} else if !ok {
		return Job{}, false, ErrMachineOffline
	}
	spec := Spec{Workspace: ws, Machine: machine}
	// No session (a close is a workspace op); its copy affinity keys by the
	// workspace (CopyKey), so create routes it to the copy's runner.
	return q.create(team, KindClose, "", spec, Links{ApplyBy: by}, nil, "", nil)
}
