package cost

import (
	"fmt"

	"github.com/staysup/agentbus/event"
)

// Sink puts the cost on every model.responded, then passes every event on to
// Next. It is safe for concurrent use when Next is.
type Sink struct {
	Table *Table
	Next  event.Sink
}

// Emit implements event.Sink.
func (s Sink) Emit(e event.Event) {
	if e.Kind == event.ModelResponded {
		complete := false
		if e.Usage != nil {
			r := s.Table.Event(e)
			usd := r.USD
			e.CostUSD = &usd
			complete = r.Complete
		}
		e.CostComplete = &complete
	}
	s.Next.Emit(e)
}

// Event prices the usage of one model.responded. Usage nil prices nothing and
// is incomplete: absent usage is unknown, not zero. A cost the vendor
// reported (Usage.CostUSD) is the price, and the table is not read.
//
// The call is priced at the tier and speed the vendor reported. No reported
// speed is standard speed: speed is a request opt-in, and fast mode reports
// "fast". No reported tier is priced at standard, as a lower bound, and is
// incomplete, since a dearer tier cannot be ruled out.
func (t *Table) Event(e event.Event) Result {
	u := e.Usage
	if u == nil {
		return Result{Missing: []string{"the reply had no usage"}}
	}
	// The vendor's own charge is the cost: OpenRouter bills by its own
	// prices, which the table does not hold.
	if u.CostUSD != nil {
		if u.OutputIncomplete {
			return Result{USD: *u.CostUSD, Missing: []string{"the reply ended before its final count"}}
		}
		return Result{USD: *u.CostUSD, Complete: true}
	}
	c := Call{
		Vendor:           Vendor(e.Vendor),
		Model:            e.Model,
		InputTokens:      int64(u.InputTokens),
		OutputTokens:     int64(u.OutputTokens),
		CacheReadTokens:  int64(u.CacheReadTokens),
		OutputIncomplete: u.OutputIncomplete,
		ServiceTier:      u.ServiceTier,
		Speed:            u.Speed,
	}
	if len(u.ServerToolUse) > 0 {
		c.ServerToolUse = make(map[string]int64, len(u.ServerToolUse))
		for name, n := range u.ServerToolUse {
			c.ServerToolUse[name] = int64(n)
		}
	}
	// inference_geo "us" is Anthropic's data residency, priced by the
	// model's Residency multiplier. "global" is the default, and no reported
	// place counts as it: residency is a request opt-in.
	unknownGeo := false
	switch u.InferenceGeo {
	case "", "global":
	case "us":
		c.DataResidency = true
	default:
		unknownGeo = true
	}
	// Cache writes are priced by lifetime. Writes the vendor gave no lifetime
	// for have no price.
	unknown := int64(u.CacheWriteTokens)
	if split := u.CacheWrite5mTokens + u.CacheWrite1hTokens; u.CacheWrite5mTokens >= 0 &&
		u.CacheWrite1hTokens >= 0 && split <= u.CacheWriteTokens {
		c.CacheWrite5mTokens = int64(u.CacheWrite5mTokens)
		c.CacheWrite1hTokens = int64(u.CacheWrite1hTokens)
		unknown -= int64(split)
	}
	r := t.Price(c)
	if unknownGeo {
		r.Complete = false
		r.Missing = append(r.Missing, fmt.Sprintf("no price for inference_geo %q; priced as global", u.InferenceGeo))
	}
	if u.ServiceTier == "" {
		r.Complete = false
		r.Missing = append(r.Missing, "the vendor reported no service tier; priced as standard")
	}
	if unknown != 0 {
		r.Complete = false
		r.Missing = append(r.Missing, fmt.Sprintf("%d cache write tokens have no stated lifetime", unknown))
	}
	return r
}
