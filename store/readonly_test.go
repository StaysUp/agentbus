package store

import (
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/event"
)

// A tool reads a record while its service holds it: no lock is taken, what
// the service wrote is read, nothing is written, and a missing file is an
// error, not a new store.
func TestOpenReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := open(t, path)
	mustAppend(t, s, ev(event.TurnStarted, "s"))
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	recs, err := ro.After(0, Filter{Session: "s", Only: true}, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read %d events: %v", len(recs), err)
	}
	if _, _, err := ro.Append(ev(event.TurnStarted, "s")); err == nil {
		t.Fatal("a read-only store wrote an event")
	}
	mustAppend(t, s, ev(event.TurnStarted, "s")) // the service still writes
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "none.db")); err == nil {
		t.Fatal("a missing file opened")
	}
}
