package api

// B15: an app is a tool whose run is an Arazzo workflow — several AgentBus API
// operations in order, each step's outputs passed to the next (toolreg). This
// file is the app's two halves on control:
//
//   - checkApp validates a stored app against the route table at save, the part
//     toolreg cannot do on its own: the operationId exists, a turn token may
//     call it, and every step field and output names a real part of that
//     operation.
//   - runApp runs the workflow when the app is called: it gates the app by its
//     name (as any tool), then runs each step as a normal API call with the
//     caller's own token, deriving each write step's idempotency key from the
//     tool call so a retried app call is a no-op. A failed step stops the app
//     and says which. Nothing waits: these steps are quick, fire-and-return.
//
// An app reaches nothing the caller could not: each step is the HTTP request the
// caller could make by hand, authorized by its own route (allow), recorded
// like any call. Control holds no app-specific knowledge beyond this runner.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/toolreg"
)

// defaultApps are the apps AgentBus ships, expressed as data like any custom
// app (his call, 2026-11-19: core holds no app-specific code). ask_agent and
// review_turn were special core paths until B15; now they are two registry
// records with an Arazzo workflow. A person may remove or replace either, as any
// default. They are built in code, but checkApp validates them at New so an
// edit to a route they step through cannot leave a default app unservable.
func defaultApps() []toolreg.Tool {
	return []toolreg.Tool{askAgentApp(), reviewTurnApp()}
}

// reviewTurnApp is review_turn: ask another agent to review a turn, then deliver
// its reply (B15). Review is ask_agent with a review prompt; the verdict IS the
// delivered reply — core parses nothing. The reviewer runs as a normal asked
// turn and reads the reviewed turn and its diff through agentbus-api, rather
// than running in a copy of that turn's workspace: a review is a turn, not a
// second kind of turn.
func reviewTurnApp() toolreg.Tool {
	return toolreg.Tool{
		Name: "review_turn",
		Description: "Ask a saved AgentBus agent (any vendor) to review your changes. " +
			"With no turn it reviews your workspace's whole unapplied set (every change applying would write to the person's folder, across turns) — this is what you want for \"review my changes\". " +
			"With a turn (its id) it reviews that one turn's diff instead. " +
			"The reviewer reads the changes through agentbus-api and replies with its findings. " +
			"The reply comes back to your session as a later turn: do not wait or poll for it. " +
			"Review with a different vendor than wrote the work, so a reviewer does not share the author's blind spots.",
		Inputs: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"turn":  map[string]any{"type": "string", "description": "Optional. One turn to review (its id). Omit to review your workspace's whole unapplied set."},
				"agent": map[string]any{"type": "string", "description": "The saved agent to review it (from GET /api/v0/agents)."},
			},
			"required":             []any{"agent"},
			"additionalProperties": false,
		},
		Where: toolreg.WhereControl,
		Run: toolreg.Run{Kind: toolreg.RunApp, App: &toolreg.App{
			Steps: []toolreg.Step{
				// Two calls in sequence (his API rules, 2026-11-21): a session in
				// the reviewer's workspace (the asker's, by the grant), then a turn
				// in it. The reviewer reads the changes through agentbus-api, not a
				// copy. The session step hands on its workspace (the asker's own, by
				// the grant) so the no-turn prompt can name it. startTurn records the
				// delivery on in the same write, and answers at once.
				{StepID: "session", OperationID: "createSession",
					RequestBody: map[string]any{},
					Outputs:     map[string]string{"session": "$response.body#/session", "workspace": "$response.body#/workspace"}},
				{StepID: "start", OperationID: "startTurn",
					RequestBody: map[string]any{
						"session":  "$steps.session.outputs.session",
						"agent":    "$inputs.agent",
						"delivery": "$caller.session",
						// A named turn reviews that turn's diff; no turn reviews the
						// workspace's whole unapplied set (the same GET the Apply panel
						// shows). The reviewer's token may read its own workspace's
						// changes (atWorkspace), and its workspace is the asker's.
						"prompt": map[string]any{
							"if_present": "$inputs.turn",
							"then": "Review a change another agent made in turn {$inputs.turn}. Read the turn at " +
								"GET /api/v0/turns/{$inputs.turn} and its diff at GET /api/v0/turns/{$inputs.turn}/diff through " +
								"agentbus-api (its base URL is $AGENTBUS_URL, its token $AGENTBUS_TOKEN). " +
								"If the ended turn's diff has files null, it ran in a direct workspace: " + reviewDirect +
								"Reply with your findings, most serious first, and say plainly whether the change is safe to apply.",
							"else": "Review the changes another agent made in your workspace and has not applied yet — " +
								"the whole set applying would write to the person's folder. Read them at " +
								"GET /api/v0/workspaces/{$steps.session.outputs.workspace}/changes through " +
								"agentbus-api (its base URL is $AGENTBUS_URL, its token $AGENTBUS_TOKEN): " +
								"each changed file with its diff, across every unapplied turn. " +
								"If it answers isolated false, the workspace is direct: " + reviewDirect +
								"Reply with your findings, most serious first, and say plainly whether the changes are safe to apply.",
						},
					},
					Outputs: map[string]string{"turn": "$response.body#/turn", "session": "$response.body#/session"}},
			},
			Outputs: map[string]string{"turn": "$steps.start.outputs.turn", "session": "$steps.start.outputs.session"},
		}},
	}
}

