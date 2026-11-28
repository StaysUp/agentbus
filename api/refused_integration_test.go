//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A reply an on over a session would deliver once its request tree has
// reached the cost cap is not delivered, and the session it was for hears why
// (#122): delivery.refused, with the cap. The tree is
// TestSelfWatchHeldToTheCostCap's: a $0.25 cap and $0.30 spent. The on is the
// public one (POST /api/v0/ons), made by the person's turn with its own token,
// and control's sweep is driven by hand.

// failRefused is the record with its first delivery.refused write failing,
// as a record that is briefly unwritable. Every other write goes through.
type failRefused struct {
	*store.Store
	failed atomic.Int32
}

func (f *failRefused) AppendOnce(e event.Event) (bool, error) {
	if e.Kind == event.DeliveryRefused && f.failed.CompareAndSwap(0, 1) {
		return false, errors.New("injected: the record is not writable")
	}
	return f.Store.AppendOnce(e)
}

// capWatch makes the tree on s: the person's turn, an on it registers over its
// own session into a new session, $0.30 spent by the turn, and the turn ended
// and in the record. It answers the env, the turn and the session the on
// delivers to; the on has not fired.
func capWatch(t *testing.T, s stack) (*env, Started, string) {
	t.Helper()
	s.permissions = filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(s.permissions, []byte("[limits]\nmax_cost_usd_per_tree = 0.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvOf(t, s)
	p := e.firstTurn(t, gitRepo(t), "claude", "parent", true)
	ptok := tokenOf(t, dir, "parent")
	code, out := as(t, e, ptok, "POST", "/api/v0/sessions", `{}`)
	var sc SessionCreated
	if code != 201 || json.Unmarshal([]byte(out), &sc) != nil {
		t.Fatalf("open a session: %d %s", code, out)
	}
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"event":"turn.completed","filter":{"session":"`+p.Session+`"},"session":"`+sc.Session+`"}`); code != 201 {
		t.Fatalf("an on: %d %s", code, out)
	}
	cost := 0.30
	if _, err := e.store.AppendOnce(event.Event{ID: "mr-" + p.Turn, Kind: event.ModelResponded, Time: time.Now().UTC(),
		Session: p.Session, Turn: p.Turn, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	release(dir, "parent")
	waitEnded(t, e, p.Turn)
	if _, _, err := jobs.RecordTurnEnds(e.q, e.store, nil, time.Now().Add(time.Hour), 200); err != nil {
		t.Fatal(err)
	}
	if ended, err := e.store.Ended(p.Turn); err != nil || !ended {
		t.Fatalf("the turn's end is not in the record: %v %v", ended, err)
	}
	return e, p, sc.Session
}

// refusedIn reads session's delivery.refused and counts its turns, through
// the API.
func refusedIn(t *testing.T, e *env, session string) ([]event.Event, int) {
	t.Helper()
	code, out := as(t, e, token, "GET", "/api/v0/sessions/"+session+"/events?kind=delivery.refused", "")
	var page struct {
		Events []event.Event `json:"events"`
	}
	if code != 200 || json.Unmarshal([]byte(out), &page) != nil {
		t.Fatalf("session events: %d %s", code, out)
	}
	code, out = as(t, e, token, "GET", "/api/v0/turns?session="+session, "")
	var list struct {
		Turns []json.RawMessage `json:"turns"`
	}
	if code != 200 || json.Unmarshal([]byte(out), &list) != nil {
		t.Fatalf("turns: %d %s", code, out)
	}
	return page.Events, len(list.Turns)
}

// heldOnce checks the session the on was for: one notice, of the cost cap,
// naming the turn whose reply it was, and no turn, shown or queued.
func heldOnce(t *testing.T, e *env, p Started, session string) {
	t.Helper()
	got, turns := refusedIn(t, e, session)
	if len(got) != 1 {
		t.Fatalf("the receiving session has %d delivery.refused, want 1: %+v", len(got), got)
	}
	if n := got[0]; !strings.Contains(n.Error, "may cost at most $0.25") || n.Origin == nil || n.Origin.Turn != p.Turn {
		t.Fatalf("the notice is %+v, want the $0.25 cap and the parent's turn as the work", n)
	}
	if turns != 0 {
		t.Fatalf("the receiving session has %d turns, want none", turns)
	}
	queued, err := e.q.List(queue.DefaultTeam, queue.Filter{Session: session}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Fatalf("the receiving session has %d jobs queued, want none: %+v", len(queued), queued)
	}
}

func TestCostCapRefusalReachesTheWatchingSession(t *testing.T) {
	e, p, into := capWatch(t, stack{})
	if _, err := e.q.FireWatchOns(queue.DefaultTeam, recordReader(e.store), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	heldOnce(t, e, p, into)
}

// readFrom is recordReader that keeps where each check reads from: the on's
// cursor, as FireWatchOns passes it.
func readFrom(st *store.Store, from *[]int64) queue.RecordReader {
	read := recordReader(st)
	return func(kind string, after int64, limit int) ([]queue.RecordEvent, error) {
		*from = append(*from, after)
		return read(kind, after, limit)
	}
}

// endedSeq is the record seq of turn's turn.ended.
func endedSeq(t *testing.T, e *env, turn string) int64 {
	t.Helper()
	evs, err := recordReader(e.store)("turn.ended", 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Turn == turn {
			return ev.Seq
		}
	}
	t.Fatalf("no turn.ended of %s", turn)
	return 0
}

// The notice's write fails once. The on stays before the turn, and the next
// sweep writes the notice: once, under its id, with no turn.
func TestRefusedNoticeSurvivesAFailedWrite(t *testing.T) {
	var rec *failRefused
	e, p, into := capWatch(t, stack{record: func(st *store.Store) jobs.Recorder {
		rec = &failRefused{Store: st}
		return rec
	}})
	ended := endedSeq(t, e, p.Turn)
	var from []int64
	read := readFrom(e.store, &from)
	_, err := e.q.FireWatchOns(queue.DefaultTeam, read, nil, nil, nil)
	if rec.failed.Load() != 1 {
		t.Fatal("the injected failure did not happen")
	}
	t.Logf("injected delivery.refused write failures: %d; first sweep answered: %v", rec.failed.Load(), err)
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("the sweep whose notice failed answered %v, want the write's error", err)
	}
	if got, _ := refusedIn(t, e, into); len(got) != 0 {
		t.Fatalf("a notice after its write failed: %+v", got)
	}
	if _, err := e.q.FireWatchOns(queue.DefaultTeam, read, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("cursor at the failed sweep %d, at the next %d; the turn's turn.ended is seq %d", from[0], from[1], ended)
	if len(from) != 2 || from[1] >= ended {
		t.Fatalf("the on read from %v; the turn's turn.ended is seq %d: the cursor moved past the refusal", from, ended)
	}
	heldOnce(t, e, p, into)
	// Once the notice is in, the on reads past the turn, and nothing more is
	// written.
	if _, err := e.q.FireWatchOns(queue.DefaultTeam, read, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(from) != 3 || from[2] < ended {
		t.Fatalf("the on read from %v; the turn's turn.ended is seq %d: the cursor stayed before a written notice", from, ended)
	}
	heldOnce(t, e, p, into)
}
