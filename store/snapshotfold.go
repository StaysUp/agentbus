package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/event"
)

// The session list is a fold of the record, kept as it goes. Every fact a
// Session carries is a sum, a least or greatest value, or the value of the
// first or latest event of some kind, over the session's events. So what the
// events of one seq range say (a fold) merges with what the next range says
// into what both say, and the store never reads an event twice for it: a
// snapshot at seq hi is the fold kept at some lo ≤ hi, plus the fold of the
// events in (lo, hi]. Events are only ever appended, each with the next seq,
// so a range once folded never changes. This is how NATS keeps a stream's
// per-subject state (stree, in memory, rebuilt from the log at start) and
// Temporal keeps a visibility row per workflow, both updated as records
// arrive rather than recomputed on read.
//
// (2026-11-28: computed from every event on each read, GET /api/v0/sessions
// took 0.5 s to 4.6 s on a record of 41,000 events, and the cockpit asks for
// it after events arrive.)
//
// The fold is the store's own read of the record, at a seq, not a second
// record: the events stay the one source, and a fold is made again from them
// after every start.

// keptFolds is how many folds the store keeps, by their seq: the latest, and
// a few before it, so the later pages of a paged list (pinned to the first
// page's seq) find their fold while new events arrive.
const keptFolds = 4

// folds holds the kept folds, newest last.
type folds struct {
	mu   sync.Mutex
	kept []*fold
}

// fold is what the events with seq in (lo, hi] say about the sessions, or
// with lo 0, every event up to hi.
type fold struct {
	hi       int64
	sessions map[string]*sessionFold
	// sandbox is each workspace's mode, from its latest workspace.created.
	sandbox map[string]atSeq[bool]
	// dropAll and dropped: an events.dropped said every session, or named
	// these.
	dropAll bool
	dropped map[string]bool
}

// atSeq is a value and the seq of the event it came from; seq 0 is none.
type atSeq[T any] struct {
	seq int64
	v   T
}

// earlier keeps b when it came first.
func (a *atSeq[T]) earlier(b atSeq[T]) {
	if b.seq != 0 && (a.seq == 0 || b.seq < a.seq) {
		*a = b
	}
}

// later keeps b when it came last.
func (a *atSeq[T]) later(b atSeq[T]) {
	if b.seq > a.seq {
		*a = b
	}
}

// least and most merge an aggregate that may have no value (NULL).
func least(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid || (b.Valid && b.Int64 < a.Int64) {
		return b
	}
	return a
}

func most(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid || (b.Valid && b.Int64 > a.Int64) {
		return b
	}
	return a
}

// holdKey names a run's hold: its session's hold id, which an older store
// may leave out (NULL).
type holdKey struct {
	hold  string
	valid bool
}

// holdCount counts a hold's run.started and run.ended.
type holdCount struct{ started, ended int }

// sessionFold is what one range of events says about one session.
type sessionFold struct {
	// The activity bounds: activity is every event but a rename, a stored
	// preview and an archive; all is every event. A session with no activity
	// takes all's.
	firstAct, firstAll, lastAct, lastAll sql.NullInt64 // times
	lastActSeq, lastAllSeq               sql.NullInt64
	turns                                int
	withUsage, usageMissing, costMissing int
	usage                                event.Usage
	outputIncomplete                     bool
	cost                                 float64
	client                               atSeq[string] // the latest that named one
	agent                                atSeq[[2]string]
	firstAgent                           atSeq[[2]string] // agent, saved agent id
	turnOrigin                           atSeq[string]    // the first turn.started's
	createdOrigin                        atSeq[*event.Origin]
	firstText, createdTitle, rename      atSeq[string]
	archived                             atSeq[[2]int64] // archived flag, time
	resume                               bool
	workspace                            atSeq[string]
	holds                                map[holdKey]holdCount
}

func newFold(hi int64) *fold {
	return &fold{hi: hi, sessions: map[string]*sessionFold{}, sandbox: map[string]atSeq[bool]{}, dropped: map[string]bool{}}
}

