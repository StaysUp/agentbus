package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// recordOf is the record's one event of kind, decoded; it fails unless there
// is exactly one.
func recordOf(t *testing.T, st *store.Store, kind event.Kind) event.Event {
	t.Helper()
	recs, err := st.After(0, store.Filter{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Event
	for _, r := range recs {
		if r.Kind == kind {
			var e event.Event
			if err := json.Unmarshal(r.Data, &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d %s in the record, want 1", len(out), kind)
	}
	return out[0]
}

// Review P2 (t_7274812a7a928fcb): control's setting reader is installed when
// the jobs server is made, before anything serves. A runner that reconnects at
// control's start and takes at once meets a due put-away with the setting
// already readable, so the put-away goes rather than ending kept as "off".
func TestPutAwaySettingReadFromTheStart(t *testing.T) {
	e := newMigEnv(t)
	if _, err := New(Config{Queue: e.q, Token: mainTok, EnrollToken: enrollTok, Record: e.st, Settings: settings.NewFiles(t.TempDir())}); err != nil {
		t.Fatal(err)
	}
	first := e.oldTurn("", "", "", false)
	ws := ids.WorkspaceID(first.Session)
	if _, err := e.q.RegisterRecordOn(queue.DefaultTeam, "workspace.discarded", queue.Action{Queue: queue.KindPutAway}, "builtin/put_away/workspace.discarded", 0); err != nil {
		t.Fatal(err)
	}
	e.append(event.Event{ID: "evt_discard", Kind: event.WorkspaceDiscarded, Workspace: string(ws), Discarded: &event.Discarded{ResetTo: "base"}})
	read := func(kind string, after int64, limit int) ([]queue.RecordEvent, error) {
		evs, err := e.st.EventsOfKind(kind, after, limit)
		var out []queue.RecordEvent
		for _, x := range evs {
			out = append(out, queue.RecordEvent{ID: x.ID, Seq: x.Seq, Time: x.Time, Workspace: x.Workspace})
		}
		return out, err
	}
	where := func(w string) (queue.DeliverTo, bool, error) {
		return queue.DeliverTo{Workspace: w, Machine: migCon, Isolate: true}, true, nil
	}
	if n, err := e.q.FireRecordOns(queue.DefaultTeam, read, where, func(string) (time.Duration, bool) { return 0, true }); err != nil || n != 1 {
		t.Fatalf("fire: queued %d err %v, want 1", n, err)
	}
	if j, _ := e.take(false); j.Kind != queue.KindPutAway {
		t.Fatalf("took %s, want the put-away", j.Kind)
	}
}

// The record says why each put-away was queued, when, and what it did: the
// one that went is a workspace.closed naming the on and the event; the one that
// kept the copy is a workspace.kept with why, written by control's sweep, and
// the job then points at it.
func TestPutAwayOutcomesInTheRecord(t *testing.T) {
	e := newMigEnv(t)
	e.q.ReadActivityWith(e.st.WorkspaceUsedAfter)
	e.q.ReadPutAwayWith(func() bool { return true })
	first := e.oldTurn("", "", "", false) // no changes: settled
	ws := ids.WorkspaceID(first.Session)
	on, err := e.q.RegisterRecordOn(queue.DefaultTeam, "workspace.discarded", queue.Action{Queue: queue.KindPutAway}, "builtin/put_away/workspace.discarded", 0)
	if err != nil {
		t.Fatal(err)
	}
	read := func(kind string, after int64, limit int) ([]queue.RecordEvent, error) {
		evs, err := e.st.EventsOfKind(kind, after, limit)
		var out []queue.RecordEvent
		for _, x := range evs {
			out = append(out, queue.RecordEvent{ID: x.ID, Seq: x.Seq, Time: x.Time, Workspace: x.Workspace})
		}
		return out, err
	}
	where := func(w string) (queue.DeliverTo, bool, error) {
		return queue.DeliverTo{Workspace: w, Machine: migCon, Isolate: true}, true, nil
	}
	now := func(string) (time.Duration, bool) { return 0, true } // due at once
	settle := func(id string) {
		e.append(event.Event{ID: id, Kind: event.WorkspaceDiscarded, Workspace: string(ws), Discarded: &event.Discarded{ResetTo: "base"}})
		if n, err := e.q.FireRecordOns(queue.DefaultTeam, read, where, now); err != nil || n != 1 {
			t.Fatalf("fire: queued %d err %v, want 1", n, err)
		}
	}

	settle("evt_discard_1")
	j, a := e.take(false)
	if j.Kind != queue.KindPutAway {
		t.Fatalf("took %s, want the put-away", j.Kind)
	}
	if _, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished}); err != nil {
		t.Fatal(err)
	}
	closed := recordOf(t, e.st, event.WorkspaceClosed)
	if p := closed.PutAway; closed.Workspace != string(ws) || p == nil || p.On != on.ID || p.Event != "evt_discard_1" || p.Kept != "" || p.Queued.IsZero() {
		t.Fatalf("workspace.closed %+v put_away %+v", closed, closed.PutAway)
	}

	// The next turn makes a fresh copy, so the workspace was used: the
	// put-away of an event before that turn keeps it.
	settle("evt_discard_2")
	e.oldTurn(ids.SessionID(first.Session), "", "", false)
	if _, _, ok, _, err := e.q.Take(e.r.ID, time.Minute, false); err != nil || ok {
		t.Fatalf("take after the turn: ok %v err %v, want nothing", ok, err)
	}
	if n, err := CloseLostApplies(e.q, e.st); err != nil || n != 1 {
		t.Fatalf("sweep: wrote %d err %v, want 1", n, err)
	}
	keptEv := recordOf(t, e.st, event.WorkspaceKept)
	if p := keptEv.PutAway; keptEv.Workspace != string(ws) || p == nil || p.On != on.ID || p.Event != "evt_discard_2" || p.Kept == "" {
		t.Fatalf("workspace.kept %+v put_away %+v", keptEv, keptEv.PutAway)
	}
	var id string
	jobs, err := e.q.List(queue.DefaultTeam, queue.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range jobs {
		if x.Kind == queue.KindPutAway && x.Links.Because != nil && x.Links.Because.Event == "evt_discard_2" {
			id = x.ID
		}
	}
	k, err := e.q.Get(queue.DefaultTeam, id)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := queue.ReadResult(k.Result)
	if res.KeptEvent != keptEv.ID || res.OutcomeDue {
		t.Fatalf("kept job result %s, want kept_event %s and no due mark", k.Result, keptEv.ID)
	}
	// The kept copy's state is untouched: the closed copy's reset stands, and
	// the record's applied state still reads.
	if _, err := e.st.Applied(ws); err != nil {
		t.Fatal(err)
	}
}
