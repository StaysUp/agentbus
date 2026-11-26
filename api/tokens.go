package api

// Turn tokens. POST /api/v0/turns gives each turn's child its own token in
// AGENTBUS_TOKEN, with the service's address in AGENTBUS_URL. With it the
// agent does what its owner may, narrowed by three things only (O2.3, his
// call 2026-11-25; authority.go): where it works, the owner's brakes, and
// never changing its own brakes. Each result it asks for comes back to a
// session as a later turn, delivered by an on row the ask path registers
// (queue/on.go, FireOns).
//
// What a turn token never does is the third: change the settings, the saved
// agents or the tools, see or make tokens (none is ever in a reply), follow a
// sign-in, or answer an approval (run.ActAdmin).
//
// One rule decides where a turn token works (inAuthority, readsWorkspace):
// the sessions of the workspaces in its authority. A Direct session's
// authority is its machine, since it works in the folders with no wall and can
// read the same store on disk. A Copy session's is its own workspace and the
// workspaces its session made. Reads, search, asks, ons, rename, archive,
// stop, and a workspace's discard, update, close and apply all ask it.
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
	turnCallRefused = "a turn token reads only the calls of the sessions of the workspaces in its authority"
	turnStopRefused = "a turn token stops only turns in the sessions it may read"
	turnNoSession   = "a turn needs a session: create one in your workspace with POST /api/v0/sessions, then run its turn"
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

// queueLimits are [limits] as the queue takes them. The queue checks every
// one of them where it makes a job (queue/budget.go), whichever path asks.
func queueLimits(p settings.Policy) queue.Limits {
	return queue.Limits{MaxDepth: *p.Limits.MaxDepth, MaxPerTurn: *p.Limits.MaxJobsPerTurn, MaxPerTree: *p.Limits.MaxJobsPerTree,
		MaxCostUSD: *p.Limits.MaxCostUSDPerTree}
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

// askChecks checks a turn token's POST /api/v0/turns body: the turn runs in a
// session in the asker's reach (inAuthority, limit 1), a new one or one that
// already ran, its own too. The caps (limit 2: depth, jobs, cost) are the
// queue's, checked where it makes the job (queue/budget.go). The
// turn runs in its session's workspace, so it shares that workspace's folder
// and sandbox wall by construction. It says why not, and has then answered.
func (a *API) askChecks(w http.ResponseWriter, g run.Grant, body *TurnRequest) (queue.Links, bool) {
	refuse := func(msg string) (queue.Links, bool) {
		writeError(w, http.StatusForbidden, msg)
		return queue.Links{}, false
	}
	if body.Session == "" {
		return refuse(turnNoSession)
	}
	in, err := a.inAuthority(g, body.Session)
	if err != nil {
		a.internal(w)
		return queue.Links{}, false
	}
	if !in {
		return refuse(turnSessionRefused)
	}
	return askedLinks(g), true
}
