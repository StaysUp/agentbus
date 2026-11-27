package adapter

import (
	"math"
	"testing"
)

// A BYOK call costs OpenRouter's fee plus the provider's charge; a call that
// is not BYOK costs usage.cost, whatever cost_details says.
func TestOpenRouterCharge(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	byok := OpenRouterCost{Cost: f(0.00001), BYOK: true}
	byok.CostDetails.Upstream = f(0.000232)
	if usd, partial := byok.Charge(); usd == nil || math.Abs(*usd-0.000242) > 1e-12 || partial {
		t.Errorf("byok: %v %v", usd, partial)
	}
	plain := OpenRouterCost{Cost: f(0.0034)}
	plain.CostDetails.Upstream = f(1)
	if usd, partial := plain.Charge(); usd == nil || *usd != 0.0034 || partial {
		t.Errorf("not byok: %v %v", usd, partial)
	}
	if usd, partial := (OpenRouterCost{BYOK: true}).Charge(); usd != nil || partial {
		t.Errorf("no cost: %v %v", usd, partial)
	}
}
