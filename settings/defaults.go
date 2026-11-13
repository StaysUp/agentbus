package settings

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/BurntSushi/toml"
)

// The defaults live in code: one set per build. A settings database holds
// only what the person changed from them (DB).
//
// A build that changes a default usually adds an upgrade step (upgradeSteps):
// the key and the value it had before. On its first start, control writes
// that old value into the person's settings, unless they set the key
// already, so nothing changes for them. To take the new default they change
// that one setting. A change meant to reach every install has no step: 0.5.2
// turned [agent_api] tools on and subagents off that way (his call,
// 2026-11-13), so a person who never set them takes the new defaults. A
// build that retires a key adds a retired step: its first start removes the
// key from the stored settings. A new data folder has nothing to keep, so it
// starts on the defaults as they are.

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
		Subagents:     off(),
		HomeFiles:     []string{},
		Limits:        Limits{MaxDepth: num(2), MaxJobsPerTurn: num(10), MaxJobsPerTree: num(30), MaxCostUSDPerTree: &cost},
		AgentAPI:      AgentAPI{Apply: off(), Tools: on()},
		Network:       Network{Contain: ContainOff, Allow: []string{}},
		Sandbox:       Sandbox{Mode: SandboxOff},
	}
}

// upgradeStep is one change of a default, or one key retired: the policy
// key, as a dotted name, and its value before the change. A retired step's
// key is gone from the policy: the step removes it from the stored policy,
// and Old is not used. A retired step with MovedTo names the key that holds
// its value now: the step writes the stored value there, unless the person
// set that key, before it removes the old one.
type upgradeStep struct {
	Key     string
	Old     json.RawMessage
	Retired bool
	MovedTo string
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
	// 0.5.1: agent_api.continue is retired with the continue_session tool
	// (his call, 2026-11-12): a turn's token never continues a session;
	// ask_agent starts a new one, and its result comes back on its own.
	{Key: "agent_api.continue", Retired: true},
	// 0.5.2: sandbox.read moves to home_files (his call, 2026-11-13): the
	// files are copied into every session's home, sandbox on or off.
	{Key: "sandbox.read", Retired: true, MovedTo: "home_files"},
	// 0.5.2: [agent_api] keeps only apply and tools (his call, 2026-11-13).
	// A turn token may always read, list agents, start, review, stop and
	// rename within its own request tree; the limits cap asking.
	{Key: "agent_api.scope", Retired: true},
	{Key: "agent_api.read", Retired: true},
	{Key: "agent_api.list_agents", Retired: true},
	{Key: "agent_api.start", Retired: true},
	{Key: "agent_api.review", Retired: true},
	{Key: "agent_api.cancel", Retired: true},
	{Key: "agent_api.rename", Retired: true},
}

// retiredKeys are the policy keys an older build had and this one does not:
// the keys of the retired steps: a key at the top of the policy, or one in a
// table by its dotted name, such as agent_api.continue. A
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

// movedKey is a key an older build had under another name: From, its old
// dotted name, and To, its new one.
type movedKey struct{ From, To string }

// movedKeys are the moved steps' keys. A file or a body that holds the old
// name is read with its value under the new one, unless it sets the new one
// too, which then wins.
var movedKeys = movedOf(upgradeSteps)

func movedOf(steps []upgradeStep) []movedKey {
	var keys []movedKey
	for _, st := range steps {
		if st.Retired && st.MovedTo != "" {
			keys = append(keys, movedKey{st.Key, st.MovedTo})
		}
	}
	return keys
}

// lookup is the value at a dotted key of top, a policy as a JSON object.
func lookup(top map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	t, sub, nested := strings.Cut(key, ".")
	raw, ok := top[t]
	if !ok || !nested {
		return raw, ok
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(raw, &inner) != nil {
		return nil, false
	}
	raw, ok = inner[sub]
	return raw, ok
}

// place sets a dotted key of top to v.
func place(top map[string]json.RawMessage, key string, v json.RawMessage) error {
	t, sub, nested := strings.Cut(key, ".")
	if !nested {
		top[t] = v
		return nil
	}
	inner := map[string]json.RawMessage{}
	if raw, ok := top[t]; ok {
		if err := json.Unmarshal(raw, &inner); err != nil {
			return err
		}
	}
	inner[sub] = v
	out, err := json.Marshal(inner)
	if err != nil {
		return err
	}
	top[t] = out
	return nil
}

// withMoved is the JSON object b with the value of each moved key under its
// new name, unless b sets the new name too. The old name stays, for
// withoutRetired to leave out. b itself when it holds none or is not an
// object (the decoder then says what is wrong).
func withMoved(b []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return b
	}
	changed := false
	for _, m := range movedKeys {
		v, ok := lookup(top, m.From)
		if _, set := lookup(top, m.To); !ok || set || string(v) == "null" {
			continue
		}
		if place(top, m.To, v) == nil {
			changed = true
		}
	}
	if !changed {
		return b
	}
	out, err := json.Marshal(top)
	if err != nil {
		return b
	}
	return out
}

// movedOver sets, in p decoded from the permissions.toml text b, each moved
// key the file holds under its new name, unless the file sets the new name
// too.
func movedOver(p *Policy, b []byte, md toml.MetaData) error {
	var patch map[string]json.RawMessage
	for _, m := range movedKeys {
		if !md.IsDefined(strings.Split(m.From, ".")...) || md.IsDefined(strings.Split(m.To, ".")...) {
			continue
		}
		if patch == nil {
			var all map[string]any
			if _, err := toml.Decode(string(b), &all); err != nil {
				return errors.New(wrongTypeTOML)
			}
			j, err := json.Marshal(all)
			if err != nil || json.Unmarshal(j, &patch) != nil {
				return errors.New(wrongTypeTOML)
			}
		}
		v, _ := lookup(patch, m.From)
		out := map[string]json.RawMessage{}
		if err := place(out, m.To, v); err != nil {
			return errors.New(wrongTypeTOML)
		}
		j, err := json.Marshal(out)
		if err != nil || json.Unmarshal(j, p) != nil {
			return errors.New(wrongTypeTOML)
		}
	}
	return nil
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
