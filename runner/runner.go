// Package runner takes jobs from the job queue and runs each one as a turn,
// through package run, in this process. It talks to the queue only through
// the runner protocol over HTTP (package jobs), the same whether the queue is
// in this process (agentbus up) or on another machine (agentbus runner).
//
// One job, one attempt, one turn: the attempt id is the turn id.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// Config is what a runner needs.
type Config struct {
	Queue       string // the queue's service, such as http://127.0.0.1:8787
	EnrollToken string
	Name        string
	Labels      []string
	Capacity    int // jobs at once; 0 means run.DefaultMaxRuns

	// Engine starts and stops the turns, and Store is the record it writes:
	// a turn's end and its reply are read from it.
	Engine *run.Engine
	Store  *store.Store

	// Workdir holds the isolated copies this runner makes, one per session
	// (package isolation).
	Workdir string
	// Settings say which isolation a new session gets. Nil: the defaults.
	Settings settings.Store
	// StateFile keeps the runner's id and token across restarts, so a
	// session's later jobs still find the runner that holds its worktree.
	// Empty enrolls a new runner on every start.
	StateFile string

	// StopWait bounds how long a runner waits for a stopped turn to end
	// before it lets go of the job. Zero means one minute.
	StopWait time.Duration
	HTTP     *http.Client
	Logf     func(format string, args ...any)
}

// ReasonRunnerStopped: the runner was shut down while the job ran, and
// stopped its turn.
const ReasonRunnerStopped = "runner_stopped"

// Runner is one enrolled runner.
type Runner struct {
	cfg       Config
	short     *http.Client // queue calls other than take
	id, token string
	heartbeat time.Duration
	lease     time.Duration
	cleanMu   sync.Mutex      // one workspace removal at a time
	cleaned   map[string]bool // sessions this process already cleaned, under cleanMu
}

// state is what StateFile keeps.
type state struct {
	Runner      string `json:"runner"`
	Token       string `json:"token"`
	HeartbeatMS int64  `json:"heartbeat_ms"`
	LeaseMS     int64  `json:"lease_ms"`
}

// Enroll adds the runner to the control plane.
func Enroll(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Capacity == 0 {
		cfg.Capacity = run.DefaultMaxRuns
	}
	if cfg.StopWait == 0 {
		cfg.StopWait = time.Minute
	}
	if cfg.Engine == nil || cfg.Store == nil {
		return nil, errors.New("runner: an engine and its store are required")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: jobs.MaxWait + 15*time.Second}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Labels == nil {
		cfg.Labels = []string{"agentbus"}
	}
	r := &Runner{cfg: cfg,
		// Shorter than a lease, so one slow call cannot outlast it.
		short: &http.Client{Timeout: 10 * time.Second, Transport: cfg.HTTP.Transport}}
	// A saved identity is used again when the control plane still knows it.
	if st, ok := loadState(cfg.StateFile); ok {
		var rn queue.Runner
		status, err := call(ctx, r.short, cfg.Queue+"/api/v0/runners/"+st.Runner+"/heartbeat", st.Token,
			jobs.RunnerBeat{Capacity: cfg.Capacity}, &rn)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			r.id, r.token = st.Runner, st.Token
			r.heartbeat, r.lease = time.Duration(st.HeartbeatMS)*time.Millisecond, time.Duration(st.LeaseMS)*time.Millisecond
			return r, nil
		}
		if status != http.StatusUnauthorized && status != http.StatusNotFound {
			return nil, fmt.Errorf("runner: the saved runner's heartbeat answered %d", status)
		}
	}
	var e jobs.Enrolled
	status, err := call(ctx, cfg.HTTP, cfg.Queue+"/api/v0/runners", cfg.EnrollToken,
		jobs.EnrollRequest{Name: cfg.Name, Labels: cfg.Labels, Capacity: cfg.Capacity, Protocol: jobs.ProtocolVersion}, &e)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("runner: enroll answered %d", status)
	}
	r.id, r.token = e.Runner.ID, e.Token
	r.heartbeat, r.lease = time.Duration(e.Heartbeat)*time.Millisecond, time.Duration(e.Lease)*time.Millisecond
	if cfg.StateFile != "" {
		if err := saveState(cfg.StateFile, state{e.Runner.ID, e.Token, e.Heartbeat, e.Lease}); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func loadState(path string) (state, bool) {
	var st state
	if path == "" {
		return st, false
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return st, false
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &st) != nil || st.Runner == "" || st.Token == "" {
		return st, false
	}
	return st, true
}

// saveState writes the state file with mode 0600, by rename, so a crash
// never leaves half a file.
func saveState(path string, st state) error {
	b, _ := json.Marshal(st)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return errors.New("runner: the state file could not be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.New("runner: the state file could not be written")
	}
	return nil
}

// ID is the runner's id.
func (r *Runner) ID() string { return r.id }

// Token is the runner's own token, for the settings it reads from the server
// (settings.Remote). It is never logged.
func (r *Runner) Token() string { return r.token }

// Run takes and runs jobs, Capacity at once, until ctx ends. A job still
// running then has its turn stopped and is finished as failed with reason
// runner_stopped.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range r.cfg.Capacity {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx)
		}()
	}
	wg.Wait()
}

