package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// A reply is kept as one piece, byte for byte, read back by Reply and shown
// in the call's context; a second reply for the exchange is ignored.
func TestKeepReply(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	if err := s.KeepCall(call("x-1", `{"role":"user","content":"a"}`)); err != nil {
		t.Fatal(err)
	}
	msg := `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2ln"},{"type":"text","text":"<b> & c"}]}`
	if err := s.KeepReply(body.Reply{Exchange: "x-1", Status: 200, Stop: "end_turn", Time: time.Unix(1, 0), Message: json.RawMessage(msg)}); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepReply(body.Reply{Exchange: "x-1", Status: 200, Stop: "other", Message: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.Reply("x-1")
	if err != nil || !ok || string(r.Message) != msg || r.Stop != "end_turn" || r.Status != 200 {
		t.Fatalf("reply %+v %v %v", r, ok, err)
	}
	if _, ok, _ := s.Reply("x-2"); ok {
		t.Error("a reply for an exchange never kept")
	}
	c, ok, err := s.CallContext("x-1", true)
	if err != nil || !ok || c.Reply == nil || string(c.Reply.JSON) != msg || c.Reply.Size != len(msg) ||
		c.Reply.Hash != (body.Piece{JSON: json.RawMessage(msg)}).Hash() || c.StopReason != "end_turn" {
		t.Fatalf("context %+v %v %v", c, ok, err)
	}
	c, _, _ = s.CallContext("x-1", false)
	if c.Reply == nil || c.Reply.JSON != nil {
		t.Errorf("full=false reply %+v, want sizes only", c.Reply)
	}
}

// What a replayed turn reads: its turn.started, the session's turns newest
// first, and a turn's calls from model.requested, newest request first,
// without probes, with what their model.responded says.
func TestReplayQueries(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	now := time.Unix(1793858039, 0)
	for _, e := range []event.Event{
		{ID: "1", Kind: event.TurnStarted, Session: "s", Turn: "t1", Text: "first"},
		{ID: "2", Kind: event.ModelRequested, Session: "s", Turn: "t1", Exchange: "x-1"},
		{ID: "3", Kind: event.ModelRequested, Session: "s", Turn: "t1", Exchange: "x-2", Probe: true},
		{ID: "4", Kind: event.ModelResponded, Session: "s", Turn: "t1", Exchange: "x-2", Status: 200, Probe: true},
		{ID: "5", Kind: event.ModelRequested, Session: "s", Turn: "t1", Exchange: "x-3"},
		{ID: "6", Kind: event.ModelRequested, Session: "s", Turn: "t1", Exchange: "x-4"},
		{ID: "7", Kind: event.ModelResponded, Session: "s", Turn: "t1", Exchange: "x-4", Status: 200, StopReason: "end_turn"},
		{ID: "8", Kind: event.ModelResponded, Session: "s", Turn: "t1", Exchange: "x-3", Status: 529, Error: "overloaded"},
		{ID: "9", Kind: event.ModelResponded, Session: "s", Turn: "t1", Exchange: "x-1", Status: 200, StopReason: "max_tokens"},
		{ID: "10", Kind: event.ModelRequested, Session: "s", Turn: "t1", Exchange: "x-5"}, // its reply event lost
		{ID: "11", Kind: event.TurnStarted, Session: "s", Turn: "t2", Text: "second", ReplayFrom: "t1"},
		{ID: "12", Kind: event.TurnStarted, Session: "other", Turn: "t9", Text: "elsewhere"},
	} {
		e.Time = now
		if _, _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	p, from, ok, err := s.TurnStart("s", "t2")
	if err != nil || !ok || p != "second" || from != "t1" {
		t.Errorf("turn start %q %q %v %v", p, from, ok, err)
	}
	if _, _, ok, _ := s.TurnStart("s", "t9"); ok {
		t.Error("another session's turn found")
	}
	if got, err := s.SessionTurns("s"); err != nil || !reflect.DeepEqual(got, []string{"t2", "t1"}) {
		t.Errorf("turns %v %v", got, err)
	}
	// Request order, whatever order the replies landed in; the lost reply
	// is still listed.
	// A reply's error is carried, so a reply that did not arrive whole is
	// never read as finished from its stop reason alone.
	want := []Call{{"x-5", false, 0, "", ""}, {"x-4", true, 200, "end_turn", ""}, {"x-3", true, 529, "", "overloaded"},
		{"x-1", true, 200, "max_tokens", ""}}
	if got, err := s.TurnCalls("s", "t1"); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("calls %v %v\nwant %v", got, err, want)
	}
	if used, err := s.UsedReplay("s"); err != nil || !used {
		t.Errorf("used replay %v %v, want true", used, err)
	}
	if used, err := s.UsedReplay("other"); err != nil || used {
		t.Errorf("other used replay %v %v, want false", used, err)
	}
	if closed, err := s.TurnClosed("s", "t1"); err != nil || closed {
		t.Errorf("t1 closed %v %v, want false", closed, err)
	}
	if _, _, err := s.Append(event.Event{ID: "13", Kind: event.SessionClosed, Session: "s", Turn: "t1", Time: now}); err != nil {
		t.Fatal(err)
	}
	if closed, err := s.TurnClosed("s", "t1"); err != nil || !closed {
		t.Errorf("t1 closed %v %v, want true", closed, err)
	}
}

// A drop naming the session after the turn started, a drop for every
// session, or an unsettled session.closed make a turn incomplete; a drop
// before the turn or for another session does not.
func TestTurnIncomplete(t *testing.T) {
	now := time.Unix(1793858039, 0)
	for name, tc := range map[string]struct {
		evs  []event.Event
		want bool
	}{
		"clean":                {nil, false},
		"drop before the turn": {[]event.Event{{ID: "d", Kind: event.EventsDropped, Dropped: 1, Sessions: []string{"s"}}}, false},
		"drop after, this session": {[]event.Event{{ID: "a", Kind: event.TurnStarted, Session: "s", Turn: "t1"},
			{ID: "d", Kind: event.EventsDropped, Dropped: 1, Sessions: []string{"x", "s"}}}, true},
		"drop after, other session": {[]event.Event{{ID: "a", Kind: event.TurnStarted, Session: "s", Turn: "t1"},
			{ID: "d", Kind: event.EventsDropped, Dropped: 1, Sessions: []string{"x"}}}, false},
		"drop after, every session": {[]event.Event{{ID: "a", Kind: event.TurnStarted, Session: "s", Turn: "t1"},
			{ID: "d", Kind: event.EventsDropped, Dropped: 1, AllSessions: true}}, true},
		"unsettled": {[]event.Event{{ID: "a", Kind: event.TurnStarted, Session: "s", Turn: "t1"},
			{ID: "c", Kind: event.SessionClosed, Session: "s", Turn: "t1", Unsettled: true}}, true},
	} {
		s := open(t, filepath.Join(t.TempDir(), "events.db"))
		evs := tc.evs
		if name == "drop before the turn" {
			evs = append(evs, event.Event{ID: "a", Kind: event.TurnStarted, Session: "s", Turn: "t1"})
		}
		for _, e := range evs {
			e.Time = now
			if _, _, err := s.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.TurnIncomplete("s", "t1"); err != nil || got != tc.want {
			t.Errorf("%s: %v %v, want %v", name, got, err, tc.want)
		}
	}
}
