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
// ever in a reply), close sessions, write program events, or read or write
// sessions outside its request tree. Its own session and the person's session,
// the tree's root, are never continued by it: results reach them by delivery
// only.
//
// A request tree is the person's session (its root) and every session work
// asked for from it, at any depth (queue.Chain).
//
// Every token resolves to a grant (run.Grant: workspaces, actions, expiry) at
// one door (api.go, authorized), and every route names the one action class it
// needs (route.action). A turn token's grant holds the turn's actions in its own
// workspaces; route.scope says how each request is confined to them, and the
// zero value refuses it. Its own workspaces are its session's and those its
// session made: it makes a workspace only within its authority
// (workspaceAuthority, workspaces.go).
//
// The engine mints and keeps the tokens (package run), in memory only, as
// their SHA-256, and revokes each when its turn's child exits.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// What a turn token may do on a route.
const (
	// scopeNone: a turn token is refused. The zero value, so a new route is
	// closed to turn tokens until it says otherwise.
	scopeNone = iota
	// treeSessionPath: {id} is a session id in the token's request tree.
	treeSessionPath
	// treeSessionQuery: the session query must name a session in the
	// token's request tree. Absent, it is set to the token's own, so the
	// reply holds that session only.
	treeSessionQuery
	// treeTurn: {id} is a turn of a session in the token's request tree:
	// the session query's when sent, else the turn's newest.
	treeTurn
	// askTurn: POST /api/v0/turns. The handler checks the rest (askChecks).
	askTurn
	// askedTurn: {id} is a turn that work in the token's request tree asked
	// for, to stop.
	askedTurn
	// anyRead: a read that holds no session's data, such as the saved
	// agents.
	anyRead
	// renameSession: {id} is a session in the token's scope, to rename.
	renameSession
	// changesWorkspaceTok: {id} is the token's own workspace, whose changes to
	// preview.
	changesWorkspaceTok
	// applyWorkspaceTok: {id} is the token's own workspace, whose changes to
	// apply, allowed only when Agents may apply is on.
	applyWorkspaceTok
	// toolList: POST /api/v0/mcp, the API as tools. Each tool call is sent
	// through its own route and checked there (tools.go).
	toolList
	// onRegister: POST /api/v0/ons. The subject turn is in the body; the
	// handler checks the token may read it (mayReadTurn) and delivers to the
	// token's own session (on.go).
	onRegister
	// onPath: DELETE /api/v0/ons/{id}. The handler checks the on's maker is
	// this token's turn (on.go).
	onPath
	// fileOp: POST /api/v0/files/read, list, write.
	fileOp
	// createSessionTok: POST /api/v0/sessions. A turn token may create a
	// session in its own workspace (the default, so an ask opens a session
	// beside the asker, in the same copy) or in a workspace its session made.
	createSessionTok
	// workspaceTok: {id} is one of the token's own workspaces (ownWorkspace):
	// its session's, or one its session made.
	workspaceTok
	// createWorkspaceTok: POST /api/v0/workspaces. The handler checks the
	// new workspace is within the token's authority (workspaceAuthority).
	createWorkspaceTok
)

