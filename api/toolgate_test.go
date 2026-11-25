package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// The gate's done-when (B6): an Ask on a tool call holds the agent until a
// person answers, then runs in the same turn. Two clients against one control:
// the agent's tool call is held while a person, on another connection, answers
// it. This is the two-process proof — one HTTP request held open on control,
// released by another.
func TestToolCallHeldUntilPersonAnswers(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, askAgentAsked)

	// The agent calls a does-tool: the gate holds it (no rule, effect does).
	done := make(chan map[string]any, 1)
	go func() {
		_, v := rpc(t, e, mcpWriteTok, askCall)
		done <- v
	}()

	// The person sees the held call and allows it.
	id := waitForApproval(t, e)
	answerApproval(t, e, id, "once")

	select {
	case v := <-done:
		if text := resultText(v); strings.Contains(text, "no one answered") || strings.Contains(text, "refused this call") {
			t.Errorf("the held call was not released by the person's allow: %q", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tools/call did not return after the person answered; the hold did not release")
	}
	// The allow is recorded, so the approval no longer lists as pending.
	if pending := pendingIDs(t, e); len(pending) != 0 {
		t.Errorf("approval still pending after the answer: %v", pending)
	}
}

// A person's refuse ends the hold with a plain refusal, and the call does not
// run.
func TestToolCallRefusedByPerson(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, askAgentAsked)
	done := make(chan map[string]any, 1)
	go func() {
		_, v := rpc(t, e, mcpWriteTok, askCall)
		done <- v
	}()
	id := waitForApproval(t, e)
	answerApproval(t, e, id, "refuse")

	select {
	case v := <-done:
		if dig(v, "result", "isError") != true || !strings.Contains(resultText(v), "refused this call") {
			t.Errorf("refuse: %v", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tools/call did not return after the refusal")
	}
}

// No answer in the hold window refuses the call with a plain message, and
// records approval.decided so nothing is left pending (no next turn).
func TestToolCallTimesOutWithNoAnswer(t *testing.T) {
	e := toolGateEnv(t, 120*time.Millisecond, 0, askAgentAsked)
	_, v := rpc(t, e, mcpWriteTok, askCall)
	if dig(v, "result", "isError") != true || !strings.Contains(resultText(v), "no one answered") {
		t.Errorf("timeout: %v", v)
	}
	if pending := pendingIDs(t, e); len(pending) != 0 {
		t.Errorf("a timed-out approval still lists as pending: %v", pending)
	}
}

// A reads tool with no rule runs without a hold: a read is never held. The
// window is short, so a (wrong) hold would time out rather than answer.
func TestToolCallReadsRunsWithoutHold(t *testing.T) {
	e := toolGateEnv(t, 50*time.Millisecond, 0, nil)
	_, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_history","arguments":{"q":"needle"}}}`)
	if text := resultText(v); strings.Contains(text, "no one answered") || dig(v, "result", "isError") == true {
		t.Errorf("a reads tool was held or failed: %q", text)
	}
}

// A forbid entry naming a tool refuses the call outright, with no hold, and
// records tool.refused with the entry.
func TestToolCallForbidden(t *testing.T) {
	e := toolGateEnv(t, 50*time.Millisecond, 0, func(p *settings.Policy) { p.Forbid = []string{"ask_agent"} })
	_, v := rpc(t, e, mcpWriteTok, askCall)
	if dig(v, "result", "isError") != true || !strings.Contains(resultText(v), "Forbid rule") {
		t.Errorf("forbidden tool: %v", v)
	}
	if !hasEvent(t, e, event.ToolRefused, "ask_agent") {
		t.Error("no tool.refused recorded for the forbidden call")
	}
}

// An allow entry naming a does-tool lets it run with no hold: a named rule
// overrides the effect default.
func TestToolCallAllowedByRuleRunsWithoutHold(t *testing.T) {
	e := toolGateEnv(t, 50*time.Millisecond, 0, func(p *settings.Policy) { p.Allow = append(p.Allow, "ask_agent") })
	_, v := rpc(t, e, mcpWriteTok, askCall)
	if text := resultText(v); strings.Contains(text, "no one answered") {
		t.Errorf("an allowed does-tool was held: %q", text)
	}
}

// When an ask entry matches by base name but is written in full (a path), the
// gate records that entry, so "always" moves the one that matched — not the
// bare tool name, which is not on the ask list and would leave it to ask again.
func TestToolCallAlwaysMovesTheMatchedRule(t *testing.T) {
	entry := "/usr/bin/ask_agent"
	e := toolGateEnv(t, 5*time.Second, 0, func(p *settings.Policy) { p.Ask = []string{entry} })
	done := make(chan map[string]any, 1)
	go func() {
		_, v := rpc(t, e, mcpWriteTok, askCall)
		done <- v
	}()
	id := waitForApproval(t, e)
	// The approval names the matched entry, not the bare tool name.
	if got := approvalRule(t, e, id); got != entry {
		t.Errorf("approval rule %q, want the matched entry %q", got, entry)
	}
	answerApproval(t, e, id, "always")
	<-done
	// "always" moved the matched entry from ask to allow.
	p, _, err := e.api.cfg.Settings.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if contains(p.Ask, entry) || !contains(p.Allow, entry) {
		t.Errorf("after always: ask=%v allow=%v, want %q moved to allow", p.Ask, p.Allow, entry)
	}
}

// Past the hold cap, a tool call is refused rather than held, so a runaway
// agent cannot pin control's resources. With the cap at one, a first call holds
// and a second is refused at once.
func TestToolCallAdmissionCapRefusesExtras(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 1, askAgentAsked)
	// The first call takes the one slot and holds.
	held := make(chan map[string]any, 1)
	go func() {
		_, v := rpc(t, e, mcpWriteTok, askCall)
		held <- v
	}()
	id1 := waitForApproval(t, e) // the first hold is live, holding the slot

	// A second call finds no slot and is refused at once, without an approval.
	_, v := rpc(t, e, mcpWriteTok, askCall)
	if dig(v, "result", "isError") != true || !strings.Contains(resultText(v), "too many tool calls") {
		t.Errorf("second call past the cap: %v", v)
	}
	if ids := pendingIDs(t, e); len(ids) != 1 {
		t.Errorf("want one pending (the first hold), got %v", ids)
	}
	// Release the first hold so it returns promptly (and its slot frees).
	answerApproval(t, e, id1, "refuse")
	<-held
}

// approvalRule is the rule GET /api/v0/approvals reports for an approval id.
func approvalRule(t *testing.T, e *env, id string) string {
	t.Helper()
	r, _ := http.NewRequest("GET", e.srv.URL+"/api/v0/approvals", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	_, body := do(t, r)
	var out struct {
		Approvals []struct {
			ID, Rule string
		} `json:"approvals"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	for _, a := range out.Approvals {
		if a.ID == id {
			return a.Rule
		}
	}
	return ""
}

// By design the gate does not validate a tool's inputs (his decision,
// 2026-11-18, DESIGN_0_6_tools.md #8): a does-tool is held on its name even
// when its inputs are invalid. An Ask on ask_agent with an empty prompt is
// HELD, not pre-rejected; once allowed, the route — not the gate — decides it
// and returns its own error.
func TestToolCallEmptyInputHeldThenRouteRejects(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, askAgentAsked)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"a_x","prompt":""}}}`
	done := make(chan map[string]any, 1)
	go func() { _, v := rpc(t, e, mcpWriteTok, call); done <- v }()
	id := waitForApproval(t, e) // it IS held, though the prompt is empty
	answerApproval(t, e, id, "once")
	v := <-done
	text := resultText(v)
	if dig(v, "result", "isError") != true || strings.Contains(text, "no one answered") || strings.Contains(text, "refused this call") {
		t.Errorf("want the route's own error after allow, got: %q", text)
	}
}

// Read-only turns retired (B12): there is no longer a per-call authorize
// refusal reachable through the one built-in app (ask_agent is always allowed
// a turn token), so the former read-only-token refusal test is gone. The
// authorize-before-hold path (R2) still runs; [agent_api] tools = false is
// refused at the tool server, covered in the mcp tests.

// "Always" clears EVERY ask entry that matches the tool, so a second equivalent
// entry does not keep asking after the first is moved (R2).
func TestToolCallAlwaysClearsEveryMatchingAskEntry(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, func(p *settings.Policy) {
		p.Ask = []string{"ask_agent", "/usr/bin/ask_agent"}
	})
	done := make(chan map[string]any, 1)
	go func() { _, v := rpc(t, e, mcpWriteTok, askCall); done <- v }()
	id := waitForApproval(t, e)
	answerApproval(t, e, id, "always")
	<-done
	p, _, err := e.api.cfg.Settings.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if contains(p.Ask, "ask_agent") || contains(p.Ask, "/usr/bin/ask_agent") {
		t.Errorf("ask still holds a matching entry after always: %v", p.Ask)
	}
}

// A built-in tool and a custom tool are gated by the same code: both are held
// at the one dispatch point, then time out for want of an answer.
func TestToolCallGatesBuiltinAndCustomAlike(t *testing.T) {
	// Both tools on the ask list, so the gate holds each one (under the default
	// tool:* allow a tool runs; a hold needs an ask rule).
	e := toolGateEnv(t, 120*time.Millisecond, 0, func(p *settings.Policy) {
		p.Ask = append(p.Ask, "ask_agent", "ask_twin")
	})
	// A custom tool on the same does-route as ask_agent, same effect.
	base, ok := e.api.servedByName("ask_agent")
	if !ok {
		t.Fatal("ask_agent not served")
	}
	twin := base.def
	twin.Name = "ask_twin"
	if _, err := e.api.registry.Add(twin); err != nil {
		t.Fatal(err)
	}
	e.api.rebuildTools()
	for _, name := range []string{"ask_agent", "ask_twin"} {
		call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{"agent":"a_x","prompt":"x"}}}`
		_, v := rpc(t, e, mcpWriteTok, call)
		if dig(v, "result", "isError") != true || !strings.Contains(resultText(v), "no one answered") {
			t.Errorf("%s not gated by the shared code: %v", name, v)
		}
	}
}

// Single-winner finalization (the race Codex found, 2026-11-18): when a person
// allows the call at the last instant, the timeout must honor that answer and
// run the call, never record a second decision that refuses it.
func TestHoldFinalizationHonorsAPersonsAnswer(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, nil)
	g := ptr(run.ForTurn(run.Grant{Session: "s1", Turn: "t_1", Root: "s1"}, ""))
	id := "ap-tool-race1"
	// The approval was raised, then a person allowed it (as decideApproval does).
	e.api.changed(event.Event{Kind: event.ApprovalRequested, Session: "s1", Turn: "t_1", Approval: id}, "")
	e.api.changed(event.Event{Kind: event.ApprovalDecided, Session: "s1", Turn: "t_1", Approval: id, Text: "allowed once by a person"}, "")
	// The deadline fires now: it must honor the allow, not override it.
	allow, _ := e.api.endHeldCall(g, id, "no one answered in time", holdTimeoutToolText)
	if !allow {
		t.Error("the timeout overrode a person's allow")
	}
	if n := countDecided(t, e, id); n != 1 {
		t.Errorf("want exactly one decision (the person's allow), got %d", n)
	}
}

// With no answer, the timeout records exactly one decision and refuses.
func TestHoldFinalizationRecordsTimeoutWhenUnanswered(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, nil)
	g := ptr(run.ForTurn(run.Grant{Session: "s1", Turn: "t_1", Root: "s1"}, ""))
	id := "ap-tool-race2"
	e.api.changed(event.Event{Kind: event.ApprovalRequested, Session: "s1", Turn: "t_1", Approval: id}, "")
	allow, res := e.api.endHeldCall(g, id, "no one answered in time", holdTimeoutToolText)
	if allow || res.IsError != true || !strings.Contains(resultText2(res), "no one answered") {
		t.Errorf("unanswered timeout: allow=%v res=%v", allow, res)
	}
	if n := countDecided(t, e, id); n != 1 {
		t.Errorf("want one decision (the timeout), got %d", n)
	}
}

// allow wins over a timeout on the record regardless of write order: even when
// the timeout was recorded first, a person's allow that follows is the answer
// (his H1-parity call, 2026-11-18). This is what keeps a late timeout from
// refusing an allowed call (r5), with no lock.
func TestAllowWinsOverTimeout(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, nil)
	id := "ap-tool-allowwins"
	e.api.changed(event.Event{Kind: event.ApprovalRequested, Session: "s1", Turn: "t_1", Approval: id}, "")
	e.api.changed(event.Event{Kind: event.ApprovalDecided, Session: "s1", Turn: "t_1", Approval: id, Text: "no one answered in time"}, "")
	e.api.changed(event.Event{Kind: event.ApprovalDecided, Session: "s1", Turn: "t_1", Approval: id, Text: "allowed once by a person"}, "")
	if e.api.toolApprovalState(id) != gateway.Allow {
		t.Error("allow did not win over a recorded timeout")
	}
}

// countDecided counts approval.decided events for an approval id in the record.
func countDecided(t *testing.T, e *env, id string) int {
	t.Helper()
	recs, err := e.store.After(0, store.Filter{}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, rec := range recs {
		if rec.Kind != event.ApprovalDecided {
			continue
		}
		var ev event.Event
		if json.Unmarshal(rec.Data, &ev) == nil && ev.Approval == id {
			n++
		}
	}
	return n
}

// resultText2 is a toolResult's text (the endHeldCall path returns a toolResult
// directly, not the JSON-RPC envelope resultText reads).
func resultText2(res toolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}

// askCall is a tools/call for ask_agent, the one built-in does-tool the write
// turn token lists. No rule covers it, so the gate holds it (Ask).
const askCall = `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"a_x","prompt":"renamed"}}}`

// resultText is a tool result's one piece of text.
func resultText(v map[string]any) string {
	c, _ := dig(v, "result", "content").([]any)
	if len(c) == 0 {
		return ""
	}
	s, _ := c[0].(map[string]any)["text"].(string)
	return s
}

// waitForApproval polls GET /api/v0/approvals until one lists, and returns its
// id.
func waitForApproval(t *testing.T, e *env) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ids := pendingIDs(t, e); len(ids) > 0 {
			return ids[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no approval appeared for the held tool call")
	return ""
}

// pendingIDs are the ids of the approvals GET /api/v0/approvals lists now.
func pendingIDs(t *testing.T, e *env) []string {
	t.Helper()
	r, _ := http.NewRequest("GET", e.srv.URL+"/api/v0/approvals", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	resp, body := do(t, r)
	if resp.StatusCode != 200 {
		t.Fatalf("GET approvals: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Approvals []struct {
			ID string `json:"id"`
		} `json:"approvals"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	ids := make([]string, 0, len(out.Approvals))
	for _, a := range out.Approvals {
		ids = append(ids, a.ID)
	}
	return ids
}

