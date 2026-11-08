// Package runner takes jobs from the control plane and runs each one as an
// AgentBus turn, through the framework's public HTTP API only:
// POST /api/v0/turns, GET /api/v0/turns/{id} and POST /api/v0/turns/{id}/stop,
// with the framework's turns token.
//
// One job, one attempt, one turn. The attempt id is the turn's
// idempotency_key, so a POST /api/v0/turns sent again after a lost reply
// starts nothing new.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// Config is what a runner needs.
type Config struct {
	Queue       string // the control plane, such as http://127.0.0.1:8790
	EnrollToken string
	Name        string
	Labels      []string
	Capacity    int // jobs at once; 0 means DefaultCapacity

	Agentbus      string // the framework, such as http://127.0.0.1:8787
	AgentbusToken string // its turns token (turns_token in its data folder)

	// Workdir holds the git worktrees this runner makes, one per session.
	Workdir string
	// StateFile keeps the runner's id and token across restarts, so a
	// session's later jobs still find the runner that holds its worktree.
	// Empty enrolls a new runner on every start.
	StateFile string

	// Poll is how often a running turn's state is read. Zero means 2 s.
	Poll time.Duration
	// StopWait bounds how long a runner waits for a stop to be taken and the
	// turn to end, on a cancel or a shutdown. Zero means 15 s.
	StopWait time.Duration
	// Unreachable bounds how long the framework may fail to describe a
	// running turn before the runner gives up on it. Zero means 2 minutes.
	Unreachable time.Duration
	HTTP        *http.Client
	Logf        func(format string, args ...any)
}

// DefaultCapacity is how many jobs a runner runs at once by default.
const DefaultCapacity = 3

// ReasonRunnerStopped: the runner was shut down while the job ran, and
// stopped its turn.
const ReasonRunnerStopped = "runner_stopped"

// ReasonTurnUnrecovered: the runner could not confirm that the job's turn
// ended or was stopped. The result says may_still_run: the turn may still be
// running in the framework.
const ReasonTurnUnrecovered = "turn_unrecovered"

// Runner is one enrolled runner.
type Runner struct {
	cfg       Config
	short, fw *http.Client // control-plane calls other than take; framework calls
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
		cfg.Capacity = DefaultCapacity
	}
	if cfg.Poll == 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.StopWait == 0 {
		cfg.StopWait = 15 * time.Second
	}
	if cfg.Unreachable == 0 {
		cfg.Unreachable = 2 * time.Minute
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
		short: &http.Client{Timeout: 10 * time.Second, Transport: cfg.HTTP.Transport},
		fw:    &http.Client{Timeout: 15 * time.Second, Transport: cfg.HTTP.Transport}}
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

