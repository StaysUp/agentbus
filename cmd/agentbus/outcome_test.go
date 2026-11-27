package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// queuedAction is the answer to a workspace apply, discard, update or close:
// 202 queued with the event its outcome will be (async by default, his rule
// 2026-11-19).
type queuedAction struct {
	State string `json:"state"`
	Event string `json:"event"`
}

// awaitAction waits for the outcome of the workspace action a route answered
// 202 for, through get (GET with the main token): GET
// /api/v0/workspaces/{ws}/outcome?event= until it holds the event the reply
// named, as the route docs tell a caller to read it.
func awaitAction(t *testing.T, get func(path string) (int, []byte), ws string, code int, raw []byte) event.Event {
	t.Helper()
	var q queuedAction
	if code != http.StatusAccepted || json.Unmarshal(raw, &q) != nil || q.State != "queued" || q.Event == "" {
		t.Fatalf("workspace %s: %d %s, want 202 queued with its event", ws, code, raw)
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var e event.Event
		if c, b := get("/api/v0/workspaces/" + ws + "/outcome?event=" + q.Event); c == 200 && json.Unmarshal(b, &e) == nil && e.ID == q.Event {
			return e
		}
	}
	t.Fatalf("workspace %s: no outcome %s on the record within a minute", ws, q.Event)
	return event.Event{}
}
