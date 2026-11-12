package api

// The runner protocol's routes. Package jobs serves them (Config.Jobs); they
// are listed here so they share the route table, the Host check and CORS,
// and are described from it.

import (
	"github.com/staysup/agentbus/blobs"
	"net/http"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// jobsRoute hands a request to Config.Jobs.
func jobsRoute(a *API, w http.ResponseWriter, r *http.Request, _ string) {
	if a.cfg.Jobs == nil {
		writeError(w, http.StatusNotFound, "this service keeps no job queue")
		return
	}
	a.cfg.Jobs.ServeHTTP(w, r)
}

var (
	staleReply    = ok(http.StatusConflict, jobs.JobError{}, "The attempt is no longer current (stale is true): stop its work. Or, for events, a gap: send from next.")
	noAttempt     = fail(http.StatusNotFound, "No such attempt for this runner token, or this service keeps no job queue.")
	noBlobs       = fail(http.StatusNotFound, "This service keeps no blob store, or no blob of that hash.")
	noRunnerToken = fail(http.StatusUnauthorized, "A runner's token is required.")
)

// runnerRoutes are the runner protocol: how runners, one per machine, talk to
// control. Package jobs serves them, with their own tokens. They are
// described on their own (/api/v0/runner/openapi.json), never with the
// client API: the protocol is our binaries' own and may change freely.
var runnerRoutes = []route{
	{method: "GET", path: "/api/v0/runner/openapi.json", op: "runnerOpenapi",
		tag:     "Runners",
		summary: "Get this description",
		doc:     "This description of the runner protocol. The client API's is at /api/v0/openapi.json.",
		replies: []reply{ok(200, map[string]any{}, "This document.")},
		public:  true, self: true},
	{method: "POST", path: "/api/v0/runner/register", op: "registerRunner",
		tag:     "Runners",
		summary: "Register a runner",
		doc:     "Add a runner, the one program of its machine, with the enrollment token: the old way, kept for setups made with it; agentbus login (POST /api/v0/runner/login) is the new. The reply holds its own token, this once.",
		body:    jobs.EnrollRequest{},
		replies: []reply{ok(201, jobs.Enrolled{}, "The runner and its token."), badRequest, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/login", op: "startLogin",
		tag:     "Runners",
		summary: "Ask to sign a machine in",
		doc: "agentbus login: a machine asks to become a runner, with no token. The reply holds a code for the machine to show and its runner token, this once. " +
			"The token works for nothing but GET /api/v0/runner/login until the person types the code to approve the machine (POST /api/v0/runners/pending/{id}/approve). " +
			"A sign-in waits 15 minutes; at most 20 wait at once.",
		body:    jobs.EnrollRequest{Protocol: jobs.ProtocolVersion},
		replies: []reply{ok(201, jobs.LoginStarted{}, "Waiting for the person."), badRequest, fail(429, "Too many machines wait."), storeFailed},
		public:  true, self: true, serve: jobsRoute},
	{method: "GET", path: "/api/v0/runner/login", op: "loginState",
		tag:     "Runners",
		summary: "Read a sign-in",
		doc:     "How a machine's sign-in stands, with the token POST /api/v0/runner/login gave: pending, approved (with the runner), refused, expired or removed.",
		replies: []reply{ok(200, jobs.LoginState{}, "The sign-in."), fail(401, "The token of a sign-in is required."), storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/take", op: "takeWork",
		tag:     "Runners",
		summary: "Take work",
		doc: "Long poll, up to 30 seconds, for a job (with its new attempt and lease) or a cleanup (a closed session's workspace to remove). " +
			"A job of a session goes to the runner that holds its workspace, and a job that names a machine (spec.connector) to that machine's runner only. " +
			"With folders true it is a folder job of the runner's own machine (an apply, a typed folder's check), in one of its 4 folder places; without it a turn or a review.",
		body:    jobs.TakeRequest{Protocol: jobs.ProtocolVersion},
		replies: []reply{ok(200, jobs.TakenWork{}, "A job, or a cleanup."), {status: 204, doc: "Nothing to do.", empty: true}, badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/heartbeat", op: "runnerHeartbeat",
		tag:     "Runners",
		summary: "Send an idle heartbeat",
		doc:     "Mark the runner seen and set its capacity. A restarted runner sends it to check the identity it saved.",
		body:    jobs.RunnerBeat{},
		replies: []reply{ok(200, queue.Runner{}, "The runner."), badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "GET", path: "/api/v0/runner/settings", op: "runnerSettings",
		tag:     "Runners",
		summary: "Read a turn's settings",
		doc: "The policy and the instructions, read fresh, as GET /api/v0/settings/permissions and /instructions send them. " +
			"A runner reads them before each turn it starts, so every runner follows the server's settings, never its own files. " +
			"When they cannot be had, the runner refuses the turn and the job fails with the reason.",
		replies: []reply{ok(200, settings.RunnerSettings{}, "The settings."), noAttempt,
			fail(http.StatusServiceUnavailable, "The server's settings cannot be used: error says why."), storeFailed},
		self: true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/removed", op: "runnerRemoved",
		tag:     "Runners",
		summary: "Report a workspace removed",
		doc:     "Report a closed session's workspace removed, or with error why it could not be.",
		body:    jobs.RemovedReport{},
		replies: []reply{{status: 204, doc: "Recorded.", empty: true}, badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/start", op: "startAttempt",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Start an attempt",
		doc:     "Taken to running. cancel in the reply says the job was asked to stop.",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.AttemptStarted{}, "Running."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/heartbeat", op: "attemptHeartbeat",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Renew a lease",
		doc:     "Renew the attempt's 30-second lease. Send it every 2 seconds. cancel in the reply says the job was asked to stop.",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.AttemptBeat{}, "Renewed."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/events", op: "addAttemptEvents",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Send events",
		doc:     "A batch of the attempt's events, numbered from offset. A batch sent again stores nothing new.",
		body:    jobs.EventBatch{},
		replies: []reply{ok(200, jobs.EventBatchAck{}, "Stored."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/snapshot", op: "attemptSnapshot",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Ask for a folder's snapshot",
		doc: "For a session's first turn in an isolated copy of a folder: the snapshot job, made the first time and the same job after, " +
			"on the folder's machine. Since protocol 4 a session runs on its folder's machine, and the runner of this build never asks for one. Send it again until snapshot is set, then build the copy from its blobs (GET /api/v0/runner/blobs/{id}).",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.SnapshotState{}, "The snapshot job, and the snapshot once taken."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/record", op: "attemptRecord",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Send a turn's record",
		doc: "A runner of a server sends its own record of the attempt's turn, while it runs and at its end, so the server's record holds the turn: " +
			"its session, prompt, model calls and how it ended. Each event keeps its id, and one already held is not stored again. " +
			"An event's folder (the copy's path on the runner) is dropped; folder_ref stays.",
		body:    jobs.RecordBatch{},
		replies: []reply{ok(200, jobs.RecordAck{}, "Stored."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/calls", op: "attemptCalls",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Send a turn's model calls",
		doc: "A runner of a server sends the model calls of the attempt's turn as its own record kept them, while the turn runs, " +
			"so a cockpit on the server shows each call's context and reply. A piece the server holds may come without its bytes; " +
			"the answer lists the hashes it lacks, and those calls are sent again with them. A call already held is not stored again.",
		body:    jobs.CallBatch{},
		replies: []reply{ok(200, jobs.CallAck{}, "Stored, or the pieces still missing."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/blobs", op: "missingBlobs",
		tag:     "Blobs",
		summary: "Ask which blobs are missing",
		doc: "The control plane's blob store holds every file of a snapshot and of a turn's changes, by the hex SHA-256 of its content. " +
			"Of the hashes sent, at most 10000, the reply lists those it does not hold, so only those are sent. A runner's token.",
		body:    blobs.MissingRequest{},
		replies: []reply{ok(200, blobs.MissingReply{}, "The hashes not held."), badRequest, noBlobs, noRunnerToken},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/packs", op: "putBlobPack",
		tag:     "Blobs",
		summary: "Send many blobs",
		doc: "A pack: frames, each a line of the blob's hash, a space and its size in bytes, then that many bytes; as application/octet-stream, " +
			"at most 1000 blobs and 8 MiB of them (a bigger blob goes alone by PUT). Each is kept only when its SHA-256 is its hash; the blobs " +
			"before a bad one stay kept. A runner's token.",
		replies: []reply{ok(200, blobs.PackReply{}, "Every blob kept."), fail(400, "The pack is not well formed, or a blob does not have its hash."),
			noBlobs, noRunnerToken, fail(413, "The pack is bigger than a pack may be."), fail(500, "The pack could not be kept.")},
		self: true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/packs/fetch", op: "getBlobPack",
		tag:     "Blobs",
		summary: "Read many blobs",
		doc: "The blobs of the hashes sent, 1 to 1000, as one pack in the order asked (the frames of POST /api/v0/runner/packs), streamed. " +
			"A runner's token, and only blobs of its own jobs: those of each session it holds an attempt of that has not ended or ended in the last 10 minutes.",
		body: blobs.FetchRequest{},
		replies: []reply{{status: 200, doc: "The pack.", empty: true}, badRequest,
			{status: 404, doc: "A blob asked for is not held: the reply lists the missing ones.", body: blobs.MissingReply{}}, noRunnerToken,
			fail(403, "A blob asked for is not of this runner's jobs."), fail(500, "The runner's jobs could not be read.")},
		self: true, serve: jobsRoute},
	{method: "PUT", path: "/api/v0/runner/blobs/{id}", op: "putBlob",
		idDoc:   "The blob's hash: 64 lowercase hex digits, the SHA-256 of the body.",
		tag:     "Blobs",
		summary: "Send a blob",
		doc: "The body is the blob's content, as application/octet-stream, streamed: the 2 MiB limit of the runner protocol does not apply, up to 4 GiB. " +
			"It is kept only when its SHA-256 is the hash in the path. A blob already held is kept as it was. A runner's token.",
		replies: []reply{{status: 204, doc: "Kept.", empty: true}, fail(400, "The content does not have the hash in the path."), noBlobs,
			noRunnerToken, fail(413, "The blob is bigger than 4 GiB."), fail(500, "The blob could not be kept.")},
		self: true, serve: jobsRoute},
	{method: "GET", path: "/api/v0/runner/blobs/{id}", op: "getBlob",
		idDoc:   "The blob's hash: 64 lowercase hex digits.",
		tag:     "Blobs",
		summary: "Read a blob",
		doc:     "The blob's content, as application/octet-stream, streamed. A runner's token, and only a blob of its own jobs (as getBlobPack).",
		replies: []reply{{status: 200, doc: "The content.", empty: true}, fail(404, "No blob of that hash, or this service keeps no blob store."), noRunnerToken,
			fail(403, "The blob is not of this runner's jobs."), fail(500, "The runner's jobs could not be read.")},
		self: true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runner/attempts/{id}/finish", op: "finishAttempt",
		idDoc:   "The attempt id, from take. Another shape gets 404.",
		tag:     "Attempts",
		summary: "Finish an attempt",
		doc:     "End the attempt and its job: finished, failed or cancelled, with the result. The same finish again gets the same answer.",
		body:    jobs.FinishCall{},
		replies: []reply{ok(200, queue.Job{}, "The job, ended."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
}
