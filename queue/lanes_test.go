package queue

import (
	"testing"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/settings"
)

// laneOf is the lane of a take for folder jobs, or else the interactive one,
// for tests that take either.
func laneOf(folders bool) Lane {
	if folders {
		return LaneFolders
	}
	return LaneInteractive
}

// delegated makes a turn an agent asked for, in a new session of a tree the
// person's session p is the root of.
func delegated(t *testing.T, q *Queue) Job {
	t.Helper()
	j, err := askTurn(q, asked("p", NewID("t_"), "p", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// isDelegated reads the one rule for j, from the queue.
func isDelegated(t *testing.T, q *Queue, j Job) bool {
	t.Helper()
	d, err := q.JobDelegated(DefaultTeam, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func takeLane(t *testing.T, q *Queue, r Runner, l Lane) (Job, bool) {
	t.Helper()
	j, _, ok, _, err := q.Take(r.ID, lease, l)
	if err != nil {
		t.Fatal(err)
	}
	return j, ok
}

// The person's turns are not counted: with one delegated place, every turn the
// person sends is taken at once beside a delegated turn that holds it. The
// limit is read at every take, so raising it lets the next delegated turn in
// with no restart, and lowering it holds the next one. (His call, 2026-11-26:
// four of his messages waited 10+ minutes behind builders his coordinator
// started.)
func TestPersonsTurnsAreNotCounted(t *testing.T) {
	q, _ := open(t)
	limit := 1
	q.ReadDelegatedWith(func() int { return limit })
	r := runner(t, q, 64)
	for range 3 {
		delegated(t, q)
	}
	if _, ok := takeLane(t, q, r, LaneDelegated); !ok {
		t.Fatal("no first delegated turn")
	}
	if _, ok := takeLane(t, q, r, LaneDelegated); ok {
		t.Fatal("a second delegated turn was taken at a limit of 1")
	}
	for i := range 5 {
		p := job(t, q, "")
		if j, ok := takeLane(t, q, r, LaneInteractive); !ok || j.ID != p.ID {
			t.Fatalf("the person's turn %d waited: %v %s", i, ok, j.ID)
		}
	}
	// The interactive lane never hands out delegated work, nor the reverse.
	if j, ok := takeLane(t, q, r, LaneInteractive); ok {
		t.Fatalf("the interactive lane took %s", j.ID)
	}
	c, err := q.Delegated(DefaultTeam)
	if err != nil || c.AtOnce != 1 || c.Busy[r.ID] != 1 || c.Waiting != 2 {
		t.Fatalf("counts: %+v %v, want 1 at once, 1 busy, 2 waiting", c, err)
	}
	limit = 2
	if _, ok := takeLane(t, q, r, LaneDelegated); !ok {
		t.Fatal("no delegated turn after the limit was raised")
	}
	limit = 1
	if _, ok := takeLane(t, q, r, LaneDelegated); ok {
		t.Fatal("a delegated turn was taken past a lowered limit")
	}
	c, err = q.Delegated(DefaultTeam)
	if err != nil || c.AtOnce != 1 || c.Busy[r.ID] != 2 || c.Waiting != 1 {
		t.Fatalf("counts: %+v %v, want 1 at once, 2 busy, 1 waiting", c, err)
	}
}

// Which turns are delegated: one rule, the job's tree is not its own session.
// A turn an agent asked for is; the person's own turn is not, nor the
// person's message into a session an agent started.
func TestWhichTurnsAreDelegated(t *testing.T) {
	q, _ := open(t)
	if p := job(t, q, ""); isDelegated(t, q, p) {
		t.Fatalf("the person's turn is delegated: %+v", p)
	}
	a := delegated(t, q)
	if !isDelegated(t, q, a) {
		t.Fatalf("an asked turn is not delegated: tree %q session %q", a.Tree, a.Session)
	}
	m, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: ids.SessionID(a.Session), Prompt: "and this"}, Links{})
	if err != nil {
		t.Fatal(err)
	}
	if isDelegated(t, q, m) {
		t.Fatalf("the person's message into an asked session is delegated: %+v", m)
	}
	// The SQL rule and Job.Delegated agree: the queue counts what the job says.
	r := runner(t, q, 64)
	got, _, ok, _, err := q.Take(r.ID, lease, LaneDelegated)
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("delegated lane: %v %v %s, want the asked turn", ok, err, got.ID)
	}
	if got, ok := takeLane(t, q, r, LaneInteractive); !ok || isDelegated(t, q, got) {
		t.Fatalf("interactive lane: %v %+v, want the person's turn", ok, got)
	}
}

// A take for no known lane is refused.
func TestUnknownLane(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 1)
	if _, _, _, _, err := q.Take(r.ID, lease, "turns"); err == nil {
		t.Fatal("a take for lane turns was accepted")
	}
}

// The queue's default, used when no settings reader was given, is the
// settings' own default.
func TestDefaultDelegatedIsTheSettings(t *testing.T) {
	if DefaultDelegatedAtOnce != settings.DefaultMaxDelegatedAtOnce || settings.DefaultPolicy().DelegatedAtOnce() != DefaultDelegatedAtOnce {
		t.Fatalf("queue %d, settings %d", DefaultDelegatedAtOnce, settings.DefaultMaxDelegatedAtOnce)
	}
	q, _ := open(t)
	if q.DelegatedAtOnce() != DefaultDelegatedAtOnce {
		t.Fatalf("a queue with no reader: %d", q.DelegatedAtOnce())
	}
}

// wsTurn makes a turn in workspace ws: the person's (links empty) or one an
// agent asked for, in a new session of root's tree (asker true). session ""
// opens a new session.
func wsTurn(t *testing.T, q *Queue, ws, session, root string, asker bool) Job {
	t.Helper()
	sp := Spec{Harness: "claude", Workspace: ids.WorkspaceID(ws), Session: ids.SessionID(session), Prompt: "hi"}
	var j Job
	var err error
	if asker {
		j, _, err = q.CreateTurnLinked(DefaultTeam, "", sp, asked(root, NewID("t_"), root, 1), testLimits, nil)
	} else {
		j, _, err = q.Create(DefaultTeam, KindTurn, "", sp, Links{})
	}
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func idsOf(js []Job) []string {
	out := []string{}
	for _, j := range js {
		out = append(out, j.ID)
	}
	return out
}

func endJobOf(t *testing.T, q *Queue, r Runner, a Attempt) {
	t.Helper()
	if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, a.ID, a.Number, Finish{Status: Finished}); err != nil {
		t.Fatal(err)
	}
}

// The 2026-11-26 case (Grok review evt_e3e61c1e333c80c6, P1): the person's
// coordinator asked for builders in its own workspace (ask_agent opens their
// sessions there), they queued while its turn ran, and the person's next
// message waited behind all of them. A queued delegated turn holds no copy, so
// the person's message goes first; a delegated turn a runner holds still
// blocks it, as one copy runs one turn at a time. A reply delivered into the
// person's own session goes ahead the same way.
func TestPersonsMessageGoesAheadOfQueuedDelegatedWork(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 64)
	first := wsTurn(t, q, "ws_coord", "s_coord", "", false)
	_, fa, ok, _, err := q.Take(r.ID, lease, LaneInteractive)
	if err != nil || !ok {
		t.Fatalf("the person's first turn: %v %v", ok, err)
	}
	var builders []Job
	for range 3 {
		builders = append(builders, wsTurn(t, q, "ws_coord", "", first.Session, true))
	}
	endJobOf(t, q, r, fa)
	msg := wsTurn(t, q, "ws_coord", first.Session, "", false)
	if ahead, err := q.TurnsAhead(DefaultTeam, msg.ID); err != nil || len(ahead) != 0 {
		t.Fatalf("ahead of the person's message: %v %v, want none", idsOf(ahead), err)
	}
	if j, ok := takeLane(t, q, r, LaneDelegated); ok {
		t.Fatalf("the delegated lane took %s before the person's message", j.ID)
	}
	got, ma, ok, _, err := q.Take(r.ID, lease, LaneInteractive)
	if err != nil || !ok || got.ID != msg.ID {
		t.Fatalf("the person's message: %v %v %s", ok, err, got.ID)
	}
	// While it runs nothing else of the workspace starts.
	if j, ok := takeLane(t, q, r, LaneDelegated); ok {
		t.Fatalf("a builder started beside the person's turn on one copy: %s", j.ID)
	}
	endJobOf(t, q, r, ma)
	// A builder now runs; a reply into the person's session waits for it only.
	b1, ba, ok, _, err := q.Take(r.ID, lease, LaneDelegated)
	if err != nil || !ok || b1.ID != builders[0].ID {
		t.Fatalf("the first builder: %v %v %s", ok, err, b1.ID)
	}
	reply, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Workspace: "ws_coord", Session: ids.SessionID(first.Session), Prompt: "[AgentBus] reply"},
		Links{ResultOf: &ResultOf{Job: b1.ID, Kind: KindTurn}})
	if err != nil {
		t.Fatal(err)
	}
	if ahead, err := q.TurnsAhead(DefaultTeam, reply.ID); err != nil || len(ahead) != 1 || ahead[0].ID != b1.ID {
		t.Fatalf("ahead of the reply: %v %v, want only the running builder", idsOf(ahead), err)
	}
	endJobOf(t, q, r, ba)
	if j, ok := takeLane(t, q, r, LaneDelegated); ok {
		t.Fatalf("the delegated lane took %s before the reply", j.ID)
	}
	if got, ok := takeLane(t, q, r, LaneInteractive); !ok || got.ID != reply.ID {
		t.Fatalf("the reply: %v %s", ok, got.ID)
	}
}

