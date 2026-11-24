//go:build integration

package main

// H1 across real processes: a sandboxed turn's model reply asks to run an Ask
// command, the runner's own gateway HOLDS the streamed reply (it does not
// refuse it inline), and learns the decision by polling control through the
// runner protocol (GET /api/v0/runner/attempts/{id}/decision). An answer on
// control releases the held call so the agent runs it in the same turn; no
// answer times the hold out, which refuses the call and records
// approval.decided ITSELF — a runner event that must reach control's record
// (relayOnly + RunnerKinds), so the approval stops listing with no one having
// answered. Neither ending starts a continuation turn (his call, 2026-11-17).
//
// The reply is a real SSE stream from the fake upstream (AGENTBUS_TEST_UPSTREAM
// on control), the one shape a hold needs and the unstreamed
// TestXProcApprovalFromRunnerReachesControl cannot exercise. The hold is
// shortened on the runner (AGENTBUS_TEST_HOLD_MS) so the timeout path does not
// take a real minute.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// askStreamUpstream is a fake vendor that streams an Anthropic reply whose one
// tool call runs a Bash command the policy holds for a person (ask).
func askStreamUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	const stream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git push\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, stream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// setAskRule holds "git push" for a person on control.
func setAskRule(t *testing.T, x *xproc) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"ask": []string{"git push"}})
	req, _ := http.NewRequest("PUT", x.base+"/api/v0/settings/permissions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+x.tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("set the ask rule: %d", resp.StatusCode)
	}
}

// xEvent is one event of a session's record, kind and text.
type xEvent struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func (x *xproc) events(session string) []xEvent {
	x.t.Helper()
	code, b := x.get("/api/v0/sessions/" + session + "/events")
	if code != 200 {
		x.t.Fatalf("GET events: %d %s", code, b)
	}
	var page struct {
		Events []xEvent `json:"events"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		x.t.Fatalf("events: %v: %s", err, b)
	}
	return page.Events
}

// decidedTexts is the text of every approval.decided in a session's record.
func (x *xproc) decidedTexts(session string) []string {
	var out []string
	for _, e := range x.events(session) {
		if e.Kind == "approval.decided" {
			out = append(out, e.Text)
		}
	}
	return out
}

// listedApproval returns the pending approval's id for "git push", or "".
func (x *xproc) listedApproval() string {
	code, b := x.get("/api/v0/approvals")
	if code != 200 {
		return ""
	}
	var page struct {
		Approvals []struct {
			ID       string   `json:"id"`
			Commands []string `json:"commands"`
		} `json:"approvals"`
	}
	if json.Unmarshal(b, &page) != nil {
		return ""
	}
	for _, a := range page.Approvals {
		for _, c := range a.Commands {
			if c == "git push" {
				return a.ID
			}
		}
	}
	return ""
}

// A streamed Ask reply on a remote runner is HELD, then released when a person
// answers allow on control: the agent runs the command in the same turn (no
// rename, no tool.refused), control records exactly one approval.decided — the
// person's, not the gateway's — and no continuation turn is started.
func TestXProcHoldReleasedByAnswer(t *testing.T) {
	folder := t.TempDir()
	upstream := askStreamUpstream(t)
	defer upstream.Close()

	x := startServerUpstream(t, upstream.URL, "claude")
	setAskRule(t, x)
	x.setAgentEnv("AGENTBUS_FAKE_CALL=1")
	x.addRunner("far", folder)
	fs := x.folders()

	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref,
		"machine": fs[0].Machine, "prompt": "do a thing", "isolate": false})
	if code/100 != 2 || st.Session == "" {
		t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
	}

	var id string
	waitFor(t, "the held approval listed on control", func() bool {
		id = x.listedApproval()
		return id != ""
	})

	if code, b := x.post("/api/v0/approvals/"+id, map[string]any{"decision": "allow"}); code != 204 {
		t.Fatalf("decide allow: %d %s", code, b)
	}
	// Prove the hold RELEASED on the allow, by a signal after the hold, not the
	// relay's pre-hold tool.requested. The agent's model call blocks inside the
	// hold until it ends; the turn's run.ended cannot land until that call
	// returns and the agent finishes. The hold here is the full 60 s (no
	// AGENTBUS_TEST_HOLD_MS), so a run.ended within seconds can ONLY mean
	// the decision poll reached control and the allow released the reply — a
	// broken poll would hold until the 60 s timeout and fail this wait.
	waitFor(t, "the released turn finished", func() bool {
		return hasKind(x.recordKinds(st.Session), "run.ended")
	})
	if a := x.listedApproval(); a != "" {
		t.Fatalf("the answered approval still lists: %s", a)
	}

	// always/allow moved the command to the allow list.
	if code, b := x.get("/api/v0/settings/permissions"); code != 200 || !strings.Contains(string(b), `"git push"`) {
		t.Fatalf("after allow, permissions: %d %s", code, b)
	}
	// The record holds exactly one approval.decided, the person's — the gateway
	// records none for an answered hold — and the released call was not refused.
	dec := x.decidedTexts(st.Session)
	if len(dec) != 1 || !strings.Contains(dec[0], "allowed") {
		t.Fatalf("approval.decided texts %q: want one person allow", dec)
	}
	for _, e := range x.events(st.Session) {
		if e.Kind == "tool.refused" {
			t.Fatalf("a released held call was refused: %q", e.Text)
		}
	}
	// No continuation turn: the one session the turn ran in, and no other.
	if ids := x.sessionIDs(); len(ids) != 1 {
		t.Fatalf("a hold released by an answer started another turn: sessions %v", ids)
	}
}

// A streamed Ask reply on a remote runner that no one answers times the hold
// out: the gateway refuses the call AND records approval.decided itself, which
// must reach control's record (relayOnly + RunnerKinds) so the approval stops
// listing with no person having answered. No continuation turn is started.
func TestXProcHoldTimesOutReachesControl(t *testing.T) {
	folder := t.TempDir()
	upstream := askStreamUpstream(t)
	defer upstream.Close()

	x := startServerUpstream(t, upstream.URL, "claude")
	setAskRule(t, x)
	// Shorten the hold on the runner, and make one model call.
	x.setAgentEnv("AGENTBUS_FAKE_CALL=1", "AGENTBUS_TEST_HOLD_MS=600")
	x.addRunner("far", folder)
	fs := x.folders()

	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref,
		"machine": fs[0].Machine, "prompt": "do a thing", "isolate": false})
	if code/100 != 2 || st.Session == "" {
		t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
	}

	// The approval lists while the hold runs, then stops once the hold times out
	// and the gateway's approval.decided reaches control.
	waitFor(t, "the held approval listed on control", func() bool { return x.listedApproval() != "" })
	waitFor(t, "the timed-out approval stops listing", func() bool { return x.listedApproval() == "" })

	// The gateway recorded the timeout itself, and that reached control: the one
	// approval.decided says no one answered, not a person's wording.
	dec := x.decidedTexts(st.Session)
	if len(dec) != 1 || !strings.Contains(dec[0], "no one answered") {
		t.Fatalf("approval.decided texts %q: want one gateway timeout", dec)
	}
	// The timed-out call was refused, and no continuation turn was started.
	refused := false
	for _, e := range x.events(st.Session) {
		if e.Kind == "tool.refused" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the timed-out held call was not refused")
	}
	if ids := x.sessionIDs(); len(ids) != 1 {
		t.Fatalf("a timed-out hold started another turn: sessions %v", ids)
	}
}
