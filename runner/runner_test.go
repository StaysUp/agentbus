package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

const (
	mainTok   = "abm_main"
	enrollTok = "abe_enroll"
	turnsTok  = "abq_framework-turns-token"
)

// fake is a framework that answers POST /api/v0/turns, GET
// /api/v0/turns/{id} and POST /api/v0/turns/{id}/stop the way AgentBus does,
// with the idempotency key. A turn runs until end or stop.
type fake struct {
	mu         sync.Mutex
	turns      map[string]*fakeTurn // by turn id
	byKey      map[string]string    // idempotency key to turn id
	starts     int                  // turns started
	posts      int                  // POST /turns received
	live       int
	maxLive    int
	refuse     string        // when set, POST /turns answers 400 with it
	dropOnce   bool          // the first POST /turns starts a turn and loses its reply
	failFirst  bool          // the first POST /turns starts a turn and answers 500
	busy       bool          // POST /turns answers 429 and starts nothing
	delay      time.Duration // POST /turns answers this late
	stopFail   int           // the first stopFail stops answer 503
	stops      int           // stops received
	getStatus  int           // when set, GET /turns/{id} answers it
	postStatus int           // when set, POST /turns answers it and starts nothing
	stopStatus int           // when set, a stop answers it and changes nothing
	// onStart, when set, runs as the agent would, in the turn's folder,
	// before the reply.
	onStart  func(folder string)
	sessions map[string]string // framework session to its folder
	folders  []string          // the folder each new turn ran in
	bodies   []map[string]any  // each new turn's request
	// replyText, when set, is the text of every turn's last model reply,
	// served by GET /api/v0/turns/{id}/events.
	replyText string
	srv       *httptest.Server
}

type fakeTurn struct {
	id, session, key string
	state            string
	exit             *int
	stopped          bool
}

func newFake(t *testing.T) *fake {
	f := &fake{turns: map[string]*fakeTurn{}, byKey: map[string]string{}, sessions: map[string]string{}}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+turnsTok {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/v0/turns", auth(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.posts++
		delay := f.delay
		if f.postStatus != 0 {
			code := f.postStatus
			f.mu.Unlock()
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "refused"})
			return
		}
		if f.busy {
			f.mu.Unlock()
			w.WriteHeader(429)
			return
		}
		if f.refuse != "" {
			f.mu.Unlock()
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": f.refuse})
			return
		}
		key, _ := b["idempotency_key"].(string)
		existing := false
		id, ok := f.byKey[key]
		if ok {
			existing = true
		} else {
			f.starts++
			id = queue.NewID("t_")
			sess, _ := b["session"].(string)
			folder, _ := b["folder"].(string)
			if sess == "" {
				sess = queue.NewID("s")
				f.sessions[sess] = folder
			} else {
				folder = f.sessions[sess]
			}
			f.folders = append(f.folders, folder)
			f.bodies = append(f.bodies, b)
			if f.onStart != nil {
				f.onStart(folder)
			}
			f.turns[id] = &fakeTurn{id: id, session: sess, key: key, state: "running"}
			f.byKey[key] = id
			f.live++
			f.maxLive = max(f.maxLive, f.live)
		}
		tr := f.turns[id]
		drop := f.dropOnce && !existing
		f.dropOnce = f.dropOnce && existing
		fail := f.failFirst && !existing
		f.failFirst = f.failFirst && existing
		f.mu.Unlock()
		time.Sleep(delay)
		if fail {
			w.WriteHeader(500)
			return
		}
		if drop {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": tr.session, "turn": tr.id, "existing": existing})
	}))
	mux.HandleFunc("GET /api/v0/turns/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.getStatus != 0 {
			w.WriteHeader(f.getStatus)
			return
		}
		tr := f.turns[r.PathValue("id")]
		if tr == nil || r.URL.Query().Get("session") != tr.session {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"store": "st_1", "seq": 1, "turn": map[string]any{
			"id": tr.id, "session": tr.session, "state": tr.state, "exit_code": tr.exit, "stopped": tr.stopped,
			"reply": "done", "model_calls": 1, "cost_usd": 0.01}})
	}))
	mux.HandleFunc("GET /api/v0/turns/{id}/events", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		tr := f.turns[r.PathValue("id")]
		if tr == nil || r.URL.Query().Get("session") != tr.session {
			w.WriteHeader(404)
			return
		}
		// Two pages, so the runner must follow next_after.
		if r.URL.Query().Get("after") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"store": "st_1", "next_after": 2, "events": []map[string]any{
				{"kind": "turn.started", "text": "the prompt"}, {"kind": "model.responded", "text": "an early reply"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"store": "st_1", "next_after": nil, "events": []map[string]any{
			{"kind": "model.responded", "text": f.replyText}, {"kind": "model.responded", "text": ""}}})
	}))
	mux.HandleFunc("POST /api/v0/turns/{id}/stop", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stops++
		if f.stopStatus != 0 {
			w.WriteHeader(f.stopStatus)
			return
		}
		if f.stopFail > 0 {
			f.stopFail--
			w.WriteHeader(503)
			return
		}
		tr := f.turns[r.PathValue("id")]
		if tr == nil || tr.state != "running" {
			w.WriteHeader(409)
			return
		}
		code := 143
		tr.state, tr.exit, tr.stopped = "failed", &code, true
		f.live--
		w.WriteHeader(202)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// end finishes every running turn with exit code 0.
func (f *fake) endAll() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tr := range f.turns {
		if tr.state == "running" {
			code := 0
			tr.state, tr.exit = "finished", &code
			f.live--
			n++
		}
	}
	return n
}

