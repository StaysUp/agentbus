package api

// The OpenAPI 3.1 description, built once at startup from the route table
// (routes.go) and the Go types the handlers send and read. Schemas come from
// kin-openapi's openapi3gen. A field's description is its `doc` tag; a field
// is required unless its json tag says omitempty. A field added to a type
// appears here with no other edit.
//
// What the types cannot say is in constraints below: patterns and bounds,
// taken from the code's own values.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/staysup/agentbus/agents"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/settings"
)

const specIntro = "agentbus runs on your machine, between your coding agents and their vendors. " +
	"It stores each step of their work as an event. This API reads those events, starts agents, " +
	"and takes a program's own events.\n\n" +
	"This document is generated from the code at startup, and it is the contract. " +
	"A short guide for coding agents is at `/docs/llms.txt`.\n\n" +
	"- `agentbus up` runs turns on this machine. `agentbus server` queues them for runners on other machines. " +
	"A runner's own service starts no turns (404) and keeps no settings of its own (409 on a change).\n" +
	"- Three nouns: an agent is a saved setup, a session is a conversation, and a turn is one message, its reply and its changes. " +
	"Every action is on a turn: start, stop, diff, apply, review and its model calls.\n" +
	"- Every request but this document sends `Authorization: Bearer <token>`: the file `token` in the data folder. Never put it in a URL. " +
	"The data folder is `$AGENTBUS_HOME`, else `$XDG_DATA_HOME/agentbus`, else `~/.local/share/agentbus`; `token` and `turns_token` there are mode 0600.\n" +
	"- A turn exists from the moment it is asked for: `queued`, then `running`, then `finished`, `failed` or `stopped`.\n" +
	"- Runners talk to this service through the runner protocol, under `/api/v0/runner/`. It is described on its own, at `/api/v0/runner/openapi.json`, and is not a public promise.\n" +
	"- A turn started by `POST /api/v0/turns` gets its own token in `$AGENTBUS_TOKEN`, and this service's address in `$AGENTBUS_URL`. " +
	"With it the agent reads its session and asks other agents: it starts turns, continues the sessions it started, asks for reviews and reads their results, on the operations that say so. " +
	"Each result comes back to the asking session as a later turn. It never changes policy or saved agents, never sees a token, " +
	"and stops working when the turn's child exits. A read-only turn's token only reads. " +
	"An asked turn finds who asked it in `$AGENTBUS_ASKED_BY_SESSION` and `$AGENTBUS_ASKED_BY_TURN`. " +
	"Its copy does not hold the asker's unapplied changes: read them through this API.\n" +
	"- The turns token, the file `turns_token` in the data folder, may only start, stop, review and read turns, on the operations that say so. " +
	"A program that only starts and follows turns uses it in place of the main token.\n" +
	"- On loopback, the default, the `Host` must be `127.0.0.1`, `localhost` or `[::1]` with the service's port, or the answer is 403. " +
	"On another address, such as `0.0.0.0`, any `Host` is accepted and the token is the guard.\n" +
	"- A browser page from any origin may call the API: CORS answers `Access-Control-Allow-Origin: *` and allows `Authorization`. " +
	"No cookies are used, so the token is the guard. `agentbus up -origin <url>` locks it to that one origin. " +
	"A preflight (`OPTIONS`) under `/api/` gets 204 without a token and allows `Authorization`, `Content-Type` and `If-Match`; " +
	"replies expose `ETag` and the `X-Piece-*` headers. A preflight elsewhere gets 403.\n" +
	"- A path with a `.`, `..` or empty segment, or an encoded dot, slash or backslash, gets 400 before any route. " +
	"Everything under `/api` is answered here and never sent on to a vendor.\n" +
	"- Start up with `GET /api/v0/sessions`, then stream `GET /api/v0/events` from its `store` and `seq`. Nothing is lost or counted twice.\n" +
	"- A cursor is a `store` and a `seq`. A cursor of another store gets 409: start over from the snapshot.\n" +
	"- v0 may change without notice. Ignore kinds and fields you do not know."

// turnsTokenDoc is what an operation the turns token may call adds to its
// description.
const turnsTokenDoc = "The turns token (the file `turns_token` in the data folder) may call this too."