// clean removes a closed session's worktree, if this runner made it, and
// tells the control plane. A folder the job named is never removed. A
// removal that fails is reported with its reason, and the worktree stays.
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
	if w.Kind == queue.WorkspaceWorktree {
		if err := workspace.RemoveWorktree(r.cfg.Workdir, w.Repo, w.Folder); err != nil {
			msg = "the worktree could not be removed: " + err.Error()
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
	// the workspace to record. before is its snapshot at the job's start;
	// session is the framework session once the turn named it.
	folder  string
	ws      *queue.Workspace
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
	if at.before == nil {
		return
	}
	res := map[string]any{}
	if len(f.Result) > 0 {
		_ = json.Unmarshal(f.Result, &res)
	}
	var c workspace.Changes
	if after, err := workspace.Take(at.folder, at.before); err != nil {
		c = workspace.Changes{Kind: "snapshot", Files: []workspace.Change{}, Error: "the workspace could not be read at the end"}
	} else {
		c = workspace.Diff(at.folder, at.before, after)
	}
	if v, _ := res["may_still_run"].(bool); v {
		c.Partial = true
	}
	// The finish must fit the queue's body bound, with room for the rest of
	// the result: a reply text, the session and the workspace.
	c.Fit(MaxChangesBytes)
	res["changes"] = c
	res["folder"] = at.folder
	f.Result = mustJSON(res)
	at.event("workspace.changes", map[string]any{"count": c.Count, "kind": c.Kind})
	// A review runs in another session's workspace: it never claims one.
	if at.ws != nil && at.session != "" && at.job.Kind == queue.KindTurn {
		f.Session, f.Workspace = at.session, at.ws
	}
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

// Result is what a finished job's result holds: the framework turn and how it
// ended.
type Result struct {
	Session    string   `json:"session,omitempty"`
	Turn       string   `json:"turn,omitempty"`
	State      string   `json:"state,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Stopped    bool     `json:"stopped,omitempty"`
	Unsettled  bool     `json:"unsettled,omitempty"`
	Error      string   `json:"error,omitempty"`
	Reply      string   `json:"reply,omitempty"`
	ModelCalls int      `json:"model_calls,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	// Refused: the framework's error when it would not start the turn.
	Refused string `json:"refused,omitempty"`
	Status  int    `json:"status,omitempty"`
	// ReplyText, for a review job: the whole text of the turn's last reply,
	// at most MaxReplyText bytes. The control plane reads the verdict from
	// it. Reply is only its first 500 characters.
	ReplyText string `json:"reply_text,omitempty"`
	// MayStillRun: the runner gave up without confirming that the turn
	// ended. The job's state then never says the turn is over.
	MayStillRun bool `json:"may_still_run,omitempty"`
	// StopTaken, when the runner gave up: the framework took a stop of the
	// turn (202), so it is ending, but its end was not seen.
	StopTaken bool `json:"stop_taken,omitempty"`
}

// unrecovered is the finish for a turn the runner gave up on. stop is the
// status the last stop got, 0 for none. Only a 409, the framework saying the
// turn is not running, clears may_still_run.
func unrecovered(session, turn string, stop int) queue.Finish {
	return queue.Finish{Status: queue.Failed, Reason: ReasonTurnUnrecovered,
		Result: mustJSON(Result{Session: session, Turn: turn, MayStillRun: stop != http.StatusConflict,
			StopTaken: stop == http.StatusAccepted || stop == http.StatusConflict})}
}

// turnRow is the part of the framework's turn row the runner reads.
type turnRow struct {
	ID         string  `json:"id"`
	Session    string  `json:"session"`
	State      string  `json:"state"`
	ExitCode   *int    `json:"exit_code"`
	Stopped    bool    `json:"stopped"`
	Unsettled  bool    `json:"unsettled"`
	Error      string  `json:"error"`
	Reply      string  `json:"reply"`
	ModelCalls int     `json:"model_calls"`
	CostUSD    float64 `json:"cost_usd"`
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
	if refused := r.prepare(ctx, at); refused != nil {
		at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: mustJSON(refused)})
		return
	}
	turn, refused := r.startTurn(ctx, at)
	if turn == nil {
		if refused != nil {
			// No turn started, so no session owns a worktree made for it.
			if at.ws != nil && at.ws.Kind == queue.WorkspaceWorktree {
				if err := workspace.RemoveWorktree(r.cfg.Workdir, at.ws.Repo, at.ws.Folder); err == nil {
					at.event("workspace.removed", map[string]any{"folder": at.ws.Folder})
				}
				at.before = nil
			}
			at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: mustJSON(refused)})
		}
		return
	}
	at.session = turn.ID
	at.event("turn.created", map[string]any{"session": turn.ID, "turn": turn.Turn, "existing": turn.Existing})
	at.flush(ctx)
	r.follow(ctx, at, turn.ID, turn.Turn)
}