// reviewDirect is the review prompt's word on a direct workspace (sandbox
// false): its turns change the person's folder in place and AgentBus keeps no
// record of those changes, so there is no diff to read from it.
const reviewDirect = "AgentBus keeps no record of its changes and gives you no diff. " +
	"The changes are already in the folder you run in, so find them there yourself. "

// askAgentApp is ask_agent: start a turn on another agent, then register the on
// that delivers its reply to the asking session. Two steps over the public API,
// what the old route-plus-auto-on did, now as data.
func askAgentApp() toolreg.Tool {
	return toolreg.Tool{
		Name: "ask_agent",
		Description: "Ask a saved AgentBus agent (from GET /api/v0/agents, any vendor) to do work. " +
			"It starts a new session on that agent, in your workspace's copy of your folder (the same copy your turn runs in). " +
			"The reply names the new turn and session at once; its result comes back to your session as a later turn: do not wait or poll for it. " +
			"This is not your harness's own subagent: it is recorded, and can be another vendor. " +
			"Each ask is a new session: to follow up, ask again with what the result said.",
		Inputs: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent":  map[string]any{"type": "string", "description": "The saved agent to ask (its id, from GET /api/v0/agents)."},
				"prompt": map[string]any{"type": "string", "description": "What to ask it to do."},
			},
			"required":             []any{"agent", "prompt"},
			"additionalProperties": false,
		},
		Where: toolreg.WhereControl,
		Run: toolreg.Run{Kind: toolreg.RunApp, App: &toolreg.App{
			Steps: []toolreg.Step{
				// Two calls in sequence (his API rules, 2026-11-21): create a
				// session in the asker's workspace, then run a turn in it. A turn
				// token's createSession takes the workspace from the grant, so the
				// asked session shares the asker's copy. startTurn records the
				// delivery on in the same write (delivery=$caller.session), so
				// start and deliver stay atomic.
				{StepID: "session", OperationID: "createSession",
					RequestBody: map[string]any{},
					Outputs:     map[string]string{"session": "$response.body#/session"}},
				{StepID: "start", OperationID: "startTurn",
					RequestBody: map[string]any{"session": "$steps.session.outputs.session",
						"agent": "$inputs.agent", "prompt": "$inputs.prompt", "delivery": "$caller.session"},
					Outputs: map[string]string{"turn": "$response.body#/turn", "session": "$response.body#/session"}},
			},
			Outputs: map[string]string{"turn": "$steps.start.outputs.turn", "session": "$steps.start.outputs.session"},
		}},
	}
}

// opIndex is the route table by operationId, built at New so an app's step
// resolves the operation it names without the route table referring to itself.
func opIndex() map[string]*route {
	out := make(map[string]*route, len(routes))
	for i := range routes {
		if routes[i].op != "" {
			out[routes[i].op] = &routes[i]
		}
	}
	return out
}

