package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
)

// Snapshot is every session as the stored events up to Seq describe it.
type Snapshot struct {
	Store    string    `json:"store" doc:"The store id. Send it back as store."`
	Seq      int64     `json:"seq" doc:"The seq the snapshot was taken at. Send it back as after."`
	Sessions []Session `json:"sessions" doc:"Newest activity first."`
}

// Session is one session in a snapshot.
type Session struct {
	ID      string    `json:"id"`
	Harness string    `json:"agent" doc:"From the session's latest turn.started or run that names one. Empty for a session no run started."`
	Client  string    `json:"client" doc:"The harness, from its latest event that named one."`
	Folder  string    `json:"folder" doc:"From the session's latest turn.started or run that names one. Empty for a session no run started."`
	Started time.Time `json:"started"`
	Last    time.Time `json:"last"`
	Open    bool      `json:"open" doc:"A run's child for this session is running: a session.opened without its session.closed."`
	Turns   int       `json:"turns"`
	// FirstAgent stays put when a later turn runs on another harness, so a
	// list grouped by it keeps the session in one place.
	FirstAgent   string `json:"first_agent,omitempty" doc:"The agent (harness) of the session's first turn.started or run that names one. A later turn may run on another agent; agent is then that one's, and first_agent stays. Absent for a session no run started."`
	FirstAgentID string `json:"first_agent_id,omitempty" doc:"The saved agent's id from the same event as first_agent: the saved agent the session started on. Absent when that turn named a harness alone, or no run started the session."`
	// Usage sums the model.responded events that carried usage. It is nil when
	// none did: absent usage is unknown, never zero.
	Usage *event.Usage `json:"usage" doc:"Sums the replies that carried usage. Null when none did: unknown, never zero."`
	// CostUSD sums the cost_usd of every model.responded. CostComplete is
	// false when any of them lacks a complete cost, or a drop names the
	// session; CostUSD is then a lower bound.
	CostUSD      float64 `json:"cost_usd" doc:"Sums cost_usd over the session's model.responded events."`
	CostComplete bool    `json:"cost_complete" doc:"False when a reply lacks a complete cost, or a drop names the session."`
	// Resumable: the session has a resume_id on a session.closed and is not
	// open, so POST /api/v0/turns with this session can run its next turn.
	Resumable bool `json:"resumable" doc:"POST /api/v0/turns accepts it as session."`
	// Title is the title of the session's latest session.renamed, else the
	// text of its first turn.started that has text, the person's own words,
	// cut to TitleRunes. Absent with neither.
	Title string `json:"title,omitempty" doc:"The name from the session's latest session.renamed; without one, the person's words from the first turn.started with text, cut to 120 characters."`

	// Why cost_complete is false. Not sent: the contract has no field for
	// them.
	UsageMissing     int  `json:"-"` // model.responded events with no usage
	OutputIncomplete bool `json:"-"` // a call's output count was a lower bound
	CostMissing      int  `json:"-"` // model.responded events without cost_complete true
	EventsDropped    bool `json:"-"` // an events.dropped names this session or all
}

