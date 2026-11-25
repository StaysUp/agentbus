package api

// The one check. Every token resolves to a grant at one door (api.go,
// authorized): its owner's authority, minus the limits the owner set for the
// agent it acts through (SOUL v5 principle 8). Every route names the one class
// it needs (route.action) and where it acts (route.at). The door (allow) asks
// the grant one question, may this action happen in this place
// (run.Grant.May), and the grant answers. No route branches on what kind of
// token it was sent.
//
// The owner's own token holds every class everywhere and has no limits
// (noLimits). The turns token holds its fixed classes everywhere, on the routes
// that say so (turnsLimits). An agent turn's own token holds the owner's
// classes but admin and copy, in the reach its limits leave (agentLimits).
// Those limits are in code today (tokens.go, workspaces.go); each is to become
// an owner setting with today's value as its default (O2.3).
//
// An agent turn's limits are decided by the rules the turn took when it
// started (rulesOf), the same rules its commands and tools are held to at the
// gateway, so a change of the permissions applies from the turn after (O2.4).

import (
	"context"
	"errors"
	"net/http"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// Where a route acts (route.at), so the door can ask the grant about it.
const (
	// atControl: control's state as a whole, every workspace, such as the
	// list of all sessions, the event stream, the approvals or the settings.
	// The zero value, so a new route is for a grant of every workspace until
	// it says where it acts.
	atControl = iota
	// atNothing: no workspace's data, such as the saved agents.
	atNothing
	// atSession: {id} is a session.
	atSession
	// atSessionQuery: the session query names the session. Absent, the
	// request is on every session, and a grant that does not reach every
	// workspace is narrowed to its own session: the query is set to it.
	atSessionQuery
	// atTurn: {id} is a turn, of the session query's session when sent, else
	// of the turn's newest.
	atTurn
	// atJob: {id} is a turn with a job, in the job's session, to stop.
	atJob
	// atWorkspace: {id} is a workspace.
	atWorkspace
	// atBody: the body names the place, and the handler asks the grant:
	// start a turn, ons, the files, the tool list, a new session or workspace.
	atBody
)

// callerKey carries a request's caller from the door to its handler.
type callerKey struct{}

// caller is a request's grant as the door resolved it, with the limits it
// carries and the rules it is held to.
type caller struct {
	run.Grant
	limits limits
	// rules are the permissions the grant's turn took at its start; the
	// defaults for a grant no rule holds (the owner's own token).
	rules settings.Policy
}

// callerOf is r's caller. A request the door did not pass holds no grant: it
// may do nothing.
func callerOf(r *http.Request) *caller {
	if c, ok := r.Context().Value(callerKey{}).(*caller); ok {
		return c
	}
	return &caller{limits: noLimits{}, rules: settings.DefaultPolicy()}
}

// actor is who acts through c, as the record names it: the agent's turn, or
// the owner when c is the owner's own hand.
func (c *caller) actor() *queue.By {
	if c.Turn == "" {
		return &queue.By{Kind: event.ByPerson}
	}
	return &queue.By{Kind: event.ByAgent, Session: c.Session, Turn: c.Turn}
}

// origin is the origin of what c makes (a session, a workspace): the agent's
// turn that made it, or none for the owner's own.
func (c *caller) origin() *event.Origin {
	if c.Turn == "" {
		return nil
	}
	return &event.Origin{Kind: event.OriginAsked, Session: c.Session, Turn: c.Turn}
}

// toolRules are c's allow, ask and forbid lists, as the tool gate reads them.
func (c *caller) toolRules() gateway.Rules {
	return gateway.Rules{Allow: c.rules.Allow, Ask: c.rules.Ask, Forbid: c.rules.Forbid}
}

// lists says a tool on rt is in the list c sees: a route c's grant may call,
// with no place known yet. Only discovery: the route decides each call.
func (c *caller) lists(rt *route) bool {
	if c.limits.route(rt, c.rules) != "" || !c.Actions.Has(rt.action) {
		return false
	}
	if rt.action == run.ActApply && !c.limits.appliesAnywhere(c.rules) {
		return false
	}
	if every, _ := c.May(rt.action, run.Everywhere); every {
		return true
	}
	return rt.at != atControl
}

// limits are the limits the owner set on whoever acts with a grant, beyond the
// grant's classes: its reach, and what a request's body may ask of it.
type limits interface {
	run.Reach
	// route says why the grant may not call rt at all under rules, or "".
	route(rt *route, rules settings.Policy) string
	// refusal is the 403 for a class the grant does not hold.
	refusal() string
	// gated says the grant's tool calls are held to its rules (toolgate.go).
	gated() bool
	// appliesAnywhere says the grant may apply some workspace's changes
	// under rules, for the tool list.
	appliesAnywhere(rules settings.Policy) bool
	// ask checks a POST /api/v0/turns body, and gives the links of the job
	// it makes. ok false has answered.
	ask(a *API, w http.ResponseWriter, rules settings.Policy, body *TurnRequest) (links queue.Links, ok bool)
	// deliver is where a turn's reply goes when the body names to. ok false
	// has answered.
	deliver(w http.ResponseWriter, to string) (d *queue.Delivery, ok bool)
	// watch checks a POST /api/v0/ons body for subject turn j, and gives
	// where the reply goes, who made the on and the asked work it counts as.
	watch(a *API, w http.ResponseWriter, j queue.Job, body *OnRequest) (session string, maker queue.Maker, asked *queue.Chain, ok bool)
	// removeOn checks the grant may remove on id. ok false has answered.
	removeOn(a *API, w http.ResponseWriter, id string) bool
	// stops says why the grant may not stop the turn of job j, or "".
	stops(j queue.Job) string
	// archives says why the grant may not archive a session, or "".
	archives() string
	// makes checks a workspace the body asks for. ok false has answered.
	makes(a *API, w http.ResponseWriter, body *WorkspaceRequest) bool
	// home is the workspace a session opens in when the body names none, or
	// "" when the grant has none. ok false has answered.
	home(a *API, w http.ResponseWriter) (ids.WorkspaceID, bool)
	// folder is the folder a file tool may open resolved in, and the rules
	// the tool is held to (nil: none). nearest says the folder is the nearest
	// one there of resolved, a file a person allowed (files.go finds it). ok
	// false has answered.
	folder(a *API, w http.ResponseWriter, r *http.Request, resolved, path string) (grantFolder string, nearest bool, rules *gateway.Rules, ok bool)
}

// noLimits are the owner's own: none.
type noLimits struct{}

func (noLimits) Holds(run.Action, run.Where) (bool, error) { return true, nil }
func (noLimits) route(*route, settings.Policy) string      { return "" }
func (noLimits) refusal() string                           { return turnWriteRefused }
func (noLimits) gated() bool                               { return false }
func (noLimits) appliesAnywhere(settings.Policy) bool      { return true }
func (noLimits) ask(*API, http.ResponseWriter, settings.Policy, *TurnRequest) (queue.Links, bool) {
	return queue.Links{}, true
}
func (noLimits) deliver(_ http.ResponseWriter, to string) (*queue.Delivery, bool) {
	return &queue.Delivery{Session: to}, true
}
func (noLimits) watch(_ *API, w http.ResponseWriter, _ queue.Job, body *OnRequest) (string, queue.Maker, *queue.Chain, bool) {
	if body.Session == "" {
		writeError(w, http.StatusBadRequest, "session is required: where to deliver the turn's reply")
		return "", queue.Maker{}, nil, false
	}
	return body.Session, queue.Maker{}, nil, true
}
func (noLimits) removeOn(*API, http.ResponseWriter, string) bool { return true }
func (noLimits) stops(queue.Job) string                          { return "" }
func (noLimits) archives() string                                { return "" }
func (noLimits) makes(*API, http.ResponseWriter, *WorkspaceRequest) bool {
	return true
}
func (noLimits) home(*API, http.ResponseWriter) (ids.WorkspaceID, bool) { return "", true }

// folder is the whole disk: the owner's file routes name any path, under no
// rule.
func (noLimits) folder(*API, http.ResponseWriter, *http.Request, string, string) (string, bool, *gateway.Rules, bool) {
	return "/", false, nil, true
}

// turnsLimits are the turns token's: only the routes that say so.
type turnsLimits struct{ noLimits }

func (turnsLimits) route(rt *route, _ settings.Policy) string {
	if !rt.turns {
		return turnsRefused
	}
	return ""
}
func (turnsLimits) refusal() string { return turnsRefused }

// agentLimits are an agent turn's: the limits of tokens.go, read under the
// rules the turn took at its start.
type agentLimits struct {
	a     *API
	g     run.Grant
	rules settings.Policy
}

// Holds is the agent's reach. A session is in it when it is the agent's own
// or its workspace is in the agent's authority (inAuthority). A workspace's
// own routes reach the agent's own workspaces (ownWorkspace): its session's
// and those its session made. Applying one takes the Agents may apply switch,
// but in a workspace its Direct session made, where it can already write.
// Every workspace is never in it.
func (l agentLimits) Holds(act run.Action, w run.Where) (bool, error) {
	switch {
	case w.Every():
		return false, nil
	case w.Workspace != "":
		own, err := l.a.ownWorkspace(l.g, string(w.Workspace))
		if err != nil || own == notOwn {
			return false, err
		}
		if act == run.ActApply && own != madeByDirect && !l.applyOn() {
			return false, refused(turnSwitchedOff("apply a workspace's changes", "Agents may apply"))
		}
		return true, nil
	}
	return l.a.inAuthority(l.g, w.Session)
}

// applyOn is the Agents may apply switch, as the turn's rules say.
func (l agentLimits) applyOn() bool { return applyOn(l.rules) }

func applyOn(p settings.Policy) bool { return p.AgentAPI.Apply != nil && *p.AgentAPI.Apply }

func (agentLimits) route(rt *route, rules settings.Policy) string {
	if rt.toolsSwitch {
		if dec, _ := gateway.DecideTool(gateway.Rules{Allow: rules.Allow, Ask: rules.Ask, Forbid: rules.Forbid}, "tool:*"); dec == gateway.Refuse {
			return turnSwitchedOff("use the agentbus MCP tools", "AgentBus tools")
		}
	}
	return ""
}
func (agentLimits) refusal() string                            { return turnWriteRefused }
func (agentLimits) gated() bool                                { return true }
func (agentLimits) appliesAnywhere(rules settings.Policy) bool { return applyOn(rules) }

func (l agentLimits) ask(a *API, w http.ResponseWriter, rules settings.Policy, body *TurnRequest) (queue.Links, bool) {
	return a.askChecks(w, l.g, rules, body)
}

func (l agentLimits) deliver(w http.ResponseWriter, to string) (*queue.Delivery, bool) {
	if to != l.g.Session {
		writeError(w, http.StatusForbidden, "a turn token may deliver only to its own session")
		return nil, false
	}
	return &queue.Delivery{Session: l.g.Session, Maker: queue.Maker{Session: l.g.Session, Turn: l.g.Turn}}, true
}

// watch: an agent may watch any turn it may read (mayReadTurn) but one of its
// own session, whose reply delivered to that same session would be a
// self-continuation. The reply goes to its own session, and the on counts as
// asked work by its turn under the caps (queue.countAsked): its delivered turn
// is not asked work, so without this a chain of ons over ended turns would
// pass the caps.
func (l agentLimits) watch(a *API, w http.ResponseWriter, j queue.Job, body *OnRequest) (string, queue.Maker, *queue.Chain, bool) {
	found, ok, err := a.mayReadTurn(l.g, body.Turn, "")
	switch {
	case err != nil:
		a.internal(w)
	case !found:
		writeError(w, http.StatusNotFound, noTurn)
	case !ok:
		writeError(w, http.StatusForbidden, turnOnRefused)
	case turnSession(j) == l.g.Session:
		writeError(w, http.StatusForbidden, turnOnSelfRefused)
	default:
		return l.g.Session, queue.Maker{Session: l.g.Session, Turn: l.g.Turn}, askedLinks(l.g).Chain, true
	}
	return "", queue.Maker{}, nil, false
}

// removeOn: an agent removes only an on its own turn made.
func (l agentLimits) removeOn(a *API, w http.ResponseWriter, id string) bool {
	on, err := a.cfg.Queue.GetOn(queue.DefaultTeam, id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noOn)
		return false
	case err != nil:
		a.internal(w)
		return false
	}
	if on.Maker.Turn != l.g.Turn || on.Maker.Session != l.g.Session {
		writeError(w, http.StatusForbidden, turnOnRemoveRefused)
		return false
	}
	return true
}

