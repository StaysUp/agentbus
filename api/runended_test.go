package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A remote turn that finished (its agent exited 0) but whose run.ended never
// reached control still ends for the API: it reads finished, as its job
// ended (control decides a turn's end once, runner/lease.go; the finisher
// closed its open run, jobs.closeOpenRuns), and its session takes a next turn
// under a new key
// (jobs/runended_test.go has the runner protocol's side). The control case
// is the same turn with its run.ended recorded.
func TestEndedTurnWithoutRunEndedTakesNextTurn(t *testing.T) {
	for name, recorded := range map[string]bool{"run.ended recorded": true, "run.ended never arrived": false} {
		t.Run(name, func(t *testing.T) {
			e := newJobsEnv(t)
			const machine, ref = "con_0123456789abcdef", "fld_0123456789abcdef"
			r, err := e.q.RegisterRunner(queue.DefaultTeam, "studio mac", nil, 4, queue.HashToken(queue.NewID("tok_")), &queue.Capabilities{Connector: machine})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "k-first", queue.Spec{Harness: "claude", Prompt: "add a test",
				FolderRef: ref, Machine: machine, Isolate: true}, queue.Links{}); err != nil {
				t.Fatal(err)
			}
			j, a, ok, _, err := e.q.Take(r.ID, time.Minute, queue.LaneInteractive)
			if err != nil || !ok {
				t.Fatalf("take: %v %v", ok, err)
			}
			if _, _, err := e.q.Start(r.ID, a.ID, 1, time.Minute); err != nil {
				t.Fatal(err)
			}
			seedWorkspaceOn(t, e.store, j.Session, "", ref, machine, true)
			now, exit, hold := time.Now().UTC(), 0, "hold_"+strings.Repeat("ab", 8)
			evs := []event.Event{
				{ID: "evt_00000000000000a1", Kind: event.TurnStarted, Time: now, Session: j.Session, Turn: j.Turn, Text: "add a test",
					Harness: "claude", FolderRef: ref, IdempotencyKey: "k-first"},
				{ID: "evt_00000000000000a2", Kind: event.RunStarted, Time: now, Session: j.Session, Turn: j.Turn, Hold: hold,
					Harness: "claude", FolderRef: ref},
			}
			if recorded {
				evs = append(evs, event.Event{ID: "evt_00000000000000a3", Kind: event.RunEnded, Time: now.Add(time.Second),
					Session: j.Session, Turn: j.Turn, Hold: hold, ExitCode: &exit})
			}
			appendAll(t, e.store, evs...)
			// The finish control accepts for it: the finisher's own end of the job.
			if _, err := jobs.EndAs(e.store, e.q, r.ID, a.ID, 1, j, queue.Finish{Status: queue.Finished, Session: j.Session,
				Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.Session, Snapshot: strings.Repeat("3", 64)},
				Result:    []byte(`{"session":"` + j.Session + `","exit_code":0,"state":"finished","changeset":"` + strings.Repeat("1", 64) + `"}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.CloseLost(e.q, e.store, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			resp, raw := do(t, e.req(t, "GET", "/api/v0/turns/"+j.Turn, nil))
			var one OneTurn
			if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &one) != nil {
				t.Fatalf("GET /api/v0/turns/%s: %d %s", j.Turn, resp.StatusCode, raw)
			}
			if one.Turn.State != store.TurnFinished {
				t.Errorf("the turn reads %s, want finished, as its job ended", one.Turn.State)
			}
			body := `{"agent":"claude","session":"` + j.Session + `","prompt":"now run it","idempotency_key":"k-next"}`
			resp, raw = do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
			if resp.StatusCode != http.StatusAccepted {
				t.Errorf("POST /api/v0/turns with a new key: %d %s, want 202", resp.StatusCode, raw)
			}
		})
	}
}

// How a turn ended is its job's, decided once by control (runner/lease.go),
// not its agent's run.ended: a turn whose agent exited 0 but whose changes
// control never got ended failed, and reads failed with why (review
// t_5d83bfd85c47b87c P1: it read finished, with changes_not_kept on its job
// and no changeset).
func TestTurnReadsHowItsJobEnded(t *testing.T) {
	e := newJobsEnv(t)
	r, err := e.q.RegisterRunner(queue.DefaultTeam, "studio mac", nil, 4, queue.HashToken(queue.NewID("tok_")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "k-lost", queue.Spec{Harness: "claude", Prompt: "add a test"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, a, ok, _, err := e.q.Take(r.ID, time.Minute, queue.LaneInteractive)
	if err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := e.q.Start(r.ID, a.ID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	now, exit, hold := time.Now().UTC(), 0, "hold_"+strings.Repeat("cd", 8)
	appendAll(t, e.store,
		event.Event{ID: "evt_00000000000000b1", Kind: event.TurnStarted, Time: now, Session: j.Session, Turn: j.Turn, Text: "add a test",
			Harness: "claude", IdempotencyKey: "k-lost"},
		event.Event{ID: "evt_00000000000000b2", Kind: event.RunStarted, Time: now, Session: j.Session, Turn: j.Turn, Hold: hold, Harness: "claude"},
		event.Event{ID: "evt_00000000000000b3", Kind: event.RunEnded, Time: now.Add(time.Second), Session: j.Session, Turn: j.Turn, Hold: hold, ExitCode: &exit})
	const why = "The turn's changes could not be sent to control (blobs: missing answered 413), so they cannot be applied; send the turn again."
	if _, err := jobs.EndAs(e.store, e.q, r.ID, a.ID, 1, j, queue.Finish{Status: queue.Failed, Reason: "changes_not_kept", Session: j.Session,
		Result: []byte(`{"session":"` + j.Session + `","exit_code":0,"state":"failed","error":"` + why + `"}`)}); err != nil {
		t.Fatal(err)
	}
	resp, raw := do(t, e.req(t, "GET", "/api/v0/turns/"+j.Turn, nil))
	var one OneTurn
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &one) != nil {
		t.Fatalf("GET /api/v0/turns/%s: %d %s", j.Turn, resp.StatusCode, raw)
	}
	if one.Turn.State != store.TurnFailed || one.Turn.Error != why {
		t.Fatalf("the turn reads %s (%q), want failed with why", one.Turn.State, one.Turn.Error)
	}
}
