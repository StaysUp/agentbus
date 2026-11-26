package settings

import "strings"

// DefaultWindow is the window used for a model AgentBus does not recognise,
// and for the older Claude models that held 200k.
const DefaultWindow = 200000

// ModelWindow is a model's assumed context window in tokens: a default, never
// a confirmed figure (the context view marks every window assumed until a
// harness reports its own). The numbers are the Claude API model table
// (cached 2026-11-03): every current Claude model holds 1M, and only Haiku 4.5
// and older hold 200k — Claude 3 (claude-3*), Haiku 4.5 (haiku ... 4-5) and the
// 4.0/4.1 generation (-4-0, -4-1). Gemini is 1M; everything else takes the 200k
// fallback. Matched on the id the vendor reports, which carries the family even
// when dated (claude-opus-5-5-...). The context view and own replay's history
// budget both read it.
func ModelWindow(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini"):
		return 1000000
	case isClaudeModel(m):
		if claudeIs200k(m) {
			return 200000
		}
		return 1000000
	default:
		return DefaultWindow
	}
}

// isClaudeModel reports whether m names a Claude model, by family word.
func isClaudeModel(m string) bool {
	return strings.Contains(m, "claude") || strings.Contains(m, "opus") ||
		strings.Contains(m, "sonnet") || strings.Contains(m, "haiku") || strings.Contains(m, "fable")
}

// claudeIs200k reports whether a Claude model m is one of the 200k ones: Haiku
// 4.5 and older. Current Claude models (4.6 generation onward, and every 5.x)
// hold 1M, so they are not matched here.
func claudeIs200k(m string) bool {
	switch {
	case strings.Contains(m, "claude-3"): // the Claude 3 generation
		return true
	case strings.Contains(m, "haiku") && strings.Contains(m, "4-5"): // Haiku 4.5
		return true
	case strings.Contains(m, "-4-0") || strings.Contains(m, "-4-1"): // the 4.0 / 4.1 generation
		return true
	default:
		return false
	}
}