// What the lane order keeps: a session's own turns run in the order they were
// made, so the person's message into an asked session waits for that
// session's earlier turn (and both go ahead of other queued delegated work);
// and no job passes a discard, update, close or put-away either way.
func TestLaneOrderKeepsSessionsAndBarriers(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 64)
	first := wsTurn(t, q, "ws_w", "s_w", "", false)
	_, fa, _, _, _ := q.Take(r.ID, lease, LaneInteractive)
	other := wsTurn(t, q, "ws_w", "", first.Session, true)
	asked := wsTurn(t, q, "ws_w", "", first.Session, true)
	msg := wsTurn(t, q, "ws_w", asked.Session, "", false)
	endJobOf(t, q, r, fa)
	if ahead, _ := q.TurnsAhead(DefaultTeam, msg.ID); len(ahead) != 1 || ahead[0].ID != asked.ID {
		t.Fatalf("ahead of a message into an asked session: %v, want its own earlier turn only", idsOf(ahead))
	}
	if ahead, _ := q.TurnsAhead(DefaultTeam, other.ID); len(ahead) != 2 {
		t.Fatalf("ahead of the other builder: %v, want the asked session's two turns", idsOf(ahead))
	}
	if got, ok := takeLane(t, q, r, LaneDelegated); !ok || got.ID != asked.ID {
		t.Fatalf("first taken: %v %s, want the asked session's turn", ok, got.ID)
	}

	// A close made after a queued builder runs after it, and the person's
	// turn made after the close runs after the close.
	q2, _ := open(t)
	r2 := runner(t, q2, 64)
	p := wsTurn(t, q2, "ws_c", "s_c", "", false)
	_, pa, _, _, _ := q2.Take(r2.ID, lease, LaneInteractive)
	b := wsTurn(t, q2, "ws_c", "", p.Session, true)
	cl, _, err := q2.create(DefaultTeam, KindClose, "", Spec{Workspace: "ws_c"}, Links{}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := wsTurn(t, q2, "ws_c", p.Session, "", false)
	endJobOf(t, q2, r2, pa)
	if ahead, _ := q2.TurnsAhead(DefaultTeam, m.ID); len(ahead) != 2 || ahead[0].ID != b.ID || ahead[1].ID != cl.ID {
		t.Fatalf("ahead of the turn after a close: %v, want the builder and the close", idsOf(ahead))
	}
	if j, ok := takeLane(t, q2, r2, LaneInteractive); ok {
		t.Fatalf("the interactive lane passed the close: %s", j.ID)
	}
}

