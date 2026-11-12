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
	j, err := q.JobByTurn(team, turn)
	if err != nil {
		return Job{}, false, err
	}
	if j.Kind != KindTurn || j.Ended == nil || j.Attempt == "" {
		return Job{}, false, ErrNotApplicable
	}
	r, _ := ReadResult(j.Result)
	if r.Changeset == "" || j.Workspace == nil || (j.Workspace.Kind != WorkspaceCopy && j.Workspace.Kind != WorkspaceWorktree) {
		return Job{}, false, ErrNotApplicable
	}
	if j.Workspace.Closed != nil {
		return Job{}, false, ErrSessionClosed
	}
	first, err := q.firstSpec(team, j.Session)
	if err != nil {
		return Job{}, false, err
	}
	if first.FolderRef == "" || first.Connector == "" {
		return Job{}, false, ErrNotApplicable
	}
	turns, err := q.appliableTurns(team, j.Session)
	if err != nil {
		return Job{}, false, err
	}
	at := indexOf(turns, turn)
	if at < 0 {
		return Job{}, false, ErrNotApplicable
	}
	applies, err := q.sessionApplies(team, j.Session)
	if err != nil {
		return Job{}, false, err
	}
	mark, refused := -1, 0
	for _, a := range applies {
		k := indexOf(turns, a.Links.AppliesTurn)
		switch {
		case a.Ended == nil && k >= at:
			return a, false, nil // being applied, by this apply or a later one
		case a.Ended == nil:
			return Job{}, false, ErrApplyBusy
		case a.State == Finished && k >= at:
			return a, false, nil // applied already
		case a.State == Finished:
			mark = max(mark, k)
		case a.Links.AppliesTurn == turn:
			refused++
		}
	}
	from := ""
	if mark >= 0 {
		from = turns[mark].changeset
	}
	// The key counts the applies of turn that ended without applying, so
	// requests at once make one job, and one after a refusal makes a new
	// one. It holds a "/", which no key a caller sends may hold (ResultKey).
	if ok, err := q.MachineSeen(team, first.Connector); err != nil {
		return Job{}, false, err
	} else if !ok {
		return Job{}, false, ErrMachineOffline
	}
	key := ApplyKey(turn, refused)
	spec := Spec{Session: j.Session, FolderRef: first.FolderRef, Connector: first.Connector, ApplyFrom: from, ApplyTo: r.Changeset}
	carried := make([]string, 0, at-mark)
	for _, t := range turns[mark+1 : at+1] {
		carried = append(carried, t.turn)
	}
	links := Links{AppliesTurn: turn, AppliesJob: j.ID, AppliesTurns: carried, ApplyBy: by}
	return q.create(team, KindApply, key, spec, links, Limits{}, j.Session)
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
