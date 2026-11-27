package store

// What a replayed turn reads: its own turn.started, the session's turns, and
// the calls of an earlier turn that can end its history.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
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

// TurnText reads the whole prompt and whole reply of (session, turn) from the
// record: the prompt is the person's own words (the turn.started's text,
// never an earlier turn's transcript), and the reply is the text of the reply
// to the turn's last model call, paired with that call as in Turns. found is
// false when the turn has no turn.started in the record (a queued turn, or
// none). reply is empty while that reply has not come, when it carried no
// text, or when the turn has no calls. Neither is cut: the single turn
// (GET /api/v0/turns/{id}) serves the whole text, and the queue reads a
// review's or asked turn's reply from here in place of a stored copy.
func (s *Store) TurnText(session, turn string) (prompt, reply string, found bool, err error) {
	var p, r sql.NullString
	err = s.r.QueryRow(`WITH t AS (
			SELECT e.seq, COALESCE(json_extract(e.data, '$.text'), '') AS prompt,
				(SELECT MAX(c.seq) FROM events c WHERE c.session = e.session AND c.turn = e.turn
					AND c.kind = 'model.requested') AS last_call
			FROM events e WHERE e.session = ?1 AND e.turn = ?2 AND e.kind = 'turn.started' ORDER BY e.seq LIMIT 1
		), u AS (
			SELECT t.seq, t.prompt,
				(SELECT p.seq FROM events p
					WHERE p.session = ?1 AND p.turn = ?2 AND p.kind = 'model.responded'
					AND CASE WHEN COALESCE(json_extract(q.data, '$.exchange'), '') <> ''
						THEN json_extract(p.data, '$.exchange') = json_extract(q.data, '$.exchange')
						ELSE COALESCE(json_extract(p.data, '$.exchange'), '') = '' AND p.seq > q.seq END
					ORDER BY p.seq LIMIT 1) AS reply
			FROM t LEFT JOIN events q ON q.seq = t.last_call
		)
		SELECT u.prompt, json_extract(r.data, '$.text')
		FROM u LEFT JOIN events r ON r.seq = u.reply`, session, turn).Scan(&p, &r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read a turn's text: %w", err)
	}
	return p.String, r.String, true, nil
}

// Answer is one answer of a turn: the text of a reply that ended it, and when.
type Answer struct {
	Text string    `json:"text" doc:"The reply's whole text."`
	Time time.Time `json:"time" doc:"When the turn.completed that ended on this reply was recorded."`
}

// TurnAnswers reads every answer of (session, turn), oldest first: for each
// turn.completed, the text of the model.responded of its exchange. A turn can
// answer more than once: Claude Code resumes after its own background command
// ends, and the record then holds a turn.completed per answer (seen
// 2026-11-26, t_5794feb266e80f67: two, four minutes apart). A turn.completed
// whose reply carried no text, or with no exchange to pair it by, gives no
// answer.
func (s *Store) TurnAnswers(session, turn string) ([]Answer, error) {
	rows, err := s.r.Query(`SELECT c.time, json_extract(r.data, '$.text')
		FROM events c JOIN events r ON r.seq = (
			SELECT p.seq FROM events p WHERE p.session = c.session AND p.turn = c.turn AND p.kind = 'model.responded'
				AND json_extract(p.data, '$.exchange') = json_extract(c.data, '$.exchange') AND p.seq < c.seq
			ORDER BY p.seq DESC LIMIT 1)
		WHERE c.session = ? AND c.turn = ? AND c.kind = 'turn.completed'
			AND COALESCE(json_extract(c.data, '$.exchange'), '') <> ''
			AND COALESCE(json_extract(r.data, '$.text'), '') <> ''
		ORDER BY c.seq`, session, turn)
	if err != nil {
		return nil, fmt.Errorf("store: read a turn's answers: %w", err)
	}
	defer rows.Close()
	out := []Answer{}
	for rows.Next() {
		var ns int64
		var a Answer
		if err := rows.Scan(&ns, &a.Text); err != nil {
			return nil, fmt.Errorf("store: read a turn's answers: %w", err)
		}
		a.Time = time.Unix(0, ns).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
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
	// Compacted: the gateway judged this call a main-thread request its
	// harness had compacted (a history.compacted with its exchange).
	Compacted bool
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
			COALESCE(MAX(json_extract(r.data, '$.error')), ''),
			EXISTS(SELECT 1 FROM events c WHERE c.session = q.session AND c.turn = q.turn
				AND c.kind = 'history.compacted' AND json_extract(c.data, '$.exchange') = json_extract(q.data, '$.exchange'))
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
		if err := rows.Scan(&c.Exchange, &n, &c.Status, &c.Stop, &c.Error, &c.Compacted); err != nil {
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
// turn.started, or its run.ended saying unsettled (the gateway could not
// confirm all of its calls were stored).
func (s *Store) TurnIncomplete(session, turn string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM events d WHERE d.kind = 'events.dropped'
			AND d.seq > COALESCE((SELECT MIN(seq) FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'turn.started'), 0)
			AND (COALESCE(json_extract(d.data, '$.all_sessions'), 0) = 1
				OR EXISTS(SELECT 1 FROM json_each(COALESCE(json_extract(d.data, '$.sessions'), '[]')) WHERE value = ?1)))
		OR EXISTS(SELECT 1 FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'run.ended'
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

// TurnClosed reports whether the turn's run has a run.ended: its child
// exited.
func (s *Store) TurnClosed(session, turn string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT EXISTS(SELECT 1 FROM events WHERE session = ? AND turn = ? AND kind = 'run.ended')`,
		session, turn).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: read a turn: %w", err)
	}
	return n > 0, nil
}

// TurnEnd is how a turn's run ended, from its run.ended, and whether any
// of its model requests was refused by own replay (replay.failed).
type TurnEnd struct {
	Closed       bool   // the turn has a run.ended
	ExitCode     *int   // its exit code; nil when the service closed it at start, or it never started
	Error        string // why it could not start, such as "the agent could not be started"; never an exited agent's own error line (0.5.7), whose stderr stays out of any history
	ReplayFailed bool   // a replay.failed of the turn is stored
	Stopped      bool   // the person stopped the turn (run.ended's stopped)
}

// TurnEnd reads how the turn's run ended: its newest run.ended.
func (s *Store) TurnEnd(session, turn string) (TurnEnd, error) {
	var e TurnEnd
	var closed, failed, stopped int
	var code sql.NullInt64
	var msg sql.NullString
	err := s.r.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'run.ended'),
		(SELECT json_extract(data, '$.exit_code') FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'run.ended' ORDER BY seq DESC LIMIT 1),
		(SELECT CASE WHEN json_extract(data, '$.exit_code') IS NULL THEN json_extract(data, '$.error') END
			FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'run.ended' ORDER BY seq DESC LIMIT 1),
		EXISTS(SELECT 1 FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'replay.failed'),
		COALESCE((SELECT json_extract(data, '$.stopped') FROM events WHERE session = ?1 AND turn = ?2 AND kind = 'run.ended' ORDER BY seq DESC LIMIT 1), 0)`,
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
