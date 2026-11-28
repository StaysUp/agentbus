package queue

// The on primitive: a standing instruction on control, "when this event
// happens, do this". It is a row (the ons table) plus the control loop's
// existing check (FireOns, called where Deliver was); nothing waits on
// control. An on has an event and an action:
//
//	on turn.completed in a session -> start a turn in another session with
//	   each ended turn's reply (watchon.go): the public on, POST /api/v0/ons.
//	on turn.completed of one turn, or command.ended of one command -> the
//	   same, once (this file): control's own since 0.6.8, a command's delivery.
//	on <a kind on the record> -> queue a job (recordon.go), such as the
//	   built-in put-away of a workspace whose work was applied or discarded.
//
// This is what today's delivery already does (asked.go, Deliver); the on row
// generalises it so later events (apply, a timer, before-apply) are the same
// primitive with another event, not another mechanism.
//
// An on over one turn is one-shot: it fires once, when its subject turn ends,
// and is then off. An on over a session repeats until it is removed, and may
// not deliver in a circle (watchon.go). A fire is recorded in
// on_fires, keyed by the subject turn, so a second sweep or a control restart
// never fires it twice: the same safety the deliveries table gives delivery.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// OnTurnCompleted is the turn's event: the subject turn has ended. Its action
// is to deliver the turn's reply. The other events an on may watch are kinds
// on the record (RecordEvents, recordon.go), whose action is to queue a job.
const OnTurnCompleted = "turn.completed"

// OnCommandEnded is a command's event: the subject command (an exec job, by
// its c_ id) has ended. Its action is to deliver the command's result, its
// exit code and the end of its output, as a turn.completed on delivers a
// turn's reply. Only POST /api/v0/commands records one, in the job's own write.
const OnCommandEnded = "command.ended"

// Delivers says an on of event delivers a result to a session (a turn's or a
// command's), as opposed to one over a kind on the record, which queues a job.
func Delivers(event string) bool { return event == OnTurnCompleted || event == OnCommandEnded }

// ErrUnknownEvent: RegisterOn was given an event it does not know. Caught at
// registration so a typo cannot become an on that never fires.
var ErrUnknownEvent = errors.New("queue: unknown on event")

// Maker is who registered an on: their session and turn. Kept for the record,
// and, for a turn.completed on, the turn whose agent the delivered result runs
// on (the asking turn).
type Maker struct {
	Session string `json:"session"`
	Turn    string `json:"turn"`
}

// Delivery, set on a turn create, records a turn.completed on in the SAME
// transaction as the turn: when the turn completes, its reply is delivered to
// Session, run on Maker.Turn's agent. Start and deliver are then one write under
// the one authorization that starts the turn (his call (a), 2026-11-19), so no
// later, separately-authorized step can be lost. Since 0.6.8 only POST
// /api/v0/commands sets it (a command's result); a turn's delivery is an on
// over its session, registered before the turn (watchon.go).
type Delivery struct {
	Session string
	Maker   Maker
}

// deliveryKey namespaces a turn's idempotency key for the delivery on recorded
// atomically with it, so that on cannot collide with a standalone POST /ons that
// used the same key (found in review 2026-11-19): a caller's key can never hold
// "/", so "delivery/"+key is unreachable by one. The turn's own key still covers
// both — a retry dedups the turn and this on together — and a keyless turn gives
// a keyless (always-new) on.
func deliveryKey(turnKey string) string {
	if turnKey == "" {
		return ""
	}
	return "delivery/" + turnKey
}

// On is a registered standing instruction.
type On struct {
	ID      string `json:"id"`
	Team    string `json:"team"`
	Event   string `json:"event"`
	Subject string `json:"subject"`
	Session string `json:"session"`
	Maker   Maker  `json:"maker"`
	Created int64  `json:"created"`
	// Action is what the on does when it fires. The zero Action delivers the
	// subject turn's reply to Session (turn.completed).
	Action Action `json:"action"`
	// Filter, on an on over a session (watchon.go), names the session whose
	// turns it watches; Subject is then empty. Zero on an on over one turn.
	Filter OnFilter `json:"filter"`
}

