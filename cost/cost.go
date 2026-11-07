// Package cost prices one model call in USD.
//
// It never guesses. When a price is missing, the call is reported as
// incomplete and only the parts that do have a price are summed.
package cost

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Vendor names the API that served the call.
type Vendor string

// Vendors with a built-in price table.
const (
	Anthropic Vendor = "anthropic"
	OpenAI    Vendor = "openai"
)

// Class is one kind of token that has its own price.
type Class string

// Token classes. Cache writes are split by lifetime because each lifetime has
// its own price: Anthropic sells 5-minute and 1-hour writes, OpenAI sells
// 30-minute and 12-hour writes.
const (
	Input         Class = "input"
	Output        Class = "output"
	CacheRead     Class = "cache_read"
	CacheWrite5m  Class = "cache_write_5m"
	CacheWrite1h  Class = "cache_write_1h"
	CacheWrite30m Class = "cache_write_30m"
	CacheWrite12h Class = "cache_write_12h"
)

// classes is the fixed order used when summing and when listing what is missing.
var classes = []Class{Input, Output, CacheRead, CacheWrite5m, CacheWrite1h, CacheWrite30m, CacheWrite12h}

// Tier is a pricing tier after the vendor's own words are normalised.
type Tier string

// Tiers. Fast covers Anthropic's speed=fast and OpenAI's Fast tier, which
// OpenAI called Priority until 2026-07-30.
const (
	Standard Tier = "standard"
	Batch    Tier = "batch"
	Flex     Tier = "flex"
	Fast     Tier = "fast"
)

// Rates holds USD per million tokens for each class. A class that is absent
// has no known price. A present zero means the class is free.
type Rates map[Class]float64

// Price is everything needed to price calls to one model.
type Price struct {
	Vendor Vendor
	Model  string

	// Tiers holds the rates for each tier the vendor publishes.
	Tiers map[Tier]Rates

	// LongAbove is the prompt length, in tokens, above which LongTiers applies
	// instead of Tiers. Zero means the model has one price at every length.
	// The prompt length counts input, cache-read and cache-write tokens.
	LongAbove int64
	LongTiers map[Tier]Rates

	// Residency multiplies every rate when the call asked for data residency
	// (Anthropic's inference_geo "us"). Zero means no published multiplier.
	Residency float64
}

// Call is the usage of one model call.
type Call struct {
	Vendor Vendor
	// Model is the id as the vendor reports it. A dated id such as
	// "claude-sonnet-5-5-20261001" resolves to its base model when the dated
	// id has no entry of its own.
	Model string

	// InputTokens counts input tokens that were neither read from nor written
	// to a cache. Anthropic reports it that way. OpenAI's input_tokens
	// includes cached tokens, so the caller subtracts them first.
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheWrite5mTokens  int64
	CacheWrite1hTokens  int64
	CacheWrite30mTokens int64
	CacheWrite12hTokens int64

	// Speed is the vendor's speed setting: "" or "standard", or "fast".
	Speed string
	// ServiceTier is the tier the vendor reported, in its own words: for
	// Anthropic "standard", "batch" or "priority"; for OpenAI "default",
	// "flex", "batch" or "priority". Empty means standard.
	ServiceTier string
	// DataResidency is true when the call asked for in-region inference.
	DataResidency bool

	// OutputIncomplete is true when OutputTokens is a lower bound, for
	// example when a stream ended before its final usage report.
	OutputIncomplete bool

	// ServerToolUse counts the tools the vendor ran itself, keyed by the
	// vendor's own name, such as Anthropic's "web_search_requests". A tool
	// with a per-use price is charged per use, outside the token rates: no
	// tier or residency multiplier applies to it.
	ServerToolUse map[string]int64
}

// Result is the price of one call.
type Result struct {
	// USD sums every token class that has a price. When Complete is false it
	// is a lower bound, or zero when the model has no price at all.
	USD float64
	// Complete is true only when every token in the call had a price and the
	// output count was exact.
	Complete bool
	// Missing says why Complete is false, one reason per entry.
	Missing []string
}

// Table is a set of model prices.
type Table struct {
	prices map[string]Price
}

// NewTable builds a table from a list of prices. A later entry for the same
// vendor and model replaces an earlier one.
//
// The table keeps its own copy of every price, and Lookup returns a copy, so
// no caller can change the rates another table or a later call sees.
func NewTable(prices []Price) *Table {
	t := &Table{prices: make(map[string]Price, len(prices))}
	for _, p := range prices {
		t.prices[key(p.Vendor, p.Model)] = p.clone()
	}
	return t
}

// clone copies a Price with its nested maps.
func (p Price) clone() Price {
	p.Tiers = cloneTiers(p.Tiers)
	p.LongTiers = cloneTiers(p.LongTiers)
	return p
}

func cloneTiers(in map[Tier]Rates) map[Tier]Rates {
	if in == nil {
		return nil
	}
	out := make(map[Tier]Rates, len(in))
	for tier, rates := range in {
		r := make(Rates, len(rates))
		for class, rate := range rates {
			r[class] = rate
		}
		out[tier] = r
	}
	return out
}

// Lookup returns the price for a vendor's model id, resolving a dated id to
// its base model.
func (t *Table) Lookup(v Vendor, model string) (Price, bool) {
	if p, ok := t.prices[key(v, model)]; ok {
		return p.clone(), true
	}
	if base := datedSuffix.ReplaceAllString(model, ""); base != model {
		p, ok := t.prices[key(v, base)]
		return p.clone(), ok
	}
	return Price{}, false
}

