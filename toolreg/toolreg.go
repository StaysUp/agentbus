// Package toolreg is the tool registry: the one place control keeps which
// tools exist and what each is. A tool is one stored record in the format of
// plans/DESIGN_0_6_tools.md (milestone B): a name, a description written for a
// model, its inputs as one JSON Schema object, what it runs and where it runs.
// A tool carries NO effect: nothing about a tool is trusted, and a call is
// decided by the tool's NAME against the one rules list (his decision,
// 2026-11-18, DESIGN_0_6_tools.md #9).
//
// The built-in tools are defaults, set from code at start-up (SetDefaults);
// the person's own tools, and any default they removed, are the overlay, kept
// on control through a Backend. List is the defaults, minus the ones removed,
// with the added ones on top: a default removed and re-added as a custom tool
// is served from the overlay and behaves as any custom tool does, which is the
// point of one format.
//
// The package imports nothing of the module, so every package may use it. It
// knows nothing of routes or of how a call is served: the api package builds
// the defaults from its route table and serves what List returns.
package toolreg

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Where a tool runs. Only control for now; a runner comes with script and
// command-MCP tools (B4, B5).
const WhereControl = "control"

// What a tool's run is.
//   - api: one call on the AgentBus API, run on control.
//   - app: an Arazzo workflow, several API operations in order, outputs passed
//     forward (B15). An api tool is the one-step case; an app is the many-step
//     case, and nothing else about the record differs.
//
// mcp and script come later (B4, B5).
const (
	RunAPI = "api"
	RunApp = "app"
)

// MaxSteps bounds how many steps an app's workflow may have.
const MaxSteps = 32

// MaxTools bounds how many tools are kept.
const MaxTools = 500

// MaxName bounds a tool's name. It matches the API's path id (sessionID,
// {1,64}), so every name a tool is added under can also be removed through
// DELETE /api/v0/tools/{name}.
const MaxName = 64

// Tool is one stored tool.
type Tool struct {
	// Name is <verb>_<noun>, lower case, unique. A model and a recorded call
	// name it.
	Name string `json:"name" doc:"The tool's name: <verb>_<noun>, lower case, such as list_turns. Unique. A model calls it by this name."`
	// Description is what the tool does and its scope first, then what it does
	// not do. Written for a model.
	Description string `json:"description" doc:"What the tool does and its scope, written for a model: the scope first, then what it does not do."`
	// Inputs is the tool's arguments as one JSON Schema object.
	Inputs map[string]any `json:"inputs" doc:"The tool's arguments as one JSON Schema object: type object, with properties and required."`
	// Where is control.
	Where string `json:"where" doc:"Where the call runs: control."`
	// Run is what the call runs.
	Run Run `json:"run" doc:"What the call runs."`
}

// Run is what a tool call runs. For an api tool it is one HTTP request on the
// AgentBus API, built from these fields and the call's arguments.
type Run struct {
	// Kind is api.
	Kind string `json:"kind" doc:"api: a call on the AgentBus API."`
	// Method and Path are the API request the call makes. Path may hold {id}.
	Method string `json:"method" doc:"The API method, such as GET or POST."`
	Path   string `json:"path" doc:"The API path, such as /api/v0/turns/{id}/diff. {id} is filled from id_input."`
	// IDInput is the input that fills {id} in Path; "" when Path has none.
	IDInput string `json:"id_input,omitempty" doc:"The input whose value fills {id} in path; absent when the path has no {id}."`
	// OwnSession makes IDInput optional: absent, {id} is the caller's own
	// session.
	OwnSession bool `json:"own_session,omitempty" doc:"When true, id_input is optional: absent, {id} is the caller's own session."`
	// Query are the inputs that go into the request's query string; the rest
	// go into its JSON body, when Body is set.
	Query []string `json:"query,omitempty" doc:"The inputs that go into the request's query string; the rest go into its JSON body."`
	// Body says the request carries a JSON body of the inputs that are not the
	// id and not in Query. False sends no body, as a GET or a bodyless POST
	// does.
	Body bool `json:"body,omitempty" doc:"Whether the request carries a JSON body of the inputs that are not the id and not in query."`
	// App is the workflow an app runs; present only when Kind is app. Its
	// shape is one Arazzo workflow over AgentBus's own openapi.json.
	App *App `json:"app,omitempty" doc:"The workflow an app runs: an Arazzo workflow over the AgentBus API. Present only when kind is app."`
}