func (f *fake) counts() (starts, posts, live, maxLive int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.posts, f.live, f.maxLive
}

type world struct {
	q      *queue.Queue
	plane  *httptest.Server
	fw     *fake
	cancel context.CancelFunc
	done   chan struct{}
	folder string // a folder for first jobs
	r      *Runner
}

func newWorld(t *testing.T, capacity int, setup func(*fake)) *world {
	t.Helper()
	return newWorldWrap(t, capacity, setup, nil)
}

// newWorldWrap is newWorld with the control plane's handler wrapped by wrap.
func newWorldWrap(t *testing.T, capacity int, setup func(*fake), wrap func(http.Handler) http.Handler) *world {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jobs.New(jobs.Config{Queue: q, Token: mainTok, EnrollToken: enrollTok,
		Lease: 2 * time.Second, Heartbeat: 50 * time.Millisecond, MaxWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler = s
	if wrap != nil {
		h = wrap(s)
	}
	plane := httptest.NewServer(h)
	fw := newFake(t)
	if setup != nil {
		setup(fw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := Enroll(ctx, Config{Queue: plane.URL, EnrollToken: enrollTok, Name: "test", Capacity: capacity,
		Agentbus: fw.srv.URL, AgentbusToken: turnsTok, Poll: 20 * time.Millisecond, StopWait: time.Second, Unreachable: 300 * time.Millisecond, Logf: t.Logf,
		Workdir: filepath.Join(t.TempDir(), "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{q: q, plane: plane, fw: fw, cancel: cancel, done: make(chan struct{}), folder: t.TempDir(), r: r}
	go func() { r.Run(ctx); close(w.done) }()
	t.Cleanup(func() {
		cancel()
		<-w.done
		plane.Close()
		q.Close()
	})
	return w
}

func (w *world) create(t *testing.T, spec queue.Spec) queue.Job {
	t.Helper()
	if spec.Folder == "" && spec.Repo == "" && spec.Session == "" {
		spec.Folder = w.folder
	}
	j, _, err := w.q.Create(queue.DefaultTeam, queue.KindTurn, "", spec, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (w *world) job(t *testing.T, id string) queue.Job {
	t.Helper()
	j, err := w.q.Get(queue.DefaultTeam, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// spec is a first job; create gives it the world's own folder.
var spec = queue.Spec{Agent: "claude", Prompt: "hi", Model: "haiku"}

// Five jobs on a runner of capacity 3: three turns run at once, the rest wait
// in the queue, and every job ends finished with its turn in the result. Each
// turn's idempotency key is its attempt id.
func TestJobsRunAsTurnsWithinCapacity(t *testing.T) {
	w := newWorld(t, 3, nil)
	var ids []string
	for range 5 {
		ids = append(ids, w.create(t, spec).ID)
	}
	waitFor(t, "three turns", func() bool { _, _, live, _ := w.fw.counts(); return live == 3 })
	time.Sleep(200 * time.Millisecond)
	if _, _, live, _ := w.fw.counts(); live != 3 {
		t.Fatalf("%d turns live past capacity 3", live)
	}
	queued, _ := w.q.List(queue.DefaultTeam, queue.Filter{State: queue.Queued}, 10)
	if len(queued) != 2 {
		t.Fatalf("%d queued, want 2", len(queued))
	}
	w.fw.endAll()
	waitFor(t, "two more turns", func() bool { s, _, _, _ := w.fw.counts(); return s == 5 })
	waitFor(t, "every turn", func() bool { w.fw.endAll(); return w.job(t, ids[4]).State == queue.Finished })
	for _, id := range ids {
		waitFor(t, "job "+id, func() bool { return w.job(t, id).State == queue.Finished })
		j := w.job(t, id)
		var res Result
		_ = json.Unmarshal(j.Result, &res)
		if res.Turn == "" || res.Session == "" || res.State != "finished" || res.ExitCode == nil || *res.ExitCode != 0 || len(j.Attempts) != 1 {
			t.Fatalf("job %s: %+v %s", id, j, j.Result)
		}
		w.fw.mu.Lock()
		key := w.fw.turns[res.Turn].key
		w.fw.mu.Unlock()
		if key != j.Attempts[0].ID {
			t.Fatalf("turn key %q, attempt %q", key, j.Attempts[0].ID)
		}
		evs, _ := w.q.Events(queue.DefaultTeam, j.Attempts[0].ID)
		kinds := []string{}
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		if strings.Join(kinds, ",") != "attempt.started,turn.created,turn.ended,workspace.changes" || j.Attempts[0].LastSeq != 4 {
			t.Fatalf("events %v, last_seq %d", kinds, j.Attempts[0].LastSeq)
		}
	}
	if _, _, _, maxLive := w.fw.counts(); maxLive != 3 {
		t.Fatalf("at most %d at once, want 3", maxLive)
	}
}

// A cancel while running stops the turn, and the job ends cancelled.
func TestCancelStopsTheTurn(t *testing.T) {
	w := newWorld(t, 1, nil)
	j := w.create(t, spec)
	waitFor(t, "running", func() bool { return w.job(t, j.ID).State == queue.Running })
	waitFor(t, "a turn", func() bool { _, _, live, _ := w.fw.counts(); return live == 1 })
	if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cancelled", func() bool { return w.job(t, j.ID).State == queue.Cancelled })
	var res Result
	_ = json.Unmarshal(w.job(t, j.ID).Result, &res)
	if !res.Stopped {
		t.Fatalf("result %+v", res)
	}
}

// A turn the framework refuses fails the job with reason setup and its words.
func TestRefusedTurnIsSetupFailure(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.refuse = "agent claude cannot start" })
	j := w.create(t, spec)
	waitFor(t, "failed", func() bool { return w.job(t, j.ID).State == queue.Failed })
	got := w.job(t, j.ID)
	var res Result
	_ = json.Unmarshal(got.Result, &res)
	if got.Reason != queue.ReasonSetup || res.Refused != "agent claude cannot start" || res.Status != 400 {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// The reply to POST /turns is lost after the turn started: the runner sends it
// again with the same key, and the framework starts one turn only.
func TestLostReplyStartsOneTurn(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.dropOnce = true })
	j := w.create(t, spec)
	waitFor(t, "running turn", func() bool { _, posts, live, _ := w.fw.counts(); return posts >= 2 && live == 1 })
	w.fw.endAll()
	waitFor(t, "finished", func() bool { return w.job(t, j.ID).State == queue.Finished })
	if starts, _, _, _ := w.fw.counts(); starts != 1 {
		t.Fatalf("%d turns started, want 1", starts)
	}
}

// A runner shut down mid-turn stops the turn and fails the job with reason
// runner_stopped.
func TestShutdownStopsTurns(t *testing.T) {
	w := newWorld(t, 1, nil)
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { _, _, live, _ := w.fw.counts(); return live == 1 })
	w.cancel()
	<-w.done
	got := w.job(t, j.ID)
	if got.State != queue.Failed || got.Reason != ReasonRunnerStopped {
		t.Fatalf("%+v", got)
	}
	if _, _, live, _ := w.fw.counts(); live != 0 {
		t.Fatal("the turn still runs")
	}
}

func (f *fake) set(fn func(*fake)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func (f *fake) running() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

// A turn create whose outcome was unknown (500 after the turn started), then
// a cancel: the runner finds the turn under the same key and stops it.
func TestCancelAfterUnknownCreateStopsTheTurn(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.failFirst = true })
	j := w.create(t, spec)
	waitFor(t, "a 500 after a start", func() bool { _, posts, live, _ := w.fw.counts(); return posts >= 1 && live == 1 })
	w.fw.set(func(f *fake) { f.busy = true })
	waitFor(t, "a retry", func() bool { _, posts, _, _ := w.fw.counts(); return posts >= 2 })
	if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	w.fw.set(func(f *fake) { f.busy = false })
	waitFor(t, "cancelled", func() bool { return w.job(t, j.ID).State == queue.Cancelled })
	waitFor(t, "the turn stopped", func() bool { return w.fw.running() == 0 })
	if starts, _, _, _ := w.fw.counts(); starts != 1 {
		t.Fatalf("%d turns started, want 1", starts)
	}
}

// A framework create slower than the lease does not lose the lease: the
// heartbeats run on their own.
func TestSlowCreateKeepsTheLease(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.delay = 2500 * time.Millisecond })
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "an end", func() bool { s := w.job(t, j.ID).State; return s == queue.Finished || s == queue.Failed })
	if got := w.job(t, j.ID); got.State != queue.Finished {
		t.Fatalf("%+v", got)
	}
}

// A stop the framework fails is sent again until it is taken.
func TestFailedStopIsRetried(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.stopFail = 2 })
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cancelled", func() bool { return w.job(t, j.ID).State == queue.Cancelled })
	w.fw.mu.Lock()
	stops := w.fw.stops
	w.fw.mu.Unlock()
	if stops < 3 || w.fw.running() != 0 {
		t.Fatalf("%d stops, %d running", stops, w.fw.running())
	}
}

// The reply to start is lost after it committed: the runner sends start again
// and runs the job.
func TestLostStartReplyIsRetried(t *testing.T) {
	var mu sync.Mutex
	dropped := false
	wrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			drop := !dropped && strings.HasSuffix(r.URL.Path, "/start")
			dropped = dropped || drop
			mu.Unlock()
			if drop {
				h.ServeHTTP(httptest.NewRecorder(), r)
				c, _, _ := w.(http.Hijacker).Hijack()
				c.Close()
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	w := newWorldWrap(t, 1, nil, wrap)
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "finished", func() bool { return w.job(t, j.ID).State == queue.Finished })
}

// More events than one batch holds are sent in several batches.
func TestFlushSplitsBatches(t *testing.T) {
	w := newWorld(t, 1, nil)
	plane := w.plane.URL
	r, err := Enroll(context.Background(), Config{Queue: plane, EnrollToken: enrollTok, Name: "flush", Capacity: 1,
		Agentbus: w.fw.srv.URL, AgentbusToken: turnsTok})
	if err != nil {
		t.Fatal(err)
	}
	// Stop the world's own runner first, so this one takes the job.
	w.cancel()
	<-w.done
	time.Sleep(300 * time.Millisecond) // its last long poll ends on the server
	j := w.create(t, spec)
	_, a, ok, _, err := w.q.Take(r.ID(), time.Minute)
	if err != nil || !ok || a.Job != j.ID {
		got, _ := w.q.Get(queue.DefaultTeam, j.ID)
		t.Fatalf("take: %v %v %+v", ok, err, got)
	}
	at := &attempt{r: r, job: j, a: a, next: 1, cancel: make(chan struct{}), stale: make(chan struct{})}
	for range 1201 {
		at.event("x", nil)
	}
	at.flush(context.Background())
	evs, err := w.q.Events(queue.DefaultTeam, a.ID)
	if err != nil || len(evs) != 1201 || len(at.pending) != 0 {
		t.Fatalf("%d stored, %d pending, %v", len(evs), len(at.pending), err)
	}
}

func result(t *testing.T, j queue.Job) Result {
	t.Helper()
	var r Result
	if err := json.Unmarshal(j.Result, &r); err != nil {
		t.Fatalf("result %s: %v", j.Result, err)
	}
	return r
}

// A framework that stops describing a running turn does not hold the job
// forever: after Unreachable the runner stops the turn as best it can and
// fails the job with turn_unrecovered and may_still_run. A refused token
// gives up at once.
func TestUndescribedTurnGivesUp(t *testing.T) {
	for _, status := range []int{404, 503, 401} {
		w := newWorld(t, 1, nil)
		j := w.create(t, spec)
		waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
		w.fw.set(func(f *fake) { f.getStatus = status })
		waitFor(t, "an end", func() bool { return w.job(t, j.ID).State == queue.Failed })
		got := w.job(t, j.ID)
		if got.Reason != ReasonTurnUnrecovered || !result(t, got).MayStillRun {
			t.Fatalf("GET %d: %+v %s", status, got, got.Result)
		}
	}
}

// A shutdown whose stop the framework never takes does not call the turn
// stopped: the job fails with turn_unrecovered and may_still_run.
func TestShutdownWithRefusedStopIsUnrecovered(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.stopFail = 1 << 30 })
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	w.cancel()
	<-w.done
	got := w.job(t, j.ID)
	if got.State != queue.Failed || got.Reason != ReasonTurnUnrecovered || !result(t, got).MayStillRun {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// A cancel whose stop the framework never takes ends turn_unrecovered, not
// cancelled.
func TestCancelWithRefusedStopIsUnrecovered(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.stopFail = 1 << 30 })
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an end", func() bool { return w.job(t, j.ID).State == queue.Failed })
	if got := w.job(t, j.ID); got.Reason != ReasonTurnUnrecovered || !result(t, got).MayStillRun {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// A cancel while creates have an unknown outcome, with the framework then
// out of reach: the job ends turn_unrecovered, not cancelled.
func TestCancelWithUnreachableFrameworkIsUnrecovered(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) { f.failFirst = true })
	j := w.create(t, spec)
	waitFor(t, "a 500 after a start", func() bool { _, posts, live, _ := w.fw.counts(); return posts >= 1 && live == 1 })
	w.fw.srv.Close()
	if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if w.job(t, j.ID).State != queue.Running {
			break
		}
	}
	if got := w.job(t, j.ID); got.State != queue.Failed || got.Reason != ReasonTurnUnrecovered {
		t.Fatalf("%+v", got)
	}
}

