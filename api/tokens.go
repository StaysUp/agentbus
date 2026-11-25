package api

// Turn tokens. POST /api/v0/turns gives each turn's child its own token in
// AGENTBUS_TOKEN, with the service's address in AGENTBUS_URL. With it the
// agent reads its session's history and asks other agents: it starts turns
// on any saved agent, each in a new session, asks for reviews, and reads
// their results. It never continues a session (his call, 2026-11-12). Each result comes back to its session as a later
// turn, delivered by an on row the ask path registers (queue/on.go, FireOns).
//
// What a turn token must never do: read or change the settings (the policy
// and the instructions: every /api/v0/settings route is main-token only),
// change or remove saved agents, see or make tokens (none is
// ever in a reply), close sessions, write program events, or read sessions
// outside its authority. Its own session and the person's session are never
// continued by it: results reach them by delivery only.
//
// One rule decides what a turn token reads (inAuthority): the sessions of the
// workspaces in its authority. A Direct session's authority is its machine,
// since it works in the folders with no wall and can read the same store on
// disk. A Copy session's is its own workspace and the workspaces its session
// made. Reads, search, ons, rename and stop of asked work all ask it.
//
// A request tree is the person's session (its root) and every session work
// asked for from it, at any depth (queue.Chain). It bounds the caps on asked
// work only (depth, jobs, cost); it decides no read.
//
// Every token resolves to a grant at one door (authority.go): the owner's
// authority minus the limits the owner set for the agent it acts through. A
// turn token's limits are the rules of this file: its reach (inAuthority,
// ownWorkspace), what it may ask for (askChecks) and what it may make
// (workspaceAuthority, workspaces.go).
//
// The engine mints and keeps the tokens (package run), in memory only, as
// their SHA-256, and revokes each when its turn's child exits.

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// Refusals a turn token gets. Fixed wording: they never quote the request.
const (
	turnWriteRefused = "a turn token cannot call this route; it takes the main token. " +
		"A turn token reads the sessions and turns of the workspaces in its authority, lists agents, " +
		"and asks for work with POST /api/v0/turns"
	turnSessionRefused = "a turn token reads only the sessions of the workspaces in its authority: " +
		"its machine when its own workspace works in the folder as it is, else its own workspace and the workspaces its session made"
	turnCallRefused     = "a turn token reads only the calls of the sessions of the workspaces in its authority"
	turnContinueRefused = "a turn token does not continue sessions: ask_agent starts a new one, and its result comes back to your session " +
		"as a later turn; to follow up, ask again with what the result said"
	turnAskedRefused = "a turn token stops only asked work in the sessions it may read"
	turnNoSession    = "a turn needs a session: create one in your workspace with POST /api/v0/sessions, then run its turn"
)

// The refusal when [agent_api] turns a kind of call off, naming the
// switch as the cockpit's Settings shows it. Fixed wording.
func turnSwitchedOff(what, label string) string {
	return "a turn token may not " + what + " here: " + label + " is off in Settings (Permissions)"
}

// turnPolicyBroken is the refusal when the policy cannot be used. It names
// where the policy is, never what is wrong with it.
func turnPolicyBroken(err error) string {
	where := "the permissions"
	var se *settings.Error
	if errors.As(err, &se) {
		where = se.Where
	}
	return where + " cannot be used, so a turn token's requests are refused; fix it"
}

// rulesOf is the policy turn grant g is held to: the rules its turn took at
// its start (O2.4), so its calls to the API are decided by the same rules as
// its commands and tools at the gateway, and a change of the permissions
// applies from the next turn. A turn whose token names no rules started before
// they were kept (an attempt from an older control): it is held to the
// policy as it is now, as it was then. Where this service runs turns, its
// folders must be folders here, as a turn's are: a policy a turn could not
// start under refuses its token's requests too. Rules that are not kept any
// more refuse them as well: nothing is decided by other rules in their place.
func (a *API) rulesOf(g run.Grant) (settings.Policy, error) {
	if a.cfg.Settings == nil {
		return settings.DefaultPolicy(), nil
	}
	var p settings.Policy
	var err error
	if g.Rules == "" {
		p, _, err = a.cfg.Settings.Policy()
	} else {
		var found bool
		p, found, err = settings.RulesOf(a.cfg.Settings, g.Rules)
		if err == nil && !found {
			err = &settings.Error{Setting: settings.SettingPermissions, Where: "the rules this turn started under",
				Why: "they are no longer kept", Fix: "Stop the turn and start it again"}
		}
	}
	if err != nil || a.cfg.Engine == nil {
		return p, err
	}
	home, _ := os.UserHomeDir()
	return p.Resolve(home)
}