// App is one Arazzo workflow: ordered steps over AgentBus API operations, each
// step passing its outputs to the next, and the workflow's own outputs read
// from step outputs. The single source is AgentBus's own openapi.json, so a
// step names an operation by its operationId alone.
type App struct {
	// Steps run in order. A failed step stops the app.
	Steps []Step `json:"steps" doc:"The workflow's steps, run in order. A step that fails stops the app."`
	// Outputs are the app's own result, each an expression reading a step
	// output ($steps.<id>.outputs.<name>).
	Outputs map[string]string `json:"outputs,omitempty" doc:"The app's result: each value reads a step output, $steps.<id>.outputs.<name>."`
}

// Step is one step of a workflow: an API operation named by its operationId,
// its request built from values and runtime expressions, and the outputs it
// extracts from the reply for later steps.
type Step struct {
	// StepID is unique within the workflow; later steps and the workflow's
	// outputs reference it.
	StepID string `json:"stepId" doc:"The step's id, unique within the workflow. Later steps read its outputs as $steps.<stepId>.outputs.<name>."`
	// OperationID names an operation in AgentBus's openapi.json.
	OperationID string `json:"operationId" doc:"The AgentBus API operation this step calls, by its operationId (such as startTurn)."`
	// RequestBody holds the body fields, each a literal value or a runtime
	// expression ($inputs.<name>, $steps.<id>.outputs.<name>, $caller.session).
	RequestBody map[string]any `json:"requestBody,omitempty" doc:"The request body fields: each value a literal or a runtime expression."`
	// Parameters hold path and query parameters, same forms as RequestBody.
	Parameters map[string]any `json:"parameters,omitempty" doc:"The path and query parameters: each value a literal or a runtime expression."`
	// Outputs name values the step exposes, each a JSON Pointer into the
	// reply body ($response.body#/<pointer>).
	Outputs map[string]string `json:"outputs,omitempty" doc:"The values this step exposes: each a JSON Pointer into its reply, $response.body#/<pointer>."`
}

// Errors.
var (
	ErrNotFound = errors.New("toolreg: no such tool")
	ErrExists   = errors.New("toolreg: a tool of that name already exists")
	ErrFull     = errors.New("toolreg: too many tools")
)

// Invalid says why a tool was refused, in words fit for the person.
type Invalid struct{ Why string }

func (e *Invalid) Error() string { return e.Why }

// nameShape is a tool name: <verb>_<noun>, lower case.
var nameShape = regexp.MustCompile(`^[a-z]+(_[a-z]+)+$`)

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// Check says what is wrong with t, or "". It fills Where when absent.
//
// Check is the structural gate: it checks what toolreg can see on its own. It
// does NOT know the route table, so for an app it cannot say an operationId
// exists, is callable by a turn token, or that a step field is a real field of
// its operation — the api package checks those against the generated spec at
// save (api.checkApp). Check and that spec check together make an accepted tool
// callable.
func Check(t *Tool) string {
	t.Name = strings.TrimSpace(t.Name)
	switch {
	case !nameShape.MatchString(t.Name):
		return "name must be <verb>_<noun>, in lower case, such as list_turns"
	case len(t.Name) > MaxName:
		return "name must be at most 64 bytes, so it can also be removed by name"
	case strings.TrimSpace(t.Description) == "":
		return "a tool needs a description, written for a model"
	case t.Where != "" && t.Where != WhereControl:
		return "where must be control"
	case !objectSchema(t.Inputs):
		return "inputs must be a JSON Schema object: type object, with properties that are each a schema object, and required a list of its property names"
	}
	var why string
	switch t.Run.Kind {
	case RunAPI:
		why = checkAPIRun(t)
	case RunApp:
		why = checkAppRun(t)
	default:
		return "run kind must be api or app"
	}
	if why != "" {
		return why
	}
	if t.Where == "" {
		t.Where = WhereControl
	}
	return ""
}

