package queue

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
)

// The first turn of a workspace made from another's copy goes to the runner
// that holds the source's copy, the only one that can make it; with no copy of
// the source anywhere, any runner of the machine takes it and its turn says
// why. Once the workspace has a copy of its own, its jobs follow that copy.
func TestForkGoesToTheSourcesRunner(t *testing.T) {
	q, _ := open(t)
	holder := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	src := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	if j := run(t, q, holder, Result{}); j.ID != src.ID {
		t.Fatalf("ran %s, want the source's first turn", j.ID)
	}
	other := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	fork := Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true, Workspace: "ws_f000000000000001",
		Fork: &Fork{Workspace: ids.WorkspaceID(src.Session), Checkpoint: "base"}}
	j := makeJob(t, q, fork)
	if got := takes(t, q, other); got != "" {
		t.Fatalf("a runner without the source's copy took %s", got)
	}
	if got := takes(t, q, holder); got != j.ID {
		t.Fatalf("the source's runner took %q, want %s", got, j.ID)
	}

	// A source with no copy at all: any runner of the machine takes it.
	orphan := fork
	orphan.Workspace, orphan.Fork = "ws_f000000000000002", &Fork{Workspace: "ws_0000000000000000", Checkpoint: "base"}
	o := makeJob(t, q, orphan)
	if got := takes(t, q, other); got != o.ID {
		t.Fatalf("with no source copy, %q was taken, want %s", got, o.ID)
	}
}

// A workspace made from another's copy is put away once its turn ends, at
// once and even with changes not applied, since they never are. Any other
// workspace's turn end queues nothing.
func TestForkIsPutAwayWhenItsTurnEnds(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)}
	q, err := Open(filepath.Join(t.TempDir(), "jobs.db"), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	st := withRecord(t, q)
	q.ReadAppliedWith(st.Applied)
	q.ReadActivityWith(st.WorkspaceUsedAfter)
	q.ReadPutAwayWith(func() bool { return true })
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})

	srcJob := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	src := ids.WorkspaceID(srcJob.Session)
	const forkWS = ids.WorkspaceID("ws_f000000000000003")
	from := &Fork{Workspace: src, Checkpoint: "base"}
	forkJob := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true, Workspace: forkWS, Fork: from})
	run(t, q, rn, Result{Changeset: hexOf('2'), Checkpoint: "cp2"})
	if _, err := q.RegisterRecordOn(DefaultTeam, "turn.ended", Action{Queue: KindPutAway}, "builtin/put_away/turn.ended", 0); err != nil {
		t.Fatal(err)
	}
	recordOutcomes(t, q, st, src)
	recordOutcomes(t, q, st, forkWS)
	for _, j := range []Job{state(t, q, srcJob.ID), state(t, q, forkJob.ID)} {
		if _, _, err := st.Append(event.Event{ID: "evt_tn_" + j.ID, Kind: event.TurnEnded, Time: c.now(), Turn: j.Turn, State: "finished"}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(kind string, after int64, limit int) ([]RecordEvent, error) {
		evs, err := st.EventsOfKind(kind, after, limit)
		var out []RecordEvent
		for _, e := range evs {
			out = append(out, RecordEvent{ID: e.ID, Seq: e.Seq, Time: e.Time, Workspace: e.Workspace, Turn: e.Turn})
		}
		return out, err
	}
	where := func(ws string) (DeliverTo, bool, error) {
		to := DeliverTo{Workspace: ws, Machine: conA, Isolate: true}
		if ws == string(forkWS) {
			to.Fork = from
		}
		return to, ws == string(src) || ws == string(forkWS), nil
	}
	after := func(string) (time.Duration, bool) { return 30 * time.Minute, true }
	if n, err := q.FireRecordOns(DefaultTeam, read, where, after); err != nil || n != 1 {
		t.Fatalf("fired %d (%v), want one put-away", n, err)
	}
	var id string
	if err := q.db.QueryRow(`SELECT id FROM jobs WHERE kind = 'put_away'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	p := state(t, q, id)
	if p.Spec.Workspace != forkWS || p.Spec.Fork == nil || p.NotBefore == nil || !p.NotBefore.Equal(c.now()) {
		t.Fatalf("put-away %+v, want the fork's, due now", p.Spec)
	}
	if j, _ := take(t, q, rn); j.ID != p.ID {
		t.Fatalf("took %s (%s), want the fork's put-away though its changes are not applied", j.ID, j.Kind)
	}
}
