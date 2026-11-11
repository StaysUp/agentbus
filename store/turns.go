package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
)

// A turn's state.
const (
	TurnRunning  = "running"
	TurnFinished = "finished"
	TurnFailed   = "failed"
	TurnStopped  = "stopped"
)

// TurnList is a page of turns as the stored events up to Seq describe them.
type TurnList struct {
	Store string `json:"store" doc:"The store id. Send it back as store."`
	Seq   int64  `json:"seq" doc:"The seq the list was computed at. Send it back as after to stream from here."`
	Turns []Turn `json:"turns" doc:"Newest first, by when each turn started."`
	// NextBefore is the before for the next, older page.
	NextBefore *int64 `json:"next_before" doc:"The before for the next, older page, or null when there are no older turns. A page stops at limit turns or at 4 MiB of rows, whichever comes first."`
}

// Turn is one turn: what its turn.started says, and what the events carrying
// its id add up to.
type Turn struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Number  int    `json:"number,omitempty" doc:"The turn's place in its session, counted from 1 in the order the turns started: turn 3 is the session's third. Absent until the turn starts."`
	Harness string `json:"agent" doc:"From the turn's turn.started. Empty for a turn no run started."`
	AgentID string `json:"agent_id,omitempty" doc:"From the turn's turn.started: the saved agent that ran it, as GET /api/v0/agents names it. Turns of one session may name different agents. Absent for a turn started by harness name alone, or no run started."`
	Folder  string `json:"folder" doc:"From the turn's turn.started. Empty for a turn no run started."`
	// Prompt is a preview: the list holds many turns, and one prompt can be
	// 256 KiB (100 turns answered 26 MB).
	Prompt          string `json:"prompt" doc:"The person's words from the turn's turn.started: the first 500 characters. The whole prompt is that turn.started's text, in GET /api/v0/sessions/{id}/events?turn={id}."`
	PromptTruncated bool   `json:"prompt_truncated,omitempty" doc:"True when prompt was cut to its first 500 characters."`
	// Reply, Model and ModelCalls let a list show a turn without its events.
	// The last call is the turn's model.requested with the highest seq; its
	// reply is the model.responded of the same exchange.
	Reply          string `json:"reply" doc:"The text of the reply to the turn's last model call: the first 500 characters. Empty while that reply has not come, when it carried no text, or when the turn has no calls. The whole text is that model.responded's text, in GET /api/v0/sessions/{id}/events?turn={id}."`
	ReplyTruncated bool   `json:"reply_truncated,omitempty" doc:"True when reply was cut to its first 500 characters."`
	Model          string `json:"model" doc:"The model the turn's last model call asked for: its model.requested's model. Empty when the turn has no calls."`
	ModelCalls     int    `json:"model_calls" doc:"How many model calls the turn made: its model.requested events."`
	// StartedSeq orders turns and pages them.
	StartedSeq int64      `json:"started_seq" doc:"The seq of the turn's turn.started. Send it as before for older turns."`
	Started    time.Time  `json:"started,omitzero" doc:"When the turn started: the time of its turn.started. Absent until it starts; asked is when it was asked for."`
	Ended      *time.Time `json:"ended,omitempty" doc:"A run's turn: when its child exited. Another turn: its latest turn.completed. Absent while running."`
	State      string     `json:"state" doc:"queued, running, finished, failed or stopped. queued: asked for, and no runner has started it yet. A run's turn: running until its child exits, then finished on exit code 0, stopped when someone stopped it, else failed. Another turn: finished once it has a turn.completed, else running."`
	Outcome    string     `json:"outcome,omitempty" doc:"The outcome of the turn's latest turn.completed, when it has one."`
	ExitCode   *int       `json:"exit_code,omitempty" doc:"A run's turn: its child's exit code, once it exited. Absent for a child the service closed on restart."`
	Stopped    bool       `json:"stopped,omitempty" doc:"A run's turn: someone stopped it with POST /api/v0/turns/{id}/stop. Its state is then stopped."`
	Unsettled  bool       `json:"unsettled,omitempty" doc:"A run's turn: its session.closed says the gateway could not confirm that every model call was stored, as for a turn the service closed on restart. Own replay does not continue from it."`
	Error      string     `json:"error,omitempty" doc:"A run's turn: why its child could not start, from its session.closed. Its state is then failed. A failed turn whose child gave no reason takes the error of its last model call's reply, when the vendor or control's relay refused that call with a status other than 200."`
	// Origin, from the turn's turn.started: who started it when the person
	// did not.
	Origin *event.Origin `json:"origin,omitempty" doc:"From the turn's turn.started: who started this turn when the person did not. kind result marks a delivered result: AgentBus started the turn, and its prompt is the result of work this session asked for. kind asked marks a turn another agent's turn asked for. Absent for a turn the person started."`
	// IdempotencyKey is the key the turn was started with, so a client that
	// lost the reply to POST /api/v0/turns can still find its turn.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"The idempotency_key POST /api/v0/turns was sent with, from the turn's turn.started."`
	// Usage, CostUSD and CostComplete follow Session's rules, over the
	// turn's own model.responded events.
	Usage        *event.Usage `json:"usage" doc:"Sums the turn's replies that carried usage. Null when none did: unknown, never zero."`
	CostUSD      float64      `json:"cost_usd" doc:"Sums cost_usd over the turn's model.responded events."`
	CostComplete bool         `json:"cost_complete" doc:"False when a reply lacks a complete cost, or a drop names the turn's session."`
}

