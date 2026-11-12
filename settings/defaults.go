package settings

import "encoding/json"

// The defaults live in code: one set per build. A settings database holds
// only what the person changed from them (DB).
//
// A build that changes a default adds an upgrade step (upgradeSteps): the
// key and the value it had before. On its first start, control writes that
// old value into the person's settings, unless they set the key already, so
// nothing changes for them. To take the new default they change that one
// setting. A build that retires a key adds a retired step: its first start
// removes the key from the stored settings. A new data folder has nothing to
// keep, so it starts on the defaults as they are.

// defaultPolicy is the policy of a person who changed nothing, written out
// in full, so a change of fill() cannot change it.
func defaultPolicy() Policy {
	on := func() *bool { b := true; return &b }
	off := func() *bool { b := false; return &b }
	num := func(n int) *int { return &n }
	cost := 0.0
	return Policy{
		ReadFolders:   []string{},
		Allow:         []string{},
		Forbid:        []string{},
		Edit:          EditSession,
		Isolation:     IsolationAuto,
		ShareReadOnly: []string{},
		Subagents:     on(),
		Limits:        Limits{MaxDepth: num(2), MaxJobsPerTurn: num(10), MaxJobsPerTree: num(30), MaxCostUSDPerTree: &cost},
		AgentAPI: AgentAPI{Scope: ScopeTree, Read: on(), ListAgents: on(), Start: on(), Continue: on(),
			Review: on(), Cancel: on(), Rename: on(), Apply: off(), Tools: off()},
		Network: Network{Contain: ContainOff, Allow: []string{}},
		Sandbox: Sandbox{Mode: SandboxOff, Read: []string{}},
	}
}

// upgradeStep is one change of a default, or one key retired: the policy
// key, as a dotted name, and its value before the change. A retired step's
// key is gone from the policy: the step removes it from the stored policy,
// and Old is not used.
type upgradeStep struct {
	Key     string
	Old     json.RawMessage
	Retired bool
}

// upgradeSteps are the changes of a default and the retired keys, oldest
// first. A step, once released, never changes: add a new one instead. A
// settings database counts how many it has written (meta upgrades), so each
// runs once.
var upgradeSteps = []upgradeStep{
	// 0.5: a turn's own token no longer applies a turn's changes to the
	// person's folder; only the main token does, until the person turns it
	// on. 0.4 let it.
	{Key: "agent_api.apply", Old: json.RawMessage("true")},
	// 0.5: commands, the switch of the person's own "!" commands, is
	// retired: a "!" is a message to the session's agent.
	{Key: "commands", Retired: true},
	// 0.5.1: subscriptions_on_runners is retired: a machine joins only when
	// the person approves its code, so every runner is theirs, and a
	// subscription connected on control is used on all of them.
	{Key: "subscriptions_on_runners", Retired: true},
}

// retiredKeys are the policy keys an older build had and this one does not:
// the keys of the retired steps. Each is a key at the top of the policy. A
// file or a body that still holds one is read with it left out, so a
// permissions.toml written before, or an older client, keeps working; any
// other unknown key is still refused.
var retiredKeys = retiredOf(upgradeSteps)

func retiredOf(steps []upgradeStep) []string {
	var keys []string
	for _, st := range steps {
		if st.Retired {
			keys = append(keys, st.Key)
		}
	}
	return keys
}

// policyBefore is the defaults as they were before step n: every step from
// n on undone. Keys an 0.4 file leaves out take these values on the import.
func policyBefore(n int) Policy {
	f := flatten(defaultPolicy())
	for i := len(upgradeSteps) - 1; i >= n && i >= 0; i-- {
		if !upgradeSteps[i].Retired {
			f[upgradeSteps[i].Key] = upgradeSteps[i].Old
		}
	}
	p, err := f.policy()
	if err != nil {
		panic(err) // the steps hold values of the policy's own types
	}
	return p
}
