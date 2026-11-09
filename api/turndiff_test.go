package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// The gateway stores a call's tool.refused before its tool.requested: the
// diff still names the rule on that call, and its command.
func TestTurnDiffKeepsARefusalStoredFirst(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	for _, ev := range []event.Event{
		{ID: "evt_1", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_1", Text: "push it", Harness: "claude"},
		{ID: "evt_2", Kind: event.ToolRefused, Time: now, Session: "s1", Turn: "t_1", Vendor: "anthropic", ToolID: "toolu_1",
			ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`), Rule: "git push"},
		{ID: "evt_3", Kind: event.ToolRequested, Time: now, Session: "s1", Turn: "t_1", Vendor: "anthropic", ToolID: "toolu_1",
			ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`)},
	} {
		if _, _, err := e.store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := do(t, e.req(t, "GET", "/api/v0/turns/t_1/diff", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var d TurnDiff
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Tools.Count != 1 || len(d.Tools.Calls) != 1 || d.Tools.Calls[0].RefusedRule != "git push" ||
		len(d.Commands) != 1 || d.Commands[0] != "git push" || d.Files != nil || d.Apply.State != "none" {
		t.Fatalf("diff: %s", body)
	}
}
