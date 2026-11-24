package jobs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

const (
	migCon = "con_aaaaaaaaaaaaaaaa"
	migRef = "fld_0123456789abcdef"
)

func cs(c byte) string { return strings.Repeat(string(c), 64) }

// migEnv is a queue and a record as a control before 0.6.6 left them: the
// test ends jobs with the queue alone (old), then writes the outcome events in
// the shape the older finisher wrote them, or none.
type migEnv struct {
	t  *testing.T
	q  *queue.Queue
	st *store.Store
	r  queue.Runner
}

func newMigEnv(t *testing.T) *migEnv {
	t.Helper()
	dir := t.TempDir()
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	q.ReadAppliedWith(st.Applied)
	r, err := q.RegisterRunner(queue.DefaultTeam, "r", []string{"agent:claude"}, 8, queue.HashToken(queue.NewID("tok_")), &queue.Capabilities{Connector: migCon})
	if err != nil {
		t.Fatal(err)
	}
	return &migEnv{t: t, q: q, st: st, r: r}
}

// take takes and starts the next job (a folder job with folder).
func (e *migEnv) take(folder bool) (queue.Job, queue.Attempt) {
	e.t.Helper()
	j, a, ok, _, err := e.q.Take(e.r.ID, time.Minute, folder)
	if err != nil || !ok {
		e.t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := e.q.Start(e.r.ID, a.ID, a.Number, time.Minute); err != nil {
		e.t.Fatal(err)
	}
	return j, a
}

// oldEnd ends the next job with the queue alone, as before 0.6.6, and returns it.
func (e *migEnv) oldEnd(folder bool, f queue.Finish) queue.Job {
	e.t.Helper()
	j, a := e.take(folder)
	if j.Spec.Session == "" && j.Kind == queue.KindTurn {
		f.Session, f.Workspace = j.Session, &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.ID}
	}
	done, err := e.q.End(e.r.ID, a.ID, a.Number, f)
	if err != nil {
		e.t.Fatal(err)
	}
	return done
}

func (e *migEnv) append(ev event.Event) {
	e.t.Helper()
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if _, err := e.st.AppendOnce(ev); err != nil {
		e.t.Fatal(err)
	}
}

// oldTurn ends a turn of the workspace (first, when session is empty) with
// changeset c and checkpoint cp, and writes its turn.started and, unless
// pre05, the turn.changed an older finisher wrote: no changeset in it.
func (e *migEnv) oldTurn(session ids.SessionID, c, cp string, pre05 bool) queue.Job {
	e.t.Helper()
	spec := queue.Spec{Session: session, Prompt: "next"}
	if session == "" {
		spec = queue.Spec{Harness: "claude", FolderRef: migRef, Machine: migCon, Isolate: true, Prompt: "hi"}
	}
	if _, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "", spec, queue.Links{}, queue.Limits{}, nil); err != nil {
		e.t.Fatal(err)
	}
	res := queue.Result{Changeset: c, Checkpoint: cp}
	if pre05 { // before 0.5 the job kept its changes, and the record had none
		res = queue.Result{Changeset: c, Changes: &workspace.Changes{Kind: workspace.KindSnapshot, From: "base", To: cp, Files: []workspace.Change{}}}
	}
	b, _ := json.Marshal(res)
	j := e.oldEnd(false, queue.Finish{Status: queue.Finished, Result: b})
	e.append(event.Event{ID: "ts_" + j.Turn, Kind: event.TurnStarted, Session: j.Session, Turn: j.Turn, Text: "hi", Harness: "claude"})
	if !pre05 {
		e.append(event.Event{ID: changedID(j), Kind: event.TurnChanged, Session: j.Session, Turn: j.Turn,
			Changes: &event.TurnChanges{Isolated: true, From: "base", To: cp, Files: []event.ChangedFile{}}})
	}
	return j
}

