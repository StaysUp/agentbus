package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
)

// Snapshot is every session as the stored events up to Seq describe it, or a
// page of them when Sessions bounded it.
type Snapshot struct {
	Store    string    `json:"store" doc:"The store id. Send it back as store."`
	Seq      int64     `json:"seq" doc:"The seq the snapshot was taken at. Send it back as after."`
	Sessions []Session `json:"sessions" doc:"Newest activity first."`
	// NextBefore is the before for the next, older page, set only by Sessions.
	NextBefore *int64 `json:"next_before,omitempty"`
}

// Session is one session in a snapshot.
type Session struct {
	ID      string    `json:"id"`
	Harness string    `json:"agent" doc:"From the session's latest turn.started or run that names one. Empty for a session no run started."`
	Client  string    `json:"client" doc:"The harness, from its latest event that named one."`
	Folder  string    `json:"folder" doc:"From the session's latest turn.started or run that names one. Empty for a session no run started."`
	Started time.Time `json:"started"`
	Last    time.Time `json:"last"`
	Open    bool      `json:"open" doc:"A run's child for this session is running: a run.started without its run.ended."`
	Turns   int       `json:"turns"`
	// FirstAgent stays put when a later turn runs on another harness, so a
	// list grouped by it keeps the session in one place.
	FirstAgent   string `json:"first_agent,omitempty" doc:"The agent (harness) of the session's first turn.started or run that names one. A later turn may run on another agent; agent is then that one's, and first_agent stays. Absent for a session no run started."`
	FirstAgentID string `json:"first_agent_id,omitempty" doc:"The saved agent's id from the same event as first_agent: the saved agent the session started on. Absent when that turn named a harness alone, or no run started the session."`
	// Origin is the origin of the session's first turn: who started the session
	// when the person did not. kind asked marks a session another agent's turn
	// asked for (ask_agent). Absent for a session the person started. A view
	// can show only the person's sessions by hiding those with an asked origin,
	// nothing dropped from the record.
	Origin *event.Origin `json:"origin,omitempty" doc:"The origin of the session's first turn: kind asked when another agent's turn asked for this session (ask_agent), naming that turn. Absent for a session the person started."`
	// Usage sums the model.responded events that carried usage. It is nil when
	// none did: absent usage is unknown, never zero.
	Usage *event.Usage `json:"usage" doc:"Sums the replies that carried usage. Null when none did: unknown, never zero."`
	// CostUSD sums the cost_usd of every model.responded. CostComplete is
	// false when any of them lacks a complete cost, or a drop names the
	// session; CostUSD is then a lower bound.
	CostUSD      float64 `json:"cost_usd" doc:"Sums cost_usd over the session's model.responded events."`
	CostComplete bool    `json:"cost_complete" doc:"False when a reply lacks a complete cost, or a drop names the session."`
	// Resumable: the session has a resume_id on a run.ended and is not
	// open, so POST /api/v0/turns with this session can run its next turn.
	Resumable bool `json:"resumable" doc:"POST /api/v0/turns accepts it as session."`
	// Title is the title of the session's latest session.renamed, else the
	// text of its first turn.started that has text, the person's own words,
	// cut to TitleRunes. Absent with neither.
	Title string `json:"title,omitempty" doc:"The name from the session's latest session.renamed; without one, the person's words from the first turn.started with text, cut to 120 characters."`
	// Archived is when the session's latest session.archived set it archived,
	// nil when it is not archived (never archived, or the latest un-archived
	// it). Archive is only a view: it hides the session from the default list
	// and removes nothing.
	Archived *time.Time `json:"archived,omitempty" doc:"When the session was archived (PATCH archived:true); absent when not archived. Only a view: the list hides archived sessions unless archived=true, and the session still takes a turn."`
	// Workspace is where the session's turns run, and Sandbox is that
	// workspace's mode, folded in so a list need not read each workspace. The
	// workspace is the one the session's turn.started events name, or the
	// session's own id for a session migrated from before workspaces. The
	// mode comes from the workspace's workspace.created.
	Workspace string `json:"workspace,omitempty" doc:"The session's workspace, where its turns run (ws_ and 16 hex digits, or the session's own id for a migrated session)."`
	Sandbox   bool   `json:"sandbox" doc:"The session's workspace mode: true for a copy behind a wall, false for the person's real folder."`

	// LastSeq is the seq of the session's latest activity (the same events that
	// set last, so not a rename or a stored preview). Sessions are ordered by
	// it, newest first, and it is the cursor a page is taken before. Not sent.
	LastSeq int64 `json:"-"`

	// Why cost_complete is false. Not sent: the contract has no field for
	// them.
	UsageMissing     int  `json:"-"` // model.responded events with no usage
	OutputIncomplete bool `json:"-"` // a call's output count was a lower bound
	CostMissing      int  `json:"-"` // model.responded events without cost_complete true
	EventsDropped    bool `json:"-"` // an events.dropped names this session or all
}

// Snapshot computes every session from the stored events.
func (s *Store) Snapshot() (Snapshot, error) {
	return s.snapshotAt(s.Last())
}

