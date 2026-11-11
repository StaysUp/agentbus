package queue

// A folder a person typed, on a control plane with no connector of its own
// (agentbus server): a folder job asks the connector to check the path and
// register it, and its result is the folder's reference.
//
// One rule keeps the path off control's disk: it is never written. The job
// is stored without it; the path is held in memory, by job id, while the
// API waits for the answer, and Take hands it to the connector from there.
// ForgetFolder drops it. A control plane that restarts has forgotten it, so
// its connector is handed no path and refuses the job.

import (
	"database/sql"
	"fmt"
	"time"
)

// FolderWait bounds how long a folder job waits for its connector: the API
// waits this long for the answer, and a job still queued after it ends.
const FolderWait = 15 * time.Second

// RegisterFolder makes a folder job: the connector checks the folder at path
// and registers it. The path is kept in memory only, until ForgetFolder.
func (q *Queue) RegisterFolder(team, connector, path string) (Job, error) {
	// Held while the job is made, so Take, which waits for this lock, never
	// hands the job out before its path is here.
	q.typedMu.Lock()
	defer q.typedMu.Unlock()
	job, _, err := q.create(team, KindFolder, "", Spec{Connector: connector}, Links{}, Limits{}, "")
	if err != nil {
		return Job{}, err
	}
	if q.typed == nil {
		q.typed = map[string]string{}
	}
	q.typed[job.ID] = path
	return job, nil
}

// typedPath is the path of the folder job id, while it waits; "" after.
func (q *Queue) typedPath(id string) string {
	q.typedMu.Lock()
	defer q.typedMu.Unlock()
	return q.typed[id]
}

// ForgetFolder drops the path of the folder job id, and stops the job if it
// has not ended.
func (q *Queue) ForgetFolder(team, id string) error {
	q.typedMu.Lock()
	delete(q.typed, id)
	q.typedMu.Unlock()
	_, err := q.Cancel(team, id)
	return err
}

// endFoldersTx ends every folder job still queued FolderWait after it was
// made: no API waits for it any more.
func endFoldersTx(tx *sql.Tx, now time.Time) error {
	if _, err := tx.Exec(`UPDATE jobs SET state = ?, cancel = 1, updated = ?, ended = ? WHERE kind = ? AND state = ? AND created <= ?`,
		Cancelled, nanos(now), nanos(now), KindFolder, Queued, nanos(now.Add(-FolderWait))); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	return nil
}

// Connectors is the id of every connector of the team seen within
// RunnerFresh, sorted.
func (q *Queue) Connectors(team string) ([]string, error) {
	rows, err := q.db.Query(`SELECT DISTINCT json_extract(caps, '$.connector') FROM runners WHERE team = ? AND seen >= ?
		AND json_extract(caps, '$.role') = ? AND COALESCE(json_extract(caps, '$.connector'), '') != '' ORDER BY 1`,
		team, nanos(q.now().Add(-RunnerFresh)), RoleConnector)
	if err != nil {
		return nil, fmt.Errorf("queue: read runners: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("queue: read runners: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
