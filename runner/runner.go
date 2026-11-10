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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
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

	// Role is queue.RoleConnector for a connector, which takes only snapshot
	// and apply jobs and does them through FolderSide; empty for a runner.
	Role       string
	FolderSide FolderSide
	// Folders are the folders of the connector on this runner's machine:
	// a connector reports them; a runner that has them uses the local fast
	// path for them (a copy by clone or overlay, and a folder used as it
	// is), reading their base content through Lender. Nil: none here.
	Folders *folders.Registry
	Lender  isolation.Lender
	// Blobs reads blobs for a copy built from a snapshot: the runner's own
	// store with the control plane's behind it, or in agentbus up the
	// control plane's own. BlobSink is the control plane's store, where a
	// turn's changeset and the files it names go. Nil: a runner without
	// them can only use the local fast path, and records no changeset.
	Blobs    isolation.BlobSource
	BlobSink blobs.Sink
	// LookPath and Environ are how the runner probes its harnesses for its
	// report (caps.go). Nil LookPath probes nothing.
	LookPath func(string) (string, error)
	Environ  func() []string

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
	switch {
	case cfg.Role == queue.RoleConnector && (cfg.FolderSide == nil || cfg.Folders == nil):
		return nil, errors.New("runner: a connector needs its folders")
	case cfg.Role != queue.RoleConnector && (cfg.Engine == nil || cfg.Store == nil):
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
			jobs.RunnerBeat{Capacity: cfg.Capacity, Labels: cfg.Labels, Capabilities: r.caps(nil)}, &rn)
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
		jobs.EnrollRequest{Name: cfg.Name, Labels: cfg.Labels, Capacity: cfg.Capacity, Protocol: jobs.ProtocolVersion,
			Capabilities: r.caps(nil)}, &e)
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
	// What a runner here left mounted, if it did not stop cleanly, is
	// unmounted first; each session mounts its copy again when it next
	// runs. Every mount goes when this runner stops.
	r.unmount("start")
	defer r.unmount("stop")
	var wg sync.WaitGroup
	wg.Add(1)
	go r.report(ctx, &wg)
	for range r.cfg.Capacity {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx)
		}()
	}
	wg.Wait()
}

