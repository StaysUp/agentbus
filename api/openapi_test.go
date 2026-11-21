package api

// The OpenAPI description, generated from the route table (spec.go), is kept
// true here: it must be a valid OpenAPI 3.1 document, name exactly the routes
// the code serves, and describe the real responses of the in-process service.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// loadSpec loads the description the service serves, and validates it as
// OpenAPI.
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	return loadDoc(t, spec)
}

// loadRunnerSpec loads the runner protocol's description.
func loadRunnerSpec(t *testing.T) *openapi3.T {
	t.Helper()
	return loadDoc(t, runnerSpec)
}

func loadDoc(t *testing.T, spec func() ([]byte, error)) *openapi3.T {
	t.Helper()
	data, err := spec()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatalf("the description does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("the description is not a valid OpenAPI document: %v", err)
	}
	return doc
}

func TestOpenAPISpecValid(t *testing.T) {
	doc := loadSpec(t)
	if !doc.IsOpenAPI31OrLater() {
		t.Errorf("openapi = %q, want 3.1", doc.OpenAPI)
	}
	// Every path is under /api/v0, and every operation but the description
	// itself and the sign-in code's trade needs the token.
	for p, item := range doc.Paths.Map() {
		if !strings.HasPrefix(p, "/api/v0/") {
			t.Errorf("%s: not under /api/v0/", p)
		}
		for m, op := range item.Operations() {
			public := op.Security != nil && len(*op.Security) == 0
			if public != (p == "/api/v0/openapi.json" || p == "/api/v0/signin") {
				t.Errorf("%s %s: public = %v", m, p, public)
			}
			if op.Responses.Status(http.StatusForbidden) == nil {
				t.Errorf("%s %s: no 403 for a wrong Host", m, p)
			}
			if !public && op.Responses.Status(http.StatusUnauthorized) == nil {
				t.Errorf("%s %s: no 401", m, p)
			}
		}
	}
}

// GET /api/v0/approvals reads the record through the store, so the 500 its
// handler writes is a reply the description must list.
func TestOpenAPIApprovalsListDescribesStoreFailure(t *testing.T) {
	op := loadSpec(t).Paths.Find("/api/v0/approvals").Get
	resp := op.Responses.Status(http.StatusInternalServerError)
	if resp == nil || resp.Value == nil {
		t.Fatal("GET /api/v0/approvals: no 500 for a store failure")
	}
	want := "The store could not be read or written."
	if resp.Value.Description == nil || *resp.Value.Description != want {
		t.Errorf("500 description = %v, want %q", resp.Value.Description, want)
	}
}

// Every operation has a short summary that names its action, the heading
// and the menu entry on /docs, and a description whose first line says what
// it does. Each is in one listed tag, and Turns comes first.
// The route doc for saving an agent names the planned harnesses from the
// registry's list, so it stays right when one is added.
func TestOpenAPICreateAgentNamesPlannedHarnesses(t *testing.T) {
	op := loadSpec(t).Paths.Find("/api/v0/agents").Post
	if harness.PlannedList() == "" {
		if strings.Contains(op.Description, "only planned") {
			t.Errorf("createAgent description names planned harnesses while none is planned:\n%s", op.Description)
		}
		return
	}
	if want := "(" + harness.PlannedList() + ") is refused: not available yet"; !strings.Contains(op.Description, want) {
		t.Errorf("createAgent description lacks %q:\n%s", want, op.Description)
	}
}

func TestOpenAPISavedAgentHarnessDescriptionIsGeneric(t *testing.T) {
	property := loadSpec(t).Components.Schemas["SavedAgent"].Value.Properties["harness"].Value
	if want := "The program that runs the agent's loop."; property.Description != want {
		t.Errorf("SavedAgent.harness description = %q, want %q", property.Description, want)
	}
}

