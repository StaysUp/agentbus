package api

// The route table. The router dispatches from it and the OpenAPI description
// is built from it (spec.go), so a route cannot be served undocumented.
//
// The table is reached only after ServeHTTP's Host check, CORS and token
// check, which stay where they are. A public row is answered by public, before
// the token check; the router never serves one.

import (
	"net/http"
	"strings"

	"github.com/staysup/agentbus/store"
)

// route is one operation: a method on a path.
type route struct {
	method string
	// path may hold {id}: a session, turn or call id, matched by sessionID.
	// Any other id shape gets 404.
	path  string
	idDoc string // what {id} names, for the description; empty means a session id
	op    string // the operationId
	tag   string // the section of the docs it is listed in: one of tags
	// summary names the action in a few words, such as "List turns". It is
	// the heading on /docs and its entry in the side menu.
	summary string
	// doc is the description. Its first sentence says what the route does.
	doc     string
	query   []param
	body    any // a zero value of the request body's type; nil for none
	replies []reply
	// public routes need no token. The router skips them: public answers
	// them before the token check.
	public bool
	// turn says whether a turn token may call the route, and how its session
	// is checked (tokens.go). The zero value, mainOnly, refuses it.
	turn  int
	serve func(a *API, w http.ResponseWriter, r *http.Request, id string)
}

// param is one query parameter.
type param struct {
	name, doc string
	integer   bool
	min, max  int // for an integer; max 0 means none
}

// reply is one status a route answers with. Every route also answers 403 for
// a wrong Host, and every route with a token 401; spec.go adds those.
type reply struct {
	status  int
	doc     string
	body    any    // a zero value of the JSON type; nil means Error
	stream  bool   // text/event-stream
	raw     bool   // application/json of any shape, sent as stored
	example string // for a stream: what it sends, so the docs show a sample
}

func ok(status int, body any, doc string) reply { return reply{status: status, doc: doc, body: body} }
func fail(status int, doc string) reply         { return reply{status: status, doc: doc} }
func streamReply(doc, example string) reply {
	return reply{status: http.StatusOK, doc: doc, stream: true, example: example}
}

// eventsExample is what GET /api/v0/events sends: one message per event.
const eventsExample = "id: st_9f2c/1042\nevent: tool.requested\ndata: {\"seq\":1042,\"id\":\"evt_7a1b\",\"kind\":\"tool.requested\",\"session\":\"abc\",\"tool_name\":\"Read\"}\n\n" +
	"id: st_9f2c/1043\nevent: tool.completed\ndata: {\"seq\":1043,\"id\":\"evt_7a1c\",\"kind\":\"tool.completed\",\"session\":\"abc\"}\n\n" +
	": ping\n\n"

// plain adapts a handler that takes no session id.
func plain(f func(*API, http.ResponseWriter, *http.Request)) func(*API, http.ResponseWriter, *http.Request, string) {
	return func(a *API, w http.ResponseWriter, r *http.Request, _ string) { f(a, w, r) }
}

var (
	qStore = param{name: "store", doc: "The store id of your cursor. Sent with after, or neither is sent."}
	qAfter = param{name: "after", doc: "The seq of your cursor: events after it are sent.", integer: true}

	storeFailed = fail(http.StatusInternalServerError, "The store could not be read or written.")
	tooMany     = fail(http.StatusTooManyRequests, "Too many at once; close one, or wait, and retry.")
	stopping    = fail(http.StatusServiceUnavailable, "The service is stopping.")
	badRequest  = fail(http.StatusBadRequest, "The request is refused; error says why.")
	noSession   = fail(http.StatusNotFound, "No such session.")
	mismatch    = ok(http.StatusConflict, StoreMismatch{}, "The cursor belongs to another store. Start over from the snapshot.")
)

// startTurn's 400 and 500, saying what each means for a turn.
var (
	turnRefused = fail(http.StatusBadRequest, "The request is refused; error says why. "+
		"This includes an agent whose program is not on the service's PATH: error names the agent and how to install it, "+
		"and nothing is stored. GET /api/v0/agents shows which agents the service finds.")
	turnFailed = fail(http.StatusInternalServerError, "The store could not be written, or the agent's program would not start.")
)

// streamDoc is the one description of the event stream's framing.
const streamDoc = "Server-sent events. Every stored event after the cursor, in order, then new ones as they land. " +
	"Without store and after it starts with new events only. Each message is `id: <store>/<seq>`, " +
	"`event: <kind>`, `data: <one Event as JSON on one line>`, then a blank line. " +
	"A `: ping` comment comes every 15 seconds. To resume, reconnect with the last store and seq you saw."

