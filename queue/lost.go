package queue

// An apply whose runner was lost ends in the sweep (expire), with no
// finish, so the finisher never writes its turn.applied. The sweep marks
// it due in the job's own result (Result.OutcomeDue), in the same
// transaction that ends it; control's sweep then writes the event to the
// record and swaps the mark for the event's id (KeepOutcome): the record
// first, the pointer second, as for every outcome. Until then a reader
// reads the apply from its state, as for a job from before 0.5.

import (
	"database/sql"
	"fmt"
)

// markOutcomeDue marks job id's outcome as due (Result.OutcomeDue).
func markOutcomeDue(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`UPDATE jobs SET result = json_set(COALESCE(NULLIF(result, ''), '{}'), '$.outcome_due', json('true'))
		WHERE id = ? AND COALESCE(json_extract(links, '$.applies_turn'), '') != ''`, id); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	return nil
}

// OutcomesDue reads the ended jobs whose outcome is due, oldest first
// (by end, then id), past after when it is set, at most limit. A reader
// that could not write one reads on from it, so the later ones never wait
// behind it.
func (q *Queue) OutcomesDue(after *Job, limit int) ([]Job, error) {
	var ended int64
	var id string
	if after != nil && after.Ended != nil {
		ended, id = nanos(*after.Ended), after.ID
	}
	rows, err := q.db.Query(`SELECT `+jobCols+` FROM jobs WHERE ended IS NOT NULL AND json_extract(result, '$.outcome_due')
		AND (ended > ? OR (ended = ? AND id > ?)) ORDER BY ended, id LIMIT ?`, ended, ended, id, limit)
	if err != nil {
		return nil, fmt.Errorf("queue: read jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: read jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// KeepOutcome swaps job id's due mark for the id of the event that now
// holds its outcome in the record: an apply's turn.applied. A job not
// marked due is left as it is.
func (q *Queue) KeepOutcome(id, applied string) error {
	if _, err := q.db.Exec(`UPDATE jobs SET result = json_set(json_remove(result, '$.outcome_due'), '$.applied_event', ?)
		WHERE id = ? AND ended IS NOT NULL AND json_extract(result, '$.outcome_due')`, applied, id); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	return nil
}
