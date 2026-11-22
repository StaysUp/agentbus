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

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/toolreg"
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
	// tools are the default tools this route ships, for POST /api/v0/mcp
	// (tools.go). None: the route ships no default tool.
	tools []tool
	// wrappable marks a route a person may build a CUSTOM tool on, though it
	// ships no default tool of its own. The read routes set it: read-only turns
	// and the default read-tool copies retired in 0.6 (B12, reading is done
	// through agentbus-api), but a route safe to serve as a tool stays open to a
	// custom tool. A route that ships a default tool is wrappable regardless.
	wrappable bool
	serve     func(a *API, w http.ResponseWriter, r *http.Request, id string)
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

	// idempotentConflictReply is the 409 every store-backed create POST can
	// give when an idempotency_key is reused with other fields.
	idempotentConflictReply = fail(http.StatusConflict, "idempotency_key already made this with other fields; send a new key.")
)

// createReplies are the responses every store-backed create POST can give: its
// success, plus the ones the shared idempotency guard returns (400, 409, 500).
// A route that resolves a typed folder adds its own (404, 429, 503) as extra.
// Deriving them from one place keeps a returnable status from going
// undocumented (his rule, 2026-11-21: one shared pipeline, not per-handler).
func createReplies(success reply, extra ...reply) []reply {
	return slices.Concat([]reply{success, badRequest, idempotentConflictReply, storeFailed}, extra)
}

// startTurn's 400 and 500, saying what each means for a turn.
var (
	turnRefused = fail(http.StatusBadRequest, "The request is refused; error says why. "+
		"This includes an agent whose program is not on the service's PATH: error names the agent and how to install it, "+
		"and nothing is stored. GET /api/v0/harnesses shows which harnesses the service finds.")
	turnFailed = fail(http.StatusInternalServerError, "The store could not be written, or the agent's program would not start.")
)

// elsewhere is the file routes' 409: they act on control's disk only.
var elsewhere = fail(http.StatusConflict, "The session's folder is on a machine other than control's: these routes act on control's disk only.")

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
	{name: "Sessions", doc: "A session is a conversation: its turns, in order. Read its history, search it, name it or close it. Create one in a workspace, then run its turns."},
	{name: "Workspaces", doc: "A workspace is where turns run: it owns the folder, the machine and the sandbox mode (a copy behind a wall, or the folder as it is). Create one, then create a session in it, then run the session's turns. The folder and the mode are the workspace's; a session carries neither."},
	{name: "Agents", doc: "Saved agents: a harness, how it reaches its model, the model and a name. A turn names one. And the harnesses, runners and folders a turn can use."},
	{name: "Tools", doc: "The tools control serves to agents: the built-in ones and the person's own, in one stored format. List, add and remove them. The main token only."},
	{name: "Files", doc: "Access to files and folders outside the session copy, gated on control."},
	{name: "Reactions", doc: "An on is a standing instruction: when an event happens (for now, a turn completes), do something (for now, deliver that turn's reply to a session). Register one and remove it. A row on control; nothing waits. ask_agent registers one for itself."},
	{name: "Settings", doc: "The policy and the instructions every turn gets, and the subscriptions control holds for runners. The main token only."},
	{name: "Approvals", doc: "The commands the gateway held for a person (an ask entry, or a command on none of the lists under the ask default): list them and answer one, once, always or refuse. The main token only."},
	{name: "Events", doc: "The record as a live stream, for a cockpit, an export or a program, and a program's own events. The main token only."},
	{name: "Service", doc: "Which build this is, and this description."},
	{name: "Runners", doc: "Register a runner, and take work.", runner: true},
	{name: "Attempts", doc: "Report an attempt: its start, lease, events, record, model calls and end.", runner: true},
	{name: "Blobs", doc: "Files by their SHA-256: a snapshot's, and a turn's changes. A runner's token.", runner: true},
}

const (
	turnIDDoc = "A turn id (such as t_cf58baf9ca42cb1d), as POST /api/v0/turns named it, never the turn's number in its session. Another shape gets 404."
	// turnSessionQuery is the session query of a route on one turn.
	turnSessionDoc = "Only this session's turn of that id. The same turn id can be in two sessions; without session the newest is used."
)