// Sessions is a page of the snapshot taken at a fixed seq, newest activity
// first (by the activity's time). at pins the snapshot: a negative at is no pin
// (the latest, and its seq is the page's seq), while 0 or more pins to that
// seq, so later pages pass the first page's seq back and every page is of the
// same snapshot -- without it a session's own activity would move it and the
// paging would skip it. before is the previous page's next_before, naming its
// last session by its unique last seq; the page is the sessions after it in the
// order. next_before is set to the last one shown when an older one was left
// out. The whole snapshot is still computed; the page bounds the answer.
func (s *Store) Sessions(at, before int64, limit int) (Snapshot, error) {
	// A pin above the latest seq would let later events in, which is what the
	// pin is for keeping out, so it is capped to the latest; the API rejects it
	// outright so a caller cannot page against a snapshot that never existed.
	hi := s.Last()
	if at >= 0 && at < hi {
		hi = at
	}
	snap, err := s.snapshotAt(hi)
	if err != nil {
		return snap, err
	}
	all := snap.Sessions
	if before > 0 {
		// before is the last session of the previous page, named by its unique
		// LastSeq. The next page is the sessions after it in the stable order,
		// so the order can be by time while the cursor stays a plain seq. A
		// before that names no session (stale, or not of this pin) pages past
		// the end.
		i := 0
		for i < len(all) && all[i].LastSeq != before {
			i++
		}
		if i < len(all) {
			all = all[i+1:]
		} else {
			all = nil
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
		// next_before is the last session shown, as GET /turns does it: the
		// next page is the sessions below it, so none repeats and none is
		// skipped.
		next := all[len(all)-1].LastSeq
		snap.NextBefore = &next
	}
	snap.Sessions = all
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
	// A session created with a title (POST /api/v0/sessions) keeps it until a
	// rename: it wins over the first message (the person named the session) and
	// loses to a later rename.
	createdTitle = `SELECT session, COALESCE(json_extract(data, '$.title'), ''), MAX(seq) FROM events
		WHERE kind = 'session.created' AND COALESCE(json_extract(data, '$.title'), '') <> ''`
	// The session's latest session.archived: its archived flag and time, from
	// the row MAX(seq) picks. archived 1 means archived at that time, 0 (the
	// un-archive, which omits the field) means not.
	latestArchived = `SELECT session, COALESCE(json_extract(data, '$.archived'), 0), time, MAX(seq) FROM events
		WHERE kind = 'session.archived'`
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
	for i, q := range []string{firstText, createdTitle, latestRename} {
		var id, text string
		var seq int64
		err := s.r.QueryRow(q+` AND session = ? GROUP BY session`, e.Session).Scan(&id, &text, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return e, fmt.Errorf("store: read title: %w", err)
		}
		old = titleOf(i == 0, text)
	}
	e.Kind = event.SessionRenamed
	e.OldTitle = old
	out, _, err := s.appendLocked(e)
	return out, err
}

// SetArchived records e, a session.archived with e.Archived set, unless the
// session is already in that state: then it is a no-op (changed false) and the
// first archive time is kept, as CloseSession keeps the first closed time. The
// current state is read and e written under one lock, so the latest event
// wins. It does not check the session exists; the caller does. Like Rename, it
// works on a session that has no workspace row yet.
func (s *Store) SetArchived(e event.Event) (out event.Event, changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return e, false, ErrClosed
	}
	var cur int
	err = s.r.QueryRow(`SELECT COALESCE(json_extract(data, '$.archived'), 0) FROM events
		WHERE kind = 'session.archived' AND session = ? ORDER BY seq DESC LIMIT 1`, e.Session).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e, false, fmt.Errorf("store: read archived: %w", err)
	}
	if (cur != 0) == e.Archived {
		return e, false, nil // already in that state: keep the first time
	}
	e.Kind = event.SessionArchived
	out, _, err = s.appendLocked(e)
	return out, err == nil, err
}

// titleOf is the title text gives: a rename's as it is, a first message's
// (first true) on one line, each run of spaces, line breaks and other
// control characters made one space, as a rename must be.
func titleOf(first bool, text string) string {
	if first {
		text = strings.Join(strings.FieldsFunc(text, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
		}), " ")
	}
	return title(text)
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

// CostOfTurns sums cost_usd over the model.responded events of the turns
// named, each counted once: what a request tree has cost, by its turns
// (queue.CostReader). A session's other turns are not in it.
func (s *Store) CostOfTurns(turns []string) (float64, error) {
	seen := map[string]bool{}
	var args []any
	for _, id := range turns {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
	}
	var cost float64
	// SQLite takes at most 32766 variables, so a large tree is summed in parts.
	for len(args) > 0 {
		part := args[:min(len(args), 500)]
		args = args[len(part):]
		var c float64
		err := s.r.QueryRow(`SELECT COALESCE(SUM(json_extract(data, '$.cost_usd')), 0) FROM events
			WHERE kind = 'model.responded' AND turn IN (?`+strings.Repeat(",?", len(part)-1)+`)`, part...).Scan(&c)
		if err != nil {
			return 0, fmt.Errorf("store: sum cost: %w", err)
		}
		cost += c
	}
	return cost, nil
}