// Refusals a turn token gets. Fixed wording: they never quote the request.
const (
	turnWriteRefused = "a turn token cannot call this route; it takes the main token. " +
		"A turn token reads its request tree's sessions and turns, lists agents, " +
		"and asks for work with POST /api/v0/turns"
	turnSessionRefused  = "a turn token reads only its own session and the sessions its request tree started"
	turnCallRefused     = "a turn token reads only the calls of its own session and the sessions its request tree started"
	turnContinueRefused = "a turn token does not continue sessions: ask_agent starts a new one, and its result comes back to your session " +
		"as a later turn; to follow up, ask again with what the result said"
	turnAskedRefused = "a turn token stops only turns its request tree asked for"
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

// grantKey carries a request's grant from route to its handler.
type grantKey struct{}

// turnCall is a request's grant with the policy read for it.
type turnCall struct {
	g    run.Grant
	perm settings.Policy
}

// grantOf is the grant of r's token when it is bound to a request tree (an
// agent turn's own token), or nil for a grant of every workspace. Handlers ask
// it whether to confine a request to the token's tree.
func grantOf(r *http.Request) *run.Grant {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil || !c.g.Bound() {
		return nil
	}
	return &c.g
}

// tokenGrant is the grant of r's token, whatever the token.
func tokenGrant(r *http.Request) run.Grant {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil {
		return run.Grant{}
	}
	return c.g
}

// policyOf is the permissions.toml read for r's turn token. A grant of every
// workspace gets the defaults.
func policyOf(r *http.Request) settings.Policy {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil {
		return settings.DefaultPolicy()
	}
	return c.perm
}

// policy is the policy for one turn token's request. Where this service runs
// turns, its folders must be folders here, as a turn's are: a policy a turn
// could not start under refuses its token's requests too.
func (a *API) policy() (settings.Policy, error) {
	if a.cfg.Settings == nil {
		return settings.DefaultPolicy(), nil
	}
	p, _, err := a.cfg.Settings.Policy()
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

// allow is the one door: it checks r's grant against rt, and says why not. The
// grant must hold the route's action class; a grant bound to a request tree is
// then confined to it (allowTurn). The request it returns carries the grant, for
// the handler.
func (a *API) allow(w http.ResponseWriter, r *http.Request, rt *route, id string, g run.Grant) (*http.Request, bool) {
	if g.Bound() {
		return a.allowTurn(w, r, rt, id, g)
	}
	if !g.Actions.Has(rt.action) || (g.TurnsOnly && !rt.turns) {
		writeError(w, http.StatusForbidden, turnsRefused)
		return nil, false
	}
	ctx := context.WithValue(r.Context(), grantKey{}, &turnCall{g: g, perm: settings.DefaultPolicy()})
	return r.WithContext(ctx), true
}

// allowTurn checks a request of a grant bound to a request tree on rt, and
// says why not. It may set the session query to the token's own
// (treeSessionQuery). The request it returns carries the grant, for the
// handler.
func (a *API) allowTurn(w http.ResponseWriter, r *http.Request, rt *route, id string, g run.Grant) (*http.Request, bool) {
	refuse := func(msg string) (*http.Request, bool) {
		writeError(w, http.StatusForbidden, msg)
		return nil, false
	}
	failed := func(err error) (*http.Request, bool) {
		if errors.Is(err, queue.ErrNotFound) {
			writeError(w, http.StatusNotFound, noTurn)
		} else {
			a.internal(w)
		}
		return nil, false
	}
	// The policy is read for every request, so an edit applies at once. A
	// file that cannot be used refuses every turn token's request: a token
	// never runs without the policy the person wrote.
	perm, err := a.policy()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, turnPolicyBroken(err))
		return nil, false
	}
	rules := gateway.Rules{Allow: perm.Allow, Ask: perm.Ask, Forbid: perm.Forbid}
	if msg := turnRefusal(rt, g.Actions, rules); msg != "" {
		return refuse(msg)
	}
	// B15 (his call, 2026-11-19): no leaf rule. A review is a normal turn, so
	// there is no "is this a review" branch here; the ordinary asked-work caps
	// (depth, jobs, cost) bound it like any asked turn. The old IsReview leaf
	// check retired with the review kind.
	//
	// A turn token reaches its own request tree, always: its own session,
	// the tree's root and every session work in the tree started. Start and
	// continue are told apart by the body: askChecks.
	inScope := func(session string) (bool, error) { return a.inTree(g, session) }
	switch rt.scope {
	case treeSessionPath, renameSession:
		if ok, err := inScope(id); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case treeSessionQuery:
		q := r.URL.Query()
		if vs, ok := q["session"]; ok {
			if len(vs) != 1 {
				return refuse(turnSessionRefused)
			}
			if in, err := inScope(vs[0]); err != nil {
				return failed(err)
			} else if !in {
				return refuse(turnSessionRefused)
			}
			break
		}
		q.Set("session", g.Session)
		r.URL.RawQuery = q.Encode()
	case treeTurn:
		session := ""
		if q := r.URL.Query(); q.Has("session") {
			if vs := q["session"]; len(vs) != 1 {
				return refuse(turnSessionRefused)
			}
			session = q.Get("session")
		}
		found, ok, err := a.mayReadTurn(g, id, session)
		switch {
		case err != nil:
			return failed(err)
		case !found:
			writeError(w, http.StatusNotFound, noTurn)
			return nil, false
		case !ok:
			return refuse(turnSessionRefused)
		}
	case changesWorkspaceTok, workspaceTok, applyWorkspaceTok:
		// A turn token reads or applies only one of its OWN workspaces: its
		// session's, or one its session made.
		own, err := a.ownWorkspace(g, id)
		if err != nil {
			a.internal(w)
			return nil, false
		}
		if own == notOwn {
			return refuse(turnSessionRefused)
		}
		// Apply adds no power only where the token could already write the
		// folders: a workspace its Direct session made (Direct has no wall).
		// Its own workspace, or one a session in a copy made, takes the
		// person's switch.
		applyOn := perm.AgentAPI.Apply != nil && *perm.AgentAPI.Apply
		if rt.scope == applyWorkspaceTok && !applyOn && own != madeByDirect {
			return refuse(turnSwitchedOff("apply a workspace's changes", "Agents may apply"))
		}
	case askedTurn:
		if a.cfg.Queue == nil {
			return failed(queue.ErrNotFound)
		}
		j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		if err != nil {
			return failed(err)
		}
		if j.Links.Chain == nil || j.Links.Chain.By == "" || j.Links.Chain.Root != g.Root {
			return refuse(turnAskedRefused)
		}
		// The same reach as a read: the asked turn's session must be in one of
		// the token's own workspaces, so a child in a narrower copy cannot stop
		// work in its parent's folders.
		if ok, err := a.inTree(g, string(j.Spec.Session)); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnAskedRefused)
		}
	case askTurn, anyRead, toolList, onRegister, onPath, fileOp, createSessionTok, createWorkspaceTok:
		// The handler checks the rest: onRegister reads the subject turn from
		// the body and the session it delivers to; onPath reads the on's maker;
		// createSession sets the workspace from the grant.
	default:
		return refuse(turnWriteRefused)
	}
	// B15: the review route's own budget gate retired with it. A review is now a
	// normal asked turn, so its cost cap is applied by the startTurn path (the
	// app's first step), overBudget in askChecks, like any ask_agent turn.
	ctx := context.WithValue(r.Context(), grantKey{}, &turnCall{g: g, perm: perm})
	return r.WithContext(ctx), true
}