// structFields are the JSON field names of v's struct type (a pointer is
// followed), or nil when v is nil or not a struct. It reuses the spec's own
// field walk, so it sees exactly the fields the API describes.
func structFields(v any) map[string]bool {
	if v == nil {
		return nil
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	out := map[string]bool{}
	for _, f := range fieldsOf(t) {
		if n := jsonName(f); n != "" && n != "-" {
			out[n] = true
		}
	}
	return out
}

// successFields are the JSON fields of an operation's success reply (the first
// 2xx reply with a body), or nil when it has none to check against (a stream, a
// raw body, or an empty reply).
func successFields(rt *route) map[string]bool {
	for _, r := range rt.replies {
		if r.status >= 200 && r.status < 300 && r.body != nil {
			return structFields(r.body)
		}
	}
	return nil
}

// checkApp validates an app against the route table at save: the spec-aware
// half of validation (toolreg.Check does the structural half). It returns "" or
// why the app is refused, in words for the person.
func (a *API) checkApp(def toolreg.Tool) string {
	if def.Run.App == nil {
		return "an app needs run.app"
	}
	for _, s := range def.Run.App.Steps {
		rt := a.byOp[s.OperationID]
		if rt == nil {
			return "step " + strconv.Quote(s.StepID) + ": no API operation named " + s.OperationID
		}
		if rt.public || rt.self || rt.at == atControl || !run.TurnActions.Has(rt.action) {
			return "step " + strconv.Quote(s.StepID) + ": operation " + s.OperationID + " cannot be called with a turn's own token"
		}
		// The app runtime fills only {id} in a path. An operation with another
		// path parameter ({call}, {n}) would 404 at run time, so it is refused
		// at save rather than accepted and always broken (found in review
		// 2026-11-19). checkTools refuses the same for a single-operation tool.
		if strings.Contains(rt.path, "{call}") || strings.Contains(rt.path, "{n}") {
			return "step " + strconv.Quote(s.StepID) + ": operation " + s.OperationID + " has a path an app cannot fill (only {id} is supported)"
		}
		bodyFields := structFields(rt.body)
		for f := range s.RequestBody {
			if !bodyFields[f] {
				return "step " + strconv.Quote(s.StepID) + ": " + s.OperationID + " has no body field " + strconv.Quote(f)
			}
		}
		hasID := strings.Contains(rt.path, "{id}")
		params := map[string]bool{}
		if hasID {
			params["id"] = true
		}
		for _, p := range rt.query {
			params[p.name] = true
		}
		for p := range s.Parameters {
			if !params[p] {
				return "step " + strconv.Quote(s.StepID) + ": " + s.OperationID + " has no parameter " + strconv.Quote(p)
			}
		}
		if hasID {
			if _, ok := s.Parameters["id"]; !ok {
				return "step " + strconv.Quote(s.StepID) + ": " + s.OperationID + " needs an id parameter"
			}
		}
		resp := successFields(rt)
		for name, ptr := range s.Outputs {
			ref, _ := toolreg.ParseRef(ptr)
			top := firstSegment(ref.A)
			if resp != nil && top != "" && !resp[top] {
				return "step " + strconv.Quote(s.StepID) + " output " + strconv.Quote(name) + ": the reply of " + s.OperationID + " has no field " + strconv.Quote(top)
			}
		}
	}
	return ""
}

// runApp runs an app's workflow: gate by name, then each step with the caller's
// own token, outputs passed forward. It returns the app's outputs as the tool
// result, or a failure naming the step that stopped it.
func (a *API) runApp(r *http.Request, st servedTool, args map[string]any, callID string) toolResult {
	// Validate the call against the app's input schema before anything runs, as
	// the single-operation tool path does (tools_registry.go): an unknown input
	// or a missing required one is refused here, not carried into a step. This
	// runs before the gate, like the route tool's own check, so a malformed call
	// is never held.
	if why := validateArgs(st.def.Inputs, args); why != "" {
		return toolFail(why)
	}
	c := callerOf(r)
	g := &c.Grant
	// Apps are for agents only (his call, 2026-11-19): an app's steps need a
	// turn's folder and session, which only a turn token carries. A call that
	// names no turn (the main token, the turns token) is refused here as well
	// as hidden from the list, so neither can run one.
	if g.Turn == "" {
		return toolFail("this app runs only for an agent's turn; use the AgentBus API directly instead")
	}
	if ok, result := a.gateAppCall(r, c, st, args); !ok {
		return result
	}
	caller := g.Session
	stepOut := map[string]map[string]any{}
	for _, s := range st.def.Run.App.Steps {
		rt := a.byOp[s.OperationID]
		if rt == nil {
			return toolFail("app " + st.def.Name + ": step " + strconv.Quote(s.StepID) + " names no operation")
		}
		body, params, why := a.resolveStep(s, args, caller, stepOut)
		if why != "" {
			return toolFail("app " + st.def.Name + " step " + strconv.Quote(s.StepID) + ": " + why)
		}
		// Derive this write step's idempotency key from the tool call, so a
		// retried app call returns the same turn and the same on (his call,
		// 2026-11-19). Only when the operation takes one and the author set none.
		if structFields(rt.body)["idempotency_key"] {
			if _, set := body["idempotency_key"]; !set {
				body["idempotency_key"] = deriveKey(g, st.def.Name, callID, args, s.StepID)
			}
		}
		code, text, why := a.runStep(r, rt, body, params)
		if why != "" {
			return toolFail("app " + st.def.Name + " step " + strconv.Quote(s.StepID) + ": " + why)
		}
		if code >= 400 {
			return toolFail("app " + st.def.Name + " failed at step " + strconv.Quote(s.StepID) +
				" (" + s.OperationID + "): " + toolStatus(code) + ": " + strings.TrimSpace(text))
		}
		outs, why := stepOutputs(s, text)
		if why != "" {
			return toolFail("app " + st.def.Name + " step " + strconv.Quote(s.StepID) + ": " + why)
		}
		stepOut[s.StepID] = outs
	}
	result := map[string]any{}
	for name, expr := range st.def.Run.App.Outputs {
		if ref, ok := toolreg.ParseRef(expr); ok && ref.Kind == toolreg.RefSteps {
			result[name] = stepOut[ref.A][ref.B]
		}
	}
	out, err := json.Marshal(result)
	if err != nil {
		return toolFail("the app's result could not be built")
	}
	return toolResult{Content: []textContent{{"text", string(out)}}}
}

// validateArgs checks a tool call against the app's input schema: every
// argument must be a declared input, and every required input must be present.
// It is the structural check the serving path does for a route tool
// (toolRequestFrom), applied to an app before its steps run.
func validateArgs(inputs, args map[string]any) string {
	props, _ := inputs["properties"].(map[string]any)
	for name := range args {
		if _, ok := props[name]; !ok {
			return "this app takes no input of that name; tools/list gives its inputs"
		}
	}
	for _, r := range schemaRequired(inputs) {
		if v, ok := args[r]; !ok || v == nil {
			return r + " is required"
		}
	}
	return ""
}

// gateAppCall decides an app call by its name, as the tool gate decides any
// tool call (toolgate.go), and holds it if the rules say Ask. It does NOT run
// the per-route authorize the single-operation gate does: an app has no one
// route, and each step authorizes itself through its own route when it runs. A
// call that is not an agent's own (no turn token) is not gated here.
func (a *API) gateAppCall(r *http.Request, c *caller, st servedTool, args map[string]any) (bool, toolResult) {
	if !c.limits.gated() {
		return true, toolResult{}
	}
	g := &c.Grant
	rules := c.toolRules()
	switch dec, rule := gateway.DecideTool(rules, st.def.Name); dec {
	case gateway.Refuse:
		a.recordToolRefused(g, st, args, rule)
		return false, toolFail(refusedToolText(rule))
	case gateway.Ask:
		return a.holdToolCall(r, g, st, args, rules.Ask)
	default:
		return true, toolResult{}
	}
}

// resolveStep resolves a step's request body and parameters: each value is a
// literal, a full $... expression, or a string with {$...} embedded. It reads
// $inputs from args, $steps from earlier step outputs and $caller.session.
func (a *API) resolveStep(s toolreg.Step, args map[string]any, caller string, stepOut map[string]map[string]any) (body, params map[string]any, why string) {
	body = map[string]any{}
	for f, v := range s.RequestBody {
		rv, why := resolveValue(v, args, caller, stepOut)
		if why != "" {
			return nil, nil, "requestBody." + f + ": " + why
		}
		// An expression that resolves to nothing (an absent optional input)
		// omits the field, rather than sending JSON null, which a typed field
		// would refuse.
		if rv == nil {
			continue
		}
		body[f] = rv
	}
	params = map[string]any{}
	for p, v := range s.Parameters {
		rv, why := resolveValue(v, args, caller, stepOut)
		if why != "" {
			return nil, nil, "parameters." + p + ": " + why
		}
		if rv == nil {
			continue
		}
		params[p] = rv
	}
	return body, params, ""
}

// resolveValue resolves one request value. A non-string is a literal. A string
// that is one whole expression resolves to that expression's value (any type);
// a string with {$...} parts has each substituted as text.
func resolveValue(v any, args map[string]any, caller string, stepOut map[string]map[string]any) (any, string) {
	// A conditional value chooses "then" or "else" by whether "if_present"
	// resolves to a non-empty string (toolreg's grammar). review_turn uses it:
	// a named turn picks the turn's diff, an absent one the workspace's whole
	// unapplied set.
	if c, ok := toolreg.AsConditional(v); ok {
		cond, why := resolveValue(c["if_present"], args, caller, stepOut)
		if why != "" {
			return nil, why
		}
		if s, ok := cond.(string); ok && s != "" {
			return resolveValue(c["then"], args, caller, stepOut)
		}
		return resolveValue(c["else"], args, caller, stepOut)
	}
	s, ok := v.(string)
	if !ok {
		return v, ""
	}
	if toolreg.IsFullExpr(s) {
		return resolveRef(s, args, caller, stepOut)
	}
	// One pass over the original string, so a value holding a {$...} is not
	// re-scanned as another input's placeholder (found in review 2026-11-19).
	why := ""
	out := toolreg.Interpolate(s, func(expr string) string {
		if why != "" {
			return ""
		}
		rv, w := resolveRef(expr, args, caller, stepOut)
		if w != "" {
			why = w
			return ""
		}
		return asText(rv)
	})
	if why != "" {
		return nil, why
	}
	return out, ""
}

// resolveRef resolves one $... reference to its value.
func resolveRef(expr string, args map[string]any, caller string, stepOut map[string]map[string]any) (any, string) {
	ref, ok := toolreg.ParseRef(expr)
	if !ok {
		return nil, expr + " is not a valid expression"
	}
	switch ref.Kind {
	case toolreg.RefInputs:
		return args[ref.A], ""
	case toolreg.RefSteps:
		return stepOut[ref.A][ref.B], ""
	case toolreg.RefCaller:
		return caller, ""
	}
	return nil, expr + " cannot be used in a request value"
}

// runStep makes one step's HTTP call with the caller's own token. It builds the
// request through toolRequestFrom — the ONE hardened path the single-operation
// tool uses (tools_registry.go) — rather than a copy of the path, query and body
// shaping. His call, 2026-11-19: two review rounds found request-building gaps in
// this file (an unvalidated id turning a stop into an apply, among them), so the
// mechanism is unified, not patched again. The id-shape check is toolRequestFrom's
// own; checkApp has already validated the step's fields and operation at save.
func (a *API) runStep(r *http.Request, rt *route, body, params map[string]any) (code int, text, why string) {
	args := make(map[string]any, len(body)+len(params))
	for k, v := range body {
		args[k] = v
	}
	for k, v := range params {
		args[k] = v
	}
	method, target, raw, why := toolRequestFrom(stepTool(rt), args, "")
	if why != "" {
		return 0, "", why
	}
	code, text = a.serveStep(r, method, target, raw)
	return code, text, ""
}

// stepTool expresses a step's operation as a synthetic api tool, so a step runs
// through the same request-builder as a single-operation tool. Its inputs name
// the fields the operation accepts — the id (when the path has one), the query
// parameters and the body fields — so toolRequestFrom shapes the request, checks
// the id shape and refuses an unknown field, all in one place.
func stepTool(rt *route) toolreg.Tool {
	run := toolreg.Run{Kind: toolreg.RunAPI, Method: rt.method, Path: rt.path, Query: queryNames(rt), Body: rt.body != nil}
	props := map[string]any{}
	if strings.Contains(rt.path, "{id}") {
		run.IDInput = "id"
		props["id"] = map[string]any{"type": "string"}
	}
	for _, p := range rt.query {
		props[p.name] = map[string]any{"type": "string"}
	}
	for f := range structFields(rt.body) {
		props[f] = map[string]any{"type": "string"}
	}
	return toolreg.Tool{Inputs: map[string]any{"type": "object", "properties": props}, Run: run}
}

// serveStep sends one request back through the handler, as callTool does for a
// single-operation tool: the caller's Authorization, Host and address, so the
// token check, [agent_api] and the handler decide it as any HTTP call.
func (a *API) serveStep(r *http.Request, method, target string, body []byte) (int, string) {
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	inner, err := http.NewRequestWithContext(r.Context(), method, target, rd)
	if err != nil {
		return 0, ""
	}
	inner.Host, inner.RemoteAddr = r.Host, r.RemoteAddr
	inner.Header.Set("Authorization", r.Header.Get("Authorization"))
	if ua := r.Header.Get("User-Agent"); ua != "" {
		inner.Header.Set("User-Agent", ua)
	}
	if body != nil {
		inner.Header.Set("Content-Type", "application/json")
	}
	rec := &toolRecorder{header: http.Header{}}
	http.Handler(a).ServeHTTP(rec, inner)
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	return rec.code, rec.body.String()
}

// stepOutputs extracts a step's declared outputs from its reply body by their
// JSON Pointers. A pointer that finds nothing yields a null, as Arazzo does.
func stepOutputs(s toolreg.Step, text string) (map[string]any, string) {
	if len(s.Outputs) == 0 {
		return map[string]any{}, ""
	}
	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, "the reply is not JSON, so its outputs cannot be read"
	}
	out := map[string]any{}
	for name, ptr := range s.Outputs {
		ref, _ := toolreg.ParseRef(ptr)
		v, _ := jsonPointer(doc, ref.A)
		out[name] = v
	}
	return out, ""
}

