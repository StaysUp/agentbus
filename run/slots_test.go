package run

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// A session runs one turn at a time. The engine counts nothing else: how many
// turns run at once is control's to count, by lane (queue/lanes.go), so a
// runner starts every turn it takes (before 0.6.7 the engine refused a fifth
// with 429, whoever sent it).
func TestSessionRunsOneTurnAtATime(t *testing.T) {
	var rs runs
	for i := range 100 {
		if code, msg := rs.take("s" + strconv.Itoa(i)); code != 0 {
			t.Fatalf("take %d: %d %q", i, code, msg)
		}
	}
	if code, msg := rs.take("s0"); code != http.StatusConflict || msg != TurnRunning {
		t.Fatalf("a busy session: %d %q", code, msg)
	}
	rs.release(nil, "s0")
	if code, _ := rs.take("s0"); code != 0 {
		t.Fatalf("after a release: %d", code)
	}
}

// The engine closes the holds its previous process left open: that process
// can no longer report their exit.
func TestNewClosesStaleHolds(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.Append(event.Event{ID: "o", Kind: event.RunStarted, Session: "s", Hold: "h1", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Store: st}); err != nil {
		t.Fatal(err)
	}
	if c, err := st.Continuation("s"); err != nil || !c.Known || c.Open {
		t.Fatalf("after New: %+v %v", c, err)
	}
}
