package store

import (
	"encoding/json"
	"fmt"

	"github.com/staysup/agentbus/event"
)

// PlanUsage is the latest plan.usage stored per vendor, subscription,
// connect and window, in the record's order. The gateway stores a
// reading only when it changes or goes stale (gateway/plan.go), so these
// are few.
func (s *Store) PlanUsage() ([]event.Event, error) {
	rows, err := s.r.Query(`SELECT data, MAX(seq) FROM events WHERE kind = 'plan.usage'
		GROUP BY json_extract(data, '$.vendor'), json_extract(data, '$.plan.subscription'),
			json_extract(data, '$.plan.connection'), json_extract(data, '$.plan.window')
		ORDER BY MAX(seq)`)
	if err != nil {
		return nil, fmt.Errorf("store: read plan usage: %w", err)
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var data string
		var seq int64
		if err := rows.Scan(&data, &seq); err != nil {
			return nil, fmt.Errorf("store: read plan usage: %w", err)
		}
		var e event.Event
		if json.Unmarshal([]byte(data), &e) != nil || e.Plan == nil {
			continue
		}
		e.Seq = seq
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read plan usage: %w", err)
	}
	return out, nil
}
