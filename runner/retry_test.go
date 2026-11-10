package runner

import (
	"testing"
	"time"
)

// The wait doubles up to MaxRetry, a failure inside a wait does not make it
// longer, and an answer ends it.
func TestRetry(t *testing.T) {
	var b retry
	now := time.Unix(0, 0)
	var steps []time.Duration
	for range 8 {
		wait, grew := b.failed(now)
		if !grew {
			t.Fatalf("a failure after the wait did not grow it: %v", wait)
		}
		if w, grew := b.failed(now.Add(wait / 2)); grew || w != wait-wait/2 {
			t.Fatalf("a failure inside the wait: %v %v", w, grew)
		}
		steps = append(steps, wait)
		now = now.Add(wait)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if steps[i] != w*time.Second {
			t.Fatalf("steps %v", steps)
		}
	}
	if !b.answered() || b.answered() {
		t.Fatal("answered")
	}
	if wait, _ := b.failed(now); wait != time.Second {
		t.Fatalf("after an answer: %v", wait)
	}
	if got := retryLine("http://10.0.0.2:8787", 0, 4*time.Second); got != "control at http://10.0.0.2:8787 is not reachable; retrying in 4s" {
		t.Fatal(got)
	}
	if got := retryLine("http://x", 503, 30*time.Second); got != "control at http://x answered 503 to a take; retrying in 30s" {
		t.Fatal(got)
	}
}
