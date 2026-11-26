package queue

// Putting a copy away: the close AgentBus asks for itself, so a person never
// has to. The built-in on (recordon.go) queues a put_away job in a sandbox
// workspace's lane whenever its work is applied or discarded, not before the
// quiet time has passed (jobs.not_before). Until then it holds no place in the
// lane: the person's next turn runs at once.
//
// When the job comes due, control decides it inside the take of the runner
// that holds the copy (decidePutAways), from the record and the lane, before
// any job is chosen. The copy goes only when all of these hold:
//
//   - no other job of the workspace is waiting or running (a turn, an apply, a
//     discard, an update, a close, a later put-away);
//   - the record holds nothing that happened in the workspace after the event
//     that queued it (a turn started, an apply, a discard, an update, a close,
//     a preview);
//   - the record's applied state is settled: nothing unapplied
//     (applied.State.Settled). A copy with unapplied work is never touched.
//
// Otherwise the job ends kept, with why, and control writes workspace.kept to
// the record (jobs.CloseLostApplies). When it goes, the take hands it to the
// runner in the same transaction, so nothing can enter the lane between the
// decision and the job; the runner removes the copy as for a close, and control
// records workspace.closed with the put-away's why. Sessions and the record
// stay; the next turn makes a fresh copy, as after Close.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/staysup/agentbus/ids"
)

// ActivityReader says whether the record holds an event of workspace ws after
// seq that is a use of the workspace (a turn started in it, an apply, discard,
// update, close or preview of it).
type ActivityReader func(ws ids.WorkspaceID, after int64) (bool, error)

// ReadActivityWith sets how the queue reads a workspace's activity from the
// record, for a put-away. Set it before the queue is used. Without it no
// put-away goes: each one is kept. Take calls it, and the applied reader,
// inside its own transaction, on the queue's one connection: they read the
// record only and never call back into the queue.
func (q *Queue) ReadActivityWith(r ActivityReader) { q.activity = r }

// Why a put-away kept its copy.
const (
	KeptBusy      = "other work of the workspace is waiting or running"
	KeptUsed      = "the workspace was used after the event that queued this"
	KeptUnapplied = "the workspace holds work that is not applied or discarded"
	KeptNoCopy    = "the workspace has no copy to put away"
	KeptNoRecord  = "the record could not be read, so the copy stays"
)

// decidePutAways decides each put-away that is due and whose copy runner is
// runnerID: one that may not go ends kept, with why, its outcome due for the
// record; one that may go is left queued, for this take to choose. A put-away
// whose workspace has no copy row ends kept too: there is nothing on disk.
func (q *Queue) decidePutAways(tx *sql.Tx, runnerID, team string, now time.Time) error {
	rows, err := tx.Query(`SELECT j.id, j.links, `+jobCopyKey("j")+`,
			EXISTS (SELECT 1 FROM copies s WHERE s.id = `+jobCopyKey("j")+` AND s.team = j.team)
		FROM jobs j WHERE j.team = ? AND j.kind = 'put_away' AND j.state = 'queued'
		AND j.not_before <= `+strconv.FormatInt(nanos(now), 10)+`
		AND (NOT EXISTS (SELECT 1 FROM copies s WHERE s.id = `+jobCopyKey("j")+` AND s.team = j.team)
			OR EXISTS (SELECT 1 FROM copies s WHERE s.id = `+jobCopyKey("j")+` AND s.team = j.team AND s.runner = ?))
		ORDER BY j.rowid`, team, runnerID)
	if err != nil {
		return fmt.Errorf("queue: read put-aways: %w", err)
	}
	type due struct {
		id, links, ws string
		hasCopy       bool
	}
	var ds []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.links, &d.ws, &d.hasCopy); err != nil {
			rows.Close()
			return fmt.Errorf("queue: read put-aways: %w", err)
		}
		ds = append(ds, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("queue: read put-aways: %w", err)
	}
	for _, d := range ds {
		var l Links
		_ = json.Unmarshal([]byte(d.links), &l)
		why := KeptNoCopy
		if d.hasCopy {
			if why, err = q.putAwayWhy(tx, team, d.id, ids.WorkspaceID(d.ws), l.Because); err != nil {
				return err
			}
		}
		if why == "" {
			continue // it goes: this take may choose it
		}
		res, _ := json.Marshal(Result{Kept: why, OutcomeDue: true})
		if _, err := tx.Exec(`UPDATE jobs SET state = ?, result = ?, updated = ?, ended = ? WHERE id = ? AND state = 'queued'`,
			Finished, string(res), nanos(now), nanos(now), d.id); err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
	}
	return nil
}

// putAwayWhy is why the put-away job id must keep workspace ws's copy, or ""
// when it may go.
func (q *Queue) putAwayWhy(tx *sql.Tx, team, id string, ws ids.WorkspaceID, because *Because) (string, error) {
	var busy int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND `+jobWorkspace+` = ? AND id != ? AND ended IS NULL
		AND kind IN ('turn','apply','discard','update','close','put_away')`, team, string(ws), id).Scan(&busy); err != nil {
		return "", fmt.Errorf("queue: read put-away lane: %w", err)
	}
	if busy > 0 {
		return KeptBusy, nil
	}
	if q.activity == nil || q.applied == nil || because == nil {
		return KeptNoRecord, nil
	}
	used, err := q.activity(ws, because.Seq)
	if err != nil {
		return KeptNoRecord, nil
	}
	if used {
		return KeptUsed, nil
	}
	st, err := q.applied(ws)
	if err != nil {
		return KeptNoRecord, nil
	}
	if !st.Settled() {
		return KeptUnapplied, nil
	}
	return "", nil
}
