package runner

import (
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// A note control sends is taken once, however many heartbeats send it
// again; a call that was not accepted gives it back, and the next call takes
// it again, with the same text.
func TestNoteTakenOnceAndGivenBack(t *testing.T) {
	at := &attempt{}
	n := queue.Note{ID: 3, Text: "your folder changed"}
	at.heardNotes([]queue.Note{n})
	at.heardNotes([]queue.Note{n})
	got := at.takeNotes()
	if len(got) != 1 || got[0].Note != 3 || got[0].Text != event.NotePrefix+n.Text || got[0].Kind != event.AddedNote {
		t.Fatalf("took %+v", got)
	}
	at.heardNotes([]queue.Note{n})
	if again := at.takeNotes(); len(again) != 0 {
		t.Fatalf("a heartbeat's repeat was taken again: %+v", again)
	}
	at.returnNotes(got)
	back := at.takeNotes()
	if len(back) != 1 || back[0] != got[0] {
		t.Fatalf("after a refused call took %+v, want %+v", back, got[0])
	}
	// A note given back twice, or one never taken, is not doubled.
	at.returnNotes(back)
	at.returnNotes(back)
	if twice := at.takeNotes(); len(twice) != 1 {
		t.Fatalf("took %d, want 1", len(twice))
	}
}