// checkAPIRun validates an api tool's run: one route, built from the fields and
// the call's arguments.
func checkAPIRun(t *Tool) string {
	switch {
	case t.Run.App != nil:
		return "an api tool has no app; set run.app only when kind is app"
	case !methods[t.Run.Method]:
		return "run method must be GET, POST, PUT, PATCH or DELETE"
	case !strings.HasPrefix(t.Run.Path, "/api/"):
		return "run path must be an AgentBus API path, such as /api/v0/turns"
	case strings.Contains(t.Run.Path, "{id}") != (t.Run.IDInput != ""):
		return "run id_input must be set exactly when the path has {id}"
	}
	// Every input the run references must be a property, so a call can supply
	// it and the serving path accepts it: the id and the query inputs.
	props, _ := t.Inputs["properties"].(map[string]any)
	if t.Run.IDInput != "" {
		if _, ok := props[t.Run.IDInput]; !ok {
			return "run id_input must be one of the inputs (a property of inputs)"
		}
	}
	for _, q := range t.Run.Query {
		if _, ok := props[q]; !ok {
			return "every run query input must be one of the inputs (a property of inputs)"
		}
	}
	return ""
}

// checkAppRun validates an app's run on what toolreg can see: the steps exist
// and are well formed, step ids are unique, and every expression refers to a
// name in scope (an input, an earlier step's declared output, or the caller).
// The operationId's existence and the fields' validity are the api package's
// to check against the spec.
func checkAppRun(t *Tool) string {
	r := t.Run
	switch {
	case r.Method != "" || r.Path != "" || r.IDInput != "" || r.OwnSession || len(r.Query) > 0 || r.Body:
		return "an app's run uses app, not the api fields (method, path, id_input, own_session, query, body)"
	case r.App == nil || len(r.App.Steps) == 0:
		return "an app's run needs app.steps: at least one step"
	case len(r.App.Steps) > MaxSteps:
		return "an app has too many steps"
	}
	props, _ := t.Inputs["properties"].(map[string]any)
	seen := map[string]bool{}            // step ids declared so far
	outs := map[string]map[string]bool{} // step id -> its declared output names
	for i := range r.App.Steps {
		s := &r.App.Steps[i]
		switch {
		case !stepIDShape.MatchString(s.StepID):
			return "every step needs a stepId of letters, digits, _ and - (step " + strconv.Itoa(i+1) + ")"
		case seen[s.StepID]:
			return "two steps share the stepId " + quote(s.StepID)
		case !operationShape.MatchString(s.OperationID):
			return "step " + quote(s.StepID) + " needs an operationId, such as startTurn"
		}
		// A step's request values may read inputs, earlier steps or the caller.
		for field, v := range s.RequestBody {
			if why := checkValueExpr(v, props, seen, outs, s.StepID); why != "" {
				return "step " + quote(s.StepID) + " requestBody." + field + ": " + why
			}
		}
		for field, v := range s.Parameters {
			if why := checkValueExpr(v, props, seen, outs, s.StepID); why != "" {
				return "step " + quote(s.StepID) + " parameters." + field + ": " + why
			}
		}
		// A step's outputs read from its own reply.
		mine := map[string]bool{}
		for name, expr := range s.Outputs {
			if !responseExpr.MatchString(expr) {
				return "step " + quote(s.StepID) + " output " + quote(name) + " must read $response.body#/<pointer>"
			}
			mine[name] = true
		}
		seen[s.StepID] = true
		outs[s.StepID] = mine
	}
	// The app's own outputs read declared step outputs.
	for name, expr := range r.App.Outputs {
		if why := checkStepsExpr(expr, seen, outs); why != "" {
			return "app output " + quote(name) + ": " + why
		}
	}
	return ""
}

