package store

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// foldEvents is one of each event the session list reads, over three
// sessions, so every field of a Session is set and changed along the way.
func foldEvents() []event.Event {
	yes, no := true, false
	cost := func(c float64) *float64 { return &c }
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	asked := &event.Origin{Kind: "asked", Session: "boss", Turn: "t_boss"}
	var out []event.Event
	add := func(e event.Event) {
		e.ID = fmt.Sprintf("evt_fold%d", len(out))
		if e.Time.IsZero() {
			e.Time = at(len(out))
		}
		out = append(out, e)
	}
	add(event.Event{Kind: event.WorkspaceCreated, Workspace: "ws_a", Sandbox: &yes})
	add(event.Event{Kind: event.SessionCreated, Session: "a", Workspace: "ws_a", Title: "named at birth", Origin: asked})
	add(event.Event{Kind: event.TurnStarted, Session: "a", Turn: "t1", Harness: "claude", AgentID: "agt_1", Folder: "/f", Workspace: "ws_a", Text: "first\nwords", Client: "claude-cli/2.1"})
	add(event.Event{Kind: event.RunStarted, Session: "a", Turn: "t1", Hold: "h1", Harness: "claude", Folder: "/f"})
	add(event.Event{Kind: event.ModelResponded, Session: "a", Turn: "t1", Usage: &event.Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 7, CacheWrite5mTokens: 2}, CostUSD: cost(0.1), CostComplete: &yes, Client: "claude-cli/2.1"})
	add(event.Event{Kind: event.ModelResponded, Session: "a", Turn: "t1", CostUSD: cost(0.2), CostComplete: &no})
	add(event.Event{Kind: event.RunEnded, Session: "a", Turn: "t1", Hold: "h1", ResumeID: "r1"})
	add(event.Event{Kind: event.TurnStarted, Session: "b", Turn: "t2", Text: "probe", Probe: true, Client: "codex/1"})
	add(event.Event{Kind: event.TurnStarted, Session: "b", Turn: "t3", Harness: "codex", Text: "real words", Origin: asked})
	add(event.Event{Kind: event.RunStarted, Session: "b", Turn: "t3", Hold: "h2", Harness: "codex", Folder: "/g"})
	add(event.Event{Kind: event.ChangesPreviewed, Session: "a", Time: at(100)}) // not activity
	add(event.Event{Kind: event.SessionRenamed, Session: "a", Title: "renamed"})
	add(event.Event{Kind: event.SessionArchived, Session: "b", Archived: true})
	add(event.Event{Kind: event.ModelResponded, Session: "b", Turn: "t3", Usage: &event.Usage{InputTokens: 5, CacheWrite12hTokens: 4, OutputIncomplete: true}, CostUSD: cost(0.3), CostComplete: &yes})
	add(event.Event{Kind: event.EventsDropped, Sessions: []string{"b"}})
	add(event.Event{Kind: event.SessionArchived, Session: "b"})                                                  // un-archived
	add(event.Event{Kind: event.TurnStarted, Session: "c", Turn: "t4", Text: "third", Time: t0.Add(-time.Hour)}) // an older moment, sent later
	add(event.Event{Kind: event.WorkspaceCreated, Workspace: "c", Sandbox: &no})
	add(event.Event{Kind: event.TurnStarted, Session: "a", Turn: "t5", Harness: "codex", Folder: "/h", Workspace: "ws_a"})
	add(event.Event{Kind: event.EventsDropped, AllSessions: true})
	return out
}

