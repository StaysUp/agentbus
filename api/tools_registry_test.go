package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/staysup/agentbus/toolreg"
)

// toolEntry is one tool as tools/list sends it, by name.
func toolEntry(t *testing.T, e *env, tok, name string) map[string]any {
	t.Helper()
	_, v := rpc(t, e, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for _, x := range dig(v, "result", "tools").([]any) {
		m := x.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	return nil
}

// callText is the text a tools/call of name with args answers, and whether it
// was an error.
func callText(t *testing.T, e *env, tok, name, args string) (bool, string) {
	t.Helper()
	_, v := rpc(t, e, tok, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	c, ok := dig(v, "result", "content").([]any)
	if !ok || len(c) != 1 {
		t.Fatalf("%s: %v", name, v)
	}
	return dig(v, "result", "isError") == true, c[0].(map[string]any)["text"].(string)
}

// Apps are for agents only (B15, his call 2026-11-19): the main token neither
// lists nor runs them. It is hidden from the list and refused on a call, so the
// main token cannot drive an app that would need a turn's folder and session.
func TestMainTokenCannotRunApp(t *testing.T) {
	e := mcpEnv(t)
	if slices.Contains(toolNames(t, e, token), "ask_agent") {
		t.Errorf("the main token lists ask_agent; apps are agents-only")
	}
	isErr, text := callText(t, e, token, "ask_agent", `{"agent":"a_x","prompt":"x"}`)
	if !isErr || !strings.Contains(text, "agent's turn") {
		t.Errorf("main token ran ask_agent: isErr=%v text=%q", isErr, text)
	}
	// A turn token is not refused for being an agent (it reaches the gate; here
	// a missing field is refused before the gate, proving it is not the main-
	// token refusal).
	if isErr, text := callText(t, e, mcpWriteTok, "ask_agent", `{"prompt":"x"}`); !isErr || strings.Contains(text, "agent's turn") {
		t.Errorf("turn token wrongly got the agents-only refusal: %q", text)
	}
}

// listToolSummaries reads GET /api/v0/tools with the main token: the list, as
// facts only.
func listToolSummaries(t *testing.T, e *env) []ToolSummary {
	t.Helper()
	resp, body := do(t, e.req(t, "GET", "/api/v0/tools", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("GET tools: %d %s", resp.StatusCode, body)
	}
	var out ToolList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Tools
}

// getToolDef reads GET /api/v0/tools/{name}: one tool's full definition.
func getToolDef(t *testing.T, e *env, name string) toolreg.Tool {
	t.Helper()
	resp, body := do(t, e.req(t, "GET", "/api/v0/tools/"+name, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("GET tools/%s: %d %s", name, resp.StatusCode, body)
	}
	var def toolreg.Tool
	if err := json.Unmarshal([]byte(body), &def); err != nil {
		t.Fatal(err)
	}
	return def
}

// B3's done-when: a built-in removed and re-added as a custom tool behaves the
// same. The tool's list entry (schema, annotations, description) and a call's
// result are byte-identical before removing it and after re-adding it.
// A folder tool persisted in the registry from before 0.6.5 (removed and
// re-added as a custom tool) is dropped on serve: its route ships no tool and
// is not wrappable, so resolveServed rejects it the same way createTool rejects
// a new one. Without this an upgraded install would keep serving read_file.
func TestPersistedFolderToolNotServed(t *testing.T) {
	e := mcpEnv(t)
	// Inject it straight into the registry, as a stored overlay would, past the
	// createTool check that now refuses it.
	if _, err := e.api.registry.Add(toolreg.Tool{
		Name:        "read_file",
		Description: "Read a file outside the session copy.",
		Inputs: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
		Where: toolreg.WhereControl,
		Run:   toolreg.Run{Kind: toolreg.RunAPI, Method: "POST", Path: "/api/v0/files/read", Body: true},
	}); err != nil {
		t.Fatal(err)
	}
	e.api.rebuildTools()
	if _, ok := e.api.servedByName("read_file"); ok {
		t.Error("a persisted read_file tool is still served after rebuild")
	}
	if got := len(listToolSummaries(t, e)); got != 2 {
		t.Errorf("%d served tools, want 2 (the persisted folder tool must not show)", got)
	}
}

func TestDefaultRemovedAndReaddedBehavesTheSame(t *testing.T) {
	e := mcpEnv(t)
	// The built-in tools are the two apps, ask_agent and review_turn. The B8
	// folder tools (read_file, list_folder, write_file) are hidden in 0.6.5,
	// so they are not served.
	if got := len(listToolSummaries(t, e)); got != 2 {
		t.Fatalf("%d built-in tools, want 2", got)
	}
	before := toolEntry(t, e, mcpWriteTok, "ask_agent")
	if before == nil {
		t.Fatal("ask_agent not listed")
	}
	_, beforeCall := callText(t, e, mcpWriteTok, "ask_agent", `{"prompt":"x"}`)

	// The stored definition of ask_agent: B15 makes it a default app (a workflow
	// over the API), not a route tool.
	def := getToolDef(t, e, "ask_agent")
	if def.Run.Kind != toolreg.RunApp || def.Run.App == nil || len(def.Run.App.Steps) == 0 {
		t.Fatalf("ask_agent definition: %+v", def)
	}

	// Remove it: it stops being served.
	resp, body := do(t, e.req(t, "DELETE", "/api/v0/tools/ask_agent", nil))
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE ask_agent: %d %s", resp.StatusCode, body)
	}
	if toolEntry(t, e, mcpWriteTok, "ask_agent") != nil {
		t.Fatal("ask_agent still listed after removal")
	}

	// Re-add the same definition as a custom tool.
	b, _ := json.Marshal(def)
	resp, body = do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader(b)))
	if resp.StatusCode != 201 {
		t.Fatalf("POST ask_agent: %d %s", resp.StatusCode, body)
	}

	after := toolEntry(t, e, mcpWriteTok, "ask_agent")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("list entry changed after re-add:\nbefore %v\nafter  %v", before, after)
	}
	if _, afterCall := callText(t, e, mcpWriteTok, "ask_agent", `{"prompt":"x"}`); afterCall != beforeCall {
		t.Errorf("call result changed: %q vs %q", beforeCall, afterCall)
	}
}

// The list carries facts only (the 0.6 rule): each entry has a name, a
// description and where it runs, and never the inputs schema or the run (and no
// effect, which a tool no longer carries). A tool's whole definition is read
// from GET /api/v0/tools/{id}, which a turn token cannot reach and 404s on a
// name not served.
func TestToolListFactsOnly(t *testing.T) {
	e := mcpEnv(t)
	resp, body := do(t, e.req(t, "GET", "/api/v0/tools", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("GET tools: %d %s", resp.StatusCode, body)
	}
	var raw struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Tools) == 0 {
		t.Fatal("no tools listed")
	}
	for _, m := range raw.Tools {
		for _, k := range []string{"name", "description", "where"} {
			if _, ok := m[k]; !ok {
				t.Errorf("list entry has no %q: %v", k, m)
			}
		}
		for _, k := range []string{"inputs", "run", "effect"} {
			if _, ok := m[k]; ok {
				t.Errorf("list entry carries %q; the list is facts only: %v", k, m)
			}
		}
	}

	// The single tool route carries the full definition: for an app, its inputs
	// and its workflow.
	def := getToolDef(t, e, "ask_agent")
	if def.Name != "ask_agent" || def.Run.Kind != toolreg.RunApp || def.Run.App == nil || def.Inputs == nil {
		t.Fatalf("single tool not full: %+v", def)
	}

	// A name not served is a 404, with the same words as remove.
	if r, b := do(t, e.req(t, "GET", "/api/v0/tools/no_such", nil)); r.StatusCode != 404 {
		t.Errorf("GET missing tool: %d %s", r.StatusCode, b)
	}

	// A turn token cannot read a single tool, as it cannot list them.
	r := e.req(t, "GET", "/api/v0/tools/apply_turn", nil)
	r.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	if resp, _ := do(t, r); resp.StatusCode == 200 {
		t.Errorf("turn token read a single tool: %d", resp.StatusCode)
	}
}

// Add refuses a run that names no route, a duplicate name, and a body that is
// not a tool; remove refuses a name that is not served; and a turn token
// cannot reach the management routes at all.
func TestToolsRoutesGuardrails(t *testing.T) {
	e := mcpEnv(t)
	post := func(body string) (int, string) {
		resp, b := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(body))))
		return resp.StatusCode, b
	}
	// A run may name only a route a built-in tool exposes. Each of these is a
	// route a custom tool cannot be: a path with no route, a real route that
	// is not a tool (POST /api/v0/agents), a {call} path, and a settings write
	// that needs an If-Match header.
	for _, run := range []string{
		`{"kind":"api","method":"GET","path":"/api/v0/nope"}`,
		`{"kind":"api","method":"POST","path":"/api/v0/agents","body":true}`,
		`{"kind":"api","method":"GET","path":"/api/v0/turns/{id}/calls/{call}","id_input":"turn"}`,
		`{"kind":"api","method":"PUT","path":"/api/v0/settings/instructions","body":true}`,
	} {
		body := `{"name":"my_tool","description":"x","inputs":{"type":"object","properties":{}},"run":` + run + `}`
		if code, b := post(body); code != 400 || !bytes.Contains([]byte(b), []byte("a built-in tool already exposes")) {
			t.Errorf("non-tool route %s: %d %s", run, code, b)
		}
	}
	// A property that is not a schema object.
	badSchema := `{"name":"my_tool","description":"x","inputs":{"type":"object","properties":{"session":17}},"run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if code, _ := post(badSchema); code != 400 {
		t.Errorf("malformed schema: %d", code)
	}
	// A name already served (ask_agent, the one built-in app).
	dup := `{"name":"ask_agent","description":"x","inputs":{"type":"object","properties":{}},"run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if code, _ := post(dup); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
	// An unknown field (such as the retired effect) is refused.
	bad := `{"name":"my_tool","description":"x","inputs":{"type":"object","properties":{}},"effect":"reads","run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if code, _ := post(bad); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	// Remove a name that is not served.
	if resp, _ := do(t, e.req(t, "DELETE", "/api/v0/tools/no_such", nil)); resp.StatusCode != 404 {
		t.Errorf("remove missing: %d", resp.StatusCode)
	}
	// A turn token cannot manage tools: the route is the main token's.
	r := e.req(t, "GET", "/api/v0/tools", nil)
	r.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	if resp, _ := do(t, r); resp.StatusCode == 200 {
		t.Errorf("turn token reached /api/v0/tools: %d", resp.StatusCode)
	}
}

// A tool's MCP hints come from the ROUTE it runs, not from its name or any
// claim it makes (decision 9: nothing about a tool is trusted). The same name
// on a GET route is read-only and on a POST route is not.
func TestToolHintsFollowTheRoute(t *testing.T) {
	e := mcpEnv(t)
	// A custom tool on a GET route (the turns list): read-only.
	read := `{"name":"my_tool","description":"reads turns","inputs":{"type":"object","properties":{}},"run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
	if resp, body := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(read)))); resp.StatusCode != 201 {
		t.Fatalf("POST my_tool (GET): %d %s", resp.StatusCode, body)
	}
	if a := toolEntry(t, e, token, "my_tool")["annotations"].(map[string]any); a["readOnlyHint"] != true {
		t.Fatalf("a tool on a GET route not read-only: %v", a)
	}
	if resp, body := do(t, e.req(t, "DELETE", "/api/v0/tools/my_tool", nil)); resp.StatusCode != 204 {
		t.Fatalf("DELETE my_tool: %d %s", resp.StatusCode, body)
	}
	// Same name, but it now runs a POST route, so its hint follows that route.
	write := `{"name":"my_tool","description":"starts work now","inputs":{"type":"object","properties":{"agent":{"type":"string"},"prompt":{"type":"string"}},"required":["agent","prompt"]},"run":{"kind":"api","method":"POST","path":"/api/v0/turns","body":true}}`
	if resp, body := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(write)))); resp.StatusCode != 201 {
		t.Fatalf("POST my_tool (POST): %d %s", resp.StatusCode, body)
	}
	if a := toolEntry(t, e, token, "my_tool")["annotations"].(map[string]any); a["readOnlyHint"] != false {
		t.Errorf("a tool on a POST route is still read-only: %v", a)
	}
}

