package jobs

import (
	"encoding/json"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// A runner cannot send plan.usage: it is control's own gateway's reading
// of a subscription control holds, shown for that subscription to everyone.
// Before, a runner's codex reading on a claude turn was taken (200) and
// became the shown value. The batch is refused whole, and the record holds
// none.
func TestRunnerPlanUsageRefused(t *testing.T) {
	e := newOutcomeEnv(t)
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, base := e.take(t, queue.KindTurn)
	forged, _ := json.Marshal(event.Event{ID: "e-forged-1", Kind: event.PlanUsage, Session: j.Session, Turn: j.Turn, Vendor: "openai",
		Plan: &event.Plan{Subscription: "codex", Window: "primary", WindowMinutes: 300, UsedPercent: 99}})
	if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{forged}}, nil); code != 400 {
		t.Fatalf("a runner's plan.usage: %d, want 400", code)
	}
	if k := e.kinds(t, j.Session); k[event.PlanUsage] != 0 {
		t.Fatalf("the record holds %v", k)
	}
	if plans, err := e.st.PlanUsage(); err != nil || len(plans) != 0 {
		t.Fatalf("plan usage: %+v %v", plans, err)
	}
}