// prepare finds or makes the job's workspace and takes its snapshot: a
// worktree of the job's repo at its base (a session's first job), the job's
// folder, or the folder of the session it continues. It answers why the job
// cannot run, or nil.
func (r *Runner) prepare(ctx context.Context, at *attempt) *Result {
	sp := at.job.Spec
	switch {
	case sp.Repo != "":
		if r.cfg.Workdir == "" {
			return &Result{Refused: "this runner has no workdir for worktrees"}
		}
		dir := filepath.Join(r.cfg.Workdir, at.job.ID)
		if err := workspace.MakeWorktree(sp.Repo, sp.Base, dir); err != nil {
			return &Result{Refused: "the worktree could not be made: " + err.Error()}
		}
		at.folder = dir
		at.ws = &queue.Workspace{Kind: queue.WorkspaceWorktree, Folder: dir, Repo: sp.Repo, Base: sp.Base}
		at.event("workspace.ready", map[string]any{"kind": "worktree", "folder": dir, "base": sp.Base})
	case sp.Folder != "":
		at.folder = sp.Folder
		at.ws = &queue.Workspace{Kind: queue.WorkspaceFolder, Folder: sp.Folder}
	case at.job.Workspace != nil:
		at.folder = at.job.Workspace.Folder
	default:
		// A session the queue does not know: the framework knows its folder.
		var list struct {
			Turns []struct {
				Folder string `json:"folder"`
			} `json:"turns"`
		}
		status, err := call(context.WithoutCancel(ctx), r.fw, r.cfg.Agentbus+"/api/v0/turns?limit=1&session="+url.QueryEscape(sp.Session),
			r.cfg.AgentbusToken, nil, &list)
		if err == nil && status == http.StatusOK && len(list.Turns) > 0 {
			at.folder = list.Turns[0].Folder
		}
	}
	if at.folder == "" {
		return nil // no folder to watch: the framework still runs the turn
	}
	b, err := workspace.Take(at.folder, nil)
	if err != nil {
		return &Result{Refused: "the workspace could not be read"}
	}
	at.before = b
	return nil
}

// started is the framework's answer to POST /api/v0/turns.
type started struct {
	ID       string `json:"id"`
	Turn     string `json:"turn"`
	Existing bool   `json:"existing"`
}

func (r *Runner) turnBody(at *attempt) map[string]any {
	spec := at.job.Spec
	body := map[string]any{"prompt": spec.Prompt, "idempotency_key": at.a.ID}
	if spec.Session != "" {
		body["session"] = spec.Session
	} else {
		body["agent"], body["folder"] = spec.Agent, at.folder
	}
	if spec.Model != "" {
		body["model"] = spec.Model
	}
	if spec.ReadOnly {
		body["read_only"] = true
	}
	return body
}

// startTurn sends POST /api/v0/turns, keyed by the attempt id, and sends it
// again on a lost reply, a 5xx or a 429. It answers the turn, or why the
// framework refused it. It answers neither when the attempt went stale, was
// cancelled or the runner is shutting down; it has then finished the attempt
// where that is still its to finish, and stopped any turn a request with an
// unknown outcome may have started.
func (r *Runner) startTurn(ctx context.Context, at *attempt) (*started, *Result) {
	body := r.turnBody(at)
	// uncertain: a request may have started a turn whose reply was lost. A
	// 429 or a refusal starts nothing and stores no key.
	uncertain := false
	for wait := time.Second; ; wait = min(wait*2, 10*time.Second) {
		var s started
		var e jobs.JobError
		status, err := callErr(context.WithoutCancel(ctx), r.fw, r.cfg.Agentbus+"/api/v0/turns", r.cfg.AgentbusToken, body, &s, &e)
		if err == nil && status == http.StatusAccepted {
			return &s, nil
		}
		if err == nil && status != http.StatusTooManyRequests && status < 500 {
			if !uncertain {
				return nil, &Result{Refused: e.Error, Status: status}
			}
			// An earlier request may have started a turn, and a refusal now
			// (a replaced token, say) does not say it did not. Say so.
			at.event("turn.unknown", map[string]any{"why": "refused_after_unknown", "status": status})
			at.finish(ctx, unrecovered("", "", 0))
			return nil, nil
		}
		uncertain = uncertain || err != nil || status >= 500
		at.event("turn.retry", map[string]any{"status": status})
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			continue
		case <-at.stale:
			t.Stop()
			if uncertain {
				r.recoverAndStop(ctx, at, body, "stale")
			}
			return nil, nil
		case <-at.cancel:
			t.Stop()
			if uncertain {
				if ok, turn, stop := r.recoverAndStop(ctx, at, body, "cancel"); !ok {
					at.finish(ctx, unrecovered(turn.ID, turn.Turn, stop))
					return nil, nil
				}
			}
			at.finish(ctx, queue.Finish{Status: queue.Cancelled})
			return nil, nil
		case <-ctx.Done():
			t.Stop()
			if uncertain {
				if ok, turn, stop := r.recoverAndStop(ctx, at, body, "runner_stopped"); !ok {
					at.finish(ctx, unrecovered(turn.ID, turn.Turn, stop))
					return nil, nil
				}
			}
			at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: ReasonRunnerStopped})
			return nil, nil
		}
	}
}

