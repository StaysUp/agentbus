package runner

// A note for a running turn (queue/notes.go). Control sends the notes of the
// turn's session the record does not hold as given, on the attempt's start
// and every heartbeat; the attempt keeps those it has not given yet, its own
// work in progress. Each is given at the turn's next main-thread model call,
// its first one too: the gateway's own replay takes it (TakeNotes) and adds
// it to the call. A call the vendor does not accept gives it back
// (ReturnNotes), for the next call. A harness own replay does not serve (agy
// today) takes none, and its notes stay on control. Each note goes to the
// turn once: the heartbeats that come before control's record holds it as
// given send it again, and it is not taken twice. A note the turn never took
// stays on control, for the session's next turn.

import (
	"strings"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// noteText is a note as the agent gets it: marked as AgentBus's, so the
// agent never takes it for the person's words.
func noteText(text string) string {
	return event.NotePrefix + text
}

// heardNotes keeps the notes control sent that the attempt has neither given
// nor kept already.
func (at *attempt) heardNotes(notes []queue.Note) {
	if len(notes) == 0 {
		return
	}
	at.mu.Lock()
	defer at.mu.Unlock()
	for _, n := range notes {
		if at.given[n.ID] || at.kept(n.ID) {
			continue
		}
		at.notes = append(at.notes, n)
	}
}

// kept says note id waits in the attempt. Called with at.mu held.
func (at *attempt) kept(id int64) bool {
	for _, n := range at.notes {
		if n.ID == id {
			return true
		}
	}
	return false
}

// takeNotes gives the turn the notes it keeps: each as a context.added piece,
// marked given, so it is never taken again.
func (at *attempt) takeNotes() []event.Added {
	at.mu.Lock()
	defer at.mu.Unlock()
	if len(at.notes) == 0 {
		return nil
	}
	if at.given == nil {
		at.given = map[int64]bool{}
	}
	out := make([]event.Added, 0, len(at.notes))
	for _, n := range at.notes {
		at.given[n.ID] = true
		text := noteText(n.Text)
		out = append(out, event.Added{Kind: event.AddedNote, Bytes: len(text), Text: text, Note: n.ID})
	}
	at.notes = nil
	return out
}

// returnNotes puts notes the turn was handed back, first, so the next call
// takes them again.
func (at *attempt) returnNotes(notes []event.Added) {
	at.mu.Lock()
	defer at.mu.Unlock()
	back := make([]queue.Note, 0, len(notes)+len(at.notes))
	for _, n := range notes {
		if !at.given[n.Note] {
			continue
		}
		delete(at.given, n.Note)
		back = append(back, queue.Note{ID: n.Note, Text: strings.TrimPrefix(n.Text, event.NotePrefix)})
	}
	at.notes = append(back, at.notes...)
}

// TakeNotes gives the running turn of session with this id the notes it
// keeps, for its next main-thread model call (replay.Notes). A turn this
// runner does not run, or one of another session, has none.
func (r *Runner) TakeNotes(session, turn string) []event.Added {
	if at := r.sessionAttempt(session, turn); at != nil {
		return at.takeNotes()
	}
	return nil
}

// ReturnNotes gives back notes a call of the turn took that the vendor did
// not accept (replay.Notes).
func (r *Runner) ReturnNotes(session, turn string, notes []event.Added) {
	if at := r.sessionAttempt(session, turn); at != nil {
		at.returnNotes(notes)
	}
}

// sessionAttempt is the attempt this runner runs turn with, when it is of
// session.
func (r *Runner) sessionAttempt(session, turn string) *attempt {
	at := r.attemptForTurn(turn)
	if at == nil {
		return nil
	}
	own := string(at.job.Spec.Session)
	if own == "" {
		own = at.job.Session
	}
	if own != session {
		return nil
	}
	return at
}
