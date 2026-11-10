// Package settings is where the person's settings live: the policy
// (permissions.toml), the instructions every turn gets (instructions.md), and
// the list of the data folder's own files, which no turn may edit. The rest of
// the code reads and writes them through Store only, so they can move from
// files to a database without it changing.
//
// Two stores today. Files is the data folder's files, read fresh on every
// call, so a hand edit applies to the next turn without a restart. Remote is
// a runner's: the server's settings, read through the runner protocol with
// the runner's token, so every runner follows the one policy.
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
	"enroll_token", "events.db", "jobs.db", "runner.json", "folders.json", filepath.Join("bin", APIClient)}

// protected is dataFiles under dir.
func protected(dir string) []string {
	out := make([]string, 0, len(dataFiles))
	for _, f := range dataFiles {
		out = append(out, filepath.Join(dir, f))
	}
	return out
}
