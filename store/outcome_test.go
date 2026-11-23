package store

import (
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// LatestPreview is the workspace's newest changes.previewed event, with the seq
// it was stored at. A later preview supersedes an earlier one; a preview is
// keyed by its workspace (its session field is empty); PreviewByID reads one by
// its event id, scoped to its workspace.
func TestLatestPreview(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	if _, _, found, _ := s.LatestPreview("ws1"); found {
		t.Fatal("a workspace with no preview found one")
	}
	if _, _, err := s.Append(event.Event{ID: "evt_p1", Kind: event.ChangesPreviewed, Workspace: "ws1", Time: now,
		Previewed: &event.Previewed{Turn: "t1", Applies: 0, Count: 3}}); err != nil {
		t.Fatal(err)
	}
	e, seq, found, err := s.LatestPreview("ws1")
	if err != nil || !found || e.ID != "evt_p1" || seq < 1 || e.Previewed.Turn != "t1" {
		t.Fatalf("latest: %q seq %d found %v err %v", e.ID, seq, found, err)
	}
	// A preview keyed by a workspace does not make that id a known session.
	if known, _ := s.Known("ws1"); known {
		t.Fatal("a workspace with only a preview is a known session")
	}
	// A newer preview becomes the latest, with a higher seq.
	if _, _, err := s.Append(event.Event{ID: "evt_p2", Kind: event.ChangesPreviewed, Workspace: "ws1", Time: now,
		Previewed: &event.Previewed{Turn: "t2", Applies: 1, Count: 1}}); err != nil {
		t.Fatal(err)
	}
	e2, seq2, _, _ := s.LatestPreview("ws1")
	if e2.ID != "evt_p2" || seq2 <= seq || e2.Previewed.Applies != 1 {
		t.Fatalf("latest after the second: %q seq %d applied %d (first seq %d)", e2.ID, seq2, e2.Previewed.Applies, seq)
	}
	// PreviewByID reads an earlier preview by its id, within the workspace.
	if pe, ok, err := s.PreviewByID("ws1", "evt_p1"); err != nil || !ok || pe.Previewed.Turn != "t1" {
		t.Fatalf("PreviewByID: %+v ok %v err %v", pe, ok, err)
	}
	if _, ok, _ := s.PreviewByID("ws_other", "evt_p1"); ok {
		t.Fatal("PreviewByID found a preview under the wrong workspace")
	}
	// Another workspace's preview is not this one's.
	if _, _, found, _ := s.LatestPreview("other"); found {
		t.Fatal("another workspace found a preview")
	}
}