// Waiting is counted per runner for the turns bound to it: by its machine
// here. A turn that names no machine counts in the total only. (Grok review
// evt_e3e61c1e333c80c6, P3: waiting was reported for all runners only.)
func TestWaitingPerRunner(t *testing.T) {
	q, _ := open(t)
	q.ReadDelegatedWith(func() int { return 1 })
	a, err := q.RegisterRunner(DefaultTeam, "a", nil, 64, HashToken(NewID("tok_")), &Capabilities{Connector: conA})
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.RegisterRunner(DefaultTeam, "b", nil, 64, HashToken(NewID("tok_")), &Capabilities{Connector: conB})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(machine string) {
		t.Helper()
		if _, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Harness: "claude", FolderRef: refX, Machine: machine, Prompt: "hi"},
			asked("p", NewID("t_"), "p", 1), testLimits, nil); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		mk(conA)
	}
	mk(conB)
	if _, ok := takeLane(t, q, a, LaneDelegated); !ok {
		t.Fatal("runner a took nothing")
	}
	// Made after the take, so runner a's one place goes to a turn bound to it
	// whatever the order of turns made in the same instant (Codex review
	// t_eab7dbd40f6e09d4: it took the unbound one 3 times in 20).
	mk("")
	c, err := q.Delegated(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	if c.Busy[a.ID] != 1 || c.WaitingFor[a.ID] != 2 || c.WaitingFor[b.ID] != 1 || c.Waiting != 4 {
		t.Fatalf("counts: %+v, want a 1 busy 2 waiting, b 1 waiting, 4 in all", c)
	}
}