// After a create whose outcome was unknown, a refusal (a replaced token, say)
// does not show that no turn started. With or without a cancel, the job fails
// turn_unrecovered with may_still_run, never setup or cancelled, while the
// turn runs on.
func TestRefusalAfterUnknownCreateIsUnrecovered(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		w := newWorld(t, 1, func(f *fake) { f.failFirst = true })
		j := w.create(t, spec)
		waitFor(t, "a 500 after a start", func() bool { _, posts, live, _ := w.fw.counts(); return posts >= 1 && live == 1 })
		if cancel {
			w.fw.set(func(f *fake) { f.busy = true })
			waitFor(t, "a retry", func() bool { _, posts, _, _ := w.fw.counts(); return posts >= 2 })
			if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		w.fw.set(func(f *fake) { f.busy, f.postStatus = false, 401 })
		waitFor(t, "an end", func() bool { return w.job(t, j.ID).State != queue.Running })
		got := w.job(t, j.ID)
		if got.State != queue.Failed || got.Reason != ReasonTurnUnrecovered || !result(t, got).MayStillRun {
			t.Fatalf("cancel %v: %+v %s", cancel, got, got.Result)
		}
		if w.fw.running() != 1 {
			t.Fatalf("cancel %v: the turn should still run in the fake, so the claim matters", cancel)
		}
	}
}