func (f *fold) session(id string) *sessionFold {
	x := f.sessions[id]
	if x == nil {
		x = &sessionFold{holds: map[holdKey]holdCount{}}
		f.sessions[id] = x
	}
	return x
}

// merge returns the fold of f's range followed by g's, g starting where f
// ends. Neither is changed.
func (f *fold) merge(g *fold) *fold {
	out := newFold(g.hi)
	for id, x := range f.sessions {
		c := *x
		c.holds = make(map[holdKey]holdCount, len(x.holds))
		for k, v := range x.holds {
			c.holds[k] = v
		}
		out.sessions[id] = &c
	}
	for id, y := range g.sessions {
		x := out.session(id)
		x.firstAct, x.firstAll = least(x.firstAct, y.firstAct), least(x.firstAll, y.firstAll)
		x.lastAct, x.lastAll = most(x.lastAct, y.lastAct), most(x.lastAll, y.lastAll)
		x.lastActSeq, x.lastAllSeq = most(x.lastActSeq, y.lastActSeq), most(x.lastAllSeq, y.lastAllSeq)
		x.turns += y.turns
		x.withUsage += y.withUsage
		x.usageMissing += y.usageMissing
		x.costMissing += y.costMissing
		x.usage.InputTokens += y.usage.InputTokens
		x.usage.OutputTokens += y.usage.OutputTokens
		x.usage.CacheReadTokens += y.usage.CacheReadTokens
		x.usage.CacheWriteTokens += y.usage.CacheWriteTokens
		x.usage.CacheWrite5mTokens += y.usage.CacheWrite5mTokens
		x.usage.CacheWrite1hTokens += y.usage.CacheWrite1hTokens
		x.usage.CacheWrite30mTokens += y.usage.CacheWrite30mTokens
		x.usage.CacheWrite12hTokens += y.usage.CacheWrite12hTokens
		x.outputIncomplete = x.outputIncomplete || y.outputIncomplete
		x.cost += y.cost
		x.client.later(y.client)
		x.agent.later(y.agent)
		x.firstAgent.earlier(y.firstAgent)
		x.turnOrigin.earlier(y.turnOrigin)
		x.createdOrigin.earlier(y.createdOrigin)
		x.firstText.earlier(y.firstText)
		x.createdTitle.later(y.createdTitle)
		x.rename.later(y.rename)
		x.archived.later(y.archived)
		x.resume = x.resume || y.resume
		x.workspace.later(y.workspace)
		for k, v := range y.holds {
			c := x.holds[k]
			x.holds[k] = holdCount{c.started + v.started, c.ended + v.ended}
		}
	}
	for ws, v := range f.sandbox {
		out.sandbox[ws] = v
	}
	for ws, v := range g.sandbox {
		c := out.sandbox[ws]
		c.later(v)
		out.sandbox[ws] = c
	}
	out.dropAll = f.dropAll || g.dropAll
	for id := range f.dropped {
		out.dropped[id] = true
	}
	for id := range g.dropped {
		out.dropped[id] = true
	}
	return out
}

// snapshotAt is every session as the events up to hi describe them, so a
// page taken at one hi is stable while later events arrive: a session's
// activity after hi does not move it, so paging it never skips or repeats
// one. It starts from the latest kept fold at or below hi and reads only the
// events after it.
func (s *Store) snapshotAt(hi int64) (Snapshot, error) {
	s.folds.mu.Lock()
	defer s.folds.mu.Unlock()
	base := newFold(0)
	for _, f := range s.folds.kept {
		if f.hi <= hi && f.hi >= base.hi {
			base = f
		}
	}
	f := base
	if base.hi < hi {
		delta, err := s.foldRange(base.hi, hi)
		if err != nil {
			return Snapshot{Store: s.id, Seq: hi, Sessions: []Session{}}, err
		}
		f = base.merge(delta)
		s.folds.keep(f)
	}
	return f.snapshot(s.id), nil
}