// Codex review evt_ffeb7b5ccdb2ee05: a turn an agent asks for in its own root
// session (tree == session) is still delegated. With the one place taken, it
// waits, and the counts say so.
func TestAgentTurnInItsRootSessionIsDelegated(t *testing.T) {
	q, _ := open(t)
	q.ReadDelegatedWith(func() int { return 1 })
	r := runner(t, q, 64)
	delegated(t, q)
	if _, ok := takeLane(t, q, r, LaneDelegated); !ok {
		t.Fatal("the first delegated turn was not taken")
	}
	own, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Harness: "claude", Session: "p", Prompt: "again"},
		asked("p", NewID("t_"), "p", 1), testLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if own.Tree != own.Session || !isDelegated(t, q, own) {
		t.Fatalf("tree %q session %q delegated %v, want a delegated turn in its root session", own.Tree, own.Session, isDelegated(t, q, own))
	}
	if j, ok := takeLane(t, q, r, LaneInteractive); ok {
		t.Fatalf("the interactive lane took the agent's turn %s", j.ID)
	}
	if j, ok := takeLane(t, q, r, LaneDelegated); ok {
		t.Fatalf("the agent's turn %s passed the limit of 1", j.ID)
	}
	c, err := q.Delegated(DefaultTeam)
	if err != nil || c.Busy[r.ID] != 1 || c.Waiting != 1 {
		t.Fatalf("counts: %+v %v, want 1 busy and the agent's turn waiting", c, err)
	}
}

// A reply from one of the person's sessions into another keeps the asker's
// tree, yet lands in a session the person started: it is the person's work,
// never waiting for a delegated place. The receiving session decides, not the
// reply's tree. (Codex review t_eab7dbd40f6e09d4, P2.)
func TestReplyBetweenPersonsSessionsIsNotDelegated(t *testing.T) {
	q, _ := open(t)
	q.ReadDelegatedWith(func() int { return 1 })
	r := runner(t, q, 64)
	for _, s := range []ids.SessionID{"s_a", "s_b"} {
		if _, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Session: s, Prompt: "mine"}, Links{}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, ok := takeLane(t, q, r, LaneInteractive); !ok {
			t.Fatal("the person's turn was not taken")
		}
	}
	delegated(t, q)
	if _, ok := takeLane(t, q, r, LaneDelegated); !ok {
		t.Fatal("the delegated turn was not taken")
	}
	reply, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Session: "s_b", Prompt: "the result"},
		Links{ResultOf: &ResultOf{Job: NewID("job_"), Kind: KindTurn}, Chain: &Chain{Root: "s_a"}})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Tree != "s_a" || isDelegated(t, q, reply) {
		t.Fatalf("tree %q delegated %v, want the asker's tree and not delegated", reply.Tree, isDelegated(t, q, reply))
	}
	// Into a session an agent started, a reply still waits for a place.
	asked := delegated(t, q)
	into, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Session: ids.SessionID(asked.Session), Prompt: "the result"},
		Links{ResultOf: &ResultOf{Job: NewID("job_"), Kind: KindTurn}, Chain: &Chain{Root: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if !isDelegated(t, q, into) {
		t.Fatal("a reply into a session an agent started is not delegated")
	}
}
