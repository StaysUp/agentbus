package run

// The policy a turn runs under comes from the settings store
// (settings.Store), read at the start of every turn, so an edit applies to
// the next turn without a restart. A policy that cannot be used stops the
// turn: a turn never runs without the policy the person wrote.

import (
	"os"
	"path/filepath"

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

// Writes is what one turn may change (harness.Writes).
type Writes = harness.Writes

// writesFor is what a turn of h may change in folder under perm. The harness
// decides which edit folders its flags can hold (harness.Harness.Edit): a
// folder Claude Code's rules cannot hold gets no edit rule, and codex, which
// cannot keep one file of its folder out of its sandbox, runs read-only in a
// folder holding a protected file. why says, for the service log, why edits
// were left out.
func writesFor(h harness.Harness, folder string, readOnly bool, perm settings.Policy, protect []string) (w Writes, why string) {
	w.ReadOnly = readOnly
	if readOnly {
		return w, ""
	}
	for _, p := range protect {
		w.Protect = appendReal(w.Protect, p)
	}
	if perm.Edit != settings.EditSession {
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
