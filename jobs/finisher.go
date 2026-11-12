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
	kept, err := keepOutcomes(rec, a.Job, a.Finish)
	if err != nil {
		return queue.Job{}, errOutcome
	}
	fin := a.Finish
	fin.Kept = kept
	return q.End(a.Runner, a.Attempt, a.Number, fin)
}

// RunFinisher runs control's one finisher until stop closes (finisher.go).
// Its owner starts it once; without it, finishes are saved and wait.
func (s *Server) RunFinisher(stop <-chan struct{}) {
	if s.fin == nil {
		return
	}
	s.fin.run(stop)
}