// When the runner gives up on a turn the framework will not describe, the
// result says whether its stop was taken. 202: taken, may still run. 409: the
// framework says it is not running, so it no longer may. 404 or no answer:
// not taken, may still run.
func TestGiveUpRecordsTheStop(t *testing.T) {
	for _, c := range []struct {
		stop          int
		taken, mayRun bool
	}{{0, true, true}, {409, true, false}, {404, false, true}, {503, false, true}} {
		w := newWorld(t, 1, func(f *fake) { f.stopStatus = c.stop })
		j := w.create(t, spec)
		waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
		w.fw.set(func(f *fake) { f.getStatus = 503 })
		waitFor(t, "an end", func() bool { return w.job(t, j.ID).State == queue.Failed })
		got := w.job(t, j.ID)
		res := result(t, got)
		if got.Reason != ReasonTurnUnrecovered || res.StopTaken != c.taken || res.MayStillRun != c.mayRun || res.Turn == "" || res.Session == "" {
			t.Fatalf("stop %d: %+v %s", c.stop, got, got.Result)
		}
	}
}

// A session's first job names a repo and a base: the runner makes a worktree,
// the turn runs in it, and the job's result lists what changed, ignored files
// included. The session's next job runs in the same worktree on the same
// runner. Closing the session removes the worktree; a job's end never does.
func TestWorktreeSession(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	write(t, filepath.Join(repo, "app.txt"), "v1\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")

	turnNo := 0
	w := newWorld(t, 1, func(f *fake) {
		f.onStart = func(folder string) {
			turnNo++
			if turnNo == 1 {
				write(t, filepath.Join(folder, "app.txt"), "v2\n")
				write(t, filepath.Join(folder, "test.log"), "ran")
			} else {
				write(t, filepath.Join(folder, "second.txt"), "two")
			}
		}
	})
	first := w.create(t, queue.Spec{Agent: "claude", Repo: repo, Base: "main", Prompt: "edit"})
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "finished", func() bool { return w.job(t, first.ID).State == queue.Finished })
	got := w.job(t, first.ID)
	var res struct {
		Session string            `json:"session"`
		Folder  string            `json:"folder"`
		Changes workspace.Changes `json:"changes"`
	}
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(w.r.cfg.Workdir, first.ID)
	w.fw.mu.Lock()
	ran := w.fw.folders[0]
	w.fw.mu.Unlock()
	if res.Folder != wantDir || ran != wantDir {
		t.Fatalf("folder %q, turn ran in %q, want %q", res.Folder, ran, wantDir)
	}
	m := byPath(res.Changes)
	if res.Changes.Kind != "git" || res.Changes.Count != 2 || m["app.txt"].Git != "tracked" || m["app.txt"].Change != "modified" ||
		m["test.log"].Git != "ignored" || m["test.log"].Change != "added" {
		t.Fatalf("changes %+v", res.Changes)
	}
	if got.Workspace == nil || got.Workspace.Kind != queue.WorkspaceWorktree || got.Workspace.Runner != w.r.ID() {
		t.Fatalf("workspace %+v", got.Workspace)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Fatal("the worktree was removed at the job's end")
	}

	next := w.create(t, queue.Spec{Session: res.Session, Prompt: "more"})
	waitFor(t, "a second turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "finished", func() bool { return w.job(t, next.ID).State == queue.Finished })
	n := w.job(t, next.ID)
	var res2 struct {
		Changes workspace.Changes `json:"changes"`
	}
	_ = json.Unmarshal(n.Result, &res2)
	w.fw.mu.Lock()
	ran2 := w.fw.folders[1]
	w.fw.mu.Unlock()
	if ran2 != wantDir || res2.Changes.Count != 1 || res2.Changes.Files[0].Path != "second.txt" ||
		res2.Changes.Files[0].Git != "untracked" {
		t.Fatalf("second: ran in %q, changes %+v", ran2, res2.Changes)
	}

	if _, err := w.q.CloseSession(queue.DefaultTeam, res.Session); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removed", func() bool {
		list, _ := w.q.Sessions(queue.DefaultTeam, 10)
		return len(list) == 1 && list[0].Removed != nil
	})
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	if list, _ := w.q.Sessions(queue.DefaultTeam, 10); list[0].RemovalError != "" {
		t.Fatalf("removal error %q", list[0].RemovalError)
	}
}