// runnerTokenDoc is what an operation of the runner protocol adds to its
// description: it checks its own token.
const runnerTokenDoc = "Runner protocol: called by a runner, never by the cockpit. Takes the enrollment token (register) or the runner's own token (every other call), not the main token."

// turnTokenDoc is what each turn-token rule adds to an operation's
// description.
var turnTokenDoc = map[int]string{
	treeSessionPath:  "A turn token may call this for a session in its request tree: its own, the person's session it started from, or one work in that tree started.",
	treeSessionQuery: "A turn token may call this for a session in its request tree: without session, its own is used.",
	treeTurn:         "A turn token may call this for a turn of a session in its request tree, by the turn id alone.",
	askTurn: "A turn token may call this to ask another agent, unless its turn is read-only. " +
		"A new session must be in the turn's own folder or one inside it (the person's folder when absent), always isolated. " +
		"A session it continues must be one its request tree started, never its own or the person's. " +
		"The turn's result comes back to the asking session as a later turn. Caps: " + askCaps,
	askReview: "A turn token may call this for a turn of a session in its request tree, its own session's turns included, unless its turn is read-only. " +
		"The review's result comes back to the asking session as a later turn. Caps: " + askCaps,
	askedTurn: "A turn token may stop only a turn its request tree asked for, unless its turn is read-only.",
	anyRead:   "A turn token may call this too.",
	applyTurnTok: "A turn token may call this for a turn of a session in its scope only when [agent_api] apply is on (off by default), " +
		"and never from a read-only turn.",
	renameSession: "A turn token may rename a session in its scope, unless its turn is read-only. It never closes one.",
	toolList: "A turn token may call this: its list holds the tools its turn may call under [agent_api] (none of them when tools is false), " +
		"and each call is checked by the operation it names.",
}

// askCaps names the caps on asked work. A request past one gets 403 saying which.
var askCaps = fmt.Sprintf("[limits] in the settings (PUT /api/v0/settings/permissions): by default at most %d levels below the person's turn, %d jobs asked by one turn, "+
	"%d in one request tree, and no cost cap (max_cost_usd_per_tree). [agent_api] there can turn each kind of call off, "+
	"and narrow the scope to the token's own session.", settings.DefaultMaxDepth, settings.DefaultMaxJobsPerTurn, settings.DefaultMaxJobsPerTree)

// spec is the description as served, built on first use. New builds it, so a
// table or type it cannot describe stops the service from starting.
// runnerSpec is the runner protocol's, served on its own.
var (
	spec       = sync.OnceValues(func() ([]byte, error) { return buildSpec(false) })
	runnerSpec = sync.OnceValues(func() ([]byte, error) { return buildSpec(true) })
)

// constraints hold what a Go type cannot say, keyed by "Type" for the schema
// of a type and "Type.field" for one of its fields.
var constraints = map[string]func(*openapi3.Schema){
	// A stored event always has its seq; event.Event omits it only before
	// it is stored.
	"EventFields":     func(s *openapi3.Schema) { s.Required = append([]string{"seq"}, s.Required...) },
	"EventFields.seq": minimum(1),
	"EventFields.data": func(s *openapi3.Schema) {
		s.Type = &openapi3.Types{"object"}
	},

	"TurnRequest":           closed,
	"TurnRequest.agent":     pattern(sessionID.String()),
	"TurnRequest.folder":    maxLength(MaxFolder),
	"TurnRequest.prompt":    minLength(1),
	"TurnRequest.model":     pattern(runModel.String()),
	"TurnRequest.read_only": defaultFalse,
	"TurnRequest.session":   pattern(sessionID.String()),

	"AgentRequest":         closed,
	"AgentRequest.name":    func(s *openapi3.Schema) { minLength(1)(s); maxLength(agents.MaxName)(s) },
	"AgentRequest.harness": func(s *openapi3.Schema) { s.Enum = harnessEnum() },
	"AgentRequest.access":  func(s *openapi3.Schema) { s.Enum = []any{agents.Subscription, agents.OpenRouter} },
	"AgentRequest.model":   pattern(runModel.String()),
	"AgentRequest.key_env": pattern(agents.KeyEnvShape()),

	"EventRequest":         closed,
	"EventRequest.kind":    pattern(customKind.String()),
	"EventRequest.session": pattern(sessionID.String()),
	"EventRequest.turn":    maxLength(MaxEventTurn),
	"EventRequest.data":    func(s *openapi3.Schema) { s.Type = &openapi3.Types{"object"} },

	"CallContext.request": func(s *openapi3.Schema) { s.Type = &openapi3.Types{"object"} },
	"Piece.kind": func(s *openapi3.Schema) {
		s.Enum = []any{body.System, body.Tool, body.Message, body.Instructions, body.InputItem}
	},

	"Page.events": func(s *openapi3.Schema) { s.Items = openapi3.NewSchemaRef("#/components/schemas/Event", nil) },
}

