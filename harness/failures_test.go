package harness

import "testing"

// A harness's known failure shows its plain words. Any other line, and the
// same line from a harness that does not list it, passes unchanged.
func TestPlain(t *testing.T) {
	const line = "Eligibility check failed: failed to fetch user info: 401 Unauthorized"
	agy, _ := Get("agy")
	claude, _ := Get("claude")
	if got, want := agy.Info().Plain(line), "Antigravity's Google sign-in can't run sessions yet. Add a Gemini API key in Settings, Subscriptions."; got != want {
		t.Errorf("agy's known failure reads %q, want %q", got, want)
	}
	for _, c := range []struct{ name, line, got string }{
		{"another agy error", "error: invalid model selection", agy.Info().Plain("error: invalid model selection")},
		// Without loadCodeAssist agy stops with the same lead and another cause.
		{"agy's other eligibility failure", "Eligibility check failed", agy.Info().Plain("Eligibility check failed")},
		{"the line from claude", line, claude.Info().Plain(line)},
		{"no line", "", agy.Info().Plain("")},
	} {
		if c.got != c.line {
			t.Errorf("%s reads %q, want it unchanged", c.name, c.got)
		}
	}
}
