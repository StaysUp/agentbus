//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
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
	q      *queue.Queue
	st     *store.Store
	eng    *run.Engine
	plane  *httptest.Server
	cancel context.CancelFunc
	done   chan struct{}
	folder string // a folder for first jobs
	log    string // the fake agent's log
	r      *Runner
}

func newWorld(t *testing.T, capacity int) *world {
	t.Helper()
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
	eng, err := run.New(run.Config{Store: st, BaseURL: "http://127.0.0.1:1", MaxRuns: capacity, RunGrace: time.Second,
		Wire: func(agent, baseURL string, _ run.Access) (run.Launch, error) {
			return run.Launch{Path: self, Env: append(os.Environ(), "RUNNER_FAKE_AGENT=1", "RUNNER_FAKE_LOG="+log)}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), func() time.Time { return time.Now().Add(time.Duration(w.skew.Load())) })
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
	s, err := jobs.New(jobs.Config{Queue: q, Token: mainTok, EnrollToken: enrollTok,
		Lease: 2 * time.Second, Heartbeat: 50 * time.Millisecond, MaxWait: 200 * time.Millisecond, Folders: fr, Blobs: bs})
	if err != nil {
		t.Fatal(err)
	}
	plane := httptest.NewServer(s)
	ctx, cancel := context.WithCancel(context.Background())
	conn := &connector.Connector{Folders: fr, Store: bs, State: filepath.Join(dir, "connector")}
	cn, err := Enroll(ctx, Config{Queue: plane.URL, EnrollToken: enrollTok, Name: "connector", Capacity: 2,
		Role: queue.RoleConnector, FolderSide: connector.Jobs{Connector: conn}, Folders: fr, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	cnDone := make(chan struct{})
	go func() { cn.Run(ctx); close(cnDone) }()
	t.Cleanup(func() { <-cnDone })
	r, err := Enroll(ctx, Config{Queue: plane.URL, EnrollToken: enrollTok, Name: "test", Capacity: capacity,
		Engine: eng, Store: st, StopWait: 5 * time.Second, Logf: t.Logf, Workdir: filepath.Join(dir, "worktrees"), Folders: fr,
		Lender: conn, Blobs: bs, BlobSink: bs})
	if err != nil {
		t.Fatal(err)
	}
	w.q, w.st, w.eng, w.plane, w.cancel, w.done, w.folder, w.log, w.r = q, st, eng, plane, cancel, make(chan struct{}), t.TempDir(), log, r
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

// Three jobs on a runner of capacity 2: two turns run at once and the third
// waits in the queue. Every job ends finished. Its attempt is its turn: the
// turn the record holds has the attempt's id, in the session the queue named
// when the job was made.
func TestJobsRunAsTurns(t *testing.T) {
	w := newWorld(t, 2)
	var ids []string
	for range 3 {
		ids = append(ids, w.create(t, queue.Spec{Harness: "claude", Prompt: "hold", Model: "haiku"}).ID)
	}
	waitFor(t, "two turns", func() bool { return len(w.runs()) == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := len(w.runs()); n != 2 {
		t.Fatalf("%d turns past capacity 2", n)
	}
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
		if strings.Join(kinds, ",") != "attempt.started,turn.created,turn.ended,workspace.changes" {
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
	j := w.create(t, queue.Spec{Session: "nosuch", Prompt: "hi"})
	waitFor(t, "failed", func() bool { return w.job(t, j.ID).Ended != nil })
	got := w.job(t, j.ID)
	res := result(t, got)
	if got.State != queue.Failed || got.Reason != queue.ReasonSetup || res.Refused != "no such session" || res.Status != 404 {
		t.Fatalf("%+v %s", got, got.Result)
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
	next := w.create(t, queue.Spec{Session: first.Session, Prompt: "write second.txt two"})
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

	if _, err := w.q.CloseSession(queue.DefaultTeam, first.Session); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removed", func() bool {
		list, _ := w.q.Sessions(queue.DefaultTeam, 10)
		return len(list) == 1 && list[0].Removed != nil
	})
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there: %v", err)
	}
}

func sameDir(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra != "" && ra == rb
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

// A review of a worktree job runs read-only in the author's worktree, in a
// session of its own. The verdict is read from the reviewer's last reply in
// the record.
func TestReviewRunsInTheAuthorsWorktree(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "app.txt"), "v1\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "one")
	w := newWorld(t, 2)
	w.release()
	author := w.create(t, queue.Spec{Harness: "claude", Repo: repo, Base: "main", Prompt: "write app.txt v2"})
	waitFor(t, "finished", func() bool { return w.job(t, author.ID).Ended != nil })

	// The reviewer's reply, as the gateway would store it: written into the
	// record as soon as the review's turn has started, before it ends. The
	// review's prompt is the control plane's, so the fake holds by file.
	os.Remove(w.log + ".go")
	write(t, w.log+".hold", "")
	rev, _, err := w.q.Review(queue.DefaultTeam, author.ID, "claude", "sonnet", "", false)
	if err != nil {
		t.Fatal(err)
	}
	var revTurn string
	waitFor(t, "the review's turn", func() bool {
		j := w.job(t, rev.ID)
		if len(j.Attempts) == 0 || len(w.runs()) < 2 {
			return false
		}
		revTurn = j.Turn
		return true
	})
	list, _ := w.st.Turns(store.TurnQuery{Turn: revTurn, Limit: 1})
	if len(list.Turns) != 1 {
		t.Fatal("no review turn in the record")
	}
	reply := "Checked app.txt.\n```json\n{\"verdict\":\"pass\",\"findings\":[{\"severity\":\"P2\",\"file\":\"app.txt\",\"line\":1,\"summary\":\"no test\"}]}\n```"
	if _, _, err := w.st.Append(event.Event{ID: "evt_r1", Kind: event.ModelResponded, Time: time.Now().UTC(),
		Session: list.Turns[0].Session, Turn: revTurn, Text: reply}); err != nil {
		t.Fatal(err)
	}
	w.release()
	waitFor(t, "review finished", func() bool { return w.job(t, rev.ID).Ended != nil })
	got := w.job(t, rev.ID)
	if got.Verdict == nil || *got.Verdict != queue.VerdictPass || len(got.Findings) != 1 || got.Findings[0].Severity != "P2" {
		t.Fatalf("review: %+v %s", got, got.Result)
	}
	r := w.runs()[1]
	args := strings.Join(r.Args, " ")
	if !sameDir(r.Cwd, filepath.Join(w.r.cfg.Workdir, author.Session)) || !strings.Contains(args, "--tools Read,Grep,Glob") ||
		!strings.Contains(args, "--model sonnet") || strings.Contains(args, "--resume") {
		t.Fatalf("review ran in %q with %q", r.Cwd, args)
	}
	if res := result(t, got); res.Session == author.Session || res.Session == "" {
		t.Fatalf("the review ran in session %q, the author's is %q", res.Session, author.Session)
	}
}

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
	next := w.create(t, queue.Spec{Session: first.Session, Prompt: "write second.txt two"})
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