func minimum(n float64) func(*openapi3.Schema) { return func(s *openapi3.Schema) { s.Min = &n } }
func minLength(n uint64) func(*openapi3.Schema) {
	return func(s *openapi3.Schema) { s.MinLength = n }
}
func maxLength(n uint64) func(*openapi3.Schema) {
	return func(s *openapi3.Schema) { s.MaxLength = &n }
}
func pattern(p string) func(*openapi3.Schema) { return func(s *openapi3.Schema) { s.Pattern = p } }
func closed(s *openapi3.Schema) {
	no := false
	s.AdditionalProperties = openapi3.AdditionalProperties{Has: &no}
}
func defaultFalse(s *openapi3.Schema) { s.Default = false }

func anys[T any](xs []T) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

var (
	timeType    = reflect.TypeFor[time.Time]()
	kindType    = reflect.TypeFor[event.Kind]()
	outcomeType = reflect.TypeFor[event.Outcome]()
)

// typeName names a type's component. event.Event is EventFields: Event is
// the schema that adds what each kind always carries.
func typeName(t reflect.Type) string {
	if t == reflect.TypeFor[event.Event]() {
		return "EventFields"
	}
	return t.Name()
}

// omitempty says the field is left out when empty or zero, so it is not
// required.
func omitempty(tag reflect.StructTag) bool {
	_, opts, _ := strings.Cut(tag.Get("json"), ",")
	return strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,")
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// customize runs on every schema openapi3gen makes. tag is the tag of the
// field the schema is for.
func customize(_ string, t reflect.Type, tag reflect.StructTag, s *openapi3.Schema) error {
	if t.Kind() == reflect.Struct && t != timeType {
		// A struct becomes a shared component: a field's description and
		// nullability belong to the field, so components.finish sets them.
		for _, f := range fieldsOf(t) {
			if n := jsonName(f); n != "" && n != "-" && !omitempty(f.Tag) {
				s.Required = append(s.Required, n)
			}
		}
		return nil
	}
	s.Description = tag.Get("doc")
	if s.Nullable {
		// openapi3gen marks a pointer nullable, the OpenAPI 3.0 way. In 3.1
		// null is a type, and a pointer with omitempty is never sent as null.
		s.Nullable = false
		if s.Type != nil && !omitempty(tag) {
			*s.Type = append(*s.Type, "null")
		}
	}
	switch t {
	case kindType:
		var kinds []any
		for _, k := range event.Kinds {
			kinds = append(kinds, string(k.Kind))
		}
		s.AnyOf = openapi3.SchemaRefs{
			{Value: &openapi3.Schema{Enum: kinds}},
			{Value: &openapi3.Schema{Pattern: customKind.String()}},
		}
	case outcomeType:
		s.Enum = anys(event.Outcomes)
	}
	return nil
}

// components collects the shared schemas and the Go type behind each.
type components struct {
	schemas openapi3.Schemas
	types   map[string]reflect.Type
	// clash is the first name two Go types share: their schemas would be
	// one, and one of them wrong.
	clash error
}

// ref returns the schema of v's type, adding the structs it uses to the
// components.
func (c *components) ref(v any) (*openapi3.SchemaRef, error) {
	c.collect(reflect.TypeOf(v))
	g := openapi3gen.NewGenerator(openapi3gen.SchemaCustomizer(customize),
		openapi3gen.CreateTypeNameGenerator(typeName),
		openapi3gen.CreateComponentSchemas(openapi3gen.ExportComponentSchemasOptions{
			ExportComponentSchemas: true, ExportTopLevelSchema: true}))
	return g.NewSchemaRefForValue(v, c.schemas)
}

func (c *components) collect(t reflect.Type) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == timeType {
		return
	}
	if have := c.types[typeName(t)]; have != nil {
		if have != t && c.clash == nil {
			c.clash = fmt.Errorf("api: two types are both named %s: %v and %v", typeName(t), have, t)
		}
		return
	}
	c.types[typeName(t)] = t
	for _, f := range fieldsOf(t) {
		c.collect(f.Type)
	}
}