// turnRefusal says why a turn token may not call a route under its actions and
// the person's rules, or "". A route that cannot be confined to a request tree
// (scopeNone) is refused whatever the actions. Reading, listing agents,
// starting, reviewing, stopping and renaming have no switch: within its request
// tree a turn token always may (0.5.2). The Agents may apply switch needs the
// workspace, so allowTurn checks it. The rest of the check needs the request
// (allowTurn). The tool list (tools.go) asks it too, so a tool is listed only
// where its route would take the call.
func turnRefusal(rt *route, actions run.Action, rules gateway.Rules) string {
	toolDec, _ := gateway.DecideTool(rules, "tool:*")
	switch {
	case rt.scope == scopeNone || !actions.Has(rt.action):
		return turnWriteRefused
	case (rt.scope == toolList || rt.scope == fileOp) && toolDec == gateway.Refuse:
		return turnSwitchedOff("use the agentbus MCP tools", "AgentBus tools")
	}
	return ""
}

// mayReadTurn is the one check of whether g may read turn id: the turn's
// session (session when sent, else the turn's newest) is in g's reach
// (inTree). found is false when there is no such turn. Reading a turn
// (treeTurn) and watching one (POST /api/v0/ons) both ask it, so what a turn
// token may watch is what it may read, and a wider read widens both.
func (a *API) mayReadTurn(g run.Grant, id, session string) (found, ok bool, err error) {
	session, err = a.sessionOfTurn(id, session)
	if err != nil || session == "" {
		return false, false, err
	}
	ok, err = a.inTree(g, session)
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

// inTree says g reaches session: it is in g's request tree (its own session,
// the tree's root, or a session work in the tree started) AND in one of g's own
// workspaces (ownWorkspace). The tree alone was the reach while every session
// of a tree shared one workspace; a session may now make a narrower copy and
// run work there, and that work must not reach back into the wider folders'
// sessions (their recorded tool output, their turns). Every read, rename and
// stop of a turn token asks this one check.
func (a *API) inTree(g run.Grant, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	if session == g.Session {
		return true, nil
	}
	if session != g.Root {
		if a.cfg.Queue == nil {
			return false, nil
		}
		root, err := a.cfg.Queue.SessionRoot(queue.DefaultTeam, session)
		if err != nil || root == "" || root != g.Root {
			return false, err
		}
	}
	ws, found, err := a.workspaceOf(session)
	if err != nil || !found {
		return false, err
	}
	own, err := a.ownWorkspace(g, string(ws))
	return own != notOwn, err
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
