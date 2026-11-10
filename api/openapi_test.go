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

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// loadSpec loads the description the service serves, and validates it as
// OpenAPI.
func loadSpec(t *testing.T) *openapi3.T {
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
	// itself needs the token.
	for p, item := range doc.Paths.Map() {
		if !strings.HasPrefix(p, "/api/v0/") {
			t.Errorf("%s: not under /api/v0/", p)
		}
		for m, op := range item.Operations() {
			public := op.Security != nil && len(*op.Security) == 0
			if public != (p == "/api/v0/openapi.json") {
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

// Every operation has a short summary that names its action, the heading
// and the menu entry on /docs, and a description whose first line says what
// it does. Each is in one listed tag, and Turns comes first.
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
	if _, err := describe(); err == nil || !strings.Contains(err.Error(), "Nowhere") {
		t.Errorf("describe() = %v, want it to refuse the tag Nowhere", err)
	}
}

// specOps lists the spec's operations as "METHOD path", with {id} as abc.
func specOps(doc *openapi3.T) []string {
	var ops []string
	for p, item := range doc.Paths.Map() {
		for m := range item.Operations() {
			ops = append(ops, m+" "+strings.NewReplacer("{id}", "abc", "{n}", "0").Replace(p))
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
			s = strings.ReplaceAll(s, "{id}", "abc")
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

// Every route the code serves is in the spec, and every operation in the spec
// is served.
// One endpoint starts every turn: POST /api/v0/turns,
// with session for a session's next turn. /sessions is read-only but for the
// rename, PATCH /sessions/{id}, which starts nothing. The turn is
// the one resource that is written: POST /turns starts one, GET /turns lists
// them, GET /turns/{id}/events reads one; /runs is gone with no alias.
func TestOneEndpointStartsEveryTurn(t *testing.T) {
	doc := loadSpec(t)
	var ops []string
	for p, item := range doc.Paths.Map() {
		for m := range item.Operations() {
			ops = append(ops, m+" "+p)
			if strings.HasPrefix(p, "/api/v0/sessions") && m != http.MethodGet && m+" "+p != "PATCH /api/v0/sessions/{id}" {
				t.Errorf("%s %s: /sessions takes only GET, and PATCH to rename", m, p)
			}
		}
	}
	if len(ops) != 45 {
		t.Errorf("%d endpoints, want 45: %v", len(ops), ops)
	}
	for _, want := range []string{"POST /api/v0/turns", "GET /api/v0/turns", "GET /api/v0/turns/{id}", "GET /api/v0/turns/{id}/events", "POST /api/v0/turns/{id}/stop",
		"GET /api/v0/turns/{id}/diff", "POST /api/v0/turns/{id}/apply"} {
		if !slices.Contains(ops, want) {
			t.Errorf("%s is not in the description", want)
		}
	}
	if doc.Paths.Find("/api/v0/runs") != nil {
		t.Error("/api/v0/runs is still described")
	}
	run := doc.Paths.Find("/api/v0/turns").Post
	if run.OperationID != "startTurn" || run.Responses.Status(http.StatusConflict) == nil || run.Responses.Status(http.StatusNotFound) == nil {
		t.Error("POST /api/v0/turns is not startTurn, or does not describe the 404 and 409 of a session's next turn")
	}
	req := doc.Components.Schemas["TurnRequest"].Value
	if !slices.Equal(req.Required, []string{"prompt"}) {
		t.Errorf("TurnRequest requires %v, want only prompt: agent and folder come from the session when it is sent", req.Required)
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
	for _, op := range specOps(doc) {
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
	spec := specOps(doc)
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

// jsonFields lists the JSON names of a struct's fields.
func jsonFields(typ reflect.Type) []string {
	var names []string
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
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
		"EventFields":  reflect.TypeFor[event.Event](),
		"Usage":        reflect.TypeFor[event.Usage](),
		"Unexpected":   reflect.TypeFor[event.Unexpected](),
		"Snapshot":     reflect.TypeFor[store.Snapshot](),
		"Session":      reflect.TypeFor[store.Session](),
		"Harness":      reflect.TypeFor[Harness](),
		"AccessOption": reflect.TypeFor[AccessOption](),
		"SavedAgent":   reflect.TypeFor[SavedAgent](),
		"AgentRequest": reflect.TypeFor[AgentRequest](),
		"Page":         reflect.TypeFor[Page](),
		"TurnRequest":  reflect.TypeFor[TurnRequest](),
		"TurnList":     reflect.TypeFor[store.TurnList](),
		"Turn":         reflect.TypeFor[store.Turn](),
		"Started":      reflect.TypeFor[Started](),
		"EventRequest": reflect.TypeFor[EventRequest](),
		"CallContext":  reflect.TypeFor[store.CallContext](),
		"Piece":        reflect.TypeFor[store.Piece](),
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