// unmount unmounts every overlay copy in the runner's workdir. A copy's
// files stay where they are.
func (r *Runner) unmount(when string) {
	if r.cfg.Workdir == "" {
		return
	}
	n, err := isolation.Unmount(r.cfg.Workdir)
	if n > 0 {
		r.cfg.Logf("isolation.unmounted n=%d when=%s", n, when)
	}
	if err != nil {
		r.cfg.Logf("isolation.unmount_failed when=%s error=%q", when, err.Error())
	}
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
	r.cfg.Logf("session %s closed: workspace %s %s (control plane %d, %v)", w.Session, w.Kind, w.Space+w.Folder, status, err)
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
	s, err := r.loadSpace(w)
	switch {
	case err == nil:
		return isolation.Close(s)
	case w.Kind == queue.WorkspaceWorktree:
		return isolation.RemoveWorktree(r.cfg.Workdir, w.Repo, w.Folder)
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
	// the workspace to record. In an isolated session, space is its copy;
	// for a folder used as it is, watch checkpoints it for this job only
	// (isolation.None). start is the checkpoint the turn starts from, in
	// either. session is the turn's session once it started. ref is the
	// folder reference of a session's first job, for the record.
	folder  string
	ref     string
	ws      *queue.Workspace
	space   *isolation.Space
	watch   *isolation.Space
	start   string
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
	at.addChanges(ctx, &f)
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
func (at *attempt) addChanges(ctx context.Context, f *queue.Finish) {
	if at.watch == nil && at.space == nil {
		return
	}
	res, _ := queue.ReadResult(f.Result)
	c, ok := at.changes()
	if !ok {
		c = workspace.Changes{Kind: workspace.KindSnapshot, Files: []workspace.Change{}, Error: "the workspace could not be read at the end"}
	}
	// What the copy holds now goes to the control plane's store, so an
	// apply reads it there, not from this runner.
	if ok && at.space != nil && at.job.Kind == queue.KindTurn {
		if h, err := at.r.publish(ctx, at.space, c.To); err != nil {
			at.r.cfg.Logf("job %s: the turn's changes could not be put in the store: %s", at.job.ID, err)
		} else {
			res.Changeset = h
		}
	}
	res.Folder, res.Changes = "", nil
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

// changes is what the job changed: from the turn's start checkpoint to a
// checkpoint taken now, named by the turn, in the session's copy or in the
// folder used as it is. It is the same diff GET /turns/{id}/diff shows.
func (at *attempt) changes() (workspace.Changes, bool) {
	s := at.space
	if s == nil {
		s = at.watch
	}
	b, ok := isolation.Get(s.Backend)
	if !ok {
		return workspace.Changes{}, false
	}
	end, err := b.Checkpoint(s, at.a.ID)
	if err != nil {
		return workspace.Changes{}, false
	}
	c, err := b.Diff(s, at.start, end.Name)
	return c, err == nil
}

// unwatch removes the record of a folder used as it is, once the job is
// done with it. The folder itself is never touched.
func (at *attempt) unwatch() {
	if at.watch == nil {
		return
	}
	if err := isolation.Close(at.watch); err != nil {
		at.r.cfg.Logf("job %s: the checkpoints of its folder could not be removed", at.job.ID)
	}
	at.watch = nil
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
	switch {
	case job.Kind == queue.KindApply || job.Kind == queue.KindSnapshot:
		at.finish(ctx, r.folderWork(ctx, at))
		return
	case r.cfg.Role == queue.RoleConnector:
		at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: "a connector takes only snapshot and apply jobs", Status: http.StatusBadRequest})})
		return
	}
	refused := r.prepare(ctx, at)
	defer at.unwatch()
	if refused != nil {
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
				at.event("workspace.removed", map[string]any{"space": at.ws.Space})
			}
			at.space = nil
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
		req.Harness, req.Folder, req.FolderRef = sp.Harness, at.folder, at.ref
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
func (r *Runner) prepare(ctx context.Context, at *attempt) *queue.Result {
	sp := at.job.Spec
	switch {
	case at.job.Kind == queue.KindReview && at.job.Workspace != nil:
		// A review reads the reviewed session's workspace, as it is.
		at.folder = r.workspaceDir(*at.job.Workspace)
		if res := r.remount(at, at.folder); res != nil {
			return res
		}
		if info, err := os.Stat(at.folder); err != nil || !info.IsDir() {
			at.folder = ""
			return nil // the engine refuses it, in its own words
		}
	case sp.Repo != "" || sp.RepoRef != "":
		repo, ok := r.folderPath(sp.Repo, sp.RepoRef, sp.Connector)
		if !ok {
			return &queue.Result{Refused: folderUnknown, Status: http.StatusBadRequest}
		}
		at.ref = sp.RepoRef
		b, _ := isolation.Get(isolation.Git)
		return r.isolate(at, b, isolation.Request{Repo: repo, Base: sp.Base, Ref: sp.RepoRef})
	case (sp.Folder != "" || sp.FolderRef != "") && !r.local(sp):
		// The folder is on another machine: the copy is built from its
		// snapshot, which its connector takes.
		if !sp.Isolate {
			return &queue.Result{Refused: folderNotHere, Status: http.StatusBadRequest}
		}
		at.ref = sp.FolderRef
		return r.fromSnapshot(ctx, at)
	case sp.Folder != "" || sp.FolderRef != "":
		folder, ok := r.folderPath(sp.Folder, sp.FolderRef, sp.Connector)
		if !ok {
			return &queue.Result{Refused: folderUnknown, Status: http.StatusBadRequest}
		}
		at.ref = sp.FolderRef
		if at.ref == "" && r.cfg.Folders != nil {
			at.ref = folders.Ref(folder)
		}
		if res := r.remount(at, folder); res != nil {
			return res
		}
		if info, err := os.Stat(folder); err != nil || !info.IsDir() {
			at.folder = folder
			return nil // the engine refuses it, in its own words
		}
		if sp.Isolate {
			p, err := r.policy()
			if err != nil {
				return &queue.Result{Refused: "the permissions cannot be read, so the turn did not start; fix them and send the turn again"}
			}
			if b, ok := isolation.For(p.Isolation, folder); ok {
				return r.isolate(at, b, isolation.Request{Folder: folder, Share: p.ShareReadOnly, Ref: at.ref})
			}
		}
		at.folder = folder
		at.ws = &queue.Workspace{Kind: queue.WorkspaceFolder, FolderRef: at.ref}
	case at.job.Workspace != nil:
		w := *at.job.Workspace
		at.folder = r.workspaceDir(w)
		if k := w.Kind; k == queue.WorkspaceWorktree || k == queue.WorkspaceCopy {
			// A worktree made before the backends kept a record has none:
			// it is checkpointed as a folder used as it is.
			if s, err := r.loadSpace(w); err == nil {
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
	return r.watchFolder(at)
}

// local says the folder sp names is on this runner's machine: its
// connector is this runner's, or sp names the path itself, as a job stored
// before references did.
func (r *Runner) local(sp queue.Spec) bool {
	if sp.FolderRef == "" {
		return true
	}
	if r.cfg.Folders == nil || sp.Connector != r.cfg.Folders.Connector() {
		return false
	}
	_, ok := r.cfg.Folders.Path(sp.FolderRef)
	return ok
}

// folderNotHere refuses a folder used as it is on a runner that is not on
// the folder's machine.
const folderNotHere = "the folder is on another machine, so this runner can only work in a copy of it; " +
	"start the session with isolate true"

// workspaceDir is where a session's workspace is on this runner: its
// space's folder, a folder used as it is, or the path a session made
// before ids recorded.
func (r *Runner) workspaceDir(w queue.Workspace) string {
	switch {
	case w.Space != "":
		if s, err := isolation.LoadID(r.cfg.Workdir, w.Space); err == nil {
			return s.Dir
		}
		return ""
	case w.FolderRef != "":
		if r.cfg.Folders != nil {
			p, _ := r.cfg.Folders.Path(w.FolderRef)
			return p
		}
		return ""
	}
	return w.Folder
}

// loadSpace is the space of a session's workspace, ready to use: with this
// runner's blob store and lender.
func (r *Runner) loadSpace(w queue.Workspace) (*isolation.Space, error) {
	var s *isolation.Space
	var err error
	if w.Space != "" {
		s, err = isolation.LoadID(r.cfg.Workdir, w.Space)
	} else {
		s, err = isolation.Load(r.cfg.Workdir, w.Folder)
	}
	if err != nil {
		return nil, err
	}
	r.equip(s)
	return s, nil
}

// equip gives s what reads its base content.
func (r *Runner) equip(s *isolation.Space) {
	s.Blobs, s.Lender = r.cfg.Blobs, r.cfg.Lender
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
	r.equip(s)
	at.space, at.folder = s, s.Dir
	at.ws = &queue.Workspace{Kind: kind, Space: s.ID, Repo: s.Repo, Base: s.Base}
	// The session's snapshot id: the copy as it was made.
	if base, err := b.Checkpoint(s, isolation.Base); err == nil {
		at.ws.Snapshot = base.Root
	}
	if res := r.checkpointStart(at); res != nil {
		// Nothing records the copy yet, so nothing else would remove it.
		if err := isolation.Close(s); err != nil {
			r.cfg.Logf("the %s for job %s could not be removed after a refused setup", b.Name(), at.job.ID)
		}
		at.space, at.ws = nil, nil
		return res
	}
	ready := map[string]any{"kind": kind, "space": s.ID}
	if at.ws.Snapshot != "" {
		ready["snapshot"] = at.ws.Snapshot
	}
	if s.Base != "" {
		ready["base"] = s.Base
	}
	if s.Ref != "" {
		ready["folder_ref"] = s.Ref
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

// remount mounts again the copy that holds folder, when folder is in a copy
// this runner made: a review names its author's copy, and that review's
// session, or a session that named such a folder, continues there. After a
// restart the copy's mount is gone until then.
func (r *Runner) remount(at *attempt, folder string) *queue.Result {
	if r.cfg.Workdir == "" {
		return nil
	}
	s, err := isolation.Load(r.cfg.Workdir, folder)
	if err != nil {
		return nil // not in a copy
	}
	if err := isolation.Remount(s); err != nil {
		r.cfg.Logf("the copy for job %s could not be mounted again: %s", at.job.ID, err)
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	return nil
}

// watchFolder checkpoints the folder the job uses as it is, before the turn,
// so its changes come from checkpoints as an isolated turn's do.
func (r *Runner) watchFolder(at *attempt) *queue.Result {
	if at.folder == "" {
		return nil // no folder to watch: the engine still runs the turn, or refuses it
	}
	if res := r.remount(at, at.folder); res != nil {
		return res
	}
	b, _ := isolation.Get(isolation.None)
	s, err := b.Open(isolation.Request{ID: at.a.ID, Workdir: r.cfg.Workdir, Folder: at.folder})
	if err != nil {
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	at.watch, at.start = s, isolation.Base
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

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
