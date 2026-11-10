package runner

import (
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
}

// failed says how long to wait after a failed take, and grew is true when
// this failure made the wait longer.
func (b *retry) failed(now time.Time) (wait time.Duration, grew bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	b.delay, b.until = 0, time.Time{}
	return was
}

// retryLine is the log line for a failed take: status 0 is no answer.
func retryLine(queue string, status int, wait time.Duration) string {
	in := strconv.Itoa(int(wait.Round(time.Second)/time.Second)) + "s"
	if status == 0 {
		return "control at " + queue + " is not reachable; retrying in " + in
	}
	return "control at " + queue + " answered " + strconv.Itoa(status) + " to a take; retrying in " + in
}
