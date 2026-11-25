package api

// B6: the gate for tool calls. Every call an agent makes to an AgentBus tool
// (mcp.go, callTool) is decided before it runs — by the tool's effect and the
// person's rules, the SAME allow/ask/refuse the gateway applies to a shell
// command, through the one decision function (gateway.DecideTool over
// gateway.Rules read from permissions.toml). His call, 2026-11-18: one rules
// list, one decision function, two checkpoints, no second copy.
//
// The gate knows nothing about what a tool does (his decision, 2026-11-18,
// DESIGN_0_6_tools.md #8). It decides from the tool's name, its declared effect
// and the person's rules, plus one permission step (authorize), in this order
// at the one dispatch point every tool shares (callTool): authorize -> decide
// -> hold if Ask -> run. It does NOT decode or validate the tool's inputs; the
// tool validates its own after the gate, so a call the tool later rejects (an
// empty title) may be held first, by design, bounded by the admission cap.
//
// A tool that only reads, with no rule, runs. A tool that does something, with
// no rule, is held for a person. A forbid/ask/allow entry naming the tool
// overrides the default. The gate runs only for an agent's own call (a turn
// token); the authorize step refuses a call the caller may not make (out of its
// tree, [agent_api] off) at once, before any hold.
//
// Ask holds the call here, on control's MCP endpoint, up to 60 s — H1's hold
// carried to a tool call. The agent's tool-call HTTP request is a plain
// request we keep open; control records the call as one approval.requested
// (shown on the Ask card like a command) and reads its OWN record for the
// answer. A person answers over POST /api/v0/approvals/{id} exactly as for a
// command: allow runs the call in the same turn, refuse refuses it, and no
// answer in 60 s refuses it too — a plain refusal, no next turn (his call,
// 2026-11-17). Control emits approval.decided itself on a timeout or a dropped
// connection, so nothing is left pending.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/run"
)

// maxToolHolds bounds how many Ask tool calls the gate holds at once, across
// all turns. A legitimate reply holds a few parallel calls; past this a call is
// refused rather than held, so a runaway agent cannot pin control's resources.
const maxToolHolds = 64

// gateToolCall decides a turn's tool call before it runs. ok is false when the
// call is refused or held-and-refused, and result is what the agent is told;
// ok is true when the call may run (allowed outright, or a person allowed the
// held call). A call that is not an agent's own (no turn token) or names a tool
// the token cannot already call is not gated: ok is true and the route decides.
func (a *API) gateToolCall(r *http.Request, inner *http.Request, c *caller, st servedTool, args map[string]any) (ok bool, result toolResult) {
	if !c.limits.gated() {
		return true, toolResult{} // not held to rules (the owner's own call): the route decides it
	}
	g := &c.Grant
	// authorize (permission, not validation): may this caller use this session at
	// all — the SAME check the route applies to a turn token (out of the token's
	// tree, [agent_api] off), reusing the door (allow) so there is no second
	// copy. Run before the decision, so a call the caller may not make is refused
	// at once and never held. The route runs it again; it reads only, so twice is
	// safe. The gate does NOT validate the tool's inputs — the tool validates its
	// own after the gate (his decision, 2026-11-18, DESIGN_0_6_tools.md #8), so a
	// call the tool later rejects (an empty title) may be held first, by design,
	// bounded by the admission cap.
	if res, authorized := a.authorizeToolCall(inner, st.rt, *g); !authorized {
		return false, res
	}
	// decide, by the rules the turn took at its start (O2.4)
	rules := c.toolRules()
	dec, rule := gateway.DecideTool(rules, st.def.Name)
	if dec == gateway.Refuse {
		a.recordToolRefused(g, st, args, rule)
		return false, toolFail(refusedToolText(rule))
	}
	if isFolderServedTool(st) && a.cfg.Store != nil {
		targetPath, _ := args["path"].(string)
		if targetPath != "" {
			if resolved, err := resolveFilePath(targetPath); err == nil {
				targetPath = resolved
			}
			if resolved, err := resolveSymlinkPath(targetPath); err == nil {
				targetPath = resolved
			}
			granted, err := a.cfg.Store.HasFolderGrant(g.Session, targetPath, g.RulesAt)
			if err == nil && granted {
				return true, toolResult{}
			}
		}
	}
	switch dec {
	case gateway.Ask:
		return a.holdToolCall(r, g, st, args, rules.Ask)
	default:
		return true, toolResult{}
	}
}

