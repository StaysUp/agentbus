package main

import (
	"testing"

	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/sandbox"
)

// A process whose sessions get no home of their own (a runner with
// -gateway local) reports it cannot sandbox, with the reason a walled turn
// is refused with, so Settings can grey the switch out. A runner with homes
// reports what the machine can (0.5.1), and so does agentbus up, whose
// sessions have homes since its credentials come from control too.
func TestSandboxReportNeedsHomes(t *testing.T) {
	if c := (config{mode: modeRunner, gatewayMode: gatewayLocal}); true {
		if can, why := c.canSandbox(); can || why != engine.NoHomes {
			t.Errorf("mode %q gateway %q: can %v, %q", c.mode, c.gatewayMode, can, why)
		}
	}
	wantCan, wantWhy := sandbox.Can()
	for _, c := range []config{{mode: modeRunner}, {mode: modeUp}} {
		if can, why := c.canSandbox(); can != wantCan || why != wantWhy {
			t.Errorf("mode %q: can %v %q, want the machine's %v %q", c.mode, can, why, wantCan, wantWhy)
		}
	}
}
