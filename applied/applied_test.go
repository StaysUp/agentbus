package applied

import (
	"reflect"
	"testing"
	"time"
)

func turnsOf(s State) []string {
	out := []string{}
	for _, t := range s.Turns {
		out = append(out, t.Turn)
	}
	return out
}

// The appliable turns are the ones with a changeset, minus what a discard
// dropped and an update folded; the mark is the furthest successful apply.
func TestFoldAppliableAndMark(t *testing.T) {
	var f Fold
	if s := f.State(); s.Mark != -1 || s.Head() != "" || s.Applied {
		t.Fatalf("empty: %+v", s)
	}
	f.Changed("t1", "cs1", "cp1")
	f.Changed("t2", "", "cp2") // its changeset could not be kept
	f.Changed("t3", "cs3", "cp3")
	s := f.State()
	if got := turnsOf(s); !reflect.DeepEqual(got, []string{"t1", "t3"}) || s.Head() != "t3" {
		t.Fatalf("turns %v head %q", got, s.Head())
	}
	if s.Changed("t2") || !s.Changed("t1") {
		t.Fatalf("changed: t1 %v t2 %v", s.Changed("t1"), s.Changed("t2"))
	}

	f.Applied("t3", "cs3", false) // refused: moves nothing
	if s := f.State(); s.Mark != -1 || s.Applied {
		t.Fatalf("after a refusal: %+v", s)
	}
	f.Applied("t1", "cs1", true)
	if s := f.State(); s.Mark != 0 || s.AppliedTurn != "t1" || s.AppliedTo != "cs1" {
		t.Fatalf("after apply t1: mark %d applied %q to %q", s.Mark, s.AppliedTurn, s.AppliedTo)
	}

	when := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	f.Discarded(when, []string{"t3"})
	s = f.State()
	if got := turnsOf(s); !reflect.DeepEqual(got, []string{"t1"}) || s.Mark != 0 || s.DiscardedAt == nil {
		t.Fatalf("after discard: turns %v mark %d discarded %v", got, s.Mark, s.DiscardedAt)
	}
	// A dropped turn still names its checkpoint: a reset point may be it.
	if !s.Changed("t3") || s.CheckpointOf("cs3") != "cp3" {
		t.Fatalf("dropped t3: changed %v checkpoint %q", s.Changed("t3"), s.CheckpointOf("cs3"))
	}
	// Any apply, refused too, ends the discard's standing.
	f.Applied("t1", "", false)
	if s := f.State(); s.DiscardedAt != nil {
		t.Fatalf("discarded_at after a later apply: %v", s.DiscardedAt)
	}
}

// A clean update folds the unapplied turns but its head, sets the folder's
// baseline until a successful apply or a discard, and a merge's markers last
// until then too, through a later plain update.
func TestFoldUpdate(t *testing.T) {
	var f Fold
	f.Changed("t1", "cs1", "cp1")
	f.Changed("t2", "cs2", "cp2")
	f.Updated(UpdateInForce{From: "theirs", To: "merged", HeadTurn: "t2", TheirsCheckpoint: "tcp", MergedCheckpoint: "mcp"}, []string{"t1"}, true)
	s := f.State()
	if got := turnsOf(s); !reflect.DeepEqual(got, []string{"t2"}) || s.Update == nil || s.Update.HeadTurn != "t2" || !s.Markers {
		t.Fatalf("after update: turns %v update %+v markers %v", got, s.Update, s.Markers)
	}
	if s.CheckpointOf("merged") != "mcp" || s.CheckpointOf("theirs") != "tcp" || s.CheckpointOf("cs2") != "cp2" || s.CheckpointOf("") != "" {
		t.Fatal("checkpoints")
	}
	f.Updated(UpdateInForce{From: "theirs2", To: "merged2", HeadTurn: "t2", TheirsCheckpoint: "tcp2", MergedCheckpoint: "mcp2"}, nil, false)
	if s := f.State(); !s.Markers || s.Update.From != "theirs2" {
		t.Fatalf("a later plain update: markers %v update %+v", s.Markers, s.Update)
	}
	f.Applied("t2", "merged2", true)
	if s := f.State(); s.Update != nil || s.Markers || s.Mark != 0 {
		t.Fatalf("after apply: update %+v markers %v mark %d", s.Update, s.Markers, s.Mark)
	}
}

// A snapshot stands for every outcome before it: a fold restored from it
// answers as the fold it was taken from, and later outcomes add to it.
func TestFoldSnapshotRoundTrip(t *testing.T) {
	var f Fold
	f.Changed("t1", "cs1", "cp1")
	f.Changed("t2", "cs2", "cp2")
	f.Changed("t3", "cs3", "cp3")
	f.Applied("t1", "cs1", true)
	f.Discarded(time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), []string{"t3"})
	f.Updated(UpdateInForce{From: "theirs", To: "merged", HeadTurn: "t2", TheirsCheckpoint: "tcp", MergedCheckpoint: "mcp"}, nil, true)
	want := f.State()

	var g Fold
	g.Changed("t0", "cs0", "cp0") // replaced by the snapshot
	g.Restore(want.Snapshot())
	got := g.State()
	if !reflect.DeepEqual(got.Snapshot(), want.Snapshot()) || got.Index("t0") != -1 {
		t.Fatalf("restored %+v, want %+v", got.Snapshot(), want.Snapshot())
	}
	if !got.Changed("t3") || got.CheckpointOf("merged") != "mcp" || got.CheckpointOf("cs3") != "cp3" || got.Mark != 0 {
		t.Fatalf("restored lookups: changed t3 %v merged %q cs3 %q mark %d", got.Changed("t3"), got.CheckpointOf("merged"), got.CheckpointOf("cs3"), got.Mark)
	}
	g.Changed("t4", "cs4", "cp4")
	g.Applied("t4", "merged2", true)
	if s := g.State(); s.Head() != "t4" || s.Mark != 2 || s.Update != nil || s.Markers {
		t.Fatalf("after the snapshot: head %q mark %d update %+v markers %v", s.Head(), s.Mark, s.Update, s.Markers)
	}
	// A turn given again is the same turn.
	g.Changed("t4", "cs4", "cp4")
	if s := g.State(); len(s.Turns) != 3 {
		t.Fatalf("turns %+v", s.Turns)
	}
}
