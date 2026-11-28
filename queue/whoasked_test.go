package queue

import (
	"database/sql"
	"strings"
	"testing"
)

// Who asked is stored once: a job keeps the asking turn (chain.by), never its
// session, which is read back from the asking turn's own job; an on keeps its
// maker, never a copy of the maker's tree (O2.1).
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

	// An on keeps its maker, never a copy of the maker's tree, and does not
	// count as asked work (0.6.8).
	if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: string(child.Session)}, "p", "", Maker{Session: "p", Turn: asker.Turn}, &testLimits, 0); err != nil {
		t.Fatal(err)
	}
	var chain sql.NullString
	var counts sql.NullInt64
	if err := q.db.QueryRow(`SELECT chain, asked FROM ons WHERE json_extract(maker, '$.turn') = ?`, asker.Turn).Scan(&chain, &counts); err != nil {
		t.Fatal(err)
	}
	if chain.Valid || counts.Valid {
		t.Errorf("an on kept a copy of its maker's tree, or counts as asked work: chain %v asked %v", chain, counts)
	}
}
