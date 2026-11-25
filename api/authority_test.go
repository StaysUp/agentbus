package api

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// O2.4: an "always for this folder" given while a turn runs applies from the
// next turn. The running turn holds the grants made before it started.
func TestFolderGrantDuringTurnAppliesNext(t *testing.T) {
	grants := map[string]run.Grant{}
	e, db := rulesEnv(t, grants)
	setTools(t, db, true)
	v := keep(t, db)
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, _ = filepath.EvalSymlinks(f)
	before := time.Now().Add(-time.Hour)
	grants["abr_running"] = run.Grant{Session: "s1", Turn: "t_run", Root: "s1", Rules: v, RulesAt: before}
	if err := e.store.AddFolderGrant("s1", filepath.Dir(f)); err != nil {
		t.Fatal(err)
	}
	grants["abr_next"] = run.Grant{Session: "s1", Turn: "t_next", Root: "s1", Rules: v, RulesAt: time.Now().Add(time.Hour)}
	body := fmt.Sprintf(`{"path":%q}`, f)
	if code, out := callAs(t, e, "abr_running", "POST", "/api/v0/files/read", body); code != 403 {
		t.Errorf("a grant made during the turn reached it: %d %s", code, out)
	}
	if code, out := callAs(t, e, "abr_next", "POST", "/api/v0/files/read", body); code != 200 {
		t.Errorf("the next turn lacks the grant: %d %s", code, out)
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
		{"turn, no copy", turn, run.ActCopy, run.InWorkspace("w"), false},
		{"turn before the door, its own session", turn, run.ActRead, run.InSession("s1"), false},
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
	for _, path := range []string{"/api/v0/sessions", "/api/v0/approvals", "/api/v0/settings/permissions"} {
		if code, _ := callAs(t, e, "abr_blank", "GET", path, ""); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", path, code)
		}
	}
}
