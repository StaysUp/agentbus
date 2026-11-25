package store

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// The record has one owner, made once, and every event it keeps names it.
// An event sent on from another store gets this record's owner, never the
// sender's (O2.2).
func TestOwnerMadeOnceAndOnEveryEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := st.Owner()
	if !regexp.MustCompile(`^own_[0-9a-f]{16}$`).MatchString(owner) {
		t.Fatalf("owner id %q", owner)
	}
	now := time.Now().UTC()
	if _, _, err := st.Append(event.Event{ID: "evt_1", Kind: event.SessionCreated, Time: now, Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendOnce(event.Event{ID: "evt_2", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Owner: "own_somebodyelse00"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Owner() != owner {
		t.Fatalf("owner made again: %q, was %q", st.Owner(), owner)
	}
	recs, err := st.After(0, Filter{}, 10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("%v %d", err, len(recs))
	}
	for _, r := range recs {
		var e event.Event
		if err := json.Unmarshal(r.Data, &e); err != nil || e.Owner != owner {
			t.Errorf("%s: owner %q, want %q", r.Kind, e.Owner, owner)
		}
	}
}

// A turn holds the folder grants made before it started (asOf); one made
// later applies from the next turn.
func TestFolderGrantAsOf(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	before := time.Now()
	if err := st.AddFolderGrant("s1", "/data/notes"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if ok, err := st.HasFolderGrant("s1", "/data/notes/a.txt", before.Add(-time.Second)); err != nil || ok {
		t.Errorf("a grant made after the turn started: %v %v", ok, err)
	}
	if ok, err := st.HasFolderGrant("s1", "/data/notes/a.txt", after); err != nil || !ok {
		t.Errorf("a grant made before the turn started: %v %v", ok, err)
	}
}