// authorizeToolCall runs the route's own door for this call before the gate
// decides it, reusing allow so there is no second copy of the authorization to
// drift. authorized is false when the call is refused, and
// result is the refusal the agent reads — the same text the route would return,
// so a refusal looks the same whether caught here or by the route. allow reads
// only, so the route running it again is safe.
func (a *API) authorizeToolCall(inner *http.Request, rt *route, g run.Grant) (result toolResult, authorized bool) {
	id, vals, matched := rt.match(inner.URL.Path)
	if !matched {
		return toolResult{}, true // no match here: let the route answer
	}
	withValues(inner, vals)
	rec := &toolRecorder{header: http.Header{}}
	if _, ok := a.allow(rec, inner, rt, id, g); !ok {
		return toolFail(toolStatus(rec.code) + ": " + strings.TrimSpace(rec.body.String())), false
	}
	return toolResult{}, true
}

// holdToolCall holds an Ask tool call until a person answers or the hold times
// out, and reports whether the call may then run. It records the call as one
// approval.requested, polls control's own record, and on an allow returns to
// run the call in the same turn; a refuse or a timeout refuses it with a plain
// message and records approval.decided so nothing stays pending.
func (a *API) holdToolCall(r *http.Request, g *run.Grant, st servedTool, args map[string]any, ask []string) (ok bool, result toolResult) {
	// Admission: hold at most maxToolHolds calls at once (api.go). Past the cap a
	// call is refused here, before an approval is recorded, so an agent firing
	// many parallel tool calls cannot hold them all open and starve control.
	select {
	case a.toolHolds <- struct{}{}:
		defer func() { <-a.toolHolds }()
	default:
		return false, toolFail(tooManyHoldsText)
	}

	id := newApprovalID()
	cmd := renderToolCall(st.def.Name, args)
	// What a later "always" moves from ask to allow: EVERY ask entry that matches
	// this tool, by the gate's own matcher — so an entry written as a path, or
	// several equivalent entries, are all cleared, not just one (one left behind
	// keeps matching and the next call asks again). Plus the tool's own name, so
	// "always" adds it to allow even when no ask entry matched (the effect default
	// held the call).
	moveRules := gateway.MatchingRules(ask, st.def.Name)
	if !contains(moveRules, st.def.Name) {
		moveRules = append(moveRules, st.def.Name)
	}
	if isFolderServedTool(st) {
		if targetFolder := folderOfTool(st, args); targetFolder != "" {
			moveRules = []string{targetFolder}
		}
	}
	a.changed(event.Event{
		Kind: event.ApprovalRequested, Session: g.Session, Turn: g.Turn, Approval: id,
		ToolName: st.def.Name, Commands: []string{cmd}, Rule: moveRules[0],
		Rules: moveRules, Text: cmd,
	}, "tool.ask session=%s turn=%s tool=%s approval=%s", g.Session, g.Turn, st.def.Name, id)

	timeout, poll := a.toolHoldTiming()
	deadline := a.cfg.Now().Add(timeout)
	ctx := r.Context()
	for {
		switch a.toolApprovalState(id) {
		case gateway.Allow:
			return true, toolResult{}
		case gateway.Refuse:
			return false, toolFail(holdRefusedToolText)
		}
		if !a.cfg.Now().Before(deadline) {
			return a.endHeldCall(g, id, "no one answered in time", holdTimeoutToolText)
		}
		select {
		case <-ctx.Done():
			return a.endHeldCall(g, id, "the agent's connection closed before an answer", holdClosedToolText)
		case <-time.After(poll):
		}
	}
}

