package queue

// An on over a session: "every time a turn of session S completes, deliver
// its reply to session T". It is the public on (POST /api/v0/ons, 0.6.8): an
// event kind on the record and a filter, never one turn. The ask_agent and
// review_turn apps compose it themselves: they make the asked session, register
// this on over it for the asking session, then start its turn. So every later
// turn of the asked session reaches the asker too, a review's result included.
//
// It reads the record as the put-away's on does (recordon.go): a cursor, the
// record seq it has read to, moved past each turn.ended it reads. A fire is
// kept in on_fires keyed by the turn, and the delivered turn's key is the on
// and the turn (ResultKey), so a second tick or a restart delivers nothing
// twice. It starts reading at the record's end when it is made, so it fires
// only on turns that end after it: an on never starts work by itself.
//
// It repeats, so it must not deliver in a circle: an on whose delivery would
// reach its own watched session again through live ons over sessions is
// refused (ErrCycle). Registering an on costs nothing; each turn it delivers
// is a turn its maker asked for, at the maker's depth, under the same turn
// caps as an ask (deliverResult), so a chain of ons stops where a chain of asks
// does.
//
// Prior art (_references/). Temporal pushes: a signal names the workflow it
// goes to, and a caller hears a run end through completion callbacks set when
// the run starts (frontend/workflow_handler.go, CompletionCallbacks), a
// per-run shape that the "no shortcuts" rule leaves to apps here. Buildkite's
// agent hooks (pre-command, post-command, pre-exit; internal/job/executor.go)
// are standing scripts per phase of every job, configured once, never per job.
// NATS JetStream's consumer is this on's shape: filter subjects plus a durable
// position in the stream (server/consumer.go, FilterSubjects).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// OnFilter is what an on over a session matches: the session whose turns it
// watches.
type OnFilter struct {
	Session string `json:"session,omitempty"`
}

// WatchEvents are the events an on over a session may name. turn.completed
// reads the record's turn.ended, which control writes once for every ended
// turn job, however it ended.
var WatchEvents = []string{OnTurnCompleted}

// recordKindOf is the record kind an on's event reads.
func recordKindOf(event string) string {
	if event == OnTurnCompleted {
		return "turn.ended"
	}
	return ""
}

// ErrCycle: an on over a session would deliver in a circle, back into the
// session it watches.
var ErrCycle = errors.New("queue: an on would deliver in a circle")

