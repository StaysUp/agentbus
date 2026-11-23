package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

const (
	mcpWriteTok = "turn-tok-write-0123456789abcdef"
	mcpReadTok  = "turn-tok-read-0123456789abcdef"
)

// mcpEnv is an API whose runner tokens are two turn tokens of session s1,
// under a policy with tools = true. Read-only turns retired (B12), so both are
// ordinary turn tokens; the second keeps its old name.
func mcpEnv(t *testing.T) *env { return mcpEnvTools(t, true) }

// mcpEnvTools is mcpEnv with [agent_api] tools as given.
func mcpEnvTools(t *testing.T, tools bool) *env {
	t.Helper()
	return mcpEnvGrants(t, tools, map[string]run.Grant{
		mcpWriteTok: {Session: "s1", Turn: "t_1", Root: "s1"},
		mcpReadTok:  {Session: "s1", Turn: "t_1", Root: "s1"},
	})
}

// mcpEnvGrants is an API whose runner tokens are grants' keys, each with its
// grant, and tool:* rule as given.
func mcpEnvGrants(t *testing.T, tools bool, grants map[string]run.Grant) *env {
	t.Helper()
	dir := t.TempDir()
	p := settings.FirstInstallPolicy() // a fresh install's policy
	if tools {
		p.Allow = append(p.Allow, "tool:*")
		p.Forbid = slices.DeleteFunc(p.Forbid, func(s string) bool { return s == "tool:*" })
	} else {
		p.Allow = slices.DeleteFunc(p.Allow, func(s string) bool { return s == "tool:*" })
		p.Forbid = append(p.Forbid, "tool:*")
	}
	if err := os.WriteFile(filepath.Join(dir, settings.PolicyFile), settings.FormatPolicy(p), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Settings: settings.NewFiles(dir),
		RunnerTokens: func(tok string) (run.Grant, bool) {
			g, ok := grants[tok]
			return g, ok
		}})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &env{srv: srv, api: a, store: st, port: port}
}

// rpc sends one JSON-RPC message with tok and decodes the answer.
func rpc(t *testing.T, e *env, tok, msg string) (int, map[string]any) {
	t.Helper()
	r, _ := http.NewRequest("POST", e.srv.URL+"/api/v0/mcp", strings.NewReader(msg))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	resp, out := do(t, r)
	var v map[string]any
	_ = json.Unmarshal([]byte(out), &v)
	return resp.StatusCode, v
}