// keep adds f to the kept folds, dropping the oldest past keptFolds.
func (k *folds) keep(f *fold) {
	k.kept = append(k.kept, f)
	sort.Slice(k.kept, func(i, j int) bool { return k.kept[i].hi < k.kept[j].hi })
	if len(k.kept) > keptFolds {
		k.kept = k.kept[len(k.kept)-keptFolds:]
	}
}

// snapshot is the sessions f describes.
func (f *fold) snapshot(store string) Snapshot {
	snap := Snapshot{Store: store, Seq: f.hi, Sessions: make([]Session, 0, len(f.sessions))}
	for id, y := range f.sessions {
		if !y.lastAll.Valid {
			continue // named by an event without a session's activity row
		}
		x := Session{ID: id, Turns: y.turns, Client: y.client.v,
			UsageMissing: y.usageMissing, OutputIncomplete: y.outputIncomplete, CostMissing: y.costMissing, CostUSD: y.cost}
		first, last, lastSeq := y.firstAct, y.lastAct, y.lastActSeq
		if !first.Valid {
			first = y.firstAll
		}
		if !last.Valid {
			last = y.lastAll
		}
		if !lastSeq.Valid {
			lastSeq = y.lastAllSeq
		}
		x.Started, x.Last, x.LastSeq = time.Unix(0, first.Int64).UTC(), time.Unix(0, last.Int64).UTC(), lastSeq.Int64
		if y.withUsage > 0 {
			u := y.usage
			u.OutputIncomplete = y.outputIncomplete
			x.Usage = &u
		}
		x.Harness, x.Folder = y.agent.v[0], y.agent.v[1]
		x.FirstAgent, x.FirstAgentID = y.firstAgent.v[0], y.firstAgent.v[1]
		// The first turn.started's origin wins; without one, the first
		// session.created that names one (an asked session before its turn).
		if y.turnOrigin.v != "" {
			var o event.Origin
			if json.Unmarshal([]byte(y.turnOrigin.v), &o) == nil {
				x.Origin = &o
			}
		}
		if x.Origin == nil && y.createdOrigin.v != nil {
			o := *y.createdOrigin.v
			x.Origin = &o
		}
		// A rename wins, then a title the session was created with, then
		// the first turn's text.
		switch {
		case y.rename.seq != 0:
			x.Title = titleOf(false, y.rename.v)
		case y.createdTitle.seq != 0:
			x.Title = titleOf(false, y.createdTitle.v)
		case y.firstText.seq != 0:
			x.Title = titleOf(true, y.firstText.v)
		}
		if y.archived.seq != 0 && y.archived.v[0] != 0 {
			t := time.Unix(0, y.archived.v[1]).UTC()
			x.Archived = &t
		}
		for _, c := range y.holds {
			if c.started > 0 && c.ended == 0 {
				x.Open = true
			}
		}
		x.Resumable = y.resume && !x.Open
		x.EventsDropped = f.dropAll || f.dropped[id]
		ws := y.workspace.v
		if ws == "" {
			ws = id // a migrated session's workspace is its own id
		}
		x.Workspace, x.Sandbox = ws, f.sandbox[ws].v
		// A call without a complete cost covers no usage, an incomplete
		// output count and a model without a price: the cost step marks
		// each of those incomplete.
		x.CostComplete = x.CostMissing == 0 && x.UsageMissing == 0 && !x.OutputIncomplete && !x.EventsDropped
		snap.Sessions = append(snap.Sessions, x)
	}
	// Newest activity first, by the time of the activity, not the seq it was
	// stored at: a remote runner can forward an older event later, so a higher
	// seq is not always a later moment. LastSeq is only the page cursor, unique
	// per session, not the order.
	sort.Slice(snap.Sessions, func(i, j int) bool {
		a, b := snap.Sessions[i], snap.Sessions[j]
		if !a.Last.Equal(b.Last) {
			return a.Last.After(b.Last)
		}
		return a.ID < b.ID
	})
	return snap
}

