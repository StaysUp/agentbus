package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"go/types"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// rulesEnv is an API on a settings database, whose runner tokens are grants'
// keys, filled in by the test after it reads the rule versions it needs.
func rulesEnv(t *testing.T, grants map[string]run.Grant) (*env, *settings.DB) {
	t.Helper()
	db, err := settings.OpenDB(filepath.Join(t.TempDir(), settings.DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Settings: db,
		RunnerTokens: func(tok string) (run.Grant, bool) { g, ok := grants[tok]; return g, ok }})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); st.Close(); db.Close() })
	return &env{srv: srv, api: a, store: st, port: port}, db
}

// setTools sets the tool:* rule of db's policy: allowed, or refused.
func setTools(t *testing.T, db *settings.DB, on bool) settings.Version {
	t.Helper()
	p, _, err := db.Policy()
	if err != nil {
		t.Fatal(err)
	}
	p.Allow = slices.DeleteFunc(p.Allow, func(s string) bool { return s == "tool:*" })
	p.Forbid = slices.DeleteFunc(p.Forbid, func(s string) bool { return s == "tool:*" })
	if on {
		p.Allow = append(p.Allow, "tool:*")
	} else {
		p.Forbid = append(p.Forbid, "tool:*")
	}
	if _, _, err := db.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	snap, err := settings.Read(db)
	if err != nil {
		t.Fatal(err)
	}
	return snap.PolicyVersion
}

