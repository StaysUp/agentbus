package api

// Turn tokens. POST /api/v0/turns gives each turn's child its own token in
// AGENTBUS_TOKEN, with the service's address in AGENTBUS_URL. With it the
// agent reads its session's history and asks other agents: it starts turns
// on any saved agent, continues the sessions it started, asks for reviews,
// and reads their results. Each result comes back to its session as a later
// turn (queue.Deliver).
//
// What a turn token must never do: change policy (permissions.toml, which no
// route writes), change or remove saved agents, see or make tokens (none is
// ever in a reply), close workspaces, write program events, or read or write
// sessions outside its request tree. A read-only turn's token only reads.
// Its own session and the person's session, the tree's root, are never
// continued by it: results reach them by delivery only.
//
// A request tree is the person's session (its root) and every session work
// asked for from it, at any depth (queue.Chain). route.turn says what a turn
// token may do on each route; the zero value refuses it.
//
// The engine mints and keeps the tokens (package run), in memory only, as
// their SHA-256, and revokes each when its turn's child exits.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
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
	// treeCall: {id} is a model call whose stored request belongs to a
	// session in the token's request tree.
	treeCall
	// treeJob: {id} is a job of a session in the token's request tree.
	treeJob
	// treeAttempt: {id} is an attempt of a job in the token's request tree.
	treeAttempt
	// askTurn: POST /api/v0/turns. The handler checks the rest (askChecks).
	askTurn
	// askReview: {id} is a job of a session in the token's request tree, to
	// review. Not for a read-only turn.
	askReview
	// askedJob: {id} is a job that work in the token's request tree asked
	// for, to cancel. Not for a read-only turn.
	askedJob
	// askedTurn: {id} is a turn of such a job, to stop. Not for a read-only
	// turn.
	askedTurn
	// anyRead: a read that holds no session's data, such as the saved
	// agents.
	anyRead
)

// Refusals a turn token gets. Fixed wording: they never quote the request.
const (
	turnWriteRefused = "a turn token cannot call this route; it takes the main token. " +
		"A turn token reads its request tree's sessions, turns and jobs, lists agents, " +
		"and asks for work with POST /api/v0/turns and POST /api/v0/jobs/{id}/review"
	turnSessionRefused  = "a turn token reads only its own session and the sessions its request tree started"
	turnCallRefused     = "a turn token reads only the calls of its own session and the sessions its request tree started"
	turnReadOnly        = "this turn is read-only, so its token only reads; it cannot start, continue, review, cancel or stop work"
	turnContinueRefused = "a turn token continues only sessions its request tree started, never its own or the person's; " +
		"results come back to your session as later turns"
	turnFolderRefused  = "a turn token starts sessions only in its own session's folder, or a folder inside it"
	turnIsolateRefused = "a session a turn token starts always gets its own worktree when its folder is in a git repo; leave isolate out"
	turnAskedRefused   = "a turn token cancels and stops only work its request tree asked for"
)

// The refusal when [agent_api] turns a kind of call off. Fixed wording.
func turnSwitchedOff(what, key string) string {
	return "a turn token may not " + what + " here: " + key + " is false in [agent_api] of permissions.toml"
}

const turnPolicyBroken = "the permissions file permissions.toml in the data folder cannot be used, so a turn token's requests are refused; fix the file"

// grantKey carries a turn token's grant from route to its handler.
type grantKey struct{}