// A plain folder session: the changes are a snapshot, and closing never
// removes the folder.
func TestFolderSessionNeverRemoved(t *testing.T) {
	w := newWorld(t, 1, func(f *fake) {
		f.onStart = func(folder string) { write(t, filepath.Join(folder, "made.txt"), "x") }
	})
	j := w.create(t, spec)
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "finished", func() bool { return w.job(t, j.ID).State == queue.Finished })
	var res struct {
		Session string            `json:"session"`
		Changes workspace.Changes `json:"changes"`
	}
	_ = json.Unmarshal(w.job(t, j.ID).Result, &res)
	if res.Changes.Kind != "snapshot" || res.Changes.Count != 1 || res.Changes.Files[0].Path != "made.txt" {
		t.Fatalf("%+v", res.Changes)
	}
	if _, err := w.q.CloseSession(queue.DefaultTeam, res.Session); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removed", func() bool {
		list, _ := w.q.Sessions(queue.DefaultTeam, 10)
		return len(list) == 1 && list[0].Removed != nil
	})
	if _, err := os.Stat(filepath.Join(w.folder, "made.txt")); err != nil {
		t.Fatal("a folder the job named was removed")
	}
}

// A base that does not exist fails the job as setup, before any turn.
func TestBadBaseIsSetup(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	w := newWorld(t, 1, nil)
	j := w.create(t, queue.Spec{Agent: "claude", Repo: repo, Base: "main", Prompt: "x"})
	waitFor(t, "failed", func() bool { return w.job(t, j.ID).State == queue.Failed })
	if got := w.job(t, j.ID); got.Reason != queue.ReasonSetup || !strings.Contains(string(got.Result), "worktree could not be made") {
		t.Fatalf("%+v %s", got, got.Result)
	}
	if starts, _, _, _ := w.fw.counts(); starts != 0 {
		t.Fatal("a turn started")
	}
}