// Expression grammar for an app's workflow (Arazzo's, with two additions):
//   - $inputs.<name>              a tool-call argument
//   - $steps.<stepId>.outputs.<name>  an earlier step's declared output
//   - $caller.session             the calling turn's session (an addition)
//   - $response.body#/<pointer>   a value in a step's own reply (step outputs only)
//
// A request value is a literal, a full expression (the whole string is one
// $... reference), or a string with {$...} references embedded in text.
//
// A value may also be a conditional (the second addition): an object
//
//	{"if_present": <value>, "then": <value>, "else": <value>}
//
// that resolves to "then" when "if_present" resolves to a non-empty string and
// to "else" otherwise. It lets one app answer a call that gives an optional
// input and one that omits it (review_turn: a named turn, or the workspace's
// whole unapplied set). "then" and "else" are themselves values, so each may
// embed its own {$...} references. A plain object with no "if_present" key is a
// literal, as before.
var (
	stepIDShape    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	operationShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	inputsRef      = regexp.MustCompile(`^\$inputs\.([A-Za-z_][A-Za-z0-9_]*)$`)
	stepsRef       = regexp.MustCompile(`^\$steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z_][A-Za-z0-9_]*)$`)
	callerRef      = regexp.MustCompile(`^\$caller\.session$`)
	responseExpr   = regexp.MustCompile(`^\$response\.body#/[^\s]+$`)
	embedded       = regexp.MustCompile(`\{(\$[^{}]+)\}`)
)

// Ref is one parsed reference expression.
type Ref struct {
	// Kind is inputs, steps, caller or response.
	Kind string
	// A is the input name, the step id or the response JSON Pointer; B is the
	// step's output name (steps only).
	A, B string
}

// Reference kinds.
const (
	RefInputs   = "inputs"
	RefSteps    = "steps"
	RefCaller   = "caller"
	RefResponse = "response"
)

// ParseRef parses one $... reference, or false if s is not a single reference.
func ParseRef(s string) (Ref, bool) {
	switch {
	case inputsRef.MatchString(s):
		return Ref{Kind: RefInputs, A: inputsRef.FindStringSubmatch(s)[1]}, true
	case stepsRef.MatchString(s):
		m := stepsRef.FindStringSubmatch(s)
		return Ref{Kind: RefSteps, A: m[1], B: m[2]}, true
	case callerRef.MatchString(s):
		return Ref{Kind: RefCaller}, true
	case responseExpr.MatchString(s):
		return Ref{Kind: RefResponse, A: strings.TrimPrefix(s, "$response.body#")}, true
	}
	return Ref{}, false
}

// IsFullExpr says s is exactly one $... reference (no surrounding text).
func IsFullExpr(s string) bool {
	return strings.HasPrefix(s, "$") && !strings.ContainsAny(s, "{}")
}

