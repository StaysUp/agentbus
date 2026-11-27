package applied

import (
	"testing"
	"time"
)

// Settled is the put-away's rule: nothing appliable past the last successful
// apply, and no update or markers waiting on the next one.
func TestSettled(t *testing.T) {
	var f Fold
	if !f.State().Settled() {
		t.Fatal("a workspace with no changes is settled")
	}
	f.Changed("t1", "cs1", "cp1")
	if f.State().Settled() {
		t.Fatal("an unapplied turn is not settled")
	}
	f.Applied("t1", "cs1", false)
	if f.State().Settled() {
		t.Fatal("a refused apply leaves the turn unapplied")
	}
	f.Applied("t1", "cs1", true)
	if !f.State().Settled() {
		t.Fatal("every turn applied is settled")
	}
	f.Changed("t2", "cs2", "cp2")
	f.Updated(UpdateInForce{From: "theirs", To: "merged", HeadTurn: "t2"}, []string{"t2"}, false)
	if f.State().Settled() {
		t.Fatal("an update in force waits on the next apply")
	}
	f.Discarded(time.Unix(1, 0), nil)
	if !f.State().Settled() {
		t.Fatal("a discard settles the workspace")
	}
}
