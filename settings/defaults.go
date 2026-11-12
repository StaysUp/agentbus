package settings

// The defaults live in code, each set under a version number. A control's
// settings database names the version it runs under, and holds only what the
// person changed from that version's values (DB). A build that changes a
// default adds a new version and leaves the old ones as they were. A control
// keeps its version until the person moves to the new one (PUT
// /api/v0/settings/defaults), so an upgrade never widens what a turn may do
// on its own.

// LatestDefaults is the newest version of the defaults. A new settings
// database starts on it.
func LatestDefaults() int { return len(defaultSets) }

// Defaults is one version of the defaults.
type Defaults struct {
	Version      int
	Policy       Policy
	Instructions string
}

// defaultSets is every version, oldest first. Version n is defaultSets[n-1].
// A version, once released, never changes: add a new one instead.
var defaultSets = []func() Defaults{defaultsV1}

// DefaultsFor is version v of the defaults, and false when there is none.
func DefaultsFor(v int) (Defaults, bool) {
	if v < 1 || v > len(defaultSets) {
		return Defaults{}, false
	}
	d := defaultSets[v-1]()
	d.Version = v
	return d, true
}

// defaultsV1 are the defaults of 0.4, written out in full, so a change of
// fill() cannot change them.
func defaultsV1() Defaults {
	on := func() *bool { b := true; return &b }
	off := func() *bool { b := false; return &b }
	num := func(n int) *int { return &n }
	cost := 0.0
	return Defaults{
		Policy: Policy{
			ReadFolders:            []string{},
			Allow:                  []string{},
			Forbid:                 []string{},
			Edit:                   EditSession,
			Isolation:              IsolationAuto,
			ShareReadOnly:          []string{},
			Subagents:              on(),
			SubscriptionsOnRunners: SubscriptionsOff,
			Limits:                 Limits{MaxDepth: num(2), MaxJobsPerTurn: num(10), MaxJobsPerTree: num(30), MaxCostUSDPerTree: &cost},
			AgentAPI: AgentAPI{Scope: ScopeTree, Read: on(), ListAgents: on(), Start: on(), Continue: on(),
				Review: on(), Cancel: on(), Rename: on(), Apply: on(), Tools: off()},
			Network: Network{Contain: ContainOff, Allow: []string{}},
		},
		Instructions: DefaultInstructions,
	}
}
