package api

// The tool registry served: the built-in apps (the routes that are tools) and
// the person's own, kept in one format by package toolreg. tools.go turns a
// route into a built-in tool; here each stored tool, built-in or custom, is
// resolved to the route it runs and served by one path, so a default removed
// and re-added as a custom tool behaves the same.
//
// toolSnap holds the served set, rebuilt whenever the registry changes and
// read without a lock. The management routes (GET/POST/DELETE /api/v0/tools)
// are control's own: a turn token cannot reach them.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/staysup/agentbus/toolreg"
)

// toolSnapshot is the tools served at one moment.
type toolSnapshot struct {
	list   []servedTool
	byName map[string]servedTool
}

// servedTool is one stored tool resolved for serving: its stored record, the
// route it runs, and its MCP annotations.
type servedTool struct {
	def   toolreg.Tool
	rt    *route
	annot map[string]any
}

// defaultTools expresses the built-in tools in the stored format: each route
// that is a tool becomes one record, with its generated input schema and a
// run that names its route. These are the registry's defaults.
func defaultTools(tools []toolRoute, schemas map[string]map[string]any) []toolreg.Tool {
	out := make([]toolreg.Tool, 0, len(tools))
	for _, tr := range tools {
		out = append(out, toolreg.Tool{
			Name:        tr.t.name,
			Description: tr.t.doc,
			Inputs:      schemas[tr.t.name],
			Where:       toolreg.WhereControl,
			Run: toolreg.Run{
				Kind:       toolreg.RunAPI,
				Method:     tr.rt.method,
				Path:       tr.rt.path,
				IDInput:    tr.t.id,
				OwnSession: tr.t.ownSession,
				Query:      queryNames(tr.rt),
				Body:       tr.rt.body != nil,
			},
		})
	}
	return out
}

// queryNames are a route's query parameters, the tool inputs that go into the
// query string rather than the body.
func queryNames(rt *route) []string {
	if len(rt.query) == 0 {
		return nil
	}
	out := make([]string, 0, len(rt.query))
	for _, p := range rt.query {
		out = append(out, p.name)
	}
	return out
}

// routeIndex is the route table by "METHOD path", built at New. It is a field
// so no function the route table's own initializer reaches refers back to the
// routes variable, which would be an initialization cycle.
func routeIndex() map[string]*route {
	out := make(map[string]*route, len(routes))
	for i := range routes {
		out[routes[i].method+" "+routes[i].path] = &routes[i]
	}
	return out
}

// routeFor is the route with this method and path, or nil.
func (a *API) routeFor(method, path string) *route {
	return a.routes[method+" "+path]
}

// rebuildTools resolves the registry into the served snapshot and stores it.
// A tool whose run names no route is left out, with a line in the service log:
// it cannot be served, but the others still can.
func (a *API) rebuildTools() {
	var list []servedTool
	byName := map[string]servedTool{}
	for _, def := range a.registry.List() {
		st, ok := a.resolveServed(def)
		if !ok {
			if a.cfg.Logf != nil {
				a.cfg.Logf("tool.unserved name=%s method=%s path=%s", def.Name, def.Run.Method, def.Run.Path)
			}
			continue
		}
		list = append(list, st)
		byName[def.Name] = st
	}
	a.toolSnap.Store(&toolSnapshot{list: list, byName: byName})
}

