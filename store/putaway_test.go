package store

import (
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
)

// A use of the workspace after a seq is a turn started in it, or an apply,
// discard, update, close or preview of it. Another workspace's events, and a
// put-away's own workspace.kept, are not.
func TestWorkspaceUsedAfter(t *testing.T) {
	s := newStore(t)
	const ws = "ws_1111111111111111"
	at := mustAppend(t, s, event.Event{ID: "evt_d", Kind: event.WorkspaceDiscarded, Workspace: ws, Time: t0})
	used := func() bool {
		t.Helper()
		u, err := s.WorkspaceUsedAfter(ids.WorkspaceID(ws), at.Seq)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if used() {
		t.Fatal("nothing after the event")
	}
	mustAppend(t, s, event.Event{ID: "evt_k", Kind: event.WorkspaceKept, Workspace: ws, Time: t0})
	mustAppend(t, s, event.Event{ID: "evt_o", Kind: event.WorkspaceApplied, Workspace: "ws_2222222222222222", Time: t0})
	if used() {
		t.Fatal("a kept put-away and another workspace's apply are no use of this one")
	}
	mustAppend(t, s, event.Event{ID: "t1", Kind: event.TurnStarted, Session: "s1", Turn: "t1", Workspace: ws, Time: t0})
	if !used() {
		t.Fatal("a turn started in the workspace is a use")
	}
	evs, err := s.EventsOfKind(string(event.WorkspaceDiscarded), 0, 10)
	if err != nil || len(evs) != 1 || evs[0].ID != "evt_d" || evs[0].Workspace != ws || evs[0].Seq != at.Seq || !evs[0].Time.Equal(t0) {
		t.Fatalf("events of kind: %+v %v", evs, err)
	}
	if evs, _ := s.EventsOfKind(string(event.WorkspaceDiscarded), at.Seq, 10); len(evs) != 0 {
		t.Fatalf("past the cursor: %+v", evs)
	}
}
