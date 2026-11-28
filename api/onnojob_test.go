package api

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// An on over a turn the record holds but no job ran (an older turn, or one
// recorded at the gateway) could never fire: nothing writes its turn.ended.
// Review of dd2dcb1: it answered 404, no such turn, to a caller that may read
// the turn. Now a caller who may read it hears why (409); one who may not gets
// what it gets for any turn it cannot read.
func TestOnOverATurnWithNoJob(t *testing.T) {
	grants := map[string]run.Grant{"abr_copy": {Session: "s_asker", Turn: "t_copy", Root: "s_asker"}}
	e, _ := rulesEnv(t, grants)
	seedReach(t, e.store)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	e.api.cfg.Queue = q
	for session, turn := range map[string]string{"s_mine": "t_mine_old", "s_other": "t_other_old"} {
		if _, err := e.store.AppendOnce(event.Event{ID: "ts-" + turn, Kind: event.TurnStarted, Time: time.Now().UTC(),
			Session: session, Turn: turn}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		tok, turn, body string
		want            int
	}{
		{"abr_copy", "t_mine_old", "", http.StatusConflict},
		{"abr_copy", "t_other_old", "", http.StatusForbidden},
		{"abr_copy", "t_nowhere", "", http.StatusNotFound},
		{token, "t_other_old", `,"session":"s_other"`, http.StatusConflict},
		{token, "t_nowhere", `,"session":"s_other"`, http.StatusNotFound},
	} {
		who := c.tok
		if who == token {
			who = "the main token"
		}
		code, out := callAs(t, e, c.tok, "POST", "/api/v0/ons", `{"turn":"`+c.turn+`"`+c.body+`}`)
		if code != c.want {
			t.Errorf("%s on over %s: %d %s, want %d", who, c.turn, code, out, c.want)
		}
		if c.want == http.StatusConflict && !strings.Contains(out, "not run as a job") {
			t.Errorf("%s on over %s: %s, want why", who, c.turn, out)
		}
	}
	if ons, err := q.ListOns(queue.DefaultTeam); err != nil || len(ons) != 0 {
		t.Fatalf("ons registered: %v %v", ons, err)
	}
}
