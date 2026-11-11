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

	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

const (
	mcpWriteTok = "turn-tok-write-0123456789abcdef"
	mcpReadTok  = "turn-tok-read-0123456789abcdef"
)

// mcpEnv is an API whose runner tokens are two turn tokens of session s1:
// one that may write, one read-only, under a policy with tools = true.
func mcpEnv(t *testing.T) *env { return mcpEnvTools(t, true) }

// mcpEnvTools is mcpEnv with [agent_api] tools as given.
func mcpEnvTools(t *testing.T, tools bool) *env {
	t.Helper()
	return mcpEnvGrants(t, tools, map[string]run.Grant{
		mcpWriteTok: {Session: "s1", Turn: "t_1", Root: "s1"},
		mcpReadTok:  {Session: "s1", Turn: "t_1", Root: "s1", ReadOnly: true},
	})
}

// mcpEnvGrants is an API whose runner tokens are grants' keys, each with its
// grant, and [agent_api] tools as given.
func mcpEnvGrants(t *testing.T, tools bool, grants map[string]run.Grant) *env {
	t.Helper()
	dir := t.TempDir()
	p := settings.DefaultPolicy()
	p.AgentAPI.Tools = &tools
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

// A turn token sees the tools of its turn; a read-only one sees reads only.
// The main token sees them all.
func TestMCPToolListFollowsTheToken(t *testing.T) {
	e := mcpEnv(t)
	all := toolNames(t, e, token)
	for _, want := range []string{"list_agents", "ask_agent", "continue_session", "list_turns",
		"get_turn", "stop_turn", "turn_diff", "apply_turn", "review_turn",
		"search_history", "rename_session"} {
		if !slices.Contains(all, want) {
			t.Errorf("main token: %s missing from %v", want, all)
		}
	}
	if w := toolNames(t, e, mcpWriteTok); !slices.Equal(w, all) {
		t.Errorf("write turn: %v, want %v", w, all)
	}
	ro := toolNames(t, e, mcpReadTok)
	want := []string{"list_turns", "get_turn", "turn_diff", "search_history", "list_agents"}
	if !slices.Equal(ro, want) {
		t.Errorf("read-only turn: %v, want %v", ro, want)
	}
}

// Each [agent_api] switch takes its tools out of a turn token's list, as the
// route refuses them.
func TestMCPToolListFollowsAgentAPI(t *testing.T) {
	e := mcpEnv(t)
	g := &run.Grant{Session: "s1", Root: "s1"}
	names := func(api settings.AgentAPI) []string {
		var out []string
		for _, tr := range e.api.tools {
			if listed(tr, g, false, api) {
				out = append(out, tr.t.name)
			}
		}
		return out
	}
	off := func(set func(*settings.AgentAPI)) settings.AgentAPI {
		p := settings.DefaultPolicy()
		set(&p.AgentAPI)
		return p.AgentAPI
	}
	no := false
	for _, tc := range []struct {
		api  settings.AgentAPI
		gone []string
	}{
		{off(func(a *settings.AgentAPI) { a.Apply = &no }), []string{"apply_turn"}},
		{off(func(a *settings.AgentAPI) { a.Start = &no }), []string{"ask_agent"}},
		{off(func(a *settings.AgentAPI) { a.Continue = &no }), []string{"continue_session"}},
		{off(func(a *settings.AgentAPI) { a.Scope = settings.ScopeSession }), []string{"continue_session"}},
		{off(func(a *settings.AgentAPI) { a.Review = &no }), []string{"review_turn"}},
		{off(func(a *settings.AgentAPI) { a.Cancel = &no }), []string{"stop_turn"}},
		{off(func(a *settings.AgentAPI) { a.ListAgents = &no }), []string{"list_agents"}},
		{off(func(a *settings.AgentAPI) { a.Read = &no }), []string{"list_turns", "get_turn", "turn_diff", "search_history"}},
	} {
		got := names(tc.api)
		for _, n := range tc.gone {
			if slices.Contains(got, n) {
				t.Errorf("%s listed with its switch off: %v", n, got)
			}
		}
		if len(got) != len(e.api.tools)-len(tc.gone) {
			t.Errorf("%v: %d tools, want %d", tc.gone, len(got), len(e.api.tools)-len(tc.gone))
		}
	}
	// tools = false refuses the tool server to a turn token.
	if msg := turnRefusal(toolList, *g, off(func(a *settings.AgentAPI) { a.Tools = &no })); !strings.Contains(msg, "tools is false") {
		t.Errorf("tools off: %q", msg)
	}
}

// A tool left out of the list is still refused by its route when called:
// the list is discovery only. The refusal is the route's own 403.
func TestMCPCallOutsideTheGrantIsRefused(t *testing.T) {
	e := mcpEnv(t)
	for _, call := range []string{
		`{"name":"apply_turn","arguments":{"turn":"t_1"}}`,
		`{"name":"ask_agent","arguments":{"agent":"claude","prompt":"x"}}`,
		`{"name":"stop_turn","arguments":{"turn":"t_1"}}`,
	} {
		code, v := rpc(t, e, mcpReadTok, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":`+call+`}`)
		text, _ := dig(v, "result", "content").([]any)[0].(map[string]any)["text"].(string)
		if code != 200 || dig(v, "result", "isError") != true || !strings.HasPrefix(text, "HTTP 403") || !strings.Contains(text, "read-only") {
			t.Errorf("%s: %d %v", call, code, v)
		}
	}
	// Another session's history, outside the token's tree.
	code, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search_history","arguments":{"session":"other","q":"x"}}}`)
	text, _ := dig(v, "result", "content").([]any)[0].(map[string]any)["text"].(string)
	if code != 200 || dig(v, "result", "isError") != true || !strings.HasPrefix(text, "HTTP 403") {
		t.Errorf("other session: %d %v", code, v)
	}
}

// A call fills the route from its inputs, refuses inputs the tool does not
// take, and answers the route's JSON.
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
	if isErr, text := call(mcpWriteTok, `{"name":"search_history","arguments":{"q":"needle"}}`); isErr || !strings.Contains(text, `"hits"`) {
		t.Errorf("own session search: %v %s", isErr, text)
	}
	for params, want := range map[string]string{
		`{"name":"get_turn","arguments":{}}`:                                              "turn is required",
		`{"name":"get_turn","arguments":{"turn":"t_1","x":1}}`:                            "no input of that name",
		`{"name":"get_turn","arguments":{"turn":"../x"}}`:                                 "turn must be an id",
		`{"name":"continue_session","arguments":{"prompt":"hi"}}`:                         "session is required",
		`{"name":"ask_agent","arguments":{"agent":"claude","session":"s1","prompt":"x"}}`: "no input of that name",
		// No folder: the asking session's folder, always.
		`{"name":"ask_agent","arguments":{"agent":"claude","folder":"/x","prompt":"x"}}`: "no input of that name",
	} {
		if isErr, text := call(mcpWriteTok, params); !isErr || !strings.Contains(text, want) {
			t.Errorf("%s: %v %q, want %q", params, isErr, text, want)
		}
	}
	if _, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"agentbus_shell","arguments":{}}}`); dig(v, "error", "code") != float64(rpcInvalidParams) {
		t.Errorf("unknown tool: %v", v)
	}
	// The main token reaches the same route.
	if isErr, text := call(token, `{"name":"list_agents","arguments":{}}`); isErr {
		t.Errorf("main token list agents: %s", text)
	}
}

// Every tool's input schema stands alone (no $ref), names its required
// inputs, and refuses inputs it does not list.
func TestMCPToolSchemas(t *testing.T) {
	e := mcpEnv(t)
	for name, s := range e.api.toolSchemas {
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), "$ref") {
			t.Errorf("%s: schema holds a $ref: %s", name, b)
		}
		if s["additionalProperties"] != false || s["type"] != "object" {
			t.Errorf("%s: %s", name, b)
		}
	}
	req := func(name string) []string { return schemaRequired(e.api.toolSchemas[name]) }
	if r := req("review_turn"); !slices.Contains(r, "turn") || !slices.Contains(r, "agent") {
		t.Errorf("review requires %v", r)
	}
	if r := req("search_history"); slices.Contains(r, "session") || !slices.Contains(r, "q") {
		t.Errorf("search requires %v", r)
	}
}

// The OpenAPI description names each operation's tools.
func TestOpenAPINamesTools(t *testing.T) {
	doc := loadSpec(t)
	op := doc.Paths.Value("/api/v0/turns/{id}/apply").Post
	b, _ := json.Marshal(op.Extensions["x-agentbus-tools"])
	if !strings.Contains(string(b), `"apply_turn"`) || !strings.Contains(string(b), effectApply) {
		t.Errorf("apply's tools: %s", b)
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
// the rename never happens. A notification never runs either.
func TestMCPRefusesABadID(t *testing.T) {
	e := mcpEnv(t)
	call := `"method":"tools/call","params":{"name":"rename_session","arguments":{"title":"renamed"}}}`
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
			t.Errorf("a refused request renamed the session: %s", ev.Data)
		}
	}
	// A string id and an integer id are taken.
	for _, id := range []string{`"a"`, `7`} {
		if code, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`); code != 200 || dig(v, "result") == nil {
			t.Errorf("id %s: %d %v", id, code, v)
		}
	}
}

// With tools = false, the default, the tool server refuses a turn token; the
// main token still reaches it.
func TestMCPOffByDefault(t *testing.T) {
	if p := settings.DefaultPolicy(); *p.AgentAPI.Tools {
		t.Fatal("tools is on by default")
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
		read := strings.Contains("list_agents list_turns get_turn turn_diff search_history", m["name"].(string))
		if a["destructiveHint"] != false || a["readOnlyHint"] != read {
			t.Errorf("%s: %v", m["name"], a)
		}
	}
}
