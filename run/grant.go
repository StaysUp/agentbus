package run

import (
	"strings"
	"time"
)

// Action is one class of what a token may do. Every route of the client API
// names the one class it needs (api/routes.go), and a grant holds a set of
// them. The vocabulary is fixed and generic: no class is for one route, one
// vendor or one program (DESIGN_tokens.md, decided 2026-11-23).
type Action uint8

const (
	// ActRead reads sessions, turns, their changes and calls, the saved
	// agents and the settings' values.
	ActRead Action = 1 << iota
	// ActRun starts and stops turns, asks other agents, and registers an on
	// for a reply.
	ActRun
	// ActApply applies a workspace's changes to the person's folder, and
	// discards or updates its copy.
	ActApply
	// ActSessionCreate opens a session in a workspace the grant holds.
	ActSessionCreate
	// ActWorkspaceCreate makes a new workspace: a folder and a sandbox mode.
	ActWorkspaceCreate
	// ActAdmin changes the settings, the saved agents, the runners and the
	// keys.
	ActAdmin

	// AllActions is every class: the owner's.
	AllActions = ActRead | ActRun | ActApply | ActSessionCreate | ActWorkspaceCreate | ActAdmin
)

// Has says a holds every class in b. The empty class is held by no one, so a
// route that names none is refused to every token.
func (a Action) Has(b Action) bool { return b != 0 && a&b == b }

// String names the classes in a, in the vocabulary's order.
func (a Action) String() string {
	var out []string
	for i, n := range []string{"read", "run", "apply", "session.create", "workspace.create", "admin"} {
		if a&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// Workspaces is which workspaces a grant may act in.
type Workspaces uint8

const (
	// NoWorkspaces is the zero value: none.
	NoWorkspaces Workspaces = iota
	// OwnWorkspaces is an agent turn's: the workspace of its own session,
	// the workspaces its session made, and the sessions of its request tree.
	// The grant's Session, Turn and Root say which.
	OwnWorkspaces
	// AllWorkspaces is every workspace.
	AllWorkspaces
)

// Grant is what a token may do. The API resolves every token it takes to one
// (api/api.go, authorized) and checks every request against it at one door.
//
// The owner token holds every action in every workspace. An agent turn's own
// token holds the turn's actions in its own workspaces, and carries its place
// in a request tree: its session and turn, Root (the session the person's
// turn ran in) and Depth (how many agents asked in a row before it).
type Grant struct {
	Workspaces Workspaces
	Actions    Action
	// Expiry is when the grant stops working; zero is never. Not checked
	// yet: a turn's token already ends with its turn (revoked, or its
	// attempt no longer current), and no other token has an expiry.
	Expiry time.Time
	// TurnsOnly marks the turns token (the file turns_token): it may call
	// only the routes that say so (route.turns), whatever its actions. It
	// retires with that file, when the owner can make scoped tokens.
	TurnsOnly bool

	Session, Turn string
	Root          string
	Depth         int
	// Harness is the turn's harness, such as claude, when the token's
	// maker knew it; empty for a session's next turn that names no agent,
	// which runs on the session's latest.
	Harness string
}

// TurnActions are what an agent turn's own token may do. Apply is narrowed
// per request by the person's Agents may apply switch (api/tokens.go).
const TurnActions = ActRead | ActRun | ActApply | ActSessionCreate | ActWorkspaceCreate

// OwnerGrant is the owner token's grant: everything.
func OwnerGrant() Grant { return Grant{Workspaces: AllWorkspaces, Actions: AllActions} }

// TurnsGrant is the turns token's grant: start, stop and read turns, and open
// sessions to run them in, in any workspace, on the routes that say so.
func TurnsGrant() Grant {
	return Grant{Workspaces: AllWorkspaces, Actions: ActRead | ActRun | ActSessionCreate, TurnsOnly: true}
}

// ForTurn is g, a turn's place in its request tree as its token's maker
// recorded it, as the grant the token carries: the turn's actions in its own
// workspaces.
func ForTurn(g Grant) Grant {
	g.Workspaces, g.Actions, g.TurnsOnly, g.Expiry = OwnWorkspaces, TurnActions, false, time.Time{}
	return g
}

// Bound says g is bound to a request tree: an agent turn's token.
func (g Grant) Bound() bool { return g.Workspaces == OwnWorkspaces }