// tags are the sections of the docs, in the order they are read: a new
// user starts a turn first. Every route names one.
var tags = []struct{ name, doc string }{
	{"Turns", "Start an agent and follow what it does. A turn is one prompt and the work it starts."},
	{"Sessions", "The sessions the service has seen, and each one's stored history."},
	{"Events", "The live stream of every event, and a program's own events."},
	{"Calls", "What one model call sent to the vendor, piece by piece, and the reply it got."},
	{"Service", "What this service can start, and this description."},
}

var routes = []route{
	{method: "GET", path: "/api/v0/sessions", op: "listSessions",
		tag:     "Sessions",
		summary: "List sessions",
		doc:     "The sessions, and the seq they were computed at.",
		replies: []reply{ok(200, store.Snapshot{}, "The snapshot."), storeFailed},
		serve:   plain((*API).sessions)},
	{method: "GET", path: "/api/v0/events", op: "streamEvents",
		tag:     "Events",
		summary: "Stream events",
		doc:     "A live stream of events.\n\n" + streamDoc,
		query: []param{qStore, qAfter,
			{name: "session", doc: "Only this session's events, plus the drops that name it."}},
		replies: []reply{streamReply("The stream.", eventsExample), badRequest, mismatch, tooMany,
			fail(500, "This connection cannot stream.")},
		serve: plain((*API).stream)},
	{method: "POST", path: "/api/v0/events", op: "writeEvent",
		tag:     "Events",
		summary: "Write a program event",
		doc:     "A program writes its own event.",
		body:    EventRequest{},
		replies: []reply{ok(201, Written{}, "Stored."), badRequest, noSession, storeFailed},
		serve:   plain((*API).writeEvent)},
	{method: "GET", path: "/api/v0/sessions/{id}/events", op: "sessionEvents",
		tag:     "Sessions",
		summary: "Read a session's events",
		doc:     "One session's stored events, oldest first, a page at a time.",
		query: []param{qStore, qAfter,
			{name: "limit", doc: "Events per page. 1000 when absent.", integer: true, min: 1, max: maxLimit}},
		replies: []reply{ok(200, Page{}, "A page."), badRequest, mismatch, storeFailed},
		turn:    ownSessionPath,
		serve:   (*API).sessionEvents},
	{method: "GET", path: "/api/v0/sessions/{id}/search", op: "searchSession",
		tag:     "Sessions",
		summary: "Search a session",
		doc:     "Search one session's stored history for a piece of text, newest first.\n\n" + searchDoc,
		query: []param{
			{name: "q", doc: "The text to find: 1 to 256 bytes of UTF-8, no NUL byte. Case is ignored."},
			{name: "before", doc: "The next_before of the previous page. Absent: start from the newest."},
			{name: "limit", doc: "Hits per page. 20 when absent.", integer: true, min: 1, max: maxSearchLimit}},
		replies: []reply{ok(200, store.SearchResult{}, "A page of hits."), badRequest, storeFailed},
		turn:    ownSessionPath,
		serve:   (*API).search},
	{method: "POST", path: "/api/v0/turns", op: "startTurn",
		tag:     "Turns",
		summary: "Start a turn",
		doc: "Start a turn: in a new session, or with session the next turn of that one; its events carry the turn id the reply names.\n\n" +
			"Without session, agent, folder and prompt are required and the turn opens a new session. " +
			"With session, prompt is required and the turn continues that session in its own agent and folder; " +
			"an agent or folder sent that differs from the session's is refused with 400. " +
			"The turn's turn.started, with the prompt, is stored before the reply.",
		body: TurnRequest{},
		replies: []reply{ok(202, Started{}, "The agent has started."), turnRefused,
			fail(404, "Starting turns is turned off in this service, or the session sent has no events."),
			fail(409, "A turn is already running in the session sent, or it cannot be continued from here."),
			tooMany, turnFailed, stopping},
		serve: plain((*API).run)},
	{method: "GET", path: "/api/v0/turns", op: "listTurns",
		tag:     "Turns",
		summary: "List turns",
		doc: "The turns, newest first, and the seq they were computed at.\n\n" +
			"A turn is what a turn.started with a turn id starts. Page older turns with before set to the " +
			"next_before of the page before. Stream from the list's store and seq to follow new events.",
		query: []param{
			{name: "session", doc: "Only this session's turns."},
			{name: "before", doc: "Only turns whose started_seq is below it: the next_before of the previous page.", integer: true, min: 1},
			{name: "limit", doc: "Turns per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies: []reply{ok(200, store.TurnList{}, "A page of turns."), badRequest, storeFailed},
		turn:    ownSessionQuery,
		serve:   plain((*API).turns)},
	{method: "GET", path: "/api/v0/turns/{id}/events", op: "turnEvents",
		idDoc:   "A turn id: the turn field of its events. Another shape gets 404.",
		tag:     "Turns",
		summary: "Read a turn's events",
		doc: "One turn's stored events, oldest first, a page at a time.\n\n" +
			"Only events that carry the turn id: events.dropped names sessions, never turns, so it is not among them.",
		query: []param{qStore, qAfter,
			{name: "session", doc: "Only this session's events of the turn. The same turn id can be in two sessions; without session both are sent."},
			{name: "limit", doc: "Events per page. 1000 when absent.", integer: true, min: 1, max: maxLimit}},
		replies: []reply{ok(200, Page{}, "A page."), badRequest, mismatch, storeFailed},
		turn:    ownSessionQuery,
		serve:   (*API).turnEvents},
	{method: "POST", path: "/api/v0/turns/{id}/stop", op: "stopTurn",
		idDoc:   "A turn id, as POST /api/v0/turns named it. Another shape gets 404.",
		tag:     "Turns",
		summary: "Stop a turn",
		doc: "Stop a running turn: its agent and every process it started end, as when the service stops.\n\n" +
			"The service sends SIGTERM to the turn's whole process tree, then SIGKILL after 5 seconds to what still runs. " +
			"The reply comes at once; the turn's session.closed, with stopped true, comes once the tree has ended. " +
			"The session's next turn continues after it, and the model is told that the person stopped it. " +
			"A second stop while it is stopping gets 202 again and sends nothing more. Takes the main token only. No body is read.",
		replies: []reply{ok(202, Stopping{}, "The turn is stopping."),
			fail(404, "Starting turns is turned off in this service, or no event carries this turn id."),
			fail(409, "The turn is not running: it has ended, or this service did not start it."),
			storeFailed},
		serve: (*API).stopTurn},
	{method: "GET", path: "/api/v0/agents", op: "listAgents",
		tag:     "Service",
		summary: "List agents",
		doc:     "Which agents this service can start. Kept for one minute.",
		replies: []reply{ok(200, AgentList{}, "The agents."), badRequest},
		serve:   plain((*API).listAgents)},
	{method: "GET", path: "/api/v0/calls/{id}/context", op: "callContext",
		idDoc:   "A model call: the exchange field of its events. Another shape gets 404.",
		tag:     "Calls",
		summary: "Read a call's request",
		doc:     "Everything one model call's request carried, piece by piece, with sizes.\n\n" + callContextDoc,
		query:   []param{{name: "full", doc: "false leaves out each piece's json, for sizes and labels only. true when absent."}},
		replies: []reply{ok(200, store.CallContext{}, "The call's request."), badRequest,
			fail(404, "No request body is stored for this call."), storeFailed},
		turn:  ownCall,
		serve: (*API).callContext},
	{method: "GET", path: "/api/v0/calls/{id}/pieces/{n}", op: "callPiece",
		idDoc:   "A model call: the exchange field of its events. Another shape gets 404.",
		tag:     "Calls",
		summary: "Read a call's piece",
		doc:     "One piece of a model call's request, or its reply, byte for byte.\n\n" + callPieceDoc,
		replies: []reply{{status: 200, doc: "The piece's bytes, exactly as stored.", raw: true},
			fail(404, "No request body is stored for this call, it has no piece n, or no reply is kept."), storeFailed},
		turn:  ownCall,
		serve: (*API).callPiece},
	{method: "GET", path: "/api/v0/openapi.json", op: "openapi",
		tag:     "Service",
		summary: "Get this description",
		doc:     "This description.",
		replies: []reply{ok(200, map[string]any{}, "This document.")},
		public:  true},
}

// match reports whether p is rt's path, and the id in it.
func (rt *route) match(p string) (id string, ok bool) {
	want, got := strings.Split(rt.path, "/"), strings.Split(p, "/")
	if len(want) != len(got) {
		return "", false
	}
	for i, w := range want {
		switch {
		case w == "{id}":
			if !sessionID.MatchString(got[i]) {
				return "", false
			}
			id = got[i]
		case w == "{n}":
			if !pieceNumber.MatchString(got[i]) {
				return "", false
			}
		case w != got[i]:
			return "", false
		}
	}
	return id, true
}

// route serves a request that has passed the Host, CORS and token checks. g is
// the grant of a turn token, nil for the main token.
func (a *API) route(w http.ResponseWriter, r *http.Request, g *grant) {
	var allow []string
	for i := range routes {
		rt := &routes[i]
		if rt.public {
			continue
		}
		id, ok := rt.match(r.URL.Path)
		if !ok {
			continue
		}
		if rt.method == r.Method {
			if g != nil && !a.allowTurn(w, r, rt, id, *g) {
				return
			}
			rt.serve(a, w, r, id)
			return
		}
		allow = append(allow, rt.method)
	}
	if allow != nil {
		notAllowed(w, strings.Join(allow, ", "))
		return
	}
	writeError(w, http.StatusNotFound, "no such API path")
}