func (r *Runner) loop(ctx context.Context) {
	for ctx.Err() == nil {
		var t jobs.TakenWork
		status, err := call(ctx, r.cfg.HTTP, r.cfg.Queue+"/api/v0/runners/"+r.id+"/take", r.token,
			jobs.TakeRequest{WaitMS: jobs.MaxWait.Milliseconds()}, &t)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil || (status != http.StatusOK && status != http.StatusNoContent):
			r.cfg.Logf("take failed: status %d", status)
			sleep(ctx, 2*time.Second)
			continue
		case status == http.StatusNoContent:
			continue
		case t.Cleanup != nil:
			r.clean(ctx, *t.Cleanup)
			continue
		case t.Job == nil || t.Attempt == nil:
			continue
		}
		r.work(ctx, *t.Job, *t.Attempt)
	}
}

// clean removes a closed session's copy or worktree, if this runner made it,
// and tells the control plane. A folder the job named is never removed. A
// removal that fails is reported with its reason, and the copy stays.
func (r *Runner) clean(ctx context.Context, w queue.Workspace) {
	r.cleanMu.Lock()
	defer r.cleanMu.Unlock()
	// Every worker's take can hand out the same cleanup before the first
	// one reports it; the others then have nothing to do.
	if r.cleaned[w.Session] {
		sleep(ctx, 200*time.Millisecond)
		return
	}
	msg := ""
	if w.Kind == queue.WorkspaceWorktree || w.Kind == queue.WorkspaceCopy {
		if err := r.remove(w); err != nil {
			msg = "the " + w.Kind + " could not be removed: " + err.Error()
		}
	}
	status, err := call(context.WithoutCancel(ctx), r.short, r.cfg.Queue+"/api/v0/runners/"+r.id+"/removed", r.token,
		jobs.RemovedReport{Session: w.Session, Error: msg}, nil)
	r.cfg.Logf("session %s closed: workspace %s %s (control plane %d, %v)", w.Session, w.Kind, w.Folder, status, err)
	if err == nil && (status == http.StatusNoContent || status == http.StatusNotFound) {
		if r.cleaned == nil {
			r.cleaned = map[string]bool{}
		}
		r.cleaned[w.Session] = true
	}
}

// remove removes the copy or worktree of w through its isolation backend. A
// worktree made before the backends kept a record has none, and is removed
// as a worktree.
func (r *Runner) remove(w queue.Workspace) error {
	s, err := isolation.Load(r.cfg.Workdir, w.Folder)
	switch {
	case err == nil:
		return isolation.Close(s)
	case w.Kind == queue.WorkspaceWorktree:
		return workspace.RemoveWorktree(r.cfg.Workdir, w.Repo, w.Folder)
	}
	return nil // a copy with no record: nothing of it is left
}