// RegisterOn stores a one-shot on over one turn (or one command) and returns
// it. It is control's own since 0.6.8: the public API registers ons over a
// session (RegisterWatchOn, watchon.go), and a command's on is recorded with
// its job (create's Delivery). event must be known; subject (the turn watched)
// and session (where the action starts its turn) are required.
//
// Dedup is by idempotency_key ONLY, the SAME mechanism POST /api/v0/turns uses
// (his call, 2026-11-19): a non-empty key already used returns the on it made
// (same subject and session); a reused key with other fields is
// ErrKeyConflict. A keyless on is always a new registration. An on counts as
// no asked work (0.6.8): it delivers a result, it is not new work.
func (q *Queue) RegisterOn(team, event, subject, session, key string, maker Maker) (On, error) {
	if !Delivers(event) {
		return On{}, fmt.Errorf("%w: %q", ErrUnknownEvent, event)
	}
	if subject == "" || session == "" {
		return On{}, ErrBadState
	}
	mk, err := json.Marshal(maker)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	o := On{ID: NewID("on_"), Team: team, Event: event, Subject: subject, Session: session, Maker: maker, Created: nanos(q.now())}
	err = q.tx(func(tx *sql.Tx) error {
		// The insert and the dedup are ONE atomic statement: ON CONFLICT on the
		// key index, not a read-then-insert. A keyless on never conflicts (the
		// index is partial on idem_key != '') and is always new.
		res, err := tx.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, idem_key, created) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(team, idem_key) WHERE idem_key != '' DO NOTHING`,
			o.ID, o.Team, o.Event, o.Subject, o.Session, string(mk), key, o.Created)
		if err != nil {
			return fmt.Errorf("queue: register on: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil // inserted: a new on, or a keyless one
		}
		existing, err := onByKey(tx, team, key)
		if err != nil {
			return err
		}
		if existing.Subject != subject || existing.Session != session {
			return ErrKeyConflict
		}
		o = existing
		return nil
	})
	if err != nil {
		return On{}, err
	}
	return o, nil
}

// EndedTurns lists turn and exec jobs that ended at or before atOrBefore, oldest end
// first, from after (nil to start at the first), up to limit. The sweep's
// reconciler (jobs.RecordTurnEnds) walks them to record each turn's end in the
// record. atOrBefore is how the reconciler gives the finisher a head start: a
// turn just ended is left to the finisher's own prompt turn.ended, and the
// reconciler only backfills ends older than that window (and the ones the
// finisher never writes: a lost runner, a cancel). The (ended, id) cursor is
// stable because ended is set once, to a monotonic time, when a job ends.
func (q *Queue) EndedTurns(after *Job, atOrBefore time.Time, limit int) ([]Job, error) {
	var ended int64
	var id string
	if after != nil && after.Ended != nil {
		ended, id = nanos(*after.Ended), after.ID
	}
	rows, err := q.db.Query(`SELECT `+jobCols+` FROM jobs
		WHERE kind IN (?, ?) AND ended IS NOT NULL AND ended <= ? AND (ended > ? OR (ended = ? AND id > ?))
		ORDER BY ended, id LIMIT ?`, KindTurn, KindExec, nanos(atOrBefore), ended, ended, id, limit)
	if err != nil {
		return nil, fmt.Errorf("queue: list ended turns: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list ended turns: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// onByKey reads the on idempotency key names, or ErrNotFound: the on it made,
// or the on it was bound to when it returned one already live (on_keys).
func onByKey(tx *sql.Tx, team, key string) (On, error) {
	o := On{Team: team}
	var mk string
	var act string
	var filter string
	switch err := tx.QueryRow(`SELECT id, event, subject, session, maker, created, COALESCE(action, ''), COALESCE(filter, '') FROM ons
		WHERE team = ?1 AND (idem_key = ?2 OR id = (SELECT on_id FROM on_keys WHERE team = ?1 AND key = ?2)) ORDER BY idem_key = ?2 DESC LIMIT 1`,
		team, key).Scan(&o.ID, &o.Event, &o.Subject, &o.Session, &mk, &o.Created, &act, &filter); {
	case errors.Is(err, sql.ErrNoRows):
		return On{}, ErrNotFound
	case err != nil:
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	if err := readOn(&o, mk, act, filter); err != nil {
		return On{}, err
	}
	return o, nil
}

// readOn fills o's maker, action and filter from their stored JSON.
func readOn(o *On, maker, action, filter string) error {
	if filter != "" {
		if err := json.Unmarshal([]byte(filter), &o.Filter); err != nil {
			return fmt.Errorf("queue: read on: %w", err)
		}
	}
	if err := json.Unmarshal([]byte(maker), &o.Maker); err != nil {
		return fmt.Errorf("queue: read on: %w", err)
	}
	if action != "" {
		if err := json.Unmarshal([]byte(action), &o.Action); err != nil {
			return fmt.Errorf("queue: read on: %w", err)
		}
	}
	return nil
}

// GetOn reads an on by id, off or not; ErrNotFound if none.
func (q *Queue) GetOn(team, id string) (On, error) {
	o := On{ID: id, Team: team}
	var mk, act, filter string
	switch err := q.db.QueryRow(`SELECT event, subject, session, maker, created, COALESCE(action, ''), COALESCE(filter, '') FROM ons WHERE id = ? AND team = ?`,
		id, team).Scan(&o.Event, &o.Subject, &o.Session, &mk, &o.Created, &act, &filter); {
	case errors.Is(err, sql.ErrNoRows):
		return On{}, ErrNotFound
	case err != nil:
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	if err := readOn(&o, mk, act, filter); err != nil {
		return On{}, err
	}
	return o, nil
}

// RemoveOn sets an on off: FireOns ignores it from then on. Removing one that
// is already off is a no-op; an unknown id is ErrNotFound.
//
// Remove is best-effort against a fire already in flight, the standard shape of
// cancellation: it stops every future fire, but an on the sweep is already
// delivering this tick may still deliver its reply once (never twice: the
// delivery is idempotent on the result's key, ResultKey). FireOns re-reads off
// right before it delivers, so a remove that finishes before the sweep reaches
// the on wins; the residual window is between that read and the delivery. This
// is a design choice, not a defect: closing it fully would put the off check
// inside the delivery's own transaction and give up the crash-safe re-delivery
// the sweep relies on (deliver-first, deduped by on_fires, survives a restart).
func (q *Queue) RemoveOn(team, id string) error {
	res, err := q.db.Exec(`UPDATE ons SET off = ? WHERE id = ? AND team = ? AND off IS NULL`, nanos(q.now()), id, team)
	if err != nil {
		return fmt.Errorf("queue: remove on: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var one int
	switch err := q.db.QueryRow(`SELECT 1 FROM ons WHERE id = ? AND team = ?`, id, team).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("queue: remove on: %w", err)
	default:
		return nil // already off
	}
}

// ListOns returns the team's live (not off) ons, oldest first.
func (q *Queue) ListOns(team string) ([]On, error) {
	rows, err := q.db.Query(`SELECT id, event, subject, session, maker, created, COALESCE(action, ''), COALESCE(filter, '') FROM ons
		WHERE team = ? AND off IS NULL ORDER BY created, id`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: list ons: %w", err)
	}
	defer rows.Close()
	var out []On
	for rows.Next() {
		o := On{Team: team}
		var mk, act, filter string
		if err := rows.Scan(&o.ID, &o.Event, &o.Subject, &o.Session, &mk, &o.Created, &act, &filter); err != nil {
			return nil, fmt.Errorf("queue: list ons: %w", err)
		}
		if err := readOn(&o, mk, act, filter); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FireOns is the control loop's check for ons that have matured: for each live
// turn.completed on that has not fired and whose subject turn has ended, it
// queues the delivery turn (exactly as Deliver did), records the fire once,
// and sets the one-shot off. It says how many delivery turns it queued. name
// and agent are as Deliver's: name gives a saved agent's name for the prompt,
// agent the agent a delivered result runs on. A full queue stops the sweep
// with no error, to try again later.
//
// Whether the subject ended is read from the RECORD, through ended, not from
// the scheduler's jobs table (C0, DESIGN_C0 §1: the one real boundary break).
// ended reports whether the record holds a turn.ended for the subject turn;
// control writes one for every ended turn job (the finisher and the sweep's
// reconciler, jobs/ended.go). The scan stays over live unfired ons rather than
// a cursor over turn.ended events, so an on registered AFTER its subject ended
// still fires -- a cursor past that event would not, and that would change
// deliveries. on_fires remains the idempotency key: a replay or a second tick
// fires each (on, subject) once. Reading the subject's job for its reply
// (deliverResult) stays: that is the delivered content, not the ended trigger.
// DeliverTo is where a delivery runs: the receiving session's workspace, from
// the record, so a delivered turn carries the same folder, machine and mode as
// any turn of that session. The caller (control) resolves it from the store; it
// is false for a session the record places in no workspace.
type DeliverTo struct {
	Workspace, FolderRef, Machine string
	// Folders, in place of FolderRef, for a workspace with several folders.
	Folders []workspace.Place
	Isolate bool
}

func (q *Queue) FireOns(team string, ended func(turn string) (bool, error), to func(session string) (DeliverTo, bool, error), name func(id string) string, agent AgentOf) (int, error) {
	if ended == nil {
		return 0, nil // nothing can be known to have ended
	}
	// All live unfired ons, not a capped page: the ended test is per on, in the
	// record, so a LIMIT here could fill with ons whose subject is still running
	// and starve a newer on whose subject has ended (review P2, 2026-11-19). The
	// set is small — a delivery on goes off as soon as it fires — and each ended
	// check is an indexed lookup.
	rows, err := q.db.Query(`SELECT o.id, o.subject, o.session, o.maker, o.limits FROM ons o
		WHERE o.team = ? AND o.event IN (?, ?) AND o.subject != '' AND o.off IS NULL
		AND NOT EXISTS (SELECT 1 FROM on_fires f WHERE f.on_id = o.id AND f.key = o.subject)
		ORDER BY o.created, o.id`, team, OnTurnCompleted, OnCommandEnded)
	if err != nil {
		return 0, fmt.Errorf("queue: list ons: %w", err)
	}
	type pending struct {
		id, subject, session, maker string
		limits                      sql.NullString
	}
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.subject, &p.session, &p.maker, &p.limits); err != nil {
			rows.Close()
			return 0, fmt.Errorf("queue: list ons: %w", err)
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		var mk Maker
		if err := json.Unmarshal([]byte(p.maker), &mk); err != nil {
			return n, fmt.Errorf("queue: read on maker: %w", err)
		}
		// Re-read the on: a removal since the sweep listed it should win, so a
		// removed on does not deliver. A removal in the small window between
		// this read and the delivery is the residual race; the delivery is
		// idempotent (ResultKey), so at worst one reply is queued for an on
		// removed at that instant, never more.
		var off sql.NullInt64
		if err := q.db.QueryRow(`SELECT off FROM ons WHERE id = ? AND team = ?`, p.id, team).Scan(&off); err != nil {
			return n, fmt.Errorf("queue: read on: %w", err)
		}
		if off.Valid {
			continue
		}
		// The subject must have ended, read from the record (not jobs.db). An
		// on whose subject has not ended yet is left for a later tick.
		switch done, err := ended(p.subject); {
		case err != nil:
			return n, err
		case !done:
			continue
		}
		// The subject is a turn's or a command's job; ended has read its end
		// from the record either way.
		j, err := q.jobBySubject(team, p.subject, "1")
		if err != nil {
			return n, err
		}
		budget, err := limitsOf(p.limits)
		if err != nil {
			return n, err
		}
		did, why, err := q.deliverResult(team, p.id, p.session, mk.Turn, j, to, name, agent, budget)
		if errors.Is(err, ErrFull) {
			return n, nil // the queue is full: try again later, the on is still live
		}
		if err != nil {
			return n, err
		}
		if why == "" {
			n++
		}
		if err := q.reportRefused(p.id, p.session, j, why); err != nil {
			return n, err // the on stays live: the next check reports it again
		}
		if _, err := q.db.Exec(`INSERT INTO on_fires(on_id, key, job, error, created) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`,
			p.id, p.subject, did, why, nanos(q.now())); err != nil {
			return n, fmt.Errorf("queue: write on fire: %w", err)
		}
		if _, err := q.db.Exec(`UPDATE ons SET off = ? WHERE id = ? AND team = ? AND off IS NULL`,
			nanos(q.now()), p.id, team); err != nil {
			return n, fmt.Errorf("queue: set on off: %w", err)
		}
	}
	return n, nil
}

// An asked session is not closed here. Closing it (which retires its workspace
// copy) safely needs more than a delivered turn's change count: the session
// must be an asked one, every turn ended, with no unapplied changes anywhere
// (an unreadable diff counts as changes) and no reply still owed to it.
// Deciding that from one turn in the delivery sweep closed sessions that had
// changes or pending replies, so it is a separate "safe to retire" check, a
// follow-up with those cases as its tests. Until then nothing auto-closes.
