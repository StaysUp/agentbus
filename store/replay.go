package store

// What a replayed turn reads: its own turn.started, the session's turns, and
// the calls of an earlier turn that can end its history.

import (
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"
)

// TurnStart reads the turn.started of (session, turn): the prompt its agent
// got and the earlier turn it replays, if any. The prompt is the person's
// words, after the earlier turns' transcript when the turn got one (its
// context; transcript says its continuity is transcript). found is false
// when there is none.
func (s *Store) TurnStart(session, turn string) (prompt, replayFrom string, transcript, found bool, err error) {
	var p, r, c, k sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.text'), json_extract(data, '$.replay_from'), json_extract(data, '$.context'),
		json_extract(data, '$.continuity')
		FROM events WHERE session = ? AND turn = ? AND kind = 'turn.started' ORDER BY seq LIMIT 1`, session, turn).Scan(&p, &r, &c, &k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, false, nil
	}
	if err != nil {
		return "", "", false, false, fmt.Errorf("store: read a turn: %w", err)
	}
	return c.String + p.String, r.String, k.String == "transcript", true, nil
}

// TranscriptTurn is one turn as plain text: who ran it, the person's words
// and the text of the reply to its last model call.
type TranscriptTurn struct {
	Turn, Agent, AgentID string
	Model                string // the model its last model call asked for
	Prompt, Reply        string
	// PromptCut and ReplyCut say the text was cut to TranscriptTextMax.
	PromptCut, ReplyCut bool
}

// TranscriptTextMax bounds each prompt and reply Transcript reads, in bytes.
const TranscriptTextMax = 16 << 10

// TranscriptTurns bounds how many turns Transcript reads: the newest.
const TranscriptTurns = 50

// Transcript reads the session's newest turns, oldest first, as plain text.
// A turn's reply pairs with its last model call as in Turns. The prompt is
// the person's own words, never an earlier turn's transcript (context).
func (s *Store) Transcript(session string) ([]TranscriptTurn, error) {
	rows, err := s.r.Query(`WITH t AS (
			SELECT e.seq, e.session, e.turn,
				(SELECT MAX(c.seq) FROM events c WHERE c.session = e.session AND c.turn = e.turn
					AND c.kind = 'model.requested') AS last_call
			FROM events e WHERE e.session = ? AND e.kind = 'turn.started' AND e.turn <> ''
			ORDER BY e.seq DESC LIMIT ?
		), u AS (
			SELECT t.seq, t.last_call,
				(SELECT p.seq FROM events p
					WHERE p.session = t.session AND p.turn = t.turn AND p.kind = 'model.responded'
					AND CASE WHEN COALESCE(json_extract(q.data, '$.exchange'), '') <> ''
						THEN json_extract(p.data, '$.exchange') = json_extract(q.data, '$.exchange')
						ELSE COALESCE(json_extract(p.data, '$.exchange'), '') = '' AND p.seq > q.seq END
					ORDER BY p.seq LIMIT 1) AS reply
			FROM t LEFT JOIN events q ON q.seq = t.last_call
		)
		SELECT s.turn, COALESCE(json_extract(s.data, '$.agent'), ''), COALESCE(json_extract(s.data, '$.agent_id'), ''),
			COALESCE(json_extract(q.data, '$.model'), ''),
			substr(CAST(COALESCE(json_extract(s.data, '$.text'), '') AS BLOB), 1, ?),
			length(CAST(COALESCE(json_extract(s.data, '$.text'), '') AS BLOB)),
			substr(CAST(COALESCE(json_extract(r.data, '$.text'), '') AS BLOB), 1, ?),
			length(CAST(COALESCE(json_extract(r.data, '$.text'), '') AS BLOB))
		FROM u JOIN events s ON s.seq = u.seq
			LEFT JOIN events q ON q.seq = u.last_call
			LEFT JOIN events r ON r.seq = u.reply
		ORDER BY s.seq`, session, TranscriptTurns, TranscriptTextMax, TranscriptTextMax)
	if err != nil {
		return nil, fmt.Errorf("store: read a transcript: %w", err)
	}
	defer rows.Close()
	var out []TranscriptTurn
	for rows.Next() {
		var t TranscriptTurn
		var prompt, reply []byte
		var promptLen, replyLen int64
		if err := rows.Scan(&t.Turn, &t.Agent, &t.AgentID, &t.Model, &prompt, &promptLen, &reply, &replyLen); err != nil {
			return nil, fmt.Errorf("store: read a transcript: %w", err)
		}
		t.Prompt, t.PromptCut = cutText(prompt, promptLen)
		t.Reply, t.ReplyCut = cutText(reply, replyLen)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read a transcript: %w", err)
	}
	return out, nil
}

// cutText is head as text, without a rune the byte cap cut in two, and
// whether it is shorter than total.
func cutText(head []byte, total int64) (string, bool) {
	n := len(head)
	if int64(n) < total {
		for n > 0 && n > len(head)-utf8.UTFMax && !utf8.Valid(head[:n]) {
			n--
		}
	}
	return string(head[:n]), int64(n) < total
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
// replay: a turn.started with replay_from. Only turns after the session's
// latest turn that got the earlier turns as text count: that turn started
// fresh, so a resume_id from it or after holds the whole conversation.
func (s *Store) UsedReplay(session string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM events WHERE session = ?1 AND kind = 'turn.started'
		AND COALESCE(json_extract(data, '$.replay_from'), '') <> ''
		AND seq > COALESCE((SELECT MAX(seq) FROM events WHERE session = ?1 AND kind = 'turn.started'
			AND json_extract(data, '$.continuity') = 'transcript'), 0) LIMIT 1)`, session).Scan(&n)
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
