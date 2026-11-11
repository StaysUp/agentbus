package store

// The shell commands the person ran in a session between its turns
// (command.ran), for the session's next turn.

import (
	"encoding/json"
	"fmt"

	"github.com/staysup/agentbus/event"
)

// CommandsMax bounds how many commands CommandsSince reads: the newest.
const CommandsMax = 50

// CommandsSince reads the session's command.ran events stored after its
// latest turn.started, oldest first: what the person ran since the session's
// latest turn began. A next turn's own turn.started is stored after them, so
// the turn after it does not read them again.
func (s *Store) CommandsSince(session string) ([]event.Event, error) {
	rows, err := s.r.Query(`SELECT data FROM (SELECT seq, data FROM events
		WHERE session = ?1 AND kind = 'command.ran'
			AND seq > COALESCE((SELECT MAX(seq) FROM events WHERE session = ?1 AND kind = 'turn.started'), 0)
		ORDER BY seq DESC LIMIT ?2) ORDER BY seq`, session, CommandsMax)
	if err != nil {
		return nil, fmt.Errorf("store: read commands: %w", err)
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("store: read commands: %w", err)
		}
		var e event.Event
		if json.Unmarshal([]byte(data), &e) != nil || e.Command == nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OpenCommands are the ids of the commands the record has queued
// (command.queued) and holds no end for (command.ran or command.failed).
func (s *Store) OpenCommands() ([]string, error) {
	rows, err := s.r.Query(`SELECT json_extract(q.data, '$.command.id') FROM events q WHERE q.kind = 'command.queued'
		AND NOT EXISTS (SELECT 1 FROM events t WHERE t.session = q.session AND t.kind IN ('command.ran', 'command.failed')
			AND json_extract(t.data, '$.command.id') = json_extract(q.data, '$.command.id'))`)
	if err != nil {
		return nil, fmt.Errorf("store: read commands: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read commands: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
