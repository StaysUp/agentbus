package cost

import (
	"math"
	"testing"
)

const million = 1_000_000

// short stays under OpenAI's 272K long-context threshold. A million input
// tokens would cross it.
const short = 100_000

type priceCase struct {
	name     string
	call     Call
	usd      float64
	complete bool
}

func runCases(t *testing.T, table *Table, cases []priceCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := table.Price(tc.call)
			if math.Abs(got.USD-tc.usd) > 1e-9 {
				t.Errorf("USD = %.10f, want %.10f (missing: %v)", got.USD, tc.usd, got.Missing)
			}
			if got.Complete != tc.complete {
				t.Errorf("Complete = %v, want %v (missing: %v)", got.Complete, tc.complete, got.Missing)
			}
			if got.Complete != (len(got.Missing) == 0) {
				t.Errorf("Complete = %v but Missing = %v", got.Complete, got.Missing)
			}
		})
	}
}

func TestPriceBuiltin(t *testing.T) {
	opus := func(c Call) Call { c.Vendor, c.Model = Anthropic, "claude-opus-5-5"; return c }
	sol := func(c Call) Call { c.Vendor, c.Model = OpenAI, "gpt-6.1-sol"; return c }
	haiku := func(c Call) Call { c.Vendor, c.Model = Anthropic, "claude-haiku-5-5"; return c }

	runCases(t, Default(), []priceCase{
		// Each token class on its own, Opus 5.5 standard.
		{"input", opus(Call{InputTokens: million}), 4, true},
		{"output", opus(Call{OutputTokens: million}), 20, true},
		{"cache read", opus(Call{CacheReadTokens: million}), 0.20, true},
		{"cache write 5m", opus(Call{CacheWrite5mTokens: million}), 5, true},
		{"cache write 1h", opus(Call{CacheWrite1hTokens: million}), 8, true},
		{"zero tokens", opus(Call{}), 0, true},

		// All classes in one call: 1000*2 + 2000*10 + 3000*0.1 + 4000*2.5 + 5000*4.
		{"sonnet all classes", Call{
			Vendor: Anthropic, Model: "claude-sonnet-5-5",
			InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 3000,
			CacheWrite5mTokens: 4000, CacheWrite1hTokens: 5000,
		}, 0.0523, true},
		{"fable all classes", Call{
			Vendor: Anthropic, Model: "claude-fable-5-1",
			InputTokens: million, OutputTokens: million, CacheReadTokens: million,
			CacheWrite5mTokens: million, CacheWrite1hTokens: million,
		}, 10 + 50 + 0.25 + 12.5 + 20, true},

		// speed=fast doubles Opus 5.5, cache multipliers on top.
		{"fast input and output", opus(Call{InputTokens: million, OutputTokens: million, Speed: "fast"}), 48, true},
		{"fast cache classes", opus(Call{CacheReadTokens: million, CacheWrite5mTokens: million, CacheWrite1hTokens: million, Speed: "fast"}), 0.40 + 10 + 16, true},
		{"fast on a model without fast prices", Call{Vendor: Anthropic, Model: "claude-sonnet-5-5", InputTokens: million, Speed: "fast"}, 0, false},
		{"fast with batch", opus(Call{InputTokens: million, Speed: "fast", ServiceTier: "batch"}), 0, false},
		{"unknown speed", opus(Call{InputTokens: million, Speed: "turbo"}), 0, false},

		// Service tiers.
		{"anthropic batch", opus(Call{InputTokens: million, CacheWrite1hTokens: million, ServiceTier: "batch"}), 2 + 4, true},
		{"anthropic standard named", opus(Call{InputTokens: million, ServiceTier: "standard"}), 4, true},
		{"anthropic priority has no public price", opus(Call{InputTokens: million, ServiceTier: "priority"}), 0, false},
		{"openai default", sol(Call{InputTokens: short, ServiceTier: "default"}), 0.2, true},
		{"openai priority is fast", sol(Call{InputTokens: short, ServiceTier: "priority"}), 0.4, true},
		{"openai speed fast", sol(Call{OutputTokens: million, Speed: "fast"}), 20, true},
		{"openai flex", sol(Call{OutputTokens: million, ServiceTier: "flex"}), 5, true},
		{"openai auto is not priced", sol(Call{InputTokens: million, ServiceTier: "auto"}), 0, false},

		// OpenAI cache classes.
		{"openai cache read", sol(Call{CacheReadTokens: short}), 0.01, true},
		{"openai cache write 30m", sol(Call{CacheWrite30mTokens: short}), 0.25, true},
		{"openai cache write 12h", sol(Call{CacheWrite12hTokens: short}), 0.5, true},

		// Dated ids resolve to the base model.
		{"anthropic dated id", Call{Vendor: Anthropic, Model: "claude-sonnet-5-5-20261001", InputTokens: million}, 2, true},
		{"openai dated id", Call{Vendor: OpenAI, Model: "gpt-6.1-sol-2026-10-07", InputTokens: short}, 0.2, true},
		{"dated id of an unknown model", Call{Vendor: Anthropic, Model: "claude-nova-9-20261001", InputTokens: million}, 0, false},

		// Missing prices are reported, never invented.
		{"unknown model", Call{Vendor: Anthropic, Model: "claude-nova-9", InputTokens: million, OutputTokens: million}, 0, false},
		{"known model under the wrong vendor", Call{Vendor: OpenAI, Model: "claude-opus-5-5", InputTokens: million}, 0, false},
		{"unpriced class keeps the priced part", opus(Call{InputTokens: million, CacheWrite30mTokens: 1000}), 4, false},
		{"unpriced class with zero tokens is fine", opus(Call{InputTokens: million, CacheWrite30mTokens: 0}), 4, true},
		{"incomplete output count", opus(Call{OutputTokens: 1000, OutputIncomplete: true}), 0.02, false},
		{"negative count", opus(Call{InputTokens: million, OutputTokens: -5}), 4, false},

		// Long context.
		{"haiku at the threshold", haiku(Call{InputTokens: 100_000}), 100_000 * 0.10 / million, true},
		{"haiku over the threshold", haiku(Call{InputTokens: 100_001}), 100_001 * 0.50 / million, true},
		{"haiku threshold counts cache reads", haiku(Call{InputTokens: 1, CacheReadTokens: 100_000}), (0.50 + 100_000*0.05) / million, true},
		{"haiku long batch", haiku(Call{InputTokens: 200_000, ServiceTier: "batch"}), 200_000 * 0.25 / million, true},
		{"openai over 272K", sol(Call{InputTokens: 272_001}), 272_001 * 4.0 / million, true},
		{"openai long fast", sol(Call{InputTokens: 300_000, OutputTokens: 1000, ServiceTier: "priority"}), (300_000*8 + 1000*30.0) / million, true},

		// Data residency.
		{"anthropic residency", opus(Call{InputTokens: million, DataResidency: true}), 4.4, true},
		{"openai residency not priced", sol(Call{InputTokens: million, DataResidency: true}), 0, false},
	})
}

