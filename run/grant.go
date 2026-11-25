package run

import (
	"strings"
	"time"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/settings"
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
	// ActApply applies a workspace's changes to the person's folder.
	ActApply
	// ActSessionCreate opens a session in a workspace the grant holds.
	ActSessionCreate
	// ActWorkspaceCreate makes a new workspace: a folder and a sandbox mode.
	ActWorkspaceCreate
	// ActAdmin changes the settings, the saved agents, the runners and the
	// keys.
	ActAdmin
	// ActCopy discards or updates a workspace's copy outside a turn.
	ActCopy

	// AllActions is every class: the owner's.
	AllActions = ActRead | ActRun | ActApply | ActSessionCreate | ActWorkspaceCreate | ActAdmin | ActCopy
)

// Has says a holds every class in b. The empty class is held by no one, so a
// route that names none is refused to every token.
func (a Action) Has(b Action) bool { return b != 0 && a&b == b }

// String names the classes in a, in the vocabulary's order.
func (a Action) String() string {
	var out []string
	for i, n := range []string{"read", "run", "apply", "session.create", "workspace.create", "admin", "copy"} {
		if a&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// Where is the place a request acts in: one session, one workspace, every
// workspace (control's state as a whole, such as the list of all sessions),
// or none (a request that holds no workspace's data, such as the saved
// agents). The zero value is none.
type Where struct {
	Session   string
	Workspace ids.WorkspaceID
	every     bool
}

// Everywhere is every workspace: control's state as a whole.
var Everywhere = Where{every: true}

// InSession is the place of a request on session s.
func InSession(s string) Where { return Where{Session: s} }

// InWorkspace is the place of a request on workspace ws.
func InWorkspace(ws ids.WorkspaceID) Where { return Where{Workspace: ws} }

// Every says w is every workspace.
func (w Where) Every() bool { return w.every }

// None says w holds no workspace's data.
func (w Where) None() bool { return !w.every && w.Session == "" && w.Workspace == "" }

// Reach is where a grant may act: which places, for which class. The owner's
// reach is every workspace (EveryWorkspace). An agent turn's reach is the
// owner's narrowed by the limits the owner set for agents; it reads the
// record, so the API builds it (api/authority.go).
type Reach interface {
	// Holds says act may happen at w: a session, a workspace, or every
	// workspace. It is never asked about none.
	Holds(act Action, w Where) (bool, error)
}

type everyWorkspace struct{}

func (everyWorkspace) Holds(Action, Where) (bool, error) { return true, nil }

// EveryWorkspace is the owner's reach: every place, for every class.
var EveryWorkspace Reach = everyWorkspace{}

// Grant is the authority a request carries: its owner's, minus the limits the
// owner set for the agent it acts through. The API resolves every token it
// takes to one (api/api.go, authorized), and every route asks it one question
// at one door (May): may this action happen in this place.
//
// The owner token holds every action in every workspace, with no limits. An
// agent turn's own token holds the owner's actions but admin, in the reach the
// limits leave, and names the turn the owner acts through: its session and
// turn, Root (the session the person's turn ran in) and Depth (how many agents
// asked in a row before it), all read from the turn's job.
type Grant struct {
	// Owner is the id of the owner whose authority this is (store.Owner): the
	// record names it beside the actor on every event.
	Owner string
	// Actions are the classes the grant holds.
	Actions Action
	// Reach is where it may act. Nil reaches nowhere: an empty grant may do
	// nothing.
	Reach Reach
	// Expiry is when the grant stops working; zero is never. Not checked
	// yet: a turn's token already ends with its turn (revoked, or its
	// attempt no longer current), and no other token has an expiry.
	Expiry time.Time
	// TurnsOnly marks the turns token (the file turns_token): it may call
	// only the routes that say so (route.turns), whatever its actions. It
	// retires with that file, when the owner can make scoped tokens.
	TurnsOnly bool

	// The agent the owner acts through: the turn whose token this is. All
	// empty when the owner acts with their own token.
	Session, Turn string
	Root          string
	Depth         int
	// Harness is the turn's harness, such as claude, when the token's
	// maker knew it; empty for a session's next turn that names no agent,
	// which runs on the session's latest.
	Harness string
	// Rules is the version of the rules the turn took at its start
	// (settings.Snapshot.PolicyVersion), and RulesAt when it took them.
	// Everything the turn does is decided by them: its commands and tools at
	// the gateway, and its calls to the API (O2.4). Empty for the owner's own
	// token, which no rule holds.
	Rules   settings.Version
	RulesAt time.Time
}

// May says g may do act at w: it holds the class, and its reach holds the
// place. A request that acts in no workspace needs the class only.
func (g Grant) May(act Action, w Where) (bool, error) {
	if !g.Actions.Has(act) || g.Reach == nil {
		return false, nil
	}
	if w.None() {
		return true, nil
	}
	return g.Reach.Holds(act, w)
}

// TurnActions are what an agent turn's own token may do: the owner's, but
// admin and changing a copy outside a turn (ActCopy). Admin stays the owner's
// so an agent can never lift the limits the owner set. Apply is narrowed per
// workspace by the Agents may apply switch (api/authority.go).
const TurnActions = ActRead | ActRun | ActApply | ActSessionCreate | ActWorkspaceCreate

// OwnerGrant is the owner token's grant: everything, everywhere, for owner.
func OwnerGrant(owner string) Grant {
	return Grant{Owner: owner, Actions: AllActions, Reach: EveryWorkspace}
}

// TurnsGrant is the turns token's grant: start, stop and read turns, and open
// sessions to run them in, in any workspace, on the routes that say so.
func TurnsGrant(owner string) Grant {
	return Grant{Owner: owner, Actions: ActRead | ActRun | ActSessionCreate, Reach: EveryWorkspace, TurnsOnly: true}
}

// ForTurn is g, a turn's place in its request tree as its token's maker
// recorded it, as the grant the token carries: owner's authority, the turn's
// actions. Its reach is the agent's limits, which read the record: the API
// works it out at its door, once per request. Until then it reaches nowhere.
func ForTurn(g Grant, owner string) Grant {
	g.Owner, g.Actions, g.Reach, g.TurnsOnly, g.Expiry = owner, TurnActions, nil, false, time.Time{}
	return g
}
