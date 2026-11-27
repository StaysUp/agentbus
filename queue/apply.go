package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/staysup/agentbus/applied"
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

// jobWorkspace is the workspace a job runs in, for scoping a copy's jobs: its
// spec workspace, or its session (its own workspace). Every session of one
// workspace shares the one copy, so its jobs are scoped by this key, not by
// session.
const jobWorkspace = `COALESCE(NULLIF(json_extract(spec, '$.workspace'), ''), session)`

// coveringApply is the finished apply job the record says went up to the
// record's apply of turn at index at or later: the newest one whose turn the
// record places at or after at. ok is false when the record holds no such
// apply, or its job is not finished yet (the record is written a moment
// before the job ends).
func coveringApply(rec applied.State, applies []Job, at int) (Job, bool) {
	if at < 0 || at > rec.Mark {
		return Job{}, false
	}
	for _, a := range applies { // newest first
		if a.State == Finished && rec.Index(a.Links.AppliesTurn) >= at {
			return a, true
		}
	}
	return Job{}, false
}

// AppliedBy is the apply that put turn's changes in the folder: the newest
// apply that finished, of turn or of a later turn of its session, since an
// apply up to a turn applies every turn before it. Else the newest apply of
// turn, refused or not ended; ok is false when there is none.
func (q *Queue) AppliedBy(team, turn string) (job Job, ok bool, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	j, err := q.JobByTurn(team, turn)
	if err != nil {
		return Job{}, false, err
	}
	ws, err := q.workspaceID(team, ids.SessionID(j.Session))
	if err != nil {
		return Job{}, false, err
	}
	rec, err := q.appliedState(ws)
	if err != nil {
		return Job{}, false, err
	}
	applies, err := q.sessionApplies(team, ws)
	if err != nil {
		return Job{}, false, err
	}
	if a, ok := coveringApply(rec, applies, rec.Index(turn)); ok {
		return a, true, nil
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
// appliable head when turn is empty. The head is the record's (applied.State,
// the same fold store.Workspace.HeadTurn shows), read under applyMu so the
// target cannot move under the apply. A turn of another workspace is
// ErrNotFound; a workspace
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
			rec, err := q.appliedState(ws)
			if err != nil {
				return Job{}, false, err
			}
			if rec.ClosedAt != nil {
				return Job{}, false, ErrSessionClosed
			}
			// Nothing the queue can apply up to. Report a closed copy or an
			// apply-or-discard in flight the way an apply with a turn would, so
			// an empty-body apply is not silently 200 while a discard runs.
			if w, found, werr := q.Workspace(team, ws); werr != nil {
				return Job{}, false, werr
			} else if found && w.Closed != nil {
				return Job{}, false, ErrSessionClosed
			}
			if busy, berr := q.jobsInFlight(team, ws, `'discard','update','close'`); berr != nil {
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
	return q.create(team, KindApply, ApplyKey(turn, p.refused), p.spec, p.links, nil, string(p.spec.Session), nil)
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
	ws, err := q.workspaceID(team, ids.SessionID(j.Session))
	if err != nil {
		return plan, Job{}, false, err
	}
	// What is applied and what is appliable is the record's (applied.State);
	// the job rows say only how each apply job stands.
	rec, err := q.appliedState(ws)
	if err != nil {
		return plan, Job{}, false, err
	}
	if !rec.Changed(turn) || j.Workspace == nil || (j.Workspace.Kind != WorkspaceCopy && j.Workspace.Kind != WorkspaceWorktree) {
		return plan, Job{}, false, ErrNotApplicable
	}
	if j.Workspace.Closed != nil {
		return plan, Job{}, false, ErrSessionClosed
	}
	// Apply, discard and update all move the applied point or the copy, so one
	// runs per workspace: an apply while a discard or an update is in flight is
	// refused (ErrApplyBusy -> 409), the same way a discard is refused while an
	// apply runs (his call, 2026-11-22).
	if busy, err := q.jobsInFlight(team, ws, `'discard','update','close'`); err != nil {
		return plan, Job{}, false, err
	} else if busy {
		return plan, Job{}, false, ErrApplyBusy
	}
	first, err := q.firstSpec(team, ws)
	if err != nil {
		return plan, Job{}, false, err
	}
	if len(first.Places()) == 0 || first.Machine == "" {
		return plan, Job{}, false, ErrNotApplicable
	}
	at := rec.Index(turn)
	if at < 0 {
		return plan, Job{}, false, ErrNotApplicable
	}
	applies, err := q.sessionApplies(team, ws)
	if err != nil {
		return plan, Job{}, false, err
	}
	// An apply job not ended yet: the record has no outcome of it to read.
	for _, a := range applies {
		if a.Ended == nil {
			if rec.Index(a.Links.AppliesTurn) >= at {
				return plan, a, true, nil // being applied, by this apply or a later one
			}
			return plan, Job{}, false, ErrApplyBusy
		}
	}
	if a, ok := coveringApply(rec, applies, at); ok {
		return plan, a, true, nil // applied already
	}
	// The applies of turn that ended without applying, for the next one's key.
	for _, a := range applies {
		if a.State != Finished && a.Links.AppliesTurn == turn {
			plan.refused++
		}
	}
	// The baseline the apply starts from is what the folder holds now: the
	// latest successful apply's target changeset. An active update (one with no
	// apply or discard after it) moved the copy to the folder's current state
	// without touching the folder, so until the next apply the baseline is the
	// update's recorded folder state (from), and applying up to the update's
	// head turn writes the merge (to) rather than that turn's own changeset.
	from := rec.AppliedTo
	to := rec.Turns[at].Changeset
	if upd := rec.Update; upd != nil {
		from = upd.From
		if turn == upd.HeadTurn {
			to = upd.To
		}
	}
	plan.spec = Spec{Session: ids.SessionID(j.Session), Workspace: j.Spec.Workspace, FolderRef: first.FolderRef, Folders: first.Folders, Machine: first.Machine, ApplyFrom: from, ApplyTo: to, CheckMarkers: rec.Markers}
	carried := make([]string, 0, at-rec.Mark)
	for _, t := range rec.Turns[rec.Mark+1 : at+1] {
		carried = append(carried, t.Turn)
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
	rec, err := q.appliedState(wsID)
	if err != nil {
		return Job{}, "", 0, err
	}
	if rec.ClosedAt != nil {
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
	job, _, err = q.create(team, KindPreview, "", p.spec, p.links, nil, string(wsID), nil)
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
	rec, err := q.appliedState(ws)
	if err != nil {
		return "", 0, false, err
	}
	if len(rec.Turns) == 0 {
		return "", 0, false, nil
	}
	// Count applies, discards and updates: each moves the folder's baseline or
	// the copy, so a new one must retake a stored preview (the preview reuse
	// cursor is this count).
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND kind IN ('apply','discard','update','close')`,
		team, string(ws)).Scan(&applies); err != nil {
		return "", 0, false, fmt.Errorf("queue: count applies: %w", err)
	}
	return rec.Head(), applies, true, nil
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
	busy, err := q.jobsInFlight(team, ws, `'apply','discard','update','close'`)
	if err != nil {
		return Job{}, false, err
	}
	if busy {
		return Job{}, false, ErrApplyBusy
	}
	rec, err := q.appliedState(ws)
	if err != nil {
		return Job{}, false, err
	}
	if rec.Mark == len(rec.Turns)-1 {
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
	if rec.Mark >= 0 {
		// Reset to the checkpoint whose content the folder's last apply wrote --
		// which may be an update's merge, not the applied turn's own pre-merge
		// checkpoint. Using the turn checkpoint would reset the copy behind the
		// merge the folder holds, and a later turn + apply would then revert the
		// folder's imported content.
		if rec.Applied {
			restore = rec.CheckpointOf(rec.AppliedTo)
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
	return q.create(team, KindDiscard, "", spec, Links{ApplyBy: by}, nil, "", nil)
}

// DroppedTurns are the unapplied turns a discard running now drops: the
// workspace's appliable turns after its last applied one, as the record holds
// them. Read when the discard RUNS, not when it is queued, so a turn that ran
// while the discard waited in the lane is included. The caller is inside
// Outcome.
func (q *Queue) DroppedTurns(team string, ws ids.WorkspaceID) ([]string, error) {
	rec, err := q.appliedState(ws)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rec.Turns)-rec.Mark-1)
	for _, t := range rec.Turns[rec.Mark+1:] {
		out = append(out, t.Turn)
	}
	return out, nil
}

// MarkDiscarded records on the discard job the turns it dropped, for the job's
// own answer (GET of the discard). They stop being appliable through the
// record's workspace.discarded, not through this. Called at the finish, once
// the dropped turns are known -- a turn that ran while the discard waited is
// among them.
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
	// A put-away a runner holds is a close in progress; one still queued is not
	// busy work: it is decided when due and keeps the copy if anything happened.
	err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND `+jobWorkspace+` = ?
		AND (kind IN (`+kinds+`) OR (kind = 'put_away' AND state IN ('taken','running'))) AND ended IS NULL`, team, string(ws)).Scan(&n)
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

// Outcome runs fn, which puts a job's outcome into the record and then ends the
// job, as one step for every reader of the applied state: apply, preview,
// discard and update read the record and the job rows under the same lock, so
// none sees an outcome in the record whose job has not ended, or the reverse.
// Control's finisher calls it for each job it ends.
func (q *Queue) Outcome(fn func() error) error {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	return fn()
}

// OutcomeJobs are team's ended turn, apply, discard and update jobs, in the
// order they ended: what the queue held of each workspace's applied state
// before the record held it (0.6.6). Only the record's one-time snapshot of
// that state reads them (jobs.MigrateApplied).
func (q *Queue) OutcomeJobs(team string) ([]Job, error) {
	rows, err := q.db.Query(`SELECT id FROM jobs WHERE team = ? AND kind IN ('turn','apply','discard','update','close')
		AND ended IS NOT NULL ORDER BY ended, rowid`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: list outcome jobs: %w", err)
	}
	var jobIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("queue: list outcome jobs: %w", err)
		}
		jobIDs = append(jobIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list outcome jobs: %w", err)
	}
	out := make([]Job, 0, len(jobIDs))
	for _, id := range jobIDs {
		j, err := q.Get(team, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