// Codex's P1s on the first round (2026-11-24): a record written before 0.6.6
// left a turn's changeset, an apply's to and an update's head and checkpoints
// in the job rows only, and a turn from before 0.5 has no event at all. Read
// from the record alone, the next apply targeted the pre-merge turn, an update
// merged from the base, a discard reset behind the applied merge, and the
// pre-0.5 turn was gone. The one-time snapshot makes each answer what the
// queue answered from its job rows before.
func TestMigrateAppliedKeepsTheQueuesAnswers(t *testing.T) {
	e := newMigEnv(t)
	t1 := e.oldTurn("", cs('1'), "cp1", false)
	ws := ids.WorkspaceID(t1.Session)
	t2 := e.oldTurn(ids.SessionID(t1.Session), cs('2'), "cp2", false)

	// An update the older finisher ended: head t2, t1 folded, and a
	// workspace.updated with no head or checkpoints.
	if _, _, err := e.q.Update(queue.DefaultTeam, ws, migCon, nil, false); err != nil {
		t.Fatal(err)
	}
	uj, ua := e.take(false)
	if err := e.q.MarkUpdated(queue.DefaultTeam, uj.ID, t2.Turn, []string{t1.Turn}); err != nil {
		t.Fatal(err)
	}
	ub, _ := json.Marshal(queue.Result{Update: &queue.UpdateResult{From: cs('a'), To: cs('b'), TheirsCheckpoint: "theirs1", MergedCheckpoint: "merged1"}})
	uj, err := e.q.End(e.r.ID, ua.ID, ua.Number, queue.Finish{Status: queue.Finished, Result: ub})
	if err != nil {
		t.Fatal(err)
	}
	e.append(event.Event{ID: updateID(uj), Kind: event.WorkspaceUpdated, Workspace: string(ws),
		Updated: &event.Updated{Result: "updated", From: cs('a'), To: cs('b'), FoldedTurns: []string{t1.Turn}}})

	// A second workspace whose only turn is from before 0.5.
	p1 := e.oldTurn("", cs('p'), "cpp", true)
	pws := ids.WorkspaceID(p1.Session)

	// Read from the record alone, before the snapshot, both are gone.
	if _, _, ok, err := e.q.AppliableCursor(queue.DefaultTeam, ws); err != nil || ok {
		t.Fatalf("before the snapshot: ok %v err %v", ok, err)
	}
	n, err := MigrateApplied(e.st, e.q)
	if err != nil || n != 2 {
		t.Fatalf("migrate: %d %v, want both workspaces", n, err)
	}
	if n, err := MigrateApplied(e.st, e.q); err != nil || n != 0 {
		t.Fatalf("migrate again: %d %v, want none", n, err)
	}

	if turn, _, ok, err := e.q.AppliableCursor(queue.DefaultTeam, pws); err != nil || !ok || turn != p1.Turn {
		t.Fatalf("the pre-0.5 turn: %q %v %v", turn, ok, err)
	}
	// An update now merges from the folder the last update read (theirs).
	nu, _, err := e.q.Update(queue.DefaultTeam, ws, migCon, nil, false)
	if err != nil || nu.Spec.Restore != "theirs1" {
		t.Fatalf("update merge base %q (%v), want theirs1", nu.Spec.Restore, err)
	}
	if _, err := e.q.Cancel(queue.DefaultTeam, nu.ID); err != nil {
		t.Fatal(err)
	}
	// The apply goes from the folder the update read to the merge.
	aj, created, err := e.q.Apply(queue.DefaultTeam, t2.Turn)
	if err != nil || !created || aj.Spec.ApplyFrom != cs('a') || aj.Spec.ApplyTo != cs('b') {
		t.Fatalf("apply after the update: from %q to %q (%v %v), want the folder and the merge", aj.Spec.ApplyFrom, aj.Spec.ApplyTo, created, err)
	}
	// It ends the 0.6.6 way, its outcome in the record on top of the snapshot.
	j, a := e.take(true)
	if _, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished,
		Result: []byte(`{"apply":{"files":["a.txt"]}}`)}); err != nil {
		t.Fatal(err)
	}
	// A new turn after it: the discard resets to the applied merge, not to t2's
	// own pre-merge checkpoint.
	if _, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "", queue.Spec{Session: ids.SessionID(t1.Session), Prompt: "more"},
		queue.Links{}, queue.Limits{}, nil); err != nil {
		t.Fatal(err)
	}
	j3, a3 := e.take(false)
	e.append(event.Event{ID: "ts_" + j3.Turn, Kind: event.TurnStarted, Session: j3.Session, Turn: j3.Turn, Text: "more", Harness: "claude"})
	if _, err := EndAs(e.st, e.q, e.r.ID, a3.ID, a3.Number, j3, queue.Finish{Status: queue.Finished,
		Result: []byte(`{"changeset":"` + cs('3') + `","changes":{"kind":"snapshot","from":"cp2","to":"cp3","files":[]}}`)}); err != nil {
		t.Fatal(err)
	}
	dj, _, err := e.q.Discard(queue.DefaultTeam, ws, migCon, nil)
	if err != nil || dj.Spec.Restore != "merged1" {
		t.Fatalf("discard restore %q (%v), want merged1", dj.Spec.Restore, err)
	}
}