// attempt is one attempt in progress: its events not yet sent, and what its
// heartbeats have heard.
type attempt struct {
	r   *Runner
	job queue.Job
	a   queue.Attempt

	mu      sync.Mutex
	next    int64 // the next event's sequence number
	pending []queue.AttemptEvent
	until   time.Time // the lease, on this machine's clock

	cancel, stale         chan struct{} // closed when a heartbeat hears it
	cancelOnce, staleOnce sync.Once

	// The workspace, once known: its folder, and for a session's first job
	// the workspace to record. In an isolated session, space is its copy and
	// start the checkpoint the turn starts from; otherwise before is the
	// folder's snapshot at the job's start. session is the turn's session
	// once it started.
	folder  string
	ws      *queue.Workspace
	space   *isolation.Space
	start   string
	before  *workspace.Snapshot
	session string
}

func (at *attempt) path(op string) string {
	return at.r.cfg.Queue + "/api/v0/attempts/" + at.a.ID + "/" + op
}

func (at *attempt) event(kind string, data any) {
	b, _ := json.Marshal(data)
	at.mu.Lock()
	defer at.mu.Unlock()
	at.pending = append(at.pending, queue.AttemptEvent{Seq: at.next, Time: time.Now().UTC(), Kind: kind, Data: b})
	at.next++
}

// flush sends the pending events, at most jobs.MaxBatch at a time. A batch
// the control plane already holds, in part or whole, is stored once. A batch
// that fails stays pending for the next flush.
func (at *attempt) flush(ctx context.Context) {
	for {
		at.mu.Lock()
		n := min(len(at.pending), jobs.MaxBatch)
		batch := append([]queue.AttemptEvent(nil), at.pending[:n]...)
		at.mu.Unlock()
		if n == 0 {
			return
		}
		var ack jobs.EventBatchAck
		status, err := call(ctx, at.r.short, at.path("events"), at.r.token,
			jobs.EventBatch{Number: at.a.Number, Offset: batch[0].Seq, Events: batch}, &ack)
		if err != nil || status != http.StatusOK {
			return
		}
		at.mu.Lock()
		at.pending = at.pending[n:]
		at.mu.Unlock()
	}
}

func (at *attempt) lastSeq() int64 {
	at.mu.Lock()
	defer at.mu.Unlock()
	return at.next - 1
}

// finish ends the attempt, retrying a lost reply: the same finish is safe to
// send again.
func (at *attempt) finish(ctx context.Context, f queue.Finish) {
	ctx = context.WithoutCancel(ctx)
	at.addChanges(&f)
	at.flush(ctx)
	f.LastSeq = at.lastSeq()
	for i := range 5 {
		var j queue.Job
		status, err := call(ctx, at.r.short, at.path("finish"), at.r.token, jobs.Finishing(at.a.Number, f), &j)
		if err == nil && status == http.StatusOK {
			at.r.cfg.Logf("job %s attempt %d %s %s", at.job.ID, at.a.Number, f.Status, f.Reason)
			return
		}
		if err == nil && status < 500 {
			at.r.cfg.Logf("job %s finish refused: %d", at.job.ID, status)
			return
		}
		sleep(ctx, time.Duration(i+1)*time.Second)
	}
}

// addChanges puts what the job changed in its workspace into f's result, and
// for a session's first job the session and its workspace, so the session's
// later jobs come to this runner.
func (at *attempt) addChanges(f *queue.Finish) {
	if at.before == nil && at.space == nil {
		return
	}
	res, _ := queue.ReadResult(f.Result)
	c, ok := at.changes()
	if !ok {
		c = workspace.Changes{Kind: "snapshot", Files: []workspace.Change{}, Error: "the workspace could not be read at the end"}
	}
	res.Folder, res.Changes = at.folder, nil
	// The whole finish must fit the jobs API's body limit, so the changes get
	// what the rest leaves, less a margin for the fields added after this.
	f.Result = mustJSON(res)
	c.Fit(jobs.MaxBody - len(mustJSON(f)) - finishMargin)
	res.Changes = &c
	f.Result = mustJSON(res)
	at.event("workspace.changes", map[string]any{"count": c.Count, "kind": c.Kind})
	// A review runs in another session's workspace: it never claims one.
	if at.ws != nil && at.session != "" && at.job.Kind == queue.KindTurn {
		f.Session, f.Workspace = at.session, at.ws
	}
}