var routes = slices.Concat([]route{
	{method: "POST", path: "/api/v0/turns", op: "startTurn",
		tag:     "Turns",
		summary: "Start a turn",
		doc: "Start a turn: in a new session, or with session the next turn of that one. The reply names the turn at once; every event of the turn carries its id.\n\n" +
			"Without session, agent, folder and prompt are required and the turn opens a new session. " +
			"With session, agent and prompt are required and the turn continues that session, in its folder, on the agent named; " +
			"a folder sent that differs from the session's is refused with 400. Only the main token continues a session: a turn token is refused with 403. " +
			"The turn's turn.started, with the prompt, is stored before the reply.\n\n" +
			"A session runs on one machine, the one that holds its folder, for its whole life: pick the machine (its machine id, from GET /api/v0/runners), " +
			"then type a folder on it (folder) or name one it holds (folder_ref, from GET /api/v0/folders). Only that machine's runner takes the session's work.\n\n" +
			"The reply waits up to 30 seconds for a runner to start the turn. A turn no runner starts by then answers 202 with state queued: " +
			"it runs when a runner is free, and GET /api/v0/turns/{id} follows it.\n\n" +
			"A session whose turn runs, or has turns waiting, is never refused: the new turn queues behind them and runs next, in order. " +
			"It answers 202 at once with state queued and ahead, how many of the session's turns are before it.",
		body:  TurnRequest{},
		query: []param{{name: "wait", doc: "false returns once the turn is recorded (state queued), without waiting for a runner to start it; the reply still names the turn. Absent or true waits up to 30 seconds, as above. The ask_agent and review_turn apps send false, so a turn and the on for its reply are recorded in one call that never blocks."}},
		turn:  askTurn,
		// B15: ask_agent is no longer a tool on this route; it is a default app
		// (apps.go, askAgentApp) that calls this operation (startTurn) in ONE step,
		// with delivery set so the reply's on is recorded in the same write. The
		// route stays — the app and an agent's own agentbus-api both call it — but
		// ships no tool of its own. "A turn is a turn." It stays wrappable, so a
		// person may still build a custom tool (or app step) on it.
		wrappable: true,
		replies: []reply{ok(202, Started{}, "The turn: running, or queued behind the session's earlier turns or for a runner."), turnRefused,
			fail(403, "A turn token's request is refused: error says why, such as a cap on asked work."),
			fail(404, "This service keeps no queue, or the session sent has no events."),
			fail(409, "The session sent cannot be continued from here, it is closed, its machine was removed, or the idempotency key names another turn. A session whose turn is running is not refused: the new turn waits behind it (queued)."),
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
		replies:   []reply{ok(200, TurnList{}, "A page of turns."), badRequest, storeFailed},
		turn:      treeSessionQuery,
		turns:     true,
		wrappable: true,
		serve:     plain((*API).turns)},
	{method: "GET", path: "/api/v0/turns/{id}", op: "getTurn",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Get a turn",
		doc: "One turn, from the moment it is asked for: its state (queued, running, finished, failed or stopped), its agent, prompt and reply, its cost, and for a review its verdict and findings.\n\n" +
			"Poll it to wait for a turn to end, or stream from its store and seq.",
		query:     []param{{name: "session", doc: turnSessionDoc}},
		replies:   []reply{ok(200, OneTurn{}, "The turn."), badRequest, fail(404, "No such turn."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).getTurn},
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
		turn:  askedTurn,
		turns: true,
		serve: (*API).stopTurn},
	{method: "POST", path: "/api/v0/ons", op: "registerOn",
		tag:     "Reactions",
		summary: "Register an on",
		doc: "Register a standing instruction: when the turn you name completes, its reply is delivered to a session as that session's next turn.\n\n" +
			"This is the one primitive every reaction is built on; for now the only event is a turn completing. " +
			"A turn token may name only a turn its own turn started, and the reply goes to its own session, so it leaves session out. " +
			"The main token may watch any turn and names the session to deliver to. " +
			"It is a row on control plus control's own check: nothing waits, and the reply arrives as a later turn. " +
			"Dedup is by idempotency_key only: the same key for the same turn and session returns the on already registered, the same key with other fields is 409, and a request with no key always registers a new on (several ons may watch one turn). ask_agent registers one of these for itself.",
		body: OnRequest{},
		replies: []reply{ok(201, OnView{}, "The on."), turnRefused,
			fail(403, "A turn token named a turn its tree did not start, or a session not its own."),
			fail(404, "This service keeps no queue, or no turn has this id."),
			fail(409, "The idempotency_key was already used to register an on for a different turn or session.")},
		turn:  onRegister,
		serve: plain((*API).registerOn)},
	{method: "DELETE", path: "/api/v0/ons/{id}", op: "removeOn",
		idDoc:   "An on's id (on_...), as POST /api/v0/ons named it.",
		tag:     "Reactions",
		summary: "Remove an on",
		doc:     "Remove an on: it stops reacting. Removing one already removed, or one that has fired, is accepted. A turn token may remove only an on its own turn registered. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, turnRefused,
			fail(403, "A turn token tried to remove an on another turn registered."),
			fail(404, "This service keeps no queue, or no on has this id.")},
		turn:  onPath,
		serve: (*API).removeOn},
	{method: "GET", path: "/api/v0/turns/{id}/diff", op: "turnDiff",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Get a turn's diff summary",
		doc: "What one turn did, in summary: its agent and timing, how many files it changed and its model and tool call counts (with usage, cost and per-model counts), and whether its changes were applied. Each list is on its own paged route, so none is cut here.\n\n" +
			"files is the summary of the changed files (isolated, the checkpoints, the count); the files themselves page at GET /api/v0/turns/{id}/changes. model_calls summarises the model calls (count, usage, cost, per-model); they page at GET /api/v0/turns/{id}/calls. tools is the tool call count; they page at GET /api/v0/turns/{id}/tools, each with the commands it ran. files is null while the turn runs, and for a turn this service did not run.",
		query:     []param{{name: "session", doc: turnSessionDoc}},
		replies:   []reply{ok(200, TurnDiff{}, "The turn's diff summary."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).turnDiff},
	{method: "GET", path: "/api/v0/turns/{id}/changes", op: "turnChanges",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "List a turn's file changes",
		doc: "A page of the files the turn added, modified or deleted, sorted by path: the list the diff's files summary counts. Each file has its change, size and SHA-256, and, in an isolated copy, its own unified diff for small text files; a file with no diff whose content changed says diff_left_out.\n\n" +
			"changes is empty while the turn runs, and for a turn this service did not run. Page the earlier files (by path) with before set to the previous page's next_before.",
		query: []param{
			{name: "session", doc: turnSessionDoc},
			{name: "before", doc: "Only files before this position: the next_before of the previous page.", integer: true, min: 1},
			{name: "limit", doc: "Files per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies:   []reply{ok(200, TurnChangesPage{}, "A page of the turn's file changes."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).turnChanges},
	{method: "GET", path: "/api/v0/turns/{id}/calls", op: "turnCalls",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "List a turn's model calls",
		doc: "A page of the turn's model calls, oldest first: the list the diff's model_calls summary counts. Each call has its exchange (GET /api/v0/turns/{id}/calls/{exchange} shows what it sent), model, status, usage, cost and latency.\n\n" +
			"Page older calls with before set to the next_before of the page before.",
		query: []param{
			{name: "session", doc: turnSessionDoc},
			{name: "before", doc: "Only calls before this position: the next_before of the previous page.", integer: true, min: 1},
			{name: "limit", doc: "Calls per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies:   []reply{ok(200, TurnCallsPage{}, "A page of the turn's model calls."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).turnCalls},
	{method: "GET", path: "/api/v0/turns/{id}/tools", op: "turnTools",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "List a turn's tool calls",
		doc: "A page of the turn's tool calls, oldest first: the list the diff's tools summary counts. Each tool call has its name, its input when small, whether it errored, the forbid rule that refused it, and the shell commands it ran.\n\n" +
			"Page older tool calls with before set to the next_before of the page before.",
		query: []param{
			{name: "session", doc: turnSessionDoc},
			{name: "before", doc: "Only tool calls before this position: the next_before of the previous page.", integer: true, min: 1},
			{name: "limit", doc: "Tool calls per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies:   []reply{ok(200, TurnToolsPage{}, "A page of the turn's tool calls."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).turnTools},
	{method: "GET", path: "/api/v0/turns/{id}/record", op: "turnRecord",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Read a turn's record",
		doc: "One turn's two halves in a single read, from the one event log: its machine half and its agent half, joined by the turn id.\n\n" +
			"The machine half leads, in review order: files is the changeset the turn made (a sandbox checkpoint; a direct-mode turn records commands only, so files is empty), without the per-file diffs that page at GET /api/v0/turns/{id}/changes; commands is each shell command the turn ran, as its tool calls reported it (a refused call is left out: it never ran), oldest first, with its output (the start inline up to a cap, the whole output on the tool call's tool.completed in the turn's events); network is each host it was refused when closed off. Then the agent half: model_calls, the turn's model calls in summary with their cost, paging at GET /api/v0/turns/{id}/calls.\n\n" +
			"The files are the changeset and each command's output is capped, so the read stays small; stream more from the store and seq. This reads the record, so it needs no runner and works after the turn's session is gone.",
		query:     []param{{name: "session", doc: turnSessionDoc}},
		replies:   []reply{ok(200, TurnRecord{}, "The turn's record."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).turnRecord},
	{method: "GET", path: "/api/v0/turns/{id}/context", op: "turnContext",
		idDoc:   turnIDDoc,
		tag:     "Turns",
		summary: "Read a turn's context",
		doc: "How big the turn's context got, how much of it came from the prompt cache, and what its largest prompt was made of. Nothing is stored: the turn's model.responded events and that call's stored request pieces are read now.\n\n" +
			"context_tokens is the largest prompt the turn sent, the most one of its calls read: input + cache_read + cache_write, the vendor's own count. cache_read_tokens and fresh_tokens (input + cache_write) are summed over the turn's calls: how much was re-read from the cache against processed afresh, which drives the cost. made_of breaks that largest prompt into its parts from the request's own pieces (system, tools, messages), largest first; its note says what a vendor's request does not separate, such as a file read inside a tool result. made_of is null when no call carried usage or no request body is kept.",
		query:     []param{{name: "session", doc: turnSessionDoc}},
		replies:   []reply{ok(200, TurnContext{}, "The turn's context."), badRequest, fail(404, "No such turn, or it has not started."), storeFailed},
		turn:      treeTurn,
		turns:     true,
		wrappable: true,
		serve:     (*API).turnContext},
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
		turn:  applyTurnTok,
		serve: (*API).applyTurn},
	// B15: the review route is removed. Review is a default app (review_turn,
	// apps.go) — ask_agent with a review prompt — so core holds no review route,
	// kind, verdict or recipe. A review is a normal turn.
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

	{method: "POST", path: "/api/v0/workspaces", op: "createWorkspace",
		tag:     "Workspaces",
		summary: "Create a workspace",
		doc: "Create a workspace: where turns run. Send folder (an absolute path) or folder_ref, optionally machine, and sandbox (true by default: turns run in a copy of the folder behind a wall; false: in the folder as it is). The reply names the workspace; create a session in it (POST /api/v0/sessions), then run the session's turns.\n\n" +
			"A typed folder's machine checks and registers it, and the workspace keeps only its reference. With idempotency_key a retry returns the same workspace and makes nothing new.",
		body:    WorkspaceRequest{},
		replies: createReplies(ok(201, store.Workspace{}, "The workspace."), fail(404, "This service keeps no queue, or no such machine."), tooMany, stopping),
		serve:   plain((*API).createWorkspace)},
	{method: "GET", path: "/api/v0/workspaces", op: "listWorkspaces",
		tag:     "Workspaces",
		summary: "List workspaces",
		doc:     "Every workspace, newest first, each with its folder, machine, sandbox mode and apply state.",
		replies: []reply{ok(200, WorkspacesPage{}, "The workspaces."), storeFailed},
		serve:   plain((*API).listWorkspaces)},
	{method: "GET", path: "/api/v0/workspaces/{id}", op: "getWorkspace",
		tag:     "Workspaces",
		summary: "Get a workspace",
		idDoc:   "A workspace id (ws_ and 16 hex digits), as POST /api/v0/workspaces named it. Another shape gets 404.",
		doc:     "One workspace: its folder, folder reference, machine, sandbox mode, when it was created, its head turn (the latest with a changeset), the turn its latest apply went up to, and when its latest action was a discard.",
		replies: []reply{ok(200, store.Workspace{}, "The workspace."), fail(404, "No such workspace."), storeFailed},
		serve:   (*API).getWorkspace},
	{method: "POST", path: "/api/v0/workspaces/{id}/discard", op: "discardWorkspace",
		tag:     "Workspaces",
		summary: "Discard a workspace's unapplied changes",
		idDoc:   "A workspace id (ws_ and 16 hex digits). Another shape gets 404.",
		doc: "Drop a sandbox workspace's unapplied changes: reset its copy to the last applied state, or to the folder as it was first copied when nothing was applied. The person's folder is never touched, and the conversation stays so the agent can try another way. No body is read.\n\n" +
			"One apply or discard runs per workspace, so a discard while an apply or discard of the workspace is still running or waiting is refused with 409. A running TURN does not refuse it: the discard waits behind the turn in the lane, and what it drops is worked out when it runs, so a turn that ran while it waited is dropped and marked too. The reply waits up to 30 seconds; past that it is 202 with state queued or running. Nothing unapplied answers 200 with already true.",
		replies: []reply{ok(200, DiscardState{}, "Discarded: dropped_turns lists the turns whose changes were dropped. already true: nothing was unapplied."),
			ok(202, DiscardState{}, "The discard has not finished yet (it waits behind a running turn)."),
			badRequest,
			fail(404, "This service keeps no queue, or no such workspace."),
			ok(409, DiscardState{}, "Refused: an apply or discard of the workspace is still running or waiting. Nothing was dropped."),
			tooMany, fail(503, "The runner holding the copy has not been seen for a minute. Nothing was discarded."), storeFailed},
		serve: (*API).discardWorkspace},
	{method: "POST", path: "/api/v0/sessions", op: "createSession",
		tag:     "Sessions",
		summary: "Create a session",
		doc: "Create a session in a workspace: a conversation whose turns run there. Send workspace (from POST /api/v0/workspaces) and optionally title. The reply names the session; run its turns with POST /api/v0/turns.\n\n" +
			"A session owns only its title and its archived view; its folder and mode are its workspace's. With idempotency_key a retry returns the same session and makes nothing new.",
		body: SessionRequest{},
		replies: createReplies(ok(201, SessionCreated{}, "The session."), fail(404, "No such workspace."),
			fail(403, "A turn token with no workspace to create a session in.")),
		// A turns token may open a session in an EXISTING workspace (then run its
		// turns), the same as an ask: choosing a folder and the Sandbox switch --
		// POST /api/v0/workspaces -- stays with the main token (his ruling,
		// 2026-11-22). The handler checks the named workspace exists (404).
		turn:  createSessionTok,
		turns: true,
		serve: plain((*API).createSession)},
	{method: "GET", path: "/api/v0/sessions", op: "listSessions",
		tag:     "Sessions",
		summary: "List sessions",
		doc: "A page of sessions, newest activity first, and the seq they were computed at.\n\n" +
			"Page older sessions with before set to the next_before of the page before, and store and after set to the first page's store and seq: those pin the snapshot, so a session that gains activity meanwhile keeps its place and no page skips it. Stream from the list's store and seq to follow new events.",
		query: []param{
			{name: "before", doc: "The previous page's next_before: it names that page's last session, and this page is the sessions after it.", integer: true, min: 1},
			{name: "limit", doc: "Sessions per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit},
			{name: "store", doc: "The first page's store, to pin later pages to the same snapshot. With after."},
			{name: "after", doc: "The first page's seq, to pin later pages to the same snapshot. With store.", integer: true, min: 0},
			{name: "archived", doc: "archived=true also lists archived sessions (hidden by default). Any other value, or absent, leaves them out; nothing is ever removed. A page may then hold fewer than limit, and next_before still steps to the next page."}},
		replies: []reply{ok(200, Snapshot{}, "A page of sessions."), badRequest, mismatch, storeFailed},
		serve:   plain((*API).sessions)},
	{method: "GET", path: "/api/v0/sessions/{id}", op: "getSession",
		tag:     "Sessions",
		summary: "Get a session",
		doc: "One session, as GET /api/v0/sessions lists it, and its copy: the runner that holds the session's isolated copy of its folder, " +
			"and whether it is archived, closed and removed. A session that runs in the person's folder as it is has no copy.",
		replies: []reply{ok(200, OneSession{}, "The session."), noSession, storeFailed},
		turn:    treeSessionPath,
		serve:   (*API).getSession},
	{method: "PATCH", path: "/api/v0/sessions/{id}", op: "updateSession",
		tag:     "Sessions",
		summary: "Rename or archive a session",
		doc: "Give a session a name, or archive it. Send title, archived, closed, or a mix.\n\n" +
			"title stores a session.renamed with the new name and the old one. GET /api/v0/sessions then names the session by its latest rename; without one, by its first message. " +
			"Earlier names stay in the record, as the session's events. A rename does not change the session's last activity.\n\n" +
			"archived true hides the session from the default list (archived=true shows it); false un-hides it. It is only a view, recorded as a session.archived event: the session keeps its copy, its history and its name, and can still take a turn. Nothing is removed; it is reversible. It works on any session the service knows -- through the record or, while its first turn is still queued, through the queue -- and is never refused for a running or queued turn. Only the main token archives a session.\n\n" +
			"closed is retired: close is gone. closed true now archives the session (hides it, removes nothing), so an old client keeps working; closed false is refused. Send archived instead.",
		body: SessionUpdate{},
		replies: []reply{ok(200, Renamed{}, "Done."), badRequest,
			fail(403, "A turn token's request is refused: error says why."), noSession, storeFailed},
		turn:  renameSession,
		serve: (*API).updateSession},
	{method: "GET", path: "/api/v0/sessions/{id}/changes", op: "sessionChanges",
		tag:     "Sessions",
		summary: "Preview a session's changes",
		doc: "A page of what applying the session's changes would do to the person's folder now, without writing anything: a preview of POST /api/v0/turns/{id}/apply for the session's latest ended turn with changes, which applies every turn not applied yet.\n\n" +
			"The preview is made by the apply's own code, on the runner of the folder's machine, and stops before the apply would write. Control stores it as a changes.previewed event, the one source, and this route serves and pages that event. A later request reuses the stored preview until the session changes (a new turn or apply) or ?fresh=true asks for a new one; previewed_at says when it was taken. An apply always re-checks the folder itself.\n\n" +
			"files is a page, sorted by path: each file the apply would write or remove, with its change (added, modified or deleted) against the folder now, how many lines it adds and removes, and its unified diff for small text files; a file with no diff says diff_left_out. count is every file. Page the rest with before set to the previous page's next_before and event set to the event id, so every page is of the same preview. truncated is set only when the stored preview kept fewer files than count because they were too many to send (some are not in the record at all); that is distinct from a page simply holding fewer than count, which next_before continues. " +
			"conflicts lists each file the apply would refuse, with why: a file in the folder that is neither as it was at the start of the changes nor already as they leave it. Those files are in files too. " +
			"unapplied is true when files or conflicts is not empty; turn is the turn to apply. When every change is applied, or the folder already holds them, unapplied is false and files is empty. " +
			"A session that works in the folder as it is, not in an isolated copy, answers isolated false, with why; so does a closed session, whose copy is gone. The reply waits up to 30 seconds for the folder's machine when a fresh preview is run.",
		query: []param{
			{name: "fresh", doc: "true runs a new preview instead of reusing the stored one: the Refresh on the changes view, and opening the apply view."},
			{name: "event", doc: "The event id of a changes.previewed from a previous page: pages the same preview."},
			{name: "before", doc: "Only files before this position: the previous page's next_before.", integer: true, min: 1},
			{name: "limit", doc: "Files per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies: []reply{ok(200, SessionChangesPage{}, "A page of the preview."),
			badRequest, noSession,
			fail(409, "An apply of the session has not ended (ask again once it has), or the preview could not be made: error says why. Nothing was written."),
			tooMany, fail(503, "The runner of the folder's machine has not been seen for a minute. Nothing was written."),
			fail(504, "The folder's machine did not answer within 30 seconds. Nothing was written."), storeFailed},
		turn:  treeSessionPath,
		serve: (*API).sessionChanges},
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
		replies:   []reply{ok(200, store.SearchResult{}, "A page of hits."), badRequest, storeFailed},
		turn:      treeSessionPath,
		wrappable: true,
		serve:     (*API).search},
	{method: "GET", path: "/api/v0/sessions/{id}/context", op: "sessionContext",
		tag:     "Sessions",
		summary: "Read a session's context",
		doc: "A session's context: its token totals and a page of its turns, newest first, so the context's growth across the session is visible. Nothing is stored: the session's model.responded events are read now.\n\n" +
			"peak_context_tokens is the largest context any of the session's turns carried. cache_read_tokens and fresh_tokens (input + cache_write) are the session's calls summed. growth is a page of the turns, each with how big its context got (context_tokens) and its own cache split; later turns carry the earlier ones, so the context grows. Page older turns with before set to the previous page's next_before. For one turn's parts, see GET /api/v0/turns/{id}/context.",
		query: []param{
			{name: "before", doc: "Only turns before this position: the next_before of the previous page.", integer: true, min: 1},
			{name: "limit", doc: "Turns per page. 100 when absent.", integer: true, min: 1, max: maxTurnLimit}},
		replies:   []reply{ok(200, SessionContext{}, "The session's context."), badRequest, storeFailed},
		turn:      treeSessionPath,
		wrappable: true,
		serve:     (*API).sessionContext},

	{method: "GET", path: "/api/v0/agents", op: "listAgents",
		tag:     "Agents",
		summary: "List agents",
		doc: "The saved agents, in the order they were made. A turn names one by id in agent.\n\n" +
			"A new data folder starts with none, under agentbus up as under agentbus server: add them from what GET /api/v0/harnesses reports. " +
			"A data folder that already had agents keeps them.",
		replies:   []reply{ok(200, AgentList{}, "The agents."), badRequest},
		turn:      anyRead,
		wrappable: true,
		serve:     plain((*API).listSaved)},
	{method: "POST", path: "/api/v0/agents", op: "createAgent",
		tag:     "Agents",
		summary: "Save an agent",
		doc: "Save a new agent: a harness, how it reaches its model, the model and a name. It gets a new id.\n\n" +
			"access subscription uses the person's subscription for the harness, connected once on control (GET /api/v0/subscriptions). access openrouter needs a model in OpenRouter's naming. Control adds its key to the agent's model calls: " +
			"the key set in Settings (PUT /api/v0/subscriptions/openrouter/key), or, while none is set, the one in the environment variable key_env names where control runs (OPENROUTER_API_KEY when absent). " +
			"The key itself is never sent to a runner or an agent, and never shown or logged. " +
			"access api uses the vendor's own API key, set once in Settings (PUT /api/v0/subscriptions/{name}/key: anthropic for claude, openai for codex, gemini for agy); control's relay adds it to the agent's calls, which carry only a placeholder, and a turn while none is set is refused with where to set it. " +
			"opencode runs on access openrouter only." + plannedRefused(),
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
			"A turn that names it afterwards is refused with \"was removed; pick another\", for a default agent (claude, codex, agy) too. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, badRequest, fail(404, "No such agent."), storeFailed},
		serve:   (*API).removeSaved},
	{method: "GET", path: "/api/v0/tools", op: "listTools",
		tag:     "Tools",
		summary: "List tools",
		doc: "The tools control serves to agents, in a stable order: the built-in ones first, then the person's own. " +
			"The list carries facts only — a name, a description for a model, and where it runs (control). A tool carries no " +
			"effect; a call is decided by its name against the rules. A tool's full definition (its inputs as a JSON Schema " +
			"object and what it runs) is read from GET /api/v0/tools/{id}.",
		replies: []reply{ok(200, ToolList{}, "The tools."), badRequest},
		serve:   plain((*API).listTools)},
	{method: "POST", path: "/api/v0/tools", op: "addTool",
		tag:     "Tools",
		summary: "Add a tool",
		doc: "Add a custom tool in the stored format. A tool whose run kind is api names an AgentBus route by method and path; " +
			"its call is made as that request, with the caller's own token. A name already served is refused; a built-in that was removed may be re-added as a custom tool, which then behaves as any custom tool does.",
		body:    toolreg.Tool{},
		replies: []reply{ok(201, toolreg.Tool{}, "Added."), badRequest, fail(409, "A tool of that name already exists, or there are too many."), storeFailed},
		serve:   plain((*API).createTool)},
	{method: "GET", path: "/api/v0/tools/{id}", op: "getTool",
		idDoc:   "A tool's name.",
		tag:     "Tools",
		summary: "Get a tool",
		doc: "One tool's full definition in the stored format: its name, its description for a model, its inputs as a JSON Schema object, " +
			"what it runs and where it runs. A tool carries no effect. The list (GET /api/v0/tools) carries facts only; this is where a tool's whole definition is read.",
		replies: []reply{ok(200, toolreg.Tool{}, "The tool."), badRequest, fail(404, "No such tool.")},
		serve:   (*API).getTool},
	{method: "DELETE", path: "/api/v0/tools/{id}", op: "removeTool",
		idDoc:   "A tool's name. Another shape gets 404.",
		tag:     "Tools",
		summary: "Remove a tool",
		doc: "Remove a tool by name: a custom tool is dropped; a built-in is marked removed and stops being served. " +
			"A removed built-in can be re-added as a custom tool. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, badRequest, fail(404, "No such tool."), storeFailed},
		serve:   (*API).removeTool},
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
		doc: "The runners control knows, one per machine, for a person: each one's name, id, machine, role, labels, whether it is online, " +
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
			"Its sessions, jobs and their events stay in the record, but its sessions take no more turns or reviews: a turn or review of one waiting now ends failed, and a new one gets 409, each saying the machine was removed. Their applies still go to the folder's machine. A runner.removed event is stored. " +
			"An online runner is refused: stop it on its machine first. To add the machine again, run agentbus login on it. Takes the main token only. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, badRequest, fail(404, "No runner by that id."),
			fail(409, "It is online."), storeFailed},
		serve: (*API).removeRunner},
	{method: "GET", path: "/api/v0/folders", op: "listFolders",
		tag:     "Agents",
		summary: "List folders",
		doc: "The folders the runners hold, each by its reference: a turn names one as folder_ref, with its machine as machine, and runs on that machine. " +
			"A folder's path stays on the machine that owns it; the answer has only the last part of it. " +
			"In agentbus up every folder a turn was started in is listed, newest first, within a minute of its first turn. " +
			"A session's copy is never listed, though a turn may run in one. No body is read.",
		replies: []reply{ok(200, FolderList{}, "The folders."), badRequest},
		serve:   plain((*API).listFolders)},
	{method: "GET", path: "/api/v0/folders/{id}/home-files", op: "folderHomeFiles",
		idDoc:   "A folder's reference (fld_ and 16 hex digits), from GET /api/v0/folders. Another shape gets 404.",
		tag:     "Agents",
		summary: "List the usual config files",
		doc: "Which of the usual config files are in the home of the folder's machine, for \"Use my config\": a cockpit fills Home files (home_files) with them, and the person saves. " +
			"The machine's runner looks, not the server, and answers each path's name, kind (file or folder) and size only, never its content. " +
			"The paths looked for, in this order: ~/.claude/settings.json, ~/.claude/CLAUDE.md, ~/.claude/skills/, ~/.claude/commands/, ~/.claude/agents/, " +
			"~/.claude.json (its mcpServers part only), ~/.codex/config.toml, ~/.codex/AGENTS.md, ~/.gemini/config/mcp_config.json (agy's MCP servers), " +
			"~/.aws/config, ~/.aws/credentials, ~/.config/gh/, ~/.gitconfig and ~/.ssh/config. A path the home does not hold is left out. " +
			"A vendor sign-in file (claude's .credentials.json, codex's auth.json, agy's token) is never listed or counted, and home_files refuses one. " +
			"Nothing is stored. The reply waits up to 15 seconds for the folder's machine. No body is read.",
		query: []param{{name: "machine", doc: "The folder's machine (con_ and 16 hex digits), from GET /api/v0/folders: needed only when two machines hold a folder of that reference. The old name connector is read for one release."}},
		replies: []reply{ok(200, HomeFileList{}, "The config files that machine's home holds."), badRequest,
			fail(404, "No runner holds that folder."), fail(409, "The machine's runner could not list its home: error says why."), tooMany,
			fail(503, "The folder's machine is offline."), fail(504, "The folder's machine did not answer within 15 seconds."), storeFailed},
		serve: (*API).homeFiles},

	{method: "POST", path: "/api/v0/files/read", op: "readFile",
		tag:     "Files",
		summary: "Read an outside file",
		doc:     "Read a file outside the session copy, on control's machine. Only for a session whose folder is on control's own machine (agentbus up), or a call without a session; a session on another machine's runner gets 409, before any approval card.",
		body:    ReadFileRequest{},
		turn:    fileOp,
		tools: []tool{
			{name: "read_file",
				fields:   []string{"path"},
				required: []string{"path"},
				doc:      "Read a file outside the session copy, on control's machine. A session on another machine is refused.",
			},
		},
		replies: []reply{ok(200, ReadFileResponse{}, "The file content."), badRequest, fail(404, "File not found."), elsewhere, storeFailed},
		serve:   plain((*API).readFile)},
	{method: "POST", path: "/api/v0/files/list", op: "listFolder",
		tag:     "Files",
		summary: "List an outside folder",
		doc:     "List entries of a folder outside the session copy, on control's machine. Only for a session whose folder is on control's own machine (agentbus up), or a call without a session; a session on another machine's runner gets 409, before any approval card.",
		body:    ListFolderRequest{},
		turn:    fileOp,
		tools: []tool{
			{name: "list_folder",
				fields:   []string{"path"},
				required: []string{"path"},
				doc:      "List a folder outside the session copy, on control's machine. A session on another machine is refused.",
			},
		},
		replies: []reply{ok(200, ListFolderResponse{}, "The folder entries."), badRequest, fail(404, "Folder not found."), elsewhere, storeFailed},
		serve:   plain((*API).listFolder)},
	{method: "POST", path: "/api/v0/files/write", op: "writeFile",
		tag:     "Files",
		summary: "Write an outside file",
		doc:     "Write a file outside the session copy, on control's machine. Only for a session whose folder is on control's own machine (agentbus up), or a call without a session; a session on another machine's runner gets 409, before any approval card.",
		body:    WriteFileRequest{},
		turn:    fileOp,
		tools: []tool{
			{name: "write_file",
				fields:   []string{"path", "content"},
				required: []string{"path", "content"},
				doc:      "Write a file outside the session copy, on control's machine. A session on another machine is refused.",
			},
		},
		replies: []reply{ok(200, WriteFileResponse{}, "File written."), badRequest, elsewhere, storeFailed},
		serve:   plain((*API).writeFile)},

	{method: "GET", path: "/api/v0/approvals", op: "listApprovals",
		tag:     "Approvals",
		summary: "List commands to approve",
		doc: "The commands a person must answer: each one's id, its session and turn, the agent's shell tool, the commands, the entry of permissions.toml they matched (an ask entry, or one suggested from a command on no list), and when they were recorded. " +
			"The gateway holds a call whose command matches ask, or is on none of the lists under the ask default, and records it here; the turn ends, nothing is held. GET it here, then POST your answer. " +
			"Once answered a command leaves this list; its approval.decided event says how it ended. No token or header value is in the answer. Takes the main token only. No body is read.",
		query:   []param{{name: "state", doc: `Only "pending" is kept here; decided commands are in the events. Absent or "pending" lists them.`}},
		replies: []reply{ok(200, HeldCalls{}, "The commands to approve, oldest first."), badRequest, storeFailed},
		serve:   plain((*API).listApprovals)},
	{method: "POST", path: "/api/v0/approvals/{id}", op: "decideApproval",
		idDoc:   "A command's id, from GET /api/v0/approvals. Another shape gets 404.",
		tag:     "Approvals",
		summary: "Answer a command",
		doc: "Answer a held command, once, always or refuse. The gateway is holding the model's reply at the command: once releases it so the agent runs these commands in the same turn and writes nothing to the settings; always releases them and also moves each matched entry to allow in the settings, for later turns; refuse refuses the held call. No continuation turn is started for any of them (an allow runs in the turn already held). (allow is the old name for always.) " +
			"An approval.decided event records the answer. Takes the main token only.",
		body: Decision{},
		replies: []reply{{status: 204, doc: "Answered.", empty: true}, badRequest, fail(404, "No command by that id, or it was answered already."),
			fail(409, "That command is already being answered."), storeFailed},
		serve: (*API).decideApproval},
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
			"A turn's harness is pointed here with the turn's own token, so its agent sees the apps it may call, such as ask_agent, as tools of its own. " +
			"Reading the record (turns, diffs, sessions, saved agents) and starting, reviewing, applying or stopping work are the API's own routes, reached with agentbus-api, not tools.\n\n" +
			"tools/list answers the tools the token may call: a turn token's under [agent_api] in the settings. " +
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
	{method: "POST", path: SignInPath, op: "signIn",
		tag:     "Service",
		summary: "Sign in with a code",
		doc: "Trade a one-time sign-in code for the main token: how the cockpit that agentbus up serves signs in. " +
			"agentbus up prints and opens http://127.0.0.1:<port>/?t=<code> when it starts; the page sends the code here once. A new link comes from starting agentbus up again. " +
			"A code works once, for 10 minutes. Takes no token. " +
			"Only this service's own address may call it: a request whose Origin is another address, or that the browser marks cross-site, is refused with 403, " +
			"the reply never carries Access-Control-Allow-Origin, and no preflight for it is answered. The body must be application/json.",
		body: SignInRequest{},
		replies: []reply{ok(200, SignedIn{}, "The main token."), badRequest,
			fail(http.StatusUnauthorized, "The code is wrong, used or expired."),
			fail(http.StatusUnsupportedMediaType, "The body is not application/json.")},
		public: true},
}, runnerRoutes, settingsRoutes, subscriptionRoutes, keyRoutes(), []route{modelsRoute})

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

// plannedRefused is the sentence of createAgent's doc that names the planned
// harnesses, from the registry's list, or "" while none is planned.
func plannedRefused() string {
	if harness.PlannedList() == "" {
		return ""
	}
	return " A harness that is only planned (" + harness.PlannedList() + ") is refused: not available yet."
}
