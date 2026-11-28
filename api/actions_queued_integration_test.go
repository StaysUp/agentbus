//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// Apply, discard and update answer at once with the queued job and the event
// its outcome will be, as Close does since 6bce6a8 and POST /api/v0/turns since
// 0.6.7 (async by default, his rule 2026-11-19). They waited up to TurnWait
// (30 seconds by default) for the job to run. Nothing takes the job here until
// the reply is in; then the test ends it as a runner would, and the outcome,
// refusals and all, is GET /api/v0/workspaces/{id}'s last_action under that
// event's id.
func TestWorkspaceActionsAnswerQueuedAtOnce(t *testing.T) {
	refusal := `{"refused":"there is no sandbox copy to discard","status":400}`
	for _, tc := range []struct {
		action, body string
		folder       bool // the job is a folder job (an apply)
		finish       queue.Finish
		check        func(t *testing.T, e event.Event)
	}{
		{action: "apply", body: "{}", folder: true,
			finish: queue.Finish{Status: queue.Failed, Reason: queue.ReasonConflict, Result: []byte(
				`{"refused":"a.txt changed in your folder","apply":{"files":[],"count":0,"conflicts":[{"path":"a.txt","why":"changed in your folder since the checkpoint"}]}}`)},
			check: func(t *testing.T, e event.Event) {
				a := e.Applied
				if e.Kind != event.WorkspaceApplied || a == nil || a.Result != "refused" || a.Error != "a.txt changed in your folder" ||
					len(a.Conflicts) != 1 || a.Conflicts[0].Path != "a.txt" || a.Conflicts[0].Why != "changed in your folder since the checkpoint" {
					t.Fatalf("the refused apply's outcome lost its reasons: %+v %+v", e, a)
				}
			}},
		{action: "discard",
			finish: queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: []byte(refusal)},
			check: func(t *testing.T, e event.Event) {
				if e.Kind != event.WorkspaceFailed || e.Failed == nil || e.Failed.Action != "discard" ||
					e.Failed.Error != "there is no sandbox copy to discard" || e.Failed.By == nil || e.Failed.By.Kind != "person" {
					t.Fatalf("the failed discard's outcome: %+v %+v", e, e.Failed)
				}
			}},
		{action: "update", body: "{}",
			finish: queue.Finish{Status: queue.Finished, Result: []byte(`{"update":{"conflicts":["a.txt"]}}`)},
			check: func(t *testing.T, e event.Event) {
				if e.Kind != event.WorkspaceUpdated || e.Updated == nil || e.Updated.Result != "conflict" ||
					len(e.Updated.Conflicts) != 1 || e.Updated.Conflicts[0] != "a.txt" {
					t.Fatalf("the update's outcome: %+v %+v", e, e.Updated)
				}
			}},
		{action: "update", body: `{"mode":"merge"}`,
			finish: queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup, Result: []byte(`{"refused":"the update was stopped; nothing was changed","status":409}`)},
			check: func(t *testing.T, e event.Event) {
				if e.Kind != event.WorkspaceFailed || e.Failed == nil || e.Failed.Action != "update" ||
					e.Failed.Error != "the update was stopped; nothing was changed" {
					t.Fatalf("the failed update's outcome: %+v %+v", e, e.Failed)
				}
			}},
	} {
		t.Run(tc.action+tc.body, func(t *testing.T) {
			// A wait of an hour: a route that still waited would hold the reply
			// far past the deadline below.
			e, q, r, session, _, _ := changesQueue(t, time.Hour)
			ws := session // changesQueue's workspace is its session's own id
			post := func() (int, map[string]any) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var body io.Reader
				if tc.body != "" {
					body = strings.NewReader(tc.body)
				}
				resp, err := http.DefaultClient.Do(e.req(t, "POST", "/api/v0/workspaces/"+ws+"/"+tc.action, body).WithContext(ctx))
				if err != nil {
					t.Fatalf("%s: %v, want an answer at once", tc.action, err)
				}
				defer resp.Body.Close()
				var out map[string]any
				raw, _ := io.ReadAll(resp.Body)
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatalf("%s: %d %s", tc.action, resp.StatusCode, raw)
				}
				return resp.StatusCode, out
			}
			code, out := post()
			evt, _ := out["event"].(string)
			if code != http.StatusAccepted || out["state"] != "queued" || !strings.HasPrefix(evt, "evt_") {
				t.Fatalf("%s: %d %v, want 202 queued with its event", tc.action, code, out)
			}
			if tc.action == "apply" {
				// The same apply again while it waits is the same job: its
				// idempotency key holds, and it names the same event.
				if code, again := post(); code != http.StatusAccepted || again["event"] != evt {
					t.Fatalf("second apply: %d %v, want 202 with event %s", code, again, evt)
				}
			} else if code, again := post(); code != http.StatusConflict {
				t.Fatalf("second %s while the first waits: %d %v, want 409", tc.action, code, again)
			}
			// Nothing on the record yet: the job has not run.
			var w store.Workspace
			if code := getJSON(t, e, "/api/v0/workspaces/"+ws, &w); code != 200 || w.LastAction != nil {
				t.Fatalf("workspace before the job ran: %d %+v", code, w.LastAction)
			}

			// A runner takes it and ends it, through control's finisher.
			j, at, ok, _, err := q.Take(r.ID, 30*time.Second, laneOf(tc.folder))
			if err != nil || !ok || jobs.OutcomeID(j) != evt {
				t.Fatalf("take: %v %v %s, want the job of %s", ok, err, j.Kind, evt)
			}
			if _, _, err := q.Start(r.ID, at.ID, at.Number, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.EndAs(e.store, q, r.ID, at.ID, at.Number, j, tc.finish); err != nil {
				t.Fatal(err)
			}
			if code := getJSON(t, e, "/api/v0/workspaces/"+ws, &w); code != 200 || w.LastAction == nil || w.LastAction.ID != evt {
				t.Fatalf("workspace after the job ran: %d %+v, want last_action %s", code, w.LastAction, evt)
			}
			if w.LastAction.Workspace != ws {
				t.Fatalf("the outcome names workspace %q, want %q", w.LastAction.Workspace, ws)
			}
			tc.check(t, *w.LastAction)
		})
	}
}
