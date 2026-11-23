package gateway

import (
	"path"
	"slices"
	"strings"
)

// B6: the gate's second checkpoint. The command gate (command.go) decides a
// shell command the model wrote; DecideTool decides a tool call the agent made
// to the AgentBus tool server (api/mcp.go). Both are the ONE decision — his
// call, 2026-11-18: one rules list and one decision function, no second copy,
// so a rule gives the same answer at both checkpoints.
//
// A tool name is one word, so DecideTool matches it through the SAME matchAny
// the command gate uses (decideWords): a forbid, ask or allow entry of
// permissions.toml decides a tool call and a shell command alike. His decision,
// 2026-11-18 (DESIGN_0_6_tools.md #9): a tool is decided by NAME alone, against
// the same rules; nothing about the tool is trusted, so there is no effect and
// no per-tool default. A tool on no list is held for a person (Ask). The apps
// that do something, such as ask_agent, carry no rule and so are asked by
// default; reading the record is the API's own routes, not tools (B12).

func matchExactTool(rule, target, bare string) bool {
	f := strings.Fields(rule)
	if len(f) != 1 {
		return false
	}
	r := f[0]
	if target == "tool:*" {
		return r == "tool:*"
	}
	if r == target || r == bare {
		return true
	}
	ruleBase := path.Base(strings.TrimPrefix(r, "tool:"))
	return ruleBase == bare
}

// DecideTool decides one tool call by the person's rules and the tool's name.
// A forbid, ask or allow entry naming the tool wins; then the tool:* catch-alls,
// in the same precedence (forbid over ask over allow): tool:* in forbid refuses
// every other tool, tool:* in ask holds every other tool, tool:* in allow allows
// every other tool. With no rule the call is held for a person (Ask), never
// allowed silently. rule is the ask or forbid entry that drove an Ask or a
// Refuse, "" when no rule matched.
func DecideTool(r Rules, name string) (Decision, string) {
	bare := strings.TrimPrefix(name, "tool:")
	target := "tool:" + bare
	for _, rule := range r.Forbid {
		if matchExactTool(rule, target, bare) {
			return Refuse, rule
		}
	}
	for _, rule := range r.Ask {
		if matchExactTool(rule, target, bare) {
			return Ask, rule
		}
	}
	for _, rule := range r.Allow {
		if matchExactTool(rule, target, bare) {
			return Allow, ""
		}
	}
	if target != "tool:*" {
		if slices.Contains(r.Forbid, "tool:*") {
			return Refuse, "tool:*"
		}
		if slices.Contains(r.Ask, "tool:*") {
			return Ask, "tool:*"
		}
		if slices.Contains(r.Allow, "tool:*") {
			return Allow, ""
		}
	}
	return Ask, ""
}

// MatchingRules are every entry of list the tool name matches, by the SAME
// matcher the gate decides with (matchWords on the one-word name). "Always
// allow this tool" moves all of them from ask to allow, so an ask entry in any
// form — a bare name, a full path, several equivalents — is cleared, not just
// the first; otherwise one left behind keeps matching and the next call asks
// again. The order is list's.
func MatchingRules(list []string, name string) []string {
	var out []string
	bare := strings.TrimPrefix(name, "tool:")
	target := "tool:" + bare
	for _, r := range list {
		if matchExactTool(r, target, bare) || (target != "tool:*" && r == "tool:*") {
			out = append(out, r)
		}
	}
	return out
}
