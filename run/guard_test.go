package run

import (
	"slices"
	"testing"
)

func TestTurnRules(t *testing.T) {
	e := &Engine{}
	e.turnRules.set("s1", "t_1", []string{"git push"})
	e.turnRules.set("s1", "t_2", nil)
	if got := e.Forbid("s1", "t_1"); !slices.Equal(got, []string{"git push"}) {
		t.Errorf("Forbid = %q", got)
	}
	if got := e.Forbid("s1", "t_2"); got != nil {
		t.Errorf("a turn without forbid got %q", got)
	}
	e.turnRules.clear("s1", "t_1")
	if got := e.Forbid("s1", "t_1"); got != nil {
		t.Errorf("after clear: %q", got)
	}
}
