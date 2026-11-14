package store

import (
	"testing"

	"github.com/staysup/agentbus/event"
)

// The latest reading per vendor, subscription and window wins; a window's
// older readings and other kinds are not listed.
func TestPlanUsageLatestPerWindow(t *testing.T) {
	s := newStore(t)
	plan := func(vendor, sub, window string, used float64) event.Event {
		e := ev(event.PlanUsage, "")
		e.Vendor = vendor
		e.Plan = &event.Plan{Subscription: sub, Window: window, UsedPercent: used}
		return e
	}
	mustAppend(t, s, plan("anthropic", "claude", "7d", 10))
	mustAppend(t, s, plan("openai", "codex", "primary", 5))
	mustAppend(t, s, plan("anthropic", "claude", "5h", 40))
	mustAppend(t, s, ev(event.ModelResponded, "s1"))
	mustAppend(t, s, plan("anthropic", "claude", "7d", 12))

	got, err := s.PlanUsage()
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range got {
		seen = append(seen, e.Vendor+"/"+e.Plan.Subscription+"/"+e.Plan.Window)
		if e.Plan.Window == "7d" && e.Plan.UsedPercent != 12 {
			t.Errorf("7d: %v%%, want the latest, 12%%", e.Plan.UsedPercent)
		}
		if e.Seq == 0 {
			t.Errorf("%s: no seq", e.Plan.Window)
		}
	}
	want := []string{"openai/codex/primary", "anthropic/claude/5h", "anthropic/claude/7d"}
	if len(seen) != len(want) {
		t.Fatalf("got %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("got %v, want %v", seen, want)
		}
	}
}