// keep keeps db's policy as it is now, as a turn's start does, and says its
// version.
func keep(t *testing.T, db *settings.DB) settings.Version {
	t.Helper()
	snap, err := settings.Read(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Keep(db, snap.Policy, snap.PolicyVersion); err != nil {
		t.Fatal(err)
	}
	return snap.PolicyVersion
}

func callAs(t *testing.T, e *env, tok, method, path, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r, _ = http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r, _ = http.NewRequest(method, e.srv.URL+path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, out := do(t, r)
	return resp.StatusCode, out
}

const toolsList = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

// O2.4: a turn's calls to the API follow the rules the turn took at its start.
// A change of the permissions while it runs reaches the next turn, never it.
func TestTurnCallsFollowItsOwnRules(t *testing.T) {
	grants := map[string]run.Grant{}
	e, db := rulesEnv(t, grants)
	setTools(t, db, true)
	started := keep(t, db) // a turn starts: tools on
	grants["abr_running"] = run.Grant{Session: "s1", Turn: "t_run", Root: "s1", Rules: started}

	setTools(t, db, false) // the person turns the tools off while it runs
	next := keep(t, db)    // the next turn starts under that
	grants["abr_next"] = run.Grant{Session: "s2", Turn: "t_next", Root: "s2", Rules: next}
	// A turn from before rules were kept names none: it follows the policy as
	// it is now, as every turn did then.
	grants["abr_old"] = run.Grant{Session: "s3", Turn: "t_old", Root: "s3"}

	if code, out := callAs(t, e, "abr_running", "POST", "/api/v0/mcp", toolsList); code != 200 {
		t.Errorf("the running turn lost the tools it started with: %d %s", code, out)
	}
	for _, tok := range []string{"abr_next", "abr_old"} {
		if code, out := callAs(t, e, tok, "POST", "/api/v0/mcp", toolsList); code != 403 || !strings.Contains(out, "AgentBus tools is off") {
			t.Errorf("%s: the tools are off for it: %d %s", tok, code, out)
		}
	}
	// Rules no longer kept refuse the turn's requests: nothing is decided by
	// other rules in their place.
	grants["abr_lost"] = run.Grant{Session: "s4", Turn: "t_lost", Root: "s4", Rules: "not-kept"}
	if code, _ := callAs(t, e, "abr_lost", "POST", "/api/v0/mcp", toolsList); code != http.StatusServiceUnavailable {
		t.Errorf("rules not kept: %d, want 503", code)
	}
	// The owner's own token is held to no rule.
	if code, _ := callAs(t, e, token, "POST", "/api/v0/mcp", toolsList); code != 200 {
		t.Errorf("the owner's tool list: %d", code)
	}
}

// O2.2: every grant names the record's owner, the owner's token's and an
// agent's alike, and an event an agent's request writes carries the owner
// beside the agent's turn.
func TestGrantsAndEventsNameTheOwner(t *testing.T) {
	grants := map[string]run.Grant{"abr_t": {Session: "s1", Turn: "t_1", Root: "s1"}}
	e, _ := rulesEnv(t, grants)
	owner := e.store.Owner()
	if !strings.HasPrefix(owner, "own_") || len(owner) != len("own_")+16 {
		t.Fatalf("owner id %q", owner)
	}
	for _, tok := range []string{token, "abr_t"} {
		r := httptest.NewRequest("GET", "/api/v0/version", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		g, ok := e.api.authorized(r)
		if !ok || g.Owner != owner {
			t.Errorf("%s: grant owner %q, want %q", tok[:5], g.Owner, owner)
		}
	}
	e.api.changed(event.Event{Kind: event.ToolsListed, Session: "s1", Turn: "t_1", Origin: &event.Origin{Kind: event.OriginAsked, Session: "s0", Turn: "t_0"}}, "")
	recs, err := e.store.After(0, store.Filter{}, 10)
	if err != nil || len(recs) == 0 {
		t.Fatalf("%v %d", err, len(recs))
	}
	for _, rec := range recs {
		var ev event.Event
		if err := json.Unmarshal(rec.Data, &ev); err != nil || ev.Owner != owner || ev.Turn != "t_1" || ev.Origin == nil {
			t.Errorf("event %s: owner %q turn %q origin %v (%v)", rec.Kind, ev.Owner, ev.Turn, ev.Origin, err)
		}
	}
}

// O2.1: one check. The grant answers May; an empty grant may do nothing, the
// owner's may do everything everywhere, and a turn's grant reaches nowhere
// until the door works out its reach.
func TestGrantMay(t *testing.T) {
	owner := run.OwnerGrant("own_x")
	turn := run.ForTurn(run.Grant{Session: "s1", Turn: "t_1"}, "own_x")
	for _, c := range []struct {
		name string
		g    run.Grant
		act  run.Action
		w    run.Where
		want bool
	}{
		{"empty, nowhere", run.Grant{}, run.ActRead, run.Where{}, false},
		{"owner, everywhere", owner, run.ActAdmin, run.Everywhere, true},
		{"owner, a session", owner, run.ActRun, run.InSession("s9"), true},
		{"turn, no admin", turn, run.ActAdmin, run.Where{}, false},
		{"turn before the door, a copy", turn, run.ActCopy, run.InWorkspace("w"), false},
		{"turn before the door, its own session", turn, run.ActRead, run.InSession("s1"), false},
		// A place is stated: Nowhere needs the class only, and a missing
		// place (a session or workspace given as "") is refused whoever asks
		// (review P1, 2026-11-25).
		{"owner, nowhere", owner, run.ActRead, run.Nowhere, true},
		{"owner, an empty session", owner, run.ActRead, run.InSession(""), false},
		{"owner, an empty workspace", owner, run.ActRead, run.InWorkspace(""), false},
		{"owner, no place stated", owner, run.ActRead, run.Where{}, false},
	} {
		if got, err := c.g.May(c.act, c.w); err != nil || got != c.want {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}

// O2.1: no route branches on what kind of token it was sent. Only the door
// (authority.go) and the token check (api.go) read which token it was: a
// handler asks the caller's grant and limits.
func TestOneDoor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "authority.go" || f == "api.go" {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "TurnsOnly", "TurnsGrant", "OwnerGrant", "ForTurn":
					t.Errorf("%s: %s reads what kind of token was sent; ask the caller's grant", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// Every route a turn token can reach says what it may do there, in its own doc
// or in turnTokenDoc, and turnTokenDoc names only operations that are.
func TestTurnTokenDocs(t *testing.T) {
	ops := map[string]bool{}
	for i := range routes {
		rt := &routes[i]
		ops[rt.op] = true
		if rt.public || rt.self || rt.at == atControl || !run.TurnActions.Has(rt.action) || rt.toolsSwitch && rt.op != "mcp" {
			continue
		}
		if turnTokenDoc[rt.op] == "" && !strings.Contains(rt.doc, "turn token") {
			t.Errorf("%s %s: a turn token may call it, and its doc does not say how", rt.method, rt.path)
		}
		if strings.Contains(strings.ToLower(rt.doc), "main token only") {
			t.Errorf("%s %s: a turn token may call it, and its doc says the main token only", rt.method, rt.path)
		}
	}
	for op := range turnTokenDoc {
		if !ops[op] {
			t.Errorf("turnTokenDoc names %s, which is no route", op)
		}
	}
}

// A turn's token whose grant names no turn still carries an agent's limits:
// which limits a grant gets follows from its reach, never from a field the
// token's maker may leave empty.
func TestTurnGrantWithoutTurnIsNoOwner(t *testing.T) {
	grants := map[string]run.Grant{"abr_blank": {Session: "s1", Root: "s1"}}
	e, _ := rulesEnv(t, grants)
	for _, path := range []string{"/api/v0/subscriptions", "/api/v0/subscriptions/claude/connect"} {
		if code, _ := callAs(t, e, "abr_blank", "GET", path, ""); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", path, code)
		}
	}
	// Its lists hold only its reach: s1 is in no workspace, so none.
	if code, out := callAs(t, e, "abr_blank", "GET", "/api/v0/workspaces", ""); code != 200 || !strings.Contains(out, `"workspaces":[]`) {
		t.Errorf("workspaces: %d %s", code, out)
	}
}

// forbiddenSites are the places in this package's code that answer 403, each
// as "function: what it answers": a writeError with StatusForbidden, a call
// of a refuse helper, a reach's refused, and the outside an atX place sets.
func forbiddenSites(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	fset := gotoken.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				name = types.ExprString(fn.Recv.List[0].Type) + "." + name
			}
			name = strings.TrimPrefix(name, "*")
			add := func(n ast.Node, e ast.Expr) {
				out[name+": "+types.ExprString(e)] = fset.Position(n.Pos()).String()
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					id, ok := n.Fun.(*ast.Ident)
					switch {
					case !ok:
					case id.Name == "writeError" && len(n.Args) == 3 && types.ExprString(n.Args[1]) == "http.StatusForbidden":
						add(n, n.Args[2])
					case (id.Name == "refuse" || id.Name == "refused") && len(n.Args) == 1:
						add(n, n.Args[0])
					}
				case *ast.ReturnStmt:
					switch name {
					case "agentLimits.route", "agentLimits.refusal":
						if len(n.Results) > 0 {
							if lit, ok := n.Results[0].(*ast.BasicLit); !ok || lit.Value != `""` {
								add(n, n.Results[0])
							}
						}
					}
				case *ast.AssignStmt:
					for i, l := range n.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == "outside" && i < len(n.Rhs) {
							add(n, n.Rhs[i])
						}
					}
				case *ast.ValueSpec:
					for i, id := range n.Names {
						if id.Name == "outside" && i < len(n.Values) {
							add(n, n.Values[i])
						}
					}
				}
				return true
			})
		}
	}
	return out
}

