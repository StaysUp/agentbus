package queue

// A folder a person typed, on a control plane with no folders of its own
// (agentbus server): a folder job asks the runner of the machine named to
// check the path and register it, and its result is the folder's reference.
//
// One rule keeps the path off control's disk: it is never written. The job
// is stored without it; the path is held in memory, by job id, while the
// API waits for the answer, and Take hands it to that runner from there.
// ForgetFolder drops it. A control plane that restarts has forgotten it, so
// the runner is handed no path and refuses the job.

import (
	"database/sql"
	"fmt"
	"time"
)

// FolderWait bounds how long a folder job waits for its machine's runner: the API
// waits this long for the answer, and a job still queued after it ends.
const FolderWait = 15 * time.Second

// RegisterFolder makes a folder job: the runner of the machine checks the
// folder at path and registers it. The path is kept in memory only, until ForgetFolder.
func (q *Queue) RegisterFolder(team, machine, path string) (Job, error) {
	// Held while the job is made, so Take, which waits for this lock, never
	// hands the job out before its path is here.
	q.typedMu.Lock()
	defer q.typedMu.Unlock()
	job, _, err := q.create(team, KindFolder, "", Spec{Machine: machine}, Links{}, nil, "", nil)
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

// AskHome makes a home job (KindHome): the runner of the machine lists
// which of the usual config files its home holds. The API waits for it up
// to FolderWait, reads it, and drops it (DropHome).
func (q *Queue) AskHome(team, machine string) (Job, error) {
	// What an earlier ask left: a job its caller gave up on (cancelled)
	// that ended since. A job another caller is about to read was never
	// cancelled, and stays until that caller drops it.
	if err := q.dropJobs(team, KindHome, `cancel = ?`, 1); err != nil {
		return Job{}, err
	}
	job, _, err := q.create(team, KindHome, "", Spec{Machine: machine}, Links{}, nil, "", nil)
	return job, err
}

// DropHome removes the home job id once ended, with its attempts: what it
// listed is read once, and kept nowhere.
func (q *Queue) DropHome(team, id string) error {
	return q.dropJobs(team, KindHome, `id = ?`, id)
}

// endFoldersTx ends every folder or home job still queued FolderWait after
// it was made: no API waits for it any more.
func endFoldersTx(tx *sql.Tx, now time.Time) error {
	if _, err := tx.Exec(`UPDATE jobs SET state = ?, cancel = 1, updated = ?, ended = ? WHERE kind IN (?, ?) AND state = ? AND created <= ?`,
		Cancelled, nanos(now), nanos(now), KindFolder, KindHome, Queued, nanos(now.Add(-FolderWait))); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	return nil
}

// Machines is the id of every machine of the team whose runner was seen
// within RunnerFresh, sorted.
func (q *Queue) Machines(team string) ([]string, error) {
	// compat 0.6: drop connector. A runner of a build before 0.6 reports the
	// id under the old key, so read machine then connector.
	rows, err := q.db.Query(`SELECT DISTINCT COALESCE(json_extract(caps, '$.machine'), json_extract(caps, '$.connector')) FROM runners WHERE team = ? AND seen >= ? AND removed IS NULL
		AND COALESCE(json_extract(caps, '$.role'), '') = '' AND COALESCE(json_extract(caps, '$.machine'), json_extract(caps, '$.connector'), '') != '' ORDER BY 1`,
		team, nanos(q.now().Add(-RunnerFresh)))
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
