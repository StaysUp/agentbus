package run

// The policy a turn runs under comes from the settings store
// (settings.Store), read at the start of every turn, so an edit applies to
// the next turn without a restart. A policy that cannot be used stops the
// turn: a turn never runs without the policy the person wrote.
//
// What each list becomes, per harness (the flags are built in harness/):
//
//	allow/ask/forbid  the gateway decides every shell command of the
//	                  turn from these lists, for claude and codex alike
//	                  (gateway/guard.go): forbid refused, ask held, allow run, a
//	                  command on no list held for a person.
//	                  claude's own Bash check must not refuse one first, so an
//	                  edit turn is given the whole Bash tool (--allowedTools Bash)
//	                  and the gateway is the gate; agy, whose format the guard
//	                  cannot read, refuses a turn that has forbid or ask.
//	edit = "session"  claude: --allowedTools Edit(//<folder>/**), and a deny Edit(//<file>) on each data folder file;
//	                  codex: --sandbox workspace-write -c sandbox_workspace_write.network_access=true
//	edit = "none"     claude: --disallowedTools Edit,Write,NotebookEdit; codex: --sandbox read-only (no network)
//	subagents = false claude: --disallowedTools Task,Agent,Workflow; codex: --disable multi_agent --disable multi_agent_v2
//
// A forbid wins over an allow and over the person's own settings. Each turn's flags are
// in its header line in logs/runs/<session>.log.

import (
	"os"
	"path/filepath"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/settings"
)

// policy is the turn's policy, its folders resolved on this machine, with ~
// as the service's home folder. No store gives every default.
func (e *Engine) policy() (settings.Policy, error) {
	if e.cfg.Settings == nil {
		return settings.DefaultPolicy(), nil
	}
	p, _, err := e.cfg.Settings.Policy()
	if err != nil {
		return settings.Policy{}, err
	}
	home, _ := os.UserHomeDir()
	return p.Resolve(home)
}

// settingsNow is what a turn starts under, read at one moment: the policy,
// its folders resolved as policy resolves them, the instructions before
// {session} is filled in, and the settings version. No store gives every
// default, at version 0.
func (e *Engine) settingsNow() (settings.Snapshot, error) {
	if e.cfg.Settings == nil {
		return settings.Snapshot{Policy: settings.DefaultPolicy(), Instructions: settings.DefaultInstructions}, nil
	}
	s, err := settings.Read(e.cfg.Settings)
	if err != nil {
		return settings.Snapshot{}, err
	}
	home, _ := os.UserHomeDir()
	if s.Policy, err = s.Policy.Resolve(home); err != nil {
		return settings.Snapshot{}, err
	}
	return s, nil
}

// Writes is what one turn may change (harness.Writes).
type Writes = harness.Writes

// writesFor is what a turn of h may change in folder under perm. The harness
// decides which edit folders its flags can hold (harness.Harness.Edit): a
// folder Claude Code's rules cannot hold gets no edit rule, and codex, which
// cannot keep one file of its folder out of its sandbox, runs read-only in a
// folder holding a protected file. why says, for the service log, why edits
// were left out.
func writesFor(h harness.Harness, folder string, perm settings.Policy, protect []string) (w Writes, why string) {
	for _, p := range protect {
		w.Protect = appendReal(w.Protect, p)
	}
	// Temporary glue: mapping tool:edit to harness edit writes until AgentBus's own loop replaces each harness.
	rules := gateway.Rules{Allow: perm.Allow, Ask: perm.Ask, Forbid: perm.Forbid}
	if dec, _ := gateway.DecideTool(rules, "tool:edit"); dec != gateway.Allow {
		return w, ""
	}
	w.Edit, why = h.Edit(appendReal(nil, folder), w.Protect)
	return w, why
}

// appendReal appends p, absolute and clean, and its real path when that
// differs, as a folder reached through a symlink (/tmp on macOS) has two
// names. A file that does not exist yet gets its folder's real path.
func appendReal(list []string, p string) []string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	list = append(list, p)
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		if d, derr := filepath.EvalSymlinks(filepath.Dir(p)); derr == nil {
			r, err = filepath.Join(d, filepath.Base(p)), nil
		}
	}
	if err == nil && r != p {
		list = append(list, r)
	}
	return list
}