// A workspace whose record is complete (0.6.6 on) gets no snapshot.
func TestMigrateAppliedLeavesACompleteRecord(t *testing.T) {
	e := newMigEnv(t)
	if _, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "", queue.Spec{Harness: "claude", FolderRef: migRef, Machine: migCon,
		Isolate: true, Prompt: "hi"}, queue.Links{}, queue.Limits{}, nil); err != nil {
		t.Fatal(err)
	}
	j, a := e.take(false)
	e.append(event.Event{ID: "ts_" + j.Turn, Kind: event.TurnStarted, Session: j.Session, Turn: j.Turn, Text: "hi", Harness: "claude"})
	if _, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished, Session: j.Session,
		Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.ID},
		Result:    []byte(`{"changeset":"` + cs('1') + `","changes":{"kind":"snapshot","from":"base","to":"cp1","files":[]}}`)}); err != nil {
		t.Fatal(err)
	}
	if n, err := MigrateApplied(e.st, e.q); err != nil || n != 0 {
		t.Fatalf("migrate: %d %v, want none", n, err)
	}
	if turn, _, ok, err := e.q.AppliableCursor(queue.DefaultTeam, ids.WorkspaceID(j.Session)); err != nil || !ok || turn != j.Turn {
		t.Fatalf("cursor %q %v %v", turn, ok, err)
	}
}

// Without a record the queue cannot say what is applied, and says so rather
// than answering from its job rows.
func TestQueueWithoutRecordRefuses(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, _, _, err := q.AppliableCursor(queue.DefaultTeam, "ws_x"); !errors.Is(err, queue.ErrNoRecord) {
		t.Fatalf("%v", err)
	}
}

// Codex's P1s on the second round (2026-11-24). A turn whose finish names its
// changeset but carries no file list is recorded all the same, so it stays
// appliable. And an apply cancelled before it ran has no outcome, then as now,
// so a complete record with one gets no snapshot.
func TestChangesetOnlyTurnAndCancelledApply(t *testing.T) {
	e := newMigEnv(t)
	if _, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "", queue.Spec{Harness: "claude", FolderRef: migRef, Machine: migCon,
		Isolate: true, Prompt: "hi"}, queue.Links{}, queue.Limits{}, nil); err != nil {
		t.Fatal(err)
	}
	j, a := e.take(false)
	ws := ids.WorkspaceID(j.Session)
	e.append(event.Event{ID: "ts_" + j.Turn, Kind: event.TurnStarted, Session: j.Session, Turn: j.Turn, Text: "hi", Harness: "claude"})
	if _, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished, Session: j.Session,
		Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.ID},
		Result:    []byte(`{"changeset":"` + cs('1') + `","checkpoint":"cp1"}`)}); err != nil {
		t.Fatal(err)
	}
	if turn, _, ok, err := e.q.AppliableCursor(queue.DefaultTeam, ws); err != nil || !ok || turn != j.Turn {
		t.Fatalf("a changeset-only turn: %q %v %v", turn, ok, err)
	}
	// It sent no file list, and the record holds none: not an empty one, which
	// would say no file changed (Codex, round 3).
	ended, err := e.q.Get(queue.DefaultTeam, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := Changes(e.st, ended); err != nil || c != nil {
		t.Fatalf("changes of a changeset-only turn: %+v %v, want none", c, err)
	}
	aj, created, err := e.q.Apply(queue.DefaultTeam, j.Turn)
	if err != nil || !created || aj.Spec.ApplyTo != cs('1') {
		t.Fatalf("apply: to %q %v %v", aj.Spec.ApplyTo, created, err)
	}
	if _, err := e.q.Cancel(queue.DefaultTeam, aj.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := MigrateApplied(e.st, e.q); err != nil || n != 0 {
		t.Fatalf("migrate: %d %v, want none for a complete record", n, err)
	}
}