// The three limits an agent's refusals may come from (O2.3), and what is no
// agent's limit at all: a refusal every caller gets.
const (
	limWhere   = "1: where it works"
	limBrake   = "2: the owner's brakes"
	limOwn     = "3: never its own brakes"
	limNoAgent = "not an agent's limit"
	// limPass is a helper that writes what its caller hands it: each caller's
	// refusal is listed on its own.
	limPass = "a helper: its callers' refusals are listed"
)

// agentRefusals is every 403 this package answers, by the limit it keeps. A
// new one fails TestAgentRefusalsAreTheThree until it is named here, and a
// refusal no limit covers does not belong: an agent may do what its owner may,
// narrowed by the three only (his call, 2026-11-25).
var agentRefusals = map[string]string{
	`API.ServeHTTP: "no preflight is answered on this path"`:                                   limNoAgent,
	`API.ServeHTTP: "the Host header must name this service on 127.0.0.1, localhost or [::1]"`: limNoAgent,
	`API.ServeHTTP: "this origin is not allowed"`:                                              limNoAgent,
	`API.signIn: "a sign-in code is taken only from this service's own address"`:               limNoAgent,
	// The door: a route's own switch (limits.route: AgentBus tools), a class the
	// grant lacks (admin), and where the route acts.
	`API.allow: msg`:                 limBrake,
	`API.allow: c.limits.refusal()`:  limOwn,
	`API.within: msg`:                limPass,
	`API.within: why.msg`:            limBrake,
	`API.within: outside`:            limPass,
	`API.within: turnSessionRefused`: limWhere,
	`API.within: turnStopRefused`:    limWhere,
	`API.within: turnWriteRefused`:   limOwn,
	`agentLimits.Holds: turnSwitchedOff("apply a workspace's changes", "Agents may apply")`: limBrake,
	`agentLimits.route: turnSwitchedOff("use the agentbus MCP tools", "AgentBus tools")`:    limBrake,
	`agentLimits.refusal: turnWriteRefused`:                                                 limOwn,
	// Asking and its results.
	`API.askChecks: msg`:                        limPass,
	`API.askChecks: turnNoSession`:              limWhere, // no session named: nowhere to run
	`API.askChecks: turnSessionRefused`:         limWhere,
	`API.run: limit.Msg`:                        limBrake, // depth, job and cost caps (queue/budget.go)
	`agentLimits.inReach: turnSessionRefused`:   limWhere,
	`agentLimits.removeOn: turnOnRemoveRefused`: limWhere,
	`API.writeEvent: turnSessionRefused`:        limWhere,
	// Sessions and workspaces.
	`API.createSession: turnWorkspaceRefused`:                                  limWhere,
	`agentLimits.home: "a turn token has no workspace to create a session in"`: limWhere,
	`API.workspaceAuthority: msg`:                                              limPass,
	`API.workspaceAuthority: agentDirectRefused`:                               limWhere,
	`API.workspaceAuthority: agentFolderRefused`:                               limWhere,
	`API.workspaceAuthority: agentMachineRefused`:                              limWhere,
	`API.workspaceAuthority: agentNoWorkspace`:                                 limWhere,
	`API.workspaceAuthority: agentPathRefused`:                                 limWhere,
	`API.homeFiles: homeRefused`:                                               limWhere,
	// Commands an agent hands to AgentBus (H1).
	`API.within: commandStopRefused`:                         limWhere,
	`API.runCommand: commandOnlyTurns`:                       limNoAgent, // the main token and the turns token: no turn to run it for
	`refuseLimit: commandBeyondTurn + limit`:                 limWhere,   // the turn's shell wall the command would leave
	`API.runCommand: commandLimitUnknown`:                    limWhere,   // the runner records no command limit
	`API.runCommand: commandNoStart`:                         limWhere,   // no start of the turn in the record
	`API.runCommand: commandRefusedPrefix + quoteRule(rule)`: limBrake,   // the owner's command rules
	`API.runCommand: commandNeedsPerson`:                     limBrake,
	`API.runCommand: limit.Msg`:                              limBrake,
}

