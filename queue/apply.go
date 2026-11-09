package queue

import (
	"errors"
	"fmt"
)

// ErrNotApplicable: the turn's changes cannot be applied. Its job has not
// ended, it is not the job's last attempt, or it did not run in an isolated
// copy with its checkpoints recorded.
var ErrNotApplicable = errors.New("queue: the turn's changes cannot be applied")

// Applies are the apply jobs of turn, newest first.
func (q *Queue) Applies(team, turn string) ([]Job, error) {
	rows, err := q.db.Query(`SELECT id FROM jobs WHERE team = ? AND kind = 'apply'
		AND json_extract(links, '$.applies_turn') = ? ORDER BY created DESC, id DESC`, team, turn)
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

// Apply makes the job that puts turn's changes into the person's folder. It
// runs on the runner that holds the turn's session, and starts no turn. A
// turn already applied, or being applied, gets that job and created false:
// applying twice writes nothing more. An apply that was refused or failed
// may be asked for again, as a new job.
func (q *Queue) Apply(team, turn string) (job Job, created bool, err error) {
	j, err := q.JobByAttempt(team, turn)
	if err != nil {
		return Job{}, false, err
	}
	if j.Kind != KindTurn || j.Ended == nil || j.Attempt != turn {
		return Job{}, false, ErrNotApplicable
	}
	r, _ := ReadResult(j.Result)
	if r.Changes == nil || r.Changes.From == "" || r.Changes.To == "" ||
		j.Workspace == nil || (j.Workspace.Kind != WorkspaceCopy && j.Workspace.Kind != WorkspaceWorktree) {
		return Job{}, false, ErrNotApplicable
	}
	if j.Workspace.Closed != nil {
		return Job{}, false, ErrSessionClosed
	}
	prev, err := q.Applies(team, turn)
	if err != nil {
		return Job{}, false, err
	}
	for _, p := range prev {
		if p.Ended == nil || p.State == Finished {
			return p, false, nil
		}
	}
	// The key counts the applies that ended without applying, so requests
	// at once make one job, and one after a refusal makes a new one. It
	// holds a "/", which no key a caller sends may hold (ResultKey).
	key := ApplyKey(turn, len(prev))
	spec := Spec{Session: j.Session, ApplyFrom: r.Changes.From, ApplyTo: r.Changes.To}
	links := Links{AppliesTurn: turn, AppliesJob: j.ID}
	return q.create(team, KindApply, key, spec, links, Limits{}, j.Session)
}

// ApplyKey is the idempotency key of the n-th apply job of turn. Like
// ResultKey it holds a "/", so no caller's key can take it first.
func ApplyKey(turn string, n int) string { return fmt.Sprintf("apply/%s/%d", turn, n) }