// stops: asked work only. A turn the person started is the person's to stop.
func (agentLimits) stops(j queue.Job) string {
	if j.Links.Chain == nil || j.Links.Chain.By == "" {
		return turnAskedRefused
	}
	return ""
}

func (agentLimits) archives() string {
	return "a turn token cannot archive a session; only the person can"
}

func (l agentLimits) makes(a *API, w http.ResponseWriter, body *WorkspaceRequest) bool {
	return a.workspaceAuthority(w, l.g, body)
}

// home: an ask opens its session beside the asker, in its own workspace.
func (l agentLimits) home(a *API, w http.ResponseWriter) (ids.WorkspaceID, bool) {
	ws, found, err := a.cfg.Store.WorkspaceOf(ids.SessionID(l.g.Session))
	if err != nil {
		a.internal(w)
		return "", false
	}
	if !found {
		writeError(w, http.StatusForbidden, "a turn token has no workspace to create a session in")
		return "", false
	}
	return ws, true
}

// folder is where an agent's file tool may open resolved: within a folder the
// person granted its session before the turn started ("always for this
// folder"), or where a person allowed this held call. A grant made during the
// turn applies from the next (O2.4). Its tool is held to the turn's rules.
func (l agentLimits) folder(a *API, w http.ResponseWriter, r *http.Request, resolved, path string) (string, bool, *gateway.Rules, bool) {
	rules := &gateway.Rules{Allow: l.rules.Allow, Ask: l.rules.Ask, Forbid: l.rules.Forbid}
	if gf, ok, err := a.cfg.Store.MatchingFolderGrant(l.g.Session, resolved, l.g.RulesAt); err == nil && ok {
		return gf, false, rules, true
	}
	approved, held := r.Context().Value(toolGatedKey{}).(string)
	switch {
	case !held || approved == "" || approved == "*":
		writeError(w, http.StatusForbidden, "folder access not granted for this session: "+path)
	case approved != resolved:
		writeError(w, http.StatusForbidden, "path changed to ungranted target while approval was pending: "+path)
	default:
		return resolved, true, rules, true
	}
	return "", false, nil, false
}