// The access values an agent can be saved with come from the agents package's
// constants, so the spec's enum on AgentRequest.access must list exactly those.
// The spec must also accept each one, and the code must save an agent with api.
func TestOpenAPIAgentAccessMatchesCode(t *testing.T) {
	access := loadSpec(t).Components.Schemas["AgentRequest"].Value.Properties["access"].Value
	want := []any{agents.Subscription, agents.OpenRouter, agents.API}
	for _, provider := range harness.Providers {
		want = append(want, provider.Name)
	}
	if !slices.Equal(access.Enum, want) {
		t.Errorf("AgentRequest.access enum = %v, want %v", access.Enum, want)
	}
	for _, v := range want {
		if err := access.VisitJSON(v); err != nil {
			t.Errorf("the spec refuses access %v: %v", v, err)
		}
	}
	if why := agents.Check(&agents.Fields{Name: "x", Harness: "agy", Access: agents.API}); why != "" {
		t.Errorf("the code refuses access api for agy: %s", why)
	}
}

func TestOpenAPIOperationsNamedAndTagged(t *testing.T) {
	doc := loadSpec(t)
	var names []string
	for _, tg := range doc.Tags {
		names = append(names, tg.Name)
		if tg.Description == "" {
			t.Errorf("tag %s: no description", tg.Name)
		}
	}
	if len(names) == 0 || names[0] != "Turns" {
		t.Errorf("tags = %v, want Turns first", names)
	}
	used := map[string]bool{}
	for p, item := range doc.Paths.Map() {
		for m, op := range item.Operations() {
			s := op.Summary
			if n := len(strings.Fields(s)); n == 0 || n > 5 || strings.HasSuffix(s, ".") || strings.ToUpper(s[:1]) != s[:1] {
				t.Errorf("%s %s: summary %q, want a few words that name the action, with no period", m, p, s)
			}
			first, _, _ := strings.Cut(op.Description, "\n")
			if !strings.HasSuffix(first, ".") {
				t.Errorf("%s %s: the description's first line %q is not a sentence", m, p, first)
			}
			if len(op.Tags) != 1 || !slices.Contains(names, op.Tags[0]) {
				t.Errorf("%s %s: tags %v, want one of %v", m, p, op.Tags, names)
				continue
			}
			used[op.Tags[0]] = true
		}
	}
	for _, n := range names {
		if !used[n] {
			t.Errorf("tag %s has no operation", n)
		}
	}
}

// A route whose tag is not listed stops the description from being built.
func TestOpenAPIRefusesUnlistedTag(t *testing.T) {
	was := routes[0].tag
	t.Cleanup(func() { routes[0].tag = was })
	routes[0].tag = "Nowhere"
	if _, err := describe(false); err == nil || !strings.Contains(err.Error(), "Nowhere") {
		t.Errorf("describe() = %v, want it to refuse the tag Nowhere", err)
	}
}

// specOps lists the spec's operations as "METHOD path", with {id} as abc.
func specOps(doc *openapi3.T) []string {
	var ops []string
	for p, item := range doc.Paths.Map() {
		for m := range item.Operations() {
			ops = append(ops, m+" "+strings.NewReplacer("{id}", "abc", "{call}", "abc", "{n}", "0").Replace(p))
		}
	}
	sort.Strings(ops)
	return ops
}

// routeLiterals reads every string literal in the package's own source:
// whole paths such as /api/v0/sessions, prefixes such as /api/v0/sessions/,
// and lowercase words that could follow a session id, such as hold. A route
// served anywhere in the package, in the table or not, shows up here without
// anyone listing it. A table path's {id} becomes abc.
func routeLiterals(t *testing.T) (paths, prefixes, words []string) {
	t.Helper()
	fset := gotoken.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			s = strings.NewReplacer("{id}", "abc", "{call}", "abc").Replace(s)
			switch {
			case strings.HasPrefix(s, "/api/") && strings.HasSuffix(s, "/"):
				prefixes = append(prefixes, s)
			case strings.HasPrefix(s, "/api/") && !strings.ContainsAny(s, " ?`"):
				paths = append(paths, s)
			case s != "" && len(s) <= 32 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz") == "":
				words = append(words, s)
			}
			return true
		})
	}
	if len(paths) == 0 || len(prefixes) == 0 || len(words) == 0 {
		t.Fatalf("found %d paths, %d prefixes, %d words", len(paths), len(prefixes), len(words))
	}
	return paths, prefixes, words
}

// served reports whether the API answers method on path as a route of its
// own: anything but 405 and the 404 for an unknown path.
func served(t *testing.T, e *env, method, path string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := e.req(t, method, path, nil)
	r = r.WithContext(ctx)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return false
	}
	if resp.StatusCode == http.StatusNotFound {
		var body struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
		return body.Error != "no such API path"
	}
	return true
}

