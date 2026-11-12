package api

// The route table. The router dispatches from it and the OpenAPI description
// is built from it (spec.go), so a route cannot be served undocumented.
//
// The table is reached only after ServeHTTP's Host check, CORS and token
// check, which stay where they are. A public row is answered by public, before
// the token check; the router never serves one.

import (
	"net/http"
	"slices"
	"strings"

	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// route is one operation: a method on a path.
type route struct {
	method string
	// path may hold {id}: a session, turn, agent or runner id, and {call}: a
	// model call, both matched by sessionID; {n}: a piece. Any other shape
	// gets 404. The handler reads {call} and {n} with r.PathValue.
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
	turn int
	// turns says the turns token (Config.TurnsToken) may call the route. The
	// zero value refuses it.
	turns bool
	// self routes check their own token: the runner protocol, which takes
	// the enrollment token or a runner's own token (package jobs). ServeHTTP
	// hands them to Config.Jobs before its own token check.
	self bool
	// versioned routes send the setting's version in ETag, and a write takes
	// it back in If-Match (settings.go).
	versioned bool
	// tools are the tools this route is, for POST /api/v0/mcp (tools.go).
	// None: it is not a tool.
	tools []tool
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
	empty   bool   // no body, such as a 204
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
		"and nothing is stored. GET /api/v0/harnesses shows which harnesses the service finds.")
	turnFailed = fail(http.StatusInternalServerError, "The store could not be written, or the agent's program would not start.")
)

// streamDoc is the one description of the event stream's framing.
const streamDoc = "Server-sent events. Every stored event after the cursor, in order, then new ones as they land. " +
	"Without store and after it starts with new events only. Each message is `id: <store>/<seq>`, " +
	"`event: <kind>`, `data: <one Event as JSON on one line>`, then a blank line. " +
	"A `: ping` comment comes every 15 seconds. To resume, reconnect with the last store and seq you saw."

// tags are the sections of the docs, in the order they are read: a new
// user starts a turn first. Every route names one. A runner tag is in the
// runner protocol's description, never the client API's.
var tags = []struct {
	name, doc string
	runner    bool
}{
	{name: "Turns", doc: "A turn is one message to an agent, its reply and its changes. Every action is on a turn: start it, stop it, read its diff, apply its changes to the person's folder, ask another agent to review it, and read its model calls."},
	{name: "Sessions", doc: "A session is a conversation: its turns, in order. Read its history, search it, name it or close it."},
	{name: "Agents", doc: "Saved agents: a harness, how it reaches its model, the model and a name. A turn names one. And the harnesses, runners and folders a turn can use."},
	{name: "Settings", doc: "The policy and the instructions every turn gets, and the subscriptions control holds for runners. The main token only."},
	{name: "Events", doc: "The record as a live stream, for a cockpit, an export or a program, and a program's own events. The main token only."},
	{name: "Service", doc: "Which build this is, and this description."},
	{name: "Runners", doc: "Register a runner, and take work.", runner: true},
	{name: "Attempts", doc: "Report an attempt: its start, lease, events, record, model calls and end.", runner: true},
	{name: "Blobs", doc: "Files by their SHA-256: a snapshot's, and a turn's changes. A runner's token.", runner: true},
}

const (
	turnIDDoc = "A turn id, as POST /api/v0/turns named it. Another shape gets 404."
	// turnSessionQuery is the session query of a route on one turn.
	turnSessionDoc = "Only this session's turn of that id. The same turn id can be in two sessions; without session the newest is used."
)

