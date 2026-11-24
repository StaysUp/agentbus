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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/harness"
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
	Queue string // the queue's service, such as http://127.0.0.1:8787
	// EnrollToken adds the runner when StateFile holds no identity control
	// accepts. Empty: a machine signed in by agentbus login, which has none.
	EnrollToken string
	Name        string
	Labels      []string
	Capacity    int // jobs at once; 0 means run.DefaultMaxRuns
	// Version is this runner's release, such as 0.6.0 ("dev" for a build from
	// a clone). The runner refuses to start against a control whose release is
	// older, so control is upgraded first. Empty, or either side not a release,
	// skips the check.
	Version string

	// Engine starts and stops the turns, and Store is the record it writes:
	// a turn's end and its reply are read from it.
	Engine *run.Engine
	Store  *store.Store

	// Workdir holds the isolated copies this runner makes, one per session,
	// each named after its session (package isolation).
	Workdir string
	// OldWorkdir is where a build before 0.4 made its copies, each named
	// after the job that opened it. Copies there are still found, used and
	// removed; new ones go to Workdir. Empty: none.
	OldWorkdir string
	// Settings say which isolation a new session gets. Nil: the defaults.
	Settings settings.Store
	// StateFile keeps the runner's id and token across restarts, so a
	// session's later jobs still find the runner that holds its worktree.
	// Empty enrolls a new runner on every start.
	StateFile string

	// Folders are this machine's folders, and FolderSide what reads and
	// writes them (package connector): the runner reports them, a job that
	// names their machine comes to this runner only, and it does that
	// job's folder work itself, the copy (by clone or overlay, reading base
	// content through Lender) and the apply. Both or neither; nil: a runner
	// with no folders, which takes only jobs that name no machine.
	FolderSide FolderSide
	Folders    *folders.Registry
	Lender     isolation.Lender
	// Blobs reads blobs for a copy's base content: the runner's own store
	// with the control plane's behind it, or in agentbus up the control
	// plane's own. BlobSink is the control plane's store, where a
	// turn's changeset and the files it names go. Nil: a runner without
	// them can only use the local fast path, and records no changeset.
	Blobs    isolation.BlobSource
	BlobSink blobs.Sink
	// Sandbox says whether this machine can build a session's wall, and why
	// not (sandbox.Can). It is asked once, when the runner enrolls, and
	// reported with the runner's capabilities. Nil reports nothing.
	Sandbox func() (bool, string)
	// LookPath and Environ are how the runner probes its harnesses for its
	// report (caps.go). Nil LookPath probes nothing.
	LookPath func(string) (string, error)
	Environ  func() []string

	// Forward sends each turn's record to the control plane while the turn
	// runs (POST /api/v0/runner/attempts/{id}/record and /calls): a runner of a
	// server, whose Store is its own. Its agents then call control's API,
	// at Queue, with the turn token control minted. In agentbus up Store is
	// the control plane's and nothing is sent; its agents get control's
	// token too, as NoSecrets says.
	Forward bool
	// NoSecrets: this runner holds no credential. Its agents' model calls
	// carry the turn's stand-in (jobs.AttemptStarted.StandIn), never its
	// token, and control's gateway adds the real credential
	// (jobs/relay.go): on a runner of a server, and in
	// agentbus up, whose gateways add it in the same process. A turn
	// whose agent control has no credential for is refused before it starts
	// (jobs.AttemptStarted.Signin).
	NoSecrets bool

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
	// openMu holds one *sync.Mutex per workspace id, so two first turns of one
	// workspace (two sessions a caller started at once) do not open the same
	// copy on disk concurrently: the first makes it, the rest load it.
	openMu sync.Map
	down   retry // the take loops' wait while control does not answer
	// probed is the last probe's harnesses, under probeMu: every report
	// sends it (caps.go).
	probeMu sync.Mutex
	probed  []harness.Report
	// beatMu keeps one report in flight at a time (beat).
	beatMu sync.Mutex
	// sandbox is Config.Sandbox's answer, nil without one.
	sandbox *queue.SandboxReport
	// attempts are the running attempts by their turn, under attemptsMu, so the
	// gateway's hold can find the attempt a held reply's turn runs in and poll
	// its decision (decide.go). A turn runs one attempt at a time.
	attemptsMu sync.Mutex
	attempts   map[string]*attempt
}

// state is what StateFile keeps.
type state struct {
	Runner      string `json:"runner"`
	Token       string `json:"token"`
	HeartbeatMS int64  `json:"heartbeat_ms"`
	LeaseMS     int64  `json:"lease_ms"`
}

// ErrNotSignedIn: the runner has no enrollment token (only the built-in
// runner of agentbus up has one), and control does not accept the identity
// agentbus login saved (refused, removed, or another control), or there is
// none.
var ErrNotSignedIn = errors.New("this machine is not signed in to this control, or control no longer accepts it: run agentbus login with control's address")