// changes is what the job changed: in an isolated session, from the turn's
// start checkpoint to a checkpoint taken now, named by the turn; otherwise
// from the folder's snapshot at the start.
func (at *attempt) changes() (workspace.Changes, bool) {
	if at.space != nil {
		b, _ := isolation.Get(at.space.Backend)
		end, err := b.Checkpoint(at.space, at.a.ID)
		if err != nil {
			return workspace.Changes{}, false
		}
		c, err := b.Diff(at.space, at.start, end.Name)
		return c, err == nil
	}
	after, err := workspace.Take(at.folder, at.before)
	if err != nil {
		return workspace.Changes{}, false
	}
	return workspace.Diff(at.folder, at.before, after), true
}

// heartbeats renews the lease every heartbeat interval until done closes, on
// its own goroutine, so a slow framework call never holds one up. It closes
// cancel when the control plane asks the job to stop, and stale when the
// attempt is no longer current or the control plane has been out of reach
// for a whole lease.
func (at *attempt) heartbeats(ctx context.Context, done <-chan struct{}) {
	t := time.NewTicker(at.r.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		var b jobs.AttemptBeat
		var e jobs.JobError
		status, err := callErr(context.WithoutCancel(ctx), at.r.short, at.path("heartbeat"), at.r.token,
			jobs.AttemptCall{Number: at.a.Number}, &b, &e)
		switch {
		case err == nil && status == http.StatusOK:
			at.mu.Lock()
			at.until = time.Now().Add(at.r.lease)
			at.mu.Unlock()
			if b.Cancel {
				at.cancelOnce.Do(func() { close(at.cancel) })
			}
		case err == nil && (e.Stale || status == http.StatusNotFound):
			at.staleOnce.Do(func() { close(at.stale) })
			return
		default:
			at.mu.Lock()
			gone := time.Now().After(at.until)
			at.mu.Unlock()
			if gone {
				at.staleOnce.Do(func() { close(at.stale) })
				return
			}
		}
	}
}

func (r *Runner) work(ctx context.Context, job queue.Job, a queue.Attempt) {
	at := &attempt{r: r, job: job, a: a, next: 1, until: time.Now().Add(r.lease),
		cancel: make(chan struct{}), stale: make(chan struct{})}
	// start is safe to send again: a lost reply is retried until the lease
	// must have run out. A stale answer means the job is no longer ours.
	var st jobs.AttemptStarted
	for {
		var e jobs.JobError
		status, err := callErr(context.WithoutCancel(ctx), r.short, at.path("start"), r.token, jobs.AttemptCall{Number: a.Number}, &st, &e)
		if err == nil && status == http.StatusOK {
			break
		}
		if (err == nil && status < 500) || time.Now().After(at.until) {
			r.cfg.Logf("job %s start failed: %d", job.ID, status)
			return
		}
		sleep(ctx, time.Second)
	}
	at.mu.Lock()
	at.until = time.Now().Add(r.lease)
	at.mu.Unlock()
	done := make(chan struct{})
	defer close(done)
	go at.heartbeats(ctx, done)

	at.event("attempt.started", map[string]any{"runner": r.id})
	if st.Cancel {
		at.finish(ctx, queue.Finish{Status: queue.Cancelled})
		return
	}
	if job.Kind == queue.KindApply {
		at.finish(ctx, r.apply(at))
		return
	}
	if refused := r.prepare(at); refused != nil {
		at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: mustJSON(refused)})
		return
	}
	turn, err := r.cfg.Engine.Start(r.request(at))
	if err != nil {
		res := queue.Result{Refused: "the turn could not be started", Status: http.StatusInternalServerError}
		var rf *run.Refusal
		if errors.As(err, &rf) {
			res.Refused, res.Status = rf.Msg, rf.Status
		}
		// No turn started, so no session owns a copy made for it.
		if at.ws != nil && at.space != nil {
			if err := isolation.Close(at.space); err == nil {
				at.event("workspace.removed", map[string]any{"folder": at.ws.Folder})
			}
			at.space, at.before = nil, nil
		}
		at.event("turn.refused", map[string]any{"status": res.Status})
		at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: mustJSON(res)})
		return
	}
	at.session = turn.Session
	at.event("turn.created", map[string]any{"session": turn.Session, "turn": turn.ID})
	at.flush(ctx)
	r.follow(ctx, at, turn)
}

