//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/ids"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// The test binary doubles as the fake agent. With RUNNER_FAKE_AGENT set it
// appends its folder and arguments to $RUNNER_FAKE_LOG, then follows its
// prompt (the last argument), one instruction a line:
//
//	write <path> <text>   writes text to path, in its folder
//	hold                  waits until the file $RUNNER_FAKE_LOG.go exists
//	exit <n>              exits with n
//
// While the file $RUNNER_FAKE_LOG.hold exists, every turn holds first, as
// hold does: for a turn whose prompt the test does not write, a review's.
func TestMain(m *testing.M) {
	if os.Getenv("RUNNER_FAKE_AGENT") == "1" {
		fakeAgent()
	}
	os.Exit(m.Run())
}

func fakeAgent() {
	// The vendor's session id, as claude prints it, so a next turn can
	// continue the session by resume.
	fmt.Printf("{\"type\":\"result\",\"session_id\":\"fake-%d\"}\n", os.Getpid())
	cwd, _ := os.Getwd()
	log := os.Getenv("RUNNER_FAKE_LOG")
	b, _ := json.Marshal(fakeRun{Cwd: cwd, Args: os.Args[1:]})
	if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(b, '\n'))
		f.Close()
	}
	prompt := os.Args[len(os.Args)-1]
	if _, err := os.Stat(log + ".hold"); err == nil {
		prompt = "hold\n" + prompt
	}
	code := 0
	for _, line := range strings.Split(prompt, "\n") {
		f := strings.SplitN(line, " ", 3)
		switch f[0] {
		case "write":
			p := filepath.Join(cwd, f[1])
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(f[2]), 0o644)
		case "hold":
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(log + ".go"); err == nil {
					break
				}
			}
		case "exit":
			fmt.Sscan(f[1], &code)
		}
	}
	os.Exit(code)
}

type fakeRun struct {
	Cwd  string   `json:"cwd"`
	Args []string `json:"args"`
}

const (
	mainTok   = "abm_MAIN-NEVER-ECHOED"
	enrollTok = "abe_ENROLL-NEVER-ECHOED"
)

type world struct {
	skew   atomic.Int64 // added to the queue's clock
	jobsDB string       // the queue's file
	q      *queue.Queue
	st     *store.Store
	eng    *run.Engine
	plane  *httptest.Server
	cancel context.CancelFunc
	done   chan struct{}
	folder string // a folder for first jobs
	log    string // the fake agent's log
	r      *Runner
	// settings the runner reads for each turn's isolation. Empty dir, so it
	// answers the default (auto) unless a test writes a policy; a test can
	// SetPolicy to pin a backend (e.g. copy) before it queues turns.
	settings *settings.Files
}