// Enroll adds the runner to the control plane: with the identity saved in
// StateFile (by an earlier enroll, or by agentbus login) when control still
// accepts it, else with the enrollment token. Without one it answers
// ErrNotSignedIn.
func Enroll(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Capacity == 0 {
		cfg.Capacity = run.DefaultMaxRuns
	}
	if cfg.StopWait == 0 {
		cfg.StopWait = time.Minute
	}
	switch {
	case cfg.Engine == nil || cfg.Store == nil:
		return nil, errors.New("runner: an engine and its store are required")
	case (cfg.FolderSide == nil) != (cfg.Folders == nil):
		return nil, errors.New("runner: a runner's folders need their folder side, and the reverse")
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
	if cfg.Sandbox != nil {
		can, why := cfg.Sandbox()
		r.sandbox = queue.NewSandboxReport(can, why)
	}
	// A saved identity is used again when the control plane still knows it.
	if st, ok := loadState(cfg.StateFile); ok {
		var rn jobs.RunnerBeatReply
		status, err := call(ctx, r.short, cfg.Queue+"/api/v0/runner/heartbeat", st.Token,
			jobs.RunnerBeat{Capacity: cfg.Capacity, Labels: cfg.Labels, Capabilities: r.caps(nil)}, &rn)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			// A saved identity, from a restart or agentbus login, checks control
			// the same way a fresh enrol does: its reply carries the version.
			if refuseControl(rn.ControlVersion, cfg.Version) {
				return nil, oldControlErr(rn.ControlVersion, cfg.Version)
			}
			r.id, r.token = st.Runner, st.Token
			r.heartbeat, r.lease = time.Duration(st.HeartbeatMS)*time.Millisecond, time.Duration(st.LeaseMS)*time.Millisecond
			return r, nil
		}
		if status != http.StatusUnauthorized && status != http.StatusNotFound {
			return nil, fmt.Errorf("runner: the saved runner's heartbeat answered %d", status)
		}
	}
	if cfg.EnrollToken == "" {
		// Signed in by agentbus login, which saved the identity: there is
		// no enrollment token to fall back on.
		return nil, ErrNotSignedIn
	}
	var e jobs.Enrolled
	status, err := call(ctx, cfg.HTTP, cfg.Queue+"/api/v0/runner/register", cfg.EnrollToken,
		jobs.EnrollRequest{Name: cfg.Name, Labels: cfg.Labels, Capacity: cfg.Capacity, Protocol: jobs.ProtocolVersion,
			Capabilities: r.caps(nil)}, &e)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("runner: enroll answered %d", status)
	}
	// Upgrade control first: a runner refuses a control older than itself,
	// learned from the enroll reply. Such control reads the old record shape
	// this runner no longer writes (a review's verdict, a delivered reply) and
	// offers routes this runner's instructions no longer name, so it would
	// quietly lose work. A newer control, or an older runner on it, is fine.
	if refuseControl(e.ControlVersion, cfg.Version) {
		return nil, oldControlErr(e.ControlVersion, cfg.Version)
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

// refuseControl says whether a runner of version mine refuses to start against
// a control whose reply carried controlVersion. A runner that is not a release
// (a dev build) never refuses. A release runner refuses a control that is an
// older release, and one that sent no version at all -- control before 0.6,
// which added the field -- since that is older than any 0.6 release.
func refuseControl(controlVersion, mine string) bool {
	if _, ok := parseRelease(mine); !ok {
		return false
	}
	return controlVersion == "" || controlOlder(controlVersion, mine)
}

// oldControlErr is the plain line a refused runner stops with.
func oldControlErr(controlVersion, mine string) error {
	had := controlVersion
	if had == "" {
		had = "a release before 0.6 (it does not report its version)"
	}
	return fmt.Errorf("control is %s, this runner is %s: upgrade control first", had, mine)
}

// controlOlder is true only when both are releases (major.minor.patch) and
// control is below mine. A "dev" or otherwise unparsable version is never
// older, so a developer's own builds are not blocked.
func controlOlder(control, mine string) bool {
	c, ok1 := parseRelease(control)
	m, ok2 := parseRelease(mine)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if c[i] != m[i] {
			return c[i] < m[i]
		}
	}
	return false
}

