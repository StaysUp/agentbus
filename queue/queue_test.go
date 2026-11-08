package queue

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const lease = 30 * time.Second

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func open(t *testing.T) (*Queue, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	q, err := Open(filepath.Join(t.TempDir(), "jobs.db"), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return q, c
}

func runner(t *testing.T, q *Queue, capacity int) Runner {
	t.Helper()
	r, err := q.RegisterRunner(DefaultTeam, "r", []string{"agent:claude"}, capacity, HashToken(NewID("tok_")))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func job(t *testing.T, q *Queue, key string) Job {
	t.Helper()
	j, _, err := q.Create(DefaultTeam, KindTurn, key, Spec{Agent: "claude", Folder: "/x", Prompt: "hi"}, Links{})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func take(t *testing.T, q *Queue, r Runner) (Job, Attempt) {
	t.Helper()
	j, a, ok, _, err := q.Take(r.ID, lease)
	if err != nil || !ok {
		t.Fatalf("take: ok %v err %v", ok, err)
	}
	return j, a
}

func state(t *testing.T, q *Queue, id string) Job {
	t.Helper()
	j, err := q.Get(DefaultTeam, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// queued → taken → running → finished, with the verdict kept apart from the
// state, and the result on both the job and its attempt.
func TestStates(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 3)
	j := job(t, q, "")
	if j.State != Queued || j.Team != DefaultTeam || j.Verdict != nil {
		t.Fatalf("created: %+v", j)
	}
	tj, a := take(t, q, r)
	if tj.ID != j.ID || tj.State != Taken || a.Number != 1 || a.State != Taken || tj.Attempt != a.ID {
		t.Fatalf("taken: %+v %+v", tj, a)
	}
	sa, cancel, err := q.Start(r.ID, a.ID, 1, lease)
	if err != nil || cancel || sa.State != Running || sa.Started == nil {
		t.Fatalf("start: %+v %v %v", sa, cancel, err)
	}
	if state(t, q, j.ID).State != Running {
		t.Fatal("job not running after start")
	}
	// Start again is the same answer.
	if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
		t.Fatalf("start again: %v", err)
	}
	v := "changes_requested"
	f := Finish{Status: Finished, Verdict: &v, Result: json.RawMessage(`{"turn":"t_1"}`), LastSeq: 0}
	done, err := q.End(r.ID, a.ID, 1, f)
	if err != nil || done.State != Finished || done.Verdict == nil || *done.Verdict != v || string(done.Result) != `{"turn":"t_1"}` ||
		done.Ended == nil || len(done.Attempts) != 1 || done.Attempts[0].State != Finished {
		t.Fatalf("finished: %+v %v", done, err)
	}
	if _, ok, _ := takeAny(q, r); ok {
		t.Fatal("a finished job was taken again")
	}
}

func takeAny(q *Queue, r Runner) (Attempt, bool, error) {
	_, a, ok, _, err := q.Take(r.ID, lease)
	return a, ok, err
}

// A finish sent twice answers the same; a different finish of the same
// attempt is refused; a status that is not an end is refused.
func TestFinishIdempotent(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 1)
	j := job(t, q, "")
	_, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, a.ID, 1, Finish{Status: Running}); !errors.Is(err, ErrBadState) {
		t.Fatalf("status running: %v", err)
	}
	f := Finish{Status: Failed, Reason: ReasonAgent, Result: json.RawMessage(`{"exit_code":1}`)}
	one, err := q.End(r.ID, a.ID, 1, f)
	if err != nil {
		t.Fatal(err)
	}
	two, err := q.End(r.ID, a.ID, 1, f)
	if err != nil || two.State != Failed || two.Reason != ReasonAgent || !two.Updated.Equal(one.Updated) {
		t.Fatalf("again: %+v %v", two, err)
	}
	if _, err := q.End(r.ID, a.ID, 1, Finish{Status: Finished}); !errors.Is(err, ErrFinishConflict) {
		t.Fatalf("another finish: %v", err)
	}
	if state(t, q, j.ID).State != Failed {
		t.Fatal("the job changed")
	}
}

// Every call names its attempt, and a call for an attempt that is not current,
// with the wrong number, or from another runner, is refused and changes
// nothing.
func TestFencing(t *testing.T) {
	q, c := open(t)
	r1, r2 := runner(t, q, 1), runner(t, q, 1)
	j := job(t, q, "")
	_, a1 := take(t, q, r1)
	// The wrong number, and another runner.
	if _, _, err := q.Start(r1.ID, a1.ID, 2, lease); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong number: %v", err)
	}
	if _, _, err := q.Start(r2.ID, a1.ID, 1, lease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another runner: %v", err)
	}
	// The lease runs out while taken: back to the queue, and r2 takes it as
	// attempt 2.
	c.add(lease + time.Second)
	_, a2 := take(t, q, r2)
	if a2.Number != 2 || a2.Job != j.ID {
		t.Fatalf("second attempt: %+v", a2)
	}
	// r1 comes back: every call for attempt 1 is refused.
	if _, _, err := q.Start(r1.ID, a1.ID, 1, lease); !errors.Is(err, ErrStale) {
		t.Fatalf("stale start: %v", err)
	}
	if _, _, err := q.Heartbeat(r1.ID, a1.ID, 1, lease); !errors.Is(err, ErrStale) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if _, err := q.End(r1.ID, a1.ID, 1, Finish{Status: Finished}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale finish: %v", err)
	}
	// Its late events are kept on attempt 1 and say it is not current, even
	// before a sweep: the lease decides.
	if next, cur, err := q.AddEvents(r1.ID, a1.ID, 1, 1, []AttemptEvent{{Kind: "late"}}); err != nil || cur || next != 2 {
		t.Fatalf("late events: %d %v %v", next, cur, err)
	}
	got := state(t, q, j.ID)
	if got.State != Taken || got.Attempt != a2.ID || len(got.Attempts) != 2 || got.Attempts[0].State != Lost {
		t.Fatalf("after stale calls: %+v", got)
	}
	// Attempt 2 runs to its end.
	if _, _, err := q.Start(r2.ID, a2.ID, 2, lease); err != nil {
		t.Fatal(err)
	}
	if j, err := q.End(r2.ID, a2.ID, 2, Finish{Status: Finished}); err != nil || j.State != Finished {
		t.Fatalf("finish 2: %+v %v", j, err)
	}
}

