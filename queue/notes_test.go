package queue

import (
	"errors"
	"strings"
	"testing"
)

// A note attached to a turn waits for its session's turn, which its
// attempt's answers carry until the record says a turn was given it; then
// no later attempt gets it again.
func TestNoteWaitsUntilDelivered(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 1)
	j := job(t, q, "k1")
	_, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	n, err := q.AddNote(DefaultTeam, j.Turn, "  your folder changed  ")
	if err != nil {
		t.Fatal(err)
	}
	if n.Text != "your folder changed" || n.ID == 0 {
		t.Fatalf("note %+v", n)
	}
	got, err := q.AttemptNotes(r.ID, a.ID, a.Number)
	if err != nil || len(got) != 1 || got[0] != n {
		t.Fatalf("attempt notes %+v %v, want %+v", got, err, n)
	}
	// Another session's turn marks nothing.
	if err := q.NotesDelivered(DefaultTeam, "s_other", j.Turn, []int64{n.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.PendingNotes(DefaultTeam, j.Session); len(got) != 1 {
		t.Fatalf("pending %+v after another session's turn", got)
	}
	if err := q.NotesDelivered(DefaultTeam, j.Session, j.Turn, []int64{n.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.AttemptNotes(r.ID, a.ID, a.Number); len(got) != 0 {
		t.Fatalf("a delivered note is still sent: %+v", got)
	}
}

// A note needs short UTF-8 text, a turn job that exists, and room among its
// session's waiting notes.
func TestAddNoteRefuses(t *testing.T) {
	q, _ := open(t)
	j := job(t, q, "k1")
	for _, text := range []string{"", "   ", strings.Repeat("x", MaxNote+1), "a\x00b", "\xff"} {
		if _, err := q.AddNote(DefaultTeam, j.Turn, text); !errors.Is(err, ErrNoteInvalid) {
			t.Errorf("text %q: %v, want ErrNoteInvalid", text, err)
		}
	}
	if _, err := q.AddNote(DefaultTeam, "t_none", "hi"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown turn: %v", err)
	}
	if _, err := q.AddNote("team_other", j.Turn, "hi"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another team's turn: %v", err)
	}
	for i := 0; i < MaxNotes; i++ {
		if _, err := q.AddNote(DefaultTeam, j.Turn, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.AddNote(DefaultTeam, j.Turn, "one more"); !errors.Is(err, ErrNotesFull) {
		t.Errorf("past MaxNotes: %v", err)
	}
}