// apply puts the changes of the turn an apply job names into the person's
// folder, through its session's isolation backend. A conflict finishes as
// failed with reason conflict and each file; nothing is written then.
func (r *Runner) apply(at *attempt) queue.Finish {
	sp := at.job.Spec
	refuse := func(msg string, written []string) queue.Finish {
		res := queue.Result{Refused: msg, Status: http.StatusConflict}
		if len(written) > 0 {
			res.Apply = &queue.ApplyResult{Files: written}
		}
		return applyFinish(queue.Failed, queue.ReasonSetup, res)
	}
	if at.job.Workspace == nil {
		return refuse(applyNoCopy, nil)
	}
	s, err := isolation.Load(r.cfg.Workdir, at.job.Workspace.Folder)
	if err != nil {
		return refuse(applyNoCopy, nil)
	}
	b, ok := isolation.Get(s.Backend)
	if !ok {
		return refuse(applyNoCopy, nil)
	}
	done, err := b.Apply(s, sp.ApplyFrom, sp.ApplyTo)
	var cf *isolation.Conflict
	switch {
	case errors.As(err, &cf):
		msg := cf.Error()
		if len(msg) > maxApplyMessage {
			msg = fmt.Sprintf("apply refused: %d file(s) in your folder changed since the checkpoint; conflicts lists them", len(cf.Files))
		}
		res := queue.Result{Refused: msg, Status: http.StatusConflict, Apply: &queue.ApplyResult{Files: []string{}}}
		for _, f := range cf.Files {
			res.Apply.Conflicts = append(res.Apply.Conflicts, queue.ApplyConflict{Path: f.Path, Why: f.Why})
		}
		at.event("workspace.apply_refused", map[string]any{"turn": at.job.Links.AppliesTurn, "conflicts": len(cf.Files)})
		return applyFinish(queue.Failed, queue.ReasonConflict, res)
	case errors.Is(err, isolation.ErrNoCheckpoint):
		return refuse("the turn's checkpoints are no longer kept, so its changes cannot be applied", nil)
	case errors.Is(err, isolation.ErrNoSource):
		return refuse("this session's copy was made from a repo and a base, not from a folder of yours, so there is nowhere to apply", nil)
	case errors.Is(err, isolation.ErrTruncated):
		return refuse("the folder has more files than a checkpoint holds, so the turn cannot be applied", nil)
	case err != nil:
		// The backend's words go to the service log only: they can name
		// paths on this runner.
		r.cfg.Logf("the apply of turn %s could not be written: %s", at.job.Links.AppliesTurn, err)
		return refuse("the changes could not all be written to your folder; files lists those that were", done.Files)
	}
	at.event("workspace.applied", map[string]any{"turn": at.job.Links.AppliesTurn, "files": len(done.Files)})
	return applyFinish(queue.Finished, "", queue.Result{Apply: &queue.ApplyResult{Files: done.Files, Pruned: done.Pruned}})
}

// maxApplyMessage bounds a refusal's message; past it, the message gives the
// count and conflicts lists the files.
const maxApplyMessage = 4 << 10

// applyFinish is an apply job's finish, its result sized so the finish fits
// the jobs API's body limit.
func applyFinish(status, reason string, res queue.Result) queue.Finish {
	if res.Apply != nil {
		res.Apply.Count = len(res.Apply.Files)
		res.Apply.Fit(jobs.MaxBody - finishMargin - len(res.Refused) - 1024)
	}
	return queue.Finish{Status: status, Reason: reason, Result: mustJSON(res)}
}

// applyNoCopy: the session keeps no isolated copy on this runner.
const applyNoCopy = "this session keeps no isolated copy, so there is nothing to apply"

