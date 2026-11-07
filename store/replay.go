package store

// What a replayed turn reads: its own turn.started, the session's turns, and
// the calls of an earlier turn that can end its history.

import (
	"database/sql"
	"errors"
	"fmt"
)

// TurnStart reads the turn.started of (session, turn): the person's words and
// the earlier turn it replays, if any. found is false when there is none.
func (s *Store) TurnStart(session, turn string) (prompt, replayFrom string, found bool, err error) {
	var p, r sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.text'), json_extract(data, '$.replay_from') FROM events
		WHERE session = ? AND turn = ? AND kind = 'turn.started' ORDER BY seq LIMIT 1`, session, turn).Scan(&p, &r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read a turn: %w", err)
	}
	return p.String, r.String, true, nil
}

// SessionTurns lists the session's turn ids, newest first.
func (s *Store) SessionTurns(session string) ([]string, error) {
	rows, err := s.r.Query(`SELECT turn FROM events WHERE session = ? AND kind = 'turn.started' AND turn <> ''
		ORDER BY seq DESC`, session)
	if err != nil {
		return nil, fmt.Errorf("store: read turns: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("store: read turns: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read turns: %w", err)
	}
	return out, nil
}

// Call is one model call of a turn, from its model.requested, and what its
// model.responded says, if one is stored.
type Call struct {
	Exchange  string
	Responded bool   // a model.responded of the exchange is stored
	Status    int    // its status
	Stop      string // its stop reason
	// Error is its error: a vendor error, or a reply that did not arrive
	// whole (the stream ended before message_stop, or reported an error).
	// Such a reply's stop reason cannot be trusted, and its reply is never
	// kept.
	Error string
}

// TurnCalls lists the turn's model calls, newest request first, leaving out
// the harness's own checks. They are read from model.requested, which the
// gateway emits before the request goes on, so a call whose model.responded
// was never stored (lost, or not written) is still listed. The order is the
// order of the requests, not the order their replies landed in: a reply's
// events are written off the reply's path, so an earlier call's reply can be
// stored after a later one's.
func (s *Store) TurnCalls(session, turn string) ([]Call, error) {
	rows, err := s.r.Query(`SELECT json_extract(q.data, '$.exchange'),
			COUNT(r.seq), COALESCE(MAX(json_extract(r.data, '$.status')), 0),
			COALESCE(MAX(json_extract(r.data, '$.stop_reason')), ''),
			COALESCE(MAX(json_extract(r.data, '$.error')), '')
		FROM events q
		LEFT JOIN events r ON r.session = q.session AND r.turn = q.turn AND r.kind = 'model.responded'
			AND json_extract(r.data, '$.exchange') = json_extract(q.data, '$.exchange')
		WHERE q.session = ? AND q.turn = ? AND q.kind = 'model.requested'
			AND COALESCE(json_extract(q.data, '$.probe'), 0) <> 1
			AND COALESCE(json_extract(q.data, '$.exchange'), '') <> ''
		GROUP BY q.seq
		ORDER BY q.seq DESC`, session, turn)
	if err != nil {
		return nil, fmt.Errorf("store: read a turn's calls: %w", err)
	}
	defer rows.Close()
	var out []Call
	for rows.Next() {
		var c Call
		var n int
		if err := rows.Scan(&c.Exchange, &n, &c.Status, &c.Stop, &c.Error); err != nil {
			return nil, fmt.Errorf("store: read a turn's calls: %w", err)
		}
		c.Responded = n > 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read a turn's calls: %w", err)
	}
	return out, nil
}

// TurnIncomplete reports whether the turn may be missing events: an
// events.dropped naming its session (or every session) stored after its
// turn.started, or its session.closed saying unsettled (the gateway could not
// confirm all of its calls were stored).
func (s *Store) TurnIncomplete(session, turn string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM events d WHERE d.kind = 'events.dropped'
			AND d.seq > COALESCE((SELECT MIN(seq) FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'turn.started'), 0)
			AND (COALESCE(json_extract(d.data, '$.all_sessions'), 0) = 1
				OR EXISTS(SELECT 1 FROM json_each(COALESCE(json_extract(d.data, '$.sessions'), '[]')) WHERE value = ?1)))
		OR EXISTS(SELECT 1 FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'session.closed'
			AND COALESCE(json_extract(data, '$.unsettled'), 0) = 1)`, session, turn).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: read a turn: %w", err)
	}
	return n > 0, nil
}

// UsedReplay reports whether any turn of the session continued by own
// replay: a turn.started with replay_from.
func (s *Store) UsedReplay(session string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM events WHERE session = ? AND kind = 'turn.started'
		AND COALESCE(json_extract(data, '$.replay_from'), '') <> '' LIMIT 1)`, session).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: read a session: %w", err)
	}
	return n > 0, nil
}

// TurnClosed reports whether the turn's run has a session.closed: its child
// exited.
func (s *Store) TurnClosed(session, turn string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT EXISTS(SELECT 1 FROM events WHERE session = ? AND turn = ? AND kind = 'session.closed')`,
		session, turn).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: read a turn: %w", err)
	}
	return n > 0, nil
}

// TurnEnd is how a turn's run ended, from its session.closed, and whether any
// of its model requests was refused by own replay (replay.failed).
type TurnEnd struct {
	Closed       bool   // the turn has a session.closed
	ExitCode     *int   // its exit code; nil when the service closed it at start, or it never started
	Error        string // its error, such as "the agent could not be started"
	ReplayFailed bool   // a replay.failed of the turn is stored
	Stopped      bool   // the person stopped the turn (session.closed's stopped)
}

// TurnEnd reads how the turn's run ended: its newest session.closed.
func (s *Store) TurnEnd(session, turn string) (TurnEnd, error) {
	var e TurnEnd
	var closed, failed, stopped int
	var code sql.NullInt64
	var msg sql.NullString
	err := s.r.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'session.closed'),
		(SELECT json_extract(data, '$.exit_code') FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'session.closed' ORDER BY seq DESC LIMIT 1),
		(SELECT json_extract(data, '$.error') FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'session.closed' ORDER BY seq DESC LIMIT 1),
		EXISTS(SELECT 1 FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'replay.failed'),
		COALESCE((SELECT json_extract(data, '$.stopped') FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'session.closed' ORDER BY seq DESC LIMIT 1), 0)`,
		session, turn).Scan(&closed, &code, &msg, &failed, &stopped)
	if err != nil {
		return e, fmt.Errorf("store: read a turn: %w", err)
	}
	e.Closed, e.ReplayFailed, e.Error, e.Stopped = closed > 0, failed > 0, msg.String, stopped > 0
	if code.Valid {
		c := int(code.Int64)
		e.ExitCode = &c
	}
	return e, nil
}
