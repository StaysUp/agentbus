package queue

// A session runs on its runner for its whole life (Take: a job of a known
// session goes to the session's runner). When the person removes that
// runner (RemoveRunner), the session's work has nowhere to go: its copy is
// on that machine. So the session takes no more turns or reviews, with a
// plain reason naming the machine: a new one is refused (create), and one
// waiting ends with that reason (endRemovedTx), at the removal and in every
// sweep after it. An apply is the folder's machine's job, not the runner's, so it
// is not held up by the removal and is left as it is.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// RunnerRemovedError: a job for a session whose runner was removed.
type RunnerRemovedError struct {
	Machine string // the removed runner's name
}

func (e *RunnerRemovedError) Error() string { return RunnerRemovedWhy(e.Machine) }

// RunnerRemovedWhy is why a session whose runner was removed takes no more
// work, naming the machine.
func RunnerRemovedWhy(machine string) string {
	return "this session ran on the machine " + strconv.Quote(machine) +
		", which was removed, so the session takes no more turns; start a new session"
}

// folderKind says a job of kind is a folder job (folderKinds).
func folderKind(kind string) bool {
	return kind == KindApply || kind == KindFolder
}

// endRemovedTx ends every queued job, but folder jobs, of a session whose
// runner was removed, with why naming the machine: no other runner takes
// them. It is the one place the rule is applied, at the removal and in
// every sweep (sweepTx), so a job that becomes queued later (a taken
// attempt that expires back to the queue, a first turn's finish that ties
// its session to the runner after the removal) ends too.
func endRemovedTx(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT DISTINCT r.id, r.name FROM jobs j
		JOIN sessions s ON s.id = j.session AND s.team = j.team
		JOIN runners r ON r.id = s.runner
		WHERE j.state = 'queued' AND j.kind NOT IN ` + folderKinds + ` AND r.removed IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("queue: read jobs: %w", err)
	}
	type gone struct{ id, name string }
	var list []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.name); err != nil {
			rows.Close()
			return fmt.Errorf("queue: read jobs: %w", err)
		}
		list = append(list, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range list {
		res, _ := json.Marshal(Result{Refused: RunnerRemovedWhy(g.name), Status: 409}) // 409 Conflict, as the API answers it
		if _, err := tx.Exec(`UPDATE jobs SET state = ?, reason = ?, result = ?, updated = ?, ended = ?
			WHERE state = 'queued' AND kind NOT IN `+folderKinds+`
			AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = jobs.session AND s.team = jobs.team AND s.runner = ?)`,
			Failed, ReasonSetup, string(res), nanos(now), nanos(now), g.id); err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
	}
	return nil
}