// queueLimits are [limits] as the queue takes them.
func queueLimits(p settings.Policy) queue.Limits {
	return queue.Limits{MaxDepth: *p.Limits.MaxDepth, MaxPerTurn: *p.Limits.MaxJobsPerTurn, MaxPerTree: *p.Limits.MaxJobsPerTree}
}

// mayReadTurn is the one check of whether g may read turn id: the turn's
// session (session when sent, else the turn's newest) is in g's authority
// (inAuthority). found is false when there is no such turn. Reading a turn
// (atTurn) and watching one (POST /api/v0/ons) both ask it, so what a turn
// token may watch is what it may read, and a wider read widens both.
func (a *API) mayReadTurn(g run.Grant, id, session string) (found, ok bool, err error) {
	session, err = a.sessionOfTurn(id, session)
	if err != nil || session == "" {
		return false, false, err
	}
	ok, err = a.inAuthority(g, session)
	return true, ok, err
}

// How a workspace stands to a turn token (ownWorkspace).
const (
	notOwn       = iota // not one of the token's workspaces
	ownSession          // the workspace of the token's own session
	madeByCopy          // made by the token's session, which runs in a copy
	madeByDirect        // made by the token's session, which runs in its folders
)

// ownWorkspace says how workspace id stands to g: the workspace of g's session,
// one g's session made (POST /api/v0/workspaces records who made it), or
// neither. Which session made a workspace is read from the record, never
// held: one owner per fact.
func (a *API) ownWorkspace(g run.Grant, id string) (int, error) {
	own, found, err := a.workspaceOf(g.Session)
	if err != nil || !found {
		return notOwn, err
	}
	if string(own) == id {
		return ownSession, nil
	}
	maker, err := a.cfg.Store.WorkspaceMaker(ids.WorkspaceID(id))
	if err != nil || maker != g.Session {
		return notOwn, err
	}
	ws, found, err := a.cfg.Store.Workspace(own)
	if err != nil || !found {
		return notOwn, err
	}
	if ws.Sandbox {
		return madeByCopy, nil
	}
	return madeByDirect, nil
}

// askedLinks are the links of work g's turn asks for: the request tree it is
// in, one level deeper, naming g's turn and session as who asked (Chain.By/
// BySession). That is the job's recorded origin and the caps' provenance; its
// result is delivered by an on row the ask path registers (on.go), not from
// these links.
func askedLinks(g run.Grant) queue.Links {
	return queue.Links{Chain: &queue.Chain{Root: g.Root, Depth: g.Depth + 1, By: g.Turn, BySession: g.Session}}
}

// queueJob reads a job of the default team.
func (a *API) queueJob(id string) (queue.Job, error) {
	if a.cfg.Queue == nil {
		return queue.Job{}, queue.ErrNotFound
	}
	return a.cfg.Queue.Get(queue.DefaultTeam, id)
}

// overBudget says why g's request tree may ask for no more work under
// max_cost_usd_per_tree, or "" while it may: the cost of every model call in
// the tree's sessions so far. 0 is off. A retry whose idempotency key already
// made a job is not new work, so it passes, and gets its job as before.
func (a *API) overBudget(g run.Grant, perm settings.Policy, key string) (string, error) {
	max := *perm.Limits.MaxCostUSDPerTree
	if max <= 0 || a.cfg.Queue == nil {
		return "", nil
	}
	if known, err := a.cfg.Queue.HasKey(queue.DefaultTeam, key); err != nil || known {
		return "", err
	}
	sessions, err := a.cfg.Queue.TreeSessions(queue.DefaultTeam, g.Root)
	if err != nil {
		return "", err
	}
	cost, err := a.cfg.Store.CostOf(append(sessions, g.Root, g.Session))
	if err != nil {
		return "", err
	}
	if cost >= max {
		return fmt.Sprintf("work started from one session may cost at most $%.2f in all, and it has cost $%.2f (Cost per request tree in Settings, Limits)", max, cost), nil
	}
	return "", nil
}