// fieldsOf is t's fields, with an embedded struct's fields in its place, as
// encoding/json sends them.
func fieldsOf(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Tag.Get("json") == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, fieldsOf(f.Type)...)
			continue
		}
		out = append(out, f)
	}
	return out
}

// finish gives each component's fields their descriptions and constraints.
func (c *components) finish() error {
	if c.clash != nil {
		return c.clash
	}
	for name, t := range c.types {
		sr := c.schemas[name]
		if sr == nil || sr.Value == nil {
			return fmt.Errorf("api: no schema for %s", name)
		}
		s := sr.Value
		for _, f := range fieldsOf(t) {
			n := jsonName(f)
			p := s.Properties[n]
			if p == nil {
				continue
			}
			if p.Ref != "" {
				// A field of a component type: openapi3gen gives a bare $ref,
				// which can carry neither a description nor null.
				doc := f.Tag.Get("doc")
				switch {
				case f.Type.Kind() == reflect.Pointer && !omitempty(f.Tag):
					p = &openapi3.SchemaRef{Value: &openapi3.Schema{Description: doc,
						AnyOf: openapi3.SchemaRefs{p, {Value: &openapi3.Schema{Type: &openapi3.Types{"null"}}}}}}
				case doc != "":
					p = &openapi3.SchemaRef{Value: &openapi3.Schema{Description: doc, AllOf: openapi3.SchemaRefs{p}}}
				}
			} else {
				// openapi3gen hands the field's tag to its items too.
				if it := p.Value.Items; it != nil && it.Value != nil {
					it.Value.Description = ""
				}
				if ap := p.Value.AdditionalProperties.Schema; ap != nil && ap.Value != nil {
					ap.Value.Description = ""
				}
			}
			if fix := constraints[name+"."+n]; fix != nil {
				fix(p.Value)
			}
			s.Properties[n] = p
		}
		if fix := constraints[name]; fix != nil {
			fix(s)
		}
	}
	return nil
}

// checkConstraints refuses a constraint that names no type or field, so a
// renamed field cannot leave its bound behind unnoticed.
func (c *components) checkConstraints() error {
	for key := range constraints {
		name, field, _ := strings.Cut(key, ".")
		t := c.types[name]
		if t == nil {
			return fmt.Errorf("api: a constraint names %s, which no route uses", name)
		}
		if field != "" && c.schemas[name].Value.Properties[field] == nil {
			return fmt.Errorf("api: a constraint names %s.%s, which %s does not have", name, field, name)
		}
	}
	return nil
}

// kindSchemas adds Event: every field, plus what each kind always carries.
func (c *components) kindSchemas() {
	var one openapi3.SchemaRefs
	for _, k := range event.Kinds {
		name := ""
		for _, part := range strings.Split(string(k.Kind), ".") {
			name += strings.ToUpper(part[:1]) + part[1:]
		}
		c.schemas[name] = &openapi3.SchemaRef{Value: &openapi3.Schema{
			Title: string(k.Kind), Description: k.Doc, Type: &openapi3.Types{"object"},
			Required:   append([]string{"kind"}, k.Needs...),
			Properties: openapi3.Schemas{"kind": {Value: &openapi3.Schema{Const: string(k.Kind)}}},
		}}
		one = append(one, openapi3.NewSchemaRef("#/components/schemas/"+name, nil))
	}
	c.schemas["ProgramEvent"] = &openapi3.SchemaRef{Value: &openapi3.Schema{
		Title: "x.<program>.<name>", Description: "A program's own event, from POST /api/v0/events: text, data, turn.",
		Type: &openapi3.Types{"object"}, Required: []string{"kind", "vendor", "session"},
		Properties: openapi3.Schemas{
			"kind":   {Value: &openapi3.Schema{Type: &openapi3.Types{"string"}, Pattern: customKind.String()}},
			"vendor": {Value: &openapi3.Schema{Const: "program"}},
		},
	}}
	one = append(one, openapi3.NewSchemaRef("#/components/schemas/ProgramEvent", nil))
	c.schemas["Event"] = &openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: "One step of an agent's work. Ignore kinds and fields you do not know. " +
			"A built-in kind has two parts, such as turn.started; a program's has three and starts with x. " +
			"A runner's reports on its attempts stay in the queue and never reach this API.",
		AllOf: openapi3.SchemaRefs{openapi3.NewSchemaRef("#/components/schemas/EventFields", nil)},
		OneOf: one,
	}}
}

