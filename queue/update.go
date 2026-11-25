package queue

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/staysup/agentbus/applied"
	"github.com/staysup/agentbus/ids"
)

// ErrTurnBusy: a merge was asked for while a turn of the workspace runs or
// waits. Nothing was queued.
var ErrTurnBusy = errors.New("queue: a turn of this workspace is running or waiting")

// Update queues a KindUpdate job that merges the person's folder as it is now
// into the workspace's copy, so the session's unapplied work sits on top of the
// folder and the next Apply writes only that work. The runner holding the copy
// reads the folder (its own machine, the local fast path) and does a three-way
// merge in the copy's private git: the last applied checkpoint (or base) as the
// merge base (Restore), the copy head as ours, a fresh snapshot of the folder as
// theirs. One apply, discard or update runs per workspace: a second is refused at
// queue time with ErrApplyBusy (a plain 409). A running TURN is NOT refused --
// the update waits behind it in the lane, and WHAT it folds (the unapplied turns)
// is computed when it runs, so a turn that ran while it waited is folded too.
// The merge base is read here under applyMu (no apply runs beside the update, so
// the applied point is stable). ErrNotApplicable when the applied turn has no
// checkpoint or the workspace names no folder, ErrMachineOffline when the copy's
// runner is not seen. The caller (the API) checked the workspace is a sandbox.
//
// merge asks for a merge (Spec.Merge): on a conflict the copy gets the
// conflicting files with conflict markers, for the agent's next turn to
// resolve. Unlike an update it is refused with ErrTurnBusy while a turn of the
// workspace runs or waits, so the markers land only between turns, before the
// turn the person sends to resolve them.
func (q *Queue) Update(team string, ws ids.WorkspaceID, machine string, by *By, merge bool) (job Job, created bool, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	busy, err := q.jobsInFlight(team, ws, `'apply','discard','update'`)
	if err != nil {
		return Job{}, false, err
	}
	if busy {
		return Job{}, false, ErrApplyBusy
	}
	if merge {
		if busy, err := q.jobsInFlight(team, ws, `'turn'`); err != nil {
			return Job{}, false, err
		} else if busy {
			return Job{}, false, ErrTurnBusy
		}
	}
	// The merge base: the copy checkpoint whose content the folder currently
	// holds -- the last applied turn's end checkpoint, a prior update's folder
	// snapshot, or the copy's base when nothing was applied. NOT simply the last
	// applied turn: a prior update already moved the folder baseline, and using
	// the turn checkpoint would re-import the earlier folder state over the
	// person's later change (folder A->B, update, person reverts to A, a second
	// update must merge from B, not from base A).
	rec, err := q.appliedState(ws)
	if err != nil {
		return Job{}, false, err
	}
	base := folderBaseCheckpoint(rec)
	if base == "" && rec.Mark >= 0 {
		// Something was applied (or a prior update ran) but its checkpoint could
		// not be found; refuse rather than merge from the copy's base and lose the
		// folder baseline.
		return Job{}, false, ErrNotApplicable
	}
	// The folder to read for theirs: the workspace's folder and its machine.
	first, err := q.firstSpec(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	if len(first.Places()) == 0 || first.Machine == "" {
		return Job{}, false, ErrNotApplicable
	}
	// The update runs on the runner holding the copy (copy affinity routes it),
	// so its state -- not the folder machine's -- decides it, with the right
	// reason, exactly as Discard does. Fall back to the folder machine only when
	// no copy row exists yet.
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
	// The job carries no session (an update is a workspace op, not a session's).
	// Its copy affinity keys by the workspace (CopyKey), so create routes it to
	// the copy's runner. The head turn and folded turns are filled at the finish,
	// not here (a turn may run while it waits in the lane).
	spec := Spec{Workspace: ws, Machine: first.Machine, FolderRef: first.FolderRef, Folders: first.Folders, Restore: base, Merge: merge}
	return q.create(team, KindUpdate, "", spec, Links{ApplyBy: by}, Limits{}, "", nil)
}

// folderBaseCheckpoint is the copy checkpoint whose content the person's folder
// currently holds -- the merge base for an update. An unapplied clean update
// governs it (its theirs, the folder as the update imported it); else the
// latest successful apply's target; else "" (the copy's base, nothing
// applied). All of it is the record's (applied.State).
func folderBaseCheckpoint(rec applied.State) string {
	if rec.Update != nil {
		return rec.Update.TheirsCheckpoint
	}
	if !rec.Applied {
		return ""
	}
	return rec.CheckpointOf(rec.AppliedTo)
}

// UpdateFolds are the head turn and the folded turns of an update finishing now:
// the workspace's unapplied turns, the latest kept as the head (which a later
// Apply targets as the merge) and the rest folded (no longer applied on their
// own). Read when the update RUNS, not when it was queued, so a turn that ran
// while it waited in the lane is included. head is empty when nothing is
// unapplied (the update re-synced the copy but there is no session work on top).
// The caller is inside Outcome.
func (q *Queue) UpdateFolds(team string, ws ids.WorkspaceID) (head string, folded []string, err error) {
	rec, err := q.appliedState(ws)
	if err != nil {
		return "", nil, err
	}
	unapplied := rec.Turns[rec.Mark+1:]
	if len(unapplied) == 0 {
		return "", nil, nil
	}
	head = unapplied[len(unapplied)-1].Turn
	folded = make([]string, 0, len(unapplied)-1)
	for _, t := range unapplied[:len(unapplied)-1] {
		folded = append(folded, t.Turn)
	}
	return head, folded, nil
}

// MarkUpdated records on the update job its head turn (links.applies_turn) and
// the folded turns (links.applies_turns), for the job's own answer. A later
// apply targets the head as the merge, and the folded turns stop being
// appliable, through the record's workspace.updated, not through this. Called
// at the finish, once the folds are known.
func (q *Queue) MarkUpdated(team, jobID, head string, folded []string) error {
	b, err := json.Marshal(folded)
	if err != nil {
		return fmt.Errorf("queue: mark updated: %w", err)
	}
	if _, err := q.db.Exec(`UPDATE jobs
		SET links = json_set(json_set(COALESCE(links, '{}'), '$.applies_turn', ?), '$.applies_turns', json(?)), updated = ?
		WHERE team = ? AND id = ? AND kind = 'update'`, head, string(b), nanos(q.now()), team, jobID); err != nil {
		return fmt.Errorf("queue: mark updated: %w", err)
	}
	return nil
}
