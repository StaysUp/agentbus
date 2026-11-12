package main

import (
	"testing"

	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/sandbox"
)

// A process whose sessions get no home of their own (agentbus up, a runner
// with -gateway local) reports it cannot sandbox, with the reason a walled
// turn is refused with, so Settings can grey the switch out. A runner with
// homes reports what the machine can (0.5.1).
func TestSandboxReportNeedsHomes(t *testing.T) {
	for _, c := range []config{{mode: modeUp}, {mode: modeRunner, gatewayMode: gatewayLocal}} {
		if can, why := c.canSandbox(); can || why != engine.NoHomes {
			t.Errorf("mode %q gateway %q: can %v, %q", c.mode, c.gatewayMode, can, why)
		}
	}
	wantCan, wantWhy := sandbox.Can()
	if can, why := (config{mode: modeRunner}).canSandbox(); can != wantCan || why != wantWhy {
		t.Errorf("runner: can %v %q, want the machine's %v %q", can, why, wantCan, wantWhy)
	}
}
