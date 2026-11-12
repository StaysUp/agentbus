package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
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
	if err := out.addJobs(rows); err != nil {
		return out, err
	}
	// A session's own snapshot id, for a session made from one.
	srows, err := q.db.Query(`SELECT snapshot FROM sessions WHERE closed IS NULL AND snapshot <> ''`)
	if err != nil {
		return out, fmt.Errorf("queue: read blob roots: %w", err)
	}
	return out, out.addSnapshots(srows)
}

// RunnerBlobRoots are the roots of the blobs a runner may read: those of
// every job of each session it holds an attempt of that has not ended or
// ended within recent, the snapshot jobs of those sessions' attempts
// included; of each job it holds so with no session; and each such
// session's own snapshot. Its apply and snapshot jobs, of its machine's
// folders, are its jobs too.
func (q *Queue) RunnerBlobRoots(runnerID string, recent time.Duration) (BlobRoots, error) {
	var out BlobRoots
	since := nanos(q.now().Add(-recent))
	const held = `WITH held AS (SELECT j.id, COALESCE(j.session, (SELECT t.session FROM attempts sa JOIN jobs t ON t.id = sa.job
			WHERE j.kind = 'snapshot' AND sa.id = substr(j.idem_key, length('snapshot/') + 1))) AS session
		FROM attempts r JOIN jobs j ON j.id = r.job WHERE r.runner = ? AND (r.ended IS NULL OR r.ended >= ?)),
	ss AS (SELECT DISTINCT session FROM held WHERE session IS NOT NULL) `
	rows, err := q.db.Query(held+`
		SELECT spec, result FROM jobs WHERE id IN (SELECT id FROM held)
		UNION ALL
		SELECT spec, result FROM jobs WHERE session IN (SELECT session FROM ss)
		UNION ALL
		SELECT s.spec, s.result FROM jobs t JOIN attempts a ON a.job = t.id
			JOIN jobs s ON s.team = t.team AND s.idem_key = 'snapshot/' || a.id AND s.kind = 'snapshot'
			WHERE t.session IN (SELECT session FROM ss)`, runnerID, since)
	if err != nil {
		return out, fmt.Errorf("queue: read blob roots: %w", err)
	}
	if err := out.addJobs(rows); err != nil {
		return out, err
	}
	srows, err := q.db.Query(held+`SELECT snapshot FROM sessions WHERE id IN (SELECT session FROM ss) AND snapshot <> ''`, runnerID, since)
	if err != nil {
		return out, fmt.Errorf("queue: read blob roots: %w", err)
	}
	return out, out.addSnapshots(srows)
}

// addJobs adds the roots of each job's spec and result that rows reads.
func (out *BlobRoots) addJobs(rows *sql.Rows) error {
	defer rows.Close()
	for rows.Next() {
		var spec string
		var result *string
		if err := rows.Scan(&spec, &result); err != nil {
			return fmt.Errorf("queue: read blob roots: %w", err)
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
		return fmt.Errorf("queue: read blob roots: %w", err)
	}
	return nil
}

// addSnapshots adds each session snapshot id rows reads as a tree.
func (out *BlobRoots) addSnapshots(rows *sql.Rows) error {
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("queue: read blob roots: %w", err)
		}
		out.Trees = appendSet(out.Trees, id)
	}
	return rows.Err()
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