// endHeldCall ends a hold whose window passed or whose connection closed, at
// the firm deadline (his call, 2026-11-18, H1 parity: no answer in the window is
// a plain refusal, no next turn). It needs NO lock against a person's answer,
// because the read is allow-wins (toolApprovalState): a person's allow already
// on the record wins, and a timeout decision recorded alongside it is harmless.
// So it reads the record; if a person allowed, the call runs; otherwise it
// records the ending and reads once more (an allow that landed while it recorded
// still wins); only a record with no allow refuses the call. A person who
// answers AFTER this ending finds the approval already decided and is refused
// (decideApproval returns 404); the late answer is not recorded and the call
// does not run — H1's own rule, not a defect.
func (a *API) endHeldCall(g *run.Grant, id, reason, text string) (allow bool, result toolResult) {
	if a.toolApprovalState(id) == gateway.Allow {
		return true, toolResult{} // a person allowed within the window: run it
	}
	a.recordHoldEnded(g, id, reason)
	if a.toolApprovalState(id) == gateway.Allow {
		return true, toolResult{} // an allow landed as we recorded the ending: it wins
	}
	return false, toolFail(text)
}

// toolApprovalState reads whether the held tool-call approval was answered, and
// how, from control's OWN record — the same truth the command hold polls
// through the runner protocol (jobs.approvalState), read here directly because
// the tool gate runs on control. gateway.Ask means not yet decided, so the hold
// keeps holding; Allow releases the call; Refuse refuses it. A read error is
// "not yet decided", so a transient store error never refuses a call a person
// may still be answering — the deadline ends the hold instead.
//
// ALLOW WINS (his call, 2026-11-18, H1 parity): if an "allowed" decision exists
// for the approval it is the answer, whatever else is on the record. So the hold
// and a person's answer need NO lock coordination: the deadline may record a
// timeout decision while a person's allow is also recorded, and the allow still
// wins — the call runs, never refused by a late timeout (the r5 race). A read is
// idempotent: any number of decisions resolve to one answer, allow first.
func (a *API) toolApprovalState(id string) gateway.Decision {
	evs, err := a.cfg.Store.ApprovalEvents()
	if err != nil {
		return gateway.Ask
	}
	requested, allowed, refused := false, false, false
	for _, e := range evs {
		if e.Approval != id {
			continue
		}
		switch e.Kind {
		case event.ApprovalRequested:
			requested = true
		case event.ApprovalDecided:
			// Every allow is written by the approvals API, whose decisionText
			// begins "allowed"; a timeout or a dropped connection is always a
			// refusal and never does. The one prefix tells them apart — the same
			// convention jobs.approvalAllowed reads.
			if approvalWasAllowed(e.Text) {
				allowed = true
			} else {
				refused = true
			}
		}
	}
	switch {
	case !requested:
		return gateway.Ask
	case allowed:
		return gateway.Allow // allow wins over any timeout/close refusal
	case refused:
		return gateway.Refuse
	}
	return gateway.Ask
}

// recordToolRefused records a forbidden tool call, so the record shows the gate
// refused it and which entry matched.
func (a *API) recordToolRefused(g *run.Grant, st servedTool, args map[string]any, rule string) {
	input, _ := json.Marshal(args)
	a.changed(event.Event{
		Kind: event.ToolRefused, Session: g.Session, Turn: g.Turn,
		ToolName: st.def.Name, ToolInput: input, Rule: rule, Text: refusedToolText(rule),
	}, "tool.refused session=%s turn=%s tool=%s rule=%q", g.Session, g.Turn, st.def.Name, rule)
}

// recordHoldEnded records approval.decided for a held tool call the gate itself
// ended — a timeout or a dropped connection — so the approval stops listing as
// pending though no person answered. text must not begin "allowed", so the
// record reads it as a refusal (toolApprovalState, jobs.approvalState).
func (a *API) recordHoldEnded(g *run.Grant, id, text string) {
	a.changed(event.Event{
		Kind: event.ApprovalDecided, Session: g.Session, Turn: g.Turn, Approval: id, Text: text,
	}, "tool.hold_ended session=%s turn=%s approval=%s why=%q", g.Session, g.Turn, id, text)
}

