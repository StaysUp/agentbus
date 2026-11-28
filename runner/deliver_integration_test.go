//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A runner that leaves while control does not answer the turn's record
// sends no finish and keeps the turn's end on disk: only control's answer
// ends what control may still hold (lease.go), and the next runner sends the
// record and the finish (review t_5d83bfd85c47b87c P2: the finish went after
// five unanswered record batches, and the kept end was removed with the
// history control never got).
func TestLeavingRunnerKeepsAnEndWhoseRecordControlNeverGot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now, exit := time.Now().UTC(), 0
	for _, e := range []event.Event{
		{ID: "evt_r1", Kind: event.RunStarted, Hold: "hold_1"},
		{ID: "evt_r2", Kind: event.RunEnded, Hold: "hold_1", ExitCode: &exit},
	} {
		e.Time, e.Session, e.Turn = now, "s1", "t_1"
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	var finishes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/record"):
			// No answer at all, as while control is away: the connection
			// closes with nothing sent. (A 5xx is an answer: send.)
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				c.Close()
			}
		case strings.HasSuffix(r.URL.Path, "/finish"):
			finishes.Add(1)
			json.NewEncoder(w).Encode(queue.Job{})
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	leave := make(chan struct{})
	close(leave) // the runner retires
	dir := t.TempDir()
	r := &Runner{cfg: Config{Queue: srv.URL, Store: st, Forward: true, Finishes: dir, Retire: leave, Logf: t.Logf},
		short: srv.Client(), token: "tok"}
	at := &attempt{r: r, job: queue.Job{ID: "job_1", Kind: queue.KindTurn, Turn: "t_1"}, session: "s1"}
	at.a.ID, at.a.Number = "att_1", 1
	at.finish(context.Background(), queue.Finish{Status: queue.Finished, Session: "s1", Result: []byte(`{"state":"finished"}`)})
	if n := finishes.Load(); n != 0 {
		t.Fatalf("the finish was sent %d times without the turn's record", n)
	}
	if _, err := os.Stat(r.keptPath("att_1")); err != nil {
		t.Fatalf("the turn's end is not kept for the next runner: %v", err)
	}
}
