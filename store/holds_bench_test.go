package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// Benchmarks of finding open holds (#128): a session's next turn
// (Continuation), the sweep's OpenHolds, and the write every index costs.
//
//	go test ./store -run '^$' -bench 'Continuation|OpenHolds|Append' -benchmem -count 6
//
// The store has turns completed turns across sessions of ten turns each, and
// a target session of 50 completed turns and one open, among them. inner is
// how many model calls the target's turns hold between them: events of the
// session that are not run events.

const (
	benchTarget = "ses_target"
	targetTurns = 50
)

var holdsWorkloads = []struct{ turns, inner int }{
	{10_000, 0}, {50_000, 0}, // history across sessions grows
	{10_000, 10_000}, {10_000, 50_000}, // the target session's own calls grow
}

// seedHolds makes the store of the workload, in one transaction (Append would
// write the same rows, one transaction each), and opens it again so that its
// last seq is read as on a service's start.
func seedHolds(b testing.TB, turns, inner int) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	tx, err := s.w.Begin()
	if err != nil {
		b.Fatal(err)
	}
	ins, err := tx.Prepare(`INSERT INTO events(seq, session, turn, kind, time, client, data) VALUES(?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	var seq int64
	put := func(e event.Event) {
		seq++
		e.Seq, e.ID, e.Time = seq, fmt.Sprintf("evt_%016x", seq), t0.Add(time.Duration(seq)*time.Millisecond)
		data, err := json.Marshal(e)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ins.Exec(e.Seq, e.Session, e.Turn, string(e.Kind), e.Time.UnixNano(), e.Client, string(data)); err != nil {
			b.Fatal(err)
		}
	}
	turn := func(session, id string, calls int, ended bool) {
		put(event.Event{Kind: event.TurnStarted, Session: session, Turn: id, Harness: "claude", Folder: "/w"})
		put(event.Event{Kind: event.RunStarted, Session: session, Turn: id, Hold: "h_" + id, Harness: "claude", Folder: "/w"})
		for range calls {
			put(event.Event{Kind: event.ModelRequested, Session: session, Turn: id, Client: "claude-cli/2.1.318"})
		}
		if ended {
			put(event.Event{Kind: event.RunEnded, Session: session, Turn: id, Hold: "h_" + id, ResumeID: "r_" + id})
		}
	}
	others := turns - targetTurns
	every := max(others/targetTurns, 1)
	calls := inner / (targetTurns + 1)
	done := 0
	for j := range others {
		if j%every == 0 && done < targetTurns {
			turn(benchTarget, fmt.Sprintf("trn_t%04d", done), calls, true)
			done++
		}
		turn(fmt.Sprintf("ses_%06d", j/10), fmt.Sprintf("trn_%08d", j), 1, true)
	}
	for ; done < targetTurns; done++ {
		turn(benchTarget, fmt.Sprintf("trn_t%04d", done), calls, true)
	}
	turn(benchTarget, "trn_open", inner-calls*targetTurns, false)
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	// The one hold open in the store is the target's.
	holds, err := s.OpenHolds(s.Last())
	if want := []OpenHold{{Session: benchTarget, Hold: "h_trn_open", Turn: "trn_open"}}; err != nil || !reflect.DeepEqual(holds, want) {
		b.Fatalf("open holds %+v err %v, want %+v", holds, err, want)
	}
	return s
}

// BenchmarkContinuation is what a session's next turn reads, up to three
// times per turn (POST /api/v0/turns, then run's next before and after it
// takes the session's slot).
func BenchmarkContinuation(b *testing.B) {
	for _, w := range holdsWorkloads {
		b.Run(fmt.Sprintf("turns=%d/inner=%d", w.turns, w.inner), func(b *testing.B) {
			s := seedHolds(b, w.turns, w.inner)
			b.ReportAllocs()
			for b.Loop() {
				c, err := s.Continuation(benchTarget)
				if err != nil || !c.Open || c.Harness != "claude" {
					b.Fatalf("continuation %+v err %v, want open on claude", c, err)
				}
			}
		})
	}
}

// BenchmarkOpenHolds is the read of every session's holds: the sweep's (once
// a second), a snapshot's and a start's.
func BenchmarkOpenHolds(b *testing.B) {
	for _, w := range holdsWorkloads[:2] {
		b.Run(fmt.Sprintf("turns=%d/inner=%d", w.turns, w.inner), func(b *testing.B) {
			s := seedHolds(b, w.turns, w.inner)
			b.ReportAllocs()
			for b.Loop() {
				if holds, err := s.OpenHolds(s.Last()); err != nil || len(holds) != 1 {
					b.Fatalf("open holds %+v err %v, want one", holds, err)
				}
			}
		})
	}
}

// BenchmarkAppend is one model call stored into the target session: what each
// index of events adds to every write.
func BenchmarkAppend(b *testing.B) {
	s := seedHolds(b, 10_000, 0)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := s.Append(event.Event{ID: "evt_b", Kind: event.ModelRequested, Session: benchTarget, Turn: "trn_open", Time: t0}); err != nil {
			b.Fatal(err)
		}
	}
}