// refusal is a place a reach refuses with its own words, such as a switch
// that is off.
type refusal struct{ msg string }

func (e *refusal) Error() string { return e.msg }

func refused(msg string) error { return &refusal{msg} }

// callerFor is the caller grant g makes: its limits, and the rules it is held
// to. An agent turn's reach is its limits, worked out here, once per request,
// from the record and the rules its turn took: ForTurn leaves it to the door,
// so a grant with no reach gets an agent's limits. A grant that reaches every
// workspace (the owner's, the turns token's) has none but its own. err is why
// the rules cannot be used.
func (a *API) callerFor(g run.Grant) (*caller, error) {
	c := &caller{Grant: g, rules: settings.DefaultPolicy(), limits: noLimits{}}
	switch {
	case g.TurnsOnly:
		c.limits = turnsLimits{}
	case g.Reach == nil:
		p, err := a.rulesOf(g)
		if err != nil {
			return nil, err
		}
		lim := agentLimits{a: a, g: g, rules: p}
		c.rules, c.limits, c.Reach = p, lim, lim
	}
	return c, nil
}

// allow is the one door: it asks r's grant whether it may do rt's action
// where rt acts, and says why not. A grant that may do the action in every
// workspace needs no place. The request it returns carries the caller, for
// the handler. It may set the session query to the caller's own
// (atSessionQuery).
func (a *API) allow(w http.ResponseWriter, r *http.Request, rt *route, id string, g run.Grant) (*http.Request, bool) {
	c, err := a.callerFor(g)
	if err != nil {
		// A file that cannot be used refuses every turn token's request: a
		// token never runs without the policy the person wrote.
		writeError(w, http.StatusServiceUnavailable, turnPolicyBroken(err))
		return nil, false
	}
	if msg := c.limits.route(rt, c.rules); msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return nil, false
	}
	if !c.Actions.Has(rt.action) {
		writeError(w, http.StatusForbidden, c.limits.refusal())
		return nil, false
	}
	every, err := c.May(rt.action, run.Everywhere)
	if err != nil {
		a.internal(w)
		return nil, false
	}
	if !every && !a.within(w, r, rt, id, c) {
		return nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, c)), true
}

