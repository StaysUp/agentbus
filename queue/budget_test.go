package queue

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/staysup/agentbus/ids"
)

// costs is a CostReader over a fixed cost per turn.
func costs(by map[string]float64) CostReader {
	return func(turns []string) (float64, error) {
		var sum float64
		for _, t := range turns {
			sum += by[t]
		}
		return sum, nil
	}
}

// Every turn job records its request tree when it is made: the person's own
// turn roots its session's, asked work is in its chain's root, whatever
// session it runs in.
func TestTurnRecordsItsTree(t *testing.T) {
	q, _ := open(t)
	parent := job(t, q, "")
	if parent.Tree != parent.Session {
		t.Fatalf("the person's turn is in tree %q, want its session %s", parent.Tree, parent.Session)
	}
	other := job(t, q, "")
	// An asked turn that continues another session belongs to the asker's
	// tree; that session's own turn stays in its own.
	cont, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Session: ids.SessionID(other.Session), Prompt: "go on"},
		asked(parent.Session, parent.Turn, parent.Session, 1), testLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cont.Tree != parent.Session || cont.Session != other.Session {
		t.Fatalf("the continuation is in tree %q, session %q; want tree %s in session %s", cont.Tree, cont.Session, parent.Session, other.Session)
	}
	if o, _ := q.Get(DefaultTeam, other.ID); o.Tree != other.Session {
		t.Fatalf("the other session's own turn moved to tree %q", o.Tree)
	}
}

// Review P2, 2026-11-25: cost was summed per session, so a turn that
// continued another session it may reach counted that session's whole earlier
// cost against its tree. Cost is summed by turn per tree now: only the tree's
// own turns count, wherever they ran.
func TestCostCountsTheTreesTurnsOnly(t *testing.T) {
	q, _ := open(t)
	parent := job(t, q, "")
	other := job(t, q, "") // another tree's session, which has cost a lot
	spent := map[string]float64{other.Turn: 5}
	q.ReadCostWith(costs(spent))
	lim := testLimits
	lim.MaxCostUSD = 1
	cont, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Session: ids.SessionID(other.Session), Prompt: "go on"},
		asked(parent.Session, parent.Turn, parent.Session, 1), lim, nil)
	if err != nil {
		t.Fatalf("continuing a session another tree spent in: %v, want allowed", err)
	}
	// The continuation's own cost counts against the asker's tree, though it
	// ran in the other session.
	spent[cont.Turn] = 0.6
	spent[parent.Turn] = 0.4
	_, _, err = q.CreateTurnLinked(DefaultTeam, "", Spec{Harness: "claude", Folder: "/x", Prompt: "more"},
		asked(parent.Session, parent.Turn, parent.Session, 1), lim, nil)
	if !isLimit(err, "$1.00") || !strings.Contains(err.Error(), "$1.00 in all") {
		t.Fatalf("past the cost cap: %v, want the cost cap", err)
	}
	// The person's own turn is held to no cap.
	if _, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Session: ids.SessionID(parent.Session), Prompt: "mine"}, Links{}, lim, nil); err != nil {
		t.Fatalf("the person's own turn past the cap: %v", err)
	}
}

// Review P1, 2026-11-25: a turn watching itself scheduled a continuation that
// was held to the job caps but never to the cost cap, so a tree over
// max_cost_usd_per_tree kept going. The on keeps its maker's caps, and the
// turn it delivers is made under them, in create, as every job is.
func TestSelfWatchHeldToTheCostCap(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	lim := testLimits
	lim.MaxCostUSD = 0.25
	q.ReadCostWith(costs(map[string]float64{child.Turn: 0.30}))
	mk := Maker{Session: string(child.Session), Turn: child.Turn}
	on, err := q.RegisterOnLinked(DefaultTeam, OnTurnCompleted, child.Turn, string(child.Session), "", mk,
		&Chain{Root: parent.Session, By: child.Turn, BySession: string(child.Session)}, lim)
	if err != nil {
		t.Fatal(err)
	}
	// The child's session keeps its copy, so it takes more turns.
	if _, err := q.db.Exec(`INSERT INTO copies(id, team, runner, kind, folder, created) VALUES(?,?,?,?,?,0)`,
		"ws_"+child.Session, DefaultTeam, r.ID, "overlay", "/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) { return "again", nil })
	to := func(session string) (DeliverTo, bool, error) { return DeliverTo{Workspace: "ws_" + session}, true, nil }
	if _, err := q.FireOns(DefaultTeam, jobEnded(q), to, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, d := range deliveriesOf(t, q, string(child.Session)) {
		t.Fatalf("a tree past its cost cap got a continuation: %s", d.Turn)
	}
	var why string
	if err := q.db.QueryRow(`SELECT error FROM on_fires WHERE on_id = ?`, on.ID).Scan(&why); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(why, "may cost at most $0.25") {
		t.Fatalf("the on fired with %q, want the cost cap", why)
	}
}

// A cost cap with no way to read cost refuses, rather than read the tree as
// free.
func TestCostCapWithoutReaderRefuses(t *testing.T) {
	q, _ := open(t)
	parent := job(t, q, "")
	lim := testLimits
	lim.MaxCostUSD = 1
	if _, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Harness: "claude", Folder: "/x", Prompt: "x"},
		asked(parent.Session, parent.Turn, parent.Session, 1), lim, nil); err == nil {
		t.Fatal("a cost cap with no cost reader let the job through")
	}
}
