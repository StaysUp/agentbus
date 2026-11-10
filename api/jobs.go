package api

// The job queue's routes: jobs, reviews and workspaces for the cockpit and
// programs, and the runner protocol for runners. Package jobs serves them
// (Config.Jobs); they are listed here so they share the route table, the
// description, the Host check, CORS and the main token.

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
	noJob         = fail(http.StatusNotFound, "No such job, or this service keeps no job queue.")
	queueFull     = fail(http.StatusTooManyRequests, "Too many jobs are waiting; wait for some to end.")
	jobConflict   = fail(http.StatusConflict, "The idempotency key names a job with other fields, or the session is closed.")
	staleReply    = ok(http.StatusConflict, jobs.JobError{}, "The attempt is no longer current (stale is true): stop its work. Or, for events, a gap: send from next.")
	noAttempt     = fail(http.StatusNotFound, "No such attempt or runner for this runner token, or this service keeps no job queue.")
	noBlobs       = fail(http.StatusNotFound, "This service keeps no blob store, or no blob of that hash.")
	noRunnerToken = fail(http.StatusUnauthorized, "A runner's or a connector's token is required.")
)

const jobsDoc = "A job is a requested turn. Its kind is turn or review. A runner takes it and runs it as an attempt, and " +
	"the attempt is the turn: its id is the turn id. Its turn_started says the runner started the turn."

