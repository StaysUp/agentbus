package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
)

// A runner stopped while control does not answer a turn's start drops the
// start at once: nothing was spent, and control queues the job again when it
// starts. (Review t_fe36d3855a58fd0f P1: the loop ignored the stop, and a
// cancelled sleep returned at once, so it tried over 46,000 times in 100 ms
// and the runner never stopped.)
func TestStoppedRunnerDropsAnUnansweredStart(t *testing.T) {
	var starts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			starts.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable) // control away
	}))
	defer srv.Close()
	r := &Runner{cfg: Config{Queue: srv.URL, Logf: t.Logf}, short: srv.Client(), token: "tok"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the runner is stopping
	done := make(chan struct{})
	go func() {
		r.work(ctx, queue.Job{ID: "job_1", Kind: queue.KindTurn}, queue.Attempt{ID: "att_1", Number: 1})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("work did not return after the runner stopped: %d starts sent", starts.Load())
	}
	if n := starts.Load(); n > 2 {
		t.Fatalf("%d starts sent after the runner stopped, want at most 2", n)
	}
}
