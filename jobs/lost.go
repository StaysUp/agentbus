package jobs

// Outcomes of jobs that ended with no finish (queue/lost.go): an apply
// whose runner was lost. Control's sweep writes each one's turn.applied
// to the record, then swaps the job's due mark for the event's id.

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
			e := event.Event{ID: appliedID(j), Kind: event.TurnApplied, Time: endedAt(j), Session: j.Session,
				Turn: j.Links.AppliesTurn, Applied: AppliedOf(j, res)}
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
