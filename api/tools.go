package api

// The API as tools: what POST /api/v0/mcp lists and calls (mcp.go). A
// route's tools field says which tools it is, with a name and a description
// written for a model. The input of each tool is built from the route's own
// OpenAPI description (spec.go), so a field added to a request type is in the
// tool with no other edit. One tool per route is the default; POST
// /api/v0/turns is two, since starting a session and continuing one are
// allowed apart ([agent_api] start and continue).
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
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// What a tool changes, for a model and a screen (the x-agentbus-tools of the
// OpenAPI description, and the tool's MCP annotations).
const (
	effectNone   = "none"                       // it only reads
	effectStarts = "starts work"                // a new turn, by another agent
	effectStops  = "stops work"                 // a turn ends
	effectApply  = "writes the person's folder" // a turn's changes are applied
	effectRename = "renames a session"
)

// tool is one tool a route is.
type tool struct {
	// name is stable: agentbus_<verb>_<noun>. A model and a recorded tool
	// call name it, so a rename breaks both.
	name string
	// doc is the description a model reads: what it does and its scope
	// first, then what it does not do.
	doc    string
	effect string
	// id is the input that fills the path's {id}: "turn" or "session".
	// ownSession makes it optional for a turn token: its own session.
	id         string
	ownSession bool
	// fields are the request body's fields the tool takes, in order; nil
	// takes them all.
	fields []string
	// required are the inputs the tool must be sent, beyond id.
	required []string
	// need is the [agent_api] switch the tool needs beyond its route's own
	// check: "start" or "continue", for the two tools of POST /api/v0/turns.
	need string
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

// checkTools refuses a tool table that cannot be served: a name used twice, or
// a tool on a route a turn token cannot call, the public or the runner's.
func checkTools() error {
	seen := map[string]bool{}
	for _, tr := range allTools() {
		t, rt := tr.t, tr.rt
		switch {
		case !strings.HasPrefix(t.name, "agentbus_"):
			return fmt.Errorf("api: tool %s must start with agentbus_", t.name)
		case seen[t.name]:
			return fmt.Errorf("api: tool %s is named twice", t.name)
		case rt.public || rt.self || rt.turn == mainOnly:
			return fmt.Errorf("api: tool %s is on %s %s, which a turn token cannot call", t.name, rt.method, rt.path)
		case strings.Contains(rt.path, "{id}") != (t.id != ""):
			return fmt.Errorf("api: tool %s must name its {id} input, and only on a path with one", t.name)
		case strings.Contains(rt.path, "{call}") || strings.Contains(rt.path, "{n}"):
			return fmt.Errorf("api: tool %s: a path with {call} or {n} is not a tool yet", t.name)
		case t.doc == "" || t.effect == "":
			return fmt.Errorf("api: tool %s needs a doc and an effect", t.name)
		}
		seen[t.name] = true
	}
	return nil
}

// toolNamed is the tool named name, of every tool, listed or not.
func (a *API) toolNamed(name string) (toolRoute, bool) {
	for _, tr := range a.tools {
		if tr.t.name == name {
			return tr, true
		}
	}
	return toolRoute{}, false
}

// listed says t is in the list a caller sees: every tool for the main token,
// the routes it may call for the turns token, and for a turn token the tools
// its grant and [agent_api] allow. Only discovery: the route decides a call.
func listed(tr toolRoute, g *run.Grant, turnsOnly bool, api settings.AgentAPI) bool {
	switch {
	case turnsOnly:
		return tr.rt.turns
	case g == nil:
		return true
	}
	if turnRefusal(tr.rt.turn, *g, api) != "" {
		return false
	}
	switch tr.t.need {
	case "start":
		return *api.Start
	case "continue":
		return *api.Continue && api.Scope != settings.ScopeSession
	}
	return true
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

// toolRequest is the HTTP request a tool call names: its method, its path
// with {id} filled, its query and its body. own is the caller's session, for
// a tool whose session is optional, or "". It says why not, in fixed words
// that name the input, never its value.
func toolRequest(tr toolRoute, schema map[string]any, args map[string]any, own string) (method, target string, body []byte, why string) {
	t, rt := tr.t, tr.rt
	props, _ := schema["properties"].(map[string]any)
	for _, r := range schemaRequired(schema) {
		if v, ok := args[r]; !ok || v == nil {
			if r == t.id && t.ownSession && own != "" {
				continue
			}
			return "", "", nil, r + " is required"
		}
	}
	path := rt.path
	q := url.Values{}
	var fields map[string]any
	for name, v := range args {
		if _, ok := props[name]; !ok {
			return "", "", nil, "this tool takes no input of that name; tools/list gives its inputs"
		}
		switch {
		case name == t.id:
			s, ok := v.(string)
			if !ok || !sessionID.MatchString(s) {
				return "", "", nil, name + " must be an id of letters, digits, _ and -"
			}
			path = strings.Replace(path, "{id}", s, 1)
		case isQuery(rt, name):
			s, ok := queryValue(v)
			if !ok {
				return "", "", nil, name + " must be a string or a number"
			}
			q.Set(name, s)
		default:
			if fields == nil {
				fields = map[string]any{}
			}
			fields[name] = v
		}
	}
	if strings.Contains(path, "{id}") {
		if own == "" {
			return "", "", nil, t.id + " is required"
		}
		path = strings.Replace(path, "{id}", own, 1)
	}
	if rt.body != nil {
		if fields == nil {
			fields = map[string]any{}
		}
		body, _ = json.Marshal(fields)
	}
	target = path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	return rt.method, target, body, ""
}

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

func isQuery(rt *route, name string) bool {
	for _, p := range rt.query {
		if p.name == name {
			return true
		}
	}
	return false
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