// recoverAndStop finds the turn an earlier request with a lost reply may have
// started, by sending the same request with the same key, and stops it. If
// no turn had started, this starts one and stops it at once: a turn left
// running would be worse. ok is true only when the framework named the turn
// and took its stop. A refusal does not count: after a lost reply it cannot
// show that no turn started. turn is the turn when it was found, and stop the
// status its last stop got.
func (r *Runner) recoverAndStop(ctx context.Context, at *attempt, body map[string]any, why string) (ok bool, turn started, stop int) {
	ctx = context.WithoutCancel(ctx)
	for i := range 5 {
		var s started
		status, err := call(ctx, r.fw, r.cfg.Agentbus+"/api/v0/turns", r.cfg.AgentbusToken, body, &s)
		if err == nil && status == http.StatusAccepted {
			at.event("turn.created", map[string]any{"session": s.ID, "turn": s.Turn, "existing": s.Existing})
			taken, code := r.stopTurn(ctx, at, s.Turn, why, r.cfg.StopWait)
			if !taken {
				at.event("turn.unknown", map[string]any{"why": why, "turn": s.Turn})
			}
			return taken, s, code
		}
		if err == nil && status != http.StatusTooManyRequests && status < 500 {
			break // refused: the turn, if any, cannot be found
		}
		sleep(ctx, time.Duration(i+1)*time.Second)
	}
	at.event("turn.unknown", map[string]any{"why": why})
	return false, turn, 0
}

// stopTurn sends POST /api/v0/turns/{id}/stop until the framework takes it or
// says the turn is not running, for at most within. It says whether it got
// there, and the status the last stop got.
func (r *Runner) stopTurn(ctx context.Context, at *attempt, turnID, why string, within time.Duration) (bool, int) {
	ctx = context.WithoutCancel(ctx)
	deadline := time.Now().Add(within)
	for {
		taken, code := r.stopOnce(ctx, at, turnID, why)
		if taken || time.Now().After(deadline) {
			return taken, code
		}
		sleep(ctx, 500*time.Millisecond)
	}
}

// stopOnce sends one stop. taken is true for 202 (stopping) and 409 (not
// running). A 404 is not taken: a framework that does not know the turn
// cannot say it ended. code is the status, 0 when no answer came.
func (r *Runner) stopOnce(ctx context.Context, at *attempt, turnID, why string) (taken bool, code int) {
	code, err := call(context.WithoutCancel(ctx), r.fw, r.cfg.Agentbus+"/api/v0/turns/"+turnID+"/stop", r.cfg.AgentbusToken, nil, nil)
	if err != nil {
		code = 0
	}
	at.event("turn.stop", map[string]any{"why": why, "status": code})
	return code == http.StatusAccepted || code == http.StatusConflict, code
}

