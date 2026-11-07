package cost

import (
	"math"
	"testing"

	"github.com/staysup/agentbus/event"
)

func priced(t *testing.T, e event.Event) event.Event {
	t.Helper()
	var m event.Memory
	Sink{Table: Default(), Next: &m}.Emit(e)
	evs := m.Events()
	if len(evs) != 1 {
		t.Fatalf("forwarded %d events, want 1", len(evs))
	}
	return evs[0]
}

func wantCost(t *testing.T, e event.Event, usd float64, complete bool) {
	t.Helper()
	if e.CostUSD == nil || math.Abs(*e.CostUSD-usd) > 1e-9 {
		t.Errorf("cost_usd = %v, want %v", ptrString(e.CostUSD), usd)
	}
	if e.CostComplete == nil || *e.CostComplete != complete {
		t.Errorf("cost_complete = %v, want %v", e.CostComplete, complete)
	}
}

func ptrString(f *float64) any {
	if f == nil {
		return "nil"
	}
	return *f
}

func responded(vendor, model string, u *event.Usage) event.Event {
	return event.Event{Kind: event.ModelResponded, Vendor: vendor, Model: model, Usage: u}
}

// Claude Code 2.1.318 writes its cache with a one-hour lifetime (seen
// 2026-11-05). The write is priced at the 1h rate, $8/M on Opus 5.5, not the
// 5m rate of $5/M.
func TestSinkPricesOneHourWrites(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5-20261001", &event.Usage{
		InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 100_000,
		CacheWriteTokens: 50_000, CacheWrite1hTokens: 50_000, ServiceTier: "standard",
	}))
	// 1000*4 + 2000*20 + 100000*0.20 + 50000*8, per million.
	wantCost(t, e, 0.004+0.04+0.02+0.4, true)
}

func TestSinkPricesMixedLifetimes(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{
		CacheWriteTokens: 3_000_000, CacheWrite5mTokens: 1_000_000, CacheWrite1hTokens: 2_000_000, ServiceTier: "standard",
	}))
	wantCost(t, e, 5+16, true)
}

// A write whose lifetime the vendor did not state has no price. The known
// parts are summed and the cost is incomplete.
func TestSinkUnknownLifetimeIsIncomplete(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{
		InputTokens: 1_000_000, CacheWriteTokens: 1_000_000, ServiceTier: "standard",
	}))
	wantCost(t, e, 4, false)

	e = priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{
		CacheWriteTokens: 3_000_000, CacheWrite1hTokens: 1_000_000, ServiceTier: "standard",
	}))
	wantCost(t, e, 8, false)
}

func TestSinkUsageNilIsUnknown(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", nil))
	if e.CostUSD != nil {
		t.Errorf("cost_usd = %v, want nil when usage is absent", *e.CostUSD)
	}
	if e.CostComplete == nil || *e.CostComplete {
		t.Errorf("cost_complete = %v, want false", e.CostComplete)
	}
}

func TestSinkUnknownModelIsIncomplete(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-x", &event.Usage{InputTokens: 10, OutputTokens: 1}))
	wantCost(t, e, 0, false)
}

func TestSinkOutputIncomplete(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{OutputTokens: 1_000_000, OutputIncomplete: true}))
	wantCost(t, e, 20, false)
}

// OpenAI's adapter has already taken cached tokens out of input. Its cache
// writes come with no lifetime (30m or 12h), so they are not priced.
func TestSinkOpenAI(t *testing.T) {
	e := priced(t, responded("openai", "gpt-6.1-sol", &event.Usage{InputTokens: 100_000, OutputTokens: 10_000, CacheReadTokens: 100_000, ServiceTier: "default"}))
	wantCost(t, e, 0.2+0.1+0.01, true)

	e = priced(t, responded("openai", "gpt-6.1-sol", &event.Usage{InputTokens: 100_000, CacheWriteTokens: 1000, ServiceTier: "default"}))
	wantCost(t, e, 0.2, false)
}

func TestSinkLeavesOtherKindsAlone(t *testing.T) {
	for _, k := range []event.Kind{event.ModelRequested, event.TurnStarted, event.ToolRequested, event.EventsDropped} {
		e := priced(t, event.Event{Kind: k, Vendor: "anthropic", Model: "claude-opus-5-5", Usage: &event.Usage{InputTokens: 1}})
		if e.CostUSD != nil || e.CostComplete != nil {
			t.Errorf("%s got a cost", k)
		}
	}
}

