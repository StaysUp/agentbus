package cost

import (
	"fmt"
	"math"
	"strings"
)

// Drift is one rate that two tables price differently.
type Drift struct {
	// Model is the entry's vendor and model, as Models lists it.
	Model string
	// Rate names what differs: a tier and class ("standard input",
	// "long batch output"), "long-context threshold" or "residency".
	Rate string
	// Have is the value in the table Diff was called on, Other the value in
	// the table it was compared with. Rates are USD per million tokens, the
	// threshold is tokens and residency is a multiplier.
	Have, Other float64
}

// Diff compares every entry of t with the same vendor and model in other and
// returns the rates they price differently, sorted by model and then in the
// order tiers and classes are listed. Entries of t that other has no entry
// for are returned in missing; entries only other has are not compared.
//
// Only what both entries know is compared: a tier or class one of them has
// no price for is unknown there, not different, so it is skipped. The same
// holds for the long-context rates and the residency multiplier.
func (t *Table) Diff(other *Table) (drift []Drift, missing []string) {
	for _, m := range t.Models() {
		have := t.prices[m]
		theirs, ok := other.prices[m]
		if !ok {
			missing = append(missing, m)
			continue
		}
		drift = append(drift, diffTiers(m, "", have.Tiers, theirs.Tiers)...)
		if have.LongAbove > 0 && theirs.LongAbove > 0 {
			if have.LongAbove != theirs.LongAbove {
				drift = append(drift, Drift{Model: m, Rate: "long-context threshold", Have: float64(have.LongAbove), Other: float64(theirs.LongAbove)})
			}
			drift = append(drift, diffTiers(m, "long ", have.LongTiers, theirs.LongTiers)...)
		}
		if have.Residency > 0 && theirs.Residency > 0 && !same(have.Residency, theirs.Residency) {
			drift = append(drift, Drift{Model: m, Rate: "residency", Have: have.Residency, Other: theirs.Residency})
		}
	}
	return drift, missing
}

// tiers is the fixed order Diff reports tiers in.
var tiers = []Tier{Standard, Batch, Flex, Fast}

func diffTiers(model, prefix string, have, theirs map[Tier]Rates) []Drift {
	var out []Drift
	for _, tier := range tiers {
		a, b := have[tier], theirs[tier]
		for _, class := range classes {
			x, okA := a[class]
			y, okB := b[class]
			if okA && okB && !same(x, y) {
				rate := prefix + string(tier) + " " + strings.ReplaceAll(string(class), "_", " ")
				out = append(out, Drift{Model: model, Rate: rate, Have: x, Other: y})
			}
		}
	}
	return out
}

// same reports whether two rates are equal once the rounding of a per-token
// price scaled to per million tokens is allowed for: 3e-06 * 1e6 is
// 2.9999999999999996, not 3.
func same(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}

// String formats the drift as one line: model, rate, then both values.
func (d Drift) String() string {
	return fmt.Sprintf("%s %s: %g, other %g", d.Model, d.Rate, d.Have, d.Other)
}
