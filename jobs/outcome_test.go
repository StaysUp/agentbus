package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
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
	s   *Server
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
	startFinisher(t, s)
	return &outcomeEnv{q: q, st: st, srv: srv, tok: enroll(t, srv, 4).Token}
}

// take takes and starts the queue's next job, and answers its attempt's
// path.
func (e *outcomeEnv) take(t *testing.T, kind string) (queue.Job, string) {
	t.Helper()
	var taken TakenWork
	folders := kind == queue.KindApply || kind == queue.KindFolder || kind == queue.KindSnapshot
	if code := send(t, e.srv, e.tok, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion, Folders: folders}, &taken); code != 200 || taken.Job.Kind != kind {
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

// enrollConnector enrolls the runner of the machine testConnector, which takes apply
// jobs, and answers its token.
func enrollConnector(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	var e Enrolled
	if code := send(t, srv, enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "c", Capacity: 2, Protocol: ProtocolVersion,
		Capabilities: &queue.Capabilities{Connector: testConnector}}, &e); code != 201 {
		t.Fatalf("enroll: %d", code)
	}
	return e.Token
}

// failingRecord is a record whose next writes fail: fail of them store
// nothing, after of them store the event and then fail, as a control that
// stops right after the write.
type failingRecord struct {
	*store.Store
	mu          sync.Mutex
	fail, after int
}

func (r *failingRecord) AppendOnce(e event.Event) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return false, errFailingRecord
	}
	if r.after > 0 {
		r.after--
		r.Store.AppendOnce(e)
		return false, errFailingRecord
	}
	return r.Store.AppendOnce(e)
}

func (r *failingRecord) set(fail, after int) {
	r.mu.Lock()
	r.fail, r.after = fail, after
	r.mu.Unlock()
}

var errFailingRecord = errors.New("the record is not written")

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newFailingEnv is an outcomeEnv whose record can fail and whose queue
// runs on clock.
func newFailingEnv(t *testing.T) (*outcomeEnv, *failingRecord, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Now().UTC()}
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), clock.now)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &failingRecord{Store: st}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Record: rec})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	return &outcomeEnv{q: q, st: st, srv: srv, tok: enroll(t, srv, 4).Token, s: s}, rec, clock
}

// up runs e's finisher, as a control that is running, until the answered
// func stops it (a control that stops) or the test ends.
func (e *outcomeEnv) up(t *testing.T) (down func()) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { e.s.RunFinisher(stop); close(done) }()
	down = sync.OnceFunc(func() { close(stop); <-done })
	t.Cleanup(down)
	return down
}

// ended waits up to 10 seconds for job id to end, and answers it.
func (e *outcomeEnv) ended(t *testing.T, id string) queue.Job {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		j, err := e.q.Get(queue.DefaultTeam, id)
		if err == nil && j.Ended != nil {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not end", id)
		}
	}
}

func turnChanges(to string) *workspace.Changes {
	return &workspace.Changes{Kind: workspace.KindSnapshot, Count: 1, From: "cp1", To: to,
		Files: []workspace.Change{{Path: "a.txt", Change: "added", Size: 1, SHA256: "h1"}}}
}