// O2.3: every refusal an agent's turn can get is one of the three limits. A
// 403 added anywhere in this package, a class withheld from agents that is not
// admin, or a route an agent holds the class for but can never reach, fails
// here until it names its limit, or goes.
func TestAgentRefusalsAreTheThree(t *testing.T) {
	sites := forbiddenSites(t)
	for site, at := range sites {
		if _, ok := agentRefusals[site]; !ok {
			t.Errorf("%s: a new refusal %q. Name which of the three limits it keeps in agentRefusals, or remove it: an agent may do what its owner may", at, site)
		}
	}
	for site := range agentRefusals {
		if _, ok := sites[site]; !ok {
			t.Errorf("agentRefusals names %q, which is no refusal any more: remove it", site)
		}
	}
	// The one class an agent lacks is admin: limit 3.
	if run.TurnActions != run.AllActions&^run.ActAdmin {
		t.Errorf("an agent's classes are %q: every class but admin", run.TurnActions)
	}
	for _, rt := range routes {
		if rt.public || rt.self {
			continue
		}
		if !run.TurnActions.Has(rt.action) && rt.action != run.ActAdmin {
			t.Errorf("%s %s: needs %q, which an agent lacks and is not admin", rt.method, rt.path, rt.action)
		}
		// atControl is every workspace, which no agent reaches: on a route an
		// agent has the class for, it would refuse every agent outright.
		if run.TurnActions.Has(rt.action) && rt.at == atControl {
			t.Errorf("%s %s: an agent holds %q but the route acts at control, so every agent is refused; say where it acts", rt.method, rt.path, rt.action)
		}
	}
}

