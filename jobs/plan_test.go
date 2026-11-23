package jobs

import (
	"encoding/json"
	"strconv"
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

// A runner sends only the kinds it writes for a turn (RunnerKinds). A
// control-only kind, a program's x.* kind and a retired kind are refused
// whole with 400, and the record holds none of them; every listed kind is
// taken.
func TestRunnerRecordAllowList(t *testing.T) {
	e := newOutcomeEnv(t)
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "x"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, base := e.take(t, queue.KindTurn)
	refused := []event.Kind{event.ToolsListed, event.SessionRenamed, event.PlanUsage, event.EventsDropped, event.TurnChanged,
		event.TurnApplied, event.SettingsChanged, event.RunnerEnrolled, event.SessionRegistered, event.CommandRan, "x.review.verdict", "made.up"}
	for i, k := range refused {
		ev, _ := json.Marshal(event.Event{ID: "e-refused-" + strconv.Itoa(i), Kind: k, Session: j.Session, Turn: j.Turn})
		if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: []json.RawMessage{ev}}, nil); code != 400 {
			t.Errorf("a runner's %s: %d, want 400", k, code)
		}
	}
	var all []json.RawMessage
	n := 0
	for k := range RunnerKinds {
		if k == event.RunEnded {
			continue // a turn's end settles it, tested with the outcome
		}
		if k == event.ApprovalRequested || k == event.ApprovalDecided {
			continue // need an attempt-scoped id; tested in TestRunnerApprovalDecisionOwnership
		}
		ev, _ := json.Marshal(event.Event{ID: "e-taken-" + strconv.Itoa(n), Kind: k, Session: j.Session, Turn: j.Turn})
		all = append(all, ev)
		n++
	}
	if code := send(t, e.srv, e.tok, "POST", base+"/record", RecordBatch{Number: 1, Events: all}, nil); code != 200 {
		t.Fatalf("the runner's own kinds: %d, want 200", code)
	}
	got := e.kinds(t, j.Session)
	for _, k := range refused {
		if got[k] != 0 {
			t.Errorf("the record holds %s", k)
		}
	}
	for k := range RunnerKinds {
		if k != event.RunEnded && k != event.ApprovalRequested && k != event.ApprovalDecided && got[k] == 0 {
			t.Errorf("the record lacks %s", k)
		}
	}
}
