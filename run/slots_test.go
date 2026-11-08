package run

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// The run limit comes from Config.MaxRuns, and its 429 names it. A session
// runs one turn at a time.
func TestMaxRuns(t *testing.T) {
	rs := runs{max: 2}
	for i, s := range []string{"a", "b"} {
		if code, _ := rs.take(s); code != 0 {
			t.Fatalf("take %d: %d", i, code)
		}
	}
	if code, msg := rs.take("a"); code != http.StatusConflict || msg != turnRunning {
		t.Fatalf("a busy session: %d %q", code, msg)
	}
	code, msg := rs.take("c")
	if code != http.StatusTooManyRequests || !strings.HasPrefix(msg, "2 runs") {
		t.Fatalf("third: %d %q", code, msg)
	}
	rs.release(nil, "a")
	if code, _ := rs.take("c"); code != 0 {
		t.Fatalf("after a release: %d", code)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e, err := New(Config{Store: st})
	if err != nil || e.MaxRuns() != DefaultMaxRuns {
		t.Fatalf("default: %v %d", err, e.MaxRuns())
	}
	if _, err := New(Config{Store: st, MaxRuns: -1}); err == nil {
		t.Error("MaxRuns -1 accepted")
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
	if _, _, err := st.Append(event.Event{ID: "o", Kind: event.SessionOpened, Session: "s", Hold: "h1", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Store: st}); err != nil {
		t.Fatal(err)
	}
	if c, err := st.Continuation("s"); err != nil || !c.Known || c.Open {
		t.Fatalf("after New: %+v %v", c, err)
	}
}
