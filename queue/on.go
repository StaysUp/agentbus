package queue

// The on primitive: a standing instruction on control, "when this event
// happens, do this". It is a row (the ons table) plus the control loop's
// existing check (FireOns, called where Deliver was); nothing waits on
// control. B13 has one event and one action:
//
//	on turn.completed -> start a turn in a session with the subject turn's reply.
//
// This is what today's delivery already does (asked.go, Deliver); the on row
// generalises it so later events (apply, a timer, before-apply) are the same
// primitive with another event, not another mechanism.
//
// A turn.completed on is one-shot: it fires once, when its subject turn ends,
// and is then off. So it has a definite end, which is how "an on cannot set
// itself off forever" holds for B13 (there is no repeating on yet; a repeating
// one, C3, must carry an end or be ended by off). A fire is recorded in
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

// OnTurnCompleted is B13's only event: the subject turn has ended.
const OnTurnCompleted = "turn.completed"

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
// later, separately-authorized step can be lost — closing the two-step app's
// token-lifetime window found across three review rounds. The ask_agent and
// review_turn apps set it; POST /api/v0/ons stays for every other case.
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
}

// B15 (his call (a), 2026-11-19): a turn create MAY record its delivery on in
// the same transaction, when POST /api/v0/turns is given a delivery (see
// create and Delivery). That is how the ask_agent and review_turn apps deliver,
// in one atomic write. RegisterOn below is the separate, public path POST
// /api/v0/ons uses for every other case.

// RegisterOn stores an on and returns it. event must be known; subject (the
// turn watched) and session (where the action starts its turn) are required.
//
// Dedup is by idempotency_key ONLY, the SAME mechanism POST /api/v0/turns uses
// (his call, 2026-11-19): a non-empty key already used returns the on it made
// (same subject and session), so an app's registerOn step, run with a key
// derived from the tool call, is a no-op on a retry; a reused key with other
// fields is ErrKeyConflict. There is no one-on-per-subject rule — several ons
// may watch one turn — so a key is never left unbound by a subject collision
// (the round-3 finding). A keyless on is always a new registration.
func (q *Queue) RegisterOn(team, event, subject, session, key string, maker Maker) (On, error) {
	return q.RegisterOnLinked(team, event, subject, session, key, maker, nil, Limits{})
}

