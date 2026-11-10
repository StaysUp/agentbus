package queue

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func asked(session, turn, root string, depth int) Links {
	return Links{Asker: &Asker{Session: session, Turn: turn}, Chain: &Chain{Root: root, Depth: depth}}
}

// The default caps, as permissions.toml writes them.
const MaxDepth, MaxAskedPerTurn, MaxAskedPerRoot = 2, 10, 30

var testLimits = Limits{MaxDepth: MaxDepth, MaxPerTurn: MaxAskedPerTurn, MaxPerTree: MaxAskedPerRoot}

func askTurn(q *Queue, l Links, key string) (Job, error) {
	j, _, err := q.CreateTurnLinked(DefaultTeam, key, Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"}, l, testLimits)
	return j, err
}

// The caps on asked work refuse past each limit, with a reason, and count
// asked work only: the person's own jobs and delivered results never count.
func TestAskedCaps(t *testing.T) {
	q, _ := open(t)
	if _, err := askTurn(q, asked("p", "t_1", "p", MaxDepth+1), ""); !isLimit(err, "levels") {
		t.Fatalf("too deep: %v", err)
	}
	if _, err := askTurn(q, Links{Asker: &Asker{Session: "p", Turn: "t_1"}}, ""); !errors.Is(err, ErrBadState) {
		t.Fatalf("asked without a chain: %v", err)
	}
	for i := range MaxAskedPerTurn {
		if _, err := askTurn(q, asked("p", "t_1", "p", 1), ""); err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
		job(t, q, "") // the person's own work is not counted
	}
	if _, err := askTurn(q, asked("p", "t_1", "p", 1), ""); !isLimit(err, "one turn") {
		t.Fatalf("child past the per-turn cap: %v", err)
	}
	// A retry of a job already made is not a new one.
	first, err := askTurn(q, asked("p", "t_2", "p", 1), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := askTurn(q, asked("p", "t_2", "p", 1), "k1"); err != nil || again.ID != first.ID {
		t.Fatalf("retry: %v %v", again.ID, err)
	}
	// The whole tree: other turns, deeper levels, all count against the root.
	n := MaxAskedPerTurn + 1
	for i := 0; n < MaxAskedPerRoot; i++ {
		turn := "t_x" + string(rune('a'+i/5))
		if _, err := askTurn(q, asked("c", turn, "p", 2), ""); err != nil {
			t.Fatalf("tree job %d: %v", n, err)
		}
		n++
	}
	if _, err := askTurn(q, asked("c", "t_new", "p", 2), ""); !isLimit(err, "in all") {
		t.Fatalf("job past the tree cap: %v", err)
	}
	// Another tree is not affected.
	if _, err := askTurn(q, asked("q", "t_1", "q", 1), ""); err != nil {
		t.Fatalf("another tree: %v", err)
	}
}

func isLimit(err error, word string) bool {
	var l *LimitError
	return errors.As(err, &l) && strings.Contains(l.Msg, word) && strings.Contains(l.Msg, "[limits]")
}

// SessionRoot names the tree a session was asked for in; a session the
// person started has none.
func TestSessionRoot(t *testing.T) {
	q, _ := open(t)
	c, err := askTurn(q, asked("p", "t_1", "p", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if root, err := q.SessionRoot(DefaultTeam, c.Session); err != nil || root != "p" {
		t.Fatalf("asked session: %q %v", root, err)
	}
	own := job(t, q, "")
	if root, err := q.SessionRoot(DefaultTeam, own.Session); err != nil || root != "" {
		t.Fatalf("the person's session: %q %v", root, err)
	}
	// A job stored with a path names no reference.
	if ref, con, _, err := q.SessionFolder(DefaultTeam, c.Session); err != nil || ref != "" || con != "" {
		t.Fatalf("folder: %q %q %v", ref, con, err)
	}
}

// Asked work that ends is delivered once, as a turn job in the asking
// session, after the turn running there; the person's own work never is.
func TestDeliver(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	pj, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, pa.ID, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	cj, ca := take(t, q, r)
	if cj.ID != child.ID {
		t.Fatalf("took %s, want the child", cj.ID)
	}
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if n, err := q.Deliver(DefaultTeam, nil); err != nil || n != 0 {
		t.Fatalf("before the child ends: %d %v", n, err)
	}
	res := `{"session":"` + child.Session + `","state":"finished","reply":"short","reply_text":"the whole answer","folder":"/w/1",` +
		`"changes":{"count":2,"files":[{"path":"a.go","change":"modified"},{"path":"b.go","change":"added"}]}}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.Deliver(DefaultTeam, nil); err != nil {
			t.Fatal(err)
		}
	}
	ds, err := q.List(DefaultTeam, Filter{Session: parent.Session}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var d *Job
	for i := range ds {
		if ds[i].Links.ResultOf != nil {
			if d != nil {
				t.Fatal("delivered twice")
			}
			d = &ds[i]
		}
	}
	if d == nil {
		t.Fatal("no delivery")
	}
	if d.State != Queued || d.Spec.Session != parent.Session || d.Spec.Harness != "" || d.Links.ResultOf.Job != child.ID ||
		d.Links.ResultOf.Session != child.Session || d.Links.ResultOf.Turn != ca.ID || d.Links.ResultOf.Agent != "claude" ||
		d.Links.Chain == nil || d.Links.Chain.Depth != 1 || d.Links.Asker != nil {
		t.Fatalf("delivery: %+v %+v", d, d.Links)
	}
	for _, want := range []string{"[AgentBus]", "not from the person", "Agent: claude", "Folder: /w/1", "Changed files: 2",
		"- a.go (modified)", "- b.go (added)", "the whole answer", "Session: " + child.Session} {
		if !strings.Contains(d.Spec.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, d.Spec.Prompt)
		}
	}
	// It waits for the parent's turn, then runs.
	if _, _, ok, _, err := q.Take(r.ID, lease); err != nil || ok {
		t.Fatalf("took the delivery while the parent runs: %v %v", ok, err)
	}
	if _, err := q.End(r.ID, pa.ID, pa.Number, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
	if got, _ := take(t, q, r); got.ID != d.ID {
		t.Fatalf("took %s, want the delivery", got.ID)
	}
	// The person's own job ended too, and is never delivered.
	if n, err := q.Deliver(DefaultTeam, nil); err != nil || n != 0 {
		t.Fatalf("after: %d %v", n, err)
	}
	_ = pj
}

// A review's result says its verdict and findings; a reply past the cap is
// cut; a refused start says why.
func TestResultPrompt(t *testing.T) {
	v := VerdictFail
	p := ResultPrompt(Job{ID: "job_1", Kind: KindReview, State: Finished, Verdict: &v, Spec: Spec{AgentID: "codex", Model: "m-1"},
		Findings: []Finding{{Severity: "P1", File: "a.go", Line: 3, Summary: "bad\nthing"}},
		Result:   json.RawMessage(`{"session":"rs"}`)}, func(id string) string { return map[string]string{"codex": "Codex"}[id] })
	for _, want := range []string{"the review you asked for", "Agent: Codex (id codex, model m-1)", "Verdict: fail", "- P1 a.go:3 bad thing", "Session: rs"} {
		if !strings.Contains(p, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, p)
		}
	}
	long := strings.Repeat("x", MaxResultReply+100)
	b, _ := json.Marshal(map[string]string{"reply_text": long})
	if p := ResultPrompt(Job{Kind: KindTurn, State: Finished, Result: b}, nil); strings.Contains(p, long) || !strings.Contains(p, "cut here") {
		t.Error("a long reply was not cut")
	}
	p = ResultPrompt(Job{Kind: KindTurn, State: Failed, Reason: ReasonSetup, Result: json.RawMessage(`{"refused":"no such agent"}`)}, nil)
	if !strings.Contains(p, "failed (setup)") || !strings.Contains(p, "Error: no such agent") {
		t.Errorf("refused prompt:\n%s", p)
	}
}

// A caller's key can never take a delivery's: a job made first with a
// look-alike key does not stop the result from being delivered.
func TestDeliverKeyIsOurs(t *testing.T) {
	q, _ := open(t)
	child, err := askTurn(q, asked("p", "t_1", "p", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Trim(ResultKey(child.ID), "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:-") == "" {
		t.Fatalf("%s is a key a caller may send", ResultKey(child.ID))
	}
	for _, k := range []string{"result_" + child.ID, "result:" + child.ID} {
		if _, _, err := q.Create(DefaultTeam, KindTurn, k, Spec{Session: "p", Prompt: "squat"}, Links{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Cancel(DefaultTeam, child.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.Deliver(DefaultTeam, nil); err != nil || n != 1 {
		t.Fatalf("deliver: %d %v", n, err)
	}
}

// A retry of asked work with the same key gets the same job from a later turn
// of the same tree; another tree gets a conflict.
func TestAskedRetryAcrossTurns(t *testing.T) {
	q, _ := open(t)
	first, err := askTurn(q, asked("p", "t_1", "p", 1), "k")
	if err != nil {
		t.Fatal(err)
	}
	again, err := askTurn(q, asked("p", "t_2", "p", 1), "k")
	if err != nil || again.ID != first.ID {
		t.Fatalf("a later turn's retry: %v %v", again.ID, err)
	}
	if _, err := askTurn(q, asked("x", "t_9", "x", 1), "k"); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("another tree: %v", err)
	}
}

// An asked review's own session joins its tree once its turn has started,
// while it still runs.
func TestSessionRootOfReview(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	j := job(t, q, "")
	_, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	res := `{"session":"` + j.Session + `","changes":{"kind":"snapshot","count":0,"files":[]}}`
	if _, err := q.End(r.ID, a.ID, a.Number, Finish{Status: Finished, Result: json.RawMessage(res),
		Session: j.Session, Workspace: &Workspace{Kind: WorkspaceFolder, Folder: "/x"}}); err != nil {
		t.Fatal(err)
	}
	rv, _, err := q.ReviewLinked(DefaultTeam, j.ID, Spec{Harness: "codex"}, "", false, asked(j.Session, "t_1", j.Session, 1), testLimits)
	if err != nil {
		t.Fatal(err)
	}
	_, ra := take(t, q, r)
	if _, _, err := q.Start(r.ID, ra.ID, ra.Number, lease); err != nil {
		t.Fatal(err)
	}
	if root, err := q.SessionRoot(DefaultTeam, "rev1"); err != nil || root != "" {
		t.Fatalf("before its turn started: %q %v", root, err)
	}
	if _, _, err := q.AddEvents(r.ID, ra.ID, ra.Number, 1, []AttemptEvent{{Seq: 1, Time: time.Now(), Kind: "turn.created",
		Data: json.RawMessage(`{"session":"rev1","turn":"` + ra.ID + `"}`)}}); err != nil {
		t.Fatal(err)
	}
	if root, err := q.SessionRoot(DefaultTeam, "rev1"); err != nil || root != j.Session {
		t.Fatalf("review session root while it runs: %q %v (review %s)", root, err, rv.ID)
	}
	if _, err := q.End(r.ID, ra.ID, ra.Number, Finish{Status: Finished, Result: json.RawMessage(`{"session":"rev1","reply_text":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	if root, err := q.SessionRoot(DefaultTeam, "rev1"); err != nil || root != j.Session {
		t.Fatalf("review session root after: %q %v", root, err)
	}
}

// However large a review's findings, the delivered prompt stays bounded and
// holds no NUL byte, so its turn can start.
func TestResultPromptBounded(t *testing.T) {
	v := VerdictFail
	var fs []Finding
	for range 200 {
		fs = append(fs, Finding{Severity: "P1", File: strings.Repeat("d/", 2000), Summary: strings.Repeat("y\x00", 5000)})
	}
	p := ResultPrompt(Job{Kind: KindReview, State: Finished, Verdict: &v, Findings: fs,
		Result: json.RawMessage(`{"error":"` + strings.Repeat("e", 100000) + `"}`)}, nil)
	if len(p) > MaxResultPrompt+100 || strings.IndexByte(p, 0) >= 0 {
		t.Fatalf("prompt of %d bytes, NUL %v", len(p), strings.IndexByte(p, 0) >= 0)
	}
}

// A worktree's session names its repo as its folder.
func TestSessionFolderOfARepo(t *testing.T) {
	q, _ := open(t)
	j := makeJob(t, q, Spec{Harness: "claude", RepoRef: refX, Connector: conA, Base: "main"})
	if ref, con, repo, err := q.SessionFolder(DefaultTeam, j.Session); err != nil || ref != refX || con != conA || !repo {
		t.Fatalf("%q %q %v", ref, con, err)
	}
}
