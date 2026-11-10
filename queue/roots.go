package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// BlobRoots are the blobs the queue still names, from which every blob a
// session may still need is reached: the store keeps those and may remove
// the rest (jobs.Sweep).
type BlobRoots struct {
	// Trees are content trees: each snapshot's top tree and its .git tree.
	Trees []string
	// Changesets are changeset blobs (workspace.Changeset): each turn's,
	// and the two an apply goes between.
	Changesets []string
	// Blobs are named alone: each snapshot's blob of its files' times.
	Blobs []string
}

// BlobRoots reads the roots of every job but those of a closed session. A
// snapshot job belongs to the session of the turn whose attempt asked for
// it; a job of no session yet is kept.
func (q *Queue) BlobRoots() (BlobRoots, error) {
	var out BlobRoots
	rows, err := q.db.Query(`SELECT j.spec, j.result FROM jobs j
		WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.closed IS NOT NULL AND s.id = COALESCE(j.session,
			(SELECT t.session FROM attempts a JOIN jobs t ON t.id = a.job
				WHERE j.kind = 'snapshot' AND a.id = substr(j.idem_key, length('snapshot/') + 1))))`)
	if err != nil {
		return out, fmt.Errorf("queue: read blob roots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var spec string
		var result *string
		if err := rows.Scan(&spec, &result); err != nil {
			return out, fmt.Errorf("queue: read blob roots: %w", err)
		}
		var sp Spec
		if json.Unmarshal([]byte(spec), &sp) == nil {
			out.Changesets = appendSet(out.Changesets, sp.ApplyFrom, sp.ApplyTo)
		}
		if result == nil {
			continue
		}
		res, _ := ReadResult(json.RawMessage(*result))
		out.Changesets = appendSet(out.Changesets, res.Changeset)
		if s := res.Snapshot; s != nil {
			out.Trees = appendSet(out.Trees, s.ID, s.Git)
			out.Blobs = appendSet(out.Blobs, s.Times)
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("queue: read blob roots: %w", err)
	}
	// A session's own snapshot id, for a session made from one.
	srows, err := q.db.Query(`SELECT snapshot FROM sessions WHERE closed IS NULL AND snapshot <> ''`)
	if err != nil {
		return out, fmt.Errorf("queue: read blob roots: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var id string
		if err := srows.Scan(&id); err != nil {
			return out, fmt.Errorf("queue: read blob roots: %w", err)
		}
		out.Trees = appendSet(out.Trees, id)
	}
	return out, srows.Err()
}

func appendSet(to []string, hs ...string) []string {
	for _, h := range hs {
		if h != "" {
			to = append(to, h)
		}
	}
	return to
}

// Current says the attempt is the runner's and current: ErrNotFound,
// ErrStale and the like otherwise, as the other attempt calls answer. It
// answers the attempt's job.
func (q *Queue) Current(runnerID, attemptID string, number int) (Job, error) {
	var j Job
	err := q.tx(func(tx *sql.Tx) error {
		var err error
		_, j, err = current(tx, q.now(), runnerID, attemptID, number)
		return err
	})
	return j, err
}
