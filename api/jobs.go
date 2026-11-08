package api

// The job queue's routes: jobs, reviews and workspaces for the cockpit and
// programs, and the runner protocol for runners. Package jobs serves them
// (Config.Jobs); they are listed here so they share the route table, the
// description, the Host check, CORS and the main token.

import (
	"net/http"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
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
	noJob       = fail(http.StatusNotFound, "No such job, or this service keeps no job queue.")
	queueFull   = fail(http.StatusTooManyRequests, "Too many jobs are waiting; wait for some to end.")
	jobConflict = fail(http.StatusConflict, "The idempotency key names a job with other fields, or the session is closed.")
	staleReply  = ok(http.StatusConflict, jobs.JobError{}, "The attempt is no longer current (stale is true): stop its work. Or, for events, a gap: send from next.")
	noAttempt   = fail(http.StatusNotFound, "No such attempt or runner for this runner token, or this service keeps no job queue.")
)

const jobsDoc = "A job is a requested turn. Its kind is turn or review. A runner takes it and runs it as an attempt."

var jobRoutes = []route{
	{method: "POST", path: "/api/v0/jobs", op: "createJob",
		tag:     "Jobs",
		summary: "Make a job",
		doc: "Make a job. " + jobsDoc + "\n\n" +
			"A session's first turn job names agent and a workspace: folder, or repo and base (a git worktree the runner makes). " +
			"A later turn job names session only, and runs in that session's workspace. " +
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
		serve:   jobsRoute},
	{method: "GET", path: "/api/v0/jobs/{id}", op: "getJob",
		idDoc:   "A job id. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Get a job",
		doc:     "One job, with its attempts. A review's verdict and findings are on it once the review has finished.",
		replies: []reply{ok(200, queue.Job{}, "The job."), noJob, storeFailed},
		serve:   jobsRoute},
	{method: "POST", path: "/api/v0/jobs/{id}/cancel", op: "cancelJob",
		idDoc:   "A job id. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Cancel a job",
		doc:     "Cancel a job. A queued job ends at once. A running one stops on its runner's next heartbeat. No body is read.",
		replies: []reply{ok(202, queue.Job{}, "The job, as it is now."), noJob, storeFailed},
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
			badRequest, noJob, fail(409, "The job cannot be reviewed, its session is closed, or the key names another job."),
			queueFull, storeFailed},
		serve: jobsRoute},
	{method: "GET", path: "/api/v0/attempts/{id}/events", op: "attemptEvents",
		idDoc:   "An attempt id. Another shape gets 404.",
		tag:     "Jobs",
		summary: "Read an attempt's events",
		doc:     "Every event the runner reported while it ran the attempt: the workspace, the turn, a stop. Oldest first.",
		replies: []reply{ok(200, jobs.AttemptEventList{}, "The events."), fail(404, "No such attempt."), storeFailed},
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
	{method: "POST", path: "/api/v0/runners/{id}/removed", op: "runnerRemoved",
		idDoc:   "The runner's id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Report a workspace removed",
		doc:     "Report a closed session's workspace removed, or with error why it could not be.",
		body:    jobs.RemovedReport{},
		replies: []reply{{status: 204, doc: "Recorded.", empty: true}, badRequest, noAttempt, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/start", op: "startAttempt",
		idDoc:   "The attempt id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Start an attempt",
		doc:     "Taken to running. cancel in the reply says the job was asked to stop.",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.AttemptStarted{}, "Running."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/heartbeat", op: "attemptHeartbeat",
		idDoc:   "The attempt id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Renew a lease",
		doc:     "Renew the attempt's 30-second lease. Send it every 10 seconds. cancel in the reply says the job was asked to stop.",
		body:    jobs.AttemptCall{},
		replies: []reply{ok(200, jobs.AttemptBeat{}, "Renewed."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/events", op: "addAttemptEvents",
		idDoc:   "The attempt id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Send events",
		doc:     "A batch of the attempt's events, numbered from offset. A batch sent again stores nothing new.",
		body:    jobs.EventBatch{},
		replies: []reply{ok(200, jobs.EventBatchAck{}, "Stored."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
	{method: "POST", path: "/api/v0/attempts/{id}/finish", op: "finishAttempt",
		idDoc:   "The attempt id. Another shape gets 404.",
		tag:     "Runners",
		summary: "Finish an attempt",
		doc:     "End the attempt and its job: finished, failed or cancelled, with the result. The same finish again gets the same answer.",
		body:    jobs.FinishCall{},
		replies: []reply{ok(200, queue.Job{}, "The job, ended."), badRequest, noAttempt, staleReply, storeFailed},
		self:    true, serve: jobsRoute},
}