// inRange bounds each of foldRange's reads to its range, (lo, hi]. A fold
// from the start (lo 0) reads with wholeRange instead, the same two
// arguments with no lower bound on the seq, so SQLite reads the whole list
// through an index (events_activity) rather than every row by its seq.
const (
	inRange    = `seq > ? AND seq <= ?`
	wholeRange = `? = 0 AND seq <= ?`
)

// activitySQL is each session's activity in a range: its bounds, turns and
// latest client's seq. Every column it reads is in events_activity.
const activitySQL = `SELECT session,
			MIN(CASE WHEN kind NOT IN ('session.renamed', 'changes.previewed', 'session.archived') THEN time END), MIN(time),
			MAX(CASE WHEN kind NOT IN ('session.renamed', 'changes.previewed', 'session.archived') THEN time END), MAX(time),
			MAX(CASE WHEN kind NOT IN ('session.renamed', 'changes.previewed', 'session.archived') THEN seq END), MAX(seq),
			SUM(kind = 'turn.started'),
			MAX(CASE WHEN client <> '' THEN seq END)
		FROM events
		WHERE ` + inRange + ` AND session <> ''
		GROUP BY session`

// usageSQL sums each session's replies in a range: the one read of the list
// that needs an event's data, so only model.responded rows are read.
const usageSQL = `SELECT session,
			SUM(COALESCE(json_type(data, '$.usage'), 'null') = 'object'),
			SUM(COALESCE(json_type(data, '$.usage'), 'null') <> 'object'),
			COALESCE(SUM(json_extract(data, '$.usage.input_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.output_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.cache_read_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.cache_write_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.cache_write_5m_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.cache_write_1h_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.cache_write_30m_tokens')), 0),
			COALESCE(SUM(json_extract(data, '$.usage.cache_write_12h_tokens')), 0),
			COALESCE(MAX(json_extract(data, '$.usage.output_incomplete') = 1), 0),
			COALESCE(SUM(json_extract(data, '$.cost_usd')), 0),
			SUM(COALESCE(json_extract(data, '$.cost_complete'), 0) <> 1)
		FROM events
		WHERE ` + inRange + ` AND kind = 'model.responded' AND session <> ''
		GROUP BY session`

