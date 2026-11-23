package gateway

import "testing"

// A rule gives the SAME answer at both checkpoints (his decision, 2026-11-18:
// one decision function, no second copy of the logic). The command gate
// classifies a name as a one-command line; the tool gate decides it as a tool
// call. For every name that matches a forbid, ask or allow entry, the two agree
// — the rule, not the no-rule default, decides, so the effect default passed to
// DecideTool cannot change the answer.
func TestToolAndCommandAgreeOnARule(t *testing.T) {
	rules := Rules{
		Forbid:   []string{"apply_turn"},
		Ask:      []string{"review_turn"},
		Allow:    []string{"list_turns"},
		Unlisted: Ask,
	}
	for _, name := range []string{"apply_turn", "review_turn", "list_turns"} {
		cmdDec, _ := classify(rules, name)
		toolDec, _ := DecideTool(rules, name)
		if toolDec != cmdDec {
			t.Errorf("%s: command gate %v, tool gate %v; a rule must decide both the same", name, cmdDec, toolDec)
		}
	}
}

// With no rule for the tool, the call is held (Ask); nothing about the tool
// grants it. A named rule overrides: a forbid refuses it, an allow runs it.
func TestDecideToolDefaultsAndOverrides(t *testing.T) {
	none := Rules{}
	if d, _ := DecideTool(none, "get_turn"); d != Ask {
		t.Errorf("a tool with no rule: %v, want Ask", d)
	}
	if d, _ := DecideTool(none, "ask_agent"); d != Ask {
		t.Errorf("a tool with no rule: %v, want Ask", d)
	}
	// AgentBus ships its read-only built-ins as allow rules: an allowed tool runs.
	allow := Rules{Allow: []string{"get_turn"}}
	if d, _ := DecideTool(allow, "get_turn"); d != Allow {
		t.Errorf("an allowed tool: %v, want Allow", d)
	}
	// A forbid entry refuses it.
	forbid := Rules{Forbid: []string{"ask_agent"}}
	if d, rule := DecideTool(forbid, "ask_agent"); d != Refuse || rule != "ask_agent" {
		t.Errorf("a forbidden tool: %v %q, want Refuse ask_agent", d, rule)
	}
}

// A command rule of several words never matches a one-word tool name, and a
// tool rule never swallows a different tool (names are compared whole).
func TestDecideToolDoesNotCrossMatch(t *testing.T) {
	rules := Rules{Forbid: []string{"git push"}, Ask: []string{"apply_turn"}, Allow: []string{"list_turns"}}
	if d, _ := DecideTool(rules, "apply_turn"); d != Ask {
		t.Errorf("apply_turn: %v, want Ask", d)
	}
	// "git push" (two words) cannot match the one-word name "git": no rule, so Ask.
	if d, _ := DecideTool(rules, "git"); d != Ask {
		t.Errorf("git (no rule): %v, want Ask", d)
	}
	// An allowed tool runs.
	if d, _ := DecideTool(rules, "list_turns"); d != Allow {
		t.Errorf("list_turns: %v, want Allow", d)
	}
}

// B16: Rules in AgentBus's own names (tool:<name>, tool:*).
// tool:* in allow allows every tool (0.6.5); tool:* in forbid forbids all
// tools, and a specific allow still wins over it. tool:subagent is forbidden.
// tool:* in allow never allows a shell command.
func TestToolRules(t *testing.T) {
	rules := Rules{
		Allow:    []string{"tool:*", "tool:edit", "ls"},
		Forbid:   []string{"tool:subagent"},
		Unlisted: Ask,
	}
	// tool:* is allowed
	if d, _ := DecideTool(rules, "tool:*"); d != Allow {
		t.Errorf("tool:*: %v, want Allow", d)
	}
	// every other tool is allowed under tool:*
	if d, _ := DecideTool(rules, "ask_agent"); d != Allow {
		t.Errorf("ask_agent under tool:*: %v, want Allow", d)
	}
	// tool:edit is allowed
	if d, _ := DecideTool(rules, "edit"); d != Allow {
		t.Errorf("edit under tool:edit: %v, want Allow", d)
	}
	if d, _ := DecideTool(rules, "tool:edit"); d != Allow {
		t.Errorf("tool:edit: %v, want Allow", d)
	}
	// tool:subagent is forbidden
	if d, rule := DecideTool(rules, "subagent"); d != Refuse || rule != "tool:subagent" {
		t.Errorf("subagent under tool:subagent: %v %q, want Refuse tool:subagent", d, rule)
	}
	if d, rule := DecideTool(rules, "tool:subagent"); d != Refuse || rule != "tool:subagent" {
		t.Errorf("tool:subagent: %v %q, want Refuse tool:subagent", d, rule)
	}
	// The tool:* catch-alls keep forbid > ask > allow: ask tool:* holds a tool
	// that allow tool:* would otherwise run, and forbid tool:* refuses it.
	askAll := Rules{Allow: []string{"tool:*"}, Ask: []string{"tool:*"}}
	if d, rule := DecideTool(askAll, "ask_agent"); d != Ask || rule != "tool:*" {
		t.Errorf("ask_agent under ask tool:* and allow tool:*: %v %q, want Ask tool:*", d, rule)
	}
	forbidOverAsk := Rules{Allow: []string{"tool:*"}, Ask: []string{"tool:*"}, Forbid: []string{"tool:*"}}
	if d, rule := DecideTool(forbidOverAsk, "ask_agent"); d != Refuse || rule != "tool:*" {
		t.Errorf("ask_agent under forbid+ask+allow tool:*: %v %q, want Refuse tool:*", d, rule)
	}
	// A specific allow still wins over ask tool:*.
	askAllButOne := Rules{Allow: []string{"tool:*", "tool:ask_agent"}, Ask: []string{"tool:*"}}
	if d, _ := DecideTool(askAllButOne, "ask_agent"); d != Allow {
		t.Errorf("ask_agent with a specific allow under ask tool:*: %v, want Allow", d)
	}
	// forbid tool:* forbids unlisted tools, but explicit tool:edit in allow stays allowed
	forbidAll := Rules{
		Allow:    []string{"tool:edit"},
		Forbid:   []string{"tool:*"},
		Unlisted: Ask,
	}
	if d, rule := DecideTool(forbidAll, "ask_agent"); d != Refuse || rule != "tool:*" {
		t.Errorf("ask_agent under forbid tool:*: %v %q, want Refuse tool:*", d, rule)
	}
	if d, _ := DecideTool(forbidAll, "tool:edit"); d != Allow {
		t.Errorf("tool:edit under forbid tool:* but explicit allow: %v, want Allow", d)
	}
	// tool:* in allow NEVER allows a shell command
	if d, _ := classify(rules, "rm -rf /"); d != Ask {
		t.Errorf("rm -rf / under tool:*: %v, want Ask (unlisted)", d)
	}
	// tool:edit in allow NEVER allows a shell command named tool:edit
	if d, _ := classify(rules, "tool:edit"); d != Ask {
		t.Errorf("shell command tool:edit under tool:edit rule: %v, want Ask", d)
	}
	if d, _ := classify(rules, "./tool:edit"); d != Ask {
		t.Errorf("shell command ./tool:edit under tool:edit rule: %v, want Ask", d)
	}
}