// A custom tool whose id is optional only when it says so: with own_session
// off, a missing id is refused, not filled from the caller's session, so the
// tool cannot act on the caller's session by leaving the id out.
func TestCustomToolNoSilentOwnSession(t *testing.T) {
	e := mcpEnv(t)
	// A tool on the search route (a kept built-in's route) with own_session off
	// and a schema that does not require the id.
	def := `{"name":"peek_session","description":"searches a session","inputs":{"type":"object","properties":{"session":{"type":"string"},"q":{"type":"string"}},"required":["q"]},"run":{"kind":"api","method":"GET","path":"/api/v0/sessions/{id}/search","id_input":"session","query":["q"]}}`
	if resp, body := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(def)))); resp.StatusCode != 201 {
		t.Fatalf("POST peek_session: %d %s", resp.StatusCode, body)
	}
	// Called by a turn of session s1 with no session: refused, never filled in
	// with s1.
	isErr, text := callText(t, e, mcpWriteTok, "peek_session", `{"q":"x"}`)
	if !isErr || !bytes.Contains([]byte(text), []byte("session is required")) {
		t.Errorf("missing id was not refused: isErr=%v text=%q", isErr, text)
	}
}

// Many tool changes at once all land in the served set: no rebuild overwrites
// a newer one with a stale snapshot. Run under -race.
func TestConcurrentToolChangesAllLand(t *testing.T) {
	e := mcpEnv(t)
	letters := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	var wg sync.WaitGroup
	for _, l := range letters {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			body := `{"name":"` + name + `","description":"x","inputs":{"type":"object","properties":{}},"run":{"kind":"api","method":"GET","path":"/api/v0/turns"}}`
			if resp, b := do(t, e.req(t, "POST", "/api/v0/tools", bytes.NewReader([]byte(body)))); resp.StatusCode != 201 {
				t.Errorf("POST %s: %d %s", name, resp.StatusCode, b)
			}
		}("tool_name_" + l)
	}
	wg.Wait()
	served := toolNames(t, e, token)
	for _, l := range letters {
		if !slices.Contains(served, "tool_name_"+l) {
			t.Errorf("tool_name_%s missing from served set %v", l, served)
		}
	}
}