// foldRange folds the events with seq in (lo, hi]. Each query reads that
// range only, through the seq (the rowid), so a small range costs little
// however large the record.
func (s *Store) foldRange(lo, hi int64) (*fold, error) {
	f := newFold(hi)
	each := func(q string, scan func(*sql.Rows) error) error {
		if lo == 0 {
			q = strings.Replace(q, inRange, wholeRange, 1)
		}
		rows, err := s.r.Query(q, lo, hi)
		if err != nil {
			return fmt.Errorf("store: snapshot: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return fmt.Errorf("store: snapshot: %w", err)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: snapshot: %w", err)
		}
		return nil
	}
	// A rename, a stored preview or an archive is not activity: each leaves
	// started and last as they were.
	err := each(activitySQL, func(rows *sql.Rows) error {
		var id string
		var y sessionFold
		var clientSeq sql.NullInt64
		if err := rows.Scan(&id, &y.firstAct, &y.firstAll, &y.lastAct, &y.lastAll, &y.lastActSeq, &y.lastAllSeq,
			&y.turns, &clientSeq); err != nil {
			return err
		}
		y.holds = map[holdKey]holdCount{}
		f.sessions[id] = &y
		if clientSeq.Valid {
			y.client.seq = clientSeq.Int64
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The usage and cost of the session's replies: the one part of the list
	// that reads an event's data, so only the model.responded rows are read.
	if err := each(usageSQL, func(rows *sql.Rows) error {
		var id string
		var u event.Usage
		var withUsage, usageMissing, costMissing int
		var outputIncomplete bool
		var cost float64
		if err := rows.Scan(&id, &withUsage, &usageMissing,
			&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens,
			&u.CacheWrite5mTokens, &u.CacheWrite1hTokens, &u.CacheWrite30mTokens, &u.CacheWrite12hTokens,
			&outputIncomplete, &cost, &costMissing); err != nil {
			return err
		}
		y := f.session(id)
		y.usage, y.withUsage, y.usageMissing, y.costMissing = u, withUsage, usageMissing, costMissing
		y.outputIncomplete, y.cost = outputIncomplete, cost
		return nil
	}); err != nil {
		return nil, err
	}
	// The harness of the session's latest event that named one: the scan
	// above found its seq, so each is one read by seq, not a second scan.
	for _, y := range f.sessions {
		if y.client.seq == 0 {
			continue
		}
		if err := s.r.QueryRow(`SELECT client FROM events WHERE seq = ?`, y.client.seq).Scan(&y.client.v); err != nil {
			return nil, fmt.Errorf("store: snapshot: %w", err)
		}
	}
	// The latest turn or run that names them gives the agent and the folder
	// (agentFrom).
	if err := each(`SELECT session, COALESCE(json_extract(data, '$.agent'), ''), COALESCE(json_extract(data, '$.folder'), ''), MAX(seq)
		FROM events WHERE `+inRange+` AND `+agentFrom+` GROUP BY session`, func(rows *sql.Rows) error {
		var id string
		var v atSeq[[2]string]
		if err := rows.Scan(&id, &v.v[0], &v.v[1], &v.seq); err != nil {
			return err
		}
		f.session(id).agent = v
		return nil
	}); err != nil {
		return nil, err
	}
	// The first that names an agent gives first_agent, and its saved agent
	// first_agent_id, both from one event.
	if err := each(`SELECT session, json_extract(data, '$.agent'), COALESCE(json_extract(data, '$.agent_id'), ''), MIN(seq)
		FROM events WHERE `+inRange+` AND `+agentFrom+` GROUP BY session`, func(rows *sql.Rows) error {
		var id string
		var v atSeq[[2]string]
		if err := rows.Scan(&id, &v.v[0], &v.v[1], &v.seq); err != nil {
			return err
		}
		f.session(id).firstAgent = v
		return nil
	}); err != nil {
		return nil, err
	}
	// The origin of the session's first turn.started: who started the session
	// when the person did not (ask_agent gives kind asked).
	if err := each(`SELECT session, COALESCE(json_extract(data, '$.origin'), ''), MIN(seq)
		FROM events WHERE `+inRange+` AND kind = 'turn.started' GROUP BY session`, func(rows *sql.Rows) error {
		var id string
		var v atSeq[string]
		if err := rows.Scan(&id, &v.v, &v.seq); err != nil {
			return err
		}
		f.session(id).turnOrigin = v
		return nil
	}); err != nil {
		return nil, err
	}
	// An asked session (POST /api/v0/sessions by a turn token) carries the
	// asker's origin on session.created, so it names who asked even before its
	// turn starts: the first such event whose origin reads.
	if err := each(`SELECT session, json_extract(data, '$.origin'), seq FROM events
		WHERE `+inRange+` AND kind = 'session.created' AND COALESCE(json_extract(data, '$.origin'), '') <> ''`, func(rows *sql.Rows) error {
		var id, origin string
		var seq int64
		if err := rows.Scan(&id, &origin, &seq); err != nil {
			return err
		}
		var o event.Origin
		if json.Unmarshal([]byte(origin), &o) == nil {
			f.session(id).createdOrigin.earlier(atSeq[*event.Origin]{seq, &o})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Each run's hold: open while it has a run.started and no run.ended.
	if err := each(`SELECT session, json_extract(data, '$.hold'), SUM(kind = 'run.started'), SUM(kind = 'run.ended') FROM events
		WHERE `+inRange+` AND kind IN ('run.started', 'run.ended') GROUP BY session, json_extract(data, '$.hold')`, func(rows *sql.Rows) error {
		var id string
		var hold sql.NullString
		var c holdCount
		if err := rows.Scan(&id, &hold, &c.started, &c.ended); err != nil {
			return err
		}
		f.session(id).holds[holdKey{hold.String, hold.Valid}] = c
		return nil
	}); err != nil {
		return nil, err
	}
	// The title's three sources (firstText, createdTitle, latestRename): a
	// probe (Claude Code's startup quota check, marked probe: true) never
	// names one. SQLite takes the bare column from the row MAX(seq) or
	// MIN(seq) picked.
	for _, t := range []struct {
		q   string
		set func(*sessionFold, atSeq[string])
	}{
		{firstText, func(x *sessionFold, v atSeq[string]) { x.firstText = v }},
		{createdTitle, func(x *sessionFold, v atSeq[string]) { x.createdTitle = v }},
		{latestRename, func(x *sessionFold, v atSeq[string]) { x.rename = v }},
	} {
		if err := each(t.q+` AND `+inRange+` GROUP BY session`, func(rows *sql.Rows) error {
			var id string
			var v atSeq[string]
			if err := rows.Scan(&id, &v.v, &v.seq); err != nil {
				return err
			}
			t.set(f.session(id), v)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	// The latest session.archived decides whether a session is archived, and
	// when. archived 0 (the un-archive) leaves it not archived.
	if err := each(latestArchived+` AND `+inRange+` GROUP BY session`, func(rows *sql.Rows) error {
		var id string
		var v atSeq[[2]int64]
		if err := rows.Scan(&id, &v.v[0], &v.v[1], &v.seq); err != nil {
			return err
		}
		f.session(id).archived = v
		return nil
	}); err != nil {
		return nil, err
	}
	// A session with a resume_id can be continued.
	if err := each(`SELECT DISTINCT session FROM events
		WHERE `+inRange+` AND kind = 'run.ended' AND json_extract(data, '$.resume_id') IS NOT NULL`, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		f.session(id).resume = true
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT json_extract(data, '$.all_sessions'), json_extract(data, '$.sessions')
		FROM events WHERE `+inRange+` AND kind = 'events.dropped'`, func(rows *sql.Rows) error {
		var all sql.NullInt64
		var list sql.NullString
		if err := rows.Scan(&all, &list); err != nil {
			return err
		}
		if all.Int64 == 1 {
			f.dropAll = true
		}
		for _, id := range decodeList(list.String) {
			f.dropped[id] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Each session's workspace: the latest its session.created or turn.started
	// events name, or (none named) the session's own id, which is a migrated
	// session's workspace id. session.created carries it for a session with no
	// turn yet.
	if err := each(`SELECT session, json_extract(data, '$.workspace'), MAX(seq) FROM events
		WHERE `+inRange+` AND kind IN ('session.created', 'turn.started') AND COALESCE(json_extract(data, '$.workspace'), '') <> ''
		GROUP BY session`, func(rows *sql.Rows) error {
		var id string
		var v atSeq[string]
		if err := rows.Scan(&id, &v.v, &v.seq); err != nil {
			return err
		}
		f.session(id).workspace = v
		return nil
	}); err != nil {
		return nil, err
	}
	// A workspace's mode: its latest workspace.created's sandbox.
	if err := each(`SELECT json_extract(data, '$.workspace'), COALESCE(json_extract(data, '$.sandbox'), 0), MAX(seq) FROM events
		WHERE `+inRange+` AND kind = 'workspace.created' GROUP BY json_extract(data, '$.workspace')`, func(rows *sql.Rows) error {
		var ws string
		var sandbox int
		var seq int64
		if err := rows.Scan(&ws, &sandbox, &seq); err != nil {
			return err
		}
		f.sandbox[ws] = atSeq[bool]{seq, sandbox != 0}
		return nil
	}); err != nil {
		return nil, err
	}
	return f, nil
}
