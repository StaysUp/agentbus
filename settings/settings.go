// Package settings is where the person's settings live: the policy, the
// instructions every turn gets, and the list of the data folder's own files,
// which no turn may edit. The rest of the code reads and writes them through
// Store only.
//
// DB is control's store: settings.db in the data folder, reached only
// through the API, with the defaults in code under a version (defaults.go).
// Remote is a runner's: the server's settings, read through the runner
// protocol with the runner's token, so every runner follows the one policy.
// Files is the settings files of 0.4 and before (permissions.toml,
// instructions.md): the upgrade reads them once into the database
// (upgrade.go), and nothing reads them after that.
//
// The package imports nothing of the module, so every package may use it.
package settings

import (
	"errors"
	"path/filepath"
)

// Store keeps the settings. Each read is fresh: a store never hands back a
// copy older than the call.
type Store interface {
	// Policy is the policy as written, every value filled (Policy). Its
	// folders are as written: Policy.Resolve makes them paths on the
	// machine that runs the turn. A policy that cannot be used is an
	// *Error.
	Policy() (Policy, Version, error)
	// SetPolicy writes p when the stored policy is still at match, or
	// whatever it is with Any. It returns what it stored. A value out of
	// range is an *Invalid; a store moved past match is ErrChanged.
	SetPolicy(p Policy, match Version) (Policy, Version, error)
	// Instructions is the text every turn gets, before {session} is filled
	// in. Text that cannot be used is an *Error.
	Instructions() (string, Version, error)
	// SetInstructions writes text as SetPolicy writes a policy.
	SetInstructions(text string, match Version) (Version, error)
	// Protected are files no turn may edit with its edit tools: the data
	// folder's settings, saved agents, tokens and stores.
	Protected() []string
}

// policyChanger is a store that changes some keys of the policy itself
// (DB.ChangePolicy).
type policyChanger interface {
	ChangePolicy(change Policy, match Version) (Policy, Version, error)
}

// ChangePolicy writes the keys change sets, each key it leaves out keeping
// what s holds, when the policy is still at match: s's own ChangePolicy when
// it has one, else its policy with change on top, written whole. change is
// read with the keys it leaves out unset (DecodePolicyChange).
func ChangePolicy(s Store, change Policy, match Version) (Policy, Version, error) {
	if c, ok := s.(policyChanger); ok {
		return c.ChangePolicy(change, match)
	}
	if err := change.Check(); err != nil {
		return Policy{}, "", &Invalid{Why: err.Error()}
	}
	cur, v, err := s.Policy()
	if err != nil {
		return Policy{}, "", err
	}
	if match != Any && match != v {
		return Policy{}, "", ErrChanged
	}
	p, err := change.over(cur)
	if err != nil {
		return Policy{}, "", &Invalid{Why: err.Error()}
	}
	return s.SetPolicy(p, v)
}

// Snapshot is the settings a turn starts under, read at one moment: the
// policy, the instructions, and the settings version (Revision), which the
// turn records on its turn.started.
type Snapshot struct {
	Policy              Policy
	PolicyVersion       Version
	Instructions        string
	InstructionsVersion Version
	// Revision is the settings version: it moves up by one with every
	// change of the settings. 0 for a store that keeps none, such as files.
	Revision int64
}

// snapshotter is a store that reads every setting at one moment.
type snapshotter interface {
	Snapshot() (Snapshot, error)
}

// Read is s's settings at one moment: its own Snapshot when it has one,
// else its policy and instructions read one after the other, at version 0.
func Read(s Store) (Snapshot, error) {
	if sn, ok := s.(snapshotter); ok {
		return sn.Snapshot()
	}
	var out Snapshot
	var err error
	if out.Policy, out.PolicyVersion, err = s.Policy(); err != nil {
		return Snapshot{}, err
	}
	if out.Instructions, out.InstructionsVersion, err = s.Instructions(); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// Version names one state of a setting, for an ETag. It changes whenever the
// setting does, by the API or by hand.
type Version string

// Any matches every version: If-Match: *.
const Any Version = "*"

// ErrChanged: the setting is no longer at the version the writer read. It
// was changed in between, through the API or by hand.
var ErrChanged = errors.New("settings: changed since read")

// ErrReadOnly: this store's settings are kept elsewhere, such as a runner's,
// which are the server's.
var ErrReadOnly = errors.New("settings: read-only here")

// Error is a setting that cannot be used. A turn's refusal reads
// "<Where> cannot be used, so the turn did not start: <Why>. <Fix>, and send
// the turn again." Version, when set, is the version of what is there, so a
// writer can replace it.
type Error struct {
	Setting string // "permissions" or "instructions"
	Where   string // such as "the permissions file permissions.toml in the data folder"
	Why     string // what is wrong, in words of its own
	Fix     string // such as "Fix the file, or remove it for the defaults"
	Version Version
}

func (e *Error) Error() string { return e.Where + " cannot be used: " + e.Why }

// Invalid is a value a writer sent that is out of range. Why names the key.
type Invalid struct{ Why string }

func (e *Invalid) Error() string { return e.Why }

// The data folder's files.
const (
	PolicyFile       = "permissions.toml"
	InstructionsFile = "instructions.md"
	AgentsFile       = "agents.json"
	// APIClient is the API client a turn's agent calls, under bin/.
	APIClient = "agentbus-api"
)

// dataFiles are the data folder's own files, which no turn may edit: a turn
// that could would raise its own limits, or read a token.
var dataFiles = []string{PolicyFile, InstructionsFile, AgentsFile, "token", "turns_token",
	"enroll_token", "events.db", "jobs.db", DBFile, "runner.json", "folders.json", filepath.Join("bin", APIClient)}

// protected is dataFiles under dir.
func protected(dir string) []string {
	out := make([]string, 0, len(dataFiles))
	for _, f := range dataFiles {
		out = append(out, filepath.Join(dir, f))
	}
	return out
}