func newWorld(t *testing.T, capacity int, record ...bool) *world {
	t.Helper()
	// With a record, control keeps the turn's events and reads a review's
	// verdict from there, as agentbus up does; without one, the finish ends
	// the job directly and outcomes stay in its result.
	withRecord := len(record) > 0 && record[0]
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "fake.log")
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := run.New(run.Config{Store: st, BaseURL: "http://127.0.0.1:1", RunGrace: time.Second,
		Wire: func(agent, baseURL string, _ run.Access) (run.Launch, error) {
			return run.Launch{Path: self, Env: append(os.Environ(), "RUNNER_FAKE_AGENT=1", "RUNNER_FAKE_LOG="+log)}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{jobsDB: filepath.Join(dir, "jobs.db")}
	q, err := queue.Open(w.jobsDB, func() time.Time { return time.Now().Add(time.Duration(w.skew.Load())) })
	if err != nil {
		t.Fatal(err)
	}
	fr, err := folders.Open(filepath.Join(dir, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	// The lease is control's own (jobs.Lease). The queue has one connection,
	// so a heartbeat waits for every write before it, and a slow commit under
	// the gate's parallel load held one past a 2 s lease: control failed a
	// running folder job (runner_lost) and refused its finish with 409
	// (2026-11-26; TestHeartbeatWaitsOutASlowQueue). A test that needs a
	// lease to lapse moves the queue's clock (skew), never waits.
	cfg := jobs.Config{Queue: q, Token: mainTok, EnrollToken: enrollTok,
		Lease: jobs.Lease, Heartbeat: 50 * time.Millisecond, MaxWait: 200 * time.Millisecond, Folders: fr, Blobs: bs}
	if withRecord {
		cfg.Record = st
	}
	s, err := jobs.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plane := httptest.NewServer(s)
	ctx, cancel := context.WithCancel(context.Background())
	// One runner holds the folders and does their jobs, as on every machine.
	conn := &connector.Connector{Folders: fr, Store: bs, State: filepath.Join(dir, "connector")}
	setDir := filepath.Join(dir, "settings")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}
	set := settings.NewFiles(setDir)
	r, err := Enroll(ctx, Config{Queue: plane.URL, EnrollToken: enrollTok, Name: "test", Capacity: capacity,
		Engine: eng, Store: st, StopWait: 5 * time.Second, Logf: t.Logf, Workdir: filepath.Join(dir, "worktrees"), Folders: fr,
		FolderSide: connector.Jobs{Connector: conn}, Lender: conn, Blobs: bs, BlobSink: bs, Settings: set})
	if err != nil {
		t.Fatal(err)
	}
	w.q, w.st, w.eng, w.plane, w.cancel, w.done, w.folder, w.log, w.r, w.settings = q, st, eng, plane, cancel, make(chan struct{}), t.TempDir(), log, r, set
	go func() { r.Run(ctx); close(w.done) }()
	t.Cleanup(func() {
		w.release()
		cancel()
		<-w.done
		eng.StopAll()
		plane.Close()
		q.Close()
		st.Close()
	})
	if withRecord {
		// Control's one finisher writes each outcome to the record and ends
		// the job, as in agentbus up. Stopped before the store is closed.
		finStop, finDone := make(chan struct{}), make(chan struct{})
		go func() { s.RunFinisher(finStop); close(finDone) }()
		t.Cleanup(func() { close(finStop); <-finDone })
	}
	return w
}

// release lets every held turn end, and every later one pass its hold.
func (w *world) release() { os.WriteFile(w.log+".go", nil, 0o600) }

// runs is what the fake agent logged: one entry a turn.
func (w *world) runs() []fakeRun {
	b, _ := os.ReadFile(w.log)
	var out []fakeRun
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r fakeRun
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
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

// createAsked makes a turn job an agent asked for: delegated work, in a new
// session of the tree person is the root of (queue/lanes.go).
func (w *world) createAsked(t *testing.T, spec queue.Spec) queue.Job {
	t.Helper()
	if spec.Folder == "" && spec.Repo == "" && spec.Session == "" {
		spec.Folder = w.folder
	}
	j, _, err := w.q.CreateTurnLinked(queue.DefaultTeam, "", spec,
		queue.Links{Chain: &queue.Chain{Root: "person", Depth: 1, By: queue.NewID("t_"), BySession: "person"}},
		queue.Limits{MaxDepth: 2, MaxPerTurn: 10, MaxPerTree: 30}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (w *world) job(t *testing.T, id string) queue.Job {
	t.Helper()
	j, err := w.q.Get(queue.DefaultTeam, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func result(t *testing.T, j queue.Job) queue.Result {
	t.Helper()
	var r queue.Result
	if err := json.Unmarshal(j.Result, &r); err != nil {
		t.Fatalf("result %s: %v", j.Result, err)
	}
	return r
}

// Three delegated jobs on a runner of capacity 2: two turns run at once and
// the third waits in the queue. The person's turns are not counted: two more,
// sent meanwhile, run beside them at once. Every job ends finished. Its
// attempt is its turn: the turn the record holds has the attempt's id, in the
// session the queue named when the job was made.
func TestJobsRunAsTurns(t *testing.T) {
	w := newWorld(t, 2)
	var ids []string
	for range 3 {
		ids = append(ids, w.createAsked(t, queue.Spec{Harness: "claude", Prompt: "hold", Model: "haiku"}).ID)
	}
	waitFor(t, "two turns", func() bool { return len(w.runs()) == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := len(w.runs()); n != 2 {
		t.Fatalf("%d delegated turns past capacity 2", n)
	}
	for range 2 {
		ids = append(ids, w.create(t, queue.Spec{Harness: "claude", Prompt: "hold", Model: "haiku"}).ID)
	}
	waitFor(t, "the person's two turns beside them", func() bool { return len(w.runs()) == 4 })
	w.release()
	for _, id := range ids {
		waitFor(t, "job "+id, func() bool { return w.job(t, id).Ended != nil })
		j := w.job(t, id)
		res := result(t, j)
		if j.State != queue.Finished || res.State != "finished" || res.ExitCode == nil || *res.ExitCode != 0 ||
			len(j.Attempts) != 1 || res.Session != j.Session || j.Session == "" {
			t.Fatalf("job %s: %+v %s", id, j, j.Result)
		}
		turn := j.Turn
		if !strings.HasPrefix(turn, "t_") || turn == j.Attempts[0].ID {
			t.Fatalf("turn id %q, attempt %q: want the job's own turn id", turn, j.Attempts[0].ID)
		}
		list, err := w.st.Turns(store.TurnQuery{Session: j.Session, Turn: turn, Limit: 1})
		if err != nil || len(list.Turns) != 1 || list.Turns[0].State != "finished" {
			t.Fatalf("the record has no finished turn %s in session %s: %+v", turn, j.Session, list)
		}
		evs, _ := w.q.Events(queue.DefaultTeam, j.Attempts[0].ID)
		var kinds []string
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		// A folder used as it is (Direct) records no changes: no workspace.changes.
		if strings.Join(kinds, ",") != "attempt.started,turn.created,turn.ended" {
			t.Fatalf("events %v", kinds)
		}
	}
	if args := strings.Join(w.runs()[0].Args, " "); !strings.Contains(args, "--model haiku") {
		t.Fatalf("args %q", args)
	}
}

// A cancel while running stops the turn, and the job ends cancelled.
func TestCancelStopsTheTurn(t *testing.T) {
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Harness: "claude", Prompt: "hold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	if _, err := w.q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cancelled", func() bool { return w.job(t, j.ID).Ended != nil })
	got := w.job(t, j.ID)
	if res := result(t, got); got.State != queue.Cancelled || !res.Stopped {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// A turn the engine refuses fails the job with reason setup, its words and
// its status. Here: a session no event names.
func TestRefusedTurnIsSetupFailure(t *testing.T) {
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Session: "nosuch", Harness: "claude", Prompt: "hi"})
	waitFor(t, "failed", func() bool { return w.job(t, j.ID).Ended != nil })
	got := w.job(t, j.ID)
	res := result(t, got)
	if got.State != queue.Failed || got.Reason != queue.ReasonSetup || res.Refused != "no such session" || res.Status != 404 {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// A job of a kind the runner does not run, such as a "command" job an older
// control made (the person's "!" of 0.4, retired), fails as setup with
// words that name its kind, and runs no turn in its session.
func TestUnknownKindIsRefused(t *testing.T) {
	w := newWorld(t, 1)
	first := w.create(t, queue.Spec{Harness: "claude", Prompt: "hi"})
	waitFor(t, "the first turn", func() bool { return w.job(t, first.ID).Ended != nil })
	runs := len(w.runs())
	j, _, err := w.q.Create(queue.DefaultTeam, "command", "", queue.Spec{Session: ids.SessionID(first.Session)}, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refused", func() bool { return w.job(t, j.ID).Ended != nil })
	got := w.job(t, j.ID)
	res := result(t, got)
	if got.State != queue.Failed || got.Reason != queue.ReasonSetup || !strings.Contains(res.Refused, `kind "command"`) {
		t.Fatalf("%+v %s", got, got.Result)
	}
	if n := len(w.runs()); n != runs {
		t.Errorf("%d agent runs, want %d: the command job ran as a turn", n, runs)
	}
}

// A turn whose agent exits with a failure fails the job with reason agent.
func TestAgentFailureFailsTheJob(t *testing.T) {
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Harness: "claude", Prompt: "exit 3"})
	waitFor(t, "failed", func() bool { return w.job(t, j.ID).Ended != nil })
	got := w.job(t, j.ID)
	if res := result(t, got); got.State != queue.Failed || got.Reason != queue.ReasonAgent || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// A runner shut down mid-turn interrupts the turn, as the service stopping
// does: not a stop by a person, so the turn's exit code says the signal. The
// job fails with reason runner_stopped.
func TestShutdownStopsTurns(t *testing.T) {
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Harness: "claude", Prompt: "hold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	w.cancel()
	<-w.done
	got := w.job(t, j.ID)
	if res := result(t, got); got.State != queue.Failed || got.Reason != ReasonRunnerStopped || res.Stopped ||
		res.ExitCode == nil || *res.ExitCode != 143 {
		t.Fatalf("%+v %s", got, got.Result)
	}
}

// A session's first job names a repo and a base: the runner makes a worktree,
// the turn runs in it, and the job's result lists what changed between the
// turn's checkpoints, files git ignores included. The session's next job runs in the same worktree on the same
// runner, and waits while the first one runs. Closing the session removes the
// worktree; a job's end never does.
func TestWorktreeSession(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	write(t, filepath.Join(repo, "app.txt"), "v1\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")

	w := newWorld(t, 2)
	first := w.create(t, queue.Spec{Harness: "claude", Repo: repo, Base: "main", Prompt: "write app.txt v2\nwrite test.log ran\nhold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	// The next job of the session is queued now, and waits for the first.
	next := w.create(t, queue.Spec{Session: ids.SessionID(first.Session), Harness: "claude", Prompt: "write second.txt two"})
	time.Sleep(300 * time.Millisecond)
	if got := w.job(t, next.ID); got.State != queue.Queued || len(w.runs()) != 1 {
		t.Fatalf("the next job did not wait: %s, %d turns", got.State, len(w.runs()))
	}
	w.release()
	waitFor(t, "finished", func() bool { return w.job(t, first.ID).Ended != nil })
	got := w.job(t, first.ID)
	var res struct {
		Session string            `json:"session"`
		Folder  string            `json:"folder"`
		Changes workspace.Changes `json:"changes"`
	}
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatal(err)
	}
	// The copy is named after its session.
	wantDir := filepath.Join(w.r.cfg.Workdir, first.Session)
	if res.Folder != "" || got.Workspace == nil || got.Workspace.Space != first.Session || w.runs()[0].Cwd != wantDir && !sameDir(w.runs()[0].Cwd, wantDir) {
		t.Fatalf("folder %q, turn ran in %q, want %q", res.Folder, w.runs()[0].Cwd, wantDir)
	}
	m := byPath(res.Changes)
	if res.Changes.Kind != "snapshot" || res.Changes.Count != 2 || m["app.txt"].Change != "modified" || m["app.txt"].Diff == nil ||
		m["test.log"].Change != "added" || res.Changes.From != isolation.Base || res.Changes.To != got.Turn {
		t.Fatalf("changes %+v", res.Changes)
	}
	if got.Workspace == nil || got.Workspace.Kind != queue.WorkspaceWorktree || got.Workspace.Runner != w.r.ID() {
		t.Fatalf("workspace %+v", got.Workspace)
	}

	waitFor(t, "finished", func() bool { return w.job(t, next.ID).Ended != nil })
	n := w.job(t, next.ID)
	var res2 struct {
		Changes workspace.Changes `json:"changes"`
	}
	_ = json.Unmarshal(n.Result, &res2)
	if n.State != queue.Finished || !sameDir(w.runs()[1].Cwd, wantDir) || res2.Changes.Count != 1 || res2.Changes.Files[0].Path != "second.txt" {
		t.Fatalf("second: %s, ran in %q, changes %+v", n.State, w.runs()[1].Cwd, res2.Changes)
	}

	if _, err := w.q.CloseSession(queue.DefaultTeam, ids.WorkspaceID(first.Session)); err != nil {
		t.Fatal(err)
	}
	// Close is retired (B14): it marks the session closed and wakes the runner,
	// but hands it no cleanup, so the worktree is kept. Give the runner poll
	// cycles to act (it must not), then confirm nothing was removed.
	time.Sleep(500 * time.Millisecond)
	list, _ := w.q.Sessions(queue.DefaultTeam, 10)
	if len(list) != 1 || list[0].Closed == nil || list[0].Removed != nil {
		t.Fatalf("session after close: %+v", list)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Fatalf("the worktree was removed by close: %v", err)
	}
}

func sameDir(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra != "" && ra == rb
}

// A continuation whose source folder was removed still loads its workspace copy
// and records its changeset. The copy is self-contained, so it needs no source
// folder: prepare loads it before it stats the folder the turn names (P2 #1).
// Without the fix the continuation fails setup when it stats the gone folder.
func TestContinuationWhoseSourceFolderIsGone(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "v1\n")
	w := newWorld(t, 2)
	// Pin the copy backend so the test reads one way on every platform (auto
	// picks overlay on Linux). Either backend makes an independent copy -- the
	// copy backend duplicates the folder, overlay snapshots it into its own
	// layer -- so neither needs the source once the copy is made.
	p := settings.DefaultPolicy()
	p.Isolation = settings.IsolationCopy
	if _, _, err := w.settings.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	first := w.create(t, queue.Spec{Harness: "claude", Folder: src, Isolate: true, Prompt: "write b.txt two\nhold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	// The continuation names the same folder and sandbox mode, as control stamps
	// them from the workspace record; it waits behind the first in the lane.
	next := w.create(t, queue.Spec{Session: ids.SessionID(first.Session), Harness: "claude", Folder: src, Isolate: true, Prompt: "write c.txt three"})
	// The copy exists now (the first turn made it), so the source folder is no
	// longer needed. Remove it before the continuation runs.
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	w.release()
	waitFor(t, "the continuation finished", func() bool { return w.job(t, next.ID).Ended != nil })
	n := w.job(t, next.ID)
	var res struct {
		Changeset string            `json:"changeset"`
		Changes   workspace.Changes `json:"changes"`
	}
	_ = json.Unmarshal(n.Result, &res)
	if n.State != queue.Finished {
		t.Fatalf("continuation state %s (reason %s): %s", n.State, n.Reason, n.Result)
	}
	if res.Changeset == "" || res.Changes.Count != 1 || len(res.Changes.Files) == 0 || res.Changes.Files[0].Path != "c.txt" {
		t.Fatalf("continuation changeset %q changes %+v", res.Changeset, res.Changes)
	}
}

// A discard whose workspace never made a copy (a lost or stopped first turn) is
// refused, not finished: so control records no workspace.discarded for a discard
// that reset nothing.
func TestDiscardWithNoCopyIsRefused(t *testing.T) {
	w := newWorld(t, 1)
	ws := ids.WorkspaceID("ws_0000000000000001")
	dj, _, err := w.q.Create(queue.DefaultTeam, queue.KindDiscard, "",
		queue.Spec{Workspace: ws, Session: ids.SessionID(ws)}, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the discard ended", func() bool { return w.job(t, dj.ID).Ended != nil })
	if d := w.job(t, dj.ID); d.State == queue.Finished {
		t.Fatalf("a discard with no copy finished (it would record a spurious workspace.discarded): %s", d.Result)
	}
}

// A discard resets the workspace's copy to base (nothing applied), through the
// copy's private git repo, dropping the turn's unapplied changes, and never
// touches the person's folder.
func TestDiscardResetsTheCopyAndLeavesTheFolder(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "a1")
	w := newWorld(t, 2)
	p := settings.DefaultPolicy()
	p.Isolation = settings.IsolationCopy
	if _, _, err := w.settings.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	first := w.create(t, queue.Spec{Harness: "claude", Folder: src, Isolate: true, Prompt: "write a.txt a2\nwrite b.txt b1\nhold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	w.release()
	waitFor(t, "the turn finished", func() bool { return w.job(t, first.ID).Ended != nil })

	dj, _, err := w.q.Create(queue.DefaultTeam, queue.KindDiscard, "",
		queue.Spec{Session: ids.SessionID(first.Session), Workspace: ids.WorkspaceID(first.Session)},
		queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the discard finished", func() bool { return w.job(t, dj.ID).Ended != nil })
	if d := w.job(t, dj.ID); d.State != queue.Finished {
		t.Fatalf("discard %s: %s", d.State, d.Result)
	}

	rd := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	// The copy is reset to base: the turn's edits are gone.
	copyDir := filepath.Join(w.r.cfg.Workdir, first.Session)
	if a, b := rd(filepath.Join(copyDir, "a.txt")), rd(filepath.Join(copyDir, "b.txt")); a != "a1" || b != "<missing>" {
		t.Fatalf("copy not reset to base: a=%q b=%q", a, b)
	}
	// The person's folder is untouched.
	if a, b := rd(filepath.Join(src, "a.txt")), rd(filepath.Join(src, "b.txt")); a != "a1" || b != "<missing>" {
		t.Fatalf("the discard touched the person's folder: a=%q b=%q", a, b)
	}
}

// A base that does not exist fails the job as setup, before any turn.
func TestBadBaseIsSetup(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Harness: "claude", Repo: repo, Base: "main", Prompt: "x"})
	waitFor(t, "failed", func() bool { return w.job(t, j.ID).Ended != nil })
	if got := w.job(t, j.ID); got.Reason != queue.ReasonSetup || !strings.Contains(string(got.Result), "worktree could not be made") {
		t.Fatalf("%+v %s", got, got.Result)
	}
	if len(w.runs()) != 0 {
		t.Fatal("a turn started")
	}
}

// B15: TestReviewRunsInItsOwnCopy retired. A review is a normal turn (the
// review_turn app), running in a fresh copy like any asked turn and reading the
// reviewed turn's diff through agentbus-api; there is no review kind or verdict.
// An attempt whose lease ran out is stale: its next heartbeat says so, and
// the runner stops its turn and finishes nothing, as the queue has already
// failed the job with runner_lost.
func TestStaleAttemptStopsTheTurn(t *testing.T) {
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Harness: "claude", Prompt: "hold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	turn := w.job(t, j.ID).Turn
	w.skew.Store(int64(time.Minute)) // the queue's clock passes the lease
	waitFor(t, "the turn stopped", func() bool {
		list, _ := w.st.Turns(store.TurnQuery{Turn: turn, Limit: 1})
		return len(list.Turns) == 1 && list.Turns[0].State != "running"
	})
	got := w.job(t, j.ID)
	if got.State != queue.Failed || got.Reason != queue.ReasonRunnerLost {
		t.Fatalf("%+v", got)
	}
	list, _ := w.st.Turns(store.TurnQuery{Turn: turn, Limit: 1})
	if !list.Turns[0].Stopped {
		t.Fatalf("the turn ended without a stop: %+v", list.Turns[0])
	}
}

// A file written to a session's copy between turns, as by a turn whose
// runner was lost or what it left running, is in the next turn's diff: the
// diff starts at the copy's latest checkpoint, so nothing reaches an apply
// unseen.
func TestCopyWrittenBetweenTurnsIsInTheNextDiff(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "app.txt"), "v1\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")
	w := newWorld(t, 1)
	first := w.create(t, queue.Spec{Harness: "claude", Repo: repo, Base: "main", Prompt: "write app.txt v2"})
	waitFor(t, "finished", func() bool { return w.job(t, first.ID).Ended != nil })
	f := w.job(t, first.ID)
	if f.State != queue.Finished {
		t.Fatalf("first: %s %s", f.State, f.Reason)
	}
	write(t, filepath.Join(w.r.cfg.Workdir, first.Session, "stray.txt"), "left behind\n")
	next := w.create(t, queue.Spec{Session: ids.SessionID(first.Session), Harness: "claude", Prompt: "write second.txt two"})
	waitFor(t, "finished", func() bool { return w.job(t, next.ID).Ended != nil })
	var res struct {
		Changes workspace.Changes `json:"changes"`
	}
	n := w.job(t, next.ID)
	_ = json.Unmarshal(n.Result, &res)
	m := byPath(res.Changes)
	if n.State != queue.Finished || res.Changes.Count != 2 || m["second.txt"].Change != "added" || m["stray.txt"].Change != "added" ||
		res.Changes.From != f.Turn {
		t.Fatalf("second: %s, changes %+v", n.State, res.Changes)
	}
}

// Folders typed at once are each listed once their jobs end: the runner's
// reports go one at a time, each read as it is sent, so an older list never
// reaches control after a newer one.
func TestFoldersTypedAtOnceAllListed(t *testing.T) {
	w := newWorld(t, 1)
	var paths []string
	for i := range queue.FolderCapacity {
		p := filepath.Join(t.TempDir(), fmt.Sprintf("typed-%d", i))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for round := range 5 {
		var ids []string
		for _, p := range paths {
			// The folder is there before its job is queued, so a fast
			// runner never checks it first (codex, evt_18366367fd3ff929).
			if err := os.MkdirAll(fmt.Sprintf("%s-%d", p, round), 0o755); err != nil {
				t.Fatal(err)
			}
			j, err := w.q.RegisterFolder(queue.DefaultTeam, w.r.cfg.Folders.Machine(), fmt.Sprintf("%s-%d", p, round))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, j.ID)
		}
		for _, id := range ids {
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				j, err := w.q.Get(queue.DefaultTeam, id)
				if err == nil && j.Ended != nil {
					if j.State != queue.Finished {
						t.Fatalf("folder job %s ended %s: %s", id, j.State, j.Result)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("folder job %s did not end", id)
				}
			}
			w.q.ForgetFolder(queue.DefaultTeam, id)
		}
		list, err := w.q.Folders(queue.DefaultTeam)
		if err != nil {
			t.Fatal(err)
		}
		if want := (round + 1) * len(paths); len(list) != want {
			t.Fatalf("round %d: %d folders listed once every job ended, want %d", round, len(list), want)
		}
	}
}

// A runner newer than its control refuses to start, learning control's version
// from the enroll reply; against equal or newer control it starts. (Milestone
// A fix: upgrade control first.)
func TestEnrollRefusesOlderControl(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := run.New(run.Config{Store: st, BaseURL: "http://127.0.0.1:1", RunGrace: time.Second,
		Wire: func(string, string, run.Access) (run.Launch, error) { return run.Launch{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(controlVersion, runnerVersion, name, wt string) error {
		s, err := jobs.New(jobs.Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Version: controlVersion})
		if err != nil {
			t.Fatal(err)
		}
		plane := httptest.NewServer(s)
		defer plane.Close()
		_, err = Enroll(context.Background(), Config{Queue: plane.URL, EnrollToken: enrollTok, Name: name, Capacity: 1,
			Version: runnerVersion, Engine: eng, Store: st, Workdir: filepath.Join(dir, wt)})
		return err
	}
	// Control before 0.6 sends no version at all (as release v0.5.10 does not):
	// absent is older, so a 0.6 runner is refused.
	if err := enroll("", "0.6.0", "a", "wt_a"); err == nil || !strings.Contains(err.Error(), "upgrade control first") {
		t.Fatalf("a newer runner did not refuse pre-0.6 control: %v", err)
	}
	// A control that sends an older release is refused too.
	if err := enroll("0.6.0", "0.7.0", "a2", "wt_a2"); err == nil || !strings.Contains(err.Error(), "upgrade control first") {
		t.Fatalf("a newer runner did not refuse an older-release control: %v", err)
	}
	// Equal control: it starts.
	if err := enroll("0.6.0", "0.6.0", "b", "wt_b"); err != nil {
		t.Fatalf("a runner against equal control was refused: %v", err)
	}
	// Newer control (an old runner on new control): it starts.
	if err := enroll("0.7.0", "0.6.0", "c", "wt_c"); err != nil {
		t.Fatalf("a runner against newer control was refused: %v", err)
	}
}

// A saved identity (a restart, or agentbus login) checks control on the
// heartbeat path too: a 0.6 runner refuses a pre-0.6 control it heartbeats to.
func TestSavedIdentityRefusesOlderControl(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := run.New(run.Config{Store: st, BaseURL: "http://127.0.0.1:1", RunGrace: time.Second,
		Wire: func(string, string, run.Access) (run.Launch, error) { return run.Launch{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	// A runner control already knows, and its saved identity.
	tok := queue.NewID("abr_")
	rn, err := q.RegisterRunner(queue.DefaultTeam, "saved", nil, 1, queue.HashToken(tok), nil)
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(dir, "runner.state")
	if err := SaveIdentity(stateFile, rn, tok, int64(50), int64(2000)); err != nil {
		t.Fatal(err)
	}
	// Control before 0.6 (sends no version): the heartbeat succeeds, then the
	// runner refuses.
	s, err := jobs.New(jobs.Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Version: ""})
	if err != nil {
		t.Fatal(err)
	}
	plane := httptest.NewServer(s)
	defer plane.Close()
	_, err = Enroll(context.Background(), Config{Queue: plane.URL, EnrollToken: enrollTok, Name: "saved", Capacity: 1,
		Version: "0.6.0", StateFile: stateFile, Engine: eng, Store: st, Workdir: filepath.Join(dir, "wt")})
	if err == nil || !strings.Contains(err.Error(), "upgrade control first") {
		t.Fatalf("a saved-identity runner did not refuse pre-0.6 control: %v", err)
	}
}

// A close removes the workspace's copy to reclaim disk, leaving the person's
// folder untouched, and the next turn rebuilds a fresh copy from the folder.
func TestCloseRemovesTheCopyAndRebuilds(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "a1")
	w := newWorld(t, 2, true)
	p := settings.DefaultPolicy()
	p.Isolation = settings.IsolationCopy
	if _, _, err := w.settings.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	first := w.create(t, queue.Spec{Harness: "claude", Folder: src, Isolate: true, Prompt: "write b.txt b1\nhold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	w.release()
	waitFor(t, "the turn finished", func() bool { return w.job(t, first.ID).Ended != nil })

	copyDir := filepath.Join(w.r.cfg.Workdir, first.Session)
	if _, err := os.Stat(copyDir); err != nil {
		t.Fatalf("the copy was not made: %v", err)
	}

	cj, _, err := w.q.Create(queue.DefaultTeam, queue.KindClose, "",
		queue.Spec{Session: ids.SessionID(first.Session), Workspace: ids.WorkspaceID(first.Session)},
		queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the close finished", func() bool { return w.job(t, cj.ID).Ended != nil })
	if c := w.job(t, cj.ID); c.State != queue.Finished {
		t.Fatalf("close %s: %s", c.State, c.Result)
	}
	// The copy is gone; the person's folder is untouched.
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Fatalf("the copy was not removed: stat err %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "a.txt")); err != nil || string(b) != "a1" {
		t.Fatalf("the close touched the person's folder: %q err %v", b, err)
	}

	// The next turn of the session rebuilds a fresh copy from the folder.
	second := w.create(t, queue.Spec{Session: ids.SessionID(first.Session), Harness: "claude", Folder: src, Isolate: true, Prompt: "write c.txt c1"})
	waitFor(t, "the second turn finished", func() bool { return w.job(t, second.ID).Ended != nil })
	if n := w.job(t, second.ID); n.State != queue.Finished {
		t.Fatalf("rebuilt turn %s (reason %s): %s", n.State, n.Reason, n.Result)
	}
	if _, err := os.Stat(copyDir); err != nil {
		t.Fatalf("the next turn did not rebuild the copy: %v", err)
	}
}
