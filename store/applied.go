package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/staysup/agentbus/applied"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
)

// appliedSQL reads a workspace's events for Applied, oldest first: one read
// per kind, each through its index. A turn.changed belongs to the workspace
// its turn's turn.started names (resolvedWorkspace). Applied needs only that
// a turn.started is there, and four short fields of a turn.changed, so the
// first is read without its data and the second reduced to those fields in
// SQLite. A turn.changed carries the turn's whole file list with diffs (up
// to 830 KiB each, 2026-11-28), and decoding it for every workspace on every
// list was most of GET /api/v0/workspaces.
const appliedSQL = `SELECT kind, time, data FROM (
	SELECT e.seq, e.kind, e.time, '{}' AS data FROM events e
		WHERE e.kind = 'turn.started' AND COALESCE(NULLIF(json_extract(e.data, '$.workspace'), ''), e.session) = ?1
	UNION ALL
	SELECT c.seq, c.kind, c.time, json_object('turn', c.turn, 'changeset', json_extract(c.data, '$.changeset'),
			'checkpoint', json_extract(c.data, '$.checkpoint'), 'changes', json_object('to', json_extract(c.data, '$.changes.to')))
		FROM (SELECT DISTINCT ts.session, ts.turn FROM events ts
			WHERE ts.kind = 'turn.started' AND ` + resolvedWorkspace + ` = ?1) t
		JOIN events c ON c.kind = 'turn.changed' AND c.session = t.session AND c.turn = t.turn
	UNION ALL
	SELECT e.seq, e.kind, e.time, e.data FROM events e WHERE json_extract(e.data, '$.workspace') = ?1
		AND e.kind IN ('workspace.applied', 'workspace.discarded', 'workspace.updated', 'workspace.snapshot', 'workspace.closed')
	UNION ALL
	SELECT e.seq, e.kind, e.time, e.data FROM events e WHERE e.kind = 'turn.applied' AND e.session = ?1)
	ORDER BY seq`

// Applied is the workspace's applied state, folded from its events in the
// record's order (applied.Fold): its turns' turn.changed, its
// workspace.applied, workspace.discarded and workspace.updated, and, for a
// workspace migrated from a pre-workspace session, that session's
// turn.applied. A workspace.snapshot (a workspace recorded before 0.6.6)
// stands for everything before it. The record is the one owner of this state: the API reads it
// here (Workspace) and so does the queue, for apply, preview, discard and
// update.
func (s *Store) Applied(ws ids.WorkspaceID) (applied.State, error) {
	var f applied.Fold
	rows, err := s.r.Query(appliedSQL, string(ws))
	if err != nil {
		return applied.State{}, fmt.Errorf("store: read applied: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, data string
		var when int64
		if err := rows.Scan(&kind, &when, &data); err != nil {
			return applied.State{}, fmt.Errorf("store: read applied: %w", err)
		}
		var e event.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return applied.State{}, fmt.Errorf("store: read applied: %w", err)
		}
		switch event.Kind(kind) {
		case event.TurnStarted:
			f.Opened()
		case event.WorkspaceClosed:
			f.Closed(time.Unix(0, when).UTC())
		case event.TurnChanged:
			cp := e.Checkpoint
			if cp == "" && e.Changes != nil {
				cp = e.Changes.To
			}
			f.Changed(e.Turn, e.Changeset, cp)
		case event.WorkspaceSnapshot:
			if e.Snapshot != nil {
				f.Restore(*e.Snapshot)
			}
		case event.WorkspaceApplied, event.TurnApplied:
			ok := e.Applied != nil && e.Applied.Result == "applied"
			to := ""
			if e.Applied != nil {
				to = e.Applied.To
			}
			f.Applied(e.Turn, to, ok)
		case event.WorkspaceDiscarded:
			var dropped []string
			if e.Discarded != nil {
				dropped = e.Discarded.DroppedTurns
			}
			f.Discarded(time.Unix(0, when).UTC(), dropped)
		case event.WorkspaceUpdated:
			if u := e.Updated; u != nil && u.To != "" { // a conflict changed nothing
				f.Updated(applied.UpdateInForce{From: u.From, To: u.To, HeadTurn: u.HeadTurn,
					TheirsCheckpoint: u.TheirsCheckpoint, MergedCheckpoint: u.MergedCheckpoint},
					u.FoldedTurns, len(u.Unresolved) > 0)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return applied.State{}, fmt.Errorf("store: read applied: %w", err)
	}
	return f.State(), nil
}