// request is the turn the attempt runs. Its id is the attempt's. A turn
// job's first turn opens the session the queue named for it; a review opens
// one of its own, read-only, in the reviewed job's folder.
func (r *Runner) request(at *attempt) run.Request {
	sp := at.job.Spec
	req := run.Request{Turn: at.a.ID, Prompt: sp.Prompt, Model: sp.Model, ReadOnly: sp.ReadOnly,
		IdempotencyKey: at.job.IdempotencyKey}
	switch {
	case sp.Session != "":
		// A next turn may name another agent: it runs on that one.
		req.Session = sp.Session
		req.Harness, req.AgentID, req.Access, req.KeyEnv, req.AgentModel = sp.Harness, sp.AgentID, sp.Access, sp.KeyEnv, sp.AgentModel
	default:
		req.Harness, req.Folder = sp.Harness, at.folder
		req.AgentID, req.Access, req.KeyEnv = sp.AgentID, sp.Access, sp.KeyEnv
		if at.job.Kind == queue.KindTurn {
			req.NewSession = at.job.Session
		}
	}
	// Work an agent asked for, and a delivered result, say so on their
	// turn.started, and place the turn's token in its request tree.
	l := at.job.Links
	if l.Chain != nil {
		req.Root, req.Depth = l.Chain.Root, l.Chain.Depth
	}
	switch {
	case l.ResultOf != nil:
		req.Origin = &event.Origin{Kind: event.OriginResult, Session: l.ResultOf.Session, Turn: l.ResultOf.Turn,
			Job: l.ResultOf.Job, JobKind: l.ResultOf.Kind, Agent: l.ResultOf.Agent}
	case l.Asker != nil:
		req.Origin = &event.Origin{Kind: event.OriginAsked, Session: l.Asker.Session, Turn: l.Asker.Turn}
	}
	return req
}

// prepare finds or makes the job's workspace and records where its turn
// starts. A session's first job gets an isolated copy (package isolation): a
// worktree of the job's repo at its base, or, for a folder with Isolate, the
// copy the isolation setting names. A folder without Isolate, or with the
// setting none, is used as it is. A later job runs in its session's
// workspace. It answers why the job cannot run, or nil.
func (r *Runner) prepare(at *attempt) *queue.Result {
	sp := at.job.Spec
	switch {
	case sp.Repo != "":
		b, _ := isolation.Get(isolation.Git)
		return r.isolate(at, b, isolation.Request{Repo: sp.Repo, Base: sp.Base})
	case sp.Folder != "":
		if info, err := os.Stat(sp.Folder); err != nil || !info.IsDir() {
			return nil // the engine refuses it, in its own words
		}
		if sp.Isolate {
			p, err := r.policy()
			if err != nil {
				return &queue.Result{Refused: "the permissions cannot be read, so the turn did not start; fix them and send the turn again"}
			}
			if b, ok := isolation.For(p.Isolation, sp.Folder); ok {
				return r.isolate(at, b, isolation.Request{Folder: sp.Folder, Share: p.ShareReadOnly})
			}
		}
		at.folder = sp.Folder
		at.ws = &queue.Workspace{Kind: queue.WorkspaceFolder, Folder: sp.Folder}
	case at.job.Workspace != nil:
		at.folder = at.job.Workspace.Folder
		if k := at.job.Workspace.Kind; k == queue.WorkspaceWorktree || k == queue.WorkspaceCopy {
			// A worktree made before the backends kept a record has none:
			// its turns are diffed from a snapshot, as before.
			if s, err := isolation.Load(r.cfg.Workdir, at.folder); err == nil {
				at.space = s
				return r.checkpointStart(at)
			}
		}
	default:
		// A session the queue holds no workspace for: the record knows its
		// folder.
		if c, err := r.cfg.Store.Continuation(sp.Session); err == nil && c.Known {
			at.folder = c.Folder
		}
	}
	return r.snapshot(at)
}

// policy is the settings' policy, or the defaults with no settings.
func (r *Runner) policy() (settings.Policy, error) {
	if r.cfg.Settings == nil {
		return settings.DefaultPolicy(), nil
	}
	p, _, err := r.cfg.Settings.Policy()
	return p, err
}

