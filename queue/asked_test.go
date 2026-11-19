package queue

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func asked(session, turn, root string, depth int) Links {
	return Links{Chain: &Chain{Root: root, Depth: depth, By: turn, BySession: session}}
}

// onFor registers the delivery on the ask path registers for child, so a
// queue test drives FireOns the way the API drives it.
func onFor(t *testing.T, q *Queue, child Job, l Links) {
	t.Helper()
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, l.Chain.BySession, "", Maker{Session: l.Chain.BySession, Turn: l.Chain.By}); err != nil {
		t.Fatal(err)
	}
}

// The default caps, as permissions.toml writes them.
const MaxDepth, MaxAskedPerTurn, MaxAskedPerRoot = 2, 10, 30

var testLimits = Limits{MaxDepth: MaxDepth, MaxPerTurn: MaxAskedPerTurn, MaxPerTree: MaxAskedPerRoot}

func askTurn(q *Queue, l Links, key string) (Job, error) {
	j, _, err := q.CreateTurnLinked(DefaultTeam, key, Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"}, l, testLimits, nil)
	return j, err
}

// The caps on asked work refuse past each limit, with a reason, and count
// asked work only: the person's own jobs and delivered results never count.
func TestAskedCaps(t *testing.T) {
	q, _ := open(t)
	if _, err := askTurn(q, asked("p", "t_1", "p", MaxDepth+1), ""); !isLimit(err, "levels") {
		t.Fatalf("too deep: %v", err)
	}
	if _, err := askTurn(q, Links{Chain: &Chain{By: "t_1", BySession: "p"}}, ""); !errors.Is(err, ErrBadState) {
		t.Fatalf("asked without a depth: %v", err)
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
	// A limit names its line in the cockpit's Settings, never the
	// permissions.toml settings.db replaced.
	return errors.As(err, &l) && strings.Contains(l.Msg, word) && strings.Contains(l.Msg, "in Settings, Limits)") &&
		!strings.Contains(l.Msg, "permissions.toml")
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
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	onFor(t, q, child, links)
	// The delivered reply is read from the record by the child's turn, not
	// from a copy in its result.
	q.ReadReplyWith(func(session, turn string) (string, error) {
		if session == child.Session && turn == child.Turn {
			return "the whole answer", nil
		}
		return "", nil
	})
	cj, ca := take(t, q, r)
	if cj.ID != child.ID {
		t.Fatalf("took %s, want the child", cj.ID)
	}
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil); err != nil || n != 0 {
		t.Fatalf("before the child ends: %d %v", n, err)
	}
	res := `{"session":"` + child.Session + `","state":"finished","folder":"/w/1",` +
		`"changes":{"count":2,"files":[{"path":"a.go","change":"modified"},{"path":"b.go","change":"added"}]}}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil); err != nil {
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
	// The delivery runs in the person's root session (parent), which has no
	// request tree of its own, so the delivery is a root-level turn with no
	// Chain — his rule: a delivered turn joins the RECEIVING session's tree, and
	// a root session has none.
	if d.State != Queued || d.Spec.Session != parent.Session || d.Spec.Harness != "claude" || d.Links.ResultOf.Job != child.ID ||
		d.Links.ResultOf.Session != child.Session || d.Links.ResultOf.Turn != child.Turn || d.Links.ResultOf.Agent != "claude" ||
		d.Links.Chain != nil {
		t.Fatalf("delivery: %+v %+v", d, d.Links)
	}
	for _, want := range []string{"[AgentBus]", "not from the person", "Agent: claude", "Folder: /w/1", "Changed files: 2",
		"- a.go (modified)", "- b.go (added)", "the whole answer", "Session: " + child.Session} {
		if !strings.Contains(d.Spec.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, d.Spec.Prompt)
		}
	}
	// It waits for the parent's turn, then runs.
	if _, _, ok, _, err := q.Take(r.ID, lease, false); err != nil || ok {
		t.Fatalf("took the delivery while the parent runs: %v %v", ok, err)
	}
	if _, err := q.End(r.ID, pa.ID, pa.Number, Finish{Status: Finished, Session: parent.Session,
		Workspace: &Workspace{Kind: WorkspaceFolder, FolderRef: "fld_0123456789abcdef"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := take(t, q, r); got.ID != d.ID {
		t.Fatalf("took %s, want the delivery", got.ID)
	}
	// The person's own job ended too, and is never delivered.
	if n, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil); err != nil || n != 0 {
		t.Fatalf("after: %d %v", n, err)
	}
	_ = pj
}

// A delivered result names the agent; a reply past the cap is cut; a refused
// start says why. (B15: no review verdict in the prompt — a review is a turn.)
func TestResultPrompt(t *testing.T) {
	p := ResultPrompt(Job{ID: "job_1", Kind: KindTurn, State: Finished, Spec: Spec{AgentID: "codex", Model: "m-1"},
		Result: json.RawMessage(`{"session":"rs"}`)}, nil, "", func(id string) string { return map[string]string{"codex": "Codex"}[id] })
	for _, want := range []string{"the turn you asked for", "Agent: Codex (id codex, model m-1)", "Session: rs"} {
		if !strings.Contains(p, want) {
			t.Errorf("result prompt lacks %q:\n%s", want, p)
		}
	}
	// The reply is read from the record and passed in; a reply past the cap is cut.
	long := strings.Repeat("x", MaxResultReply+100)
	if p := ResultPrompt(Job{Kind: KindTurn, State: Finished, Result: json.RawMessage(`{"session":"s"}`)}, nil, long, nil); strings.Contains(p, long) || !strings.Contains(p, "cut here") {
		t.Error("a long reply was not cut")
	}
	p = ResultPrompt(Job{Kind: KindTurn, State: Failed, Reason: ReasonSetup, Result: json.RawMessage(`{"refused":"no such agent"}`)}, nil, "", nil)
	if !strings.Contains(p, "failed (setup)") || !strings.Contains(p, "Error: no such agent") {
		t.Errorf("refused prompt:\n%s", p)
	}
}

// A delivered result's turn names the asking turn's agent, as control
// reads it at delivery (agent): never the asking session's latest agent.
// One whose agent cannot run (removed) is not delivered, and the deliveries
// table says why, once.
func TestDeliverNamesTheAskingTurnsAgent(t *testing.T) {
	for _, removed := range []bool{false, true} {
		q, _ := open(t)
		parent := job(t, q, "")
		links := asked(parent.Session, parent.Turn, parent.Session, 1)
		child, err := askTurn(q, links, "")
		if err != nil {
			t.Fatal(err)
		}
		onFor(t, q, child, links)
		if _, err := q.Cancel(DefaultTeam, child.ID); err != nil {
			t.Fatal(err)
		}
		var saw Spec
		agent := func(asking Spec) (Spec, string) {
			saw = asking
			if removed {
				return Spec{}, "the asking turn's agent was removed"
			}
			return Spec{Harness: "codex", AgentID: "a_1", Access: "subscription", AgentModel: "m"}, ""
		}
		for range 2 {
			if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, agent); err != nil {
				t.Fatal(err)
			}
		}
		if saw.Harness != "claude" {
			t.Fatalf("asked for the agent of %+v, want the asking turn's", saw)
		}
		ds, err := q.List(DefaultTeam, Filter{Session: parent.Session}, 10)
		if err != nil {
			t.Fatal(err)
		}
		var d *Job
		for i := range ds {
			if ds[i].Links.ResultOf != nil {
				d = &ds[i]
			}
		}
		if removed {
			if d != nil {
				t.Fatalf("delivered with its agent removed: %+v", d)
			}
			var why string
			if err := q.db.QueryRow(`SELECT error FROM on_fires WHERE key = ?`, child.Turn).Scan(&why); err != nil || why == "" {
				t.Fatalf("delivery record: %q %v", why, err)
			}
			continue
		}
		if d == nil || d.Spec.Harness != "codex" || d.Spec.AgentID != "a_1" || d.Spec.AgentModel != "m" {
			t.Fatalf("delivery: %+v", d)
		}
	}
}

// A caller's key can never take a delivery's: a job made first with a
// look-alike key does not stop the result from being delivered.
func TestDeliverKeyIsOurs(t *testing.T) {
	q, _ := open(t)
	parent := job(t, q, "")
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	onFor(t, q, child, links)
	if strings.Trim(ResultKey("on_1", child.ID, "s_x"), "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:-") == "" {
		t.Fatalf("%s is a key a caller may send", ResultKey("on_1", child.ID, "s_x"))
	}
	for _, k := range []string{"result_" + child.ID, "result:" + child.ID} {
		if _, _, err := q.Create(DefaultTeam, KindTurn, k, Spec{Session: parent.Session, Harness: "claude", Prompt: "squat"}, Links{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Cancel(DefaultTeam, child.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil); err != nil || n != 1 {
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

// B15: TestSessionRootOfReview and TestReviewIsALeaf retired. A review is a
// normal turn (the review_turn app), so there is no review kind, no review
// session named by control, and no IsReview leaf — the asked-work tree and caps
// cover a review exactly as they cover any asked turn.

// However large a delivered result, the prompt stays bounded and holds no NUL
// byte, so its turn can start.
func TestResultPromptBounded(t *testing.T) {
	p := ResultPrompt(Job{Kind: KindTurn, State: Finished,
		Result: json.RawMessage(`{"error":"` + strings.Repeat("e", 100000) + `"}`)}, nil, strings.Repeat("y\x00", 50000), nil)
	if len(p) > MaxResultPrompt+100 || strings.IndexByte(p, 0) >= 0 {
		t.Fatalf("prompt of %d bytes, NUL %v", len(p), strings.IndexByte(p, 0) >= 0)
	}
}

// A worktree's session names its repo as its folder.
func TestSessionFolderOfARepo(t *testing.T) {
	q, _ := open(t)
	j := makeJob(t, q, Spec{Harness: "claude", RepoRef: refX, Machine: conA, Base: "main"})
	if ref, con, repo, err := q.SessionFolder(DefaultTeam, j.Session); err != nil || ref != refX || con != conA || !repo {
		t.Fatalf("%q %q %v", ref, con, err)
	}
}

// A result for a session that ended with its first turn (its lease ran
// out) cannot be delivered: it is recorded as undelivered, and the sweep
// goes on to the next result instead of stopping on it.
func TestDeliverToALostSession(t *testing.T) {
	q, c := open(t)
	r := runner(t, q, 5)
	parent, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Folder: "/x", Prompt: "hi", Isolate: true}, Links{})
	if err != nil {
		t.Fatal(err)
	}
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	onFor(t, q, child, links)
	cj, ca := take(t, q, r)
	if cj.ID != child.ID {
		t.Fatalf("took %s, want the child", cj.ID)
	}
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(`{"state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	c.add(lease + time.Second)
	if _, err := q.Sweep(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil); err != nil || n != 0 {
			t.Fatalf("deliver: %d %v", n, err)
		}
	}
}

// A limit counts turns and reviews in words, singular for one; a job is the
// queue's word, not the API's (API test 2026-11-11).
func TestTurnsOrReviews(t *testing.T) {
	if got := turnsOrReviews(1); got != "1 turn or review" {
		t.Errorf("one: %q", got)
	}
	if got := turnsOrReviews(10); got != "10 turns or reviews" {
		t.Errorf("ten: %q", got)
	}
}