// parseRelease reads major.minor.patch, dropping any -pre or +build suffix.
func parseRelease(v string) ([3]int, bool) {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// ErrIdentityThere: SaveIdentity found an identity saved in its place.
var ErrIdentityThere = errors.New("runner: a runner's identity is already saved there")

// SaveIdentity keeps a runner's identity in path, the runner's StateFile, as
// Enroll reads it: what agentbus login does once the person approved the
// machine. It never replaces one already there (ErrIdentityThere), so of two
// logins on one data folder the first to be approved keeps it. Whole or not
// at all: written aside, then linked into place.
func SaveIdentity(path string, rn queue.Runner, token string, heartbeatMS, leaseMS int64) error {
	b, _ := json.Marshal(state{rn.ID, token, heartbeatMS, leaseMS})
	tmp := path + "." + queue.NewID("") + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return errors.New("runner: the state file could not be written")
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrIdentityThere
		}
		return errors.New("runner: the state file could not be written")
	}
	return nil
}

// SavedID is the runner id in the identity file at path, or "". It reads
// the id alone, never the token.
func SavedID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var st struct {
		Runner string `json:"runner"`
	}
	if json.Unmarshal(b, &st) != nil {
		return ""
	}
	return st.Runner
}

// HasIdentity says path holds a runner's identity Enroll would try.
func HasIdentity(path string) bool {
	_, ok := loadState(path)
	return ok
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

// Run takes and runs jobs, Capacity at once, until ctx ends, and with its
// folders as many as queue.FolderCapacity folder jobs besides, so an apply
// never waits behind running turns. A job still running then has its turn
// stopped and is finished as failed with reason runner_stopped.
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
			r.loop(ctx, false)
		}()
	}
	if r.cfg.FolderSide != nil {
		for range queue.FolderCapacity {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.loop(ctx, true)
			}()
		}
	}
	wg.Wait()
}

// unmount unmounts every overlay copy in the runner's workdir. A copy's
// files stay where they are.
func (r *Runner) unmount(when string) {
	for _, wd := range r.workdirs() {
		n, err := isolation.Unmount(wd)
		if n > 0 {
			r.cfg.Logf("isolation.unmounted n=%d when=%s", n, when)
		}
		if err != nil {
			r.cfg.Logf("isolation.unmount_failed when=%s error=%q", when, err.Error())
		}
	}
}

// workdirs are the folders this runner's copies may be in: Workdir, then
// OldWorkdir.
func (r *Runner) workdirs() []string {
	var out []string
	for _, wd := range []string{r.cfg.Workdir, r.cfg.OldWorkdir} {
		if wd != "" {
			out = append(out, wd)
		}
	}
	return out
}

// loadIn loads the space load finds in the first of the runner's workdirs
// that holds it.
func (r *Runner) loadIn(load func(workdir string) (*isolation.Space, error)) (*isolation.Space, error) {
	err := isolation.ErrNoSpace
	for _, wd := range r.workdirs() {
		var s *isolation.Space
		if s, err = load(wd); err == nil {
			return s, nil
		}
	}
	return nil, err
}