// Snapshot computes every session from the stored events.
func (s *Store) Snapshot() (Snapshot, error) {
	hi := s.Last()
	snap := Snapshot{Store: s.id, Seq: hi, Sessions: []Session{}}
	byID := map[string]*Session{}

	// A rename is not activity: it leaves started and last as they were.
	rows, err := s.r.Query(`SELECT session,
			COALESCE(MIN(CASE WHEN kind <> 'session.renamed' THEN time END), MIN(time)),
			COALESCE(MAX(CASE WHEN kind <> 'session.renamed' THEN time END), MAX(time)),
			SUM(kind = 'turn.started'),
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
			(SELECT e2.client FROM events e2 WHERE e2.session = events.session AND e2.client <> '' AND e2.seq <= ?
				ORDER BY e2.seq DESC LIMIT 1)
		FROM events
		WHERE seq <= ? AND kind <> 'events.dropped'
		GROUP BY session`, hi, hi)
	if err != nil {
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	for rows.Next() {
		var x Session
		var first, last int64
		var withUsage int
		var u event.Usage
		var client sql.NullString
		if err := rows.Scan(&x.ID, &first, &last, &x.Turns, &withUsage, &x.UsageMissing,
			&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens,
			&u.CacheWrite5mTokens, &u.CacheWrite1hTokens, &x.OutputIncomplete,
			&x.CostUSD, &x.CostMissing, &client); err != nil {
			rows.Close()
			return snap, fmt.Errorf("store: snapshot: %w", err)
		}
		x.Started, x.Last = time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
		x.Client = client.String
		if withUsage > 0 {
			u.OutputIncomplete = x.OutputIncomplete
			x.Usage = &u
		}
		byID[x.ID] = &x
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	rows.Close()

	// The latest turn or run that names them gives the agent and the folder
	// (agentFrom). SQLite takes the bare columns from the row MAX(seq) picked.
	rows, err = s.r.Query(`SELECT session, json_extract(data, '$.agent'), json_extract(data, '$.folder'), MAX(seq)
		FROM events WHERE seq <= ? AND `+agentFrom+` GROUP BY session`, hi)
	if err != nil {
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	for rows.Next() {
		var id string
		var agent, folder sql.NullString
		var seq int64
		if err := rows.Scan(&id, &agent, &folder, &seq); err != nil {
			rows.Close()
			return snap, fmt.Errorf("store: snapshot: %w", err)
		}
		if x := byID[id]; x != nil {
			x.Harness, x.Folder = agent.String, folder.String
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	rows.Close()

	// The first that names an agent gives first_agent, and its saved agent
	// first_agent_id: SQLite takes the bare columns from the row MIN(seq)
	// picked, so both come from one event.
	rows, err = s.r.Query(`SELECT session, json_extract(data, '$.agent'), COALESCE(json_extract(data, '$.agent_id'), ''), MIN(seq)
		FROM events WHERE seq <= ? AND `+agentFrom+` AND COALESCE(json_extract(data, '$.agent'), '') <> '' GROUP BY session`, hi)
	if err != nil {
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	for rows.Next() {
		var id, agent, agentID string
		var seq int64
		if err := rows.Scan(&id, &agent, &agentID, &seq); err != nil {
			rows.Close()
			return snap, fmt.Errorf("store: snapshot: %w", err)
		}
		if x := byID[id]; x != nil {
			x.FirstAgent, x.FirstAgentID = agent, agentID
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	rows.Close()

	holds, err := s.OpenHolds(hi)
	if err != nil {
		return snap, err
	}
	held := map[string]bool{}
	for _, h := range holds {
		held[h.Session] = true
	}

	// The latest session.renamed names the session; without one, the first
	// turn.started with text does. A probe (Claude Code's startup quota
	// check, marked probe: true) never does. SQLite takes the bare column
	// from the row MAX(seq) or MIN(seq) picked. The first query runs first,
	// so a rename wins.
	for _, q := range []string{firstText + ` AND seq <= ? GROUP BY session`, latestRename + ` AND seq <= ? GROUP BY session`} {
		rows, err = s.r.Query(q, hi)
		if err != nil {
			return snap, fmt.Errorf("store: snapshot: %w", err)
		}
		for rows.Next() {
			var id, text string
			var seq int64
			if err := rows.Scan(&id, &text, &seq); err != nil {
				rows.Close()
				return snap, fmt.Errorf("store: snapshot: %w", err)
			}
			if x := byID[id]; x != nil {
				x.Title = title(text)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return snap, fmt.Errorf("store: snapshot: %w", err)
		}
		rows.Close()
	}

	// A session with a resume_id can be continued.
	rows, err = s.r.Query(`SELECT DISTINCT session FROM events
		WHERE seq <= ? AND kind = 'session.closed' AND json_extract(data, '$.resume_id') IS NOT NULL`, hi)
	if err != nil {
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	withResume := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return snap, fmt.Errorf("store: snapshot: %w", err)
		}
		withResume[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, fmt.Errorf("store: snapshot: %w", err)
	}
	rows.Close()

	all, named, err := s.drops(hi)
	if err != nil {
		return snap, err
	}

	for id, x := range byID {
		x.Open = held[id]
		x.Resumable = withResume[id] && !x.Open
		x.EventsDropped = all || named[id]
		// A call without a complete cost covers no usage, an incomplete
		// output count and a model without a price: the cost step marks
		// each of those incomplete.
		x.CostComplete = x.CostMissing == 0 && x.UsageMissing == 0 && !x.OutputIncomplete && !x.EventsDropped
		snap.Sessions = append(snap.Sessions, *x)
	}
	sort.Slice(snap.Sessions, func(i, j int) bool {
		a, b := snap.Sessions[i], snap.Sessions[j]
		if !a.Last.Equal(b.Last) {
			return a.Last.After(b.Last)
		}
		return a.ID < b.ID
	})
	return snap, nil
}

// TitleRunes bounds a session's title, "…" included.
const TitleRunes = 120

// The two sources of a session's title, each a query of session, text and seq
// to finish with more conditions and GROUP BY session.
const (
	firstText = `SELECT session, json_extract(data, '$.text'), MIN(seq) FROM events
		WHERE kind = 'turn.started'
			AND COALESCE(json_extract(data, '$.probe'), 0) <> 1
			AND COALESCE(json_extract(data, '$.text'), '') <> ''`
	latestRename = `SELECT session, COALESCE(json_extract(data, '$.title'), ''), MAX(seq) FROM events
		WHERE kind = 'session.renamed'`
)

// Rename stores e, a session.renamed, with OldTitle set to the session's title
// before it: the latest rename, else its first message cut to TitleRunes, as
// Snapshot computes it. The title is read and e written under one lock, so
// two renames each name the title the other left.
func (s *Store) Rename(e event.Event) (event.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return e, ErrClosed
	}
	old := ""
	for _, q := range []string{firstText, latestRename} {
		var id, text string
		var seq int64
		err := s.r.QueryRow(q+` AND session = ? GROUP BY session`, e.Session).Scan(&id, &text, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return e, fmt.Errorf("store: read title: %w", err)
		}
		old = title(text)
	}
	e.Kind = event.SessionRenamed
	e.OldTitle = old
	out, _, err := s.appendLocked(e)
	return out, err
}

// title is text cut to TitleRunes runes, ending in "…" when cut.
func title(text string) string {
	if utf8.RuneCountInString(text) <= TitleRunes {
		return text
	}
	r := []rune(text)
	return string(r[:TitleRunes-1]) + "…"
}

// drops reads the events.dropped up to seq hi: all is true when one said every
// session lost events, and named holds each session one listed.
func (s *Store) drops(hi int64) (all bool, named map[string]bool, err error) {
	rows, err := s.r.Query(`SELECT json_extract(data, '$.all_sessions'), json_extract(data, '$.sessions')
		FROM events WHERE seq <= ? AND kind = 'events.dropped'`, hi)
	if err != nil {
		return false, nil, fmt.Errorf("store: read drops: %w", err)
	}
	defer rows.Close()
	named = map[string]bool{}
	for rows.Next() {
		var allSessions sql.NullInt64
		var list sql.NullString
		if err := rows.Scan(&allSessions, &list); err != nil {
			return false, nil, fmt.Errorf("store: read drops: %w", err)
		}
		if allSessions.Int64 == 1 {
			all = true
		}
		for _, id := range decodeList(list.String) {
			named[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("store: read drops: %w", err)
	}
	return all, named, nil
}

func decodeList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// CostOf sums cost_usd over the model.responded events of the sessions
// named, each counted once.
func (s *Store) CostOf(sessions []string) (float64, error) {
	seen := map[string]bool{}
	var args []any
	for _, id := range sessions {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
	}
	if len(args) == 0 {
		return 0, nil
	}
	var cost float64
	err := s.r.QueryRow(`SELECT COALESCE(SUM(json_extract(data, '$.cost_usd')), 0) FROM events
		WHERE kind = 'model.responded' AND session IN (?`+strings.Repeat(",?", len(args)-1)+`)`, args...).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("store: sum cost: %w", err)
	}
	return cost, nil
}
