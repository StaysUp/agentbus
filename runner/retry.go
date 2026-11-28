package runner

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// MaxRetry bounds the wait between takes while control does not answer.
const MaxRetry = 30 * time.Second

// retry is the wait every take loop of a runner shares while control does
// not answer: it doubles from a second up to MaxRetry, and is back to none
// at the first answer. Loops that fail while a wait is on wait it out
// without making it longer, so one line is logged per step.
type retry struct {
	mu    sync.Mutex
	delay time.Duration
	until time.Time
	since time.Time // the first failure since the last answer; zero while control answers
}

// failed says how long to wait after a failed take, and grew is true when
// this failure made the wait longer.
func (b *retry) failed(now time.Time) (wait time.Duration, grew bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.since.IsZero() {
		b.since = now
	}
	if now.Before(b.until) {
		return b.until.Sub(now), false
	}
	b.delay = min(max(2*b.delay, time.Second), MaxRetry)
	b.until = now.Add(b.delay)
	return b.delay, true
}

// answered resets the wait, and says whether one was on.
func (b *retry) answered() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.delay > 0
	b.delay, b.until, b.since = 0, time.Time{}, time.Time{}
	return was
}

// heard notes an answer from control to another call (a heartbeat, a
// finish): control is back, though the take loops may still be waiting.
func (b *retry) heard() {
	b.mu.Lock()
	b.since = time.Time{}
	b.mu.Unlock()
}

// away is how long control has not answered a take; zero while it answers.
func (b *retry) away(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.since.IsZero() {
		return 0
	}
	return now.Sub(b.since)
}

// retryLine is the log line for a failed take: status 0 is no answer.
func retryLine(queue string, status int, wait time.Duration) string {
	in := strconv.Itoa(int(wait.Round(time.Second)/time.Second)) + "s"
	if status == 0 {
		return "control at " + queue + " is not reachable; retrying in " + in
	}
	return "control at " + queue + " answered " + strconv.Itoa(status) + " to a take; retrying in " + in
}

// FinishRetry bounds the wait between tries of a turn's end (untilAnswered)
// while control is away. Short: control on its way back is on this machine or
// near it, and the person waits for the result.
const FinishRetry = 5 * time.Second

// stopTries is how many times untilAnswered tries once the runner is stopping
// or leaving.
const stopTries = 5

// untilAnswered calls try until it says control answered, and says whether it
// did. It tries again, a second at first and up to FinishRetry apart, for as
// long as the attempt is control's: until gone says control answered that it
// is not current (lease.go), so a turn's end
// reaches control after a restart of any length, as buildkite-agent's finish
// does. Once ctx ends (the runner stopping) or leave closes (the runner
// leaving, Config.Retire), it gives up after stopTries more tries: the end is
// kept on disk for the next runner (pending.go).
func untilAnswered(ctx context.Context, leave <-chan struct{}, gone func() bool, try func() bool) bool {
	wait, after := time.Second, 0
	for {
		if try() {
			return true
		}
		if gone() {
			return false
		}
		select {
		case <-ctx.Done():
		case <-leave:
		default:
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
			case <-leave:
			case <-t.C:
			}
			t.Stop()
			wait = min(2*wait, FinishRetry)
			continue
		}
		if after++; after >= stopTries {
			return false
		}
		time.Sleep(time.Duration(after) * time.Second)
	}
}
