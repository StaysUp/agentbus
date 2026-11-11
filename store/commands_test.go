package store

import (
	"testing"

	"github.com/staysup/agentbus/event"
)

// CommandsSince reads the session's commands after its latest turn.started,
// oldest first, and no other session's.
func TestCommandsSince(t *testing.T) {
	s := newStore(t)
	cmd := func(session, line string) {
		e := ev(event.CommandRan, session)
		e.Command = &event.Command{Line: line}
		mustAppend(t, s, e)
	}
	started := func(session, turn string) {
		e := ev(event.TurnStarted, session)
		e.Turn = turn
		mustAppend(t, s, e)
	}
	lines := func(session string) []string {
		t.Helper()
		got, err := s.CommandsSince(session)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range got {
			out = append(out, e.Command.Line)
		}
		return out
	}
	cmd("a", "before any turn")
	started("a", "t1")
	cmd("a", "one")
	cmd("b", "another session")
	cmd("a", "two")
	if got := lines("a"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("since t1: %q", got)
	}
	started("a", "t2")
	if got := lines("a"); len(got) != 0 {
		t.Fatalf("after the next turn started: %q", got)
	}
	if got := lines("b"); len(got) != 1 || got[0] != "another session" {
		t.Fatalf("session b: %q", got)
	}
	for i := range CommandsMax + 5 {
		cmd("c", string(rune('a'+i%26)))
	}
	if got := lines("c"); len(got) != CommandsMax {
		t.Fatalf("at most %d: %d", CommandsMax, len(got))
	}
}

// Page with Kind keeps the session's events of that kind only.
func TestPageByKind(t *testing.T) {
	s := newStore(t)
	for _, k := range []event.Kind{event.TurnStarted, event.CommandRan, event.ModelRequested, event.CommandRan} {
		mustAppend(t, s, ev(k, "a"))
	}
	mustAppend(t, s, ev(event.CommandRan, "b"))
	mustAppend(t, s, dropped(nil, true))
	recs, more, err := s.Page(0, Filter{Session: "a", Only: true, Kinds: []string{string(event.CommandRan)}}, 1, PageBytes)
	if err != nil || len(recs) != 1 || !more || recs[0].Kind != event.CommandRan {
		t.Fatalf("first page: %v %v %v", recs, more, err)
	}
	recs, more, _ = s.Page(recs[0].Seq, Filter{Session: "a", Only: true, Kinds: []string{string(event.CommandRan), string(event.CommandQueued)}}, 10, PageBytes)
	if len(recs) != 1 || more || recs[0].Kind != event.CommandRan {
		t.Fatalf("second page: %v %v", recs, more)
	}
}
