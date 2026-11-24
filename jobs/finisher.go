package jobs

// The finisher: control's one ender of jobs (from 0.5). A runner's finish
// route only saves the finish (queue.Accept) and wakes it; the finisher
// alone writes each saved finish's outcome to the record, from the saved
// finish, then ends the job (endAccepted). One per control, and one control
// per data folder (the store's lock), so no two enders ever meet: what one
// reads of a saved finish cannot change under it. It takes the saved
// finishes one at a time, when woken and once a second, so a finish saved
// before a stop is ended when control is back.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
)

// FinishWait is how long the finish route waits for the finisher to end
// the job before it answers 503: the runner sends the finish again, which
// changes nothing, and waits again.
const FinishWait = 5 * time.Second

// errFinishing: the finisher has not ended the job yet.
var errFinishing = errors.New("jobs: the job is still being finished")

type finisher struct {
	q    *queue.Queue
	rec  Recorder
	wake chan struct{}

	mu     sync.Mutex
	passes int            // passes ended
	passed chan struct{}  // closed when a pass ends
	failed map[string]int // attempt → the pass whose write of it failed
}

func newFinisher(q *queue.Queue, rec Recorder) *finisher {
	return &finisher{q: q, rec: rec, wake: make(chan struct{}, 1), passed: make(chan struct{}), failed: map[string]int{}}
}

// poke asks for a pass soon.
func (f *finisher) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// run takes saved finishes until stop closes: at once, when woken, and
// once a second.
func (f *finisher) run(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		f.pass()
		select {
		case <-stop:
			return
		case <-f.wake:
		case <-t.C:
		}
	}
}

// finishPage is how many saved finishes a pass reads at a time.
var finishPage = 100

// pass ends each finish saved now, one at a time, oldest first. It reads
// them a page at a time, each page from past the last one it read: a
// finish whose write fails is tried again next pass, and never holds up
// the finishes after it, however many fail.
func (f *finisher) pass() {
	f.mu.Lock()
	n := f.passes + 1
	f.mu.Unlock()
	failed := map[string]bool{}
	var after queue.Place
	for {
		list, err := f.q.Accepted(after, finishPage)
		if err != nil {
			break
		}
		for _, a := range list {
			if _, err := endAccepted(f.rec, f.q, a); err != nil {
				failed[a.Attempt] = true // tried again next pass
			}
		}
		if len(list) < finishPage {
			break
		}
		after = list[len(list)-1].Place
	}
	f.mu.Lock()
	f.passes = n
	for id := range f.failed {
		if !failed[id] {
			delete(f.failed, id)
		}
	}
	for id := range failed {
		f.failed[id] = n
	}
	close(f.passed)
	f.passed = make(chan struct{})
	f.mu.Unlock()
}

// await waits until the finisher has ended attempt's job (team, id), and
// answers it: errOutcome when a pass that began after this call could not
// write its outcome, errFinishing when wait ran out first.
func (f *finisher) await(ctx context.Context, team, id, attempt string, wait time.Duration) (queue.Job, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	f.mu.Lock()
	after := f.passes + 1 // a pass that has begun may have read before the save
	f.mu.Unlock()
	for {
		f.mu.Lock()
		ch, passes, bad := f.passed, f.passes, f.failed[attempt]
		f.mu.Unlock()
		if passes > after {
			j, err := f.q.Get(team, id)
			if err != nil {
				return queue.Job{}, err
			}
			if j.Ended != nil {
				return j, nil
			}
			if bad > after {
				return queue.Job{}, errOutcome
			}
		}
		f.poke()
		select {
		case <-ch:
		case <-deadline.C:
			return queue.Job{}, errFinishing
		case <-ctx.Done():
			return queue.Job{}, errFinishing
		}
	}
}

