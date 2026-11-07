package cost

// Built-in prices, USD per million tokens.
//
// Anthropic: https://platform.claude.com/docs/en/about-claude/pricing,
// fetched 2026-11-05. Rows "Model pricing", "Fast mode pricing", "Batch
// processing" and "Long context pricing". Batch cache prices are not listed
// on the page; they are derived from its rule that cache multipliers "stack
// with other pricing modifiers, including the Batch API discount" (half of
// the standard cache price). Fast cache prices are derived the same way from
// "Prompt caching multipliers apply on top of fast mode pricing" (5m write
// 1.25x, 1h write 2x, read 0.05x of the $8 fast input price on Opus 5.5).
// Data residency (inference_geo "us") is 1.1x on all token classes for
// Claude 4.6 and later models.
//
// OpenAI: https://developers.openai.com/api/docs/pricing (read as
// https://developers.openai.com/api/docs/pricing.md), fetched 2026-11-05.
// Tables "Standard", "Batch", "Flex" and "Fast" (Priority was renamed Fast on
// 2026-07-30). Short context is at most 272K input tokens, long context is
// above. The page's 10% regional processing uplift applies only to models
// released on or after 2026-03-05; the release dates were not checked, so
// Residency is left unset and a residency call is reported incomplete.
//
// Codex models: codex-cli 0.164.0's bundled catalog lists gpt-6.1-sol at
// priority 1 (its default), then gpt-6-astra, gpt-6-sol and gpt-6-luna.
// Its hidden "codex-auto-review" model has no public price and is left out.

// serverToolPrices is USD per use of a tool the vendor runs itself, keyed by
// the count's name in the vendor's usage.
//
// Anthropic: https://platform.claude.com/docs/en/about-claude/pricing,
// fetched 2026-11-05. "Web search tool": "$10 USD per 1,000 searches",
// counted as usage.server_tool_use.web_search_requests. "Web fetch tool": "no
// additional cost" beyond tokens, counted as web_fetch_requests. Code
// execution (code_execution_requests) is billed by container time, or free
// beside web search or fetch; usage reports neither, so it has no entry and
// a call that used it is incomplete.
//
// OpenAI: no entry. Its hosted tools (web_search_call and others) are
// counted from the response's output items; their prices were not checked,
// so a call that used one is incomplete.
var serverToolPrices = map[Vendor]map[string]float64{
	Anthropic: {"web_search_requests": 0.01, "web_fetch_requests": 0},
}

// Default returns the built-in table.
func Default() *Table { return NewTable(builtin) }

// anthropicRates builds a Rates from the five columns of Anthropic's table.
func anthropicRates(input, write5m, write1h, read, output float64) Rates {
	return Rates{Input: input, CacheWrite5m: write5m, CacheWrite1h: write1h, CacheRead: read, Output: output}
}

// openaiRates builds a Rates from the five columns of OpenAI's table.
func openaiRates(input, output, read, write30m, write12h float64) Rates {
	return Rates{Input: input, Output: output, CacheRead: read, CacheWrite30m: write30m, CacheWrite12h: write12h}
}

