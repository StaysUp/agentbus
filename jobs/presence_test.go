package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
)

// A runner that stops asking for work is noted gone once, and back once it
// asks again; one already quiet when control started is not noted.
func TestPresence(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	_, _ = q.RegisterRunner(queue.DefaultTeam, "old", nil, 1, "h1", nil)
	now = now.Add(2 * queue.RunnerFresh)
	rn, _ := q.RegisterRunner(queue.DefaultTeam, "r", nil, 1, "h2", nil)
	var p Presence
	check := func(at time.Time) (gone, back []queue.Runner) {
		t.Helper()
		gone, back, err := p.Check(q, at)
		if err != nil {
			t.Fatal(err)
		}
		return gone, back
	}
	if g, b := check(now); len(g)+len(b) != 0 {
		t.Fatalf("first check: %v %v", g, b)
	}
	later := now.Add(queue.RunnerFresh + time.Second)
	if g, b := check(later); len(g) != 1 || g[0].ID != rn.ID || len(b) != 0 {
		t.Fatalf("gone: %v %v", g, b)
	}
	if g, b := check(later); len(g)+len(b) != 0 {
		t.Fatalf("again: %v %v", g, b)
	}
	now = later
	if _, err := q.RunnerBeat(rn.ID, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if g, b := check(now); len(g) != 0 || len(b) != 1 || b[0].ID != rn.ID {
		t.Fatalf("back: %v %v", g, b)
	}
}
