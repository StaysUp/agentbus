package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/replay"
	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// A session replay cannot continue says why, and says Unusable only when the
// newest turn's history can never be rebuilt, as after the service stopped
// during that turn. A session with no turn is not Unusable, and an error from
// the store stays an error.
func TestReplayPlanSaysWhenAHistoryIsLostForGood(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	for _, e := range []event.Event{
		{ID: "e1", Kind: event.TurnStarted, Time: now, Session: "s", Turn: "t1", Text: "one"},
		{ID: "e2", Kind: event.RunEnded, Time: now, Session: "s", Turn: "t1", Unsettled: true, State: store.TurnFailed},
	} {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	r := replay.New(st)

	p, err := replayPlan(r.Base("s"))
	if err != nil || p.Turn != "" || !p.Unusable || !strings.Contains(p.Refused, "turn t1 may be missing events") {
		t.Errorf("a turn the service never settled: %+v %v, want Unusable naming t1", p, err)
	}
	p, err = replayPlan(r.Base("none"))
	if err != nil || p.Turn != "" || p.Unusable || p.Refused == "" {
		t.Errorf("a session with no turn: %+v %v, want refused, not Unusable", p, err)
	}
	failed := errors.New("store: read a turn")
	if p, err = replayPlan(replay.Plan{}, failed); !errors.Is(err, failed) || p != (engine.ReplayPlan{}) {
		t.Errorf("a store error: %+v %v, want the error", p, err)
	}
	if p, err = replayPlan(replay.Plan{Turn: "t1", Failed: "it stopped"}, nil); err != nil || p != (engine.ReplayPlan{Turn: "t1", Failed: "it stopped"}) {
		t.Errorf("a plan: %+v %v", p, err)
	}
}