// RegisterOnLinked is RegisterOn for an on a turn token registers: asked is
// the token's request tree and turn (Root, By, BySession; Depth is not read),
// and the on counts as asked work under lim's per-turn and per-tree caps,
// checked in the insert's own transaction (countAsked). A cap reached is a
// *LimitError and stores nothing. A retry with a key already used returns its
// on without counting again. nil asked is RegisterOn: no cap.
func (q *Queue) RegisterOnLinked(team, event, subject, session, key string, maker Maker, asked *Chain, lim Limits) (On, error) {
	if event != OnTurnCompleted {
		return On{}, fmt.Errorf("%w: %q", ErrUnknownEvent, event)
	}
	if subject == "" || session == "" || (asked != nil && asked.By == "") {
		return On{}, ErrBadState
	}
	mk, err := json.Marshal(maker)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	var chain sql.NullString
	if asked != nil {
		c, err := json.Marshal(Chain{Root: asked.Root, By: asked.By, BySession: asked.BySession})
		if err != nil {
			return On{}, fmt.Errorf("queue: register on: %w", err)
		}
		chain = sql.NullString{String: string(c), Valid: true}
	}
	o := On{ID: NewID("on_"), Team: team, Event: event, Subject: subject, Session: session, Maker: maker, Created: nanos(q.now())}
	err = q.tx(func(tx *sql.Tx) error {
		// A key already used returns its on (same subject and session, so a
		// retry is a no-op) or is a conflict, the rule POST /api/v0/turns applies
		// to a key with other fields. Read before the caps, so a retry is never
		// refused for the on it made itself.
		if key != "" {
			existing, err := onByKey(tx, team, key)
			switch {
			case err == nil:
				if existing.Subject != subject || existing.Session != session {
					return ErrKeyConflict
				}
				o = existing
				return nil
			case !errors.Is(err, ErrNotFound):
				return err
			}
		}
		if asked != nil {
			if err := countAsked(tx, team, *asked, lim); err != nil {
				return err
			}
		}
		// The insert and the dedup are ONE atomic statement: ON CONFLICT on the
		// key index, not a read-then-insert. Two concurrent requests with the same
		// key then both land here, one inserts and the other does nothing (never a
		// uniqueness error, the round-4 finding). A keyless on never conflicts
		// (the index is partial on idem_key != '') and is always new.
		res, err := tx.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, idem_key, created, chain) VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(team, idem_key) WHERE idem_key != '' DO NOTHING`,
			o.ID, o.Team, o.Event, o.Subject, o.Session, string(mk), key, o.Created, chain)
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

// EndedTurns lists turn jobs that ended at or before atOrBefore, oldest end
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
		WHERE kind = ? AND ended IS NOT NULL AND ended <= ? AND (ended > ? OR (ended = ? AND id > ?))
		ORDER BY ended, id LIMIT ?`, KindTurn, nanos(atOrBefore), ended, ended, id, limit)
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

// onByKey reads the on registered with idempotency key, or ErrNotFound.
func onByKey(tx *sql.Tx, team, key string) (On, error) {
	o := On{Team: team}
	var mk string
	switch err := tx.QueryRow(`SELECT id, event, subject, session, maker, created FROM ons WHERE team = ? AND idem_key = ?`,
		team, key).Scan(&o.ID, &o.Event, &o.Subject, &o.Session, &mk, &o.Created); {
	case errors.Is(err, sql.ErrNoRows):
		return On{}, ErrNotFound
	case err != nil:
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	if err := json.Unmarshal([]byte(mk), &o.Maker); err != nil {
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	return o, nil
}

// GetOn reads an on by id, off or not; ErrNotFound if none.
func (q *Queue) GetOn(team, id string) (On, error) {
	o := On{ID: id, Team: team}
	var mk string
	switch err := q.db.QueryRow(`SELECT event, subject, session, maker, created FROM ons WHERE id = ? AND team = ?`,
		id, team).Scan(&o.Event, &o.Subject, &o.Session, &mk, &o.Created); {
	case errors.Is(err, sql.ErrNoRows):
		return On{}, ErrNotFound
	case err != nil:
		return On{}, fmt.Errorf("queue: read on: %w", err)
	}
	if err := json.Unmarshal([]byte(mk), &o.Maker); err != nil {
		return On{}, fmt.Errorf("queue: read on: %w", err)
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
	rows, err := q.db.Query(`SELECT id, event, subject, session, maker, created FROM ons
		WHERE team = ? AND off IS NULL ORDER BY created, id`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: list ons: %w", err)
	}
	defer rows.Close()
	var out []On
	for rows.Next() {
		o := On{Team: team}
		var mk string
		if err := rows.Scan(&o.ID, &o.Event, &o.Subject, &o.Session, &mk, &o.Created); err != nil {
			return nil, fmt.Errorf("queue: list ons: %w", err)
		}
		if err := json.Unmarshal([]byte(mk), &o.Maker); err != nil {
			return nil, fmt.Errorf("queue: list ons: %w", err)
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
	rows, err := q.db.Query(`SELECT o.id, o.subject, o.session, o.maker FROM ons o
		WHERE o.team = ? AND o.event = ? AND o.off IS NULL
		AND NOT EXISTS (SELECT 1 FROM on_fires f WHERE f.on_id = o.id AND f.key = o.subject)
		ORDER BY o.created, o.id`, team, OnTurnCompleted)
	if err != nil {
		return 0, fmt.Errorf("queue: list ons: %w", err)
	}
	type pending struct{ id, subject, session, maker string }
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.subject, &p.session, &p.maker); err != nil {
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
		j, err := q.JobByTurn(team, p.subject)
		if err != nil {
			return n, err
		}
		did, why, err := q.deliverResult(team, p.id, p.session, mk.Turn, j, to, name, agent)
		if errors.Is(err, ErrFull) {
			return n, nil // the queue is full: try again later, the on is still live
		}
		if err != nil {
			return n, err
		}
		if why == "" {
			n++
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