// sameSnapshot fails unless got and want agree; costs may differ in their last
// bits, since a fold adds the sums of its ranges.
func sameSnapshot(t *testing.T, at int64, got, want Snapshot) {
	t.Helper()
	if len(got.Sessions) != len(want.Sessions) {
		t.Fatalf("at %d: %d sessions, a fold from the start has %d", at, len(got.Sessions), len(want.Sessions))
	}
	for i := range got.Sessions {
		g, w := got.Sessions[i], want.Sessions[i]
		if math.Abs(g.CostUSD-w.CostUSD) > 1e-9 {
			t.Errorf("at %d, session %s: cost %v, a fold from the start %v", at, g.ID, g.CostUSD, w.CostUSD)
		}
		g.CostUSD, w.CostUSD = 0, 0
		if !reflect.DeepEqual(g, w) {
			t.Errorf("at %d, session %s:\n got %+v\nwant %+v", at, g.ID, g, w)
		}
	}
	if got.Seq != want.Seq || got.Store != want.Store {
		t.Errorf("at %d: cursor %s/%d, want %s/%d", at, got.Store, got.Seq, want.Store, want.Seq)
	}
}

// The list a store keeps folding forward, one event at a time, is the list a
// fold of the whole record gives, at every seq, and so is a page pinned to an
// earlier seq.
func TestSnapshotFoldMatchesAFoldFromTheStart(t *testing.T) {
	s := newStore(t)
	for _, e := range foldEvents() {
		mustAppend(t, s, e)
		hi := s.Last()
		got, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		whole, err := s.foldRange(0, hi)
		if err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, hi, got, newFold(0).merge(whole).snapshot(s.ID()))
		if hi > 2 {
			pinned, err := s.snapshotAt(hi - 2)
			if err != nil {
				t.Fatal(err)
			}
			earlier, err := s.foldRange(0, hi-2)
			if err != nil {
				t.Fatal(err)
			}
			sameSnapshot(t, hi-2, pinned, newFold(0).merge(earlier).snapshot(s.ID()))
		}
	}
	// Every field above was set somewhere: the corpus covers the fold.
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	a := session(t, snap, "a")
	if a.Title != "renamed" || a.Harness != "codex" || a.FirstAgent != "claude" || a.FirstAgentID != "agt_1" || a.Origin == nil ||
		a.Usage == nil || a.Usage.CacheWrite5mTokens != 2 || a.Turns != 2 || !a.Resumable || a.Workspace != "ws_a" || !a.Sandbox ||
		a.Client != "claude-cli/2.1" || a.CostComplete || !a.EventsDropped {
		t.Errorf("session a = %+v", a)
	}
	b := session(t, snap, "b")
	if b.Archived != nil || !b.Open || b.Resumable || b.Title != "real words" || b.Usage == nil || !b.Usage.OutputIncomplete || b.Client != "codex/1" {
		t.Errorf("session b = %+v", b)
	}
	if c := session(t, snap, "c"); c.Workspace != "c" || c.Sandbox || c.Started.After(t0) {
		t.Errorf("session c = %+v", c)
	}
}

// A list after new events reads only those events, never the record again:
// the gain the fold holds (GET /api/v0/sessions read every event on every
// call, 0.5 s to 4.6 s at 41,000 events). Proved by changing an event the
// kept fold already counted, which only a full read would see.
func TestSnapshotReadsOnlyNewEvents(t *testing.T) {
	s := newStore(t)
	cost := 0.25
	mustAppend(t, s, event.Event{ID: "evt_1", Kind: event.TurnStarted, Session: "a", Turn: "t1", Time: t0})
	mustAppend(t, s, event.Event{ID: "evt_2", Kind: event.ModelResponded, Session: "a", Turn: "t1", Time: t0, CostUSD: &cost})
	if _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
	// Only a test writes an event in place; the store never does.
	if _, err := s.w.Exec(`UPDATE events SET data = json_set(data, '$.cost_usd', 100) WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, event.Event{ID: "evt_3", Kind: event.ModelResponded, Session: "a", Turn: "t1", Time: t0, CostUSD: &cost})
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := session(t, snap, "a").CostUSD; got != 0.5 {
		t.Errorf("cost %v, want 0.5: the list read the events it had already folded", got)
	}
}