// A fast-mode reply is priced at the fast rate: $8/M input and $40/M output
// on Opus 5.5, twice the standard rate.
func TestSinkAnthropicFast(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{
		InputTokens: 1_000_000, OutputTokens: 1_000_000, ServiceTier: "standard", Speed: "fast",
	}))
	wantCost(t, e, 48, true)
}

// codex's fast tier comes back as service_tier "priority", priced at OpenAI's
// Fast table: $20/M output on gpt-6.1-sol.
func TestSinkOpenAIPriority(t *testing.T) {
	e := priced(t, responded("openai", "gpt-6.1-sol", &event.Usage{OutputTokens: 1_000_000, ServiceTier: "priority"}))
	wantCost(t, e, 20, true)
}

// No reported tier is priced at standard, as a lower bound, and marked
// incomplete: the tier could have been a dearer one. No reported speed is
// standard speed.
func TestSinkAbsentTierIsIncomplete(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{InputTokens: 1_000_000}))
	wantCost(t, e, 4, false)
	e = priced(t, responded("openai", "gpt-6.1-sol", &event.Usage{OutputTokens: 1_000_000}))
	wantCost(t, e, 10, false)
	e = priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{InputTokens: 1_000_000, ServiceTier: "standard"}))
	wantCost(t, e, 4, true)
}

// A tier with no public price is incomplete, never priced as standard.
func TestSinkUnpricedTier(t *testing.T) {
	e := priced(t, responded("anthropic", "claude-opus-5-5", &event.Usage{InputTokens: 1_000_000, ServiceTier: "priority"}))
	wantCost(t, e, 0, false)
	e = priced(t, responded("openai", "gpt-6.1-sol", &event.Usage{OutputTokens: 1_000_000, ServiceTier: "auto"}))
	wantCost(t, e, 0, false)
}

// $0.044 of tokens on Opus 5.5 plus one web search at
// $10 per 1,000 searches is $0.054. Web fetch has no charge beyond tokens.
func TestSinkServerTools(t *testing.T) {
	tokens := func(tools map[string]int) *event.Usage {
		return &event.Usage{InputTokens: 1000, OutputTokens: 2000, ServiceTier: "standard", ServerToolUse: tools}
	}
	for _, c := range []struct {
		name     string
		vendor   string
		model    string
		tools    map[string]int
		usd      float64
		complete bool
	}{
		{"one web search", "anthropic", "claude-opus-5-5", map[string]int{"web_search_requests": 1}, 0.054, true},
		{"web fetch is free", "anthropic", "claude-opus-5-5", map[string]int{"web_fetch_requests": 2}, 0.044, true},
		{"a zero count", "anthropic", "claude-opus-5-5", map[string]int{"code_execution_requests": 0}, 0.044, true},
		// Code execution is billed by container time, which usage does not report.
		{"code execution has no per-use price", "anthropic", "claude-opus-5-5", map[string]int{"code_execution_requests": 1}, 0.044, false},
		{"a search on an unknown model is still summed", "anthropic", "claude-x", map[string]int{"web_search_requests": 3}, 0.03, false},
		{"openai web search has no price here", "openai", "gpt-6.1-sol", map[string]int{"web_search_call": 1}, 0.002 + 0.02, false},
	} {
		u := tokens(c.tools)
		if c.vendor == "openai" {
			u.ServiceTier = "default"
		}
		e := priced(t, responded(c.vendor, c.model, u))
		t.Run(c.name, func(t *testing.T) { wantCost(t, e, c.usd, c.complete) })
	}
}

// inference_geo "us" multiplies every token price by 1.1, not the per-search
// fee. "global" is the default and standard price. An unknown place is
// incomplete.
func TestSinkInferenceGeo(t *testing.T) {
	geo := func(g string, tools map[string]int) *event.Usage {
		return &event.Usage{InputTokens: 1000, OutputTokens: 2000, ServiceTier: "standard", InferenceGeo: g, ServerToolUse: tools}
	}
	wantCost(t, priced(t, responded("anthropic", "claude-opus-5-5", geo("us", nil))), 0.0484, true)
	wantCost(t, priced(t, responded("anthropic", "claude-opus-5-5", geo("global", nil))), 0.044, true)
	wantCost(t, priced(t, responded("anthropic", "claude-opus-5-5", geo("", nil))), 0.044, true)
	wantCost(t, priced(t, responded("anthropic", "claude-opus-5-5", geo("us", map[string]int{"web_search_requests": 1}))), 0.0584, true)
	wantCost(t, priced(t, responded("anthropic", "claude-opus-5-5", geo("eu", nil))), 0.044, false)
}