// isolate opens a session's first job's copy with b, and its turn starts
// from the copy's first checkpoint.
func (r *Runner) isolate(at *attempt, b isolation.Backend, req isolation.Request) *queue.Result {
	if r.cfg.Workdir == "" {
		return &queue.Result{Refused: "this runner has no workdir for isolated copies"}
	}
	req.ID, req.Workdir = at.job.ID, r.cfg.Workdir
	s, err := b.Open(req)
	var rf *isolation.Refusal
	switch {
	case errors.As(err, &rf):
		return &queue.Result{Refused: rf.Msg, Status: http.StatusBadRequest}
	case err != nil:
		// The backend's own words go to the service log only: they can name
		// paths on this runner.
		r.cfg.Logf("the %s for job %s could not be made: %s", b.Name(), at.job.ID, err)
		if b.Name() == isolation.Git {
			return &queue.Result{Refused: "the worktree could not be made"}
		}
		return &queue.Result{Refused: "the copy of the folder could not be made"}
	}
	if s.Fallback != "" {
		// Not an error: the session runs, in a copy made another way.
		r.cfg.Logf("job %s: %s", at.job.ID, s.Fallback)
	}
	kind := queue.WorkspaceCopy
	if s.Backend == isolation.Git {
		kind = queue.WorkspaceWorktree
	}
	at.space, at.folder = s, s.Dir
	at.ws = &queue.Workspace{Kind: kind, Folder: s.Dir, Repo: s.Repo, Base: s.Base}
	if res := r.checkpointStart(at); res != nil {
		// Nothing records the copy yet, so nothing else would remove it.
		if err := isolation.Close(s); err != nil {
			r.cfg.Logf("the %s for job %s could not be removed after a refused setup", b.Name(), at.job.ID)
		}
		at.space, at.ws = nil, nil
		return res
	}
	ready := map[string]any{"kind": kind, "folder": s.Dir}
	if s.Base != "" {
		ready["base"] = s.Base
	}
	if s.Source != "" {
		ready["from"] = s.Source
	}
	if s.Method != "" {
		ready["method"] = s.Method
	}
	if s.Fallback != "" {
		ready["fallback"] = s.Fallback
	}
	at.event("workspace.ready", ready)
	return nil
}

