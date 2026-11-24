package api

// The API as tools: what POST /api/v0/mcp lists and calls (mcp.go). A
// route's tools field says which tools it is, with a name and a description
// written for a model. The input of each tool is built from the route's own
// OpenAPI description (spec.go), so a field added to a request type is in the
// tool with no other edit. One tool per route is the default; POST
// /api/v0/turns is two, since starting a session and continuing one are
// told apart: a turn token starts sessions and never continues one.
//
// The list is discovery only. A call is sent back through ServeHTTP as the
// HTTP request it names, with the caller's own token, so the token check,
// [agent_api], [limits] and the handler decide it exactly as they decide an
// HTTP call. A tool left out of a token's list is still refused by its
// route when it is called.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/run"
)

// tool is one tool a route is. It carries no effect: nothing about a tool is
// trusted, and a call is decided by its NAME against the one rules list (his
// decision, 2026-11-18, DESIGN_0_6_tools.md #9). AgentBus still knows which of
// its OWN routes only read — a GET — and ships those tools as allow rules and
// reads their MCP read-only hint off the method, but that is AgentBus's
// knowledge of its routes, not a field on the tool format.
type tool struct {
	// name is stable: <verb>_<noun>. A model and a recorded tool call name
	// it, so a rename breaks both. No agentbus_ prefix: claude names the
	// tool mcp__agentbus__<name> and codex keeps it under the agentbus
	// server, so the server's name is in front already.
	name string
	// doc is the description a model reads: what it does and its scope
	// first, then what it does not do.
	doc string
	// id is the input that fills the path's {id}: "turn" or "session".
	// ownSession makes it optional for a turn token: its own session.
	id         string
	ownSession bool
	// fields are the request body's fields the tool takes, in order; nil
	// takes them all.
	fields []string
	// required are the inputs the tool must be sent, beyond id.
	required []string
}

// toolRoute is a tool with its route.
type toolRoute struct {
	t  tool
	rt *route
}

// allTools is every tool, in the route table's order. Names are unique: the
// spec refuses to build otherwise (checkTools).
func allTools() []toolRoute {
	var out []toolRoute
	for i := range routes {
		for _, t := range routes[i].tools {
			out = append(out, toolRoute{t, &routes[i]})
		}
	}
	return out
}

// toolName is a tool's name: <verb>_<noun>, lower case.
var toolName = regexp.MustCompile(`^[a-z]+(_[a-z]+)+$`)

// checkTools refuses a tool table that cannot be served: a name used twice, or
// a tool on a route a turn token cannot call, the public or the runner's.
func checkTools() error {
	seen := map[string]bool{}
	for _, tr := range allTools() {
		t, rt := tr.t, tr.rt
		switch {
		case !toolName.MatchString(t.name):
			return fmt.Errorf("api: tool %s must be <verb>_<noun>, in lower case", t.name)
		case seen[t.name]:
			return fmt.Errorf("api: tool %s is named twice", t.name)
		case rt.public || rt.self || rt.scope == scopeNone:
			return fmt.Errorf("api: tool %s is on %s %s, which a turn token cannot call", t.name, rt.method, rt.path)
		case strings.Contains(rt.path, "{id}") != (t.id != ""):
			return fmt.Errorf("api: tool %s must name its {id} input, and only on a path with one", t.name)
		case strings.Contains(rt.path, "{call}") || strings.Contains(rt.path, "{n}"):
			return fmt.Errorf("api: tool %s: a path with {call} or {n} is not a tool yet", t.name)
		case t.doc == "":
			return fmt.Errorf("api: tool %s needs a doc", t.name)
		}
		seen[t.name] = true
	}
	return nil
}

// listedRoute says a tool on rt is in the list a caller sees: every tool for
// the main token, the routes it may call for the turns token, and for a turn
// token the tools its grant, rules and switches allow. Only discovery: the route
// decides a call.
func listedRoute(rt *route, g *run.Grant, turnsOnly bool, rules gateway.Rules, applyAllowed bool) bool {
	switch {
	case turnsOnly:
		return rt.turns
	case g == nil:
		return true
	}
	actions := g.Actions
	if !applyAllowed {
		actions &^= run.ActApply // only discovery: the route decides per workspace
	}
	return turnRefusal(rt, actions, rules) == ""
}