// inAuthority is the one rule for which sessions g reads: session is g's own,
// or its workspace is in g's authority (readsWorkspace). The request tree
// decides nothing here (his call, 2026-11-25): it held reads to one tree, so a
// Direct coordinator could not read the person's other sessions through the
// API though it reads the same store on disk, and a reviewer opened beside a
// coordinator could not read the builder's turn in the copy the coordinator
// made. Every read, search, on, rename and stop of a turn token asks this.
func (a *API) inAuthority(g run.Grant, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	if session == g.Session {
		return true, nil
	}
	ws, found, err := a.workspaceOf(session)
	if err != nil || !found {
		return false, err
	}
	return a.readsWorkspace(g, ws)
}

// readsWorkspace says ws is in g's authority. When g's own workspace works in
// the folder as it is (Direct, no wall), its authority is its machine: every
// workspace on that machine, as POST /api/v0/workspaces already treats it
// (workspaceAuthority). When g's own workspace is a copy, its authority is that
// workspace and the workspaces its session made (ownWorkspace), so work in a
// narrower copy never reaches back into the wider folders' sessions.
func (a *API) readsWorkspace(g run.Grant, ws ids.WorkspaceID) (bool, error) {
	ownID, found, err := a.workspaceOf(g.Session)
	if err != nil || !found {
		return false, err
	}
	own, found, err := a.cfg.Store.Workspace(ownID)
	if err != nil || !found {
		return false, err
	}
	if own.Sandbox {
		rel, err := a.ownWorkspace(g, string(ws))
		return rel != notOwn, err
	}
	other, found, err := a.cfg.Store.Workspace(ws)
	if err != nil || !found {
		return false, err
	}
	return a.machineOf(other) == a.machineOf(own), nil
}

// machineOf is the machine a workspace's folders are on: its connector, or this
// service's own machine when the record names none.
func (a *API) machineOf(ws store.Workspace) string {
	if ws.Machine == "" && a.cfg.Folders != nil {
		return a.cfg.Folders.Machine()
	}
	return ws.Machine
}

// askChecks checks a turn token's POST /api/v0/turns body: the asked turn runs
// in a session the asker created in its own workspace (POST /api/v0/sessions
// {workspace: the asker's}), so the turn's session must resolve to the asker's
// workspace. Then the asked agent shares the asker's folder and its sandbox
// wall by construction, with no folder on the turn. It says why not, and has
// then answered.
func (a *API) askChecks(w http.ResponseWriter, g run.Grant, perm settings.Policy, body *TurnRequest) (queue.Links, bool) {
	refuse := func(msg string) (queue.Links, bool) {
		writeError(w, http.StatusForbidden, msg)
		return queue.Links{}, false
	}
	if msg, err := a.overBudget(g, perm, body.IdempotencyKey); err != nil {
		a.internal(w)
		return queue.Links{}, false
	} else if msg != "" {
		return refuse(msg)
	}
	if body.Session == "" {
		return refuse(turnNoSession)
	}
	// A turn token may start a turn only in a session its own tree opened (POST
	// /api/v0/sessions set the asker on it): the asker names this token's
	// session. This is the one check, and it implies the workspace, since the
	// session was created in the asker's workspace. It is not the asker's own
	// session (opened by the person, no asker) nor a sibling's (a different
	// asker), so a turn token neither continues a conversation nor writes into
	// another's -- the boundary the shared workspace would otherwise lose.
	asker, found, err := a.cfg.Store.SessionAsker(body.Session)
	if err != nil {
		a.internal(w)
		return queue.Links{}, false
	}
	if !found || asker != g.Session {
		return refuse(turnSessionRefused)
	}
	// An ask is one turn: a turn token opens a session and starts its single
	// turn. A second turn to the same asked session is refused as a continuation
	// inside the create transaction (queue.ErrAskedSessionUsed), so two asks
	// racing to one session cannot both pass; run() maps it to the same 403.
	return askedLinks(g), true
}
