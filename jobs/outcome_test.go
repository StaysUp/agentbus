package jobs

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

type outcomeEnv struct {
	q   *queue.Queue
	st  *store.Store
	srv *httptest.Server
	tok string
}

func newOutcomeEnv(t *testing.T) *outcomeEnv {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Record: st})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	return &outcomeEnv{q: q, st: st, srv: srv, tok: enroll(t, srv, 4).Token}
}

// take takes and starts the queue's next job, and answers its attempt's
// path.
func (e *outcomeEnv) take(t *testing.T, kind string) (queue.Job, string) {
	t.Helper()
	var taken TakenWork
	if code := send(t, e.srv, e.tok, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 || taken.Job.Kind != kind {
		t.Fatalf("take: %d %+v", code, taken.Job)
	}
	base := "/api/v0/runner/attempts/" + taken.Attempt.ID
	if code := send(t, e.srv, e.tok, "POST", base+"/start", AttemptCall{Number: 1}, nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	return *taken.Job, base
}

func (e *outcomeEnv) finish(t *testing.T, base string, status, reason string, res queue.Result) int {
	t.Helper()
	b, _ := json.Marshal(res)
	return send(t, e.srv, e.tok, "POST", base+"/finish", FinishCall{Number: 1, Status: status, Reason: reason, Result: b}, nil)
}

func (e *outcomeEnv) kinds(t *testing.T, session string) map[event.Kind]int {
	t.Helper()
	recs, err := e.st.After(0, store.Filter{Session: session, Only: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[event.Kind]int{}
	for _, r := range recs {
		out[r.Kind]++
	}
	return out
}

// A turn's finish puts what it changed into the record as turn.changed,
// once, and the job keeps only the event's id. The changes read back are
// the ones sent; a runner cannot send a turn.changed of its own.
func TestTurnChangedKeptOnce(t *testing.T) {
	e := newOutcomeEnv(t)
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, base := e.take(t, queue.KindTurn)
	diff := "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+two\n"
	changes := &workspace.Changes{Kind: workspace.KindSnapshot, Count: 2, From: "cp1", To: j.Turn, DiffsCut: true, Files: []workspace.Change{
		{Path: "a.txt", Change: "modified", Size: 4, SHA256: "h2", SHA256Before: "h1", Diff: &diff},
		{Path: "img.png", Change: "added", Size: 9000, SHA256: "h3", DiffLeftOut: true}}}
	// A runner's own turn.changed is refused: it is control's.
	forged, _ := json.Marshal(event.Event{ID: changedID(j), Kind: event.TurnChanged, Session: j.Session, Turn: j.Turn,
		Changes: &event.TurnChanges{Count: 99}})
	if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{forged}}, nil); code != 400 {
		t.Fatalf("a runner's turn.changed: %d", code)
	}
	res := queue.Result{Session: j.Session, Changes: changes, Changeset: "blob1", Reply: "done"}
	for range 2 { // the same finish again keeps it once
		if code := e.finish(t, base, queue.Finished, "", res); code != 200 {
			t.Fatalf("finish: %d", code)
		}
	}
	if k := e.kinds(t, j.Session); k[event.TurnChanged] != 1 {
		t.Fatalf("the record holds %v", k)
	}
	j, err := e.q.Get(queue.DefaultTeam, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := queue.ReadResult(j.Result)
	if kept.Changes != nil || kept.ChangesEvent != changedID(j) || kept.Checkpoint != j.Turn || kept.Changeset != "blob1" || kept.Reply != "done" {
		t.Fatalf("the job keeps %s", j.Result)
	}
	got, err := Changes(e.st, j)
	if err != nil || !reflect.DeepEqual(got, EventChanges(changes)) || !got.Isolated || got.Files[0].SHA256Before != "h1" || !got.DiffsCut {
		t.Fatalf("read back %+v, %v", got, err)
	}
	// A job from before 0.5 keeps its changes in its result, read as they are.
	old := queue.Job{Kind: queue.KindTurn, Result: mustJSON(queue.Result{Changes: changes})}
	if got, err := Changes(e.st, old); err != nil || !reflect.DeepEqual(got, EventChanges(changes)) {
		t.Fatalf("an old job: %+v, %v", got, err)
	}
	// A pointer to an event the record lacks is an error, not "no changes".
	lost := j
	lost.Result = mustJSON(queue.Result{ChangesEvent: "evt_tc_none"})
	if _, err := Changes(e.st, lost); err != ErrOutcomeMissing {
		t.Fatalf("a missing event: %v", err)
	}
}

// An apply's finish puts turn.applied into the record: the turns it
// carried, who asked, the result and the refused files. The job keeps only
// the event's id.
func TestTurnAppliedKept(t *testing.T) {
	e := newOutcomeEnv(t)
	by := &queue.By{Kind: event.ByAgent, Session: "s_asker", Turn: "t_asker"}
	links := queue.Links{AppliesTurn: "t_2", AppliesJob: "job_x", AppliesTurns: []string{"t_1", "t_2"}, ApplyBy: by}
	spec := queue.Spec{Session: "s_1", Connector: testConnector, FolderRef: "fld_0123456789abcdef", ApplyTo: "blob2"}
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindApply, "a1", spec, links); err != nil {
		t.Fatal(err)
	}
	e.tok = enrollConnector(t, e.srv)
	j, base := e.take(t, queue.KindApply)
	refused := queue.Result{Refused: "a file in your folder changed", Status: 409,
		Apply: &queue.ApplyResult{Files: []string{}, Conflicts: []queue.ApplyConflict{{Path: "a.txt", Why: "changed since the turn started"}}}}
	if code := e.finish(t, base, queue.Failed, queue.ReasonConflict, refused); code != 200 {
		t.Fatalf("finish: %d", code)
	}
	j, _ = e.q.Get(queue.DefaultTeam, j.ID)
	kept, _ := queue.ReadResult(j.Result)
	if kept.Apply != nil || kept.Refused != "" || kept.AppliedEvent != appliedID(j) {
		t.Fatalf("the job keeps %s", j.Result)
	}
	got, err := Applied(e.st, j)
	want := &event.Applied{Turns: []string{"t_1", "t_2"}, By: &event.By{Kind: event.ByAgent, Session: "s_asker", Turn: "t_asker"},
		Result: "refused", Error: "a file in your folder changed", Conflicts: []event.RefusedFile{{Path: "a.txt", Why: "changed since the turn started"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %+v, %v", got, err)
	}
	ev, ok, err := e.st.EventByID("s_1", "t_2", appliedID(j))
	if err != nil || !ok || ev.Kind != event.TurnApplied {
		t.Fatalf("turn.applied: %+v %v %v", ev, ok, err)
	}
	// One that applied names its files; one from before 0.5 is read from
	// its result.
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindApply, "a2", spec, links); err != nil {
		t.Fatal(err)
	}
	j2, base2 := e.take(t, queue.KindApply)
	if code := e.finish(t, base2, queue.Finished, "", queue.Result{Apply: &queue.ApplyResult{Files: []string{"a.txt"}, Count: 1}}); code != 200 {
		t.Fatalf("finish: %d", code)
	}
	j2, _ = e.q.Get(queue.DefaultTeam, j2.ID)
	if got, err := Applied(e.st, j2); err != nil || got.Result != "applied" || !reflect.DeepEqual(got.Files, []string{"a.txt"}) || got.Count != 1 {
		t.Fatalf("applied: %+v, %v", got, err)
	}
	old := j2
	old.Links = queue.Links{AppliesTurn: "t_2"}
	old.Result = mustJSON(queue.Result{Apply: &queue.ApplyResult{Files: []string{"b.txt"}, Count: 1}})
	if got, err := Applied(e.st, old); err != nil || got.Result != "applied" || got.By != nil || !reflect.DeepEqual(got.Turns, []string{"t_2"}) ||
		!reflect.DeepEqual(got.Files, []string{"b.txt"}) {
		t.Fatalf("an old apply: %+v, %v", got, err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// enrollConnector enrolls the connector of testConnector, which takes apply
// jobs, and answers its token.
func enrollConnector(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	var e Enrolled
	if code := send(t, srv, enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "c", Capacity: 2, Protocol: ProtocolVersion,
		Capabilities: &queue.Capabilities{Connector: testConnector, Role: queue.RoleConnector}}, &e); code != 201 {
		t.Fatalf("enroll: %d", code)
	}
	return e.Token
}