// describe builds the description as a document.
// describe is the description of the client API, or with runner of the
// runner protocol: the routes that check their own token.
func describe(runner bool) (*openapi3.T, error) {
	c := &components{schemas: openapi3.Schemas{}, types: map[string]reflect.Type{}}
	if !runner {
		if _, err := c.ref(event.Event{}); err != nil {
			return nil, err
		}
	}
	errRef, err := c.ref(Error{})
	if err != nil {
		return nil, err
	}
	doc := &openapi3.T{
		OpenAPI:           "3.1.0",
		JSONSchemaDialect: "https://spec.openapis.org/oas/3.1/dialect/base",
		Info: &openapi3.Info{Title: "agentbus client API", Version: "v0",
			Summary:     "What agents, cockpits and programs call on an agentbus service.",
			Description: specIntro, License: &openapi3.License{Name: "Functional Source License 1.1, ALv2 Future License", Identifier: "FSL-1.1-ALv2"}},
		Servers:  openapi3.Servers{{URL: "http://127.0.0.1:8787", Description: "The default address. Use your service's port."}},
		Security: openapi3.SecurityRequirements{{"bearerToken": {}}},
		Paths:    openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: c.schemas,
			SecuritySchemes: openapi3.SecuritySchemes{"bearerToken": {Value: &openapi3.SecurityScheme{
				Type: "http", Scheme: "bearer", Description: "The file `token` in the data folder."}}},
		},
	}
	errorReply := func(desc string) *openapi3.ResponseRef {
		return &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription(desc).WithJSONSchemaRef(errRef)}
	}
	// An error reply that two or more operations give with the same words is
	// described once, in components.responses, and each operation points at
	// it: the 403 alone is on almost every route.
	uses := map[string]int{}
	for _, rt := range routes {
		if rt.self != runner {
			continue
		}
		for _, r := range rt.replies {
			if r.errorOnly() {
				uses[replyKey(r.status, r.doc)]++
			}
		}
		for _, r := range accessReplies(rt) {
			uses[replyKey(r.status, r.doc)]++
		}
	}
	doc.Components.Responses = openapi3.ResponseBodies{}
	sharedName := map[string]string{}
	errorFor := func(status int, desc, name string) *openapi3.ResponseRef {
		key := replyKey(status, desc)
		if uses[key] < 2 {
			return errorReply(desc)
		}
		if sharedName[key] == "" {
			if name == "" {
				name = strings.ReplaceAll(http.StatusText(status), " ", "")
			}
			for n, base := 2, name; doc.Components.Responses[name] != nil; n++ {
				name = base + strconv.Itoa(n)
			}
			sharedName[key] = name
			doc.Components.Responses[name] = errorReply(desc)
		}
		name = sharedName[key]
		return &openapi3.ResponseRef{Ref: "#/components/responses/" + name, Value: doc.Components.Responses[name].Value}
	}
	// Each tag's routes, counted, so a route cannot name a tag that is not
	// listed and a listed tag cannot be empty.
	if runner {
		doc.Info.Title, doc.Info.Summary, doc.Info.Description = "agentbus runner protocol", "What runners call on control.", runnerIntro
	}
	tagged := map[string]int{}
	for _, t := range tags {
		if t.runner != runner {
			continue
		}
		doc.Tags = append(doc.Tags, &openapi3.Tag{Name: t.name, Description: t.doc})
		tagged[t.name] = 0
	}
	for _, rt := range routes {
		if rt.self != runner {
			continue
		}
		if _, ok := tagged[rt.tag]; !ok {
			return nil, fmt.Errorf("api: %s %s names tag %q, which is not in tags", rt.method, rt.path, rt.tag)
		}
		tagged[rt.tag]++
		desc := rt.doc
		if rt.turn != mainOnly {
			desc = strings.TrimSpace(desc + "\n\n" + turnTokenDoc[rt.turn])
		}
		if rt.turns {
			desc = strings.TrimSpace(desc + "\n\n" + turnsTokenDoc)
		}
		if rt.self {
			desc = strings.TrimSpace(desc + "\n\n" + runnerTokenDoc)
		}
		op := &openapi3.Operation{OperationID: rt.op, Tags: []string{rt.tag}, Summary: rt.summary, Description: desc,
			Responses: openapi3.NewResponsesWithCapacity(len(rt.replies) + 2)}
		if len(rt.tools) > 0 {
			// The tools this operation is, for POST /api/v0/mcp (tools.go).
			var ts []map[string]string
			for _, t := range rt.tools {
				ts = append(ts, map[string]string{"name": t.name, "description": t.doc, "effect": t.effect})
			}
			op.Extensions = map[string]any{"x-agentbus-tools": ts}
		}
		if strings.Contains(rt.path, "{id}") {
			idDoc := rt.idDoc
			if idDoc == "" {
				idDoc = "A session id. Another shape gets 404."
			}
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewPathParameter("id").
				WithDescription(idDoc).
				WithSchema(&openapi3.Schema{Type: &openapi3.Types{"string"}, Pattern: sessionID.String()})})
		}
		if strings.Contains(rt.path, "{call}") {
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewPathParameter("call").
				WithDescription("A model call of the turn: the exchange field of its events, as the turn's diff lists it in model_calls.").
				WithSchema(&openapi3.Schema{Type: &openapi3.Types{"string"}, Pattern: sessionID.String()})})
		}
		if strings.Contains(rt.path, "{n}") {
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewPathParameter("n").
				WithDescription("The piece's index, from 0, or reply for the call's kept reply.").
				WithSchema(&openapi3.Schema{Type: &openapi3.Types{"string"}, Pattern: pieceNumber.String()})})
		}
		for _, q := range rt.query {
			s := &openapi3.Schema{Type: &openapi3.Types{"string"}}
			if q.integer {
				min := float64(q.min)
				s = &openapi3.Schema{Type: &openapi3.Types{"integer"}, Min: &min}
				if q.max != 0 {
					max := float64(q.max)
					s.Max = &max
				}
			}
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewQueryParameter(q.name).
				WithDescription(q.doc).WithSchema(s)})
		}
		if rt.versioned && rt.method != http.MethodGet {
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewHeaderParameter("If-Match").WithRequired(true).
				WithDescription("The ETag of your GET, as it came, or * to replace whatever is there.").
				WithSchema(&openapi3.Schema{Type: &openapi3.Types{"string"}})})
		}
		if rt.body != nil {
			ref, err := c.ref(rt.body)
			if err != nil {
				return nil, err
			}
			op.RequestBody = &openapi3.RequestBodyRef{Value: openapi3.NewRequestBody().WithRequired(true).WithJSONSchemaRef(ref)}
		}
		for _, r := range rt.replies {
			var resp *openapi3.ResponseRef
			switch {
			case r.stream:
				content := openapi3.NewContentWithSchema(&openapi3.Schema{Type: &openapi3.Types{"string"}}, []string{"text/event-stream"})
				content["text/event-stream"].Example = r.example
				resp = &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription(r.doc).WithContent(content)}
			case r.raw:
				// Any JSON value: the bytes as stored, never re-encoded.
				resp = &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription(r.doc).
					WithContent(openapi3.NewContentWithJSONSchema(&openapi3.Schema{}))}
			case r.empty:
				resp = &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription(r.doc)}
			case r.body == nil && !(rt.versioned && r.status == http.StatusServiceUnavailable):
				resp = errorFor(r.status, r.doc, "")
			case r.body == nil:
				resp = errorReply(r.doc)
			default:
				ref, err := c.ref(r.body)
				if err != nil {
					return nil, err
				}
				resp = &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription(r.doc).WithJSONSchemaRef(ref)}
			}
			if rt.versioned && (r.status == http.StatusOK || r.status == http.StatusServiceUnavailable) {
				resp.Value.Headers = openapi3.Headers{"ETag": &openapi3.HeaderRef{Value: &openapi3.Header{Parameter: openapi3.Parameter{
					Description: "The setting's version, quoted. Send it in If-Match to change the setting.",
					Schema:      &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}}}}}
			}
			if op.Responses.Value(strconv.Itoa(r.status)) != nil {
				return nil, fmt.Errorf("api: %s %s lists %d twice", rt.method, rt.path, r.status)
			}
			op.Responses.Set(strconv.Itoa(r.status), resp)
		}
		for _, r := range accessReplies(rt) {
			op.Responses.Set(strconv.Itoa(r.status), errorFor(r.status, r.doc, r.name))
		}
		if rt.public {
			op.Security = openapi3.NewSecurityRequirements()
		}
		item := doc.Paths.Value(rt.path)
		if item == nil {
			item = &openapi3.PathItem{}
			doc.Paths.Set(rt.path, item)
		}
		if item.GetOperation(rt.method) != nil {
			return nil, fmt.Errorf("api: %s %s is in the route table twice", rt.method, rt.path)
		}
		item.SetOperation(rt.method, op)
	}
	for _, t := range tags {
		if t.runner == runner && tagged[t.name] == 0 {
			return nil, fmt.Errorf("api: tag %q has no route", t.name)
		}
	}
	if err := c.finish(); err != nil {
		return nil, err
	}
	if !runner {
		if err := c.checkConstraints(); err != nil {
			return nil, err
		}
		c.kindSchemas()
	}
	return doc, nil
}

