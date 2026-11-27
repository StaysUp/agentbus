package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	// Model and ModelCalls are facts a list carries about a turn's calls. The
	// last call is the turn's model.requested with the highest seq; its reply
	// is the model.responded of the same exchange. The whole prompt and reply
	// are not in the list: GET /api/v0/turns/{id} serves them, read from the
	// record at request time (api.Turn's prompt and reply).
	Model      string `json:"model" doc:"The model the turn's last model call asked for: its model.requested's model. Empty when the turn has no calls."`
	ModelCalls int    `json:"model_calls" doc:"How many model calls the turn made: its model.requested events."`
	// StartedSeq orders turns and pages them.
	StartedSeq int64      `json:"started_seq" doc:"The seq of the turn's turn.started. Send it as before for older turns."`
	Started    time.Time  `json:"started,omitzero" doc:"When the turn started: the time of its turn.started. Absent until it starts; asked is when it was asked for."`
	Ended      *time.Time `json:"ended,omitempty" doc:"A run's turn: when its child exited. Another turn: its latest turn.completed. Absent while running."`
	State      string     `json:"state" doc:"queued, running, finished, failed or stopped. queued: asked for, and no runner has started it yet. A run's turn: running until its child exits, then finished on exit code 0, stopped when someone stopped it, else failed. Another turn: finished once it has a turn.completed, else running. A turn is never retried once it runs: a runner lost before the start puts it back in the queue with the same id, and one lost while it runs fails it. A turn the person sends starts once no turn runs in its workspace and its session's earlier turns have ended; it goes ahead of delegated turns that have not started. A delegated turn (one an agent started, or a reply delivered into a session an agent started) waits for a place: one runner runs at most [limits] max_delegated_at_once of them at once, 8 by default."`
	Outcome    string     `json:"outcome,omitempty" doc:"The outcome of the turn's latest turn.completed, when it has one."`
	ExitCode   *int       `json:"exit_code,omitempty" doc:"A run's turn: its child's exit code, once it exited. Absent for a child the service closed on restart."`
	Stopped    bool       `json:"stopped,omitempty" doc:"A run's turn: someone stopped it with POST /api/v0/turns/{id}/stop. Its state is then stopped."`
	Unsettled  bool       `json:"unsettled,omitempty" doc:"A run's turn: its run.ended says the gateway could not confirm that every model call was stored, as for a turn the service closed on restart. Own replay does not continue from it."`
	// Discarded marks a turn whose file changes a discard dropped (POST
	// /api/v0/workspaces/{id}/discard), as Applied marks a turn an apply put in
	// the folder: the conversation stays, but the turn's edits are gone from the
	// copy. A turn id is in a workspace.discarded's dropped_turns for its own
	// workspace.
	Discarded bool   `json:"discarded,omitempty" doc:"The turn's file changes were dropped by a discard of its workspace. Its conversation stays in the record; its edits are gone from the copy, as after a discard."`
	Error     string `json:"error,omitempty" doc:"A failed run's turn: why it failed, why its child could not start, from its run.ended; else the error of its last model call's reply, when the vendor or control's relay refused that call with a status other than 200; else its run.ended's error: the agent's own error line from its stderr (the first line that starts with error or fatal, else the first that is not a list item, at most 300 bytes), or its exit code. A failure its harness knows reads in plain words that say what to do, and the agent's line is then in error_tail. Its state is then failed. A turn from before 0.5.7 whose run.ended has no error takes the error line of error_tail."`
	ErrorTail string `json:"error_tail,omitempty" doc:"A run's turn whose agent exited with an error: the last lines it wrote to stderr, from its run.ended (at most 20 lines and 2 KiB, anything shaped like a secret hidden)."`
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