// follow reads the turn until it ends, then finishes the attempt with how it
// ended. A cancel or a shutdown stops the turn, sent again on every poll until
// the framework takes it. A stale attempt stops the turn and finishes
// nothing: the control plane has already settled it.
func (r *Runner) follow(ctx context.Context, at *attempt, session, turnID string) {
	poll := time.NewTicker(r.cfg.Poll)
	defer poll.Stop()
	quit, cancelCh := ctx.Done(), (<-chan struct{})(at.cancel)
	cancelled := false
	var why string         // why the turn must stop; empty while it may run
	stopped := false       // the framework took the stop
	stopCode := 0          // the status the last stop got
	var stopAt time.Time   // when a shutdown asked for the stop
	var wantAt time.Time   // when a stop was first wanted, for a cancel or a shutdown
	var badSince time.Time // the first of the reads in a row that failed
	for {
		select {
		case <-quit:
			quit = nil
			stopAt, why = time.Now(), ReasonRunnerStopped
		case <-cancelCh:
			cancelCh = nil // closed: read it once
			cancelled = true
			if why == "" {
				why = "cancel"
			}
		case <-at.stale:
			if !stopped {
				_, _ = r.stopTurn(ctx, at, turnID, "stale", r.cfg.StopWait)
			}
			r.cfg.Logf("job %s attempt %d is no longer current; its turn was stopped", at.job.ID, at.a.Number)
			return
		case <-poll.C:
		}
		if why != "" && wantAt.IsZero() {
			wantAt = time.Now()
		}
		if why != "" && !stopped {
			stopped, stopCode = r.stopOnce(ctx, at, turnID, why)
		}
		at.flush(context.WithoutCancel(ctx))
		var row struct {
			Turn turnRow `json:"turn"`
		}
		status, err := call(context.WithoutCancel(ctx), r.fw, r.cfg.Agentbus+"/api/v0/turns/"+turnID+"?session="+url.QueryEscape(session), r.cfg.AgentbusToken, nil, &row)
		// Give up on a turn the framework will not describe: at once when
		// the token is refused, which no retry mends, else after
		// Unreachable. The turn is stopped if it can be, and the job says
		// the turn may still run.
		readFailed := err != nil || status != http.StatusOK
		if !readFailed {
			badSince = time.Time{}
		} else if badSince.IsZero() {
			badSince = time.Now()
		}
		if readFailed && (status == http.StatusUnauthorized || status == http.StatusForbidden || time.Since(badSince) > r.cfg.Unreachable) {
			if !stopped {
				stopped, stopCode = r.stopOnce(ctx, at, turnID, "unreachable")
			}
			at.event("turn.unknown", map[string]any{"why": "unreachable", "status": status, "stop_taken": stopped})
			at.finish(ctx, unrecovered(session, turnID, stopCode))
			return
		}
		if readFailed || row.Turn.State == "running" {
			// A stop that is wanted and not confirmed in time leaves the turn
			// possibly running: the job says so, never cancelled or stopped.
			if !wantAt.IsZero() && time.Since(wantAt) > r.cfg.StopWait {
				at.event("turn.unknown", map[string]any{"why": why, "stop_taken": stopped})
				at.finish(ctx, unrecovered(session, turnID, stopCode))
				return
			}
			continue
		}
		t := row.Turn
		cost := t.CostUSD
		res := Result{Session: session, Turn: turnID, State: t.State, ExitCode: t.ExitCode, Stopped: t.Stopped,
			Unsettled: t.Unsettled, Error: t.Error, Reply: t.Reply, ModelCalls: t.ModelCalls, CostUSD: &cost}
		if at.job.Kind == queue.KindReview {
			res.ReplyText = r.lastReply(ctx, session, turnID)
		}
		at.event("turn.ended", map[string]any{"state": t.State, "exit_code": t.ExitCode})
		f := queue.Finish{Result: mustJSON(res)}
		switch {
		case !stopAt.IsZero():
			f.Status, f.Reason = queue.Failed, ReasonRunnerStopped
		case cancelled:
			f.Status = queue.Cancelled
		case t.State == "finished":
			f.Status = queue.Finished
		default:
			f.Status, f.Reason = queue.Failed, queue.ReasonAgent
		}
		at.finish(ctx, f)
		return
	}
}

// MaxReplyText bounds a review's reply text in its result.
const MaxReplyText = 256 << 10

// MaxChangesBytes bounds the changes in a result, as JSON. With the reply
// text and the rest, a finish stays well under jobs.MaxBody.
const MaxChangesBytes = 1 << 20

// lastReply is the text of the turn's last model reply that has text, read
// from the turn's events a page at a time. "" when none can be read.
func (r *Runner) lastReply(ctx context.Context, session, turnID string) string {
	ctx = context.WithoutCancel(ctx)
	base := r.cfg.Agentbus + "/api/v0/turns/" + turnID + "/events?limit=1000&session=" + url.QueryEscape(session)
	u, last := base, ""
	for range 1000 {
		var page struct {
			Store  string `json:"store"`
			Events []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"events"`
			NextAfter *int64 `json:"next_after"`
		}
		status, err := call(ctx, r.fw, u, r.cfg.AgentbusToken, nil, &page)
		if err != nil || status != http.StatusOK {
			return last
		}
		for _, e := range page.Events {
			if e.Kind == "model.responded" && e.Text != "" {
				last = e.Text
			}
		}
		if page.NextAfter == nil {
			break
		}
		u = base + "&store=" + url.QueryEscape(page.Store) + "&after=" + fmt.Sprint(*page.NextAfter)
	}
	if len(last) > MaxReplyText {
		// The verdict block ends the reply, so the end is what is kept,
		// cut on a whole character.
		last = strings.ToValidUTF8(last[len(last)-MaxReplyText:], "")
	}
	return last
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
