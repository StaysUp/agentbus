package cost

import (
	"slices"
	"testing"
)

func TestDiff(t *testing.T) {
	built := NewTable([]Price{
		{
			Vendor: Anthropic, Model: "claude-a",
			Tiers: map[Tier]Rates{
				Standard: {Input: 3, Output: 15, CacheRead: 0.3},
				Batch:    {Input: 1.5, Output: 7.5},
			},
			Residency: 1.1,
		},
		{
			Vendor: OpenAI, Model: "gpt-a",
			Tiers:     map[Tier]Rates{Standard: {Input: 1, Output: 4}},
			LongAbove: 200_000,
			LongTiers: map[Tier]Rates{Standard: {Input: 2, Output: 8}},
		},
		{Vendor: OpenAI, Model: "gpt-gone", Tiers: map[Tier]Rates{Standard: {Input: 1}}},
	})
	other := NewTable([]Price{
		{
			Vendor: Anthropic, Model: "claude-a",
			Tiers: map[Tier]Rates{
				// Input matches once scaled from per token; output differs;
				// cache_read is absent here, so it is unknown, not different.
				Standard: {Input: 3e-06 * 1e6, Output: 18},
				// No batch tier here: not compared.
			},
			Residency: 1.2,
		},
		{
			Vendor: OpenAI, Model: "gpt-a",
			Tiers:     map[Tier]Rates{Standard: {Input: 1, Output: 4}},
			LongAbove: 272_000,
			LongTiers: map[Tier]Rates{Standard: {Input: 2, Output: 9}},
		},
		{Vendor: OpenAI, Model: "only-in-other", Tiers: map[Tier]Rates{Standard: {Input: 5}}},
	})

	drift, missing := built.Diff(other)
	want := []Drift{
		{Model: "anthropic/claude-a", Rate: "standard output", Have: 15, Other: 18},
		{Model: "anthropic/claude-a", Rate: "residency", Have: 1.1, Other: 1.2},
		{Model: "openai/gpt-a", Rate: "long-context threshold", Have: 200_000, Other: 272_000},
		{Model: "openai/gpt-a", Rate: "long standard output", Have: 8, Other: 9},
	}
	if !slices.Equal(drift, want) {
		t.Errorf("drift:\n got %v\nwant %v", drift, want)
	}
	if !slices.Equal(missing, []string{"openai/gpt-gone"}) {
		t.Errorf("missing = %v, want [openai/gpt-gone]", missing)
	}
}

func TestDiffSameTable(t *testing.T) {
	drift, missing := Default().Diff(Default())
	if len(drift) != 0 || len(missing) != 0 {
		t.Errorf("Default against itself: drift %v, missing %v; want none", drift, missing)
	}
}

func TestDiffLoadedFile(t *testing.T) {
	// The fixture's claude-test-1 at the built-in's own model id: the loader's
	// per-token prices scale back to the same per-million rates.
	file, err := LoadLiteLLM("testdata/litellm_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := file.Lookup(Anthropic, "claude-test-1")
	if !ok {
		t.Fatal("claude-test-1 not loaded")
	}
	built := NewTable([]Price{p})
	if drift, missing := built.Diff(file); len(drift) != 0 || len(missing) != 0 {
		t.Errorf("drift %v, missing %v; want none", drift, missing)
	}
	inFile := p.Tiers[Standard][Output] // 15, give or take the scaling
	p.Tiers[Standard][Output] = 16
	built = NewTable([]Price{p})
	drift, _ := built.Diff(file)
	want := []Drift{{Model: "anthropic/claude-test-1", Rate: "standard output", Have: 16, Other: inFile}}
	if !slices.Equal(drift, want) {
		t.Errorf("drift:\n got %v\nwant %v", drift, want)
	}
}