// loop takes and runs one job at a time until ctx ends: folder jobs of this
// machine with folders, else turns and reviews.
func (r *Runner) loop(ctx context.Context, folders bool) {
	for ctx.Err() == nil {
		var t jobs.TakenWork
		status, err := call(ctx, r.cfg.HTTP, r.cfg.Queue+"/api/v0/runner/take", r.token,
			jobs.TakeRequest{WaitMS: jobs.MaxWait.Milliseconds(), Protocol: jobs.ProtocolVersion, Folders: folders}, &t)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil || (status != http.StatusOK && status != http.StatusNoContent):
			wait, grew := r.down.failed(time.Now())
			if grew {
				r.cfg.Logf("%s", retryLine(r.cfg.Queue, status, wait))
			}
			sleep(ctx, wait)
			continue
		case r.down.answered():
			r.cfg.Logf("control at %s answers again", r.cfg.Queue)
		}
		switch {
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
	// The session's own home goes with it (run.Config.Homes).
	if err := r.cfg.Engine.RemoveHome(w.Session); err != nil && msg == "" {
		msg = "the session's home could not be removed"
	}
	status, err := call(context.WithoutCancel(ctx), r.short, r.cfg.Queue+"/api/v0/runner/removed", r.token,
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
		wd := r.cfg.Workdir
		if r.cfg.OldWorkdir != "" && strings.HasPrefix(w.Folder, r.cfg.OldWorkdir+string(filepath.Separator)) {
			wd = r.cfg.OldWorkdir
		}
		return isolation.RemoveWorktree(wd, w.Repo, w.Folder)
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

	cancel, stale chan struct{} // closed when a heartbeat hears it
	// signin is control's word that the turn's agent can get no
	// credential from it now (jobs.AttemptStarted.Signin).
	signin                string
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

	// token is the attempt's turn token, minted by control (jobs.AttemptStarted),
	// and standIn its agent's model credential, minted with it.
	token, standIn string
	// sent is what of the turn's record control holds (record.go).
	fwdMu sync.Mutex
	sent  sent
}

func (at *attempt) path(op string) string {
	return at.r.cfg.Queue + "/api/v0/runner/attempts/" + at.a.ID + "/" + op
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
	at.forward(ctx, at.sessionOf(f), true)
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

// MaxRecordBatch bounds the event bytes of one record batch, under the
// protocol's body limit.
const MaxRecordBatch = jobs.MaxBody * 3 / 4

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
	// The whole finish must fit the runner protocol's body limit, so the changes get
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
	end, err := b.Checkpoint(s, at.job.Turn)
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
	at.token, at.standIn, at.signin = st.Token, st.StandIn, st.Signin
	at.mu.Unlock()
	done := make(chan struct{})
	defer close(done)
	go at.heartbeats(ctx, done)

	// The attempt is current on control now: the gateway's hold can find it by
	// turn and poll its decision while it holds an Ask (decide.go).
	r.registerAttempt(at)
	defer r.unregisterAttempt(at)

	at.event("attempt.started", map[string]any{"runner": r.id})
	if st.Cancel {
		at.finish(ctx, queue.Finish{Status: queue.Cancelled})
		return
	}
	switch {
	case job.Kind == queue.KindApply || job.Kind == queue.KindPreview || job.Kind == queue.KindFolder:
		at.finish(ctx, r.folderWork(ctx, at))
		return
	case job.Kind == queue.KindDiscard:
		at.finish(ctx, r.discardWork(at))
		return
	case job.Kind == queue.KindUpdate:
		at.finish(ctx, r.updateWork(ctx, at))
		return
	case job.Kind == queue.KindHome:
		at.finish(ctx, r.homeWork(at))
		return
	case job.Kind != queue.KindTurn:
		// A kind this runner does not run, such as a command job (the
		// person's "!" in 0.4, retired) a control made before.
		at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: fmt.Sprintf("this runner does not run jobs of kind %q", job.Kind), Status: http.StatusBadRequest})})
		return
	}
	refused := r.prepare(ctx, at)
	defer at.unwatch()
	if refused != nil {
		// prepare may have loaded the copy (at.space/at.ws) before a later step
		// refused -- loadSpace succeeds, then checkpointStart fails (an overlay
		// remount after a restart, unreadable checkpoints). A refused setup starts
		// no turn, so clear them: addChanges must record nothing for a turn that
		// never started, no turn.changed for a copy it did not touch.
		at.space, at.ws = nil, nil
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
		// No turn started, so it touches no copy: it records no changeset and no
		// turn.changed (AGENTS.md: a turn that cannot start stores nothing). A
		// turn never removes the copy -- the copy belongs to the workspace, not
		// the turn, and another session may be running in it or hold unapplied
		// edits (his rule, 2026-11-21; removal is a workspace event, not a turn's
		// teardown). The attempt only lets go of the copy and its folder watch
		// before finishing, so addChanges sees nothing to record.
		at.space = nil
		at.unwatch()
		at.event("turn.refused", map[string]any{"status": res.Status})
		at.finish(ctx, queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: mustJSON(res)})
		return
	}
	at.session = turn.Session
	at.event("turn.created", map[string]any{"session": turn.Session, "turn": turn.ID})
	at.flush(ctx)
	// The turn's record reaches control while it runs (record.go).
	go at.live(ctx, done)
	r.follow(ctx, at, turn)
	// B15: no review teardown here. A review is a normal turn; its session and
	// copy follow the same lifecycle as any asked turn's (the ask app closes an
	// asked session when it is safe, later in milestone C).
}