// O2.3: what an agent may now do, as its owner may, and each of the three
// limits holding. A Copy agent (s_asker in ws_a) and a Direct one (s_d on
// reachM1) over seedReach's record.
func TestAgentMayWhatOwnerMay(t *testing.T) {
	grants := map[string]run.Grant{
		"abr_copy":   {Session: "s_asker", Turn: "t_copy", Root: "s_asker"},
		"abr_direct": {Session: "s_d", Turn: "t_direct", Root: "s_d"},
	}
	e, db := rulesEnv(t, grants)
	seedReach(t, e.store)
	may := func(tok, method, path, body string) string {
		t.Helper()
		code, out := callAs(t, e, tok, method, path, body)
		if code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Errorf("%s %s %s: %d %s, want it allowed", tok, method, path, code, out)
		}
		return out
	}
	refused := func(tok, method, path, body, want string) {
		t.Helper()
		if code, out := callAs(t, e, tok, method, path, body); code != http.StatusForbidden || !strings.Contains(out, want) {
			t.Errorf("%s %s %s: %d %s, want 403 %q", tok, method, path, code, out, want)
		}
	}
	lists := func(out string, in, out2 []string) {
		t.Helper()
		for _, s := range in {
			if !strings.Contains(out, `"`+s+`"`) {
				t.Errorf("the list lacks %s: %s", s, out)
			}
		}
		for _, s := range out2 {
			if strings.Contains(out, `"`+s+`"`) {
				t.Errorf("the list holds %s, outside the reach: %s", s, out)
			}
		}
	}

	// What it may now do. Lists hold its reach.
	lists(may("abr_copy", "GET", "/api/v0/sessions", ""), []string{"s_asker", "s_mine", "s_sib", "s_made"}, []string{"s_other", "s_d", "s_e", "s_f"})
	lists(may("abr_copy", "GET", "/api/v0/workspaces", ""), []string{"ws_a", "ws_made"}, []string{"ws_other", "ws_d", "ws_e", "ws_f"})
	lists(may("abr_direct", "GET", "/api/v0/sessions", ""), []string{"s_d", "s_e", "s_asker", "s_other"}, []string{"s_f"})
	lists(may("abr_direct", "GET", "/api/v0/workspaces", ""), []string{"ws_d", "ws_e", "ws_a"}, []string{"ws_f"})
	// Discard, update and close of a copy its session made, and of any copy
	// on a Direct agent's machine.
	for _, op := range []string{"discard", "update", "close"} {
		may("abr_copy", "POST", "/api/v0/workspaces/ws_made/"+op, "")
		may("abr_direct", "POST", "/api/v0/workspaces/ws_e/"+op, "")
	}
	// Archive a session, start or continue a turn in one, write a program
	// event there (its origin names the agent), read the tools, runners,
	// version, settings and approvals.
	may("abr_copy", "PATCH", "/api/v0/sessions/s_mine", `{"archived":true}`)
	may("abr_copy", "POST", "/api/v0/events", `{"kind":"x.prog.note","session":"s_mine"}`)
	for _, path := range []string{"/api/v0/tools", "/api/v0/runners", "/api/v0/version", "/api/v0/settings/permissions",
		"/api/v0/settings/instructions", "/api/v0/approvals", "/api/v0/folders"} {
		may("abr_copy", "GET", path, "")
	}
	recs, err := e.store.After(0, store.Filter{Session: "s_mine", Only: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var wrote bool
	for _, rec := range recs {
		var ev event.Event
		if json.Unmarshal(rec.Data, &ev) == nil && ev.Kind == "x.prog.note" {
			wrote = ev.Origin != nil && ev.Origin.Session == "s_asker" && ev.Origin.Turn == "t_copy"
		}
	}
	if !wrote {
		t.Errorf("the agent's program event does not name it as origin")
	}
	// A Direct agent makes a Direct workspace of its machine: no wider than it
	// is. The body passes the door; what follows is the route's.
	may("abr_direct", "POST", "/api/v0/workspaces", `{"folder":"/f/new","sandbox":false}`)

	// Limit 1, where it works: a Copy agent reaches nothing past its copies,
	// a Direct agent nothing past its machine.
	refused("abr_copy", "POST", "/api/v0/workspaces/ws_other/discard", "", turnSessionRefused)
	refused("abr_copy", "POST", "/api/v0/workspaces/ws_d/update", "", turnSessionRefused)
	refused("abr_copy", "PATCH", "/api/v0/sessions/s_other", `{"archived":true}`, turnSessionRefused)
	refused("abr_copy", "POST", "/api/v0/events", `{"kind":"x.prog.note","session":"s_other"}`, turnSessionRefused)
	refused("abr_copy", "GET", "/api/v0/events?session=s_e", "", turnSessionRefused)
	refused("abr_copy", "POST", "/api/v0/sessions", `{"workspace":"ws_other"}`, turnWorkspaceRefused)
	refused("abr_copy", "POST", "/api/v0/workspaces", `{"folder_ref":"fld_ws_a","sandbox":false}`, agentDirectRefused)
	refused("abr_copy", "POST", "/api/v0/workspaces", `{"folder":"/f/x"}`, agentPathRefused)
	refused("abr_direct", "POST", "/api/v0/workspaces/ws_f/close", "", turnSessionRefused)
	refused("abr_direct", "GET", "/api/v0/sessions/s_f", "", turnSessionRefused)
	refused("abr_direct", "POST", "/api/v0/workspaces", `{"folder":"/f/x","machine":"`+reachM2+`"}`, agentMachineRefused)

	// Limit 2, the owner's brakes: apply waits for the switch, read at the
	// turn's start; the tools switch closes the tool routes.
	refused("abr_copy", "POST", "/api/v0/workspaces/ws_made/apply", "{}", "Agents may apply is off")
	p, _, err := db.Policy()
	if err != nil {
		t.Fatal(err)
	}
	on := true
	p.AgentAPI.Apply = &on
	if _, _, err := db.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	grants["abr_apply"] = run.Grant{Session: "s_asker", Turn: "t_apply", Root: "s_asker", Rules: keep(t, db)}
	may("abr_apply", "POST", "/api/v0/workspaces/ws_made/apply", "{}")
	setTools(t, db, false)
	grants["abr_notools"] = run.Grant{Session: "s_asker", Turn: "t_nt", Root: "s_asker", Rules: keep(t, db)}
	refused("abr_notools", "POST", "/api/v0/mcp", toolsList, "AgentBus tools is off")

	// Limit 3, never its own brakes: no settings, saved agents, tools,
	// runners, keys, sign-ins or approvals, from either.
	for _, tok := range []string{"abr_copy", "abr_direct"} {
		for _, c := range [][3]string{
			{"PUT", "/api/v0/settings/permissions", "{}"},
			{"PATCH", "/api/v0/settings/permissions", "{}"},
			{"PUT", "/api/v0/settings/instructions", "{}"},
			{"POST", "/api/v0/agents", "{}"},
			{"DELETE", "/api/v0/agents/a1", ""},
			{"POST", "/api/v0/tools", "{}"},
			{"POST", "/api/v0/runners/r1/remove", ""},
			{"PUT", "/api/v0/subscriptions/anthropic/key", "{}"},
			{"GET", "/api/v0/subscriptions", ""},
			{"GET", "/api/v0/subscriptions/claude/connect", ""},
			{"POST", "/api/v0/approvals/ap_1", `{"decision":"once"}`},
		} {
			refused(tok, c[0], c[1], c[2], turnWriteRefused)
		}
	}
}