var jobRoutes = []route{
	{method: "POST", path: "/api/v0/jobs", op: "createJob",
		tag:     "Jobs",
		summary: "Make a job",
		doc: "Make a job. " + jobsDoc + "\n\n" +
			"A session's first turn job names agent and a workspace: folder, or repo and base (a git worktree the runner makes). " +
			"agent is a saved agent's id (GET /api/v0/agents); the control plane fills agent_id, access and key_env from it, and model when none is sent. " +
			"A caller never sends those three. With isolate true, a folder in a git repo with a commit gets a worktree of that repo at its HEAD; " +
			"another folder is used as it is. " +
			"A later turn job names session, and runs in that session's workspace. It may name agent too: its turn then runs on that saved agent, " +
			"with that agent's model when none is sent, and later turn jobs that name none keep it. " +
			"A later turn job with no agent and no model runs on the agent and model of the session's latest turn, as its turn.started names them when the turn starts. " +
			"A retry with the same idempotency_key and fields answers 200 with the same job.",
		body: jobs.JobRequest{},
		replies: []reply{ok(201, queue.Job{}, "The job, queued."), ok(200, queue.Job{}, "A retry: the job the key first made."),
			badRequest, jobConflict, queueFull, storeFailed},
		serve: jobsRoute},
	{method: "GET", path: "/api/v0/jobs", op: "listJobs",
		tag:     "Jobs",
		summary: "List jobs",
		doc:     "Jobs, newest first, with their attempts.",
		query: []param{
			{name: "state", doc: "Only jobs in this state: queued, taken, running, finished, failed or cancelled."},
			{name: "kind", doc: "Only jobs of this kind: turn or review."},
			{name: "session", doc: "Only this session's jobs, its reviews included."},
			{name: "reviews", doc: "Only the reviews of this job."},
			{name: "limit", doc: "At most this many jobs, the newest. 100 when absent.", integer: true, min: 1, max: 1000}},
		replies: []reply{ok(200, jobs.JobList{}, "The jobs."), badRequest, storeFailed},
		turn:    treeSessionQuery,
		serve:   jobsRoute},
	{method: "GET", path: "/api/v0/jobs/{id}", op: "getJob",
		idDoc:   "A job id. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Get a job",
		doc:     "One job, with its attempts. A review's verdict and findings are on it once the review has finished.",
		replies: []reply{ok(200, queue.Job{}, "The job."), noJob, storeFailed},
		turn:    treeJob,
		serve:   jobsRoute},
	{method: "POST", path: "/api/v0/jobs/{id}/cancel", op: "cancelJob",
		idDoc:   "A job id. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Cancel a job",
		doc:     "Cancel a job. A queued job ends at once. A running one stops on its runner's next heartbeat, within 2 seconds. No body is read.",
		replies: []reply{ok(202, queue.Job{}, "The job, as it is now."), noJob, storeFailed},
		turn:    askedJob,
		serve:   jobsRoute},
	{method: "POST", path: "/api/v0/jobs/{id}/review", op: "reviewJob",
		idDoc:   "The id of the turn job to review. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Review a job",
		doc: "Make a review job of a turn job that has ended: read-only, in its workspace, by agent and model. " +
			"The prompt holds the task, the changed files and the diff; the author's reply only with include_reply. " +
			"The verdict (pass, fail or unknown) and the findings are read from the reviewer's reply when the review finishes. " +
			"Any P1 finding makes it fail.",
		body: jobs.ReviewRequest{},
		replies: []reply{ok(201, queue.Job{}, "The review job, queued."), ok(200, queue.Job{}, "A retry: the review the key first made."),
			badRequest, fail(403, "A turn token's request is refused: error says why, such as a cap on asked work."),
			noJob, fail(409, "The job cannot be reviewed, its session is closed, or the key names another job."),
			queueFull, storeFailed},
		turn:  askReview,
		serve: jobsRoute},
	{method: "GET", path: "/api/v0/attempts/{id}/events", op: "attemptEvents",
		idDoc:   "An attempt id. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Read an attempt's events",
		doc:     "Every event the runner reported while it ran the attempt: the workspace, the turn, a stop. Oldest first.",
		replies: []reply{ok(200, jobs.AttemptEventList{}, "The events."), fail(404, "No such attempt."), storeFailed},
		turn:    treeAttempt,
		serve:   jobsRoute},
	{method: "GET", path: "/api/v0/workspaces", op: "listWorkspaces",
		tag:     "Jobs",
		summary: "List workspaces",
		doc:     "Each session's workspace: its runner, its folder, and when it was closed and removed.",
		replies: []reply{ok(200, jobs.WorkspaceList{}, "The workspaces."), storeFailed},
		serve:   jobsRoute},
	{method: "POST", path: "/api/v0/workspaces/{id}/close", op: "closeWorkspace",
		tag:     "Jobs",
		summary: "Close a workspace",
		doc: "Close a session's workspace. The session takes no more jobs, and its runner removes a worktree it made. " +
			"A folder the job named is never removed. No body is read.",
		replies: []reply{ok(200, queue.Workspace{}, "The workspace, closed."), noSession,
			fail(409, "A job of the session has not ended."), storeFailed},
		serve: jobsRoute},

	{method: "POST", path: "/api/v0/runners", op: "enrollRunner",
		tag:     "Runners",
		summary: "Enroll a runner",
		doc:     "Add a runner. The reply holds its own token, this once.",
		body:    jobs.EnrollRequest{},
		replies: []reply{ok(201, jobs.Enrolled{}, "The runner and its token."), badRequest, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runners/{id}/take", op: "takeWork",
		idDoc:   "The runner's id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Take work",
		doc: "Long poll, up to 30 seconds, for a job (with its new attempt and lease) or a cleanup (a closed session's workspace to remove). " +
			"A job of a session goes to the runner that holds its workspace.",
		body:    jobs.TakeRequest{},
		replies: []reply{ok(200, jobs.TakenWork{}, "A job, or a cleanup."), {status: 204, doc: "Nothing to do.", empty: true}, badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runners/{id}/heartbeat", op: "runnerHeartbeat",
		idDoc:   "The runner's id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Send an idle heartbeat",
		doc:     "Mark the runner seen and set its capacity. A restarted runner sends it to check the identity it saved.",
		body:    jobs.RunnerBeat{},
		replies: []reply{ok(200, queue.Runner{}, "The runner."), badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "GET", path: "/api/v0/runners/{id}/settings", op: "runnerSettings",
		idDoc:   "The runner's id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Read a turn's settings",
		doc: "The policy and the instructions, read fresh, as GET /api/v0/settings/permissions and /instructions send them. " +
			"A runner reads them before each turn it starts, so every runner follows the server's settings, never its own files. " +
			"When they cannot be had, the runner refuses the turn and the job fails with the reason.",
		replies: []reply{ok(200, settings.RunnerSettings{}, "The settings."), noAttempt,
			fail(http.StatusServiceUnavailable, "The server's settings cannot be used: error says why."), storeFailed},
		self: true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/runners/{id}/removed", op: "runnerRemoved",
		idDoc:   "The runner's id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Report a workspace removed",
		doc:     "Report a closed session's workspace removed, or with error why it could not be.",
		body:    jobs.RemovedReport{},
		replies: []reply{{status: 204, doc: "Recorded.", empty: true}, badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/start", op: "startAttempt",
		idDoc:   "The attempt id: the turn id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Start an attempt",
		doc:     "Taken to running. cancel in the reply says the job was asked to stop.",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.AttemptStarted{}, "Running."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/heartbeat", op: "attemptHeartbeat",
		idDoc:   "The attempt id: the turn id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Renew a lease",
		doc:     "Renew the attempt's 30-second lease. Send it every 2 seconds. cancel in the reply says the job was asked to stop.",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.AttemptBeat{}, "Renewed."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/events", op: "addAttemptEvents",
		idDoc:   "The attempt id: the turn id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Send events",
		doc:     "A batch of the attempt's events, numbered from offset. A batch sent again stores nothing new.",
		body:    jobs.EventBatch{},
		replies: []reply{ok(200, jobs.EventBatchAck{}, "Stored."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/snapshot", op: "attemptSnapshot",
		idDoc:   "The attempt id: the turn id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Ask for a folder's snapshot",
		doc: "For a session's first turn in an isolated copy of a folder on another machine: the snapshot job, made the first time and the same job after, " +
			"on the folder's connector. Send it again until snapshot is set, then build the copy from its blobs (GET /api/v0/blobs/{id}).",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.SnapshotState{}, "The snapshot job, and the snapshot once taken."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/record", op: "attemptRecord",
		idDoc:   "The attempt id: the turn id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Send a turn's record",
		doc: "A runner of a server sends its own record of the attempt's turn, at the turn's end, so the server's record holds the turn: " +
			"its session, prompt, model calls and how it ended. Each event keeps its id, and one already held is not stored again. " +
			"An event's folder (the copy's path on the runner) is dropped; folder_ref stays.",
		body:    jobs.RecordBatch{},
		replies: []reply{ok(200, jobs.RecordAck{}, "Stored."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/blobs", op: "missingBlobs",
		tag:     "Runners",
		summary: "Ask which blobs are missing",
		doc: "The control plane's blob store holds every file of a snapshot and of a turn's changes, by the hex SHA-256 of its content. " +
			"Of the hashes sent, at most 10000, the reply lists those it does not hold, so only those are sent. A runner's or a connector's token.",
		body:    blobs.MissingRequest{},
		replies: []reply{ok(200, blobs.MissingReply{}, "The hashes not held."), badRequest, noBlobs, noRunnerToken},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/packs", op: "putBlobPack",
		tag:     "Runners",
		summary: "Send many blobs",
		doc: "A pack: frames, each a line of the blob's hash, a space and its size in bytes, then that many bytes; as application/octet-stream, " +
			"at most 1000 blobs and 8 MiB of them (a bigger blob goes alone by PUT). Each is kept only when its SHA-256 is its hash; the blobs " +
			"before a bad one stay kept. A runner's or a connector's token.",
		replies: []reply{ok(200, blobs.PackReply{}, "Every blob kept."), fail(400, "The pack is not well formed, or a blob does not have its hash."),
			noBlobs, noRunnerToken, fail(413, "The pack is bigger than a pack may be."), fail(500, "The pack could not be kept.")},
		self: true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/packs/fetch", op: "getBlobPack",
		tag:     "Runners",
		summary: "Read many blobs",
		doc: "The blobs of the hashes sent, 1 to 1000, as one pack in the order asked (the frames of POST /api/v0/packs), streamed. " +
			"A runner's or a connector's token.",
		body: blobs.FetchRequest{},
		replies: []reply{{status: 200, doc: "The pack.", empty: true}, badRequest,
			{status: 404, doc: "A blob asked for is not held: the reply lists the missing ones.", body: blobs.MissingReply{}}, noRunnerToken},
		self: true, serve: jobsRoute},
	{method: "PUT", path: "/api/v0/blobs/{id}", op: "putBlob",
		idDoc:   "The blob's hash: 64 lowercase hex digits, the SHA-256 of the body.",
		tag:     "Runners",
		summary: "Send a blob",
		doc: "The body is the blob's content, as application/octet-stream, streamed: the 2 MiB limit of the runner protocol does not apply, up to 4 GiB. " +
			"It is kept only when its SHA-256 is the hash in the path. A blob already held is kept as it was. A runner's or a connector's token.",
		replies: []reply{{status: 204, doc: "Kept.", empty: true}, fail(400, "The content does not have the hash in the path."), noBlobs,
			noRunnerToken, fail(413, "The blob is bigger than 4 GiB."), fail(500, "The blob could not be kept.")},
		self: true, serve: jobsRoute},
	{method: "GET", path: "/api/v0/blobs/{id}", op: "getBlob",
		idDoc:   "The blob's hash: 64 lowercase hex digits.",
		tag:     "Runners",
		summary: "Read a blob",
		doc:     "The blob's content, as application/octet-stream, streamed. A runner's or a connector's token.",
		replies: []reply{{status: 200, doc: "The content.", empty: true}, fail(404, "No blob of that hash, or this service keeps no blob store."), noRunnerToken},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/finish", op: "finishAttempt",
		idDoc:   "The attempt id: the turn id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Finish an attempt",
		doc:     "End the attempt and its job: finished, failed or cancelled, with the result. The same finish again gets the same answer.",
		body:    jobs.FinishCall{},
		replies: []reply{ok(200, queue.Job{}, "The job, ended."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
}
