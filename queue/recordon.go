package queue

// An on over a kind on the record: "when an event of this kind is recorded,
// queue this job". It is the second shape of the on primitive (on.go), beside
// turn.completed's "deliver the reply". It is a row in the ons table with an
// action and a cursor: the seq of the last record event it has read. The
// control loop's existing tick reads the record past the cursor (FireRecordOns)
// and, for each event of the on's kind, queues the action's job in the event's
// workspace, once: the job's idempotency key is the on and the event, and the
// fire is kept in on_fires, so a second tick or a restart queues nothing more.
// Nothing waits and nothing is held in memory: the cursor and the fires are
// rows, and the job says why it was queued (Links.Because).
//
// For now the one action here is queuing a put-away (putaway.go), and these
// ons are control's own built-in ones (RegisterRecordOn). The ons POST
// /api/v0/ons registers read the record the same way and deliver a result
// (watchon.go).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/staysup/agentbus/ids"
)

// RecordEvents are the kinds on the record an on may watch: events that name
// the workspace they happened in, so the job the on queues has a workspace.
var RecordEvents = []string{"workspace.applied", "workspace.discarded"}

// QueueKinds are the job kinds an on's action may queue.
var QueueKinds = []string{KindPutAway}

// Action is what an on does when it fires. The zero Action delivers the subject
// turn's reply (a turn.completed on). Queue names the kind of job queued in the
// workspace of the record event that fired it.
type Action struct {
	Queue string `json:"queue,omitempty"`
}

// RecordEvent is one record event an on over the record reads: its id, its seq
// in the record, when it was recorded, and the workspace and turn it names.
type RecordEvent struct {
	ID        string
	Seq       int64
	Time      time.Time
	Workspace string
	Turn      string
}

// RecordReader reads the record's events of kind with seq after after, oldest
// first, at most limit.
type RecordReader func(kind string, after int64, limit int) ([]RecordEvent, error)

// RegisterRecordOn stores an on over the record's events of kind whose action
// is to queue a job, and returns it. key makes it once: a key already used
// returns the on it made, with its cursor as it stands. from is the cursor a
// new on starts at: the record's seq it reads after.
func (q *Queue) RegisterRecordOn(team, kind string, action Action, key string, from int64) (On, error) {
	if !slices.Contains(RecordEvents, kind) {
		return On{}, fmt.Errorf("%w: %q", ErrUnknownEvent, kind)
	}
	if !slices.Contains(QueueKinds, action.Queue) || key == "" {
		return On{}, ErrBadState
	}
	act, err := json.Marshal(action)
	if err != nil {
		return On{}, fmt.Errorf("queue: register on: %w", err)
	}
	o := On{ID: NewID("on_"), Team: team, Event: kind, Maker: Maker{}, Created: nanos(q.now()), Action: action}
	err = q.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, idem_key, created, action, cursor) VALUES(?,?,?,'','','{}',?,?,?,?)
			ON CONFLICT(team, idem_key) WHERE idem_key != '' DO NOTHING`, o.ID, team, kind, key, o.Created, string(act), from); err != nil {
			return fmt.Errorf("queue: register on: %w", err)
		}
		existing, err := onByKey(tx, team, key)
		if err != nil {
			return err
		}
		if existing.Event != kind || existing.Action != action {
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

// recordPage is how many record events one on reads per tick.
const recordPage = 200

// FireRecordOns is the control loop's check for ons over the record: for each
// live one, it reads the record's events of its kind past its cursor and does
// its action for each, then moves the cursor. It says how many jobs it queued.
//
// where finds a workspace's place from the record: its machine and whether it
// has a copy of its own (Isolate). A workspace it does not find, or one with no
// copy, gets no job; the fire is kept with why. after is how long a job of kind
// waits before a runner may take it, from the event's time; false is "off",
// and the fire is kept with why, so turning it on later does not reach back.
// A full queue stops the check with no error and moves no cursor, to try again
// later.
func (q *Queue) FireRecordOns(team string, read RecordReader, where func(ws string) (DeliverTo, bool, error), after func(kind string) (time.Duration, bool)) (int, error) {
	if read == nil {
		return 0, nil
	}
	rows, err := q.db.Query(`SELECT id, event, COALESCE(action, ''), COALESCE(cursor, 0) FROM ons
		WHERE team = ? AND event NOT IN (?, ?) AND off IS NULL ORDER BY created, id`, team, OnTurnCompleted, OnCommandEnded)
	if err != nil {
		return 0, fmt.Errorf("queue: list ons: %w", err)
	}
	type live struct {
		id, kind string
		action   Action
		cursor   int64
	}
	var ons []live
	for rows.Next() {
		var l live
		var act string
		if err := rows.Scan(&l.id, &l.kind, &act, &l.cursor); err != nil {
			rows.Close()
			return 0, fmt.Errorf("queue: list ons: %w", err)
		}
		if act != "" {
			if err := json.Unmarshal([]byte(act), &l.action); err != nil {
				rows.Close()
				return 0, fmt.Errorf("queue: read on: %w", err)
			}
		}
		ons = append(ons, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, o := range ons {
		evs, err := read(o.kind, o.cursor, recordPage)
		if err != nil {
			return n, err
		}
		for _, e := range evs {
			job, why, err := q.fireRecordOn(team, o.id, o.action, e, where, after)
			if errors.Is(err, ErrFull) {
				return n, nil // the queue is full: the cursor stays, try again later
			}
			if err != nil {
				return n, err
			}
			if why == "" {
				n++
			}
			if err := q.tx(func(tx *sql.Tx) error {
				if _, err := tx.Exec(`INSERT INTO on_fires(on_id, key, job, error, created) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`,
					o.id, e.ID, job, why, nanos(q.now())); err != nil {
					return fmt.Errorf("queue: write on fire: %w", err)
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

// fireRecordOn does on's action for record event e: the job it queued, or why
// it queued none.
func (q *Queue) fireRecordOn(team, on string, action Action, e RecordEvent, where func(ws string) (DeliverTo, bool, error), after func(kind string) (time.Duration, bool)) (job, why string, err error) {
	if action.Queue != KindPutAway {
		return "", "this on's action is not known to this build", nil
	}
	wait, ok := after(action.Queue)
	if !ok {
		return "", "putting copies away is off in the settings", nil
	}
	if e.Workspace == "" {
		return "", "the event names no workspace", nil
	}
	to, found, err := where(e.Workspace)
	if err != nil {
		return "", "", err
	}
	if !found || !to.Isolate {
		return "", "the workspace has no copy of its own", nil
	}
	due := e.Time.Add(wait)
	spec := Spec{Workspace: ids.WorkspaceID(to.Workspace), Machine: to.Machine}
	links := Links{Because: &Because{On: on, Event: e.ID, Seq: e.Seq}}
	// The key is the on and the event, so the job is queued once even when the
	// fire's own row was not written (a stop between the two). A '/' keeps it
	// apart from every key a caller can send (checkIdempotencyKey).
	j, _, err := q.createAt(team, KindPutAway, "on/"+on+"/"+e.ID, spec, links, nil, "", nil, &due)
	var removed *RunnerRemovedError
	switch {
	case errors.Is(err, ErrSessionClosed), errors.As(err, &removed):
		return "", "the workspace's copy is on a runner that takes no more work", nil
	case err != nil:
		return "", "", err
	}
	return j.ID, "", nil
}