// The record first, the finish second, for every outcome kind. A finish
// whose outcome write fails, before or after the event is stored, leaves
// the job unfinished, never finished with no outcome. The finish is saved:
// its lease running out meanwhile changes nothing, another finish is
// refused, and once control's record works again (here, after a stop and a
// start) its finisher ends the job, with or without the runner sending the
// finish again. Each event is stored once.
func TestOutcomeBeforeFinish(t *testing.T) {
	for _, kind := range []string{queue.KindTurn, queue.KindApply} {
		for _, stop := range []string{"before the event", "after the event"} {
			for _, by := range []string{"runner sending it again", "start alone"} {
				t.Run(kind+" stopped "+stop+", ended at the "+by, func(t *testing.T) {
					e, rec, clock := newFailingEnv(t)
					down := e.up(t)
					var res queue.Result
					switch kind {
					case queue.KindTurn:
						if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
							t.Fatal(err)
						}
						res = queue.Result{Changes: turnChanges("cp2")}
					case queue.KindApply:
						spec := queue.Spec{Session: "s_1", Connector: testConnector, FolderRef: "fld_0123456789abcdef", ApplyTo: "blob2"}
						if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindApply, "a1", spec, queue.Links{AppliesTurn: "t_1"}); err != nil {
							t.Fatal(err)
						}
						e.tok = enrollConnector(t, e.srv)
						res = queue.Result{Apply: &queue.ApplyResult{Files: []string{"a.txt"}, Count: 1}}
					}
					j, base := e.take(t, kind)
					if stop == "before the event" {
						rec.set(1<<30, 0)
					} else {
						rec.set(0, 1<<30)
					}
					if code := e.finish(t, base, queue.Finished, "", res); code != 500 {
						t.Fatalf("finish with the write failing: %d", code)
					}
					if j, _ = e.q.Get(queue.DefaultTeam, j.ID); j.Ended != nil {
						t.Fatalf("the job ended with no outcome in the record: %s %s", j.State, j.Result)
					}
					// The lease runs out while the outcome cannot be written.
					clock.add(time.Hour)
					if _, err := e.q.Sweep(); err != nil {
						t.Fatal(err)
					}
					if j, _ = e.q.Get(queue.DefaultTeam, j.ID); j.Ended != nil {
						t.Fatalf("a saved finish's job expired: %s %s", j.State, j.Reason)
					}
					other := res
					other.Reply = "another"
					if code := e.finish(t, base, queue.Finished, "", other); code != 409 {
						t.Fatalf("another finish: %d", code)
					}
					down() // control stops; when it starts, its record works
					rec.set(0, 0)
					e.up(t)
					switch by {
					case "runner sending it again":
						if code := e.finish(t, base, queue.Finished, "", res); code != 200 {
							t.Fatalf("the same finish again: %d", code)
						}
					}
					j = e.ended(t, j.ID)
					if j.State != queue.Finished {
						t.Fatalf("job %s %s", j.State, j.Reason)
					}
					switch kind {
					case queue.KindTurn:
						if c, err := Changes(e.st, j); err != nil || c == nil || c.Count != 1 {
							t.Fatalf("changes %+v %v", c, err)
						}
						if k := e.kinds(t, j.Session); k[event.TurnChanged] != 1 {
							t.Fatalf("the record holds %v", k)
						}
					case queue.KindApply:
						if a, err := Applied(e.st, j); err != nil || a == nil || a.Result != "applied" {
							t.Fatalf("applied %+v %v", a, err)
						}
						if k := e.kinds(t, "s_1"); k[event.TurnApplied] != 1 {
							t.Fatalf("the record holds %v", k)
						}
					}
					// A finish of a job already finished writes nothing.
					rec.set(1, 0)
					if code := e.finish(t, base, queue.Finished, "", res); code != 200 || rec.fail != 1 {
						t.Fatalf("a finish already accepted: %d, writes %d", code, 1-rec.fail)
					}
				})
			}
		}
	}
}

// A command's finish saved before control stopped, and the runner sending
// it again just as control starts: the start's finisher and the finish
// route meet on the same saved finish. Only the finisher ends it, from the
// saved finish, so the command's command.ran is kept once and no
// command.failed is ever written for a command that ran. 50 rounds.
func TestRestartAndFinishRouteRace(t *testing.T) {
	e, _, _ := newFailingEnv(t)
	for i := range 50 {
		session := fmt.Sprintf("s_%d", i)
		if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindCommand, "", queue.Spec{Session: session, Command: "ls"}, queue.Links{}); err != nil {
			t.Fatal(err)
		}
		j, base := e.take(t, queue.KindCommand)
		ran, _ := json.Marshal(event.Event{ID: "evt_ran_" + j.ID, Kind: event.CommandRan, Time: time.Now().UTC(), Session: session,
			Command: &event.Command{Line: "ls", Stdout: "a\n"}})
		res, _ := json.Marshal(queue.Result{Ran: ran})
		call := FinishCall{Number: 1, Status: queue.Finished, Result: res}
		// Saved before the stop: what the route did then.
		held, err := e.q.Get(queue.DefaultTeam, j.ID)
		if err != nil || len(held.Attempts) != 1 {
			t.Fatalf("round %d: %+v %v", i, held.Attempts, err)
		}
		if _, _, err := e.q.Accept(held.Attempts[0].Runner, held.Attempts[0].ID, 1, call.finish()); err != nil {
			t.Fatal(err)
		}
		got := make(chan int, 1)
		go func() { got <- sendQuiet(e.srv, e.tok, base+"/finish", call) }()
		down := e.up(t) // control starts as the finish arrives
		code := <-got
		j = e.ended(t, j.ID)
		down()
		if code != 200 || j.State != queue.Finished {
			t.Fatalf("round %d: finish %d, job %s %s", i, code, j.State, j.Reason)
		}
		if k := e.kinds(t, session); k[event.CommandRan] != 1 || k[event.CommandFailed] != 0 {
			t.Fatalf("round %d: the record holds %v", i, k)
		}
		if kept, _ := queue.ReadResult(j.Result); kept.RanEvent != "evt_ran_"+j.ID || len(kept.Ran) != 0 {
			t.Fatalf("round %d: the job keeps %s", i, j.Result)
		}
	}
}