// resolveServed resolves one stored tool for serving: the route its call runs,
// and its MCP annotations. The annotations come from the resolved route's HTTP
// method — a GET only reads — not from any claim the tool makes about itself,
// since nothing about a tool is trusted (his decision, DESIGN_0_6_tools.md #9).
func (a *API) resolveServed(def toolreg.Tool) (servedTool, bool) {
	if def.Run.Kind == toolreg.RunApp {
		// An app has no single route; it runs several (apps.go). It is not
		// read-only: it does something, so the MCP hint says so.
		return servedTool{def: def, rt: nil, annot: annotations(false)}, true
	}
	rt := a.routeFor(def.Run.Method, def.Run.Path)
	if rt == nil || !routeServableAsTool(rt) {
		// The route is gone, or not servable as a tool (it ships no default
		// tool and is not wrappable, like the hidden file routes). A stored
		// custom tool on such a route is dropped on read, the same rule
		// createTool applies to a new one, so a tool persisted before the
		// route stopped being a tool cannot keep being served.
		return servedTool{}, false
	}
	return servedTool{def: def, rt: rt, annot: annotations(rt.method == http.MethodGet)}, true
}

// routeServableAsTool says whether an api tool may run this route: it ships a
// default tool, or it is wrappable. The one rule createTool and resolveServed
// share, so a route that stops being a tool stops serving both new and stored
// tools alike.
func routeServableAsTool(rt *route) bool {
	return len(rt.tools) > 0 || rt.wrappable
}

// servedByName is the tool named name, served or not listed to this caller.
func (a *API) servedByName(name string) (servedTool, bool) {
	st, ok := a.toolSnap.Load().byName[name]
	return st, ok
}

