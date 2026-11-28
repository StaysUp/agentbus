package store

// Control's own outcomes of a job, kept in the record (turn.changed,
// turn.applied) and named from the job by their event id.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/staysup/agentbus/event"
)

// eventByIDSQL reads an event by its session, turn and id, through events_id.
const eventByIDSQL = `SELECT data FROM events WHERE session = ? AND turn = ? AND json_extract(data, '$.id') = ?
	ORDER BY seq LIMIT 1`

// EventByID reads the event with id of session and turn (turn empty for an
// event of no turn). found is false when the record holds none.
func (s *Store) EventByID(session, turn, id string) (e event.Event, found bool, err error) {
	var data string
	err = s.r.QueryRow(eventByIDSQL, session, turn, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("store: read event: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, false, fmt.Errorf("store: read event: %w", err)
	}
	return e, true, nil
}

// LatestPreview reads the workspace's newest changes.previewed event, and the
// seq it was stored at. found is false when the workspace has none. A preview
// is keyed by its workspace, not a session.
func (s *Store) LatestPreview(workspace string) (e event.Event, seq int64, found bool, err error) {
	var data string
	err = s.r.QueryRow(`SELECT seq, data FROM events WHERE kind = ?
		AND json_extract(data, '$.workspace') = ? ORDER BY seq DESC LIMIT 1`,
		string(event.ChangesPreviewed), workspace).Scan(&seq, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, 0, false, nil
	}
	if err != nil {
		return e, 0, false, fmt.Errorf("store: read preview: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, 0, false, fmt.Errorf("store: read preview: %w", err)
	}
	return e, seq, true, nil
}

// PreviewByID reads a changes.previewed of workspace by its event id, for paging
// a preview pinned by its event. A preview is keyed by its workspace (its
// session field is empty), so it is read by the workspace, not by EventByID
// (which keys by the session column).
func (s *Store) PreviewByID(workspace, id string) (e event.Event, found bool, err error) {
	var data string
	err = s.r.QueryRow(`SELECT data FROM events WHERE kind = ?
		AND json_extract(data, '$.workspace') = ? AND json_extract(data, '$.id') = ?
		ORDER BY seq LIMIT 1`, string(event.ChangesPreviewed), workspace, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("store: read preview: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, false, fmt.Errorf("store: read preview: %w", err)
	}
	return e, true, nil
}
