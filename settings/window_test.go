package settings

import "testing"

// The windows are Anthropic's Context windows page, read 2026-11-28, which
// names every 1M model and says the rest hold 200k: the 4.5 generation holds
// 200k (Opus, Sonnet and Haiku 4.5 alike), the 4.6 generation onward and every
// 5.x hold 1M. The dotted ids are real OpenRouter ones, from its
// /api/v1/models, read the same day; OpenRouter writes every Anthropic model
// with a dot and AgentBus keeps the id as the vendor gives it
// (models.OpenRouter).
func TestModelWindow(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  int
	}{
		// The 4.5 generation, as the Claude API names it: 200k, every family.
		{"claude-opus-4-5", 200000},
		{"claude-sonnet-4-5", 200000},
		{"claude-haiku-4-5", 200000},
		{"claude-haiku-4-5-20251001", 200000},
		// The same models as OpenRouter names them, with a dot.
		{"anthropic/claude-opus-4.5", 200000},
		{"anthropic/claude-sonnet-4.5", 200000},
		{"anthropic/claude-haiku-4.5", 200000},
		{"anthropic/claude-opus-4.5:batch", 200000},
		// Older still: the 4.0/4.1 generation and Claude 3, both spellings.
		{"claude-opus-4-1", 200000},
		{"claude-sonnet-4-0", 200000},
		{"anthropic/claude-opus-4.1", 200000},
		{"claude-3-5-sonnet-20241022", 200000},
		// 4.6 onward, and every 5.x: 1M, both spellings.
		{"claude-opus-4-6", 1000000},
		{"claude-sonnet-4-6", 1000000},
		{"claude-opus-5-5", 1000000},
		{"claude-sonnet-5-5", 1000000},
		{"claude-haiku-5-5", 1000000},
		{"claude-fable-5-2", 1000000},
		{"anthropic/claude-opus-4.6", 1000000},
		{"anthropic/claude-sonnet-4.6", 1000000},
		{"anthropic/claude-opus-5.5", 1000000},
		{"anthropic/claude-fable-5.2", 1000000},
		{"anthropic/claude-haiku-5.5", 1000000},
		{"anthropic/claude-opus-5.5:batch", 1000000},
		// A generation number is read as one: 14-5 is not 4-5.
		{"claude-opus-14-5", 1000000},
		// Gemini is 1M; anything unrecognised takes the fallback.
		{"gemini-3.8-flash-medium", 1000000},
		{"some-other-model", DefaultWindow},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := ModelWindow(tc.model); got != tc.want {
				t.Errorf("ModelWindow(%q) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}
