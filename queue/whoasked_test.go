package queue

import (
	"database/sql"
	"strings"
	"testing"
)

// Who asked is stored once: a job keeps the asking turn (chain.by), never its
// session, which is read back from the asking turn's own job; an on keeps its
// maker and that it counts, never a copy of the maker's tree (O2.1).
func TestWhoAskedStoredOnce(t *testing.T) {
	q, _ := open(t)
	asker, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Session: "p", Harness: "claude", Prompt: "go"}, Links{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked("p", asker.Turn, "p", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	var links string
	if err := q.db.QueryRow(`SELECT links FROM jobs WHERE id = ?`, child.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(links, "by_session") || !strings.Contains(links, asker.Turn) {
		t.Errorf("stored links %s: want the asking turn only", links)
	}
	got, err := q.Get(DefaultTeam, child.ID)
	if err != nil || got.Links.Chain == nil || got.Links.Chain.BySession != "p" {
		t.Fatalf("the asking session, read back: %+v %v", got.Links.Chain, err)
	}

	// The per-tree cap counts an on through its maker's own job: the asker's
	// (the person's turn, its tree is its session) and the child's (its chain's
	// root) both count against tree p.
	lim := Limits{MaxDepth: 2, MaxPerTurn: 10, MaxPerTree: 3}
	if _, err := q.RegisterOnLinked(DefaultTeam, OnTurnCompleted, child.Turn, "p", "", Maker{Session: "p", Turn: asker.Turn},
		&Chain{Root: "p", Depth: 1, By: asker.Turn}, lim); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RegisterOnLinked(DefaultTeam, OnTurnCompleted, asker.Turn, string(child.Session), "", Maker{Session: child.Session, Turn: child.Turn},
		&Chain{Root: "p", Depth: 2, By: child.Turn}, lim); err != nil {
		t.Fatal(err)
	}
	var chain sql.NullString
	var counts sql.NullInt64
	if err := q.db.QueryRow(`SELECT chain, asked FROM ons WHERE json_extract(maker, '$.turn') = ?`, asker.Turn).Scan(&chain, &counts); err != nil {
		t.Fatal(err)
	}
	if chain.Valid || counts.Int64 != 1 {
		t.Errorf("an on kept a copy of its maker's tree: chain %v asked %v", chain, counts)
	}
	// One job and two ons in tree p: the cap of 3 is reached.
	_, err = q.RegisterOnLinked(DefaultTeam, OnTurnCompleted, child.Turn, "p", "", Maker{Session: "p", Turn: asker.Turn},
		&Chain{Root: "p", Depth: 1, By: asker.Turn}, lim)
	if !isLimit(err, "in all") {
		t.Fatalf("an on past the tree cap: %v", err)
	}
}
