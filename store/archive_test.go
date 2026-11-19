package store

import (
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// SetArchived records a session.archived event; the snapshot reads archived
// from the latest one. It is a no-op when the state is unchanged (keeping the
// first archive time), un-archive is the same event with archived=false (the
// latest wins), archiving is not activity, and it works on a session that has
// no turn yet (its first turn still queued).
func TestSetArchived(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	mustAppend(t, s, event.Event{ID: "t1", Kind: event.TurnStarted, Session: "s1", Turn: "t1", Text: "hi", Time: base})
	find := func(id string) *Session {
		snap, err := s.Sessions(-1, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := range snap.Sessions {
			if snap.Sessions[i].ID == id {
				return &snap.Sessions[i]
			}
		}
		return nil
	}

	if x := find("s1"); x == nil || x.Archived != nil {
		t.Fatalf("before archive: %+v", x)
	}
	lastBefore := find("s1").Last

	// Archive. Its last activity is unchanged: archiving is not activity.
	at := base.Add(time.Minute)
	if _, changed, err := s.SetArchived(event.Event{ID: "a1", Session: "s1", Time: at, Archived: true}); err != nil || !changed {
		t.Fatalf("archive: changed %v %v", changed, err)
	}
	if x := find("s1"); x.Archived == nil || !x.Archived.Equal(at) || !x.Last.Equal(lastBefore) {
		t.Fatalf("after archive: archived %v last %v (was %v)", x.Archived, x.Last, lastBefore)
	}

	// Re-archive later is a no-op: the first archive time stays.
	if _, changed, err := s.SetArchived(event.Event{ID: "a2", Session: "s1", Time: at.Add(time.Hour), Archived: true}); err != nil || changed {
		t.Fatalf("re-archive should be a no-op: changed %v %v", changed, err)
	}
	if x := find("s1"); x.Archived == nil || !x.Archived.Equal(at) {
		t.Fatalf("re-archive changed the time: %v, want %v", x.Archived, at)
	}

	// Un-archive is the same event with archived=false; the latest wins.
	if _, changed, err := s.SetArchived(event.Event{ID: "a3", Session: "s1", Time: at.Add(2 * time.Hour), Archived: false}); err != nil || !changed {
		t.Fatalf("un-archive: changed %v %v", changed, err)
	}
	if x := find("s1"); x.Archived != nil {
		t.Fatalf("after un-archive: %v", x.Archived)
	}

	// Archive again: the latest event wins, with its own time.
	at2 := at.Add(3 * time.Hour)
	if _, _, err := s.SetArchived(event.Event{ID: "a4", Session: "s1", Time: at2, Archived: true}); err != nil {
		t.Fatal(err)
	}
	if x := find("s1"); x.Archived == nil || !x.Archived.Equal(at2) {
		t.Fatalf("re-archive after un-archive: %v, want %v", x.Archived, at2)
	}

	// A session with no turn (only a session.archived event, as one whose first
	// turn is still queued) is archivable and shows archived.
	if _, changed, err := s.SetArchived(event.Event{ID: "a5", Session: "s2", Time: base, Archived: true}); err != nil || !changed {
		t.Fatalf("archive a session with no turn: changed %v %v", changed, err)
	}
	if x := find("s2"); x == nil || x.Archived == nil {
		t.Fatalf("archived session with no turn: %+v", x)
	}
}
