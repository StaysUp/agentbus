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
// asked for from it, at any depth (queue.Chain). route.turn says what a turn
// token may do on each route; the zero value refuses it.
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
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// What a turn token may do on a route.
const (
	// mainOnly: a turn token is refused. The zero value, so a new route is
	// closed to turn tokens until it says otherwise.
	mainOnly = iota
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
	// handler checks it is a turn the token's tree started and the delivery
	// session is the token's own (on.go).
	onRegister
	// onPath: DELETE /api/v0/ons/{id}. The handler checks the on's maker is
	// this token's turn (on.go).
	onPath
	// fileOp: POST /api/v0/files/read, list, write.
	fileOp
	// createSessionTok: POST /api/v0/sessions. A turn token may create a
	// session only in its own workspace; the handler sets the workspace from
	// the grant and ignores the body's, so an ask opens a session beside the
	// asker, in the same copy.
	createSessionTok
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

// grantKey carries a turn token's grant from route to its handler.
type grantKey struct{}

// turnCall is a turn token's grant with the policy read for its request.
type turnCall struct {
	g    run.Grant
	perm settings.Policy
}

// grantOf is the grant of the turn token r was sent with, or nil.
func grantOf(r *http.Request) *run.Grant {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil {
		return nil
	}
	return &c.g
}

// policyOf is the permissions.toml read for r's turn token. Only a request
// grantOf answers for has one.
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

// allowTurn checks a turn token's request on rt, and says why not. It may set
// the session query to the token's own (treeSessionQuery). The request it
// returns carries the grant, for the handler.
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
	apply := perm.AgentAPI.Apply != nil && *perm.AgentAPI.Apply
	if msg := turnRefusal(rt, rules, apply); msg != "" {
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
	switch rt.turn {
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
		session, err := a.sessionOfTurn(id, session)
		if err != nil {
			a.internal(w)
			return nil, false
		}
		if session == "" {
			writeError(w, http.StatusNotFound, noTurn)
			return nil, false
		}
		if ok, err := inScope(session); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case changesWorkspaceTok, applyWorkspaceTok:
		// A turn token reads or applies only its OWN workspace: the workspace of
		// the token's session must be the id in the path. g.Session is always in
		// the token's tree, so inScope is not needed.
		ws, found, err := a.workspaceOf(g.Session)
		if err != nil {
			a.internal(w)
			return nil, false
		}
		if !found || string(ws) != id {
			return refuse(turnSessionRefused)
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
	case askTurn, anyRead, toolList, onRegister, onPath, fileOp, createSessionTok:
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

// turnRefusal says why a turn token may not call a route under the person's
// rules and switches, or "": applyWorkspace and tools. Reading, listing agents, starting,
// reviewing, stopping and renaming have no switch: within its request tree a
// turn token always may (0.5.2). The rest of the check needs the request
// (allowTurn). The tool list (tools.go) asks it too, so a tool is listed only
// where its route would take the call.
func turnRefusal(rt *route, rules gateway.Rules, applyAllowed bool) string {
	toolDec, _ := gateway.DecideTool(rules, "tool:*")
	switch {
	case rt.turn == mainOnly:
		return turnWriteRefused
	case rt.turn == applyWorkspaceTok && !applyAllowed:
		return turnSwitchedOff("apply a workspace's changes", "Agents may apply")
	case (rt.turn == toolList || rt.turn == fileOp) && toolDec == gateway.Refuse:
		return turnSwitchedOff("use the agentbus MCP tools", "AgentBus tools")
	}
	return ""
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

// inTree says session is in g's request tree: its own session, the tree's
// root, or a session work in the tree started.
func (a *API) inTree(g run.Grant, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	if session == g.Session || session == g.Root {
		return true, nil
	}
	if a.cfg.Queue == nil {
		return false, nil
	}
	root, err := a.cfg.Queue.SessionRoot(queue.DefaultTeam, session)
	return err == nil && root != "" && root == g.Root, err
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
