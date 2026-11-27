package jobs

// Outcomes of jobs that ended with no finish (queue/lost.go): an apply,
// discard, update or close whose runner was lost (or, but the apply, removed
// before it took it), and a put-away control kept when it came due.
// Control's sweep writes each one's event (workspace.applied,
// workspace.kept) to the record, then swaps the job's due mark for the
// event's id.

import (
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// Appender stores an event once by its id.
type Appender interface {
	AppendOnce(e event.Event) (bool, error)
}

// lostPage is how many due outcomes one read takes.
var lostPage = 100

// CloseLostApplies writes the turn.applied of each apply that ended with
// no finish (failed, its runner lost), and says how many it wrote. It
// reads them a page at a time, each page from past the last one read, so
// a write that fails is tried again on the next call and holds up none of
// the others; the first error is answered after the rest are done.
func CloseLostApplies(q *queue.Queue, rec Appender) (int, error) {
	n := 0
	var first error
	var after *queue.Job
	for {
		due, err := q.OutcomesDue(after, lostPage)
		if err != nil {
			return n, err
		}
		for _, j := range due {
			res, _ := queue.ReadResult(j.Result)
			ws := queue.CopyKey(j.Spec.Workspace, j.Session)
			e := event.Event{ID: appliedID(j), Kind: event.WorkspaceApplied, Time: endedAt(j), Workspace: string(ws), Session: j.Session,
				Turn: j.Links.AppliesTurn, Applied: AppliedOf(j, res)}
			if f, ok := FailedOf(j, res.Refused); ok && j.Kind != queue.KindPutAway {
				// A discard, update or close whose runner was lost, or removed
				// before it took it: it did nothing.
				e = f
			}
			if j.Kind == queue.KindPutAway {
				// A put-away control kept when it came due (queue/putaway.go).
				e = event.Event{ID: keptID(j), Kind: event.WorkspaceKept, Time: endedAt(j), Workspace: string(ws), PutAway: PutAwayOf(j, res.Kept)}
			}
			_, err := rec.AppendOnce(e)
			if err == nil {
				err = q.KeepOutcome(j.ID, e.ID)
			}
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			n++
		}
		if len(due) < lostPage {
			return n, first
		}
		after = &due[len(due)-1]
	}
}
