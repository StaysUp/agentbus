package cost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLiteLLM(t *testing.T) {
	table, err := LoadLiteLLM(filepath.Join("testdata", "litellm_fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	claude := func(c Call) Call { c.Vendor, c.Model = Anthropic, "claude-test-1"; return c }
	gpt := func(c Call) Call { c.Vendor, c.Model = OpenAI, "gpt-test"; return c }

	runCases(t, table, []priceCase{
		{"input", claude(Call{InputTokens: million}), 3, true},
		{"output", claude(Call{OutputTokens: million}), 15, true},
		{"cache read", claude(Call{CacheReadTokens: million}), 0.3, true},
		{"anthropic base cache write is 5m", claude(Call{CacheWrite5mTokens: million}), 3.75, true},
		{"anthropic above_1hr is 1h", claude(Call{CacheWrite1hTokens: million}), 6, true},
		{"anthropic has no 30m write", claude(Call{CacheWrite30mTokens: million}), 0, false},
		{"fast comes from the multiplier, not _priority", claude(Call{InputTokens: million, Speed: "fast"}), 6, true},
		{"batch input priced, batch output not", claude(Call{InputTokens: million, OutputTokens: million, ServiceTier: "batch"}), 1.5, false},
		{"residency multiplier", claude(Call{InputTokens: million, DataResidency: true}), 3.3, true},
		{"dated id", Call{Vendor: Anthropic, Model: "claude-test-1-20261001", InputTokens: million}, 3, true},

		{"openai base cache write is 30m", gpt(Call{CacheWrite30mTokens: short}), 0.125, true},
		{"openai above_12hr is 12h", gpt(Call{CacheWrite12hTokens: short}), 0.25, true},
		{"openai _priority is fast", gpt(Call{InputTokens: short, ServiceTier: "priority"}), 0.2, true},
		{"full key also works", Call{Vendor: OpenAI, Model: "openai/gpt-test", InputTokens: short}, 0.1, true},
		{"long context", gpt(Call{InputTokens: 200_001, OutputTokens: 1000}), (200_001*2 + 1000*8.0) / million, true},
		{"long context class without a price", gpt(Call{InputTokens: 200_001, CacheReadTokens: 1}), 200_001 * 2.0 / million, false},
		{"openai has no residency multiplier", gpt(Call{InputTokens: million, DataResidency: true}), 0, false},

		{"two thresholds are skipped", Call{Vendor: OpenAI, Model: "two-thresholds", InputTokens: million}, 0, false},
		{"entry without provider is skipped", Call{Vendor: OpenAI, Model: "no-provider", InputTokens: million}, 0, false},
		{"sample_spec is skipped", Call{Vendor: "one of https://docs.litellm.ai/docs/providers", Model: "sample_spec"}, 0, false},
	})
}

func TestLoadLiteLLMErrors(t *testing.T) {
	if _, err := LoadLiteLLM(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("missing file: want an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLiteLLM(bad); err == nil {
		t.Error("bad JSON: want an error")
	}
}

// A null price is unknown, not free.
func TestLoadLiteLLMNullPriceIsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	body := `{"claude-null":{"litellm_provider":"anthropic","input_cost_per_token":0.000003,"output_cost_per_token":null}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := LoadLiteLLM(path)
	if err != nil {
		t.Fatal(err)
	}
	r := table.Price(Call{Vendor: Anthropic, Model: "claude-null", OutputTokens: million})
	if r.Complete || r.USD != 0 {
		t.Errorf("null output price = %+v, want incomplete and no cost", r)
	}
}

// "azure/gpt-x" stays reachable as azure's "gpt-x" when the file also has a
// bare "gpt-x" for another vendor.
func TestLoadLiteLLMAliasPerVendor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	body := `{"gpt-x":{"litellm_provider":"openai","input_cost_per_token":0.000002},
	"azure/gpt-x":{"litellm_provider":"azure","input_cost_per_token":0.000004}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := LoadLiteLLM(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := table.Price(Call{Vendor: "azure", Model: "gpt-x", InputTokens: million}); !r.Complete || r.USD != 4 {
		t.Errorf("azure gpt-x = %+v, want 4 complete", r)
	}
	if r := table.Price(Call{Vendor: OpenAI, Model: "gpt-x", InputTokens: million}); !r.Complete || r.USD != 2 {
		t.Errorf("openai gpt-x = %+v, want 2 complete", r)
	}
}
