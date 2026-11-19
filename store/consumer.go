package store

// The event log as a thing consumers read, not just a thing the service
// writes. A reactor (deliveries, cleanups, later timers and gates) is a named
// consumer that reads the record from its own durable position and acts, in
// place of the once-a-second sweep's ad-hoc table checks (C0, DESIGN_C0).
//
// The interface is four methods over the store we already have:
//
//	Append            write an event, get it back with its seq      (store.go)
//	After             read events after a cursor, oldest first      (store.go)
//	Wait              a signal that there is something past a cursor (below)
//	Cursor / Ack      a consumer's durable position in the record   (below)
//
// Append, After and Wait exist already; the one new piece is a per-consumer
// cursor, so each reactor keeps its place in the record rather than its own
// bespoke dedup table. The seam is deliberate: AgentBus code only ever calls
// these, so a later move to Kafka or an embedded NATS is one adapter that
// satisfies them, with the event shape and the consumer code unchanged.
//
// The processing model a single cursor relies on, stated so it is not
// rediscovered (the references' sharp point, DESIGN_C0 §6): a consumer reads
// events in seq order, one at a time, and advances its cursor only after its
// action commits (ack-after-effect). Then at-least-once is enough and safe: a
// crash between the action and the ack re-runs the one event, and every
// reactor's action is idempotent on a key already in the record (FireOns on
// on_fires, delivery on ResultKey). Ack is idempotent and monotonic, so a
// replayed lower cursor never moves a consumer backward.
//
// Single active consumer per name. Today control is one process with one
// goroutine per reactor, so this holds by construction. Before control is ever
// more than one process, consumer_cursors needs a lease (owner, expires) taken
// and renewed while a reader processes and taken over after a grace period, or
// two readers double-process; watermill-sqlite takes exactly this time lease
// because SQLite cannot SELECT ... FOR UPDATE. That column is deliberately not
// added yet (one process now; no options for later).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/staysup/agentbus/event"
)

// consumerSchema adds the one table the interface needs beyond the record.
// A parked row is an event a consumer gave up on, kept so one undeliverable
// event never blocks the rest (the poison path, below).
const consumerSchema = `
CREATE TABLE IF NOT EXISTS consumer_cursors (
	consumer TEXT PRIMARY KEY,
	store    TEXT    NOT NULL,
	seq      INTEGER NOT NULL,
	updated  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS parked_events (
	consumer TEXT    NOT NULL,
	seq      INTEGER NOT NULL,
	kind     TEXT    NOT NULL,
	attempts INTEGER NOT NULL,
	reason   TEXT    NOT NULL,
	parked   INTEGER NOT NULL,
	PRIMARY KEY (consumer, seq)
);
`

// EventLog is the small interface AgentBus code talks to, so the record's
// backend (one SQLite file today) can change without touching the consumers.
// *Store is the embedded adapter.
type EventLog interface {
	// Append writes e and returns it with its seq. stored is false, with no
	// error, when an idempotent write found the event already there.
	Append(e event.Event) (out event.Event, stored bool, err error)
	// After returns up to limit events with seq greater than after, oldest
	// first, matching f.
	After(after int64, f Filter, limit int) ([]Record, error)
	// Wait returns a channel that signals when there may be events past a
	// consumer's cursor. A signal means "read past your cursor", not "here is
	// one event". It carries a poll fallback, so a missed in-memory signal
	// (Notify coalesces) still wakes the reader.
	Wait(ctx context.Context) <-chan struct{}
	// Cursor is the consumer's durable position: the highest seq it has acked,
	// or 0 for a consumer that has never acked.
	Cursor(consumer string) (int64, error)
	// Ack moves the consumer's cursor to seq. It is idempotent and monotonic.
	Ack(consumer string, seq int64) error
}

var _ EventLog = (*Store)(nil)

// WaitPoll is how often Wait signals even without a Notify, so a reader never
// sleeps forever on a signal that coalesced away or a write it registered
// after. The references keep such a poll fallback behind a real signal rather
// than trusting the signal alone.
const WaitPoll = time.Second

// Wait implements EventLog. The returned channel is closed when ctx ends.
func (s *Store) Wait(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)
	ch, cancel := s.Notify()
	go func() {
		defer cancel()
		defer close(out)
		t := time.NewTicker(WaitPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
			case <-t.C:
			}
			select {
			case out <- struct{}{}:
			case <-ctx.Done():
				return
			default: // a signal is already pending; the reader reads past its cursor
			}
		}
	}()
	return out
}

// Cursor implements EventLog. A cursor stored against another store file is an
// error: a seq from one file means nothing in another.
func (s *Store) Cursor(consumer string) (int64, error) {
	if consumer == "" {
		return 0, errors.New("store: a cursor needs a consumer name")
	}
	var seq int64
	var storeID string
	err := s.r.QueryRow(`SELECT seq, store FROM consumer_cursors WHERE consumer = ?`, consumer).Scan(&seq, &storeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read cursor: %w", err)
	}
	if storeID != s.id {
		return 0, fmt.Errorf("store: cursor for %q is against store %s, not this store %s", consumer, storeID, s.id)
	}
	return seq, nil
}

