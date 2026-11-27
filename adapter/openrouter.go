package adapter

// OpenRouterCost is the cost OpenRouter puts in a reply's usage object. It is
// the same in its three API formats (Chat Completions, Responses and
// Anthropic Messages), so the adapters embed it in their usage types.
//
// usage.cost is what OpenRouter charged in its own credits. On a call made
// with the person's own provider key (BYOK, is_byok true) that is only
// OpenRouter's fee, often 0, and the provider's charge is
// cost_details.upstream_inference_cost, which OpenRouter's usage accounting
// page says "is only available for BYOK requests" (read 2026-11-26; payload
// from issue #94 and anomalyco/opencode#60558, not yet seen live here). On
// other calls upstream_inference_cost is null (seen 2026-11-13).
type OpenRouterCost struct {
	Cost        *float64 `json:"cost"`
	BYOK        bool     `json:"is_byok"`
	CostDetails struct {
		Upstream *float64 `json:"upstream_inference_cost"`
	} `json:"cost_details"`
}

// Charge is the call's cost in US dollars, or nil when OpenRouter reported
// none. On a BYOK call it is OpenRouter's fee plus the provider's charge; with
// the provider's charge absent it is the fee alone, and partial is true.
func (c OpenRouterCost) Charge() (usd *float64, partial bool) {
	if c.Cost == nil {
		return nil, false
	}
	if !c.BYOK {
		return c.Cost, false
	}
	if c.CostDetails.Upstream == nil {
		return c.Cost, true
	}
	total := *c.Cost + *c.CostDetails.Upstream
	return &total, false
}