// PromptPreviewRunes bounds a listed turn's prompt and reply, in characters
// (runes).
const PromptPreviewRunes = 500

// TurnPageBytes bounds one page of turns, as encoded JSON rows. A page always
// holds at least one turn, and next_before is set when the budget stops it.
// Each row's size, prompt and reply previews included, is counted with a fixed
// allowance for the fields the aggregate adds (usage, cost, ended, outcome).
const TurnPageBytes = 4 << 20

// previewBytes is the most of a prompt or reply read for its preview: 500
// runes of at most 4 bytes each.
const previewBytes = PromptPreviewRunes * utf8.UTFMax

// preview is the first PromptPreviewRunes runes of a text (a prompt or a
// reply), from head, its first bytes, and total, its whole length in bytes. A
// rune cut by the byte cap is left out. truncated is true when the preview is
// shorter than the text.
func preview(head []byte, total int64) (string, bool) {
	n, runes := 0, 0
	for n < len(head) && runes < PromptPreviewRunes {
		r, size := utf8.DecodeRune(head[n:])
		if r == utf8.RuneError && size <= 1 && !utf8.FullRune(head[n:]) {
			break // a rune the byte cap cut in two
		}
		n += size
		runes++
	}
	return string(head[:n]), int64(n) < total
}

// rowAllowance covers the fields filled after a row is counted.
const rowAllowance = 512

// TurnQuery picks a page of turns.
type TurnQuery struct {
	Session string // only this session's turns; empty for all
	Turn    string // only turns with this id; empty for all
	Before  int64  // only turns whose turn.started seq is below it; 0 for none
	Limit   int
}

// FirstStartSince is the earliest start time of the turns whose turn.started
// seq is from or later, of session when it is set; ok is false when there
// are none. Pages of Turns go down by seq, so this only moves back in time
// from one page to the next, whatever order the turns' clocks are in.
func (s *Store) FirstStartSince(session string, from int64) (t time.Time, ok bool, err error) {
	q := `SELECT MIN(time) FROM events WHERE kind = 'turn.started' AND turn <> '' AND seq >= ?`
	args := []any{from}
	if session != "" {
		q += ` AND session = ?`
		args = append(args, session)
	}
	var tm sql.NullInt64
	if err := s.r.QueryRow(q, args...).Scan(&tm); err != nil {
		return time.Time{}, false, fmt.Errorf("store: read turns: %w", err)
	}
	if !tm.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(0, tm.Int64).UTC(), true, nil
}