// toolRequestFrom is the HTTP request a tool call names, built from the stored
// run and the call's arguments: the method, the path with {id} filled, the
// query and the body. own is the caller's session, for a tool whose session is
// optional, or "". It says why not in fixed words that name the input, never
// its value, exactly as the built-in path did.
func toolRequestFrom(def toolreg.Tool, args map[string]any, own string) (method, target string, body []byte, why string) {
	run := def.Run
	props, _ := def.Inputs["properties"].(map[string]any)
	for _, r := range schemaRequired(def.Inputs) {
		if v, ok := args[r]; !ok || v == nil {
			if r == run.IDInput && run.OwnSession && own != "" {
				continue
			}
			return "", "", nil, r + " is required"
		}
	}
	query := map[string]bool{}
	for _, q := range run.Query {
		query[q] = true
	}
	path := run.Path
	q := url.Values{}
	var fields map[string]any
	for name, v := range args {
		if _, ok := props[name]; !ok {
			return "", "", nil, "this tool takes no input of that name; tools/list gives its inputs"
		}
		switch {
		case run.IDInput != "" && name == run.IDInput:
			s, ok := v.(string)
			if !ok || !sessionID.MatchString(s) {
				return "", "", nil, name + " must be an id of letters, digits, _ and -"
			}
			path = strings.Replace(path, "{id}", s, 1)
		case query[name]:
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
		// The caller's own session fills {id} only when the tool allows it
		// (own_session). Without that, a missing id is refused, never filled
		// in: a tool that does not opt in can never act on the caller's
		// session by leaving the id out.
		if !run.OwnSession || own == "" {
			return "", "", nil, run.IDInput + " is required"
		}
		path = strings.Replace(path, "{id}", own, 1)
	}
	if run.Body {
		if fields == nil {
			fields = map[string]any{}
		}
		body, _ = json.Marshal(fields)
	}
	target = path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	return run.Method, target, body, ""
}

// The management routes: control's own. A turn token cannot reach them.

// ToolSummary is one tool as the list carries it: facts only. The 0.6 rule is
// that a list carries facts, not content, so the inputs schema and the run are
// left out here and loaded from GET /api/v0/tools/{id} instead.
type ToolSummary struct {
	Name        string `json:"name" doc:"The tool's name: a model calls it by this name."`
	Description string `json:"description" doc:"What the tool does, written for a model."`
	Where       string `json:"where" doc:"Where the call runs: control."`
}

// ToolList is the answer to GET /api/v0/tools: the tools as facts only.
type ToolList struct {
	Tools []ToolSummary `json:"tools" doc:"The tools, built-in and custom, in a stable order, as facts only. A tool's full definition (inputs and run) is at GET /api/v0/tools/{id}."`
}

const (
	badTool    = "the body must be one JSON object: a tool with name, description, inputs, where and run"
	noSuchTool = "no such tool"
	toolRoute0 = "run must name a route a built-in tool already exposes; another route (one that is not a tool, has a {call}/{n} path, or needs a header such as If-Match) cannot be served as a tool"
)

// GET /api/v0/tools: the tools as facts only; each one's full definition is at
// GET /api/v0/tools/{id}.
func (a *API) listTools(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	out := []ToolSummary{}
	for _, d := range a.registry.List() {
		// A stored tool whose route is not servable as a tool (a hidden route,
		// or one since removed) is not served (resolveServed) and not listed,
		// so a folder tool persisted from before 0.6.5 does not show here.
		if d.Run.Kind == toolreg.RunAPI {
			if rt := a.routeFor(d.Run.Method, d.Run.Path); rt == nil || !routeServableAsTool(rt) {
				continue
			}
		}
		out = append(out, ToolSummary{Name: d.Name, Description: d.Description, Where: d.Where})
	}
	writeJSON(w, http.StatusOK, ToolList{out})
}

// GET /api/v0/tools/{id}: one tool's full definition (id is the tool's name).
func (a *API) getTool(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	t, err := a.registry.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, noSuchTool)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// POST /api/v0/tools
func (a *API) createTool(w http.ResponseWriter, r *http.Request) {
	var t toolreg.Tool
	if !decodeObject(r, &t) {
		writeError(w, http.StatusBadRequest, badTool)
		return
	}
	// An api tool may run only a route marked servable as a tool: one that
	// ships a default tool, or one marked wrappable (the read routes, whose
	// default tool copies retired in 0.6 but which stay open to a custom tool).
	// This one rule is what keeps a custom tool servable: it rejects a route
	// that is not a tool at all, a path with {call} or {n} (never a tool's
	// route), and a route that needs a header the stored format cannot supply
	// (such as a settings write's If-Match) — none of which is marked. The call
	// still runs with the caller's own token, so a custom tool can reach nothing
	// the caller could not.
	if t.Run.Kind == toolreg.RunAPI {
		if rt := a.routeFor(t.Run.Method, t.Run.Path); rt == nil || !routeServableAsTool(rt) {
			writeError(w, http.StatusBadRequest, toolRoute0)
			return
		}
	}
	// An app's workflow is validated against the route table: the spec-aware
	// half toolreg.Check cannot do (apps.go). A step's operation must exist, be
	// callable by a turn token, and its fields and outputs must be real.
	if t.Run.Kind == toolreg.RunApp {
		if why := a.checkApp(t); why != "" {
			writeError(w, http.StatusBadRequest, why)
			return
		}
	}
	// The change and the rebuild it triggers are one critical section, so two
	// changes at once cannot publish a snapshot that misses one.
	a.toolsMu.Lock()
	saved, err := a.registry.Add(t)
	if err == nil {
		a.rebuildTools()
	}
	a.toolsMu.Unlock()
	if a.toolFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// DELETE /api/v0/tools/{id}: id is the tool's name.
func (a *API) removeTool(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	a.toolsMu.Lock()
	err := a.registry.Remove(id)
	if err == nil {
		a.rebuildTools()
	}
	a.toolsMu.Unlock()
	if a.toolFailed(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toolFailed answers err, if there is one, and says whether it did.
func (a *API) toolFailed(w http.ResponseWriter, err error) bool {
	var bad *toolreg.Invalid
	switch {
	case err == nil:
		return false
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Why)
	case errors.Is(err, toolreg.ErrExists):
		writeError(w, http.StatusConflict, "a tool of that name already exists; remove it first")
	case errors.Is(err, toolreg.ErrFull):
		writeError(w, http.StatusConflict, "too many tools; remove some first")
	case errors.Is(err, toolreg.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchTool)
	default:
		a.internal(w)
	}
	return true
}