// toolSchemas are the input schemas of every tool, by name, built from
// desc, the OpenAPI description as served. New builds them, so a tool that
// cannot be described stops the service from starting.
func toolSchemas(tools []toolRoute, desc []byte) (map[string]map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(desc, &doc); err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, tr := range tools {
		s, err := inputSchema(doc, tr)
		if err != nil {
			return nil, err
		}
		out[tr.t.name] = s
	}
	return out, nil
}

// inputSchema is a tool's input as one JSON Schema object: its {id}, the
// route's query parameters and the body fields it takes, each with the
// route's own description and constraints. $refs are written out in place,
// since a tool's schema stands alone.
func inputSchema(doc map[string]any, tr toolRoute) (map[string]any, error) {
	t, rt := tr.t, tr.rt
	op, _ := dig(doc, "paths", rt.path, strings.ToLower(rt.method)).(map[string]any)
	if op == nil {
		return nil, fmt.Errorf("api: tool %s: no operation for %s %s", t.name, rt.method, rt.path)
	}
	props := map[string]any{}
	add := func(name string, s any) error {
		if _, dup := props[name]; dup {
			return fmt.Errorf("api: tool %s has two inputs named %s", t.name, name)
		}
		props[name] = s
		return nil
	}
	required := append([]string(nil), t.required...)
	params, _ := op["parameters"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		in, name := pm["in"], pm["name"].(string)
		if in == "header" {
			continue
		}
		s := copyMap(pm["schema"].(map[string]any))
		s["description"] = pm["description"]
		if in == "path" {
			if name != "id" {
				continue // checkTools keeps {call} and {n} out
			}
			name = t.id
			if t.ownSession {
				s["description"] = "A session id. Absent: your own session."
			} else {
				required = append(required, name)
			}
		}
		if err := add(name, s); err != nil {
			return nil, err
		}
	}
	if ref, ok := dig(op, "requestBody", "content", "application/json", "schema", "$ref").(string); ok {
		body, err := resolve(doc, map[string]any{"$ref": ref}, 0)
		if err != nil {
			return nil, err
		}
		bp, _ := body.(map[string]any)["properties"].(map[string]any)
		fields := t.fields
		if fields == nil {
			for k := range bp {
				fields = append(fields, k)
			}
			slices.Sort(fields)
		}
		for _, f := range fields {
			s, ok := bp[f]
			if !ok {
				return nil, fmt.Errorf("api: tool %s takes %s, which the body of %s %s does not have", t.name, f, rt.method, rt.path)
			}
			if err := add(f, s); err != nil {
				return nil, err
			}
		}
		// What the body itself requires, of the fields the tool takes.
		for _, r := range schemaRequired(body.(map[string]any)) {
			if slices.Contains(fields, r) && !slices.Contains(required, r) {
				required = append(required, r)
			}
		}
	}
	for _, r := range required {
		if _, ok := props[r]; !ok {
			return nil, fmt.Errorf("api: tool %s requires %s, which it does not take", t.name, r)
		}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s, nil
}

// dig walks a decoded JSON document by keys.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// resolve writes every "#/components/schemas/" $ref in v out in place.
func resolve(doc map[string]any, v any, depth int) (any, error) {
	if depth > 8 {
		return nil, fmt.Errorf("api: a tool's schema nests too deep")
	}
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			name, ok := strings.CutPrefix(ref, "#/components/schemas/")
			target := dig(doc, "components", "schemas", name)
			if !ok || target == nil {
				return nil, fmt.Errorf("api: a tool's schema names %s, which is not in the description", ref)
			}
			return resolve(doc, target, depth+1)
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := resolve(doc, e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := resolve(doc, e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// schemaRequired are the names a JSON Schema object lists as required.
func schemaRequired(s map[string]any) []string {
	var out []string
	switch r := s["required"].(type) {
	case []string:
		out = r
	case []any:
		for _, x := range r {
			if n, ok := x.(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// queryValue is a JSON value as a query value: a string, a number or a bool.
func queryValue(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return fmt.Sprint(x), true
	}
	return "", false
}

// toolStatus is the text a tool call's answer starts with when it failed.
func toolStatus(code int) string {
	return fmt.Sprintf("HTTP %d %s", code, http.StatusText(code))
}