// within finds where request r on rt acts, asks c whether it may do the
// route's action there, and says why not.
func (a *API) within(w http.ResponseWriter, r *http.Request, rt *route, id string, c *caller) bool {
	refuse := func(msg string) bool {
		writeError(w, http.StatusForbidden, msg)
		return false
	}
	failed := func(err error) bool {
		if errors.Is(err, queue.ErrNotFound) {
			writeError(w, http.StatusNotFound, noTurn)
		} else {
			a.internal(w)
		}
		return false
	}
	var where run.Where
	outside := turnSessionRefused
	switch rt.at {
	case atControl:
		where, outside = run.Everywhere, turnWriteRefused
	case atNothing, atBody:
		// atBody: the handler finds the place in the body and asks c.
	case atSession:
		where = run.InSession(id)
	case atSessionQuery:
		q := r.URL.Query()
		vs, sent := q["session"]
		switch {
		case sent && len(vs) != 1:
			return refuse(turnSessionRefused)
		case sent:
			where = run.InSession(vs[0])
		case c.Session != "":
			q.Set("session", c.Session)
			r.URL.RawQuery = q.Encode()
			where = run.InSession(c.Session)
		default:
			where = run.Everywhere
		}
	case atTurn:
		session := ""
		if q := r.URL.Query(); q.Has("session") {
			if vs := q["session"]; len(vs) != 1 {
				return refuse(turnSessionRefused)
			}
			session = q.Get("session")
		}
		s, err := a.sessionOfTurn(id, session)
		switch {
		case err != nil:
			return failed(err)
		case s == "":
			writeError(w, http.StatusNotFound, noTurn)
			return false
		}
		where = run.InSession(s)
	case atJob:
		if a.cfg.Queue == nil {
			return failed(queue.ErrNotFound)
		}
		j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		if err != nil {
			return failed(err)
		}
		if msg := c.limits.stops(j); msg != "" {
			return refuse(msg)
		}
		// The same reach as a read, so a child in a narrower copy cannot stop
		// work in its parent's folders.
		where, outside = run.InSession(string(j.Spec.Session)), turnAskedRefused
	case atWorkspace:
		where = run.InWorkspace(ids.WorkspaceID(id))
	default:
		return refuse(turnWriteRefused)
	}
	ok, err := c.May(rt.action, where)
	var why *refusal
	switch {
	case errors.As(err, &why):
		return refuse(why.msg)
	case err != nil:
		return failed(err)
	case !ok:
		return refuse(outside)
	}
	return true
}

// grantOwner is the owner id every grant names: the record's one owner.
func (a *API) grantOwner() string {
	if a.cfg.Store == nil {
		return ""
	}
	return a.cfg.Store.Owner()
}