// One endpoint starts every turn: POST /api/v0/turns, with session for a
// session's next turn. /sessions takes POST to create a session in a workspace
// and PATCH /sessions/{id} to rename or archive it; neither starts a turn.
// POST /sessions/{id}/commands, the person's "!" of 0.4, is gone (a "!" is a
// message to the agent now). Every other action is on a turn.
// /runs, /jobs, /attempts and /calls are gone with no alias; /workspaces is
// the workspace noun now (where turns run), not the old runner-side view.
// A new data folder starts with no agents, under up as under server
// (87f63b2): the agents list says so, not the starters it had before.
func TestOpenAPIAgentsStartEmpty(t *testing.T) {
	op := loadSpec(t).Paths.Find("/api/v0/agents").Get
	if !strings.Contains(op.Description, "A new data folder starts with none") || strings.Contains(op.Description, "one agent for each harness") {
		t.Errorf("GET /api/v0/agents says: %s", op.Description)
	}
}

func TestOneEndpointStartsEveryTurn(t *testing.T) {
	doc := loadSpec(t)
	var ops []string
	for p, item := range doc.Paths.Map() {
		for m := range item.Operations() {
			ops = append(ops, m+" "+p)
			if strings.HasPrefix(p, "/api/v0/sessions") && m != http.MethodGet &&
				m+" "+p != "PATCH /api/v0/sessions/{id}" && m+" "+p != "POST /api/v0/sessions" {
				t.Errorf("%s %s: /sessions takes GET, POST to create a session in a workspace, and PATCH to rename or archive", m, p)
			}
		}
	}
	// 43 with POST /api/v0/mcp, which starts a turn only as the
	// POST /api/v0/turns it sends a tool call through, DELETE
	// /api/v0/subscriptions/{id}/connect, which stops a login, and PUT and
	// DELETE /api/v0/subscriptions/openrouter/key, which set and remove the
	// OpenRouter key, and 44 with PATCH /api/v0/settings/permissions, which
	// changes only the keys it is sent, 45 with GET
	// /api/v0/harnesses/{id}/models, which lists a harness's models, 46
	// with POST /api/v0/signin, the cockpit's one-time sign-in link, and 52
	// with PUT and DELETE of the anthropic, openai and gemini API keys, and
	// 53 with GET /api/v0/workspaces/{id}/changes, an apply's preview, which
	// writes nothing, and 54 with GET /api/v0/folders/{id}/home-files, which
	// lists a home's config files and starts no turn, and 56 with GET
	// /api/v0/approvals and POST /api/v0/approvals/{id}, which list and answer
	// the commands held for a person (ask in permissions.toml), and 59 with the
	// three turn list routes GET /api/v0/turns/{id}/changes, .../calls and
	// .../tools, which page a turn's lists the diff summarises, and 62 with
	// GET, POST and DELETE of /api/v0/tools and GET /api/v0/tools/{id}, which
	// list, read, add and remove the tools control serves and start no turn
	// (the list carries facts only; the {id} route reads one tool's full
	// definition), and 65 with GET
	// /api/v0/turns/{id}/context and GET /api/v0/sessions/{id}/context, the
	// context view: how big a turn's or a session's context is, how much came
	// from the prompt cache, and what a prompt is made of. Both read the
	// record and start no turn. 67 with GET and PUT
	// /api/v0/settings/compaction, which read and change the per-vendor
	// compaction points the context view shows ("compacts at ~Y%"). 69 with
	// POST /api/v0/ons and DELETE /api/v0/ons/{id}, the on primitive: register
	// a standing instruction (a turn completes -> deliver its reply) and remove
	// it (B13). 72 with POST /api/v0/files/read, POST /api/v0/files/list and
	// POST /api/v0/files/write, folder access through tools (B8). Plus GET
	// /api/v0/turns/{id}/record, a turn's two halves (the machine half it ran
	// and the agent half it called) in one read, which reads the record and
	// starts no turn (machine-record step 1). 76 with the three workspace
	// routes POST and GET /api/v0/workspaces and GET /api/v0/workspaces/{id},
	// which create, list and read a workspace (where turns run: a folder and a
	// sandbox mode), and POST /api/v0/sessions, which creates a session in a
	// workspace. All four start no turn: a workspace, then a session in it,
	// then the session's turns. 77 with POST /api/v0/workspaces/{id}/discard,
	// which drops a sandbox workspace's unapplied changes by resetting its copy,
	// and POST /api/v0/workspaces/{id}/apply, which applies them to the person's
	// folder (apply and the changes preview above moved from the turn and the
	// session to the workspace, so the count is unchanged). 78 with POST
	// /api/v0/workspaces/{id}/update, which merges the person's folder into the
	// copy so the next apply writes only the session's work. 80 with PUT and
	// DELETE /api/v0/subscriptions/xai/key, the xAI provider's key, which start
	// no turn.
	// 81 with POST /api/v0/workspaces/{id}/close, which removes the copy. 78
	// without the three file routes (his call, 2026-11-25): agents use their
	// CLI's own file tools. 79 with GET /api/v0/ons, which lists the ons. 82
	// with POST /api/v0/commands, GET /api/v0/commands/{id} and POST
	// /api/v0/commands/{id}/stop (H1): a command an agent hands to AgentBus is
	// an exec job, not a turn; its result is delivered as a turn by its on. 83
	// with GET /api/v0/workspaces/{id}/outcome, which reads one action's outcome.
	if len(ops) != 83 {
		t.Errorf("%d endpoints, want 83: %v", len(ops), ops)
	}
	for _, want := range []string{"POST /api/v0/turns", "GET /api/v0/turns", "GET /api/v0/turns/{id}", "POST /api/v0/turns/{id}/stop",
		"GET /api/v0/turns/{id}/diff", "GET /api/v0/workspaces/{id}/changes", "POST /api/v0/workspaces/{id}/apply",
		"GET /api/v0/turns/{id}/calls/{call}", "GET /api/v0/turns/{id}/calls/{call}/pieces/{n}", "GET /api/v0/sessions/{id}/events"} {
		if !slices.Contains(ops, want) {
			t.Errorf("%s is not in the description", want)
		}
	}
	// The per-turn apply and per-session changes routes moved to the workspace.
	for _, gone := range []string{"POST /api/v0/turns/{id}/apply", "GET /api/v0/sessions/{id}/changes"} {
		if slices.Contains(ops, gone) {
			t.Errorf("%s should have moved to the workspace", gone)
		}
	}
	for p := range doc.Paths.Map() {
		for _, gone := range []string{"/api/v0/runs", "/api/v0/jobs", "/api/v0/attempts", "/api/v0/calls", "/api/v0/runner", "/api/v0/blobs", "/api/v0/packs"} {
			if p == gone || strings.HasPrefix(p, gone+"/") {
				t.Errorf("%s is still described in the client API", p)
			}
		}
		if strings.HasSuffix(p, "/events") && p != "/api/v0/events" && p != "/api/v0/sessions/{id}/events" {
			t.Errorf("%s: one way to read events, by session", p)
		}
	}
	run := doc.Paths.Find("/api/v0/turns").Post
	if run.OperationID != "startTurn" || run.Responses.Status(http.StatusConflict) == nil || run.Responses.Status(http.StatusNotFound) == nil {
		t.Error("POST /api/v0/turns is not startTurn, or does not describe the 404 and 409 of a session's next turn")
	}
	req := doc.Components.Schemas["TurnRequest"].Value
	if !slices.Equal(req.Required, []string{"session", "agent", "prompt"}) {
		t.Errorf("TurnRequest requires %v, want session, agent and prompt: the folder and the mode come from the session's workspace", req.Required)
	}
	e := newEnv(t)
	if resp, body := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"prompt":"x"}`))); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/v0/runs: %d %s, want 404", resp.StatusCode, body)
	}
	resp, body := do(t, e.req(t, "POST", "/api/v0/sessions/s1/messages", strings.NewReader(`{"prompt":"next"}`)))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/v0/sessions/s1/messages: %d %s, want 404", resp.StatusCode, body)
	}
}

func TestOpenAPIRoutesMatchCode(t *testing.T) {
	doc := loadSpec(t)
	e := resumableEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) { return run.Launch{}, run.ErrNotFound })
	paths, prefixes, words := routeLiterals(t)
	candidates := map[string]bool{}
	for _, p := range paths {
		candidates[p] = true
	}
	for _, pre := range prefixes {
		for _, w := range words {
			candidates[pre+w] = true
			candidates[pre+"abc/"+w] = true
		}
	}
	runnerDoc := loadRunnerSpec(t)
	for _, op := range append(specOps(doc), specOps(runnerDoc)...) {
		_, p, _ := strings.Cut(op, " ")
		candidates[p] = true
	}
	var code []string
	for p := range candidates {
		for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			if served(t, e, m, p) {
				code = append(code, m+" "+p)
			}
		}
	}
	sort.Strings(code)
	spec := append(specOps(doc), specOps(runnerDoc)...)
	for _, op := range code {
		if !slices.Contains(spec, op) {
			t.Errorf("the code serves %s, which the description does not name", op)
		}
	}
	for _, op := range spec {
		if !slices.Contains(code, op) {
			t.Errorf("the description names %s, which the code does not serve", op)
		}
	}
}

// jsonFields lists the JSON names of a struct's fields, an embedded
// struct's among them.
func jsonFields(typ reflect.Type) []string {
	var names []string
	for _, f := range fieldsOf(typ) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

// The schemas name every field the Go types send, and no field they do not.
// A field added to event.Event is in the description with no other edit.
func TestOpenAPISchemasMatchTypes(t *testing.T) {
	doc := loadSpec(t)
	for schema, typ := range map[string]reflect.Type{
		"EventFields":   reflect.TypeFor[event.Event](),
		"Usage":         reflect.TypeFor[event.Usage](),
		"Unexpected":    reflect.TypeFor[event.Unexpected](),
		"Snapshot":      reflect.TypeFor[Snapshot](),
		"Session":       reflect.TypeFor[Session](),
		"Harness":       reflect.TypeFor[Harness](),
		"AccessOption":  reflect.TypeFor[AccessOption](),
		"SavedAgent":    reflect.TypeFor[SavedAgent](),
		"AgentRequest":  reflect.TypeFor[AgentRequest](),
		"Page":          reflect.TypeFor[Page](),
		"TurnRequest":   reflect.TypeFor[TurnRequest](),
		"TurnList":      reflect.TypeFor[TurnList](),
		"Turn":          reflect.TypeFor[Turn](),
		"OneTurn":       reflect.TypeFor[OneTurn](),
		"OneSession":    reflect.TypeFor[OneSession](),
		"SessionCopy":   reflect.TypeFor[SessionCopy](),
		"SessionUpdate": reflect.TypeFor[SessionUpdate](),
		"Started":       reflect.TypeFor[Started](),
		"EventRequest":  reflect.TypeFor[EventRequest](),
		"CallContext":   reflect.TypeFor[store.CallContext](),
		"Piece":         reflect.TypeFor[store.Piece](),
	} {
		s := doc.Components.Schemas[schema].Value
		fields := jsonFields(typ)
		for _, f := range fields {
			if _, ok := s.Properties[f]; !ok {
				t.Errorf("%s: %s.%s is not in the description", schema, typ, f)
			}
		}
		for name := range s.Properties {
			if !slices.Contains(fields, name) {
				t.Errorf("%s: the description names %s, which %s does not have", schema, name, typ)
			}
		}
	}
	// Every field of event.Event that has a doc tag carries it.
	ef := doc.Components.Schemas["EventFields"].Value
	typ := reflect.TypeFor[event.Event]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if d := f.Tag.Get("doc"); d != "" && ef.Properties[name].Value.Description != d {
			t.Errorf("EventFields.%s: description %q, want the doc tag %q", name, ef.Properties[name].Value.Description, d)
		}
	}
	// Every framework kind has its own schema, titled with the kind, and
	// is in the enum on kind.
	var titles []string
	for _, ref := range doc.Components.Schemas["Event"].Value.OneOf {
		titles = append(titles, ref.Value.Title)
	}
	enum := ef.Properties["kind"].Value.AnyOf[0].Value.Enum
	for _, k := range event.Kinds {
		if !slices.Contains(titles, string(k.Kind)) {
			t.Errorf("no schema for %s", k.Kind)
		}
		if !slices.Contains(enum, any(string(k.Kind))) {
			t.Errorf("the enum on kind has no %s", k.Kind)
		}
	}
}

// A new field on a struct is described from its tags alone: its doc tag is
// its description, it is required unless omitempty, and a pointer without
// omitempty may be null. This is the path event.Event's fields take.
func TestOpenAPINewFieldDescribed(t *testing.T) {
	type Later struct {
		Plain    string       `json:"plain" doc:"A field added later."`
		Optional int          `json:"optional,omitempty"`
		Maybe    *int         `json:"maybe" doc:"Null when unknown."`
		Shape    *event.Usage `json:"shape" doc:"A component that may be null."`
		Hidden   string       `json:"-"`
	}
	c := &components{schemas: openapi3.Schemas{}, types: map[string]reflect.Type{}}
	if _, err := c.ref(Later{}); err != nil {
		t.Fatal(err)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	s := c.schemas["Later"].Value
	if got := s.Properties["plain"].Value.Description; got != "A field added later." {
		t.Errorf("plain: description %q", got)
	}
	if !slices.Equal(s.Required, []string{"plain", "maybe", "shape"}) {
		t.Errorf("required = %v", s.Required)
	}
	if got := s.Properties["maybe"].Value.Type; got == nil || !got.Includes("null") || !got.Includes("integer") {
		t.Errorf("maybe: type %v, want integer or null", got)
	}
	if sh := s.Properties["shape"].Value; len(sh.AnyOf) != 2 || sh.AnyOf[0].Ref != "#/components/schemas/Usage" || sh.Description == "" {
		t.Errorf("shape: %+v", sh)
	}
	if _, ok := s.Properties["Hidden"]; ok || len(s.Properties) != 4 {
		t.Errorf("properties = %v", slices.Sorted(maps.Keys(s.Properties)))
	}
}

// check sends r, validates the request and the response against the spec,
// and returns the response body.
func check(t *testing.T, doc *openapi3.T, e *env, method, path, body string, auth bool, want int) []byte {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := e.req(t, method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if !auth {
		r.Header.Del("Authorization")
	}
	router, err := gorillamux.NewRouter(withServer(doc, e.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	route, params, err := router.FindRoute(r)
	if err != nil {
		t.Fatalf("%s %s: not in the spec: %v", method, path, err)
	}
	in := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	// A request meant to be refused is not checked: it breaks the spec on
	// purpose.
	if auth && want < 400 {
		if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
			t.Errorf("%s %s: the request does not match the spec: %v", method, path, err)
		}
		if body != "" {
			r.Body = io.NopCloser(strings.NewReader(body))
		}
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, got, want)
	}
	out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: resp.StatusCode,
		Header: resp.Header, Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true}}
	out.SetBodyBytes(got)
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		t.Errorf("%s %s: the %d response does not match the spec: %v\n%s", method, path, resp.StatusCode, err, got)
	}
	return got
}

// withServer is doc with its one server set to the test service's address.
func withServer(doc *openapi3.T, base string) *openapi3.T {
	c := *doc
	c.Servers = openapi3.Servers{{URL: base}}
	return &c
}

// next reads one stream message, skipping comments.
func (s *sse) next(t *testing.T) (id, kind, data string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatal("the stream ended")
			}
			switch {
			case line == "" && data != "":
				return id, kind, data
			case strings.HasPrefix(line, "id: "):
				id = line[4:]
			case strings.HasPrefix(line, "event: "):
				kind = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			}
		case <-deadline:
			t.Fatal("no stream message in 5s")
		}
	}
}

// TestOpenAPIStreamsHaveExamples: a stream reply with no example makes the docs
// page show only the route's errors as samples (seen 2026-11-05 on
// GET /api/v0/events), so every text/event-stream reply carries one.
func TestOpenAPIStreamsHaveExamples(t *testing.T) {
	doc := loadSpec(t)
	n := 0
	for p, item := range doc.Paths.Map() {
		for m, op := range item.Operations() {
			for status, resp := range op.Responses.Map() {
				mt := resp.Value.Content.Get("text/event-stream")
				if mt == nil {
					continue
				}
				n++
				if s, _ := mt.Example.(string); s == "" {
					t.Errorf("%s %s %s: a stream reply with no example", m, p, status)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no stream replies found; the test checks nothing")
	}
}
