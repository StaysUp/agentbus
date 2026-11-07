package cost

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// LoadLiteLLM reads a price file in LiteLLM's model_prices_and_context_window
// format from path and returns it as a Table. Prices in that format are USD
// per token; the table holds USD per million tokens.
//
// What it reads, per entry:
//   - litellm_provider names the vendor; an entry without one is skipped.
//   - input_cost_per_token, output_cost_per_token, cache_read_input_token_cost.
//   - cache_creation_input_token_cost is the vendor's shorter cache lifetime:
//     5 minutes for anthropic, 30 minutes for openai. For any other provider
//     the lifetime is not known and the key is ignored.
//   - cache_creation_input_token_cost_above_1hr and _above_12hr.
//   - a suffix _batches, _flex or _priority selects that tier. For anthropic,
//     _priority is ignored: Anthropic's Priority Tier is not fast mode.
//   - an infix _above_<N>k_tokens gives long-context rates above N*1000
//     prompt tokens. An entry with more than one threshold is skipped,
//     because one Price holds one threshold.
//   - provider_specific_entry.us multiplies every rate under data residency.
//     provider_specific_entry.fast builds the fast tier from the standard one
//     when the entry has no fast prices of its own.
//
// Keys it does not understand are ignored, never guessed at. A class with no
// key has no price, so a call that uses it is reported incomplete.
func LoadLiteLLM(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read price file: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse price file %s: %w", path, err)
	}

	var prices, aliases []Price
	for name, msg := range raw {
		if name == "sample_spec" {
			continue // LiteLLM's description of the format, not a model
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(msg, &entry); err != nil {
			continue // not an object, such as a comment string
		}
		p, ok := parseEntry(name, entry)
		if !ok {
			continue
		}
		prices = append(prices, p)
		if short, found := strings.CutPrefix(name, string(p.Vendor)+"/"); found {
			a := p
			a.Model = short
			aliases = append(aliases, a)
		}
	}
	// "azure/gpt-x" is also reachable as azure's "gpt-x", unless the file has
	// its own "gpt-x" entry for that same vendor. A bare "gpt-x" that belongs
	// to another vendor does not block it: the table is keyed by vendor.
	direct := make(map[string]bool, len(prices))
	for _, p := range prices {
		direct[key(p.Vendor, p.Model)] = true
	}
	for _, a := range aliases {
		if !direct[key(a.Vendor, a.Model)] {
			prices = append(prices, a)
		}
	}
	return NewTable(prices), nil
}

var litellmKey = regexp.MustCompile(
	`^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost)` +
		`(?:_above_(1hr|12hr))?` +
		`(?:_above_(\d+)k_tokens)?` +
		`(?:_(batches|flex|priority))?$`)

func parseEntry(name string, entry map[string]json.RawMessage) (Price, bool) {
	var provider string
	if err := json.Unmarshal(entry["litellm_provider"], &provider); err != nil || provider == "" {
		return Price{}, false
	}
	p := Price{Vendor: Vendor(provider), Model: name, Tiers: map[Tier]Rates{}}

	var threshold int64
	long := map[Tier]Rates{}
	for k, v := range entry {
		m := litellmKey.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		// JSON null unmarshals into a float64 as zero without an error, which
		// would price an unknown class as free. Only a number is a price.
		var perToken *float64
		if err := json.Unmarshal(v, &perToken); err != nil || perToken == nil || *perToken < 0 {
			continue
		}
		class, ok := litellmClass(p.Vendor, m[1], m[2])
		if !ok {
			continue
		}
		tier, ok := litellmTier(p.Vendor, m[4])
		if !ok {
			continue
		}
		dst := p.Tiers
		if m[3] != "" {
			n, err := strconv.ParseInt(m[3], 10, 64)
			if err != nil {
				continue
			}
			if threshold != 0 && threshold != n*1000 {
				return Price{}, false
			}
			threshold = n * 1000
			dst = long
		}
		if dst[tier] == nil {
			dst[tier] = Rates{}
		}
		dst[tier][class] = *perToken * 1e6
	}
	if threshold != 0 {
		p.LongAbove = threshold
		p.LongTiers = long
	}

	var extra map[string]float64
	if b, ok := entry["provider_specific_entry"]; ok && json.Unmarshal(b, &extra) == nil {
		if us, ok := extra["us"]; ok && us > 0 {
			p.Residency = us
		}
		if f, ok := extra["fast"]; ok && f > 0 && p.Tiers[Fast] == nil && p.Tiers[Standard] != nil {
			fast := Rates{}
			for cl, rate := range p.Tiers[Standard] {
				fast[cl] = rate * f
			}
			p.Tiers[Fast] = fast
		}
	}
	if len(p.Tiers) == 0 && len(p.LongTiers) == 0 {
		return Price{}, false
	}
	return p, true
}

func litellmClass(v Vendor, base, lifetime string) (Class, bool) {
	switch base {
	case "input_cost_per_token":
		return Input, lifetime == ""
	case "output_cost_per_token":
		return Output, lifetime == ""
	case "cache_read_input_token_cost":
		return CacheRead, lifetime == ""
	}
	switch lifetime {
	case "1hr":
		return CacheWrite1h, true
	case "12hr":
		return CacheWrite12h, true
	}
	switch v {
	case Anthropic:
		return CacheWrite5m, true
	case OpenAI:
		return CacheWrite30m, true
	}
	return "", false
}

func litellmTier(v Vendor, suffix string) (Tier, bool) {
	switch suffix {
	case "":
		return Standard, true
	case "batches":
		return Batch, true
	case "flex":
		return Flex, true
	case "priority":
		return Fast, v != Anthropic
	}
	return "", false
}