func TestBuiltinTableIsWellFormed(t *testing.T) {
	want := []string{
		"anthropic/claude-fable-5-1", "anthropic/claude-haiku-5-5", "anthropic/claude-opus-5-5",
		"anthropic/claude-sonnet-5-5", "openai/gpt-6-astra", "openai/gpt-6-luna",
		"openai/gpt-6-sol", "openai/gpt-6.1-sol",
	}
	got := Default().Models()
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
	for _, p := range builtin {
		if p.Tiers[Standard] == nil {
			t.Errorf("%s has no standard tier", p.Model)
		}
		if (p.LongAbove == 0) != (p.LongTiers == nil) {
			t.Errorf("%s: LongAbove and LongTiers must be set together", p.Model)
		}
	}
}

// Changing rates from one table's Lookup must not change another table or a
// later Default().
func TestTablesDoNotShareRates(t *testing.T) {
	a := Default()
	p, ok := a.Lookup(Anthropic, "claude-sonnet-5-5")
	if !ok {
		t.Fatal("sonnet missing")
	}
	p.Tiers[Standard][Input] = 0
	call := Call{Vendor: Anthropic, Model: "claude-sonnet-5-5", InputTokens: million}
	for name, table := range map[string]*Table{"same table": a, "new default": Default()} {
		if r := table.Price(call); r.USD != 2 {
			t.Errorf("%s: input cost = %v after a caller changed a looked-up price, want 2", name, r.USD)
		}
	}
}