// Interpolations are the inner $... expressions a string embeds as {$...}, in
// order, or nil when it embeds none.
func Interpolations(s string) []string {
	var out []string
	for _, m := range embedded.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// Interpolate replaces every {$...} in s with resolve(inner) in ONE pass over
// the original string, so a value that resolves to text containing {$...} is
// never itself re-scanned (found in review 2026-11-19: sequential replaces let
// one input's value capture another's placeholder).
func Interpolate(s string, resolve func(expr string) string) string {
	return embedded.ReplaceAllStringFunc(s, func(m string) string {
		return resolve(m[1 : len(m)-1]) // m is "{$...}"; drop the braces
	})
}

// checkValueExpr validates one request value's expressions against the names in
// scope: the input properties, the step ids declared so far and their outputs.
// A reference to $response, or to a later/own step, is refused. A literal
// (non-string, or a string with no expression) is always fine.
func checkValueExpr(v any, props map[string]any, seen map[string]bool, outs map[string]map[string]bool, self string) string {
	if c, ok := AsConditional(v); ok {
		for _, part := range []any{c["if_present"], c["then"], c["else"]} {
			if why := checkValueExpr(part, props, seen, outs, self); why != "" {
				return why
			}
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return "" // a literal number, bool, object or array
	}
	if IsFullExpr(s) {
		return checkRef(s, props, seen, outs, self)
	}
	for _, e := range Interpolations(s) {
		if why := checkRef(e, props, seen, outs, self); why != "" {
			return why
		}
	}
	return "" // a plain literal string
}

// AsConditional reports whether v is a conditional value: an object with an
// "if_present" key (see the grammar above). It returns the object so the caller
// reads its "if_present", "then" and "else" parts. A map without "if_present"
// is a literal object, not a conditional.
func AsConditional(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	if _, has := m["if_present"]; !has {
		return nil, false
	}
	return m, true
}

// checkRef validates a single reference used in a request value.
func checkRef(expr string, props map[string]any, seen map[string]bool, outs map[string]map[string]bool, self string) string {
	ref, ok := ParseRef(expr)
	if !ok {
		return quote(expr) + " is not a valid expression ($inputs.x, $steps.id.outputs.x or $caller.session)"
	}
	switch ref.Kind {
	case RefInputs:
		if _, ok := props[ref.A]; !ok {
			return quote(expr) + " names an input the tool does not have"
		}
	case RefSteps:
		if why := checkStepsRef(ref, seen, outs, self); why != "" {
			return why
		}
	case RefCaller:
		// $caller.session is always in scope.
	case RefResponse:
		return "$response may be used only in a step's outputs, not in a request value"
	}
	return ""
}

// checkStepsExpr validates a full $steps... expression (an app output).
func checkStepsExpr(expr string, seen map[string]bool, outs map[string]map[string]bool) string {
	ref, ok := ParseRef(expr)
	if !ok || ref.Kind != RefSteps {
		return "must read a step output, $steps.<id>.outputs.<name>"
	}
	return checkStepsRef(ref, seen, outs, "")
}

// checkStepsRef validates that a $steps reference names an earlier step and an
// output that step declares. self is the current step ("" for an app output),
// so a step cannot read its own or a later step's output.
func checkStepsRef(ref Ref, seen map[string]bool, outs map[string]map[string]bool, self string) string {
	if ref.A == self {
		return quote("$steps."+ref.A+".outputs."+ref.B) + " reads the step's own output; read an earlier step"
	}
	if !seen[ref.A] {
		return quote("$steps."+ref.A+".outputs."+ref.B) + " names a step that does not run before this one"
	}
	if !outs[ref.A][ref.B] {
		return quote("$steps."+ref.A+".outputs."+ref.B) + " names an output step " + quote(ref.A) + " does not declare"
	}
	return ""
}

func quote(s string) string { return strconv.Quote(s) }

// objectSchema says s is a structurally consistent object schema: one this
// service can serve and call. The whole rule, so a schema that is accepted is
// always callable:
//   - type is object;
//   - properties is a map, and every property's value is itself a schema
//     object;
//   - required, when present, is a list of names, and every name is a
//     property (so a required input a call can actually supply).
//
// It is deliberately bounded, not a full JSON Schema validator (his call,
// 2026-11-18): it checks the structure the serving path depends on, and does
// not validate the keywords inside each property's schema.
func objectSchema(s map[string]any) bool {
	if s == nil {
		return false
	}
	if ty, _ := s["type"].(string); ty != "object" {
		return false
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return false
	}
	for _, v := range props {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	required, ok := nameList(s["required"])
	if !ok {
		return false
	}
	for _, name := range required {
		if _, ok := props[name]; !ok {
			return false // a required input that is not a property
		}
	}
	return true
}

// nameList reads a JSON Schema "required" value as a list of names: absent is
// an empty list; an array of strings is that list; anything else is not a list
// of names, and false says so.
func nameList(v any) ([]string, bool) {
	switch r := v.(type) {
	case nil:
		return nil, true
	case []string:
		return r, true
	case []any:
		out := make([]string, 0, len(r))
		for _, x := range r {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// Registry keeps the tools. Its methods are safe to call at once from many
// goroutines of one process.
type Registry struct {
	b  Backend
	mu sync.Mutex
	// defaults are the built-in tools, in order, set from code.
	defaults []Tool
	// added are the person's tools and re-added defaults, by name.
	added map[string]Tool
	// removed are the names of defaults the person removed.
	removed map[string]bool
}

// Backend keeps the overlay on control: the added tools and the removed
// defaults. Nil keeps the overlay in memory for this process only.
type Backend interface {
	// Load reads the overlay.
	Load() (added []Tool, removed []string, err error)
	// Save writes the whole overlay.
	Save(added []Tool, removed []string) error
}

// New returns a registry with the overlay read from b (nil: in memory). The
// defaults are set after, with SetDefaults.
func New(b Backend) (*Registry, error) {
	r := &Registry{b: b, added: map[string]Tool{}, removed: map[string]bool{}}
	if b == nil {
		return r, nil
	}
	added, removed, err := b.Load()
	if err != nil {
		return nil, err
	}
	for _, t := range added {
		r.added[t.Name] = t
	}
	for _, n := range removed {
		r.removed[n] = true
	}
	return r, nil
}

// SetDefaults sets the built-in tools. They are kept in code, not in the
// overlay; the overlay holds only what the person changed.
func (r *Registry) SetDefaults(defaults []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults = append([]Tool(nil), defaults...)
}

// List is the tools served, in a stable order: the defaults in their order,
// minus the ones removed, then the added tools by name. A default re-added as
// a custom tool is served from the overlay.
func (r *Registry) List() []Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.list()
}

func (r *Registry) list() []Tool {
	out := make([]Tool, 0, len(r.defaults)+len(r.added))
	seen := map[string]bool{}
	for _, d := range r.defaults {
		if r.removed[d.Name] {
			continue
		}
		if t, ok := r.added[d.Name]; ok { // changed in place
			out = append(out, t)
		} else {
			out = append(out, d)
		}
		seen[d.Name] = true
	}
	extra := make([]string, 0, len(r.added))
	for n := range r.added {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		out = append(out, r.added[n])
	}
	return out
}

// Get is the tool named name, or ErrNotFound.
func (r *Registry) Get(name string) (Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.list() {
		if t.Name == name {
			return t, nil
		}
	}
	return Tool{}, ErrNotFound
}

// Add saves t as a new tool. It refuses a name already served, unless that
// name is a default the person removed, which this re-adds as a custom tool.
// It validates t and persists the overlay.
func (r *Registry) Add(t Tool) (Tool, error) {
	if why := Check(&t); why != "" {
		return Tool{}, &Invalid{why}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.list() {
		if s.Name == t.Name {
			return Tool{}, ErrExists
		}
	}
	if len(r.added) >= MaxTools {
		return Tool{}, ErrFull
	}
	prevAdded := r.added[t.Name]
	hadAdded := false
	if _, ok := r.added[t.Name]; ok {
		hadAdded = true
	}
	wasRemoved := r.removed[t.Name]
	r.added[t.Name] = t
	// Re-adding a removed default: the overlay no longer marks it removed, so
	// List serves the added one.
	delete(r.removed, t.Name)
	if err := r.save(); err != nil {
		if hadAdded {
			r.added[t.Name] = prevAdded
		} else {
			delete(r.added, t.Name)
		}
		r.removed[t.Name] = wasRemoved
		return Tool{}, err
	}
	return t, nil
}

// Remove removes the tool named name: a custom tool is dropped from the
// overlay; a default is marked removed. ErrNotFound when no tool is served by
// that name. It persists the overlay.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	served := false
	isDefault := false
	for _, d := range r.defaults {
		if d.Name == name {
			isDefault = true
		}
	}
	for _, s := range r.list() {
		if s.Name == name {
			served = true
		}
	}
	if !served {
		return ErrNotFound
	}
	prevAdded, hadAdded := r.added[name]
	wasRemoved := r.removed[name]
	delete(r.added, name)
	if isDefault {
		r.removed[name] = true
	}
	if err := r.save(); err != nil {
		if hadAdded {
			r.added[name] = prevAdded
		}
		if wasRemoved {
			r.removed[name] = true
		} else {
			delete(r.removed, name)
		}
		return err
	}
	return nil
}

// save writes the overlay through the backend. The caller holds the lock.
func (r *Registry) save() error {
	if r.b == nil {
		return nil
	}
	added := make([]Tool, 0, len(r.added))
	for _, t := range r.added {
		added = append(added, t)
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Name < added[j].Name })
	removed := make([]string, 0, len(r.removed))
	for n := range r.removed {
		removed = append(removed, n)
	}
	sort.Strings(removed)
	return r.b.Save(added, removed)
}