// checkpointStart takes the checkpoint the turn starts from. Nothing changed
// since the last turn ended, as is usual, costs a walk of the folder and
// records nothing.
func (r *Runner) checkpointStart(at *attempt) *queue.Result {
	b, ok := isolation.Get(at.space.Backend)
	if !ok {
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	c, err := b.Checkpoint(at.space, at.a.ID+".start")
	if err != nil {
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	at.start = c.Name
	return nil
}

// snapshot takes the attempt's snapshot of its folder before the turn.
func (r *Runner) snapshot(at *attempt) *queue.Result {
	if at.folder == "" {
		return nil // no folder to watch: the engine still runs the turn, or refuses it
	}
	b, err := workspace.Take(at.folder, nil)
	if err != nil {
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	at.before = b
	return nil
}

// follow waits for the turn to end, then finishes the attempt with how it
// ended. A cancel or a shutdown stops the turn. A stale attempt stops the
// turn and finishes nothing: the queue has already settled it.
func (r *Runner) follow(ctx context.Context, at *attempt, turn *run.Turn) {
	quit, cancelCh := ctx.Done(), (<-chan struct{})(at.cancel)
	cancelled, shutdown := false, false
	var stopWait <-chan time.Time // set once a stop is sent
	stop := func(why string) {
		at.event("turn.stop", map[string]any{"why": why, "running": r.cfg.Engine.Stop(turn.ID)})
		if stopWait == nil {
			stopWait = time.After(r.cfg.StopWait)
		}
	}
wait:
	for {
		select {
		case <-turn.Done():
			break wait
		case <-quit:
			// The runner is stopping, as the service is: the turn is
			// interrupted, not stopped by a person, so its next turn says
			// its agent was stopped.
			quit, shutdown = nil, true
			at.event("turn.stop", map[string]any{"why": ReasonRunnerStopped, "running": r.cfg.Engine.Interrupt(turn.ID)})
			if stopWait == nil {
				stopWait = time.After(r.cfg.StopWait)
			}
		case <-cancelCh:
			cancelCh, cancelled = nil, true
			stop("cancel")
		case <-at.stale:
			stop("stale")
			select {
			case <-turn.Done():
			case <-time.After(r.cfg.StopWait):
			}
			r.cfg.Logf("job %s attempt %d is no longer current; its turn was stopped", at.job.ID, at.a.Number)
			return
		case <-stopWait:
			// The stop's SIGKILL came and went and the turn has not ended:
			// its session.closed waits on something. The job ends now and
			// says so; the turn ends on its own.
			at.event("turn.unended", map[string]any{"turn": turn.ID})
			res := queue.Result{Session: turn.Session, Error: "the turn was stopped and had not ended in time"}
			f := queue.Finish{Status: queue.Failed, Reason: ReasonRunnerStopped, Result: mustJSON(res)}
			if cancelled && !shutdown {
				f = queue.Finish{Status: queue.Cancelled, Result: mustJSON(res)}
			}
			at.finish(ctx, f)
			return
		}
	}
	end := turn.Ended()
	res := queue.Result{Session: turn.Session, ExitCode: &end.ExitCode, Stopped: end.Stopped, Unsettled: end.Unsettled, State: "failed"}
	if end.ExitCode == 0 {
		res.State = "finished"
	}
	if list, err := r.cfg.Store.Turns(store.TurnQuery{Session: turn.Session, Turn: turn.ID, Limit: 1}); err == nil && len(list.Turns) > 0 {
		t := list.Turns[0]
		cost := t.CostUSD
		res.State, res.Error, res.Reply, res.ModelCalls, res.CostUSD = t.State, t.Error, t.Reply, t.ModelCalls, &cost
	}
	// A review's verdict is read from its whole reply; asked work's reply is
	// delivered to the session that asked (queue.Deliver).
	if at.job.Kind == queue.KindReview || at.job.Links.Asker != nil {
		res.ReplyText = r.lastReply(turn.Session, turn.ID)
	}
	at.event("turn.ended", map[string]any{"state": res.State, "exit_code": end.ExitCode})
	f := queue.Finish{Result: mustJSON(res)}
	switch {
	case shutdown:
		f.Status, f.Reason = queue.Failed, ReasonRunnerStopped
	case cancelled:
		f.Status = queue.Cancelled
	case res.State == "finished":
		f.Status = queue.Finished
	default:
		f.Status, f.Reason = queue.Failed, queue.ReasonAgent
	}
	at.finish(ctx, f)
}

// MaxReplyText bounds a review's reply text in its result.
const MaxReplyText = 256 << 10

// lastReply is the text of the turn's last model reply that has text, read
// from the record a page at a time. "" when none can be read.
func (r *Runner) lastReply(session, turnID string) string {
	f := store.Filter{Turn: turnID, TurnSession: session}
	var after int64
	last := ""
	for range 1000 {
		recs, more, err := r.cfg.Store.Page(after, f, 1000, store.PageBytes)
		if err != nil {
			return last
		}
		for _, rec := range recs {
			after = rec.Seq
			if rec.Kind != event.ModelResponded {
				continue
			}
			var e struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(rec.Data, &e) == nil && e.Text != "" {
				last = e.Text
			}
		}
		if !more {
			break
		}
	}
	if len(last) > MaxReplyText {
		// The verdict block ends the reply, so the end is what is kept,
		// cut on a whole character.
		last = strings.ToValidUTF8(last[len(last)-MaxReplyText:], "")
	}
	return last
}

// finishMargin is room in a finish body for what is set after the changes
// are sized: the session, its workspace and the last event sequence.
const finishMargin = 16 << 10

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// call POSTs body as JSON (or GETs with a nil body to a path that takes
// none), and decodes a 2xx reply into out.
func call(ctx context.Context, c *http.Client, u, token string, body, out any) (int, error) {
	return callErr(ctx, c, u, token, body, out, nil)
}

// callErr is call that also decodes an error reply into errOut.
func callErr(ctx context.Context, c *http.Client, u, token string, body, out, errOut any) (int, error) {
	method, rd := http.MethodGet, io.Reader(nil)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		method, rd = http.MethodPost, bytes.NewReader(b)
	} else if out == nil {
		method = http.MethodPost // a POST with no body, such as a stop
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, errors.New("runner: the request could not be sent")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, errors.New("runner: the reply could not be read")
	}
	if resp.StatusCode/100 == 2 {
		if out != nil && len(b) > 0 {
			if err := json.Unmarshal(b, out); err != nil {
				return resp.StatusCode, errors.New("runner: the reply is not the expected JSON")
			}
		}
	} else if errOut != nil {
		_ = json.Unmarshal(b, errOut)
	}
	return resp.StatusCode, nil
}