// Two different finishes of one attempt sent at once: one is the job's,
// the other is refused, and the record's turn.changed is the one the job
// kept, never the refused one's.
func TestRacingFinishes(t *testing.T) {
	e, _, _ := newFailingEnv(t)
	e.up(t)
	for i := range 20 {
		if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
			t.Fatal(err)
		}
		j, base := e.take(t, queue.KindTurn)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for k, to := range []string{"cp_A", "cp_B"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b, _ := json.Marshal(queue.Result{Changes: turnChanges(to)})
				codes[k] = sendQuiet(e.srv, e.tok, base+"/finish", FinishCall{Number: 1, Status: queue.Finished, Result: b})
			}()
		}
		wg.Wait()
		if !(codes[0] == 200 && codes[1] == 409 || codes[0] == 409 && codes[1] == 200) {
			t.Fatalf("round %d: answers %v, want one 200 and one 409", i, codes)
		}
		j, _ = e.q.Get(queue.DefaultTeam, j.ID)
		kept, _ := queue.ReadResult(j.Result)
		c, err := Changes(e.st, j)
		if err != nil || c == nil || c.To != kept.Checkpoint {
			t.Fatalf("round %d: the job kept %s, the record holds %+v (%v)", i, kept.Checkpoint, c, err)
		}
		if k := e.kinds(t, j.Session); k[event.TurnChanged] != 1 {
			t.Fatalf("round %d: the record holds %v", i, k)
		}
	}
}

// A reader that polls a job while it finishes never sees it ended without
// its outcome in the record.
func TestNoFinishedJobWithoutOutcome(t *testing.T) {
	e, _, _ := newFailingEnv(t)
	e.up(t)
	for i := range 20 {
		if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
			t.Fatal(err)
		}
		j, base := e.take(t, queue.KindTurn)
		stop, seen := make(chan struct{}), make(chan error, 1)
		go func() {
			for {
				select {
				case <-stop:
					seen <- nil
					return
				default:
				}
				got, err := e.q.Get(queue.DefaultTeam, j.ID)
				if err == nil && got.Ended != nil {
					if c, err := Changes(e.st, got); err != nil || c == nil {
						seen <- fmt.Errorf("round %d: ended with no outcome: %v", i, err)
						return
					}
				}
			}
		}()
		if code := e.finish(t, base, queue.Finished, "", queue.Result{Changes: turnChanges("cp2")}); code != 200 {
			t.Fatalf("finish: %d", code)
		}
		close(stop)
		if err := <-seen; err != nil {
			t.Fatal(err)
		}
	}
}

// sendQuiet posts body to path as send does, from any goroutine: it
// answers the status, or 0 when the call fails.
func sendQuiet(srv *httptest.Server, tok, path string, body any) int {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// startFinisher runs s's finisher until the test ends, stopped before the
// queue and the record close.
func startFinisher(t *testing.T, s *Server) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { s.RunFinisher(stop); close(done) }()
	t.Cleanup(func() { close(stop); <-done })
}
