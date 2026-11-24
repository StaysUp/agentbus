package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/staysup/agentbus/ids"
)

// ErrNotApplicable: the turn's changes cannot be applied. Its job has not
// ended, it is not the job's last attempt, or it did not run in an isolated
// copy with its changes recorded.
var ErrNotApplicable = errors.New("queue: the turn's changes cannot be applied")

// ErrMachineOffline: no runner on the folder's machine has asked for work
// within RunnerFresh, so a new apply of its folder would wait
// for nothing. No job is made.
var ErrMachineOffline = errors.New("queue: the folder's machine is offline")

// ErrApplyBusy: an apply of the session up to an earlier turn has not ended.
var ErrApplyBusy = errors.New("queue: an apply of this session has not ended")

// Applies are the apply jobs of turn, newest first.
func (q *Queue) Applies(team, turn string) ([]Job, error) {
	return q.appliesWhere(team, `json_extract(links, '$.applies_turn') = ?`, turn)
}

// sessionApplies are the apply jobs of the workspace ws, newest first. The
// caller resolves a session to its workspace first.
func (q *Queue) sessionApplies(team string, ws ids.WorkspaceID) ([]Job, error) {
	return q.appliesWhere(team, jobWorkspace+` = ?`, string(ws))
}

func (q *Queue) appliesWhere(team, where string, arg any) ([]Job, error) {
	rows, err := q.db.Query(`SELECT id FROM jobs WHERE team = ? AND kind = 'apply'
		AND `+where+` ORDER BY created DESC, id DESC`, team, arg)
	if err != nil {
		return nil, fmt.Errorf("queue: list applies: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("queue: list applies: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list applies: %w", err)
	}
	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		j, err := q.Get(team, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// sessionTurn is one turn of a session whose changes can be applied: the
// turn and its changeset, in the session's order.
type sessionTurn struct {
	turn, changeset string
}

// appliableTurns are the session's turns with a changeset, in the order
// they ran: a session's turn jobs run one at a time, so the order they
// ended in, which labels can make differ from the order they were made in.
// jobWorkspace is the workspace a turn job runs in, for scoping a copy's turns
// and applies: its spec workspace, or its session (its own workspace). Every
// session of one workspace shares the one copy, so apply and preview order all
// their turns together by this key, not by session.
const jobWorkspace = `COALESCE(NULLIF(json_extract(spec, '$.workspace'), ''), session)`

func (q *Queue) appliableTurns(team string, ws ids.WorkspaceID) ([]sessionTurn, error) {
	// A turn a finished discard dropped is gone from the copy, so it is not
	// appliable, previewable or droppable again; a turn a finished update folded
	// into the merge (its links.applies_turns, every folded turn but the update's
	// head turn) is no longer applied on its own either -- applying the head turn
	// writes the merge. Exclude both: every turn a finished discard or update of
	// this workspace named in links.applies_turns.
	rows, err := q.db.Query(`SELECT turn, COALESCE(json_extract(result, '$.changeset'), '') FROM jobs
		WHERE team = ? AND `+jobWorkspace+` = ? AND kind = 'turn' AND ended IS NOT NULL AND attempt IS NOT NULL
		AND turn NOT IN (
			SELECT je.value FROM jobs d, json_each(COALESCE(json_extract(d.links, '$.applies_turns'), '[]')) je
			WHERE d.team = ? AND COALESCE(NULLIF(json_extract(d.spec, '$.workspace'), ''), d.session) = ? AND d.kind IN ('discard','update') AND d.state = 'finished')
		ORDER BY ended, rowid`, team, string(ws), team, string(ws))
	if err != nil {
		return nil, fmt.Errorf("queue: list turns: %w", err)
	}
	defer rows.Close()
	var out []sessionTurn
	for rows.Next() {
		var turn, changeset string
		if err := rows.Scan(&turn, &changeset); err != nil {
			return nil, fmt.Errorf("queue: list turns: %w", err)
		}
		if changeset != "" {
			out = append(out, sessionTurn{turn: turn, changeset: changeset})
		}
	}
	return out, rows.Err()
}

func indexOf(turns []sessionTurn, turn string) int {
	for i, t := range turns {
		if t.turn == turn {
			return i
		}
	}
	return -1
}

// AppliedBy is the apply that put turn's changes in the folder: the newest
// apply that finished, of turn or of a later turn of its session, since an
// apply up to a turn applies every turn before it. Else the newest apply of
// turn, refused or not ended; ok is false when there is none.
func (q *Queue) AppliedBy(team, turn string) (job Job, ok bool, err error) {
	j, err := q.JobByTurn(team, turn)
	if err != nil {
		return Job{}, false, err
	}
	ws, err := q.workspaceID(team, ids.SessionID(j.Session))
	if err != nil {
		return Job{}, false, err
	}
	turns, err := q.appliableTurns(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	at := indexOf(turns, turn)
	applies, err := q.sessionApplies(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	for _, a := range applies {
		if a.State == Finished && at >= 0 && indexOf(turns, a.Links.AppliesTurn) >= at {
			return a, true, nil
		}
	}
	for _, a := range applies {
		if a.Links.AppliesTurn == turn {
			return a, true, nil
		}
	}
	return Job{}, false, nil
}

// Apply makes the job that makes the person's folder as the session was at
// the end of turn: every turn of the session not yet applied, up to and
// including turn, in one step, on the runner of the folder's machine. It starts no turn.
// It goes from the changeset of the last turn applied (none: the session's
// base) to turn's, so the folder's runner checks each file against where the
// folder should be now. A turn already applied, by its own apply or by one
// of a later turn, gets that job and created false: applying twice writes
// nothing more. So does a turn whose apply is running. An apply of an
// earlier turn still running is ErrApplyBusy. An apply that was refused or
// failed may be asked for again, as a new job.
func (q *Queue) Apply(team, turn string) (job Job, created bool, err error) {
	return q.ApplyAs(team, turn, nil)
}

// ApplyAs is Apply asked for by by, which the apply's turn.applied names.
func (q *Queue) ApplyAs(team, turn string, by *By) (job Job, created bool, err error) {
	// The applies of a session are read and the new one made as one step:
	// two at once must not both start from the same last applied turn.
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	return q.applyLocked(team, turn, by)
}

// ApplyWorkspace applies the workspace's changes up to turn, or up to its
// appliable head when turn is empty. The head is the queue's appliable cursor --
// the same source Preview and Discard read -- resolved under applyMu so the
// target cannot move under the apply, and NOT store.Workspace.HeadTurn, which is
// a second source that disagrees (a turn whose publish failed records
// turn.changed with an empty changeset; a pre-0.5 job stores a changeset with no
// turn.changed event). A turn of another workspace is ErrNotFound; a workspace
// with nothing the queue can apply is ErrAllApplied, unless an apply or discard
// is in flight (ErrApplyBusy) or the copy is closed (ErrSessionClosed).
func (q *Queue) ApplyWorkspace(team string, ws ids.WorkspaceID, turn string, by *By) (job Job, created bool, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	if turn == "" {
		head, _, ok, err := q.appliableCursor(team, ws)
		if err != nil {
			return Job{}, false, err
		}
		if !ok {
			// Nothing the queue can apply up to. Report a closed copy or an
			// apply-or-discard in flight the way an apply with a turn would, so
			// an empty-body apply is not silently 200 while a discard runs.
			if w, found, werr := q.Workspace(team, ws); werr != nil {
				return Job{}, false, werr
			} else if found && w.Closed != nil {
				return Job{}, false, ErrSessionClosed
			}
			if busy, berr := q.jobsInFlight(team, ws, `'discard','update'`); berr != nil {
				return Job{}, false, berr
			} else if busy {
				return Job{}, false, ErrApplyBusy
			}
			return Job{}, false, ErrAllApplied
		}
		turn = head
	} else {
		tws, werr := q.turnWorkspaceID(team, turn)
		if werr != nil {
			return Job{}, false, werr
		}
		if tws != ws {
			return Job{}, false, ErrNotFound
		}
	}
	return q.applyLocked(team, turn, by)
}

// turnWorkspaceID is the workspace a turn's job runs in. ErrNotFound when no
// job of the team has the turn.
func (q *Queue) turnWorkspaceID(team, turn string) (ids.WorkspaceID, error) {
	j, err := q.JobByTurn(team, turn)
	if err != nil {
		return "", err
	}
	return q.workspaceID(team, ids.SessionID(j.Session))
}

// applyLocked plans and creates an apply of turn. The caller holds applyMu.
func (q *Queue) applyLocked(team, turn string, by *By) (Job, bool, error) {
	p, done, has, err := q.planApply(team, turn, by)
	if err != nil {
		return Job{}, false, err
	}
	if has {
		return done, false, nil
	}
	if ok, err := q.MachineSeen(team, p.spec.Machine); err != nil {
		return Job{}, false, err
	} else if !ok {
		return Job{}, false, ErrMachineOffline
	}
	// The key counts the applies of turn that ended without applying, so
	// requests at once make one job, and one after a refusal makes a new
	// one. It holds a "/", which no key a caller sends may hold (ResultKey).
	return q.create(team, KindApply, ApplyKey(turn, p.refused), p.spec, p.links, Limits{}, string(p.spec.Session), nil)
}

// applyPlan is the apply job of a turn not applied yet: its spec (the
// folder, and the two changesets it goes between), its links (the turns it
// carries), and how many applies of the turn ended without applying.
type applyPlan struct {
	spec    Spec
	links   Links
	refused int
}

// planApply reads what an apply of turn's session up to turn would be: done
// is the apply that already covers turn (has true: being applied, or
// applied), else plan. Both Apply and its dry run (Preview) read it here, so
// a preview goes between the same two changesets as the apply after it.
// The caller holds applyMu.
func (q *Queue) planApply(team, turn string, by *By) (plan applyPlan, done Job, has bool, err error) {
	j, err := q.JobByTurn(team, turn)
	if err != nil {
		return plan, Job{}, false, err
	}
	if j.Kind != KindTurn || j.Ended == nil || j.Attempt == "" {
		return plan, Job{}, false, ErrNotApplicable
	}
	r, _ := ReadResult(j.Result)
	if r.Changeset == "" || j.Workspace == nil || (j.Workspace.Kind != WorkspaceCopy && j.Workspace.Kind != WorkspaceWorktree) {
		return plan, Job{}, false, ErrNotApplicable
	}
	if j.Workspace.Closed != nil {
		return plan, Job{}, false, ErrSessionClosed
	}
	ws, err := q.workspaceID(team, ids.SessionID(j.Session))
	if err != nil {
		return plan, Job{}, false, err
	}
	// Apply, discard and update all move the applied point or the copy, so one
	// runs per workspace: an apply while a discard or an update is in flight is
	// refused (ErrApplyBusy -> 409), the same way a discard is refused while an
	// apply runs (his call, 2026-11-22).
	if busy, err := q.jobsInFlight(team, ws, `'discard','update'`); err != nil {
		return plan, Job{}, false, err
	} else if busy {
		return plan, Job{}, false, ErrApplyBusy
	}
	first, err := q.firstSpec(team, ws)
	if err != nil {
		return plan, Job{}, false, err
	}
	if first.FolderRef == "" || first.Machine == "" {
		return plan, Job{}, false, ErrNotApplicable
	}
	turns, err := q.appliableTurns(team, ws)
	if err != nil {
		return plan, Job{}, false, err
	}
	at := indexOf(turns, turn)
	if at < 0 {
		return plan, Job{}, false, ErrNotApplicable
	}
	applies, err := q.sessionApplies(team, ws)
	if err != nil {
		return plan, Job{}, false, err
	}
	mark := -1
	lastAppliedTo := "" // what the folder holds now: the most recent finished apply's target
	for _, a := range applies {
		k := indexOf(turns, a.Links.AppliesTurn)
		switch {
		case a.Ended == nil && k >= at:
			return plan, a, true, nil // being applied, by this apply or a later one
		case a.Ended == nil:
			return plan, Job{}, false, ErrApplyBusy
		case a.State == Finished && k >= at:
			return plan, a, true, nil // applied already
		case a.State == Finished:
			mark = max(mark, k)
			if lastAppliedTo == "" { // applies are newest first: the first finished is the folder's state
				lastAppliedTo = a.Spec.ApplyTo
			}
		case a.Links.AppliesTurn == turn:
			plan.refused++
		}
	}
	// The baseline the apply starts from is what the folder holds now: the most
	// recent apply's target changeset. An active update (one with no apply or
	// discard after it) moved the copy to the folder's current state without
	// touching the folder, so until the next apply the baseline is the update's
	// recorded folder state (from), and applying up to the update's head turn
	// writes the merge (to) rather than that turn's own changeset.
	from := lastAppliedTo
	to := r.Changeset
	if upd, ok, uerr := q.activeUpdate(team, ws); uerr != nil {
		return plan, Job{}, false, uerr
	} else if ok {
		from = upd.From
		if turn == upd.HeadTurn {
			to = upd.To
		}
	}
	plan.spec = Spec{Session: ids.SessionID(j.Session), Workspace: j.Spec.Workspace, FolderRef: first.FolderRef, Machine: first.Machine, ApplyFrom: from, ApplyTo: to}
	carried := make([]string, 0, at-mark)
	for _, t := range turns[mark+1 : at+1] {
		carried = append(carried, t.turn)
	}
	plan.links = Links{AppliesTurn: turn, AppliesJob: j.ID, AppliesTurns: carried, ApplyBy: by}
	return plan, Job{}, false, nil
}

// ErrNotIsolated: the session works in the person's folder as it is, not in
// an isolated copy, so it has nothing to apply.
var ErrNotIsolated = errors.New("queue: the session does not run in an isolated copy")

// ErrAllApplied: every change of the session is applied, or it has none.
var ErrAllApplied = errors.New("queue: the session has no changes that are not applied")

// ErrNoCopyYet: the session's first turn asked for an isolated copy and has
// not ended, or ended without a workspace, so there is nothing to apply. The
// isolation setting says whether the copy is isolated: none runs it in the
// folder as it is.
var ErrNoCopyYet = errors.New("queue: the session has no copy yet")

// Preview makes the dry run (KindPreview) of the apply that an Apply of
// the session's latest turn with changes would make: the same spec, from
// planApply, on the runner of the folder's machine. turn is that turn. It
// is ErrNotFound for a session the queue does not hold; for one with no
// workspace yet (its first turn has not ended, or ended without one),
// ErrNotIsolated when that turn asked for the folder as it is, else
// ErrNoCopyYet; ErrNotIsolated,
// ErrSessionClosed, ErrAllApplied (with turn, when the session has one),
// ErrApplyBusy while an apply of the session has not ended, and
// ErrMachineOffline. Each preview is a new job: the folder may have changed
// since the last one.
func (q *Queue) Preview(team string, wsID ids.WorkspaceID) (job Job, turn string, applies int, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	ws, ok, err := q.Workspace(team, wsID)
	if err != nil {
		return Job{}, "", 0, err
	}
	switch {
	case !ok:
		// The queue writes a workspace's copy row when its first turn ends: a
		// workspace whose first turn has not ended has nothing to apply yet,
		// and is isolated as that turn asked.
		var n int
		if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND kind = 'turn'`,
			team, string(wsID)).Scan(&n); err != nil {
			return Job{}, "", 0, fmt.Errorf("queue: read workspace: %w", err)
		}
		if n == 0 {
			return Job{}, "", 0, ErrNotFound
		}
		first, err := q.firstSpec(team, wsID)
		if err != nil {
			return Job{}, "", 0, err
		}
		if first.Repo == "" && first.RepoRef == "" && !first.Isolate {
			return Job{}, "", 0, ErrNotIsolated
		}
		return Job{}, "", 0, ErrNoCopyYet
	case ws.Kind != WorkspaceCopy && ws.Kind != WorkspaceWorktree:
		return Job{}, "", 0, ErrNotIsolated
	case ws.Closed != nil:
		return Job{}, "", 0, ErrSessionClosed
	}
	// turn and applies are the workspace's appliable cursor, captured here under
	// applyMu so the preview this plans and the cursor the API records against
	// it are of the same moment: a turn or apply that lands after cannot slip
	// between them.
	turn, applies, has, err := q.appliableCursor(team, wsID)
	if err != nil {
		return Job{}, "", 0, err
	}
	if !has {
		return Job{}, "", 0, ErrAllApplied
	}
	p, done, applying, err := q.planApply(team, turn, nil)
	switch {
	case err != nil:
		return Job{}, turn, applies, err
	case applying && done.Ended == nil:
		return Job{}, turn, applies, ErrApplyBusy
	case applying:
		return Job{}, turn, applies, ErrAllApplied
	}
	if ok, err := q.MachineSeen(team, p.spec.Machine); err != nil {
		return Job{}, turn, applies, err
	} else if !ok {
		return Job{}, turn, applies, ErrMachineOffline
	}
	// A preview the API gave up on ended with nobody to read it. Preview jobs
	// are keyed by the workspace (every session of it shares the one copy).
	if err := q.dropPreviews(team, `session = ? AND cancel = 1`, string(wsID)); err != nil {
		return Job{}, turn, applies, err
	}
	job, _, err = q.create(team, KindPreview, "", p.spec, p.links, Limits{}, string(wsID), nil)
	return job, turn, applies, err
}

// appliableCursor is the session's appliable state: the latest turn with a
// changeset -- the one a preview or apply goes up to -- and how many apply
// jobs the session has. It is what a stored preview stays valid against: a new
// appliable turn moves the latest, and any apply moves the count. applies
// counts every apply job, pending or ended, a refusal included, because any
// apply may have changed the folder and its outcome is recorded in the store,
// not kept on the job; counting them all needs no second copy of that outcome
// and no read outside the queue, so the one source stays the queue under
// applyMu. What a re-run would show is read fresh then; when the runner is
// offline the last preview is served marked stale instead. A turn with no
// changeset -- not ended, or its upload failed -- is not appliable and moves
// neither number. ok is false when nothing is appliable. The caller holds
// applyMu.
func (q *Queue) appliableCursor(team string, ws ids.WorkspaceID) (turn string, applies int, ok bool, err error) {
	turns, err := q.appliableTurns(team, ws)
	if err != nil {
		return "", 0, false, err
	}
	if len(turns) == 0 {
		return "", 0, false, nil
	}
	// Count applies, discards and updates: each moves the folder's baseline or
	// the copy, so a new one must retake a stored preview (the preview reuse
	// cursor is this count).
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND kind IN ('apply','discard','update')`,
		team, string(ws)).Scan(&applies); err != nil {
		return "", 0, false, fmt.Errorf("queue: count applies: %w", err)
	}
	return turns[len(turns)-1].turn, applies, true, nil
}

// Discard queues a KindDiscard job that resets the workspace's copy to its
// Restore point -- the last applied turn's end checkpoint, or the copy's base
// when nothing was applied (Restore empty) -- on the runner holding the copy,
// dropping the unapplied turns. One apply-or-discard runs per workspace: a
// second apply or discard is refused at queue time with ErrApplyBusy (a plain
// 409), so no copy reset happens without a record. A running TURN is NOT refused
// -- the discard waits behind it in the lane, and WHAT it drops is computed when
// it runs (DroppedTurns, at the finish), so a turn that ran while it waited is
// dropped and marked too (his call, 2026-11-22). ErrAllApplied when nothing is
// unapplied, ErrMachineOffline when the machine is not seen. The applied point
// is stable while the discard is queued (no apply runs beside it), so Restore is
// read here. The caller (the API) checked the workspace is a sandbox.
func (q *Queue) Discard(team string, ws ids.WorkspaceID, machine string, by *By) (job Job, created bool, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	busy, err := q.jobsInFlight(team, ws, `'apply','discard','update'`)
	if err != nil {
		return Job{}, false, err
	}
	if busy {
		return Job{}, false, ErrApplyBusy
	}
	turns, mark, err := q.appliedMark(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	if mark == len(turns)-1 {
		// No ended unapplied turn. But a turn in flight will leave unapplied
		// changes in the copy, so the discard is not "nothing to do": it queues,
		// waits for the turn in the lane, and drops it at the finish. Only with no
		// turn in flight either is there truly nothing to discard.
		if inflight, err := q.jobsInFlight(team, ws, `'turn'`); err != nil {
			return Job{}, false, err
		} else if !inflight {
			return Job{}, false, ErrAllApplied
		}
	}
	restore := ""
	if mark >= 0 {
		// Reset to the checkpoint whose content the folder's last apply wrote --
		// which may be an update's merge, not the applied turn's own pre-merge
		// checkpoint. Using the turn checkpoint would reset the copy behind the
		// merge the folder holds, and a later turn + apply would then revert the
		// folder's imported content.
		to, ok, terr := q.lastFinishedApplyTo(team, ws)
		if terr != nil {
			return Job{}, false, terr
		}
		if ok {
			if restore, err = q.checkpointForChangeset(team, ws, to); err != nil {
				return Job{}, false, err
			}
		}
		if restore == "" {
			// The applied content has no checkpoint to reset to. Refuse rather than
			// let an empty Restore reset the copy all the way to base and drop
			// changes that were applied.
			return Job{}, false, ErrNotApplicable
		}
	}
	// The discard runs on the runner holding the copy (copy affinity routes it),
	// so its state -- not the folder machine's -- decides the discard, with the
	// right reason: a closed workspace, a removed runner, or one offline. Fall
	// back to the folder machine only when no copy row exists yet.
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
	spec := Spec{Workspace: ws, Machine: machine, Restore: restore}
	// The job carries no session (a discard is a workspace op, not a session's),
	// so events it produces name only the workspace. Its copy affinity keys by
	// the workspace (CopyKey), so create still routes it to the copy's runner.
	// AppliesTurns (the dropped turns) is filled at the finish, not here.
	return q.create(team, KindDiscard, "", spec, Links{ApplyBy: by}, Limits{}, "", nil)
}

// appliedMark is the workspace's appliable turns (every ended turn with a
// changeset not dropped by a finished discard) and the index of the last one a
// finished apply covered (-1 when none was applied). The caller holds applyMu.
func (q *Queue) appliedMark(team string, ws ids.WorkspaceID) (turns []sessionTurn, mark int, err error) {
	turns, err = q.appliableTurns(team, ws)
	if err != nil {
		return nil, -1, err
	}
	applies, err := q.sessionApplies(team, ws)
	if err != nil {
		return nil, -1, err
	}
	mark = -1
	for _, a := range applies {
		if a.State == Finished {
			if k := indexOf(turns, a.Links.AppliesTurn); k > mark {
				mark = k
			}
		}
	}
	return turns, mark, nil
}

// DroppedTurns are the unapplied turns a discard running now drops: the
// workspace's appliable turns after its last applied one. Read when the discard
// RUNS, not when it is queued, so a turn that ran while the discard waited in
// the lane is included.
func (q *Queue) DroppedTurns(team string, ws ids.WorkspaceID) ([]string, error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	turns, mark, err := q.appliedMark(team, ws)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(turns)-mark-1)
	for _, t := range turns[mark+1:] {
		out = append(out, t.turn)
	}
	return out, nil
}

// MarkDiscarded records on the discard job the turns it dropped, so they stop
// being appliable, previewable or droppable again (appliableTurns excludes a
// finished discard's dropped turns). Called at the finish, once the dropped
// turns are known -- a turn that ran while the discard waited is among them.
func (q *Queue) MarkDiscarded(team, jobID string, dropped []string) error {
	b, err := json.Marshal(dropped)
	if err != nil {
		return fmt.Errorf("queue: mark discarded: %w", err)
	}
	if _, err := q.db.Exec(`UPDATE jobs SET links = json_set(COALESCE(links, '{}'), '$.applies_turns', json(?)), updated = ?
		WHERE team = ? AND id = ? AND kind = 'discard'`, string(b), nanos(q.now()), team, jobID); err != nil {
		return fmt.Errorf("queue: mark discarded: %w", err)
	}
	return nil
}

// turnCheckpoint is turn's end checkpoint (Result.Checkpoint, or in a result
// from before 0.5 its changes' to), or empty for a turn with none or one the
// queue does not hold.
func (q *Queue) turnCheckpoint(team, turn string) (string, error) {
	var cp string
	err := q.db.QueryRow(`SELECT COALESCE(json_extract(result, '$.checkpoint'), json_extract(result, '$.changes.to'), '')
		FROM jobs WHERE team = ? AND turn = ? AND kind = 'turn'`, team, turn).Scan(&cp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("queue: read turn checkpoint: %w", err)
	}
	return cp, nil
}

// copyRunner is the state of the runner holding the workspace's copy, in ONE
// query with no filtering, so closed, removed and offline are distinguished (not
// conflated into one "seen" flag): the discard runs on that runner, so its state
// -- not the folder machine's -- decides the discard's fate, with the RIGHT
// reason. hasCopy is false (and the rest zero) when no copy row exists yet.
func (q *Queue) copyRunner(team string, ws ids.WorkspaceID) (closed, removed, fresh, hasCopy bool, name string, err error) {
	var cl, rm, seen sql.NullInt64
	var nm sql.NullString
	err = q.db.QueryRow(`SELECT s.closed, r.removed, r.seen, r.name FROM copies s JOIN runners r ON r.id = s.runner
		WHERE s.id = ? AND s.team = ?`, string(ws), team).Scan(&cl, &rm, &seen, &nm)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, false, "", nil
	}
	if err != nil {
		return false, false, false, false, "", fmt.Errorf("queue: read copy runner: %w", err)
	}
	fresh = seen.Valid && seen.Int64 >= nanos(q.now().Add(-RunnerFresh))
	return cl.Valid, rm.Valid, fresh, true, nm.String, nil
}

// jobsInFlight reports whether a job of one of kinds (an SQL list of quoted kind
// names) is queued, taken or running (not ended) in the workspace. Apply and
// discard both move the applied point, so at most one runs per workspace.
func (q *Queue) jobsInFlight(team string, ws ids.WorkspaceID, kinds string) (bool, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND `+jobWorkspace+` = ?
		AND kind IN (`+kinds+`) AND ended IS NULL`, team, string(ws)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("queue: jobs in flight: %w", err)
	}
	return n > 0, nil
}

// AppliableCursor is the session's appliable cursor (the latest appliable turn
// and the count of the session's apply jobs), read without planning or running
// a preview: the API reuses a stored preview only while this still equals the
// cursor it recorded.
func (q *Queue) AppliableCursor(team string, ws ids.WorkspaceID) (turn string, applies int, ok bool, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	return q.appliableCursor(team, ws)
}

// DropPreview removes a preview job of team that has ended, with its
// attempts: its result holds up to 1 MiB of diffs, and only the one GET
// that made it reads it. The API drops each preview once it has read it,
// and Preview drops the ones the API gave up on, so the jobs table keeps
// no finished preview.
func (q *Queue) DropPreview(team, id string) error {
	return q.dropPreviews(team, `id = ?`, id)
}

// dropPreviews removes team's ended preview jobs that match where (on
// jobs, with one argument), and their attempts and attempt events.
func (q *Queue) dropPreviews(team, where string, arg any) error {
	return q.dropJobs(team, KindPreview, where, arg)
}

// dropJobs removes team's ended jobs of kind that match where (on jobs,
// with one argument), and their attempts and attempt events.
func (q *Queue) dropJobs(team, kind, where string, arg any) error {
	jobs := `SELECT id FROM jobs WHERE team = ? AND kind = '` + kind + `' AND ended IS NOT NULL AND ` + where
	return q.tx(func(tx *sql.Tx) error {
		for _, del := range []string{
			`DELETE FROM attempt_events WHERE attempt IN (SELECT id FROM attempts WHERE job IN (` + jobs + `))`,
			`DELETE FROM attempts WHERE job IN (` + jobs + `)`,
			`DELETE FROM jobs WHERE id IN (` + jobs + `)`,
		} {
			if _, err := tx.Exec(del, team, arg); err != nil {
				return fmt.Errorf("queue: drop %s jobs: %w", kind, err)
			}
		}
		return nil
	})
}

// firstSpec is the spec of the session's first turn job: the folder it
// names.
func (q *Queue) firstSpec(team string, ws ids.WorkspaceID) (Spec, error) {
	var raw string
	err := q.db.QueryRow(`SELECT spec FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND kind = 'turn' ORDER BY created, rowid LIMIT 1`,
		team, string(ws)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Spec{}, nil
	}
	if err != nil {
		return Spec{}, fmt.Errorf("queue: read session: %w", err)
	}
	var sp Spec
	_ = json.Unmarshal([]byte(raw), &sp)
	return sp, nil
}

// ApplyKey is the idempotency key of the n-th apply job of turn. Like
// ResultKey it holds a "/", so no caller's key can take it first.
func ApplyKey(turn string, n int) string { return fmt.Sprintf("apply/%s/%d", turn, n) }