// turnCall is a turn token's grant with the policy read for its request.
type turnCall struct {
	g    run.Grant
	perm run.Permissions
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
func policyOf(r *http.Request) run.Permissions {
	c, _ := r.Context().Value(grantKey{}).(*turnCall)
	if c == nil {
		return run.DefaultPermissions()
	}
	return c.perm
}

// queueLimits are [limits] as the queue takes them.
func queueLimits(p run.Permissions) queue.Limits {
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
			writeError(w, http.StatusNotFound, "no such job")
		} else {
			a.internal(w)
		}
		return nil, false
	}
	// The policy is read for every request, so an edit applies at once. A
	// file that cannot be used refuses every turn token's request: a token
	// never runs without the policy the person wrote.
	perm, err := run.LoadPermissions(a.cfg.Permissions)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, turnPolicyBroken)
		return nil, false
	}
	switch rt.turn {
	case askTurn, askReview, askedJob, askedTurn:
		if g.ReadOnly {
			return refuse(turnReadOnly)
		}
	}
	api := perm.AgentAPI
	switch {
	case (rt.turn == treeSessionPath || rt.turn == treeSessionQuery || rt.turn == treeCall || rt.turn == treeJob || rt.turn == treeAttempt) && !*api.Read:
		return refuse(turnSwitchedOff("read sessions, turns, calls or jobs", "read"))
	case rt.turn == anyRead && !*api.ListAgents:
		return refuse(turnSwitchedOff("list agents or harnesses", "list_agents"))
	case rt.turn == askReview && !*api.Review:
		return refuse(turnSwitchedOff("ask for reviews", "review"))
	case (rt.turn == askedJob || rt.turn == askedTurn) && !*api.Cancel:
		return refuse(turnSwitchedOff("cancel jobs or stop turns", "cancel"))
	}
	// start and continue are told apart by the body: askChecks.
	inScope := func(session string) (bool, error) { return a.inScope(g, api.Scope, session) }
	switch rt.turn {
	case treeSessionPath:
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
	case treeCall:
		session, found, err := a.cfg.Store.CallSession(id)
		if err != nil {
			a.internal(w)
			return nil, false
		}
		if !found {
			writeError(w, http.StatusNotFound, "no request body is stored for this call")
			return nil, false
		}
		if ok, err := inScope(session); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnCallRefused)
		}
	case treeJob, askReview:
		j, err := a.queueJob(id)
		if err != nil {
			return failed(err)
		}
		if ok, err := inScope(j.Session); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case treeAttempt:
		if a.cfg.Queue == nil {
			return failed(queue.ErrNotFound)
		}
		j, err := a.cfg.Queue.JobByAttempt(queue.DefaultTeam, id)
		if err != nil {
			return failed(err)
		}
		if ok, err := inScope(j.Session); err != nil {
			return failed(err)
		} else if !ok {
			return refuse(turnSessionRefused)
		}
	case askedJob, askedTurn:
		if a.cfg.Queue == nil {
			return failed(queue.ErrNotFound)
		}
		var j queue.Job
		var err error
		if rt.turn == askedJob {
			j, err = a.cfg.Queue.Get(queue.DefaultTeam, id)
		} else {
			j, err = a.cfg.Queue.JobByAttempt(queue.DefaultTeam, id)
		}
		if err != nil {
			return failed(err)
		}
		if j.Links.Asker == nil || j.Links.Chain == nil || j.Links.Chain.Root != g.Root ||
			(api.Scope == run.ScopeSession && j.Links.Asker.Session != g.Session) {
			return refuse(turnAskedRefused)
		}
	case askTurn, anyRead:
	default:
		return refuse(turnWriteRefused)
	}
	if rt.turn == askReview {
		// The review's idempotency key, read from its body and put back: a
		// retry of a review already made is not new work.
		var key struct {
			Key string `json:"idempotency_key"`
		}
		if r.Body != nil {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "the body could not be read")
				return nil, false
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			_ = json.Unmarshal(b, &key)
		}
		if msg, err := a.overBudget(g, perm, key.Key); err != nil {
			a.internal(w)
			return nil, false
		} else if msg != "" {
			return refuse(msg)
		}
	}
	ctx := context.WithValue(r.Context(), grantKey{}, &turnCall{g: g, perm: perm})
	if rt.serve != nil && isJobsRoute(rt) {
		// The jobs API checks the main token itself. This request passed the
		// turn token's checks; a review it asks for names the asking turn,
		// and is held to [limits].
		ctx = jobs.Admit(ctx, askedLinks(g), queueLimits(perm))
	}
	return r.WithContext(ctx), true
}

// isJobsRoute says rt is served by the jobs API (Config.Jobs).
func isJobsRoute(rt *route) bool {
	return strings.HasPrefix(rt.path, "/api/v0/jobs") || strings.HasPrefix(rt.path, "/api/v0/attempts")
}

