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
	"net/url"
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

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
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
// with session for a session's next turn. /sessions is read-only. The turn is
// the one resource that is written: POST /turns starts one, GET /turns lists
// them, GET /turns/{id}/events reads one; /runs is gone with no alias.
func TestOneEndpointStartsEveryTurn(t *testing.T) {
	doc := loadSpec(t)
	var ops []string
	for p, item := range doc.Paths.Map() {
		for m := range item.Operations() {
			ops = append(ops, m+" "+p)
			if strings.HasPrefix(p, "/api/v0/sessions") && m != http.MethodGet {
				t.Errorf("%s %s: /sessions takes only GET", m, p)
			}
		}
	}
	if len(ops) != 12 {
		t.Errorf("%d endpoints, want 12: %v", len(ops), ops)
	}
	for _, want := range []string{"POST /api/v0/turns", "GET /api/v0/turns", "GET /api/v0/turns/{id}/events"} {
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
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) { return Launch{}, ErrNotFound })
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
		"Agent":        reflect.TypeFor[Agent](),
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

// fill stores one event of every framework kind in session s1, with the
// fields each carries in real traffic.
func fill(t *testing.T, st *store.Store) {
	t.Helper()
	now := time.Now().UTC()
	in, lat, cost, yes, no, code := 100, int64(840), 0.0123, true, false, 0
	u := &event.Usage{InputTokens: in, OutputTokens: 20, CacheReadTokens: 3000, CacheWriteTokens: 40,
		CacheWrite5mTokens: 40, ServiceTier: "standard", InferenceGeo: "global",
		ServerToolUse: map[string]int{"web_search_requests": 1}}
	base := event.Event{Time: now, Session: "s1", Exchange: "x-1", Vendor: "anthropic", Client: "claude-cli/2.1.318", Turn: "t_1"}
	evs := []event.Event{
		{Kind: event.TurnStarted, Text: "Fix the login bug", Agent: "claude", Folder: "/Users/ana/app"},
		{Kind: event.ModelRequested, Model: "claude-sonnet-5-5", ToolsOffered: []string{"Read", "Edit"}},
		{Kind: event.ModelResponded, Model: "claude-sonnet-5-5", Text: "Reading it.", StopReason: "tool_use",
			Status: 200, Usage: u, LatencyMS: &lat, CostUSD: &cost, CostComplete: &yes},
		{Kind: event.ToolRequested, ToolID: "toolu_1", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"/a.go"}`)},
		{Kind: event.ToolCompleted, ToolID: "toolu_1", ToolOutput: "package a", ToolError: &no},
		{Kind: event.ModelResponded, Status: 529, Error: "overloaded"},
		{Kind: event.TurnCompleted, Outcome: event.Finished, StopReason: "end_turn"},
		{Kind: event.SchemaUnexpected, Unexpected: &event.Unexpected{Where: "response.content[].type", Value: "server_tool_use_v2"}},
	}
	for i, e := range evs {
		e.Time, e.Session, e.Exchange, e.Vendor, e.Client, e.Turn = base.Time, base.Session, base.Exchange, base.Vendor, base.Client, base.Turn
		e.ID = "e-1-" + strconv.Itoa(i)
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []event.Event{
		{ID: "e-1-90", Kind: event.EventsDropped, Time: now, Vendor: "anthropic", Dropped: 3, Sessions: []string{"s1"}, Error: "the event output did not keep up"},
		{ID: "e-1-91", Kind: event.EventsDropped, Time: now, Vendor: "openai", Dropped: 1, AllSessions: true},
		// An older store's registration: nothing writes it now, readers
		// still meet it.
		{ID: "evt_0", Kind: event.SessionRegistered, Time: now, Session: "s1", Agent: "claude", Folder: "/Users/ana/app"},
		{ID: "evt_4", Kind: event.SessionOpened, Time: now, Session: "s1", Hold: "hold_2", Agent: "claude", Folder: "/Users/ana/app"},
		{ID: "evt_5", Kind: event.SessionClosed, Time: now, Session: "s1", Hold: "hold_2", ExitCode: &code, ResumeID: "abc-2"},
	} {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// The service's real responses match the spec: sessions, a page of events of
// every kind, the stream, a program's event, a run's 202, and
// errors.
func TestOpenAPIResponsesMatch(t *testing.T) {
	doc := loadSpec(t)
	folder := t.TempDir()
	// A run's child: a shell that prints claude's result line and exits.
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) {
		return Launch{Path: "/bin/sh", Args: []string{"-c", `echo '{"type":"result","session_id":"sess-1"}'`, "sh"}}, nil
	})
	fill(t, e.store)

	check(t, doc, e, "POST", "/api/v0/events", `{"kind":"x.review.finding","session":"s1","text":"Line 12.","data":{"line":12}}`, true, 201)
	check(t, doc, e, "POST", "/api/v0/turns", `{"agent":"claude","folder":"`+folder+`","prompt":"hi"}`, true, 202)

	var snap store.Snapshot
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/sessions", "", true, 200), &snap); err != nil {
		t.Fatal(err)
	}
	q := "?store=" + url.QueryEscape(snap.Store) + "&after=0&limit=100"
	var page struct{ Events []map[string]any }
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/sessions/s1/events"+q, "", true, 200), &page); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, ev := range page.Events {
		kinds[ev["kind"].(string)] = true
	}
	for _, k := range []string{"turn.started", "model.requested", "model.responded", "tool.requested", "tool.completed",
		"turn.completed", "session.registered", "session.opened", "session.closed", "events.dropped", "schema.unexpected", "x.review.finding"} {
		if !kinds[k] {
			t.Errorf("the page has no %s to check", k)
		}
	}
	check(t, doc, e, "GET", "/api/v0/agents", "", true, 200)
	var turns store.TurnList
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/turns", "", true, 200), &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns.Turns) == 0 {
		t.Error("GET /api/v0/turns listed no turn to check")
	}
	check(t, doc, e, "GET", "/api/v0/turns?session=s1&limit=1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events"+q, "", true, 200)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events"+q+"&session=s1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events?session=a.b", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/turns?limit=0", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events?store=st_other&after=0", "", true, 409)
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(), Size: 60,
		Request: body.Request{Top: json.RawMessage(`{"model":"claude-sonnet-5-5"}`), Pieces: []body.Piece{
			{Kind: body.System, Label: "text", JSON: json.RawMessage(`"Be brief."`)},
			{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"hi"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	check(t, doc, e, "GET", "/api/v0/calls/x-1/context", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/context?full=false", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-2/context", "", true, 404)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/context?full=maybe", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/0", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/2", "", true, 404)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/reply", "", true, 404)
	var found store.SearchResult
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/sessions/s1/search?q=login&limit=5", "", true, 200), &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Hits) == 0 {
		t.Error("the search found no hit to check")
	}
	check(t, doc, e, "GET", "/api/v0/sessions/s1/search?q=hi&limit=1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/sessions/s1/search", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/openapi.json", "", false, 200)

	// Errors.
	check(t, doc, e, "GET", "/api/v0/sessions", "", false, 401)
	check(t, doc, e, "GET", "/api/v0/sessions/s1/events?store=st_other&after=0", "", true, 409)
	check(t, doc, e, "GET", "/api/v0/sessions/s1/events?store=x", "", true, 400)
	check(t, doc, e, "POST", "/api/v0/turns", `{"session":"nobody","prompt":"next"}`, true, 404)
	check(t, doc, e, "POST", "/api/v0/turns", `{"agent":"other","folder":"/","prompt":"hi"}`, true, 400)

	// The stream: every message is framed as the spec says, and its data
	// is an Event.
	ev := doc.Components.Schemas["Event"].Value
	s := openStream(t, e, "?store="+url.QueryEscape(snap.Store)+"&after=0")
	n := 0
	for n < len(page.Events) {
		id, kind, data := s.next(t)
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("data is not JSON: %s", data)
		}
		if want := snap.Store + "/" + strconv.Itoa(int(v["seq"].(float64))); id != want || kind != v["kind"] {
			t.Errorf("framing: id %q event %q, data seq %v kind %v", id, kind, v["seq"], v["kind"])
		}
		var anyV any
		_ = json.Unmarshal([]byte(data), &anyV)
		if err := ev.VisitJSON(anyV, openapi3.EnableJSONSchema2020()); err != nil {
			t.Errorf("a streamed event does not match Event: %v\n%s", err, data)
		}
		n++
	}

	// The Event schema refuses what it should, or it checks nothing.
	for _, bad := range []string{
		`{"seq":1,"id":"a","kind":"turn.completed","time":"2026-11-05T09:00:00Z","exchange":"","vendor":""}`,
		`{"seq":1,"id":"a","kind":"x.Bad","time":"2026-11-05T09:00:00Z","exchange":"","vendor":"program","session":"s"}`,
		`{"id":"a","kind":"turn.started","time":"2026-11-05T09:00:00Z","exchange":"","vendor":""}`,
	} {
		var v any
		_ = json.Unmarshal([]byte(bad), &v)
		if ev.VisitJSON(v, openapi3.EnableJSONSchema2020()) == nil {
			t.Errorf("Event accepts %s", bad)
		}
	}
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
