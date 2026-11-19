package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// A fresh consumer starts at 0, Ack moves it forward, and Ack never moves it
// backward (monotonic) or is lost on a repeat (idempotent).
func TestCursorAndAckMonotonic(t *testing.T) {
	s := newStore(t)
	if cur, err := s.Cursor("c1"); err != nil || cur != 0 {
		t.Fatalf("fresh cursor = %d, %v; want 0, nil", cur, err)
	}
	if err := s.Ack("c1", 5); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Cursor("c1"); cur != 5 {
		t.Fatalf("cursor after ack 5 = %d, want 5", cur)
	}
	// A lower ack (a replay) does not move it back.
	if err := s.Ack("c1", 3); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Cursor("c1"); cur != 5 {
		t.Fatalf("cursor after ack 3 = %d, want 5 (monotonic)", cur)
	}
	// The same ack again is a no-op.
	if err := s.Ack("c1", 5); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Cursor("c1"); cur != 5 {
		t.Fatalf("cursor after repeat ack = %d, want 5", cur)
	}
	// Two consumers keep independent positions.
	if cur, _ := s.Cursor("c2"); cur != 0 {
		t.Fatalf("second consumer cursor = %d, want 0", cur)
	}
}

// A cursor persists across a reopen (a control restart): the consumer picks up
// where it acked and delivers each event once, never re-processing acked ones.
func TestConsumerDrainInOrderAndReplayOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := open(t, path)
	for i := 0; i < 4; i++ {
		mustAppend(t, s, ev(event.TurnCompleted, "a"))
	}
	var seen []int64
	c := NewConsumer(s, "fire", Filter{})
	// A handler that "crashes" (returns, leaving work) after two events: the
	// second event's effect committed and was acked, the third was not.
	n, err := c.Drain(func(r Record) error {
		if len(seen) == 2 {
			return errors.New("crash")
		}
		seen = append(seen, r.Seq)
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("first drain: n=%d err=%v; want 2, nil", n, err)
	}
	// The failing event is retried; it keeps failing until MaxTries, then
	// parks. But here we instead reopen the store, as a restart would, with a
	// handler that now succeeds for everything: it must resume at seq 3, not
	// re-run 1 and 2.
	s.Close()
	s2 := open(t, path)
	seen = nil
	c2 := NewConsumer(s2, "fire", Filter{})
	n, err = c2.Drain(func(r Record) error {
		seen = append(seen, r.Seq)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{3, 4}; !equal(seen, want) {
		t.Fatalf("after restart saw %v, want %v (resume past the ack, each once)", seen, want)
	}
	if n != 2 {
		t.Fatalf("drain after restart n=%d, want 2", n)
	}
	if cur, _ := s2.Cursor("fire"); cur != 4 {
		t.Fatalf("cursor after draining all = %d, want 4", cur)
	}
}

// One event a handler can never process is parked after MaxTries and the cursor
// advances past it, so the events after it are still delivered.
func TestConsumerParksPoisonAndContinues(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, ev(event.TurnCompleted, "a")) // seq 1 ok
	mustAppend(t, s, ev(event.TurnCompleted, "b")) // seq 2 poison
	mustAppend(t, s, ev(event.TurnCompleted, "c")) // seq 3 ok
	c := NewConsumer(s, "fire", Filter{})
	c.MaxTries = 3
	var ok []int64
	drain := func() (int, error) {
		return c.Drain(func(r Record) error {
			if r.Seq == 2 {
				return errors.New("undeliverable")
			}
			ok = append(ok, r.Seq)
			return nil
		})
	}
	// Drain 1 acks seq 1 (cursor past it) and stops on seq 2 (try 1 of 3).
	if n, err := drain(); err != nil || n != 1 {
		t.Fatalf("drain 1: n=%d err=%v; want 1 (seq 1), nil", n, err)
	}
	// Drains that follow resume at seq 2 and keep stopping on it: nothing new
	// is accepted until it parks.
	for i := 2; i < c.MaxTries; i++ {
		if n, err := drain(); err != nil || n != 0 {
			t.Fatalf("drain %d: n=%d err=%v; want 0 (still stuck on seq 2), nil", i, n, err)
		}
	}
	// The try that reaches MaxTries parks seq 2, advances past it, and goes on
	// to seq 3: two events accepted (the parked one and seq 3).
	if n, err := drain(); err != nil || n != 2 {
		t.Fatalf("parking drain: n=%d err=%v; want 2 (park seq 2, deliver seq 3), nil", n, err)
	}
	if want := []int64{1, 3}; !equal(ok, want) {
		t.Fatalf("delivered %v, want %v (2 parked, 3 still delivered)", ok, want)
	}
	parked, err := s.ParkedEvents("fire")
	if err != nil {
		t.Fatal(err)
	}
	if len(parked) != 1 || parked[0].Seq != 2 || parked[0].Attempts != 3 {
		t.Fatalf("parked = %+v; want one, seq 2, attempts 3", parked)
	}
	if cur, _ := s.Cursor("fire"); cur != 3 {
		t.Fatalf("cursor = %d, want 3 (past the parked event)", cur)
	}
}

// Wait signals after an append, and its channel closes when its context ends.
func TestWaitSignalsAndCloses(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := s.Wait(ctx)
	mustAppend(t, s, ev(event.TurnCompleted, "a"))
	select {
	case _, ok := <-w:
		if !ok {
			t.Fatal("Wait channel closed before the context ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not signal after an append")
	}
	cancel()
	// After cancel the channel is drained and then closed.
	closed := false
	deadline := time.After(2 * time.Second)
	for !closed {
		select {
		case _, ok := <-w:
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("Wait channel did not close after cancel")
		}
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