// askedLinks are the links of work g's turn asks for: the asking turn, and one
// level deeper in its request tree.
func askedLinks(g run.Grant) queue.Links {
	return queue.Links{Asker: &queue.Asker{Session: g.Session, Turn: g.Turn},
		Chain: &queue.Chain{Root: g.Root, Depth: g.Depth + 1}}
}

// queueJob reads a job of the default team.
func (a *API) queueJob(id string) (queue.Job, error) {
	if a.cfg.Queue == nil {
		return queue.Job{}, queue.ErrNotFound
	}
	return a.cfg.Queue.Get(queue.DefaultTeam, id)
}

// inScope says session is in reach of g under [agent_api] scope: its own
// session alone for "session", its request tree for "tree".
func (a *API) inScope(g run.Grant, scope, session string) (bool, error) {
	if scope == run.ScopeSession {
		return session != "" && session == g.Session, nil
	}
	return a.inTree(g, session)
}

// overBudget says why g's request tree may ask for no more work under
// max_cost_usd_per_tree, or "" while it may: the cost of every model call in
// the tree's sessions so far. 0 is off. A retry whose idempotency key already
// made a job is not new work, so it passes, and gets its job as before.
func (a *API) overBudget(g run.Grant, perm run.Permissions, key string) (string, error) {
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
		return fmt.Sprintf("work started from one session may cost at most $%.2f in all, and it has cost $%.2f (max_cost_usd_per_tree in [limits] of permissions.toml)", max, cost), nil
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

// askChecks checks a turn token's POST /api/v0/turns body and fills what the
// token decides: for a new session its folder, and the links of the work. It
// says why not, and has then answered.
func (a *API) askChecks(w http.ResponseWriter, g run.Grant, perm run.Permissions, body *TurnRequest, continues bool) (queue.Links, bool) {
	refuse := func(msg string) (queue.Links, bool) {
		writeError(w, http.StatusForbidden, msg)
		return queue.Links{}, false
	}
	api := perm.AgentAPI
	if continues && !*api.Continue {
		return refuse(turnSwitchedOff("continue sessions", "continue"))
	}
	if !continues && !*api.Start {
		return refuse(turnSwitchedOff("start sessions", "start"))
	}
	if msg, err := a.overBudget(g, perm, body.IdempotencyKey); err != nil {
		a.internal(w)
		return queue.Links{}, false
	} else if msg != "" {
		return refuse(msg)
	}
	if continues {
		if body.Session == g.Session || body.Session == g.Root || api.Scope == run.ScopeSession {
			return refuse(turnContinueRefused)
		}
		root, err := a.cfg.Queue.SessionRoot(queue.DefaultTeam, body.Session)
		if err != nil {
			a.internal(w)
			return queue.Links{}, false
		}
		if root == "" || root != g.Root {
			return refuse(turnContinueRefused)
		}
		return askedLinks(g), true
	}
	if body.Isolate != nil && !*body.Isolate {
		return refuse(turnIsolateRefused)
	}
	asked, ws, err := a.cfg.Queue.SessionFolders(queue.DefaultTeam, g.Session)
	if err != nil {
		a.internal(w)
		return queue.Links{}, false
	}
	if asked == "" && ws == "" {
		c, err := a.cfg.Store.Continuation(g.Session)
		if err != nil {
			a.internal(w)
			return queue.Links{}, false
		}
		ws = c.Folder
	}
	if body.Folder == "" {
		// The folder the person picked, so the new session gets a worktree
		// of its own beside this one, not inside it.
		body.Folder = asked
		if body.Folder == "" {
			body.Folder = ws
		}
		if body.Folder == "" {
			return refuse(turnFolderRefused)
		}
	} else if !within(body.Folder, asked) && !within(body.Folder, ws) {
		return refuse(turnFolderRefused)
	}
	return askedLinks(g), true
}

// within says folder is dir or inside it, once both are resolved: a symlink
// inside dir that leads out of it is not inside.
func within(folder, dir string) bool {
	if dir == "" || !filepath.IsAbs(folder) {
		return false
	}
	f, err1 := filepath.EvalSymlinks(folder)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	return f == d || strings.HasPrefix(f, d+string(os.PathSeparator))
}