var builtin = []Price{
	// Anthropic, columns: input, 5m write, 1h write, cache read, output.
	{
		Vendor: Anthropic, Model: "claude-opus-5-5",
		Tiers: map[Tier]Rates{
			Standard: anthropicRates(4, 5, 8, 0.20, 20),
			Batch:    anthropicRates(2, 2.5, 4, 0.10, 10),
			Fast:     anthropicRates(8, 10, 16, 0.40, 40),
		},
		Residency: 1.1,
	},
	{
		Vendor: Anthropic, Model: "claude-sonnet-5-5",
		Tiers: map[Tier]Rates{
			Standard: anthropicRates(2, 2.5, 4, 0.10, 10),
			Batch:    anthropicRates(1, 1.25, 2, 0.05, 5),
		},
		Residency: 1.1,
	},
	{
		// Haiku 5.5 is priced by prompt length: over 100,000 tokens, counting
		// cache reads and writes, the whole request pays the higher prices.
		Vendor: Anthropic, Model: "claude-haiku-5-5",
		Tiers: map[Tier]Rates{
			Standard: anthropicRates(0.10, 0.125, 0.20, 0.01, 0.50),
			Batch:    anthropicRates(0.05, 0.0625, 0.10, 0.005, 0.25),
		},
		LongAbove: 100_000,
		LongTiers: map[Tier]Rates{
			Standard: anthropicRates(0.50, 0.625, 1, 0.05, 2.50),
			Batch:    anthropicRates(0.25, 0.3125, 0.50, 0.025, 1.25),
		},
		Residency: 1.1,
	},
	{
		Vendor: Anthropic, Model: "claude-fable-5-1",
		Tiers: map[Tier]Rates{
			Standard: anthropicRates(10, 12.50, 20, 0.25, 50),
			Batch:    anthropicRates(5, 6.25, 10, 0.125, 25),
		},
		Residency: 1.1,
	},

	// OpenAI, columns: input, output, cached input, 30m write, 12h write.
	{
		Vendor: OpenAI, Model: "gpt-6.1-sol",
		Tiers: map[Tier]Rates{
			Standard: openaiRates(2, 10, 0.10, 2.50, 5),
			Batch:    openaiRates(1, 5, 0.05, 1.25, 2.50),
			Flex:     openaiRates(1, 5, 0.05, 1.25, 2.50),
			Fast:     openaiRates(4, 20, 0.20, 5, 10),
		},
		LongAbove: 272_000,
		LongTiers: map[Tier]Rates{
			Standard: openaiRates(4, 15, 0.20, 5, 10),
			Batch:    openaiRates(2, 7.50, 0.10, 2.50, 5),
			Flex:     openaiRates(2, 7.50, 0.10, 2.50, 5),
			Fast:     openaiRates(8, 30, 0.40, 10, 20),
		},
	},
	{
		Vendor: OpenAI, Model: "gpt-6-astra",
		Tiers: map[Tier]Rates{
			Standard: openaiRates(10, 50, 1, 12.50, 25),
			Batch:    openaiRates(5, 25, 0.50, 6.25, 12.50),
			Flex:     openaiRates(5, 25, 0.50, 6.25, 12.50),
			Fast:     openaiRates(20, 100, 2, 25, 50),
		},
		LongAbove: 272_000,
		LongTiers: map[Tier]Rates{
			Standard: openaiRates(20, 75, 2, 25, 50),
			Batch:    openaiRates(10, 37.50, 1, 12.50, 25),
			Flex:     openaiRates(10, 37.50, 1, 12.50, 25),
			Fast:     openaiRates(40, 150, 4, 50, 100),
		},
	},
	{
		Vendor: OpenAI, Model: "gpt-6-sol",
		Tiers: map[Tier]Rates{
			Standard: openaiRates(2, 10, 0.20, 2.50, 5),
			Batch:    openaiRates(1, 5, 0.10, 1.25, 2.50),
			Flex:     openaiRates(1, 5, 0.10, 1.25, 2.50),
			Fast:     openaiRates(4, 20, 0.40, 5, 10),
		},
		LongAbove: 272_000,
		LongTiers: map[Tier]Rates{
			Standard: openaiRates(4, 15, 0.40, 5, 10),
			Batch:    openaiRates(2, 7.50, 0.20, 2.50, 5),
			Flex:     openaiRates(2, 7.50, 0.20, 2.50, 5),
			Fast:     openaiRates(8, 30, 0.80, 10, 20),
		},
	},
	{
		Vendor: OpenAI, Model: "gpt-6-luna",
		Tiers: map[Tier]Rates{
			Standard: openaiRates(0.10, 0.50, 0.01, 0.125, 0.25),
			Batch:    openaiRates(0.05, 0.25, 0.005, 0.0625, 0.125),
			Flex:     openaiRates(0.05, 0.25, 0.005, 0.0625, 0.125),
			Fast:     openaiRates(0.20, 1, 0.02, 0.25, 0.50),
		},
		LongAbove: 272_000,
		LongTiers: map[Tier]Rates{
			Standard: openaiRates(0.20, 0.75, 0.02, 0.25, 0.50),
			Batch:    openaiRates(0.10, 0.375, 0.01, 0.125, 0.25),
			Flex:     openaiRates(0.10, 0.375, 0.01, 0.125, 0.25),
			Fast:     openaiRates(0.40, 1.50, 0.04, 0.50, 1),
		},
	},
}