// A lease that runs out while running fails the job with runner_lost, and the
// job is never taken again.
func TestLeaseLostWhileRunning(t *testing.T) {
	q, c := open(t)
	r := runner(t, q, 1)
	j := job(t, q, "")
	_, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	// Heartbeats keep it alive past one lease.
	c.add(20 * time.Second)
	if _, _, err := q.Heartbeat(r.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	c.add(20 * time.Second)
	if n, err := q.Sweep(); err != nil || n != 0 {
		t.Fatalf("sweep with a live lease: %d %v", n, err)
	}
	c.add(lease)
	if n, err := q.Sweep(); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	got := state(t, q, j.ID)
	if got.State != Failed || got.Reason != ReasonRunnerLost || got.Attempts[0].State != Lost || got.Ended == nil {
		t.Fatalf("after the lease: %+v", got)
	}
	if _, ok, _ := takeAny(q, r); ok {
		t.Fatal("a job lost while running was taken again")
	}
	if _, err := q.End(r.ID, a.ID, 1, Finish{Status: Finished}); !errors.Is(err, ErrStale) {
		t.Fatalf("finish after loss: %v", err)
	}
}

// A lease that runs out while taken is settled by the call that finds it, not
// only by Sweep: the expiry is the control plane's clock.
func TestLeaseLostWhileTakenSettledOnCall(t *testing.T) {
	q, c := open(t)
	r := runner(t, q, 1)
	j := job(t, q, "")
	_, a := take(t, q, r)
	c.add(lease)
	if _, _, err := q.Heartbeat(r.ID, a.ID, 1, lease); !errors.Is(err, ErrStale) {
		t.Fatalf("heartbeat at expiry: %v", err)
	}
	if got := state(t, q, j.ID); got.State != Queued || got.Attempt != "" {
		t.Fatalf("after expiry: %+v", got)
	}
}

// Cancel: a queued job ends at once; a running one is marked, the runner hears
// it on its heartbeat and finishes it cancelled; a finish that arrives first
// wins.
func TestCancel(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 2)
	queued := job(t, q, "")
	if got, err := q.Cancel(DefaultTeam, queued.ID); err != nil || got.State != Cancelled || got.Ended == nil {
		t.Fatalf("cancel queued: %+v %v", got, err)
	}
	if _, ok, _ := takeAny(q, r); ok {
		t.Fatal("a cancelled job was taken")
	}

	running := job(t, q, "")
	_, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Cancel(DefaultTeam, running.ID); err != nil || got.State != Running || !got.CancelRequested {
		t.Fatalf("cancel running: %+v %v", got, err)
	}
	if _, cancel, err := q.Heartbeat(r.ID, a.ID, 1, lease); err != nil || !cancel {
		t.Fatalf("heartbeat: cancel %v err %v", cancel, err)
	}
	if got, err := q.End(r.ID, a.ID, 1, Finish{Status: Cancelled}); err != nil || got.State != Cancelled {
		t.Fatalf("finish cancelled: %+v %v", got, err)
	}

	// A cancel after the finish changes nothing.
	raced := job(t, q, "")
	_, b := take(t, q, r)
	if _, _, err := q.Start(r.ID, b.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, b.ID, 1, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Cancel(DefaultTeam, raced.ID); err != nil || got.State != Finished {
		t.Fatalf("cancel after finish: %+v %v", got, err)
	}

	// A cancel of a taken job whose runner then goes quiet ends cancelled, not
	// queued again.
	quiet := job(t, q, "")
	_, c1 := take(t, q, r)
	if _, err := q.Cancel(DefaultTeam, quiet.ID); err != nil {
		t.Fatal(err)
	}
	if _, cancel, err := q.Start(r.ID, c1.ID, 1, lease); err != nil || !cancel {
		t.Fatalf("start of a cancelled job: cancel %v err %v", cancel, err)
	}
	if _, err := q.Cancel("team_other", quiet.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team's cancel: %v", err)
	}
}

// A runner holds at most its capacity of taken and running attempts.
func TestCapacity(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 3)
	for range 5 {
		job(t, q, "")
	}
	var held []Attempt
	for range 3 {
		_, a := take(t, q, r)
		held = append(held, a)
	}
	if _, ok, _ := takeAny(q, r); ok {
		t.Fatal("took a fourth job at capacity 3")
	}
	if _, _, err := q.Start(r.ID, held[0].ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, held[0].ID, 1, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := takeAny(q, r); !ok {
		t.Fatal("no take after a slot freed")
	}
	// Takes at once never pass capacity.
	q2, _ := open(t)
	r2 := runner(t, q2, 3)
	for range 10 {
		job(t, q2, "")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := takeAny(q2, r2); err == nil && ok {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n != 3 {
		t.Fatalf("%d takes at once, want 3", n)
	}
}

// Events are numbered from an offset: a batch sent again stores nothing new,
// a batch that overlaps stores only its new tail, and a batch past the next
// number is refused with the number to send.
func TestEventsIdempotent(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 1)
	job(t, q, "")
	_, a := take(t, q, r)
	batch := []AttemptEvent{{Kind: "a"}, {Kind: "b"}}
	if next, cur, err := q.AddEvents(r.ID, a.ID, 1, 1, batch); err != nil || next != 3 || !cur {
		t.Fatalf("first: %d %v %v", next, cur, err)
	}
	if next, _, err := q.AddEvents(r.ID, a.ID, 1, 1, batch); err != nil || next != 3 {
		t.Fatalf("again: %d %v", next, err)
	}
	if next, _, err := q.AddEvents(r.ID, a.ID, 1, 2, []AttemptEvent{{Kind: "b"}, {Kind: "c"}}); err != nil || next != 4 {
		t.Fatalf("overlap: %d %v", next, err)
	}
	var gap *GapError
	if _, _, err := q.AddEvents(r.ID, a.ID, 1, 9, []AttemptEvent{{Kind: "z"}}); !errors.As(err, &gap) || gap.Next != 4 {
		t.Fatalf("gap: %v", err)
	}
	if _, _, err := q.AddEvents(r.ID, a.ID, 1, 0, batch); !errors.As(err, &gap) {
		t.Fatalf("offset 0: %v", err)
	}
	// A finish names the last number sent; a higher one than stored shows
	// that events are missing.
	if _, _, err := q.Start(r.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	done, err := q.End(r.ID, a.ID, 1, Finish{Status: Finished, LastSeq: 9})
	if err != nil || done.Attempts[0].LastSeq != 3 || done.Attempts[0].ExpectedSeq == nil || *done.Attempts[0].ExpectedSeq != 9 {
		t.Fatalf("finish with last_seq 9: %+v %v", done.Attempts, err)
	}
	evs, err := q.Events(DefaultTeam, a.ID)
	if err != nil || len(evs) != 3 || evs[0].Kind != "a" || evs[1].Kind != "b" || evs[2].Kind != "c" || evs[2].Seq != 3 {
		t.Fatalf("stored: %+v %v", evs, err)
	}
	if _, err := q.Events("team_other", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team: %v", err)
	}
}

// Create with a key: the same fields return the same job, other fields are
// refused, and keys are per team.
func TestCreateIdempotent(t *testing.T) {
	q, _ := open(t)
	one := job(t, q, "k1")
	two, created, err := q.Create(DefaultTeam, KindTurn, "k1", Spec{Agent: "claude", Folder: "/x", Prompt: "hi"}, Links{})
	if err != nil || created || two.ID != one.ID {
		t.Fatalf("retry: %+v %v %v", two, created, err)
	}
	if _, _, err := q.Create(DefaultTeam, KindTurn, "k1", Spec{Agent: "claude", Folder: "/x", Prompt: "other"}, Links{}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("other fields: %v", err)
	}
	if _, _, err := q.Create(DefaultTeam, KindReview, "k1", Spec{Agent: "claude", Folder: "/x", Prompt: "hi"}, Links{}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("other kind: %v", err)
	}
	list, err := q.List(DefaultTeam, Filter{}, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if _, err := q.Get("team_other", one.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team's get: %v", err)
	}
}

// An events call after the lease ran out, before any sweep, does not call the
// attempt current.
func TestEventsAfterLeaseNotCurrent(t *testing.T) {
	q, c := open(t)
	r := runner(t, q, 1)
	job(t, q, "")
	_, a := take(t, q, r)
	c.add(lease)
	if _, cur, err := q.AddEvents(r.ID, a.ID, 1, 1, []AttemptEvent{{Kind: "late"}}); err != nil || cur {
		t.Fatalf("after the lease: current %v err %v", cur, err)
	}
}

// A session's first job ties the session to its runner. Its later jobs go to
// that runner only, with the workspace. Close refuses while a job of it is
// open, then takes no more jobs, and hands the runner a cleanup; removed ends
// it.
func TestSessionAffinityAndClose(t *testing.T) {
	q, _ := open(t)
	a, b := runner(t, q, 2), runner(t, q, 2)
	first, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Agent: "claude", Repo: "/r", Base: "main", Prompt: "one"}, Links{})
	if err != nil {
		t.Fatal(err)
	}
	_, att, ok, _, err := q.Take(a.ID, lease)
	if err != nil || !ok || att.Job != first.ID {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := q.Start(a.ID, att.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{Kind: WorkspaceWorktree, Folder: "/w/job_1", Repo: "/r", Base: "main"}
	done, err := q.End(a.ID, att.ID, 1, Finish{Status: Finished, Result: json.RawMessage(`{"session":"s1"}`), Session: "s1", Workspace: ws})
	if err != nil || done.Workspace == nil || done.Workspace.Runner != a.ID || done.Workspace.Folder != "/w/job_1" {
		t.Fatalf("first: %+v %v", done.Workspace, err)
	}
	next, _, _ := q.Create(DefaultTeam, KindTurn, "", Spec{Session: "s1", Prompt: "two"}, Links{})
	if _, _, ok, _, _ := q.Take(b.ID, lease); ok {
		t.Fatal("another runner took the session's job")
	}
	j, att2, ok, _, err := q.Take(a.ID, lease)
	if err != nil || !ok || j.ID != next.ID || j.Workspace == nil || j.Workspace.Folder != "/w/job_1" {
		t.Fatalf("the session's runner: %+v %v %v", j.Workspace, ok, err)
	}
	if _, err := q.CloseSession(DefaultTeam, "s1"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("close while a job is open: %v", err)
	}
	if _, _, err := q.Start(a.ID, att2.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	// A later job's finish naming the session changes nothing.
	if _, err := q.End(a.ID, att2.ID, 1, Finish{Status: Finished, Session: "s1",
		Workspace: &Workspace{Kind: WorkspaceFolder, Folder: "/elsewhere"}}); err != nil {
		t.Fatal(err)
	}
	w, err := q.CloseSession(DefaultTeam, "s1")
	if err != nil || w.Closed == nil || w.Folder != "/w/job_1" {
		t.Fatalf("close: %+v %v", w, err)
	}
	if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: "s1", Prompt: "three"}, Links{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("a job for a closed session: %v", err)
	}
	if _, _, _, c, _ := q.Take(b.ID, lease); c != nil {
		t.Fatal("another runner got the cleanup")
	}
	_, _, _, c, err := q.Take(a.ID, lease)
	if err != nil || c == nil || c.Session != "s1" || c.Folder != "/w/job_1" {
		t.Fatalf("cleanup: %+v %v", c, err)
	}
	if err := q.Removed(b.ID, "s1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed by another runner: %v", err)
	}
	if err := q.Removed(a.ID, "s1", "busy"); err != nil {
		t.Fatal(err)
	}
	if err := q.Removed(a.ID, "s1", ""); err != nil {
		t.Fatalf("removed again: %v", err)
	}
	if _, _, _, c, _ := q.Take(a.ID, lease); c != nil {
		t.Fatal("a cleanup after removed")
	}
	list, _ := q.Sessions(DefaultTeam, 10)
	if len(list) != 1 || list[0].Removed == nil || list[0].RemovalError != "busy" {
		t.Fatalf("sessions: %+v", list)
	}
	if _, err := q.CloseSession("team_other", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team's close: %v", err)
	}
}

// A job of a session the queue does not know goes to any runner.
func TestUnknownSessionAnyRunner(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 1)
	j, _, _ := q.Create(DefaultTeam, KindTurn, "", Spec{Session: "outside", Prompt: "x"}, Links{})
	got, _, ok, _, err := q.Take(r.ID, lease)
	if err != nil || !ok || got.ID != j.ID || got.Workspace != nil {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
}

// A session's turn jobs run one at a time, on one runner or on two: a next
// turn waits while the one before it is taken or running, and is takeable
// once it ends. A review of the session does not wait.
func TestSessionTurnsOneAtATime(t *testing.T) {
	q, c := open(t)
	r1, r2 := runner(t, q, 2), runner(t, q, 2)
	mk := func() Job {
		c.add(time.Millisecond) // the queue's order is the order they were made
		j, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: "s1", Prompt: "next"}, Links{})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	first, second := mk(), mk()
	j, a := take(t, q, r1)
	if j.ID != first.ID {
		t.Fatalf("took %s, want the first job", j.ID)
	}
	for _, r := range []Runner{r1, r2} {
		if _, ok, err := takeAny(q, r); err != nil || ok {
			t.Fatalf("a second turn of the session was taken while the first is taken: %v", err)
		}
	}
	if _, _, err := q.Start(r1.ID, a.ID, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := takeAny(q, r2); ok {
		t.Fatal("a second turn of the session was taken while the first runs")
	}
	if _, err := q.End(r1.ID, a.ID, 1, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	j, _ = take(t, q, r2)
	if j.ID != second.ID {
		t.Fatalf("took %s, want the second job", j.ID)
	}
	// A review job of the session is not held back by its running turn.
	if _, err := q.db.Exec(`INSERT INTO jobs(id, team, kind, state, request_hash, spec, links, session, created, updated)
		VALUES('job_rev', ?, 'review', 'queued', 'h', '{"prompt":"r"}', '{}', 's1', 1, 1)`, DefaultTeam); err != nil {
		t.Fatal(err)
	}
	if j, _ := take(t, q, r1); j.ID != "job_rev" {
		t.Fatalf("took %s, want the review", j.ID)
	}
}