// Models lists the vendor and model of every entry, sorted.
func (t *Table) Models() []string {
	out := make([]string, 0, len(t.prices))
	for k := range t.prices {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// datedSuffix matches a snapshot date at the end of a model id: Anthropic
// writes "-20261001", OpenAI writes "-2026-10-01".
var datedSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

func key(v Vendor, model string) string { return string(v) + "/" + model }

// Price prices one call.
func (t *Table) Price(c Call) Result {
	r := Result{Complete: true}
	miss := func(format string, a ...any) {
		r.Complete = false
		r.Missing = append(r.Missing, fmt.Sprintf(format, a...))
	}

	if c.OutputIncomplete {
		miss("output token count is a lower bound")
	}
	tokens := c.tokens()
	for _, cl := range classes {
		if tokens[cl] < 0 {
			miss("negative %s token count", cl)
			tokens[cl] = 0
		}
	}

	// Server tools first: their price does not depend on the model, so it is
	// summed even when the model has no price.
	names := make([]string, 0, len(c.ServerToolUse))
	for name := range c.ServerToolUse {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n := c.ServerToolUse[name]
		if n == 0 {
			continue
		}
		if n < 0 {
			miss("negative %s count", name)
			continue
		}
		rate, ok := serverToolPrices[c.Vendor][name]
		if !ok {
			miss("no per-use price for server tool %s on %s", name, c.Vendor)
			continue
		}
		r.USD += float64(n) * rate
	}

	p, ok := t.Lookup(c.Vendor, c.Model)
	if !ok {
		miss("no price for model %s/%s", c.Vendor, c.Model)
		return r
	}

	tier, err := normaliseTier(c.Vendor, c.Speed, c.ServiceTier)
	if err != "" {
		miss("%s", err)
		return r
	}

	tiers := p.Tiers
	if p.LongAbove > 0 && c.promptLength() > p.LongAbove {
		tiers = p.LongTiers
	}
	rates, ok := tiers[tier]
	if !ok {
		miss("no price for tier %s on %s", tier, p.Model)
		return r
	}

	mult := 1.0
	if c.DataResidency {
		if p.Residency == 0 {
			miss("no data residency multiplier for %s", p.Model)
			return r
		}
		mult = p.Residency
	}

	for _, cl := range classes {
		n := tokens[cl]
		if n == 0 {
			continue
		}
		rate, ok := rates[cl]
		if !ok {
			miss("no price for %s tokens on %s", cl, p.Model)
			continue
		}
		r.USD += float64(n) * rate * mult / 1e6
	}
	return r
}

func (c Call) tokens() map[Class]int64 {
	return map[Class]int64{
		Input:         c.InputTokens,
		Output:        c.OutputTokens,
		CacheRead:     c.CacheReadTokens,
		CacheWrite5m:  c.CacheWrite5mTokens,
		CacheWrite1h:  c.CacheWrite1hTokens,
		CacheWrite30m: c.CacheWrite30mTokens,
		CacheWrite12h: c.CacheWrite12hTokens,
	}
}

// promptLength counts every input token, cached or not. Anthropic states this
// rule for Haiku 5.5's 100,000-token threshold.
func (c Call) promptLength() int64 {
	return c.InputTokens + c.CacheReadTokens + c.CacheWrite5mTokens + c.CacheWrite1hTokens +
		c.CacheWrite30mTokens + c.CacheWrite12hTokens
}

// normaliseTier maps the vendor's speed and service tier to one Tier. It
// returns a reason instead when the pair has no known price.
func normaliseTier(v Vendor, speed, serviceTier string) (Tier, string) {
	speed = strings.ToLower(speed)
	serviceTier = strings.ToLower(serviceTier)
	fast := false
	switch speed {
	case "", "standard":
	case "fast":
		fast = true
	default:
		return "", fmt.Sprintf("unknown speed %q", speed)
	}

	var tier Tier
	switch v {
	case Anthropic:
		// Anthropic's "priority" is Priority Tier, a separate contract with
		// no public per-token price. It is not fast mode.
		switch serviceTier {
		case "", "standard":
			tier = Standard
		case "batch":
			tier = Batch
		default:
			return "", fmt.Sprintf("no public price for anthropic service tier %q", serviceTier)
		}
	case OpenAI:
		// "auto" is a request value: the tier actually used is only known
		// from the response, so it is not priced.
		switch serviceTier {
		case "", "default", "standard":
			tier = Standard
		case "batch":
			tier = Batch
		case "flex":
			tier = Flex
		case "priority", "fast":
			tier = Fast
		default:
			return "", fmt.Sprintf("no price for openai service tier %q", serviceTier)
		}
	default:
		switch serviceTier {
		case "", "standard", "default":
			tier = Standard
		case "batch":
			tier = Batch
		case "flex":
			tier = Flex
		case "priority", "fast":
			tier = Fast
		default:
			return "", fmt.Sprintf("unknown service tier %q", serviceTier)
		}
	}

	if fast {
		switch tier {
		case Standard, Fast:
			tier = Fast
		default:
			return "", fmt.Sprintf("speed fast with service tier %s has no price", tier)
		}
	}
	return tier, ""
}