// toolHoldTiming is how long the gate holds an Ask tool call and how often it
// reads the record, from the config or H1's own defaults.
func (a *API) toolHoldTiming() (timeout, poll time.Duration) {
	timeout, poll = gateway.HoldTimeout, gateway.PollInterval
	if a.cfg.ToolHoldTimeout > 0 {
		timeout = a.cfg.ToolHoldTimeout
	}
	if a.cfg.ToolHoldPoll > 0 {
		poll = a.cfg.ToolHoldPoll
	}
	return timeout, poll
}

// renderToolCall is how a tool call shows on the Ask card and in the record:
// the tool's name and its input as compact JSON, like a command line. Bounded,
// so a large input does not fill a card or an event.
func renderToolCall(name string, args map[string]any) string {
	if len(args) == 0 {
		return name
	}
	b, err := json.Marshal(args)
	if err != nil {
		return name
	}
	const max = 2000
	s := string(b)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return name + " " + s
}

// newApprovalID is a fresh id for a tool-call approval, tied to its request and
// decision in the record. The tool gate runs on control, which reads the answer
// from its own record by this id, so it needs no attempt scoping.
func newApprovalID() string { return "ap-tool-" + randomHex(8) }

// toolFail is a tool result that tells the agent the call did not run.
func toolFail(msg string) toolResult {
	return toolResult{Content: []textContent{{"text", msg}}, IsError: true}
}

// approvalWasAllowed classifies a recorded approval.decided as an allow, the
// one convention shared with jobs.approvalAllowed: control's decisionText
// begins "allowed" for an allow and nothing else does.
func approvalWasAllowed(text string) bool {
	return len(text) >= 7 && text[:7] == "allowed"
}

func isFolderRoute(path string) bool {
	return path == "/api/v0/files/read" || path == "/api/v0/files/list" || path == "/api/v0/files/write"
}

func isFolderToolName(name string) bool {
	return name == "read_file" || name == "list_folder" || name == "write_file"
}

func isFolderServedTool(st servedTool) bool {
	if isFolderRoute(st.def.Run.Path) {
		return true
	}
	if st.rt != nil && isFolderRoute(st.rt.path) {
		return true
	}
	return isFolderToolName(st.def.Name)
}

func (a *API) isFolderTool(name string) bool {
	if isFolderToolName(name) {
		return true
	}
	if st, ok := a.servedByName(name); ok {
		return isFolderServedTool(st)
	}
	return false
}

func isFolderTool(name string) bool {
	return isFolderToolName(name)
}

func folderOfTool(st servedTool, args map[string]any) string {
	p, _ := args["path"].(string)
	if p == "" {
		return ""
	}
	p, err := resolveFilePath(p)
	if err != nil {
		return ""
	}
	if resolved, err := resolveSymlinkPath(p); err == nil {
		p = resolved
	}
	if st.def.Name == "list_folder" || (st.rt != nil && st.rt.path == "/api/v0/files/list") || st.def.Run.Path == "/api/v0/files/list" {
		return p
	}
	return filepath.Dir(p)
}

// Hold and refusal texts: what the agent reads in place of a gated call's
// output. They mirror the command gate's (gateway/hold.go, gateway/guard.go),
// worded for a tool call.
const (
	holdTimeoutToolText = "Held by the AgentBus gateway: no one answered in time; ask again later. The call did not run."
	holdRefusedToolText = "Held by the AgentBus gateway: a person refused this call. It did not run."
	holdClosedToolText  = "Held by the AgentBus gateway: the connection closed before an answer. The call did not run."
	tooManyHoldsText    = "Held by the AgentBus gateway: too many tool calls are waiting for a person; ask again shortly. The call did not run."
)

// refusedToolText is what the agent reads when a forbid entry refuses a tool
// call outright.
func refusedToolText(rule string) string {
	if rule == "" {
		return "Refused by the AgentBus gateway: this tool is not allowed and Settings refuse it. The call did not run."
	}
	return "Refused by the AgentBus gateway: the Forbid rule " + strconv.Quote(rule) + " in Settings matched this tool. The call did not run."
}
