package store

// Control's own outcomes of a job, kept in the record (turn.changed,
// turn.applied, command.ran) and named from the job by their event id.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/staysup/agentbus/event"
)

// EventByID reads the event with id of session and turn (turn empty for an
// event of no turn). found is false when the record holds none.
func (s *Store) EventByID(session, turn, id string) (e event.Event, found bool, err error) {
	var data string
	err = s.r.QueryRow(`SELECT data FROM events WHERE session = ? AND turn = ? AND json_extract(data, '$.id') = ?
		ORDER BY seq LIMIT 1`, session, turn, id).Scan(&data)
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
