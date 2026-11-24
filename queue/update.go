package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	_, mark, err := q.appliedMark(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	base, err := q.folderBaseCheckpoint(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	if base == "" && mark >= 0 {
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
	if first.FolderRef == "" || first.Machine == "" {
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
	spec := Spec{Workspace: ws, Machine: first.Machine, FolderRef: first.FolderRef, Restore: base, Merge: merge}
	return q.create(team, KindUpdate, "", spec, Links{ApplyBy: by}, Limits{}, "", nil)
}

// updateBaseline is the folder baseline an active update set: From the folder's
// current state (the new apply baseline), To the merge (what applying the head
// turn writes), HeadTurn the turn an apply up to which writes the merge, and
// TheirsCheckpoint the copy's own checkpoint for the folder snapshot (the merge
// base of the next update).
type updateBaseline struct {
	From, To, HeadTurn, TheirsCheckpoint string
}

// activeUpdate is the workspace's latest finished clean update when no apply or
// discard ran after it, so the update still governs the folder's baseline: until
// the next apply the folder holds From, and applying up to HeadTurn writes To.
// ok is false when there is none, the latest update was a conflict, or a later
// apply or discard superseded it. The caller holds applyMu.
func (q *Queue) activeUpdate(team string, ws ids.WorkspaceID) (info updateBaseline, ok bool, err error) {
	var id string
	var ended sql.NullInt64
	err = q.db.QueryRow(`SELECT id, ended FROM jobs
		WHERE team = ? AND `+jobWorkspace+` = ? AND kind = 'update' AND state = 'finished'
		AND COALESCE(json_extract(result, '$.update.to'), '') != ''
		ORDER BY ended DESC, rowid DESC LIMIT 1`, team, string(ws)).Scan(&id, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return info, false, nil
	}
	if err != nil {
		return info, false, fmt.Errorf("queue: read active update: %w", err)
	}
	// A later apply or discard moved the folder's baseline or reset the copy away
	// from the merge, so the update no longer governs it.
	var n int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs
		WHERE team = ? AND `+jobWorkspace+` = ? AND kind IN ('apply','discard') AND state = 'finished' AND ended > ?`,
		team, string(ws), ended.Int64).Scan(&n); err != nil {
		return info, false, fmt.Errorf("queue: read update supersede: %w", err)
	}
	if n > 0 {
		return info, false, nil
	}
	j, err := q.Get(team, id)
	if err != nil {
		return info, false, err
	}
	r, _ := ReadResult(j.Result)
	if !r.Update.Clean() {
		return info, false, nil
	}
	return updateBaseline{From: r.Update.From, To: r.Update.To, HeadTurn: j.Links.AppliesTurn,
		TheirsCheckpoint: r.Update.TheirsCheckpoint}, true, nil
}

// mergedSince reports whether a finished merge of the workspace left conflict
// markers in its copy (its result names unresolved files) after the last
// finished apply or discard. Until one ends, every apply and preview checks the
// files it writes for the markers, whatever their path, so a rename or a later
// plain update cannot carry one past it. The caller holds applyMu.
func (q *Queue) mergedSince(team string, ws ids.WorkspaceID) (bool, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs u
		WHERE u.team = ? AND `+jobWorkspaceOf("u")+` = ? AND u.kind = 'update' AND u.state = 'finished'
		AND json_array_length(COALESCE(json_extract(u.result, '$.update.unresolved'), '[]')) > 0
		AND u.ended > COALESCE((SELECT MAX(d.ended) FROM jobs d WHERE d.team = ? AND `+jobWorkspaceOf("d")+` = ?
			AND d.kind IN ('apply','discard') AND d.state = 'finished'), -1)`,
		team, string(ws), team, string(ws)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("queue: read merges: %w", err)
	}
	return n > 0, nil
}

// jobWorkspaceOf is jobWorkspace for the jobs table under alias a.
func jobWorkspaceOf(a string) string {
	return `COALESCE(NULLIF(json_extract(` + a + `.spec, '$.workspace'), ''), ` + a + `.session)`
}

// checkpointForChangeset is the copy checkpoint whose content is the changeset
// cs: a turn's end checkpoint (its result.changeset), or an update's theirs or
// merged checkpoint (its result.update.from or .to). "" when cs is empty (the
// copy's base) or no job of the workspace recorded it. It is how control maps a
// changeset the folder holds back to a copy checkpoint, so an update's merge
// base and a discard's reset point are the checkpoint the folder matches, not a
// turn's own pre-merge checkpoint.
func (q *Queue) checkpointForChangeset(team string, ws ids.WorkspaceID, cs string) (string, error) {
	if cs == "" {
		return "", nil
	}
	var turn sql.NullString
	err := q.db.QueryRow(`SELECT turn FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND kind = 'turn'
		AND json_extract(result, '$.changeset') = ? ORDER BY ended DESC, rowid DESC LIMIT 1`,
		team, string(ws), cs).Scan(&turn)
	if err == nil && turn.String != "" {
		return q.turnCheckpoint(team, turn.String)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("queue: checkpoint for changeset: %w", err)
	}
	var merged, theirs, to sql.NullString
	err = q.db.QueryRow(`SELECT json_extract(result, '$.update.merged_checkpoint'),
		json_extract(result, '$.update.theirs_checkpoint'), json_extract(result, '$.update.to')
		FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND kind = 'update' AND state = 'finished'
		AND (json_extract(result, '$.update.to') = ? OR json_extract(result, '$.update.from') = ?)
		ORDER BY ended DESC, rowid DESC LIMIT 1`, team, string(ws), cs, cs).Scan(&merged, &theirs, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("queue: checkpoint for changeset: %w", err)
	}
	if to.String == cs {
		return merged.String, nil
	}
	return theirs.String, nil
}

// lastFinishedApplyTo is the target changeset of the workspace's most recent
// finished apply -- what the person's folder holds now -- and whether there was
// one. The caller holds applyMu.
func (q *Queue) lastFinishedApplyTo(team string, ws ids.WorkspaceID) (to string, ok bool, err error) {
	applies, err := q.sessionApplies(team, ws)
	if err != nil {
		return "", false, err
	}
	for _, a := range applies { // newest first
		if a.State == Finished {
			return a.Spec.ApplyTo, true, nil
		}
	}
	return "", false, nil
}

// folderBaseCheckpoint is the copy checkpoint whose content the person's folder
// currently holds -- the merge base for an update. An unapplied clean update
// governs it (its theirs, the folder as the update imported it); else the most
// recent finished apply's target; else "" (the copy's base, nothing applied).
// The caller holds applyMu.
func (q *Queue) folderBaseCheckpoint(team string, ws ids.WorkspaceID) (string, error) {
	if upd, ok, err := q.activeUpdate(team, ws); err != nil {
		return "", err
	} else if ok {
		return upd.TheirsCheckpoint, nil
	}
	to, ok, err := q.lastFinishedApplyTo(team, ws)
	if err != nil || !ok {
		return "", err
	}
	return q.checkpointForChangeset(team, ws, to)
}

// UpdateFolds are the head turn and the folded turns of an update finishing now:
// the workspace's unapplied turns, the latest kept as the head (which a later
// Apply targets as the merge) and the rest folded (no longer applied on their
// own). Read when the update RUNS, not when it was queued, so a turn that ran
// while it waited in the lane is included. head is empty when nothing is
// unapplied (the update re-synced the copy but there is no session work on top).
func (q *Queue) UpdateFolds(team string, ws ids.WorkspaceID) (head string, folded []string, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	turns, mark, err := q.appliedMark(team, ws)
	if err != nil {
		return "", nil, err
	}
	unapplied := turns[mark+1:]
	if len(unapplied) == 0 {
		return "", nil, nil
	}
	head = unapplied[len(unapplied)-1].turn
	folded = make([]string, 0, len(unapplied)-1)
	for _, t := range unapplied[:len(unapplied)-1] {
		folded = append(folded, t.turn)
	}
	return head, folded, nil
}

// MarkUpdated records on the update job its head turn (links.applies_turn) and
// the folded turns (links.applies_turns), so a later apply targets the head as
// the merge and the folded turns stop being appliable on their own
// (appliableTurns excludes them). Called at the finish, once the folds are known.
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
