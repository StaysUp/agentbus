package jobs

import (
	"testing"

	"github.com/staysup/agentbus/applied"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/store"
)

// recStub is a Recorder whose only real method is ApprovalEvents; the rest
// satisfy the interface for approvalState's unit test.
type recStub struct{ evs []event.Event }

func (r recStub) ApprovalEvents() ([]event.Event, error)                  { return r.evs, nil }
func (r recStub) Applied(ids.WorkspaceID) (applied.State, error)          { return applied.Empty(), nil }
func (r recStub) CostOfTurns([]string) (float64, error)                   { return 0, nil }
func (r recStub) WorkspaceUsedAfter(ids.WorkspaceID, int64) (bool, error) { return false, nil }
func (r recStub) OpenHoldsOfTurn(string) ([]store.OpenHold, error)        { return nil, nil }
func (r recStub) AppendOnce(event.Event) (bool, error)                    { return true, nil }
func (r recStub) CallSession(string) (string, bool, error) {
	return "", false, nil
}
func (r recStub) OtherTurns(string, string, string) (bool, error) { return false, nil }
func (r recStub) Continuation(string) (store.Continuation, error) {
	return store.Continuation{}, nil
}
func (r recStub) TurnText(string, string) (string, string, bool, error) { return "", "", false, nil }
func (r recStub) EventByID(string, string, string) (event.Event, bool, error) {
	return event.Event{}, false, nil
}

func req(approval string) event.Event {
	return event.Event{Kind: event.ApprovalRequested, Approval: approval}
}
func dec(approval, text string) event.Event {
	return event.Event{Kind: event.ApprovalDecided, Approval: approval, Text: text}
}

// approvalState reads a held approval's state from the record by its id. The id
// is scoped to the attempt that raised it (the decision handler enforces the
// attempt prefix, TestXProcHold*), so reading by id alone is safe: pending
// until the record holds the approval's request and a decision for it.
func TestApprovalState(t *testing.T) {
	const id = "t_abc-ap-1"
	for _, tc := range []struct {
		name     string
		evs      []event.Event
		approval string
		state    string
	}{
		{"pending: requested, not decided",
			[]event.Event{req(id)}, id, DecisionPending},
		{"allowed: a person allowed from now on",
			[]event.Event{req(id), dec(id, "allowed from now on by a person")}, id, DecisionAllowed},
		{"allowed: a person allowed once",
			[]event.Event{req(id), dec(id, "allowed once by a person")}, id, DecisionAllowed},
		{"refused: a person refused",
			[]event.Event{req(id), dec(id, "refused by a person")}, id, DecisionRefused},
		{"refused: the gateway timed out",
			[]event.Event{req(id), dec(id, "no one answered in time")}, id, DecisionRefused},
		{"pending: a decision with no request in the record is ignored",
			[]event.Event{dec(id, "allowed once by a person")}, id, DecisionPending},
		{"unknown approval reads pending",
			[]event.Event{req(id)}, "t_abc-ap-other", DecisionPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Record: recStub{evs: tc.evs}}}
			got, err := s.approvalState(tc.approval)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.state {
				t.Fatalf("approvalState = %q, want %q", got, tc.state)
			}
		})
	}
}

// scopeToAttempt binds an id to the attempt and is idempotent for one already
// scoped to it: a raw or foreign id is prefixed, an own-scoped id is left.
func TestScopeToAttempt(t *testing.T) {
	const at = "t_abc"
	for _, tc := range []struct{ in, want string }{
		{"ap-run-5", "t_abc-ap-run-5"},           // a legacy raw id is bound
		{"t_abc-ap-run-5", "t_abc-ap-run-5"},     // already this attempt's: left as is (idempotent)
		{"t_victim-ap-9", "t_abc-t_victim-ap-9"}, // a foreign id is bound to THIS attempt, not the named one
		{"t_abcd-ap-1", "t_abc-t_abcd-ap-1"},     // a near-prefix (t_abcd vs t_abc) is still foreign: bound
	} {
		if got := scopeToAttempt(at, tc.in); got != tc.want {
			t.Errorf("scopeToAttempt(%q, %q) = %q, want %q", at, tc.in, got, tc.want)
		}
	}
}

func TestApprovalAllowed(t *testing.T) {
	for text, want := range map[string]bool{
		"allowed once by a person":                       true,
		"allowed from now on by a person":                true,
		"refused by a person":                            false,
		"no one answered in time":                        false,
		"the agent's connection closed before an answer": false,
	} {
		if got := approvalAllowed(text); got != want {
			t.Errorf("approvalAllowed(%q) = %v, want %v", text, got, want)
		}
	}
}