// replyKey names an error reply by its status and words.
func replyKey(status int, doc string) string { return strconv.Itoa(status) + " " + doc }

// errorOnly says r is an error reply described by its words alone, which
// describe may share between operations.
func (r reply) errorOnly() bool { return r.body == nil && !r.stream && !r.raw && !r.empty }

// accessReply is a reply every route of a kind gives: the 403 for a wrong
// Host or token, and the 401. name is its entry in components.responses.
type accessReply struct {
	status    int
	doc, name string
}

// accessReplies are the 403 and, for a route with a token, the 401 that
// describe adds to rt.
func accessReplies(rt route) []accessReply {
	forbidden := "The service is on loopback and the Host header does not name it on 127.0.0.1, localhost or [::1]."
	name := "ForbiddenHost"
	if !rt.public && !rt.self {
		forbidden += " Or a turn token was sent where it may not read: another session, or an operation that does not take one."
		name = "ForbiddenToken"
		if !rt.turns {
			forbidden += " Or the turns token was sent: it may only start, stop, review and read turns."
			name = "ForbiddenMainToken"
		}
	}
	out := []accessReply{{status: http.StatusForbidden, doc: forbidden, name: name}}
	if !rt.public {
		out = append(out, accessReply{status: http.StatusUnauthorized, doc: "A missing or wrong token.", name: "Unauthorized"})
	}
	return out
}

// runnerIntro opens the runner protocol's description.
const runnerIntro = "How a runner, the one program of each machine, talks to control: register, take jobs, report their attempts, and move files as blobs. " +
	"A job is the queue's word for work: a turn, a review, an apply or a snapshot. A runner takes a job as an attempt, and every call after take names the attempt and its number. " +
	"This protocol is our own code's, between our own binaries. It is not a public promise and may change in any release. " +
	"The client API, for people, programs and agents, is at `/api/v0/openapi.json`."

// buildSpec is describe, as the bytes served.
func buildSpec(runner bool) ([]byte, error) {
	doc, err := describe(runner)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// Encoded again without Go's HTML escaping of <, > and &: this is not
	// HTML, and agents read it.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// harnessEnum is the harnesses an agent can be saved on.
func harnessEnum() []any {
	var out []any
	for _, n := range harness.Names() {
		out = append(out, n)
	}
	return out
}
