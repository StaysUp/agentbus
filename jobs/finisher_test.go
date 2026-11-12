package jobs

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// blockedRecord fails every write of the events it names.
type blockedRecord struct {
	*store.Store
	mu  sync.Mutex
	bad map[string]bool
}

func (r *blockedRecord) AppendOnce(e event.Event) (bool, error) {
	r.mu.Lock()
	bad := r.bad[e.ID]
	r.mu.Unlock()
	if bad {
		return false, errFailingRecord
	}
	return r.Store.AppendOnce(e)
}

// A pass reads every saved finish, a page at a time: when every finish of
// the first pages fails its record write, the one after them still ends
// in the same pass (0.5.1; review evt_c02fe38f804f530a).
func TestFinisherPassReadsPastFailedFinishes(t *testing.T) {
	old := finishPage
	finishPage = 3
	t.Cleanup(func() { finishPage = old })
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close(); st.Close() })
	rec := &blockedRecord{Store: st, bad: map[string]bool{}}
	r, err := q.RegisterRunner(queue.DefaultTeam, "r", nil, 10, "hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 7 // two full pages that fail, then one that works
	var jobs []queue.Job
	for i := range n {
		if _, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
			t.Fatal(err)
		}
		j, a, ok, _, err := q.Take(r.ID, time.Minute, false)
		if err != nil || !ok {
			t.Fatalf("take %d: %v %v", i, ok, err)
		}
		if _, _, err := q.Start(r.ID, a.ID, a.Number, time.Minute); err != nil {
			t.Fatal(err)
		}
		res, _ := json.Marshal(queue.Result{Changes: turnChanges("cp2")})
		if _, _, err := q.Accept(r.ID, a.ID, a.Number, queue.Finish{Status: queue.Finished, Result: res}); err != nil {
			t.Fatal(err)
		}
		if i < n-1 {
			rec.bad[changedID(j)] = true
		}
		jobs = append(jobs, j)
	}
	f := newFinisher(q, rec)
	f.pass()
	for i, j := range jobs {
		got, err := q.Get(queue.DefaultTeam, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == n-1; (got.Ended != nil) != want {
			t.Errorf("job %d ended %v, want %v", i, got.Ended != nil, want)
		}
	}
	if len(f.failed) != n-1 {
		t.Errorf("%d failed finishes kept, want %d", len(f.failed), n-1)
	}
}