// RegisterWatchOn stores an on over session filter.Session that delivers the
// reply of each of its turns that ends from now on to session to, run on
// maker's agent, the delivered turn held to budget (nil for the main token's).
// from is the record seq it starts reading after: the record's end now.
//
// key makes it once, as POST /api/v0/turns: a key already used returns its on
// (removed or not) when the fields match, else ErrKeyConflict. A live on of
// the same event, filter and target is returned rather than a second one, so
// a session's results are delivered once, and a key sent then is bound to it. An on whose delivery would come back
// round to the watched session, itself included, is ErrCycle.
func (q *Queue) RegisterWatchOn(team, event string, filter OnFilter, to, key string, maker Maker, budget *Limits, from int64) (On, error) {
	if recordKindOf(event) == "" {
		return On{}, fmt.Errorf("%w: %q", ErrUnknownEvent, event)
	}
	if filter.Session == "" || to == "" {
		return On{}, ErrBadState
	}
	mk, err := json.Marshal(maker)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	fl, err := json.Marshal(filter)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	o := On{ID: NewID("on_"), Team: team, Event: event, Session: to, Maker: maker, Created: nanos(q.now()), Filter: filter}
	same := func(e On) bool { return e.Event == event && e.Subject == "" && e.Filter == filter && e.Session == to }
	err = q.tx(func(tx *sql.Tx) error {
		if key != "" {
			existing, err := onByKey(tx, team, key)
			switch {
			case err == nil:
				if !same(existing) {
					return ErrKeyConflict
				}
				o = existing
				return nil
			case !errors.Is(err, ErrNotFound):
				return err
			}
		}
		var id string
		switch err := tx.QueryRow(`SELECT id FROM ons WHERE team = ? AND event = ? AND subject = '' AND session = ? AND filter = ? AND off IS NULL
			ORDER BY created, id LIMIT 1`, team, event, to, string(fl)).Scan(&id); {
		case err == nil:
			// One live on per watched session and target: a second would
			// deliver every result twice. A key sent with it is bound to the
			// on it returns (Codex review t_58b5f94f2ace24d3, P2), so a reuse
			// with other fields is a conflict and a retry after a removal
			// returns this on rather than make another.
			if key != "" {
				if _, err := tx.Exec(`INSERT INTO on_keys(team, key, on_id) VALUES(?,?,?)`, team, key, id); err != nil {
					return fmt.Errorf("queue: bind on key: %w", err)
				}
			}
			o.ID = id
			return committed{errFound} // keep the bound key
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("queue: register on: %w", err)
		}
		if circle, err := reaches(tx, team, to, filter.Session); err != nil {
			return err
		} else if circle {
			return ErrCycle
		}
		if _, err := tx.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, idem_key, created, limits, cursor, filter) VALUES(?,?,?,'',?,?,?,?,?,?,?)`,
			o.ID, team, event, to, string(mk), key, o.Created, limitsColumn(budget), from, string(fl)); err != nil {
			return fmt.Errorf("queue: register on: %w", err)
		}
		return nil
	})
	if errors.Is(err, errFound) {
		return q.GetOn(team, o.ID)
	}
	if err != nil {
		return On{}, err
	}
	return o, nil
}

// RefusedReporter records that on did not deliver work's result to session,
// and why. Control writes it to the record (delivery.refused). An error
// leaves the on where it was, so the next check refuses and reports again.
type RefusedReporter func(on, session string, work Job, why string) error

// ReportRefusedWith sets how the queue records a delivery it refused. Set it
// before the queue is used; without it a refusal is kept in on_fires only.
func (q *Queue) ReportRefusedWith(r RefusedReporter) { q.refused = r }

// reportRefused records why on did not deliver work's result, when set.
func (q *Queue) reportRefused(on, session string, work Job, why string) error {
	if why != "" && q.refused != nil {
		return q.refused(on, session, work, why)
	}
	return nil
}

// errFound ends RegisterWatchOn's transaction when a live on already does
// what was asked.
var errFound = errors.New("queue: on found")

// reaches says whether deliveries into session from reach session target
// through live ons over sessions: from itself, or a session an on watching
// from delivers to, and so on.
func reaches(tx *sql.Tx, team, from, target string) (bool, error) {
	seen := map[string]bool{from: true}
	next := []string{from}
	for len(next) > 0 {
		s := next[0]
		next = next[1:]
		if s == target {
			return true, nil
		}
		rows, err := tx.Query(`SELECT session FROM ons WHERE team = ? AND subject = '' AND off IS NULL AND json_extract(filter, '$.session') = ?`, team, s)
		if err != nil {
			return false, fmt.Errorf("queue: read ons: %w", err)
		}
		for rows.Next() {
			var to string
			if err := rows.Scan(&to); err != nil {
				rows.Close()
				return false, fmt.Errorf("queue: read ons: %w", err)
			}
			if !seen[to] {
				seen[to] = true
				next = append(next, to)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("queue: read ons: %w", err)
		}
	}
	return false, nil
}

// FireWatchOns is the control loop's check for ons over a session: for each
// live one, it reads the record's turn.ended events past its cursor, and for
// each turn of its watched session delivers the turn's reply to its target, as
// FireOns does for an on over one turn, then moves the cursor. It says how
// many turns it queued. to, name and agent are FireOns'. A full queue stops
// the check with no error and leaves the cursor, to try again later.
func (q *Queue) FireWatchOns(team string, read RecordReader, to func(session string) (DeliverTo, bool, error), name func(id string) string, agent AgentOf) (int, error) {
	if read == nil {
		return 0, nil
	}
	rows, err := q.db.Query(`SELECT id, event, session, maker, filter, limits, COALESCE(cursor, 0) FROM ons
		WHERE team = ? AND subject = '' AND filter IS NOT NULL AND off IS NULL ORDER BY created, id`, team)
	if err != nil {
		return 0, fmt.Errorf("queue: list ons: %w", err)
	}
	type live struct {
		id, event, session string
		maker              Maker
		filter             OnFilter
		limits             sql.NullString
		cursor             int64
	}
	var ons []live
	for rows.Next() {
		var l live
		var mk, fl string
		if err := rows.Scan(&l.id, &l.event, &l.session, &mk, &fl, &l.limits, &l.cursor); err == nil {
			err = json.Unmarshal([]byte(mk), &l.maker)
			if err == nil {
				err = json.Unmarshal([]byte(fl), &l.filter)
			}
			if err != nil {
				rows.Close()
				return 0, fmt.Errorf("queue: read on: %w", err)
			}
		} else {
			rows.Close()
			return 0, fmt.Errorf("queue: list ons: %w", err)
		}
		ons = append(ons, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, o := range ons {
		budget, err := limitsOf(o.limits)
		if err != nil {
			return n, err
		}
		evs, err := read(recordKindOf(o.event), o.cursor, recordPage)
		if err != nil {
			return n, err
		}
		for _, e := range evs {
			// A removal since the list was read wins, as in FireOns.
			var off sql.NullInt64
			if err := q.db.QueryRow(`SELECT off FROM ons WHERE id = ?`, o.id).Scan(&off); err != nil {
				return n, fmt.Errorf("queue: read on: %w", err)
			}
			if off.Valid {
				break
			}
			// A turn job of the watched session: deliver its reply. Any other
			// event (another session's turn, a turn this queue never ran)
			// only moves the cursor.
			did, why, fired := "", "", false
			j, err := q.JobByTurn(team, e.Turn)
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return n, err
			case j.Kind == KindTurn && string(j.Session) == o.filter.Session:
				fired = true
				did, why, err = q.deliverResult(team, o.id, o.session, o.maker.Turn, j, to, name, agent, budget)
				if errors.Is(err, ErrFull) {
					return n, nil // the queue is full: the cursor stays, try again later
				}
				if err != nil {
					return n, err
				}
				if why == "" {
					n++
				}
				if err := q.reportRefused(o.id, o.session, j, why); err != nil {
					return n, err // the cursor stays: the next check reports it again
				}
			}
			if err := q.tx(func(tx *sql.Tx) error {
				if fired {
					if _, err := tx.Exec(`INSERT INTO on_fires(on_id, key, job, error, created) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`,
						o.id, e.Turn, did, why, nanos(q.now())); err != nil {
						return fmt.Errorf("queue: write on fire: %w", err)
					}
				}
				if _, err := tx.Exec(`UPDATE ons SET cursor = ? WHERE id = ? AND COALESCE(cursor, 0) < ?`, e.Seq, o.id, e.Seq); err != nil {
					return fmt.Errorf("queue: move on cursor: %w", err)
				}
				return nil
			}); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}