// Turns lists the turns that have a turn.started, newest first. A turn is
// named by the turn id on its events; every turn.started with one is a turn.
// Events whose turn id has no turn.started, such as a codex conversation the
// hash renamed after a compaction, are not listed.
func (s *Store) Turns(q TurnQuery) (TurnList, error) {
	hi := s.Last()
	list := TurnList{Store: s.id, Seq: hi, Turns: []Turn{}}

	args := []any{hi, hi, hi}
	where := `e.kind = 'turn.started' AND e.turn <> '' AND e.seq <= ?`
	if q.Session != "" {
		where += ` AND e.session = ?`
		args = append(args, q.Session)
	}
	if q.Turn != "" {
		where += ` AND e.turn = ?`
		args = append(args, q.Turn)
	}
	if q.Before > 0 {
		where += ` AND e.seq < ?`
		args = append(args, q.Before)
	}
	args = append(args, q.Limit+1, hi, previewBytes, previewBytes)
	// At most previewBytes of the prompt and of the reply are read, so a whole
	// 256 KiB prompt never enters the list, then cut to runes in Go (preview).
	// As a BLOB: SQLite's text substr and length stop at a NUL, which a prompt
	// can hold.
	//
	// The turn's last model call is its model.requested with the highest seq.
	// Its reply is the model.responded with the same exchange, the first if
	// there are several; a request with no exchange takes the first reply after
	// it that has none either. That is how the cockpit pairs a turn's calls.
	// t and u carry seqs and short fields only, never a whole event's data.
	rows, err := s.r.Query(`WITH t AS (
			SELECT e.seq, e.session, e.turn,
				(SELECT COUNT(*) FROM events c WHERE c.session = e.session AND c.turn = e.turn
					AND c.kind = 'model.requested' AND c.seq <= ?) AS calls,
				(SELECT MAX(c.seq) FROM events c WHERE c.session = e.session AND c.turn = e.turn
					AND c.kind = 'model.requested' AND c.seq <= ?) AS last_call
			FROM events e WHERE `+where+` ORDER BY e.seq DESC LIMIT ?
		), u AS (
			SELECT t.seq, t.calls, t.last_call,
				(SELECT p.seq FROM events p
					WHERE p.session = t.session AND p.turn = t.turn AND p.kind = 'model.responded' AND p.seq <= ?
					AND CASE WHEN COALESCE(json_extract(q.data, '$.exchange'), '') <> ''
						THEN json_extract(p.data, '$.exchange') = json_extract(q.data, '$.exchange')
						ELSE COALESCE(json_extract(p.data, '$.exchange'), '') = '' AND p.seq > q.seq END
					ORDER BY p.seq LIMIT 1) AS reply
			FROM t LEFT JOIN events q ON q.seq = t.last_call
		)
		SELECT s.seq, s.session, s.turn, s.time,
			COALESCE(json_extract(s.data, '$.agent'), ''), COALESCE(json_extract(s.data, '$.agent_id'), ''),
			COALESCE(json_extract(s.data, '$.folder'), ''), COALESCE(json_extract(s.data, '$.idempotency_key'), ''),
			COALESCE(json_extract(s.data, '$.origin'), ''),
			substr(CAST(COALESCE(json_extract(s.data, '$.text'), '') AS BLOB), 1, ?),
			length(CAST(COALESCE(json_extract(s.data, '$.text'), '') AS BLOB)),
			u.calls, COALESCE(json_extract(q.data, '$.model'), ''),
			substr(CAST(COALESCE(json_extract(r.data, '$.text'), '') AS BLOB), 1, ?),
			length(CAST(COALESCE(json_extract(r.data, '$.text'), '') AS BLOB)),
			COALESCE(json_extract(r.data, '$.status'), 0), COALESCE(json_extract(r.data, '$.error'), ''),
			(SELECT COUNT(*) FROM events n WHERE n.session = s.session AND n.kind = 'turn.started' AND n.turn <> '' AND n.seq <= s.seq)
		FROM u JOIN events s ON s.seq = u.seq
			LEFT JOIN events q ON q.seq = u.last_call
			LEFT JOIN events r ON r.seq = u.reply
		ORDER BY s.seq DESC`, args...)
	if err != nil {
		return list, fmt.Errorf("store: read turns: %w", err)
	}
	type key struct{ session, turn string }
	at := map[key]int{}
	// refused: the error of a turn's last call when the vendor, or the relay
	// on its way, refused it with a status other than 200.
	refused := map[key]string{}
	size := 0
	stop := func() {
		last := list.Turns[len(list.Turns)-1].StartedSeq
		list.NextBefore = &last
	}
	for rows.Next() {
		if len(list.Turns) == q.Limit {
			stop()
			break
		}
		var t Turn
		var tm, promptLen, replyLen int64
		var head, replyHead []byte
		var origin string
		var replyStatus int64
		var replyErr string
		if err := rows.Scan(&t.StartedSeq, &t.Session, &t.ID, &tm, &t.Harness, &t.AgentID, &t.Folder, &t.IdempotencyKey, &origin,
			&head, &promptLen, &t.ModelCalls, &t.Model, &replyHead, &replyLen, &replyStatus, &replyErr, &t.Number); err != nil {
			rows.Close()
			return list, fmt.Errorf("store: read turns: %w", err)
		}
		if origin != "" {
			var o event.Origin
			if json.Unmarshal([]byte(origin), &o) == nil {
				t.Origin = &o
			}
		}
		t.Prompt, t.PromptTruncated = preview(head, promptLen)
		t.Reply, t.ReplyTruncated = preview(replyHead, replyLen)
		t.Started = time.Unix(0, tm).UTC()
		t.State = TurnRunning
		if replyStatus != 0 && replyStatus != 200 {
			refused[key{t.Session, t.ID}] = replyErr
		}
		b, err := json.Marshal(t)
		if err != nil {
			rows.Close()
			return list, fmt.Errorf("store: read turns: %w", err)
		}
		if n := len(b) + rowAllowance; len(list.Turns) > 0 && size+n > TurnPageBytes {
			stop()
			break
		} else {
			size += n
		}
		at[key{t.Session, t.ID}] = len(list.Turns)
		list.Turns = append(list.Turns, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return list, fmt.Errorf("store: read turns: %w", err)
	}
	rows.Close()
	if len(list.Turns) == 0 {
		return list, nil
	}

	all, named, err := s.drops(hi)
	if err != nil {
		return list, err
	}
	ids := make([]any, 0, len(list.Turns)+2)
	ids = append(ids, hi, hi)
	for _, t := range list.Turns {
		ids = append(ids, t.ID)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(list.Turns)), ",")
	rows, err = s.r.Query(`SELECT session, turn,
			SUM(kind = 'model.responded' AND COALESCE(json_type(data, '$.usage'), 'null') = 'object'),
			SUM(kind = 'model.responded' AND COALESCE(json_type(data, '$.usage'), 'null') <> 'object'),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.input_tokens') END), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.output_tokens') END), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.cache_read_tokens') END), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.cache_write_tokens') END), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.cache_write_5m_tokens') END), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.cache_write_1h_tokens') END), 0),
			COALESCE(MAX(kind = 'model.responded' AND json_extract(data, '$.usage.output_incomplete') = 1), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.cost_usd') END), 0),
			SUM(kind = 'model.responded' AND COALESCE(json_extract(data, '$.cost_complete'), 0) <> 1),
			SUM(kind = 'session.opened'),
			SUM(kind = 'session.closed'),
			MAX(CASE WHEN kind = 'session.closed' THEN time END),
			MAX(CASE WHEN kind = 'session.closed' THEN json_extract(data, '$.exit_code') END),
			MAX(CASE WHEN kind = 'session.closed' THEN COALESCE(json_extract(data, '$.error'), '') END),
			COALESCE(MAX(CASE WHEN kind = 'session.closed' THEN json_extract(data, '$.stopped') END), 0),
			COALESCE(MAX(CASE WHEN kind = 'session.closed' THEN json_extract(data, '$.unsettled') END), 0),
			MAX(CASE WHEN kind = 'turn.completed' THEN time END),
			(SELECT json_extract(e2.data, '$.outcome') FROM events e2
				WHERE e2.turn = events.turn AND e2.session = events.session AND e2.kind = 'turn.completed' AND e2.seq <= ?
				ORDER BY e2.seq DESC LIMIT 1)
		FROM events
		WHERE seq <= ? AND turn IN (`+marks+`)
		GROUP BY session, turn`, ids...)
	if err != nil {
		return list, fmt.Errorf("store: read turns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var session, turn string
		var withUsage, usageMissing, costMissing, opened, closed int
		var outputIncomplete, stopped, unsettled bool
		var u event.Usage
		var cost float64
		var closedAt, exitCode, completedAt sql.NullInt64
		var closeErr, outcome sql.NullString
		if err := rows.Scan(&session, &turn, &withUsage, &usageMissing,
			&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens,
			&u.CacheWrite5mTokens, &u.CacheWrite1hTokens, &outputIncomplete, &cost, &costMissing,
			&opened, &closed, &closedAt, &exitCode, &closeErr, &stopped, &unsettled, &completedAt, &outcome); err != nil {
			return list, fmt.Errorf("store: read turns: %w", err)
		}
		i, ok := at[key{session, turn}]
		if !ok {
			continue // the same turn id in another session: not this page's turn
		}
		t := &list.Turns[i]
		if withUsage > 0 {
			u.OutputIncomplete = outputIncomplete
			t.Usage = &u
		}
		t.CostUSD = cost
		t.CostComplete = costMissing == 0 && usageMissing == 0 && !outputIncomplete && !all && !named[session]
		t.Outcome = outcome.String
		switch {
		case opened > 0 && closed > 0:
			ended := time.Unix(0, closedAt.Int64).UTC()
			t.Ended = &ended
			if exitCode.Valid {
				c := int(exitCode.Int64)
				t.ExitCode = &c
			}
			t.Stopped = stopped
			t.Unsettled = unsettled
			t.Error = closeErr.String
			switch {
			case stopped:
				t.State = TurnStopped
			case exitCode.Valid && exitCode.Int64 == 0 && closeErr.String == "":
				t.State = TurnFinished
			default:
				t.State = TurnFailed
				// The child gave no reason: the refusal of its last call is
				// it, as a turn whose call the relay refused (no codex
				// sign-in connected) otherwise showed nothing.
				if t.Error == "" {
					t.Error = refused[key{session, turn}]
				}
			}
		case opened == 0 && completedAt.Valid:
			ended := time.Unix(0, completedAt.Int64).UTC()
			t.Ended = &ended
			t.State = TurnFinished
		}
	}
	return list, rows.Err()
}