// A runner restarted with its state file keeps its id, so its sessions'
// jobs still reach it.
func TestStateFileKeepsIdentity(t *testing.T) {
	w := newWorld(t, 1, nil)
	path := filepath.Join(t.TempDir(), "runner.json")
	cfg := Config{Queue: w.plane.URL, EnrollToken: enrollTok, Name: "s", Agentbus: w.fw.srv.URL, AgentbusToken: turnsTok, StateFile: path}
	one, err := Enroll(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v", err)
	}
	two, err := Enroll(context.Background(), cfg)
	if err != nil || two.ID() != one.ID() {
		t.Fatalf("second start: %v, id %s want %s", err, two.ID(), one.ID())
	}
	// A state file the control plane does not know enrolls anew.
	_ = saveState(path, state{Runner: "run_x", Token: "abr_unknown", HeartbeatMS: 1, LeaseMS: 1})
	three, err := Enroll(context.Background(), cfg)
	if err != nil || three.ID() == one.ID() || three.ID() == "run_x" {
		t.Fatalf("unknown state: %v %s", err, three.ID())
	}
}

// A review of a worktree job: the runner runs it read-only, in the author's
// worktree, as a new session, and the control plane reads the verdict from
// the whole last reply. The author's job recorded a diff of the tracked file
// and the content of the small untracked one, and the prompt carries both.
func TestReviewRunsInTheAuthorsWorktree(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	write(t, filepath.Join(repo, "app.txt"), "v1\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")
	first := true
	w := newWorld(t, 2, func(f *fake) {
		f.onStart = func(folder string) {
			if first {
				first = false
				write(t, filepath.Join(folder, "app.txt"), "v2\n")
				write(t, filepath.Join(folder, "notes.md"), "why v2")
				write(t, filepath.Join(folder, "out.log"), "log")
			}
		}
		f.replyText = "Checked app.txt.\n```json\n{\"verdict\":\"pass\",\"findings\":[{\"severity\":\"P2\",\"file\":\"app.txt\",\"line\":1,\"summary\":\"no test\"}]}\n```"
	})
	author := w.create(t, queue.Spec{Agent: "claude", Repo: repo, Base: "main", Prompt: "Bump app.txt to v2."})
	waitFor(t, "a turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "finished", func() bool { return w.job(t, author.ID).State == queue.Finished })
	var res struct {
		Changes workspace.Changes `json:"changes"`
	}
	_ = json.Unmarshal(w.job(t, author.ID).Result, &res)
	m := byPath(res.Changes)
	if !strings.Contains(res.Changes.Diff, "-v1") || !strings.Contains(res.Changes.Diff, "+v2") || strings.Contains(res.Changes.Diff, "notes.md") ||
		m["notes.md"].Content == nil || *m["notes.md"].Content != "why v2" || m["out.log"].Content == nil || m["app.txt"].Content != nil {
		t.Fatalf("author changes: %+v", res.Changes)
	}

	rev, _, err := w.q.Review(queue.DefaultTeam, author.ID, "claude", "sonnet", "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a review turn", func() bool { return w.fw.running() == 1 })
	w.fw.endAll()
	waitFor(t, "review finished", func() bool { return w.job(t, rev.ID).State == queue.Finished })
	got := w.job(t, rev.ID)
	if got.Verdict == nil || *got.Verdict != queue.VerdictPass || len(got.Findings) != 1 || got.Findings[0].Severity != "P2" {
		t.Fatalf("review: %+v %s", got, got.Result)
	}
	w.fw.mu.Lock()
	body := w.fw.bodies[1]
	w.fw.mu.Unlock()
	wantDir := filepath.Join(w.r.cfg.Workdir, author.ID)
	prompt, _ := body["prompt"].(string)
	if body["folder"] != wantDir || body["read_only"] != true || body["session"] != nil || body["model"] != "sonnet" ||
		!strings.Contains(prompt, "+v2") || !strings.Contains(prompt, "why v2") {
		t.Fatalf("review request: %v", body)
	}
	if list, _ := w.q.Sessions(queue.DefaultTeam, 10); len(list) != 1 {
		t.Fatalf("the review claimed a session: %+v", list)
	}
	if list, _ := w.q.List(queue.DefaultTeam, queue.Filter{Reviews: author.ID}, 10); len(list) != 1 || list[0].ID != rev.ID {
		t.Fatalf("reviews of the author: %d", len(list))
	}
}