func toolNames(t *testing.T, e *env, tok string) []string {
	t.Helper()
	code, v := rpc(t, e, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if code != 200 {
		t.Fatalf("tools/list: %d %v", code, v)
	}
	var names []string
	for _, x := range dig(v, "result", "tools").([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	return names
}

// initialize names the server and the version it speaks; a notification gets
// 202 and no body.
func TestMCPInitialize(t *testing.T) {
	e := mcpEnv(t)
	code, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if code != 200 || dig(v, "result", "protocolVersion") != "2025-06-18" || dig(v, "result", "capabilities", "tools") == nil {
		t.Fatalf("initialize: %d %v", code, v)
	}
	// [agent_api] apply is off by default, so apply_turn is not listed and
	// the server's instructions do not offer it (0.5.1).
	if got := dig(v, "result", "instructions"); got != settings.ToolServerInstructions(false) {
		t.Errorf("instructions with apply off: %v", got)
	}
	if code, _ := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Errorf("notification: %d, want 202", code)
	}
	if code, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`); code != 200 || dig(v, "error", "code") != float64(rpcMethodNotFound) {
		t.Errorf("unknown method: %d %v", code, v)
	}
	if code, _ := rpc(t, e, "not-a-token", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
}

// B15 (his call, 2026-11-19): apps are for agents only. A turn token sees the
// apps (ask_agent, review_turn); the MAIN token does not — it reaches the API
// directly. The read-tool copies are gone (read via agentbus-api), so none of
// those is listed, nor continue_session.
func TestMCPToolListFollowsTheToken(t *testing.T) {
	e := mcpEnv(t)
	// A turn token sees the apps.
	turn := toolNames(t, e, mcpWriteTok)
	for _, app := range []string{"ask_agent", "review_turn"} {
		if !slices.Contains(turn, app) {
			t.Errorf("turn token: %s missing from %v", app, turn)
		}
	}
	// The main token sees no app.
	main := toolNames(t, e, token)
	for _, app := range []string{"ask_agent", "review_turn"} {
		if slices.Contains(main, app) {
			t.Errorf("main token lists the app %s; apps are for agents only: %v", app, main)
		}
	}
	for _, gone := range []string{"continue_session", "list_agents", "list_turns", "get_turn",
		"turn_diff", "turn_changes", "turn_calls", "turn_tools", "turn_context", "search_history", "session_context"} {
		if slices.Contains(turn, gone) {
			t.Errorf("%s is listed as a tool: it is an API route, not a tool now", gone)
		}
	}
	// A write turn and the second turn see the same list.
	if ro := toolNames(t, e, mcpReadTok); !slices.Equal(ro, turn) {
		t.Errorf("second turn: %v, want %v", ro, turn)
	}
}

// Every remaining tool is listed to a turn token within its tree; tools
// refused refuses the tool server, naming the switch as Settings shows it. (B11
// removed the write route copies, so there is no apply_turn to gate off.)
func TestMCPToolListFollowsAgentAPI(t *testing.T) {
	e := mcpEnv(t)
	// A real turn token: it has a turn, so it may see the apps (listedApp).
	g := &run.Grant{Session: "s1", Root: "s1", Turn: "t_1"}
	// Apps list to a turn token whose tool server is on; route tools follow the
	// gateway rules (B16). Both read the same rules now.
	names := func(rules gateway.Rules) []string {
		var out []string
		for _, st := range e.api.toolSnap.Load().list {
			visible := false
			if st.rt == nil { // an app has no single route (apps.go)
				visible = listedApp(g, false, rules)
			} else {
				visible = listedRoute(st.rt, g, false, rules, false)
			}
			if visible {
				out = append(out, st.def.Name)
			}
		}
		return out
	}
	defaultRules := gateway.Rules{
		Allow:  []string{"tool:*", "tool:edit"},
		Forbid: []string{"tool:subagent"},
	}
	for _, tc := range []struct {
		rules gateway.Rules
		gone  []string
	}{
		{defaultRules, nil},
	} {
		got := names(tc.rules)
		for _, n := range tc.gone {
			if slices.Contains(got, n) {
				t.Errorf("%s listed with its switch off: %v", n, got)
			}
		}
		if n := len(e.api.toolSnap.Load().list); len(got) != n-len(tc.gone) {
			t.Errorf("%v: %d tools, want %d", tc.gone, len(got), n-len(tc.gone))
		}
	}
	// tool:* refused refuses the tool server to a turn token, naming the
	// switch as Settings shows it.
	toolListRoute := &route{turn: toolList}
	forbidTools := gateway.Rules{
		Forbid: []string{"tool:*"},
	}
	if msg := turnRefusal(toolListRoute, forbidTools, false); !strings.Contains(msg, "AgentBus tools is off in Settings (Permissions)") ||
		strings.Contains(msg, "permissions.toml") {
		t.Errorf("tools off: %q", msg)
	}
}

// The read-tool copies retired in 0.6 (B12): reading the record is done through
// agentbus-api, not tools. A tools/call for one of them is "no such tool"; the
// tool list names only the apps.
func TestReadToolsAreNotTools(t *testing.T) {
	e := mcpEnv(t)
	for _, name := range []string{"list_turns", "get_turn", "turn_diff", "turn_changes", "turn_calls",
		"turn_tools", "turn_context", "search_history", "session_context", "list_agents"} {
		call := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
		code, v := rpc(t, e, mcpWriteTok, call)
		if code != 200 || dig(v, "error", "code") != float64(rpcInvalidParams) {
			t.Errorf("%s is still callable as a tool: %d %v", name, code, v)
		}
	}
}

// A call validates its arguments against the tool's inputs before routing: a
// missing required input, or one the tool does not take, is refused by name,
// and an unknown tool name is a JSON-RPC error. ask_agent is the one built-in
// app now (read-only retired, B12: the read tools are gone, and ask_agent no
// longer takes read_only).
func TestMCPCallSendsTheRoute(t *testing.T) {
	e := mcpEnv(t)
	call := func(tok, params string) (bool, string) {
		t.Helper()
		_, v := rpc(t, e, tok, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":`+params+`}`)
		c, _ := dig(v, "result", "content").([]any)
		if len(c) != 1 {
			t.Fatalf("%s: %v", params, v)
		}
		return dig(v, "result", "isError") == true, c[0].(map[string]any)["text"].(string)
	}
	for params, want := range map[string]string{
		`{"name":"ask_agent","arguments":{"prompt":"x"}}`:                              "agent is required",
		`{"name":"ask_agent","arguments":{"agent":"a_x","prompt":"x","session":"s1"}}`: "no input of that name",
		// No folder: the asking session's folder, always.
		`{"name":"ask_agent","arguments":{"agent":"a_x","folder":"/x","prompt":"x"}}`: "no input of that name",
		// read_only retired (B12): ask_agent no longer takes it.
		`{"name":"ask_agent","arguments":{"agent":"a_x","prompt":"x","read_only":true}}`: "no input of that name",
	} {
		if isErr, text := call(mcpWriteTok, params); !isErr || !strings.Contains(text, want) {
			t.Errorf("%s: %v %q, want %q", params, isErr, text, want)
		}
	}
	if _, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"agentbus_shell","arguments":{}}}`); dig(v, "error", "code") != float64(rpcInvalidParams) {
		t.Errorf("unknown tool: %v", v)
	}
}

// Every tool's input schema stands alone (no $ref), names its required
// inputs, and refuses inputs it does not list.
func TestMCPToolSchemas(t *testing.T) {
	e := mcpEnv(t)
	for _, st := range e.api.toolSnap.Load().list {
		name, s := st.def.Name, st.def.Inputs
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), "$ref") {
			t.Errorf("%s: schema holds a $ref: %s", name, b)
		}
		if s["additionalProperties"] != false || s["type"] != "object" {
			t.Errorf("%s: %s", name, b)
		}
	}
	schemaOf := func(name string) map[string]any {
		st, _ := e.api.servedByName(name)
		return st.def.Inputs
	}
	// ask_agent is the one built-in app (read-only retired, B12: the read tools
	// are gone). It requires agent and prompt, and no longer takes read_only.
	r := schemaRequired(schemaOf("ask_agent"))
	if !slices.Contains(r, "agent") || !slices.Contains(r, "prompt") {
		t.Errorf("ask_agent requires %v", r)
	}
	if props, _ := schemaOf("ask_agent")["properties"].(map[string]any); props["read_only"] != nil {
		t.Errorf("ask_agent still takes read_only: %v", props)
	}
}

// The OpenAPI description names an operation's route tools (by name; a tool
// carries no effect since 0.6). B15: ask_agent is a default APP, not a route
// tool, so POST /turns (startTurn) names no tool of its own.
func TestOpenAPINamesTools(t *testing.T) {
	doc := loadSpec(t)
	op := doc.Paths.Value("/api/v0/turns").Post
	b, _ := json.Marshal(op.Extensions["x-agentbus-tools"])
	if strings.Contains(string(b), `"ask_agent"`) {
		t.Errorf("start turn still names ask_agent as a route tool; it is an app now: %s", b)
	}
	// The read-tool copies retired in 0.6 (B12): the read routes name no tool.
	for _, p := range []string{"/api/v0/turns/{id}/diff", "/api/v0/turns/{id}/changes"} {
		op := doc.Paths.Value(p).Get
		b, _ := json.Marshal(op.Extensions["x-agentbus-tools"])
		if strings.Contains(string(b), "turn_") {
			t.Errorf("%s still names a tool: %s", p, b)
		}
	}
	// The removed write route copies no longer name a tool.
	op = doc.Paths.Value("/api/v0/workspaces/{id}/apply").Post
	b, _ = json.Marshal(op.Extensions["x-agentbus-tools"])
	if strings.Contains(string(b), "apply_turn") {
		t.Errorf("apply still names apply_turn: %s", b)
	}
}

// A number in a tool's input goes into the query as the route reads it.
func TestToolQueryValue(t *testing.T) {
	for v, want := range map[any]string{0.0: "0", 20.0: "20", 1.5: "1.5", "x": "x", true: "true"} {
		if got, ok := queryValue(v); !ok || got != want {
			t.Errorf("%v: %q, want %q", v, got, want)
		}
	}
}

// A request whose id is not a string or an integer is refused before it runs:
// the tool never runs. A notification never runs either.
func TestMCPRefusesABadID(t *testing.T) {
	e := mcpEnv(t)
	call := `"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"a_x","prompt":"renamed"}}}`
	for _, id := range []string{`{}`, `[]`, `true`, `null`, `1.5`} {
		code, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":`+id+`,`+call)
		if code != 200 || dig(v, "error", "code") != float64(rpcInvalid) || dig(v, "result") != nil {
			t.Errorf("id %s: %d %v, want invalid request", id, code, v)
		}
	}
	if code, _ := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0",`+call); code != 202 {
		t.Errorf("notification: %d", code)
	}
	evs, _, err := e.store.Page(0, store.Filter{}, 1000, store.PageBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if strings.Contains(string(ev.Data), `"renamed"`) {
			t.Errorf("a refused request ran the tool: %s", ev.Data)
		}
	}
	// A string id and an integer id are taken.
	for _, id := range []string{`"a"`, `7`} {
		if code, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`); code != 200 || dig(v, "result") == nil {
			t.Errorf("id %s: %d %v", id, code, v)
		}
	}
}

// With tools refused the tool server refuses a turn token; the main token
// still reaches it. tools is allowed by default.
func TestMCPOff(t *testing.T) {
	if p := settings.DefaultPolicy(); !slices.Contains(p.Allow, "tool:*") {
		t.Fatal("tool:* is not allowed by default")
	}
	e := mcpEnvTools(t, false)
	if code, _ := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 403 {
		t.Errorf("turn token with tools off: %d, want 403", code)
	}
	if code, _ := rpc(t, e, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 200 {
		t.Errorf("main token: %d", code)
	}
}

// No tool is marked destructive: codex would ask for approval a turn cannot
// give. Reads are marked read-only.
func TestMCPAnnotations(t *testing.T) {
	e := mcpEnv(t)
	_, v := rpc(t, e, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for _, x := range dig(v, "result", "tools").([]any) {
		m := x.(map[string]any)
		a := m["annotations"].(map[string]any)
		read := strings.Contains("list_agents list_turns get_turn turn_diff turn_changes turn_calls turn_tools turn_context search_history session_context", m["name"].(string))
		if a["destructiveHint"] != false || a["readOnlyHint"] != read {
			t.Errorf("%s: %v", m["name"], a)
		}
	}
}