var routes = slices.Concat([]route{
	{method: "POST", path: "/api/v0/turns", op: "startTurn",
		tag:     "Turns",
		summary: "Start a turn",
		doc: "Start a turn: in a new session, or with session the next turn of that one. The reply names the turn at once; every event of the turn carries its id.\n\n" +
			"Without session, agent, folder and prompt are required and the turn opens a new session. " +
			"With session, prompt is required and the turn continues that session in its own agent and folder; " +
			"an agent or folder sent that differs from the session's is refused with 400. " +
			"The turn's turn.started, with the prompt, is stored before the reply.\n\n" +
			"A session runs on one machine, the one that holds its folder, for its whole life: pick the machine (connector, from GET /api/v0/runners), " +
			"then type a folder on it (folder) or name one it holds (folder_ref, from GET /api/v0/folders). Only that machine's runner takes the session's work.\n\n" +
			"The reply waits up to 30 seconds for a runner to start the turn. A turn no runner starts by then answers 202 with state queued: " +
			"it runs when a runner is free, and GET /api/v0/turns/{id} follows it.\n\n" +
			"A session whose turn runs, or has turns waiting, is never refused: the new turn queues behind them and runs next, in order. " +
			"It answers 202 at once with state queued and ahead, how many of the session's turns are before it.",
		body: TurnRequest{},
		turn: askTurn,
		tools: []tool{
			{name: "ask_agent", effect: effectStarts, need: "start",
				// No folder: the new session is always on the asking
				// session's folder. An agent on a runner cannot know the
				// person's folder path, and its own copy's path is refused
				// (his runner, 2026-11-11: it sent its working folder, got
				// 403, tried mkdir, then left folder out).
				fields:   []string{"agent", "prompt", "model", "read_only", "idempotency_key"},
				required: []string{"agent", "prompt"},
				doc: "Ask a saved AgentBus agent (from list_agents, any vendor) to do work. " +
					"It starts a new session on that agent, in its own copy of your folder. " +
					"The reply names the new turn and session at once. Its result comes back to your session as a later turn: do not wait or poll for it. " +
					"This is not your harness's own subagent: it is recorded, and can be another vendor."},
			{name: "continue_session", effect: effectStarts, need: "continue",
				fields:   []string{"session", "prompt", "agent", "model", "idempotency_key"},
				required: []string{"session", "prompt"},
				doc: "Send the next message to a session you started with ask_agent. " +
					"Never your own session or the person's. The result comes back to your session as a later turn."}},
		replies: []reply{ok(202, Started{}, "The turn: running, or queued behind the session's earlier turns or for a runner."), turnRefused,
			fail(403, "A turn token's request is refused: error says why, such as a cap on asked work."),
			fail(404, "This service keeps no queue, or the session sent has no events."),
			fail(409, "The session sent cannot be continued from here, it is closed, or the idempotency key names another turn. A session whose turn is running is not refused: the new turn waits behind it (queued)."),
			fail(http.StatusTooManyRequests, "Too many turns are waiting; wait for some to end."), turnFailed, stopping},
		turns: true,
		serve: plain((*API).run)},
	{method: "GET", path: "/api/v0/turns", op: "listTurns",
		tag:     "Turns",
		summary: "List turns",
		doc: "The turns, newest first, and the seq they were computed at.\n\n" +
			"The first page starts with the turns still queued, then the turns that started, newest first. " +
			"A turn that ended before it started, such as one stopped while queued, is listed among them where it was asked for, on one page only. " +
			"Page older turns with before set to the next_before of the page before. Stream from the list's store and seq to follow new events.",
		query: []param{
			{name: "session", doc: "Only this session's turns."},
			{name: "before", doc: "Only turns whose started_seq is below it: the next_before of the previous page.", integer: true, min: 1},
			{name: "limit", doc: "Turns per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies: []reply{ok(200, TurnList{}, "A page of turns."), badRequest, storeFailed},
		turn:    treeSessionQuery,
		tools: []tool{{name: "list_turns", effect: effectNone,
			doc: "List the turns of your AgentBus session, newest first: each turn's id, number (turn 3 is number 3), state, agent, cost and whether it is applied. " +
				"With session, another session you can reach. Use a turn's id in the other agentbus tools."}},
		turns: true,
		serve: plain((*API).turns)},
	{method: "GET", path: "/api/v0/turns/{id}", op: "getTurn",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Get a turn",
		doc: "One turn, from the moment it is asked for: its state (queued, running, finished, failed or stopped), its agent, prompt and reply, its cost, and for a review its verdict and findings.\n\n" +
			"Poll it to wait for a turn to end, or stream from its store and seq.",
		query:   []param{{name: "session", doc: turnSessionDoc}},
		replies: []reply{ok(200, OneTurn{}, "The turn."), badRequest, fail(404, "No such turn."), storeFailed},
		turn:    treeTurn,
		tools: []tool{{name: "get_turn", effect: effectNone, id: "turn",
			doc: "Read one AgentBus turn: its state (queued, running, finished, failed or stopped), agent, prompt, reply and cost, and for a review its verdict and findings."}},
		turns: true,
		serve: (*API).getTurn},
	{method: "POST", path: "/api/v0/turns/{id}/stop", op: "stopTurn",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Stop a turn",
		doc: "Stop a queued or running turn. A queued turn never starts. A running turn's agent and every process it started end, as when the service stops.\n\n" +
			"Its runner hears the stop within 2 seconds, sends SIGTERM to the turn's whole process tree, then SIGKILL after 5 seconds to what still runs. " +
			"The reply comes at once; the turn reads stopped once the tree has ended. " +
			"The session's next turn continues after it, and the model is told that the person stopped it. " +
			"A second stop while it is stopping gets 202 again and sends nothing more. No body is read.",
		replies: []reply{ok(202, Stopping{}, "The turn is stopping."),
			fail(404, "This service keeps no queue, or no turn has this id."),
			fail(409, "The turn has ended, or it is not a turn of this service."),
			storeFailed},
		turn: askedTurn,
		tools: []tool{{name: "stop_turn", effect: effectStops, id: "turn",
			doc: "Stop a queued or running AgentBus turn that work from your session asked for. A queued turn never starts; a running one and every process it started end. " +
				"The answer comes at once; get_turn then reads stopped."}},
		turns: true,
		serve: (*API).stopTurn},
	{method: "GET", path: "/api/v0/turns/{id}/diff", op: "turnDiff",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Get a turn's diff",
		doc: "Everything the record holds about what one turn did, in one answer: the files it changed, its model calls, its tool calls and the commands it ran, its agent and its timing, and whether its changes were applied.\n\n" +
			"files: each changed file with its change (added, modified or deleted), size and SHA-256, and, in an isolated copy, its own unified diff for small text files. " +
			"A file with no diff whose content changed says diff_left_out. files is null while the turn runs, and for a turn this service did not run. " +
			"Commands are what the gateway's guard reads from the agent's shell tool calls; network calls and processes are not recorded yet.",
		query:   []param{{name: "session", doc: turnSessionDoc}},
		replies: []reply{ok(200, TurnDiff{}, "The turn's diff."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:    treeTurn,
		tools: []tool{{name: "turn_diff", effect: effectNone, id: "turn",
			doc: "What one AgentBus turn did: the files it changed, each with its diff, its commands, model calls, cost and timing, and whether its changes were applied to the person's folder."}},
		turns: true,
		serve: (*API).turnDiff},
	{method: "POST", path: "/api/v0/turns/{id}/apply", op: "applyTurn",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Apply a turn's changes",
		doc: "Put a turn's file changes into the person's folder: the folder its session's copy was made from. A turn works in its own copy, so its changes reach the person's folder only by an apply. No body is read.\n\n" +
			"The apply writes the session's changes up to and including this turn: each file added or changed, and removes each one deleted. " +
			"Before it writes anything it checks every file in the folder: one that is neither as it was at the turn's start nor already as the turn left it refuses the whole apply, " +
			"with 409, state refused, and each file in conflicts with why. Nothing is written then, so the agent can read why and make another turn. " +
			"Applying the same turn again is a no-op: 200 with already true and the files the first apply wrote. " +
			"An apply that was refused may be asked for again. " +
			"It runs on the runner of the folder's machine, in a place of its own, so it does not wait behind running turns. " +
			"The reply waits up to 30 seconds; past that it is 202 with state queued or running, and GET /api/v0/turns/{id}/diff shows how it ended.",
		replies: []reply{ok(200, ApplyState{}, "Applied: files lists what was written. already true: applied before, and nothing more was written."),
			ok(202, ApplyState{}, "The apply has not finished yet."),
			badRequest,
			fail(404, "This service keeps no queue, or no turn of this service has this id."),
			ok(409, ApplyState{}, "Refused or failed: error says why, and conflicts names each file in your folder that changed. Nothing was written on a conflict."),
			tooMany, fail(503, "The runner of the folder's machine has not been seen for a minute. Nothing was written and no apply was made."), storeFailed},
		turn: applyTurnTok,
		tools: []tool{{name: "apply_turn", effect: effectApply, id: "turn",
			doc: "Apply an AgentBus turn's file changes to the person's folder. Your edits stay in your session's own copy until a turn is applied. " +
				"It writes the session's changes up to and including this turn, earlier unapplied turns too. " +
				"A file the person changed since refuses the whole apply, and nothing is written. Applying a turn again changes nothing."}},
		serve: (*API).applyTurn},
	{method: "POST", path: "/api/v0/turns/{id}/review", op: "reviewTurn",
		idDoc:   "The id of the turn to review. Another shape gets 404.",
		tag:     "Turns",
		summary: "Review a turn",
		doc: "Ask another agent to review a turn: this starts a review turn, by agent, read-only, in a session of its own, in the reviewed turn's copy as it is.\n\n" +
			"The review's prompt holds the reviewed turn's task, its changed files and its diff; the author's reply only with include_reply. " +
			"When the review ends, its verdict (pass, fail or unknown) and findings are on it: GET /api/v0/turns/{id} of the review turn. Any P1 finding makes it fail. " +
			"The reviewed turn must have ended, with its changed files recorded. " +
			"A retry with the same idempotency_key and fields answers with the same review turn.",
		body: ReviewRequest{},
		replies: []reply{ok(202, Started{}, "The review turn, queued."),
			badRequest, fail(403, "A turn token's request is refused: error says why, such as a cap on asked work."),
			fail(404, "This service keeps no queue, or no turn of this service has this id."),
			fail(409, "The turn cannot be reviewed: it has not ended, it recorded no changed files, its session is closed, or the key names another turn."),
			fail(http.StatusTooManyRequests, "Too many turns are waiting; wait for some to end."), storeFailed},
		turn: askReview,
		tools: []tool{{name: "review_turn", effect: effectStarts, id: "turn",
			doc: "Ask a saved AgentBus agent (from list_agents) to review a turn that has ended. Pick an agent other than the one that did the turn. " +
				"It starts a read-only review turn; its verdict and findings come back to your session as a later turn, and get_turn on the review turn reads them."}},
		turns: true,
		serve: (*API).reviewTurn},
	{method: "GET", path: "/api/v0/turns/{id}/calls/{call}", op: "callContext",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Read a model call",
		doc:     "Everything one model call of the turn sent, piece by piece, with sizes, and its reply.\n\n" + callContextDoc,
		query:   []param{{name: "full", doc: "false leaves out each piece's json, for sizes and labels only. true when absent."}},
		replies: []reply{ok(200, store.CallContext{}, "The call's request."), badRequest,
			fail(404, "The turn has no such call, or no request body is stored for it."), storeFailed},
		turn:  treeTurn,
		serve: callOfTurn((*API).callContext)},
	{method: "GET", path: "/api/v0/turns/{id}/calls/{call}/pieces/{n}", op: "callPiece",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Read a call's piece",
		doc:     "One piece of a model call's request, or its reply, byte for byte.\n\n" + callPieceDoc,
		replies: []reply{{status: 200, doc: "The piece's bytes, exactly as stored.", raw: true},
			fail(404, "The turn has no such call, no request body is stored for it, it has no piece n, or no reply is kept."), storeFailed},
		turn:  treeTurn,
		serve: callOfTurn((*API).callPiece)},

	{method: "GET", path: "/api/v0/sessions", op: "listSessions",
		tag:     "Sessions",
		summary: "List sessions",
		doc:     "The sessions, and the seq they were computed at.",
		replies: []reply{ok(200, Snapshot{}, "The snapshot."), storeFailed},
		serve:   plain((*API).sessions)},
	{method: "GET", path: "/api/v0/sessions/{id}", op: "getSession",
		tag:     "Sessions",
		summary: "Get a session",
		doc: "One session, as GET /api/v0/sessions lists it, and its copy: the runner that holds the session's isolated copy of its folder, " +
			"and whether it is closed and removed. A session that runs in the person's folder as it is has no copy.",
		replies: []reply{ok(200, OneSession{}, "The session."), noSession, storeFailed},
		turn:    treeSessionPath,
		serve:   (*API).getSession},
	{method: "PATCH", path: "/api/v0/sessions/{id}", op: "updateSession",
		tag:     "Sessions",
		summary: "Rename or close a session",
		doc: "Give a session a name, or close it. Send title, closed, or both.\n\n" +
			"title stores a session.renamed with the new name and the old one. GET /api/v0/sessions then names the session by its latest rename; without one, by its first message. " +
			"Earlier names stay in the record, as the session's events. A rename does not change the session's last activity.\n\n" +
			"closed true closes the session: it takes no more turns, and its runner removes the session's isolated copy and its home. " +
			"Apply what you want to keep first. The person's folder is never removed. A session with a turn that has not ended is refused with 409. Only the main token closes a session.",
		body: SessionUpdate{},
		replies: []reply{ok(200, Renamed{}, "Done."), badRequest,
			fail(403, "A turn token's request is refused: error says why."), noSession,
			fail(409, "A turn of the session has not ended, so it cannot be closed."), storeFailed},
		turn: renameSession,
		tools: []tool{{name: "rename_session", effect: effectRename, id: "session", ownSession: true,
			fields: []string{"title"}, required: []string{"title"},
			doc: "Give an AgentBus session a name, as the person's screen lists it: your own session when session is absent."}},
		serve: (*API).updateSession},
	{method: "GET", path: "/api/v0/sessions/{id}/events", op: "sessionEvents",
		tag:     "Sessions",
		summary: "Read a session's events",
		doc: "One session's stored events, oldest first, a page at a time. With turn, only that turn's events. " +
			"With kind and no turn, only the session's events of those kinds, such as a session's turn.started events.\n\n" +
			"Without turn or kind, the page also holds each events.dropped that names the session.",
		query: []param{qStore, qAfter,
			{name: "turn", doc: "Only this turn's events."},
			{name: "kind", doc: "Only events of these kinds, comma separated, such as turn.started,turn.completed. Ignored with turn."},
			{name: "limit", doc: "Events per page. 1000 when absent.", integer: true, min: 1, max: maxLimit}},
		replies: []reply{ok(200, Page{}, "A page."), badRequest, mismatch, storeFailed},
		turn:    treeSessionPath,
		turns:   true,
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
		turn:    treeSessionPath,
		tools: []tool{{name: "search_history", effect: effectNone, id: "session", ownSession: true, required: []string{"q"},
			doc: "Search an AgentBus session's stored history for a piece of text, newest first: your own session when session is absent. " +
				"It finds what earlier turns said and did, including turns you no longer hold in context."}},
		serve: (*API).search},

	{method: "GET", path: "/api/v0/agents", op: "listAgents",
		tag:     "Agents",
		summary: "List agents",
		doc: "The saved agents, in the order they were made. A turn names one by id in agent.\n\n" +
			"A new data folder starts with two: claude (Claude Code) and codex (Codex), each on its own subscription with its default model.",
		replies: []reply{ok(200, AgentList{}, "The agents."), badRequest},
		turn:    anyRead,
		tools: []tool{{name: "list_agents", effect: effectNone,
			doc: "List the saved AgentBus agents: setups across vendors (Claude Code, Codex, Gemini and others) that you can ask for work or a review. " +
				"These are saved setups, not conversations running now, and not your harness's own subagents. An agent's id goes in ask_agent or review_turn."}},
		serve: plain((*API).listSaved)},
	{method: "POST", path: "/api/v0/agents", op: "createAgent",
		tag:     "Agents",
		summary: "Save an agent",
		doc: "Save a new agent: a harness, how it reaches its model, the model and a name. It gets a new id.\n\n" +
			"access subscription uses the harness's own sign-in. access openrouter needs a model in OpenRouter's naming and reads its key, " +
			"when a turn starts, from the environment variable key_env names on the runner's machine (OPENROUTER_API_KEY when absent). " +
			"The key itself is never sent, stored or logged. access api and harness opencode are refused: not available yet.",
		body:    AgentRequest{},
		replies: []reply{ok(201, SavedAgent{}, "Saved."), badRequest, fail(404, "This service keeps no saved agents."), fail(409, "Too many agents."), storeFailed},
		serve:   plain((*API).createSaved)},
	{method: "GET", path: "/api/v0/agents/{id}", op: "getAgent",
		idDoc:   "A saved agent's id. Another shape gets 404.",
		tag:     "Agents",
		summary: "Get an agent",
		doc:     "One saved agent.",
		replies: []reply{ok(200, SavedAgent{}, "The agent."), badRequest, fail(404, "No such agent.")},
		turn:    anyRead,
		serve:   (*API).getSaved},
	{method: "PUT", path: "/api/v0/agents/{id}", op: "updateAgent",
		idDoc:   "A saved agent's id. Another shape gets 404.",
		tag:     "Agents",
		summary: "Edit an agent",
		doc: "Replace a saved agent's fields; its id stays. The same rules as saving one. " +
			"A session already started keeps the harness and access its turns recorded; its next turns use them, not the edit.",
		body:    AgentRequest{},
		replies: []reply{ok(200, SavedAgent{}, "Saved."), badRequest, fail(404, "No such agent."), storeFailed},
		serve:   (*API).updateSaved},
	{method: "DELETE", path: "/api/v0/agents/{id}", op: "removeAgent",
		idDoc:   "A saved agent's id. Another shape gets 404.",
		tag:     "Agents",
		summary: "Remove an agent",
		doc: "Remove a saved agent. Its sessions stay and can still be continued. " +
			"A turn that names claude or codex still starts once that agent is removed: that harness on its own subscription. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, badRequest, fail(404, "No such agent."), storeFailed},
		serve:   (*API).removeSaved},
	{method: "GET", path: "/api/v0/harnesses", op: "listHarnesses",
		tag:     "Agents",
		summary: "List harnesses",
		doc: "The harnesses an agent can run on: whether a runner finds each, its version, whether it is signed in, " +
			"and the access and models each offers, for a form that saves an agent. Each runner probes its own machine when it starts " +
			"and once a minute after, and reports it; this service never runs an agent's program to answer. " +
			"Just after a runner starts, the answer waits for its first report, up to 15 seconds.",
		replies: []reply{ok(200, HarnessList{}, "The harnesses."), badRequest},
		turn:    anyRead,
		serve:   plain((*API).listHarnesses)},
	{method: "GET", path: "/api/v0/runners", op: "listRunners",
		tag:     "Agents",
		summary: "List runners",
		doc: "The runners control knows, one per machine, for a person: each one's name, id, machine (connector), role, labels, whether it is online, " +
			"when it was last seen, its last probe of each harness with its version, and whether it can build a session's wall (sandbox). Online means it asked for work in the last minute. " +
			"pending lists the machines that ran agentbus login and wait for the person's approval. " +
			"A runner that stopped stays listed, offline, until removed. A connector of a build before 0.5 is listed only while no runner reports its machine. No token is in the answer. Takes the main token only. No body is read.",
		replies: []reply{ok(200, RunnerList{}, "The runners."), badRequest, storeFailed},
		serve:   plain((*API).listRunners)},
	{method: "POST", path: "/api/v0/runners/pending/{id}/approve", op: "approveMachine",
		idDoc:   "A waiting machine's id (lgn_ and 16 hex digits), from pending in GET /api/v0/runners. Another shape gets 404.",
		tag:     "Agents",
		summary: "Approve a machine",
		doc: "Let a machine that ran agentbus login become a runner of this control. Type the code the machine shows: the list never holds it, " +
			"so only the person in front of that machine can approve it. Until approved, a machine takes no job and its token works for nothing else. " +
			"Within 2 seconds the machine gets its runner credential and starts. Takes the main token only.",
		body: Approval{},
		replies: []reply{ok(200, RunnerInfo{}, "Approved: the runner it now is."), badRequest, fail(404, "No machine waits by that id."),
			fail(409, "It was approved or refused already, or waited longer than 15 minutes."), storeFailed},
		serve: (*API).approveMachine},
	{method: "POST", path: "/api/v0/runners/pending/{id}/refuse", op: "refuseMachine",
		idDoc:   "A waiting machine's id (lgn_ and 16 hex digits), from pending in GET /api/v0/runners. Another shape gets 404.",
		tag:     "Agents",
		summary: "Refuse a machine",
		doc:     "Refuse a machine that ran agentbus login: it never becomes a runner, and agentbus login on it says so. Takes the main token only. No body is read.",
		replies: []reply{{status: 204, doc: "Refused.", empty: true}, badRequest, fail(404, "No machine waits by that id."),
			fail(409, "It was approved or refused already, or waited longer than 15 minutes."), storeFailed},
		serve: (*API).refuseMachine},
	{method: "POST", path: "/api/v0/runners/{id}/remove", op: "removeRunner",
		idDoc:   "A runner's id, from GET /api/v0/runners. Another shape gets 404.",
		tag:     "Agents",
		summary: "Remove a runner",
		doc: "Remove a machine that is offline: its runner token works no more, so it never takes work again, and it leaves the list. " +
			"Its sessions, jobs and their events stay in the record. A runner.removed event is stored. " +
			"An online runner is refused: stop it on its machine first. To add the machine again, run agentbus login on it. Takes the main token only. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, badRequest, fail(404, "No runner by that id."),
			fail(409, "It is online."), storeFailed},
		serve: (*API).removeRunner},
	{method: "GET", path: "/api/v0/folders", op: "listFolders",
		tag:     "Agents",
		summary: "List folders",
		doc: "The folders the runners hold, each by its reference: a turn names one as folder_ref, with its machine as connector, and runs on that machine. " +
			"A folder's path stays on the machine that owns it; the answer has only the last part of it. " +
			"In agentbus up every folder a turn was started in is listed, newest first, within a minute of its first turn. " +
			"A session's copy is never listed, though a turn may run in one. No body is read.",
		replies: []reply{ok(200, FolderList{}, "The folders."), badRequest},
		serve:   plain((*API).listFolders)},

	{method: "GET", path: "/api/v0/events", op: "streamEvents",
		tag:     "Events",
		summary: "Stream events",
		doc:     "A live stream of the record's events: every session's, or one session's. A cockpit follows it; an export reads it from the start.\n\n" + streamDoc,
		query: []param{qStore, qAfter,
			{name: "session", doc: "Only this session's events, plus the drops that name it."}},
		replies: []reply{streamReply("The stream.", eventsExample), badRequest, mismatch, tooMany,
			fail(500, "This connection cannot stream.")},
		serve: plain((*API).stream)},
	{method: "POST", path: "/api/v0/events", op: "writeEvent",
		tag:     "Events",
		summary: "Write a program event",
		doc:     "A program writes its own event, of a kind x.<program>.<name>, into a session's record.",
		body:    EventRequest{},
		replies: []reply{ok(201, Written{}, "Stored."), badRequest, noSession, storeFailed},
		serve:   plain((*API).writeEvent)},

	{method: "POST", path: "/api/v0/mcp", op: "mcp",
		tag:     "Service",
		summary: "Call the API as tools",
		doc: "The operations an agent uses, as tools, over the Model Context Protocol: stateless Streamable HTTP, one JSON-RPC message per POST, answered as JSON. " +
			"A turn's harness is pointed here with the turn's own token, so its agent sees list_agents, ask_agent, review_turn and the rest as tools of its own.\n\n" +
			"tools/list answers the tools the token may call: a turn token's under [agent_api] in permissions.toml, and a read-only turn's reads only. " +
			"The list is discovery only. tools/call sends the operation it names through this API with the same token, so it is refused, limited and answered exactly as that HTTP call is. " +
			"A tool's answer is the operation's JSON; one that failed is an error result that starts with its HTTP status. " +
			"Each operation's tools are in its x-agentbus-tools. No session is kept: a token that has stopped working gets 401.",
		body:    map[string]any{},
		replies: []reply{{status: 200, doc: "The JSON-RPC answer.", raw: true}, {status: 202, doc: "A notification or a response, taken.", empty: true}, badRequest},
		turn:    toolList,
		turns:   true,
		serve:   plain((*API).mcp)},
	{method: "GET", path: "/api/v0/version", op: "getVersion",
		tag:     "Service",
		summary: "Get the version",
		doc: "Which build of AgentBus this service is: the values agentbus version prints. " +
			"Takes the main token only. No body is read.",
		replies: []reply{ok(200, Version{}, "The version."), badRequest},
		serve:   plain((*API).version)},
	{method: "GET", path: "/api/v0/openapi.json", op: "openapi",
		tag:     "Service",
		summary: "Get this description",
		doc:     "This description. The runner protocol's is at /api/v0/runner/openapi.json.",
		replies: []reply{ok(200, map[string]any{}, "This document.")},
		public:  true},
}, runnerRoutes, settingsRoutes, subscriptionRoutes)

// match reports whether p is rt's path, the id in it, and the values of its
// other parameters, by name.
func (rt *route) match(p string) (id string, vals map[string]string, ok bool) {
	want, got := strings.Split(rt.path, "/"), strings.Split(p, "/")
	if len(want) != len(got) {
		return "", nil, false
	}
	for i, w := range want {
		switch {
		case w == "{id}":
			if !sessionID.MatchString(got[i]) {
				return "", nil, false
			}
			id = got[i]
		case w == "{call}" || w == "{n}":
			if (w == "{call}" && !sessionID.MatchString(got[i])) || (w == "{n}" && !pieceNumber.MatchString(got[i])) {
				return "", nil, false
			}
			if vals == nil {
				vals = map[string]string{}
			}
			vals[strings.Trim(w, "{}")] = got[i]
		case w != got[i]:
			return "", nil, false
		}
	}
	return id, vals, true
}

// withValues hands a route's parameters to its handler, as r.PathValue.
func withValues(r *http.Request, vals map[string]string) {
	for k, v := range vals {
		r.SetPathValue(k, v)
	}
}

// turnsRefused is the 403 for the turns token on a route it may not call.
const turnsRefused = "the turns token may only start, stop, review and read turns; this route takes the main token"

// route serves a request that has passed the Host, CORS and token checks. g is
// the grant of a turn token, nil for the main token and the turns token;
// turnsOnly is true for the turns token.
func (a *API) route(w http.ResponseWriter, r *http.Request, g *run.Grant, turnsOnly bool) {
	var allow []string
	for i := range routes {
		rt := &routes[i]
		if rt.public || rt.self {
			continue
		}
		id, vals, ok := rt.match(r.URL.Path)
		if !ok {
			continue
		}
		if rt.method == r.Method {
			withValues(r, vals)
			if g != nil {
				if r, ok = a.allowTurn(w, r, rt, id, *g); !ok {
					return
				}
			}
			if turnsOnly && !rt.turns {
				writeError(w, http.StatusForbidden, turnsRefused)
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

// self serves a request on a route that checks its own token (route.self),
// and says whether it did.
func (a *API) self(w http.ResponseWriter, r *http.Request) bool {
	for i := range routes {
		rt := &routes[i]
		if !rt.self || rt.method != r.Method {
			continue
		}
		if id, vals, ok := rt.match(r.URL.Path); ok {
			withValues(r, vals)
			rt.serve(a, w, r, id)
			return true
		}
	}
	return false
}

// Methods is every HTTP method the client API's routes take, in the order
// the route table first names them. agentbus-api accepts exactly these.
func Methods() []string {
	var out []string
	for i := range routes {
		if !slices.Contains(out, routes[i].method) {
			out = append(out, routes[i].method)
		}
	}
	return out
}