// Ack implements EventLog: it moves consumer's cursor to seq, never backward
// (MAX), so a replay or a reordered ack cannot lose ground. Writing the same
// seq again is a no-op.
func (s *Store) Ack(consumer string, seq int64) error {
	if consumer == "" {
		return errors.New("store: an ack needs a consumer name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Exec(`INSERT INTO consumer_cursors(consumer, store, seq, updated) VALUES(?,?,?,?)
		ON CONFLICT(consumer) DO UPDATE SET seq = MAX(consumer_cursors.seq, excluded.seq), store = excluded.store, updated = excluded.updated`,
		consumer, s.id, seq, time.Now().UnixNano()); err != nil {
		return fmt.Errorf("store: ack: %w", err)
	}
	return nil
}

// Parked is one event a consumer gave up on: its seq, kind, how many tries it
// took, why, and when it was parked.
type Parked struct {
	Seq      int64
	Kind     event.Kind
	Attempts int
	Reason   string
	At       time.Time
}

// park records that consumer gave up on the event at seq after attempts tries,
// so the consumer can advance past it. Writing it again (same consumer, seq)
// keeps the latest reason and count. The caller acks past seq after this.
func (s *Store) park(consumer string, rec Record, attempts int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Exec(`INSERT INTO parked_events(consumer, seq, kind, attempts, reason, parked) VALUES(?,?,?,?,?,?)
		ON CONFLICT(consumer, seq) DO UPDATE SET kind = excluded.kind, attempts = excluded.attempts, reason = excluded.reason, parked = excluded.parked`,
		consumer, rec.Seq, string(rec.Kind), attempts, reason, time.Now().UnixNano()); err != nil {
		return fmt.Errorf("store: park: %w", err)
	}
	return nil
}

// ParkedEvents lists the events consumer has parked, oldest first. A reader
// (a person, a later cleanup) can see what a reactor could not process.
func (s *Store) ParkedEvents(consumer string) ([]Parked, error) {
	rows, err := s.r.Query(`SELECT seq, kind, attempts, reason, parked FROM parked_events WHERE consumer = ? ORDER BY seq`, consumer)
	if err != nil {
		return nil, fmt.Errorf("store: read parked: %w", err)
	}
	defer rows.Close()
	var out []Parked
	for rows.Next() {
		var p Parked
		var at int64
		if err := rows.Scan(&p.Seq, &p.Kind, &p.Attempts, &p.Reason, &at); err != nil {
			return nil, fmt.Errorf("store: read parked: %w", err)
		}
		p.At = time.Unix(0, at).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// Consumer drives one named reactor over the log: it reads from the consumer's
// cursor, hands each event to a handler in seq order, and advances the cursor
// after each — the ack-after-effect model this package's doc states. A handler
// that keeps failing a given event is retried up to MaxTries, then that one
// event is parked and the cursor advances past it, so one undeliverable event
// never blocks the rest (the poison path the references require).
type Consumer struct {
	Log      *Store
	Name     string
	Filter   Filter
	Batch    int // events read per round; 0 means DefaultBatch
	MaxTries int // tries before an event is parked; 0 means DefaultMaxTries

	// tries counts attempts per seq within this process. It resets on restart,
	// so a deterministically-failing event gets MaxTries more tries after a
	// restart before it is parked again; a persistent redelivery count (the
	// references keep one) is the hardening, not needed while the park path
	// already stops the block.
	tries map[int64]int
}

// Defaults for a Consumer left zero.
const (
	DefaultBatch    = 100
	DefaultMaxTries = 5
)

// NewConsumer makes a consumer named name that reads events matching f.
func NewConsumer(log *Store, name string, f Filter) *Consumer {
	return &Consumer{Log: log, Name: name, Filter: f, Batch: DefaultBatch, MaxTries: DefaultMaxTries, tries: map[int64]int{}}
}

// Drain processes every event available now, in seq order, one at a time. It
// returns how many events the handler accepted (handled or parked and advanced
// past). It stops at the first event still failing under MaxTries, leaving the
// cursor on it, so the next Drain retries that one event first. A full-queue or
// transient handler error is the handler's to signal by returning an error;
// Drain treats any non-nil error as "try again", and only parks after MaxTries.
func (c *Consumer) Drain(handle func(Record) error) (int, error) {
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.MaxTries <= 0 {
		c.MaxTries = DefaultMaxTries
	}
	if c.tries == nil {
		c.tries = map[int64]int{}
	}
	done := 0
	for {
		cur, err := c.Log.Cursor(c.Name)
		if err != nil {
			return done, err
		}
		recs, err := c.Log.After(cur, c.Filter, c.Batch)
		if err != nil {
			return done, err
		}
		if len(recs) == 0 {
			return done, nil
		}
		for _, r := range recs {
			if herr := handle(r); herr != nil {
				c.tries[r.Seq]++
				if c.tries[r.Seq] < c.MaxTries {
					// Leave the cursor on this event: ack-after-effect means a
					// failed effect does not advance. Retry it next Drain.
					return done, nil
				}
				if perr := c.Log.park(c.Name, r, c.tries[r.Seq], herr.Error()); perr != nil {
					return done, perr
				}
				delete(c.tries, r.Seq)
				// fall through: advance past the parked event
			} else {
				delete(c.tries, r.Seq)
			}
			if err := c.Log.Ack(c.Name, r.Seq); err != nil {
				return done, err
			}
			done++
		}
		if len(recs) < c.Batch {
			return done, nil
		}
	}
}
