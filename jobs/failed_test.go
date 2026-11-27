package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
)

const failedWS = ids.WorkspaceID("ws_0123456789abcdef")

// A discard, update or close the runner refused records workspace.failed, with
// the refusal, under the id its success would have had: the id the route's 202
// named. The routes answer before the job runs, so the record is the only
// place the refusal reaches the caller; before, such a job recorded nothing.
func TestRefusedActionRecordsWorkspaceFailed(t *testing.T) {
	for kind, action := range map[string]string{queue.KindDiscard: "discard", queue.KindUpdate: "update", queue.KindClose: "close"} {
		t.Run(kind, func(t *testing.T) {
			e := newOutcomeEnv(t)
			spec := queue.Spec{Workspace: failedWS, Machine: testConnector}
			by := &queue.By{Kind: "person"}
			if _, _, err := e.q.Create(queue.DefaultTeam, kind, "", spec, queue.Links{ApplyBy: by}); err != nil {
				t.Fatal(err)
			}
			e.tok = enrollConnector(t, e.srv)
			j, base := e.take(t, kind)
			why := "there is no sandbox copy to " + action
			if code := e.finish(t, base, queue.Failed, queue.ReasonSetup, queue.Result{Refused: why, Status: 400}); code != 200 {
				t.Fatalf("finish: %d", code)
			}
			got, found, err := e.st.EventByID("", "", OutcomeID(j))
			if err != nil || !found {
				t.Fatalf("no outcome %s on the record: %v", OutcomeID(j), err)
			}
			if got.Kind != event.WorkspaceFailed || got.Workspace != string(failedWS) || got.Failed == nil ||
				got.Failed.Action != action || got.Failed.Error != why || got.Failed.By == nil || got.Failed.By.Kind != "person" {
				t.Fatalf("outcome %+v %+v", got, got.Failed)
			}
			// The job names the event in place of its refusal.
			j, _ = e.q.Get(queue.DefaultTeam, j.ID)
			if res, _ := queue.ReadResult(j.Result); res.FailedEvent != got.ID || res.Refused != "" {
				t.Fatalf("the job keeps %s", j.Result)
			}
		})
	}
}

// A discard whose runner was lost while it ran gets its workspace.failed
// from control's sweep, as a lost apply gets its workspace.applied.
func TestLostDiscardGetsWorkspaceFailed(t *testing.T) {
	e, rec, clock := newFailingEnv(t)
	spec := queue.Spec{Workspace: failedWS, Machine: testConnector}
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindDiscard, "", spec, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	e.tok = enrollConnector(t, e.srv)
	j, _ := e.take(t, queue.KindDiscard)
	clock.add(time.Hour) // the runner stopped answering
	if _, err := e.q.Sweep(); err != nil {
		t.Fatal(err)
	}
	if n, err := CloseLostApplies(e.q, rec); err != nil || n != 1 {
		t.Fatalf("closed %d, %v; want the lost discard", n, err)
	}
	got, found, err := e.st.EventByID("", "", OutcomeID(j))
	if err != nil || !found || got.Kind != event.WorkspaceFailed || got.Failed == nil || got.Failed.Action != "discard" ||
		!strings.Contains(got.Failed.Error, "stopped answering") {
		t.Fatalf("outcome %+v %v %v", got, found, err)
	}
	j, _ = e.q.Get(queue.DefaultTeam, j.ID)
	if res, _ := queue.ReadResult(j.Result); res.FailedEvent != got.ID || res.OutcomeDue {
		t.Fatalf("the job keeps %s", j.Result)
	}
	if n, err := CloseLostApplies(e.q, rec); err != nil || n != 0 {
		t.Fatalf("closed %d again, %v", n, err)
	}
}