// deriveKey is a write step's idempotency key, from the tool call: the caller's
// turn, the tool name, the MCP request id (callID), the call arguments and the
// step id. The callID is what tells a retry from a fresh call: a client resends
// a failed request with the SAME JSON-RPC id, so a retry reproduces the key and
// the write returns what it made the first time, while two separate calls (two
// different ids) get two keys and two turns — even when their arguments match
// (found in review 2026-11-19).
func deriveKey(g *run.Grant, tool, callID string, args map[string]any, stepID string) string {
	h := sha256.New()
	if g != nil {
		io.WriteString(h, g.Turn)
	}
	h.Write([]byte{0})
	io.WriteString(h, tool)
	h.Write([]byte{0})
	io.WriteString(h, callID)
	h.Write([]byte{0})
	// json.Marshal sorts a map's keys, so the same arguments hash the same.
	canon, _ := json.Marshal(args)
	h.Write(canon)
	h.Write([]byte{0})
	io.WriteString(h, stepID)
	return "app-" + hex.EncodeToString(h.Sum(nil))[:24]
}

// jsonPointer reads an RFC 6901 pointer (such as /turn, /changes/count or
// /agents/0/id) from a decoded JSON value: an object field by name, or an array
// element by its index.
func jsonPointer(v any, ptr string) (any, bool) {
	if ptr == "" || ptr == "/" {
		return v, true
	}
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch c := v.(type) {
		case map[string]any:
			e, ok := c[seg]
			if !ok {
				return nil, false
			}
			v = e
		case []any:
			// An unsigned decimal index with no leading zeros (RFC 6901).
			if seg == "" || (len(seg) > 1 && seg[0] == '0') {
				return nil, false
			}
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			v = c[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// firstSegment is a pointer's first field name (/turn -> turn), for validation.
func firstSegment(ptr string) string {
	seg, _, _ := strings.Cut(strings.TrimPrefix(ptr, "/"), "/")
	seg = strings.ReplaceAll(seg, "~1", "/")
	return strings.ReplaceAll(seg, "~0", "~")
}

// asText renders a resolved value for substitution into a string.
func asText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return strings.Trim(string(b), `"`)
	}
}