// endAccepted ends the job of saved finish a: its outcome into the record
// first (keepOutcomes), from the saved finish alone, then the job's end
// (queue.End), the commit point. Only the finisher calls it.
func endAccepted(rec Recorder, q *queue.Queue, a queue.AcceptedFinish) (queue.Job, error) {
	// The outcome goes into the record and the job ends as one step for the
	// queue's readers of the applied state (queue.Outcome): none sees one
	// without the other.
	var j queue.Job
	var endErr error
	if err := q.Outcome(func() error {
		// A finished discard's dropped turns are read NOW, when it runs, not frozen
		// when it was queued: a turn that ran while the discard waited in the lane is
		// dropped too, so it is listed and marked. Keyed by the workspace (CopyKey),
		// as a discard carries no session. This discard is not finished yet, so it
		// does not exclude its own drops from the appliable turns.
		if a.Job.Kind == queue.KindDiscard && a.Finish.Status == queue.Finished {
			if ws := queue.CopyKey(a.Job.Spec.Workspace, a.Job.Session); ws != "" {
				dropped, derr := discardDrops(rec, q, a.Job, ws)
				if derr != nil {
					return errOutcome
				}
				// For the workspace.discarded event (keepOutcomes reads the job), which
				// makes them unappliable, and on the job for its own answer.
				a.Job.Links.AppliesTurns = dropped
				if derr := q.MarkDiscarded(a.Job.Team, a.Job.ID, dropped); derr != nil {
					return errOutcome
				}
			}
		}
		// A finished clean update's head and folded turns are read NOW, when it runs,
		// not frozen when it was queued: a turn that ran while the update waited in
		// the lane is folded too. A conflict folds nothing (it changed nothing). The
		// runner's result says clean (update.to set) or conflict.
		if a.Job.Kind == queue.KindUpdate && a.Finish.Status == queue.Finished {
			if res, _ := queue.ReadResult(a.Finish.Result); res.Update.Clean() {
				if ws := queue.CopyKey(a.Job.Spec.Workspace, a.Job.Session); ws != "" {
					head, folded, ferr := updateFolds(rec, q, a.Job, ws)
					if ferr != nil {
						return errOutcome
					}
					// For the workspace.updated event (keepOutcomes reads the job), from
					// which a later apply targets the head as the merge and the folded
					// turns stop being appliable; and on the job for its own answer.
					a.Job.Links.AppliesTurn = head
					a.Job.Links.AppliesTurns = folded
					if ferr := q.MarkUpdated(a.Job.Team, a.Job.ID, head, folded); ferr != nil {
						return errOutcome
					}
				}
			}
		}
		kept, kerr := keepOutcomes(rec, a.Job, a.Finish)
		if kerr != nil {
			return errOutcome
		}
		fin := a.Finish
		fin.Kept = kept
		j, endErr = q.End(a.Runner, a.Attempt, a.Number, fin)
		return nil
	}); err != nil {
		return queue.Job{}, err
	}
	if endErr != nil {
		return j, endErr
	}
	// Record the turn's end AFTER End commits the job's final state, so a
	// reactor that sees turn.ended (FireOns) always reads the job in its final
	// state, never mid-End (review P1, 2026-11-19). Best-effort: a failure here
	// must NOT undo the ended job, and the sweep's reconciler backfills any
	// turn.ended the finisher missed, idempotent by job id (his must #1).
	// turn.changed above is the job's outcome and still blocks End; turn.ended
	// is only the reactor trigger.
	_, _ = recordTurnEnded(rec, a.Job, queue.TurnState(a.Finish.Status), a.Finish.Reason, time.Now().UTC())
	return j, nil
}

// EndAs ends attempt number of job j (as it was taken) with finish f the way
// control's finisher does (endAccepted): its outcome into the record first,
// then the job's end. For a caller that drives a queue without the runner
// protocol, as the API's tests do, so the record holds what it would.
func EndAs(rec Recorder, q *queue.Queue, runner, attempt string, number int, j queue.Job, f queue.Finish) (queue.Job, error) {
	return endAccepted(rec, q, queue.AcceptedFinish{Runner: runner, Attempt: attempt, Number: number, Job: j, Finish: f})
}

// discardDrops are the turns discard job j drops, read from the record when it
// runs. A pass that already wrote the job's workspace.discarded (and stopped
// before End) takes them from that event: the record already counts them as
// dropped, so reading the appliable turns again would find none.
func discardDrops(rec Recorder, q *queue.Queue, j queue.Job, ws ids.WorkspaceID) ([]string, error) {
	if e, ok, err := rec.EventByID(j.Session, "", discardID(j)); err != nil {
		return nil, err
	} else if ok && e.Discarded != nil {
		return e.Discarded.DroppedTurns, nil
	}
	return q.DroppedTurns(j.Team, ws)
}

// updateFolds are the head and folded turns of clean update job j, read from
// the record when it runs; from the job's workspace.updated when an earlier
// pass already wrote it (discardDrops says why).
func updateFolds(rec Recorder, q *queue.Queue, j queue.Job, ws ids.WorkspaceID) (string, []string, error) {
	if e, ok, err := rec.EventByID(j.Session, "", updateID(j)); err != nil {
		return "", nil, err
	} else if ok && e.Updated != nil {
		return e.Updated.HeadTurn, e.Updated.FoldedTurns, nil
	}
	return q.UpdateFolds(j.Team, ws)
}

// RunFinisher runs control's one finisher until stop closes (finisher.go).
// Its owner starts it once; without it, finishes are saved and wait.
func (s *Server) RunFinisher(stop <-chan struct{}) {
	if s.fin == nil {
		return
	}
	s.fin.run(stop)
}
