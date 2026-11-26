package jobs

import (
	"encoding/json"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
)

// Old jobs carry no lineage marker. Migrating their recorded outcomes must
// preserve a reopened copy, and both the queue and workspace view read it.
func TestCloseHistoryWithoutJobStamps(t *testing.T) {
	e := newMigEnv(t)
	first := e.oldTurn("", cs('1'), "cp1", false)
	ws := ids.WorkspaceID(first.Session)
	sandbox := true
	e.append(event.Event{ID: "wc_test", Kind: event.WorkspaceCreated, Workspace: string(ws), Sandbox: &sandbox})
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindClose, "", queue.Spec{Workspace: ws, Machine: migCon}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, a := e.take(false)
	if _, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished}); err != nil {
		t.Fatal(err)
	}
	if w, _, err := e.st.Workspace(ws); err != nil || w.HeadTurn != "" || w.AppliedTurn != "" || w.Closed == nil {
		t.Fatalf("closed workspace: %+v %v", w, err)
	}
	second := e.oldTurn(ids.SessionID(first.Session), cs('2'), "cp2", false)
	if n, err := MigrateApplied(e.st, e.q); err != nil || n != 1 {
		t.Fatalf("migration: %d %v", n, err)
	}
	w, _, err := e.st.Workspace(ws)
	if err != nil || w.HeadTurn != second.Turn || w.Closed != nil {
		t.Fatalf("reopened workspace: %+v %v", w, err)
	}
	head, _, ok, err := e.q.AppliableCursor(queue.DefaultTeam, ws)
	if err != nil || !ok || head != w.HeadTurn {
		t.Fatalf("queue head %q (%v %v), workspace head %q", head, ok, err, w.HeadTurn)
	}
	after, _, err := e.q.Apply(queue.DefaultTeam, second.Turn)
	if err != nil || after.Spec.ApplyFrom != "" || after.Spec.ApplyTo != cs('2') {
		t.Fatalf("apply: %+v %v", after.Spec, err)
	}
	if _, err := e.q.Cancel(queue.DefaultTeam, after.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := e.q.CloseWorkspace(queue.DefaultTeam, ws, migCon, nil); err != nil || !created {
		t.Fatalf("close reopened old jobs: %v %v", created, err)
	}
	j, a = e.take(false)
	if _, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished}); err != nil {
		t.Fatal(err)
	}
	if head, _, ok, err := e.q.AppliableCursor(queue.DefaultTeam, ws); err != nil || ok || head != "" {
		t.Fatalf("queue after close: %q %v %v", head, ok, err)
	}
	if w, _, err := e.st.Workspace(ws); err != nil || w.HeadTurn != "" || w.Closed == nil {
		t.Fatalf("workspace after close: %+v %v", w, err)
	}
}

func TestCloseOutcomeIsRecordedOnce(t *testing.T) {
	e := newMigEnv(t)
	ws := ids.WorkspaceID("ws_1111111111111111")
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindClose, "", queue.Spec{Workspace: ws, Machine: migCon}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	j, a := e.take(false)
	for range 2 {
		ended, err := EndAs(e.st, e.q, e.r.ID, a.ID, a.Number, j, queue.Finish{Status: queue.Finished})
		if err != nil {
			t.Fatal(err)
		}
		var result queue.Result
		if json.Unmarshal(ended.Result, &result) != nil || result.ClosedEvent != closedID(j) {
			t.Fatalf("close result: %s", ended.Result)
		}
	}
	if state, err := e.st.Applied(ws); err != nil || state.ClosedAt == nil {
		t.Fatalf("closed state: %+v %v", state, err)
	}
}
