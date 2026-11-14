package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// sessionApplies are the apply jobs of session, newest first.
func (q *Queue) sessionApplies(team, session string) ([]Job, error) {
	return q.appliesWhere(team, `session = ?`, session)
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
func (q *Queue) appliableTurns(team, session string) ([]sessionTurn, error) {
	rows, err := q.db.Query(`SELECT turn, COALESCE(json_extract(result, '$.changeset'), '') FROM jobs
		WHERE team = ? AND session = ? AND kind = 'turn' AND ended IS NOT NULL AND attempt IS NOT NULL
		ORDER BY ended, rowid`, team, session)
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
	turns, err := q.appliableTurns(team, j.Session)
	if err != nil {
		return Job{}, false, err
	}
	at := indexOf(turns, turn)
	applies, err := q.sessionApplies(team, j.Session)
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
	p, done, has, err := q.planApply(team, turn, by)
	if err != nil {
		return Job{}, false, err
	}
	if has {
		return done, false, nil
	}
	if ok, err := q.MachineSeen(team, p.spec.Connector); err != nil {
		return Job{}, false, err
	} else if !ok {
		return Job{}, false, ErrMachineOffline
	}
	// The key counts the applies of turn that ended without applying, so
	// requests at once make one job, and one after a refusal makes a new
	// one. It holds a "/", which no key a caller sends may hold (ResultKey).
	return q.create(team, KindApply, ApplyKey(turn, p.refused), p.spec, p.links, Limits{}, p.spec.Session)
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
	first, err := q.firstSpec(team, j.Session)
	if err != nil {
		return plan, Job{}, false, err
	}
	if first.FolderRef == "" || first.Connector == "" {
		return plan, Job{}, false, ErrNotApplicable
	}
	turns, err := q.appliableTurns(team, j.Session)
	if err != nil {
		return plan, Job{}, false, err
	}
	at := indexOf(turns, turn)
	if at < 0 {
		return plan, Job{}, false, ErrNotApplicable
	}
	applies, err := q.sessionApplies(team, j.Session)
	if err != nil {
		return plan, Job{}, false, err
	}
	mark := -1
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
		case a.Links.AppliesTurn == turn:
			plan.refused++
		}
	}
	from := ""
	if mark >= 0 {
		from = turns[mark].changeset
	}
	plan.spec = Spec{Session: j.Session, FolderRef: first.FolderRef, Connector: first.Connector, ApplyFrom: from, ApplyTo: r.Changeset}
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
func (q *Queue) Preview(team, session string) (job Job, turn string, err error) {
	q.applyMu.Lock()
	defer q.applyMu.Unlock()
	ws, ok, err := q.Workspace(team, session)
	if err != nil {
		return Job{}, "", err
	}
	switch {
	case !ok:
		// The queue writes a session's row when its first turn ends: a
		// session whose first turn has not ended has nothing to apply yet,
		// and is isolated as that turn asked.
		var n int
		if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND session = ? AND kind = 'turn'`,
			team, session).Scan(&n); err != nil {
			return Job{}, "", fmt.Errorf("queue: read session: %w", err)
		}
		if n == 0 {
			return Job{}, "", ErrNotFound
		}
		first, err := q.firstSpec(team, session)
		if err != nil {
			return Job{}, "", err
		}
		if first.Repo == "" && first.RepoRef == "" && !first.Isolate {
			return Job{}, "", ErrNotIsolated
		}
		return Job{}, "", ErrNoCopyYet
	case ws.Kind != WorkspaceCopy && ws.Kind != WorkspaceWorktree:
		return Job{}, "", ErrNotIsolated
	case ws.Closed != nil:
		return Job{}, "", ErrSessionClosed
	}
	turns, err := q.appliableTurns(team, session)
	if err != nil {
		return Job{}, "", err
	}
	if len(turns) == 0 {
		return Job{}, "", ErrAllApplied
	}
	turn = turns[len(turns)-1].turn
	p, done, has, err := q.planApply(team, turn, nil)
	switch {
	case err != nil:
		return Job{}, turn, err
	case has && done.Ended == nil:
		return Job{}, turn, ErrApplyBusy
	case has:
		return Job{}, turn, ErrAllApplied
	}
	if ok, err := q.MachineSeen(team, p.spec.Connector); err != nil {
		return Job{}, turn, err
	} else if !ok {
		return Job{}, turn, ErrMachineOffline
	}
	// A preview the API gave up on ended with nobody to read it.
	if err := q.dropPreviews(team, `session = ? AND cancel = 1`, session); err != nil {
		return Job{}, turn, err
	}
	job, _, err = q.create(team, KindPreview, "", p.spec, p.links, Limits{}, session)
	return job, turn, err
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
	jobs := `SELECT id FROM jobs WHERE team = ? AND kind = '` + KindPreview + `' AND ended IS NOT NULL AND ` + where
	return q.tx(func(tx *sql.Tx) error {
		for _, del := range []string{
			`DELETE FROM attempt_events WHERE attempt IN (SELECT id FROM attempts WHERE job IN (` + jobs + `))`,
			`DELETE FROM attempts WHERE job IN (` + jobs + `)`,
			`DELETE FROM jobs WHERE id IN (` + jobs + `)`,
		} {
			if _, err := tx.Exec(del, team, arg); err != nil {
				return fmt.Errorf("queue: drop previews: %w", err)
			}
		}
		return nil
	})
}

// firstSpec is the spec of the session's first turn job: the folder it
// names.
func (q *Queue) firstSpec(team, session string) (Spec, error) {
	var raw string
	err := q.db.QueryRow(`SELECT spec FROM jobs WHERE team = ? AND session = ? AND kind = 'turn' ORDER BY created, rowid LIMIT 1`,
		team, session).Scan(&raw)
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