// answerApproval answers a held call: POST /api/v0/approvals/{id}.
func answerApproval(t *testing.T, e *env, id, decision string) {
	t.Helper()
	r, _ := http.NewRequest("POST", e.srv.URL+"/api/v0/approvals/"+id, strings.NewReader(`{"decision":"`+decision+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	resp, body := do(t, r)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST approval %s: %d %s", id, resp.StatusCode, body)
	}
}

// hasEvent says whether the store holds an event of kind k naming the tool.
func hasEvent(t *testing.T, e *env, k event.Kind, tool string) bool {
	t.Helper()
	recs, err := e.store.After(0, store.Filter{}, 10000)
	if err != nil {
		return false
	}
	for _, rec := range recs {
		if rec.Kind != k {
			continue
		}
		var ev event.Event
		if json.Unmarshal(rec.Data, &ev) == nil && ev.ToolName == tool {
			return true
		}
	}
	return false
}

// askAgentAsked puts ask_agent on the ask list, so the gate holds the call for
// a person. Under the default tool:* allow a tool runs without a hold (0.6.5);
// a hold now needs an explicit ask rule, which this shape adds.
func askAgentAsked(p *settings.Policy) { p.Ask = append(p.Ask, "ask_agent") }

// toolGateEnv is an API like mcpEnv (one write turn token of session s1, tools
// on) with the tool-hold window set short for a test, and an optional shaper
// for the policy the gate reads (to set a forbid or allow entry).
func toolGateEnv(t *testing.T, timeout time.Duration, max int, shape func(*settings.Policy)) *env {
	t.Helper()
	dir := t.TempDir()
	p := settings.DefaultPolicy() // a fresh install's policy (allows every command and tool)
	if shape != nil {
		shape(&p)
	}
	if err := os.WriteFile(filepath.Join(dir, settings.PolicyFile), settings.FormatPolicy(p), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	grants := map[string]run.Grant{
		mcpWriteTok: {Session: "s1", Turn: "t_1", Root: "s1"},
	}
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Settings:        settings.NewFiles(dir),
		ToolHoldTimeout: timeout, ToolHoldPoll: 5 * time.Millisecond, ToolHoldMax: max,
		RunnerTokens: func(tok string) (run.Grant, bool) { g, ok := grants[tok]; return g, ok }})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &env{srv: srv, api: a, store: st, port: port}
}
