package queue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/store"
)

// putAwayRig is a queue with a real record behind both of its put-away reads
// (the applied state and the workspace's use), one runner holding one sandbox
// workspace's copy, and the built-in on over workspace.discarded.
type putAwayRig struct {
	q   *Queue
	c   *clock
	st  *store.Store
	rn  Runner
	ws  ids.WorkspaceID
	on  On
	off bool // putting copies away is off
}

const quiet = 30 * time.Minute

// newPutAwayRig opens the rig; its first turn ran and ended with res.
func newPutAwayRig(t *testing.T, res Result) *putAwayRig {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	q, err := Open(filepath.Join(t.TempDir(), "jobs.db"), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	st := withRecord(t, q)
	// Take decides a put-away inside its own transaction, on the queue's one
	// connection, so these readers read the record only, as control's do
	// (jobs.New); the rig writes the jobs' outcomes into it first (record).
	q.ReadAppliedWith(st.Applied)
	q.ReadActivityWith(st.WorkspaceUsedAfter)
	r := &putAwayRig{q: q, c: c, st: st}
	r.rn = capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	run(t, q, r.rn, res)
	r.ws = ids.WorkspaceID(first.Session)
	if r.on, err = q.RegisterRecordOn(DefaultTeam, "workspace.discarded", Action{Queue: KindPutAway}, "builtin/put_away/workspace.discarded", 0); err != nil {
		t.Fatal(err)
	}
	r.record(t) // what the first turn did is in the record before anything else
	return r
}

// record writes the outcomes of the workspace's ended jobs into the record.
func (r *putAwayRig) record(t *testing.T) {
	t.Helper()
	recordOutcomes(t, r.q, r.st, r.ws)
}

// settle records a workspace.discarded of the workspace now, as the event the
// built-in on fires on, and fires the ons: the put-away it queued.
func (r *putAwayRig) settle(t *testing.T) Job {
	t.Helper()
	r.record(t)
	if _, _, err := r.st.Append(event.Event{ID: NewID("evt_"), Kind: event.WorkspaceDiscarded, Time: r.c.now(), Workspace: string(r.ws),
		Discarded: &event.Discarded{ResetTo: "base"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fire(); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := r.q.db.QueryRow(`SELECT id FROM jobs WHERE kind = 'put_away' ORDER BY rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("no put-away queued: %v", err)
	}
	return state(t, r.q, id)
}

// fire runs the control loop's check of the ons over the record.
func (r *putAwayRig) fire() (int, error) {
	read := func(kind string, after int64, limit int) ([]RecordEvent, error) {
		evs, err := r.st.EventsOfKind(kind, after, limit)
		var out []RecordEvent
		for _, e := range evs {
			out = append(out, RecordEvent{ID: e.ID, Seq: e.Seq, Time: e.Time, Workspace: e.Workspace})
		}
		return out, err
	}
	where := func(ws string) (DeliverTo, bool, error) {
		return DeliverTo{Workspace: ws, Machine: conA, Isolate: true}, ws == string(r.ws), nil
	}
	after := func(kind string) (time.Duration, bool) { return quiet, !r.off && kind == KindPutAway }
	return r.q.FireRecordOns(DefaultTeam, read, where, after)
}

// takeNone says no job is taken now.
func (r *putAwayRig) takeNone(t *testing.T) {
	t.Helper()
	if j, _, ok, _, err := r.q.Take(r.rn.ID, lease, false); err != nil || ok {
		t.Fatalf("took %s (%s), want nothing: err %v", j.ID, j.Kind, err)
	}
}

// kept says the put-away ended kept, with why, its outcome due for the record.
func kept(t *testing.T, q *Queue, id, why string) {
	t.Helper()
	j := state(t, q, id)
	res, _ := ReadResult(j.Result)
	if j.State != Finished || res.Kept != why || !res.OutcomeDue || j.Attempt != "" {
		t.Fatalf("put-away: state %s attempt %q kept %q due %v, want finished, no attempt, kept %q, due", j.State, j.Attempt, res.Kept, res.OutcomeDue, why)
	}
}

// A job may wait until a time: before it no runner takes it, after it the next
// take does. Its due time is on the job.
func TestNotBeforeHoldsAJobUntilItsTime(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	p := r.settle(t)
	if p.NotBefore == nil || !p.NotBefore.Equal(r.c.now().Add(quiet)) {
		t.Fatalf("not before %v, want %v", p.NotBefore, r.c.now().Add(quiet))
	}
	r.takeNone(t)
	r.c.add(quiet - time.Second)
	r.takeNone(t)
	r.c.add(time.Second)
	if j, _ := take(t, r.q, r.rn); j.ID != p.ID {
		t.Fatalf("took %s (%s), want the put-away %s", j.ID, j.Kind, p.ID)
	}
}

// A settled, quiet copy goes: when the put-away comes due the runner holding
// the copy takes it, as a close, and the job keeps why it was queued.
func TestSettledQuietCopyIsPutAway(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	p := r.settle(t)
	if b := p.Links.Because; b == nil || b.On != r.on.ID || b.Event == "" || b.Seq == 0 {
		t.Fatalf("because %+v, want the on %s and its event", b, r.on.ID)
	}
	if p.Spec.Workspace != r.ws || p.Spec.Machine != conA {
		t.Fatalf("spec %+v, want workspace %s on %s", p.Spec, r.ws, conA)
	}
	r.c.add(quiet)
	j, a := take(t, r.q, r.rn)
	if j.ID != p.ID || j.Kind != KindPutAway {
		t.Fatalf("took %s (%s), want the put-away", j.ID, j.Kind)
	}
	if _, _, err := r.q.Start(r.rn.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := r.q.End(r.rn.ID, a.ID, a.Number, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	if s := state(t, r.q, p.ID); s.State != Finished {
		t.Fatalf("put-away %s, want finished", s.State)
	}
}

// A copy with unapplied work is never touched: the put-away ends kept, and no
// runner gets it.
func TestUnappliedWorkKeepsTheCopy(t *testing.T) {
	r := newPutAwayRig(t, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})
	p := r.settle(t)
	r.c.add(quiet)
	r.takeNone(t)
	kept(t, r.q, p.ID, KeptUnapplied)
}

// A put-away waiting out its quiet time holds no place in the lane: the turn
// sent meanwhile runs at once. Once due, the put-away reads the record, finds
// the turn, and does nothing.
func TestUseSinceKeepsTheCopy(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	p := r.settle(t)
	r.c.add(time.Minute)
	turn := makeJob(t, r.q, Spec{Session: ids.SessionID(r.ws)})
	if j := run(t, r.q, r.rn, Result{}); j.ID != turn.ID {
		t.Fatalf("ran %s (%s), want the turn sent meanwhile", j.ID, j.Kind)
	}
	r.record(t)
	r.c.add(quiet)
	r.takeNone(t)
	kept(t, r.q, p.ID, KeptUsed)
}

// Work waiting in the workspace when the put-away comes due keeps the copy,
// and the waiting turn is taken in the same take.
func TestWaitingWorkKeepsTheCopy(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	p := r.settle(t)
	r.c.add(quiet)
	turn := makeJob(t, r.q, Spec{Session: ids.SessionID(r.ws)})
	if j, _ := take(t, r.q, r.rn); j.ID != turn.ID {
		t.Fatalf("took %s (%s), want the waiting turn", j.ID, j.Kind)
	}
	kept(t, r.q, p.ID, KeptBusy)
}

// Two events settle the workspace in turn: the earlier put-away yields to the
// later one, which goes. One put-away that kept the copy is no use of the
// workspace, so it never holds up the next.
func TestLaterPutAwayWins(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	first := r.settle(t)
	r.c.add(time.Minute)
	second := r.settle(t)
	r.c.add(quiet)
	j, _ := take(t, r.q, r.rn)
	if j.ID != second.ID {
		t.Fatalf("took %s (%s), want the later put-away %s", j.ID, j.Kind, second.ID)
	}
	kept(t, r.q, first.ID, KeptBusy)
}

// A put-away with no record to read keeps the copy: nothing goes on a guess.
func TestNoRecordKeepsTheCopy(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	p := r.settle(t)
	r.q.ReadActivityWith(func(ids.WorkspaceID, int64) (bool, error) { return false, errors.New("unreadable") })
	r.c.add(quiet)
	r.takeNone(t)
	kept(t, r.q, p.ID, KeptNoRecord)
}

// A pending put-away is not busy work: the person may still discard, apply or
// close while it waits.
func TestPendingPutAwayBlocksNoPersonAction(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	r.settle(t)
	if _, created, err := r.q.CloseWorkspace(DefaultTeam, r.ws, conA, nil); err != nil || !created {
		t.Fatalf("close beside a pending put-away: created %v err %v", created, err)
	}
}

// An on over the record fires once per event, across ticks; its cursor moves
// past each event it read. With putting away off it queues nothing, and the
// fire is kept with why, so turning it on later does not reach back.
func TestRecordOnFiresOncePerEvent(t *testing.T) {
	r := newPutAwayRig(t, Result{})
	r.settle(t)
	if n, err := r.fire(); err != nil || n != 0 {
		t.Fatalf("second tick: queued %d err %v, want 0", n, err)
	}
	count := func() int {
		var n int
		if err := r.q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = 'put_away'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("%d put-aways, want 1", n)
	}
	r.off = true
	if _, _, err := r.st.Append(event.Event{ID: NewID("evt_"), Kind: event.WorkspaceDiscarded, Time: r.c.now(), Workspace: string(r.ws)}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.fire(); err != nil || n != 0 {
		t.Fatalf("off: queued %d err %v, want 0", n, err)
	}
	var why string
	if err := r.q.db.QueryRow(`SELECT error FROM on_fires WHERE on_id = ? AND job = '' LIMIT 1`, r.on.ID).Scan(&why); err != nil || why == "" {
		t.Fatalf("off fire: why %q err %v", why, err)
	}
	r.off = false
	if n, err := r.fire(); err != nil || n != 0 || count() != 1 {
		t.Fatalf("on again: queued %d err %v, %d put-aways, want no reach back", n, err, count())
	}
}

// The built-in on is registered once by its key; a second start returns it
// with its cursor, and an event the record cannot hold an on for is refused.
func TestRegisterRecordOnOnce(t *testing.T) {
	q, _ := open(t)
	a, err := q.RegisterRecordOn(DefaultTeam, "workspace.applied", Action{Queue: KindPutAway}, "builtin/put_away/workspace.applied", 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.RegisterRecordOn(DefaultTeam, "workspace.applied", Action{Queue: KindPutAway}, "builtin/put_away/workspace.applied", 99)
	if err != nil || b.ID != a.ID || b.Action.Queue != KindPutAway {
		t.Fatalf("second register: %+v err %v, want %s", b, err, a.ID)
	}
	var cursor int64
	if err := q.db.QueryRow(`SELECT cursor FROM ons WHERE id = ?`, a.ID).Scan(&cursor); err != nil || cursor != 7 {
		t.Fatalf("cursor %d err %v, want 7", cursor, err)
	}
	if _, err := q.RegisterRecordOn(DefaultTeam, "turn.started", Action{Queue: KindPutAway}, "k", 0); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := q.RegisterRecordOn(DefaultTeam, "workspace.applied", Action{Queue: KindTurn}, "k2", 0); !errors.Is(err, ErrBadState) {
		t.Fatalf("unknown action: %v", err)
	}
}