// request is the turn the attempt runs. Its id is the job's turn. A turn
// job's first turn opens the session the queue named for it; a review opens
// one of its own, in the reviewed job's folder, as a normal turn (B12).
func (r *Runner) request(at *attempt) run.Request {
	sp := at.job.Spec
	req := run.Request{Turn: at.job.Turn, Prompt: sp.Prompt, Model: sp.Model, Workspace: string(sp.Workspace),
		IdempotencyKey: at.job.IdempotencyKey, AllowOnce: sp.AllowOnce}
	// Copy mode is the workspace's sandbox kind: the turn runs in a copy, so its
	// commands are walled to it. prepare set at.ws to the kind it opened; the
	// job's Workspace row is written only when a first turn ends, so the first
	// turn of a Copy workspace must read the prepared kind, not that row.
	w := at.ws
	if w == nil {
		w = at.job.Workspace
	}
	if w != nil && (w.Kind == queue.WorkspaceCopy || w.Kind == queue.WorkspaceWorktree) {
		req.Copy = true
	}
	// The agent's token for the API is control's: a runner of a server's
	// always, and in up too. Its model calls carry the stand-in, not the token.
	if (r.cfg.Forward || r.cfg.NoSecrets) && at.token != "" {
		req.Token, req.APIURL = at.token, r.cfg.Queue
	}
	// Control adds this runner's agents' credentials, and has none for
	// this agent now: the engine refuses the turn in control's words. The
	// agent's own credential is the stand-in control minted with the
	// token; a control that sent none is one from before stand-ins, and its
	// relay would want the turn token, which the agent never gets.
	if r.cfg.NoSecrets {
		req.StandIn, req.Signin = at.standIn, at.signin
		if at.standIn == "" && at.signin == "" {
			req.Signin = "control sent this turn no stand-in credential for its agent; update agentbus on control to this runner's version"
		}
	}
	// First turn vs continuation: the session is created before its first turn
	// (POST /api/v0/sessions), so sp.Session is always set. A session that has
	// not run a turn yet (only session.created) opens a fresh child in its
	// workspace; one with a prior run continues the conversation, on the agent
	// it names. It is told by whether the session RAN, not by its agent, which a
	// gateway session leaves empty.
	ran := false
	if sp.Session != "" {
		if c, err := r.cfg.Store.Continuation(string(sp.Session)); err == nil {
			ran = c.Ran
		}
	}
	switch {
	case sp.Session != "" && ran:
		req.Session = string(sp.Session)
		req.Harness, req.AgentID, req.Access, req.KeyEnv, req.AgentModel = sp.Harness, sp.AgentID, sp.Access, sp.KeyEnv, sp.AgentModel
	default:
		req.Harness, req.Folder, req.FolderRef = sp.Harness, at.folder, at.ref
		req.AgentID, req.Access, req.KeyEnv, req.AgentModel = sp.AgentID, sp.Access, sp.KeyEnv, sp.AgentModel
		// The first turn opens the session the caller created for it.
		req.NewSession = string(sp.Session)
		if req.NewSession == "" {
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
			TurnKind: l.ResultOf.Kind, Agent: l.ResultOf.Agent}
	case l.Chain != nil && l.Chain.By != "":
		req.Origin = &event.Origin{Kind: event.OriginAsked, Session: l.Chain.BySession, Turn: l.Chain.By}
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
	// B15: no review workspace recipe. A review is a normal turn, so it runs in
	// a fresh copy of the folder like any asked turn and reads the reviewed
	// turn's diff through agentbus-api, not from a copy of its workspace.
	case sp.Repo != "" || sp.RepoRef != "":
		repo, ok := r.folderPath(sp.Repo, sp.RepoRef, sp.Machine)
		if !ok {
			return &queue.Result{Refused: folderUnknown, Status: http.StatusBadRequest}
		}
		at.ref = sp.RepoRef
		b, _ := isolation.Get(isolation.Git)
		return r.isolate(at, b, isolation.Request{Repo: repo, Base: sp.Base, Ref: sp.RepoRef})
	case sp.Folder != "" || sp.FolderRef != "":
		// A continuation whose workspace already has a copy loads it here, before
		// the source folder is resolved or checked (os.Stat below), so a
		// continuation whose source folder was removed or moved still runs and
		// records its changeset (P2 #1). A first turn has no copy yet and falls
		// through to make one from the folder.
		if w := at.job.Workspace; w != nil && (w.Kind == queue.WorkspaceCopy || w.Kind == queue.WorkspaceWorktree) {
			if res, reused := r.loadCopy(at, *w); reused {
				return res
			}
		}
		// A session runs on the machine that holds its folder: this one.
		folder, ok := r.folderPath(sp.Folder, sp.FolderRef, sp.Machine)
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
			// The workspace's sandbox is the one switch for copy-or-folder; the
			// isolation setting only chooses the backend. "none" is not a backend
			// but the old way to turn the wall off, which sandbox:false now does,
			// so a sandbox turn ignores it and isolates the default way (his rule,
			// 2026-11-21; parked review finding P2.5).
			setting := p.Isolation
			if setting == settings.IsolationNone {
				setting = settings.IsolationAuto
			}
			if b, ok := isolation.For(setting, folder); ok {
				return r.isolate(at, b, isolation.Request{Folder: folder, Ref: at.ref})
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
		// A turn's place -- its folder -- comes from its WORKSPACE, the one
		// source (his rule, 2026-11-21), not from a session continuation, which
		// is empty for a new or asked session that has not run a turn yet. A
		// direct (sandbox false) workspace runs here: its folder is on its
		// workspace.created. A session the record places in no workspace, or one
		// whose workspace names no folder, has nowhere to run and is refused.
		if sp.Workspace != "" {
			if w, ok, err := r.cfg.Store.Workspace(sp.Workspace); err == nil && ok {
				at.folder = w.Folder
			}
		}
		if at.folder == "" && sp.Session != "" {
			// A legacy session with no workspace record of its own: its folder is
			// on its own run events.
			if c, err := r.cfg.Store.Continuation(string(sp.Session)); err == nil && c.Known {
				at.folder = c.Folder
			}
		}
		if at.folder == "" && sp.Session != "" {
			return &queue.Result{Refused: "no such session", Status: http.StatusNotFound}
		}
	}
	return r.watchFolder(at)
}

// workspaceDir is where a session's workspace is on this runner: its
// space's folder, a folder used as it is, or the path a session made
// before ids recorded.
func (r *Runner) workspaceDir(w queue.Workspace) string {
	switch {
	case w.Space != "":
		if s, err := r.loadIn(func(wd string) (*isolation.Space, error) { return isolation.LoadID(wd, w.Space) }); err == nil {
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
		s, err = r.loadIn(func(wd string) (*isolation.Space, error) { return isolation.LoadID(wd, w.Space) })
	} else {
		s, err = r.loadIn(func(wd string) (*isolation.Space, error) { return isolation.Load(wd, w.Folder) })
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

// loadCopy loads the workspace's copy w on this runner, if it is there, and
// names where the turn's diff starts. reused reports whether a copy was loaded;
// when it was, res is the checkpoint-start result (nil on success, else why the
// turn cannot run). A turn whose workspace already has a copy loads it rather
// than making a second (one workspace, one copy), and a copy is self-contained:
// it needs no source folder, so a continuation whose source folder was removed
// or moved still runs and records its changeset (P2 #1). The copy is loaded by
// its space id (w.Space); a folder-kind record has none and the caller does not
// pass one here.
func (r *Runner) loadCopy(at *attempt, w queue.Workspace) (res *queue.Result, reused bool) {
	s, err := r.loadSpace(w)
	if err != nil {
		return nil, false
	}
	// Name the folder reference the session's first turn records (folder_ref on
	// run.started and turn.started), without touching the source folder: an
	// asked or second session's first turn reuses an existing copy here and still
	// needs its workspace's folder_ref, or that event -- and every continuation
	// after it -- loses the folder (a remote runner's record keeps only the ref).
	// The spec carries it (control stamps it from the record); a job with none
	// falls back to the copy's saved ref. A caller that already set at.ref (the
	// folder/repo cases, before isolate) keeps it.
	if at.ref == "" {
		at.ref = at.job.Spec.FolderRef
		if at.ref == "" {
			at.ref = s.Ref
		}
	}
	kind := queue.WorkspaceCopy
	if s.Backend == isolation.Git {
		kind = queue.WorkspaceWorktree
	}
	at.space, at.folder = s, s.Dir
	at.ws = &queue.Workspace{Kind: kind, Space: s.ID, Repo: s.Repo, Base: s.Base}
	if b, ok := isolation.Get(s.Backend); ok {
		if base, err := b.Checkpoint(s, isolation.Base); err == nil {
			at.ws.Snapshot = base.Root
		}
	}
	return r.checkpointStart(at), true
}

// discardWork resets the workspace's copy to the checkpoint the discard names
// (its Restore, or the copy's base when empty), on the runner holding the copy.
// It drops the unapplied turns' file changes from the copy and never touches the
// person's folder. Control records workspace.discarded from the job.
func (r *Runner) discardWork(at *attempt) queue.Finish {
	refuse := func(msg string, status int) queue.Finish {
		return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: msg, Status: status})}
	}
	// Find the copy to reset: the workspace's copy row, or -- when the first turn
	// was lost or stopped before control recorded one -- a copy it left on disk
	// under the workspace id. A folder-kind record (direct mode) has no copy.
	var s *isolation.Space
	if w := at.job.Workspace; w != nil && (w.Kind == queue.WorkspaceCopy || w.Kind == queue.WorkspaceWorktree) {
		if sp, err := r.loadSpace(*w); err == nil {
			s = sp
		}
	} else if ws := at.job.Spec.Workspace; ws != "" {
		if sp, err := r.loadSpace(queue.Workspace{Space: string(ws)}); err == nil {
			s = sp
		}
	}
	if s == nil {
		// No copy anywhere to reset. Refuse, so a discard that reset nothing does
		// NOT record a workspace.discarded or answer "discarded".
		return refuse("there is no sandbox copy to discard", http.StatusBadRequest)
	}
	b, ok := isolation.Get(s.Backend)
	if !ok {
		return refuse("the copy could not be read, so nothing was discarded", http.StatusInternalServerError)
	}
	// A copy left unmounted after a restart is mounted again before the reset.
	if err := isolation.Remount(s); err != nil {
		return refuse("the copy could not be read, so nothing was discarded", http.StatusInternalServerError)
	}
	target := at.job.Spec.Restore
	if target == "" {
		target = isolation.Base
	}
	if err := b.RestoreTo(s, target); err != nil {
		if errors.Is(err, isolation.ErrNoRestore) {
			return refuse(err.Error(), http.StatusBadRequest)
		}
		return refuse("the copy could not be reset to the last applied state, so nothing was discarded", http.StatusInternalServerError)
	}
	return queue.Finish{Status: queue.Finished}
}

// updateWork merges the person's folder as it is now into the workspace's copy
// by a three-way merge in the copy's private git (isolation.UpdateFrom), on the
// runner holding the copy. That runner must also own the folder (the local fast
// path), so it can read the folder for the "theirs" side; a copy and folder on
// different machines are refused for now. A clean merge publishes the folder's
// current state (the new apply baseline) and the merge as changesets and answers
// them; a conflict answers the files and changes nothing. The person's folder is
// only read, never written. Control records workspace.updated from the finish.
func (r *Runner) updateWork(ctx context.Context, at *attempt) queue.Finish {
	ctx, stop := attemptContext(ctx, at)
	defer stop()
	sp := at.job.Spec
	refuse := func(msg string, status int) queue.Finish {
		return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: msg, Status: status})}
	}
	// The copy to merge into: the workspace's copy row, or a copy left on disk
	// under the workspace id, exactly as discardWork finds it.
	var s *isolation.Space
	if w := at.job.Workspace; w != nil && (w.Kind == queue.WorkspaceCopy || w.Kind == queue.WorkspaceWorktree) {
		if sp2, err := r.loadSpace(*w); err == nil {
			s = sp2
		}
	} else if ws := sp.Workspace; ws != "" {
		if sp2, err := r.loadSpace(queue.Workspace{Space: string(ws)}); err == nil {
			s = sp2
		}
	}
	if s == nil {
		return refuse("there is no sandbox copy to update", http.StatusBadRequest)
	}
	b, ok := isolation.Get(s.Backend)
	if !ok {
		return refuse("the copy could not be read, so nothing was updated", http.StatusInternalServerError)
	}
	if err := isolation.Remount(s); err != nil {
		return refuse("the copy could not be read, so nothing was updated", http.StatusInternalServerError)
	}
	// Reading the folder for "theirs" needs it on this machine: the copy's runner
	// must own the folder. A far runner holding only the copy cannot read it yet.
	if r.cfg.FolderSide == nil || r.cfg.Folders == nil || sp.Machine != r.cfg.Folders.Machine() {
		return refuse("update needs the copy and the folder on one machine; it is not available for a copy on another machine yet", http.StatusBadRequest)
	}
	folder, ok := r.folderPath("", sp.FolderRef, sp.Machine)
	if !ok {
		return refuse(folderUnknown, http.StatusBadRequest)
	}
	res, err := b.UpdateFrom(s, sp.Restore, folder)
	if err != nil {
		if errors.Is(err, isolation.ErrNoUpdate) {
			return refuse(err.Error(), http.StatusBadRequest)
		}
		if ctx.Err() != nil {
			return refuse("the update was stopped; nothing was changed", http.StatusConflict)
		}
		return refuse("the folder could not be merged into the copy, so nothing was updated", http.StatusInternalServerError)
	}
	if !res.Clean {
		// A real conflict: nothing changed. Finished, with the files, so control
		// records workspace.updated(result conflict) and the person can ask the
		// agent to resolve them.
		return queue.Finish{Status: queue.Finished,
			Result: mustJSON(queue.Result{Update: &queue.UpdateResult{Conflicts: res.Conflicts}})}
	}
	// Publish the folder's current state (the new apply baseline) and the merge as
	// changesets, so an apply reads them from the store by blob. UpdateFrom has
	// already advanced the copy to the merge; if a publish fails (or the runner
	// has no store), the merge is not recorded, so the copy must go back to its
	// pre-update head, or a later turn would checkpoint an unrecorded merge and a
	// later apply could write the folder's imported content wrongly.
	undo := func(msg string) queue.Finish {
		if res.PriorHead != "" {
			if err := b.RestoreTo(s, res.PriorHead); err != nil {
				// The copy could not be reset to its pre-update state, so it may hold
				// the merge control is not recording. Say so rather than report a plain
				// failure: a later turn must not reuse a copy in an unknown state.
				return refuse("the folder could not be merged and the copy could not be reset; discard this session's changes before continuing", http.StatusInternalServerError)
			}
		}
		return refuse(msg, http.StatusInternalServerError)
	}
	fromCS, err := r.publish(ctx, s, res.Theirs)
	if err != nil {
		return undo("the merge could not be saved, so nothing was updated")
	}
	toCS, err := r.publish(ctx, s, res.Merged)
	if err != nil {
		return undo("the merge could not be saved, so nothing was updated")
	}
	if fromCS == "" || toCS == "" {
		return undo("this runner has no store to save the merge, so nothing was updated")
	}
	return queue.Finish{Status: queue.Finished, Result: mustJSON(queue.Result{Update: &queue.UpdateResult{
		From: fromCS, To: toCS, TheirsCheckpoint: res.Theirs, MergedCheckpoint: res.Merged}})}
}