// TurnPageBytes bounds one page of turns, as encoded JSON rows. A page always
// holds at least one turn, and next_before is set when the budget stops it.
// The list carries no prompt or reply text, so a row is its facts and the
// aggregate's fields (usage, cost, ended, outcome), covered by a fixed
// allowance.
const TurnPageBytes = 4 << 20

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
	args = append(args, q.Limit+1, hi)
	// The list carries no prompt or reply text; the single turn serves them
	// (api.Turn, store.TurnText). The reply is still paired here for its model
	// and for the refusal status and error the list shows.
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
			u.calls, COALESCE(json_extract(q.data, '$.model'), ''),
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
		var tm int64
		var origin string
		var replyStatus int64
		var replyErr string
		if err := rows.Scan(&t.StartedSeq, &t.Session, &t.ID, &tm, &t.Harness, &t.AgentID, &t.Folder, &t.IdempotencyKey, &origin,
			&t.ModelCalls, &t.Model, &replyStatus, &replyErr, &t.Number); err != nil {
			rows.Close()
			return list, fmt.Errorf("store: read turns: %w", err)
		}
		if origin != "" {
			var o event.Origin
			if json.Unmarshal([]byte(origin), &o) == nil {
				t.Origin = &o
			}
		}
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
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.cache_write_30m_tokens') END), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.usage.cache_write_12h_tokens') END), 0),
			COALESCE(MAX(kind = 'model.responded' AND json_extract(data, '$.usage.output_incomplete') = 1), 0),
			COALESCE(SUM(CASE WHEN kind = 'model.responded' THEN json_extract(data, '$.cost_usd') END), 0),
			SUM(kind = 'model.responded' AND COALESCE(json_extract(data, '$.cost_complete'), 0) <> 1),
			SUM(kind = 'run.started'),
			SUM(kind = 'run.ended'),
			MAX(CASE WHEN kind = 'run.ended' THEN time END),
			MAX(CASE WHEN kind = 'run.ended' THEN json_extract(data, '$.exit_code') END),
			MAX(CASE WHEN kind = 'run.ended' THEN COALESCE(json_extract(data, '$.error'), '') END),
			MAX(CASE WHEN kind = 'run.ended' THEN COALESCE(json_extract(data, '$.error_tail'), '') END),
			COALESCE(MAX(CASE WHEN kind = 'run.ended' THEN json_extract(data, '$.stopped') END), 0),
			COALESCE(MAX(CASE WHEN kind = 'run.ended' THEN json_extract(data, '$.unsettled') END), 0),
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
		var closeErr, errTail, outcome sql.NullString
		if err := rows.Scan(&session, &turn, &withUsage, &usageMissing,
			&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens,
			&u.CacheWrite5mTokens, &u.CacheWrite1hTokens, &u.CacheWrite30mTokens, &u.CacheWrite12hTokens,
			&outputIncomplete, &cost, &costMissing,
			&opened, &closed, &closedAt, &exitCode, &closeErr, &errTail, &stopped, &unsettled, &completedAt, &outcome); err != nil {
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
			t.ErrorTail = errTail.String
			switch {
			case stopped:
				t.State = TurnStopped
			case exitCode.Valid && exitCode.Int64 == 0 && closeErr.String == "":
				t.State = TurnFinished
			default:
				t.State = TurnFailed
				// A child that exited and whose last call the vendor or the
				// relay refused: that refusal is the reason, in the vendor's
				// own words (a model the vendor does not serve, no codex
				// sign-in connected), over the agent's own error line. A
				// child that could not start (no exit code) keeps why.
				if r := refused[key{session, turn}]; r != "" && (t.Error == "" || exitCode.Valid) {
					t.Error = r
				}
				// A record from before 0.5.7 has no error on an exited
				// child's run.ended: the error line of its tail.
				if t.Error == "" {
					t.Error = event.ErrorLine(t.ErrorTail)
				}
			}
		case opened == 0 && completedAt.Valid:
			ended := time.Unix(0, completedAt.Int64).UTC()
			t.Ended = &ended
			t.State = TurnFinished
		}
	}
	if err := rows.Err(); err != nil {
		return list, fmt.Errorf("store: read turns: %w", err)
	}

	// Mark the turns a discard dropped. A turn is discarded when its id is in a
	// workspace.discarded's dropped_turns for the turn's OWN workspace (its
	// turn.started's workspace, or its session when it carries none): a turn id
	// may recur across sessions, so the discard is matched to the turn's
	// workspace, never by id alone.
	dids := make([]any, 0, len(list.Turns)+1)
	dids = append(dids, hi)
	for _, t := range list.Turns {
		dids = append(dids, t.ID)
	}
	drows, err := s.r.Query(`SELECT DISTINCT ts.session, ts.turn
		FROM events ts
		JOIN events d ON d.kind = 'workspace.discarded' AND d.seq <= ?
			AND json_extract(d.data, '$.workspace') = `+resolvedWorkspace+`
		JOIN json_each(COALESCE(json_extract(d.data, '$.discarded.dropped_turns'), '[]')) je ON je.value = ts.turn
		WHERE ts.kind = 'turn.started' AND ts.turn IN (`+marks+`)`, dids...)
	if err != nil {
		return list, fmt.Errorf("store: read turns: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		var session, turn string
		if err := drows.Scan(&session, &turn); err != nil {
			return list, fmt.Errorf("store: read turns: %w", err)
		}
		if i, ok := at[key{session, turn}]; ok {
			list.Turns[i].Discarded = true
		}
	}
	return list, drows.Err()
}
