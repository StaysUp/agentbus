package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/staysup/agentbus/ids"
)

// WorkspaceUsedAfter says whether the record holds a use of workspace ws after
// seq: a turn started in it, or an apply, discard, update, close or preview of
// it. A put-away reads it (queue/putaway.go) to do nothing when the workspace
// changed since the event that queued it. Its own workspace.kept is not a use,
// so one put-away that kept the copy never holds up a later one.
func (s *Store) WorkspaceUsedAfter(ws ids.WorkspaceID, after int64) (bool, error) {
	var used bool
	err := s.r.QueryRow(`SELECT EXISTS (SELECT 1 FROM events e WHERE e.seq > ?2 AND (
			(e.kind = 'turn.started' AND COALESCE(NULLIF(json_extract(e.data, '$.workspace'), ''), e.session) = ?1)
			OR (e.kind IN ('workspace.applied', 'workspace.discarded', 'workspace.updated', 'workspace.closed', 'changes.previewed')
				AND json_extract(e.data, '$.workspace') = ?1)))`, string(ws), after).Scan(&used)
	if err != nil {
		return false, fmt.Errorf("store: read workspace use: %w", err)
	}
	return used, nil
}

// KindEvent is one event of a kind, as an on over the record reads it: its id,
// seq, time and the workspace it names.
type KindEvent struct {
	ID        string
	Seq       int64
	Time      time.Time
	Workspace string
}

// EventsOfKind reads the events of kind with seq after after, oldest first, at
// most limit.
func (s *Store) EventsOfKind(kind string, after int64, limit int) ([]KindEvent, error) {
	rows, err := s.r.Query(`SELECT seq, time, data FROM events WHERE kind = ? AND seq > ? ORDER BY seq LIMIT ?`, kind, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	var out []KindEvent
	for rows.Next() {
		var e KindEvent
		var when int64
		var data string
		if err := rows.Scan(&e.Seq, &when, &data); err != nil {
			return nil, fmt.Errorf("store: read events: %w", err)
		}
		var head struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal([]byte(data), &head); err != nil {
			return nil, fmt.Errorf("store: read events: %w", err)
		}
		e.ID, e.Workspace, e.Time = head.ID, head.Workspace, time.Unix(0, when).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