// isolate opens a session's first job's copy with b, and its turn starts
// from the copy's first checkpoint.
func (r *Runner) isolate(at *attempt, b isolation.Backend, req isolation.Request) *queue.Result {
	if r.cfg.Workdir == "" {
		return &queue.Result{Refused: "this runner has no workdir for isolated copies"}
	}
	// The copy is named after its workspace, so every session of a workspace
	// (the person's, and any agent it asks) shares one copy, and an asked turn
	// edits the asker's copy. Falls back to the session for a job with no
	// workspace (hook or legacy traffic).
	if req.ID == "" {
		req.ID = string(at.job.Spec.Workspace)
		if req.ID == "" {
			req.ID = at.job.Session
		}
	}
	req.Workdir = r.cfg.Workdir
	// One copy per workspace, made once: two first turns of one workspace (a
	// caller started two of its sessions at once) would otherwise both find no
	// copy and both open <workdir>/<req.ID>, racing in the copy backend. The
	// per-workspace lock lets the first make it and the rest load it.
	mu, _ := r.openMu.LoadOrStore(req.ID, &sync.Mutex{})
	lk := mu.(*sync.Mutex)
	lk.Lock()
	defer lk.Unlock()
	// A turn whose workspace already has a copy on this runner loads it rather
	// than making a second, so one workspace is one copy: an asked turn edits the
	// asker's copy, and a retake after a restart finds the same one (his
	// decision, 2026-11-21). The copy is keyed by the workspace id (req.ID).
	if res, reused := r.loadCopy(at, queue.Workspace{Space: req.ID}); reused {
		return res
	}
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
		// A turn never removes the copy, even one it just made: the copy belongs
		// to the workspace and removal is a workspace event, not a turn's
		// teardown (his rule, 2026-11-21). The attempt only lets go of it.
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

// checkpointStart names the checkpoint the turn's diff starts from: the
// end of the session's latest turn whose changes control holds
// (Workspace.DiffFrom), else the copy's Base. Whatever the copy holds since
// (from a turn whose runner was lost, or whose finish control refused) is
// then in this turn's diff, as an apply up to it would write it.
func (r *Runner) checkpointStart(at *attempt) *queue.Result {
	b, ok := isolation.Get(at.space.Backend)
	if !ok {
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	// After a restart an overlay copy is not mounted; the turn needs it.
	if err := isolation.Remount(at.space); err != nil {
		r.cfg.Logf("the copy for job %s could not be mounted again: %s", at.job.ID, err)
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	list, err := b.Checkpoints(at.space)
	if err != nil {
		return &queue.Result{Refused: "the workspace could not be read"}
	}
	at.start = isolation.Base
	if w := at.job.Workspace; w != nil && w.DiffFrom != "" {
		for _, c := range list {
			if c.Name == w.DiffFrom {
				at.start = c.Name
			}
		}
	}
	return nil
}

// remount mounts again the copy that holds folder, when folder is in a copy
// this runner made: a review names its author's copy, and that review's
// session, or a session that named such a folder, continues there. After a
// restart the copy's mount is gone until then.
func (r *Runner) remount(at *attempt, folder string) *queue.Result {
	s, err := r.loadIn(func(wd string) (*isolation.Space, error) { return isolation.Load(wd, folder) })
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
			// its run.ended waits on something. The job ends now and
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
	// A harness that keeps its own list of models in the session's home
	// may have just written it: control hears of it now, not at the next
	// probe (run.Engine.Models).
	if h, ok := harness.Get(at.job.Spec.Harness); ok && h.Info().ModelsCache != "" {
		go r.beat(ctx)
	}
	end := turn.Ended()
	res := queue.Result{Session: turn.Session, ExitCode: &end.ExitCode, Stopped: end.Stopped, Unsettled: end.Unsettled, State: "failed"}
	if end.ExitCode == 0 {
		res.State = "finished"
	}
	if list, err := r.cfg.Store.Turns(store.TurnQuery{Session: turn.Session, Turn: turn.ID, Limit: 1}); err == nil && len(list.Turns) > 0 {
		t := list.Turns[0]
		cost := t.CostUSD
		res.State, res.Error, res.ModelCalls, res.CostUSD = t.State, t.Error, t.ModelCalls, &cost
	}
	// The turn's reply is not copied into the result: it lives once in the
	// record (its model.responded event). Control reads a review's verdict,
	// and an asked result's reply, from there by the turn (store.TurnText).
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
