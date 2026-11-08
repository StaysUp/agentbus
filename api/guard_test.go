package api

import (
	"slices"
	"testing"
)

func TestTurnRules(t *testing.T) {
	a := &API{}
	a.turnRules.set("s1", "t_1", []string{"git push"})
	a.turnRules.set("s1", "t_2", nil)
	if got := a.Forbid("s1", "t_1"); !slices.Equal(got, []string{"git push"}) {
		t.Errorf("Forbid = %q", got)
	}
	if got := a.Forbid("s1", "t_2"); got != nil {
		t.Errorf("a turn without forbid got %q", got)
	}
	a.turnRules.clear("s1", "t_1")
	if got := a.Forbid("s1", "t_1"); got != nil {
		t.Errorf("after clear: %q", got)
	}
}
