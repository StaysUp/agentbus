package api

// H1: a long command an agent needs (a 10 to 15 minute gate) becomes an
// AgentBus job, so the agent's turn ends when it answers. POST
// /api/v0/commands queues an exec job (queue.KindExec) in the asking turn's
// workspace, with its delivery on in the same write; the runner holding the
// workspace runs it once the asking turn has ended (the workspace's lane), and
// its end comes back to the asking session as a new turn carrying its exit code
// and the end of its output. The run_command app (apps.go) is the tool an agent
// calls. It is recorded (command.ended), listed as pending work (its on, GET
// /api/v0/ons), and can be stopped.
//
// Before building it, how two CI runners run a step and report its end
// (_references/): Buildkite's agent (agent/run_job.go, job_runner.go) reports
// an exit status with a signal reason (cancel, agent stop, or -1 for a step
// that never ran) and cancels with a signal, a grace period, then a kill; we
// take the exit code beside a reason, and SIGTERM then SIGKILL after a grace.
// GitHub Actions' runner is not cloned there; from its documentation, a step
// has timeout-minutes and a cancel is SIGINT, then SIGTERM, then a kill; we
// take a per-command time limit in minutes.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// Command time limits, in minutes.
const (
	DefaultCommandMinutes = 30
	MaxCommandMinutes     = 240
)

// MaxCommandLine bounds a command line, in bytes.
const MaxCommandLine = 16 << 10

// CommandRequest is the body of POST /api/v0/commands.
type CommandRequest struct {
	Command        string `json:"command" doc:"The shell command line, run by /bin/sh -c in your workspace's folder (its copy, or the folder itself for a direct workspace), such as make gate. From 1 byte to 16 KiB, no NUL byte. Required."`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty" doc:"The most minutes it may run, from 1 to 240. Past it the command is stopped (SIGTERM, then SIGKILL after 5 seconds) and ends failed with reason timeout. Absent: 30."`
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, as POST /api/v0/turns takes: a retry with the same key and fields answers with the command the key first made, and runs nothing new."`
}

// CommandStarted is the answer of POST /api/v0/commands.
type CommandStarted struct {
	Command string `json:"command" doc:"The command's id (c_ and 16 hex digits). GET /api/v0/commands/{id} reads it; POST /api/v0/commands/{id}/stop stops it."`
	State   string `json:"state" doc:"queued: it waits for your turn and the work before it in the workspace to end, or for a runner. running: a runner started it."`
	// Existing: the key had already made this command.
	Existing bool `json:"existing,omitempty" doc:"True when idempotency_key had already made this command, so nothing new was made."`
}

// CommandView is one command, from GET /api/v0/commands/{id}.
type CommandView struct {
	ID        string `json:"id" doc:"The command's id (c_ and 16 hex digits)."`
	Line      string `json:"line" doc:"The command line as sent."`
	State     string `json:"state" doc:"queued, running, finished (exit code 0), failed (another exit code, its time limit, or its runner lost; reason says which) or stopped."`
	Reason    string `json:"reason,omitempty" doc:"Why it failed: exit (a non-zero exit code), timeout, runner_lost, runner_stopped or setup (it could not start; error says why)."`
	Session   string `json:"session" doc:"The session of the turn that asked for it; its result comes to this session as a new turn."`
	Turn      string `json:"turn" doc:"The turn that asked for it."`
	Workspace string `json:"workspace" doc:"The workspace it runs in."`
	// The end, once known.
	ExitCode    *int       `json:"exit_code,omitempty" doc:"Its exit code, or 128 plus the signal that ended it. Absent until it ended, and for a command that never ran."`
	TimedOut    bool       `json:"timed_out,omitempty" doc:"It ran past timeout_minutes and was stopped."`
	Output      string     `json:"output,omitempty" doc:"The end of what it wrote to stdout and stderr together, at most 16 KiB. Present once it ended."`
	OutputBytes int64      `json:"output_bytes,omitempty" doc:"All the bytes it wrote, kept or not."`
	DurationMS  int64      `json:"duration_ms,omitempty" doc:"How long it ran."`
	Error       string     `json:"error,omitempty" doc:"Why it did not run, when it did not."`
	Timeout     int64      `json:"timeout_minutes" doc:"Its time limit in minutes."`
	Created     time.Time  `json:"created" doc:"When it was asked for."`
	Ended       *time.Time `json:"ended,omitempty" doc:"When it ended."`
}

const (
	noCommand             = "no such command"
	commandNotRunning     = "the command has ended"
	commandOnlyTurns      = "only an agent's turn runs a command through AgentBus: it runs in that turn's workspace and its result comes back to that turn's session"
	commandStopRefused    = "a turn token stops only commands asked for in the sessions it may read"
	commandBeyondTurn     = "AgentBus runs a command with no wall of its own yet, so it runs one only where it would get no more than your turn may do, and here it would: "
	commandLimitUnknown   = "your turn's runner runs an agentbus from before commands, so its record does not say what a command of your turn may do, and the command does not run; run it in your own shell"
	commandNoStart        = "control's record holds no start of your turn, so it cannot say what a command of your turn may do, and the command does not run"
	commandNeedsPerson    = "this command needs a person's answer under your rules, and a command AgentBus runs has no one to answer it; run it in your own shell, where it is held for a person, or ask the person to allow it"
	commandRefusedPrefix  = "the command is refused by the rule "
	commandBadLine        = "command is required: from 1 byte to 16 KiB, no NUL byte"
	commandBadTimeout     = "timeout_minutes must be from 1 to 240"
	commandBadBody        = "the body must be a JSON object with command"
	commandNoWorkspace    = "your session has no workspace to run a command in"
	commandTurnNotQueued  = "your turn is not one this service queued, so its workspace's runner is not known"
	commandWaitingTooMany = "too many jobs are waiting; wait for some to end and retry"
)

// POST /api/v0/commands: queue a command in the asking turn's workspace.
func (a *API) runCommand(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	var body CommandRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, commandBadBody)
		return
	}
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if strings.TrimSpace(body.Command) == "" || len(body.Command) > MaxCommandLine || strings.ContainsRune(body.Command, 0) {
		writeError(w, http.StatusBadRequest, commandBadLine)
		return
	}
	minutes := body.TimeoutMinutes
	if minutes == 0 {
		minutes = DefaultCommandMinutes
	}
	if minutes < 1 || minutes > MaxCommandMinutes {
		writeError(w, http.StatusBadRequest, commandBadTimeout)
		return
	}
	c := callerOf(r)
	if c.Turn == "" || c.Session == "" {
		writeError(w, http.StatusForbidden, commandOnlyTurns)
		return
	}
	// The command runs in the asking turn's workspace, the one source of its
	// folder, machine and mode (as POST /api/v0/turns reads it).
	wsID, found, err := a.workspaceOf(c.Session)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusConflict, commandNoWorkspace)
		return
	}
	wsp, ok, err := a.cfg.Store.Workspace(wsID)
	if err != nil {
		a.internal(w)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, commandNoWorkspace)
		return
	}
	asking, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, c.Turn)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusConflict, commandTurnNotQueued)
		return
	case err != nil:
		a.internal(w)
		return
	}
	// A command never gets more than the asking turn itself may do: the one
	// rule is harness.CommandLimit, which the runner decided at the turn's
	// launch from the turn's effective limits (the spec its harness started
	// with) and recorded on its turn.started, which reaches control before the
	// turn's child starts (run.Request.Recorded). A turn.started that says
	// nothing of commands (a runner from before H1) takes no command.
	may, beyond, found, err := a.cfg.Store.TurnCommands(c.Session, c.Turn)
	switch {
	case err != nil:
		a.internal(w)
		return
	case !found:
		// A runner sends a turn's start to control before its child starts
		// (run.Request.Recorded), so a turn that can call has its start here.
		writeError(w, http.StatusForbidden, commandNoStart)
		return
	case !may && beyond == "":
		// The start is here and says nothing of commands: its runner records
		// no command limit (an agentbus from before H1).
		writeError(w, http.StatusForbidden, commandLimitUnknown)
		return
	case !may:
		refuseLimit(w, beyond)
		return
	}
	// The turn's own command rules, as the gateway decides its shell commands:
	// allow runs, forbid refuses, and ask or a command on no list (held for a
	// person in a turn's own shell) is refused here, since no one is there to
	// answer.
	rules := gateway.Rules{Allow: c.rules.Allow, Ask: c.rules.Ask, Forbid: c.rules.Forbid, Unlisted: gateway.Ask}
	if c.limits.gated() {
		switch dec, rule := gateway.DecideCommand(rules, body.Command); dec {
		case gateway.Refuse:
			writeError(w, http.StatusForbidden, commandRefusedPrefix+quoteRule(rule))
			return
		case gateway.Ask:
			writeError(w, http.StatusForbidden, commandNeedsPerson)
			return
		}
	}
	spec := queue.Spec{Session: ids.SessionID(c.Session), Workspace: wsID, FolderRef: wsp.FolderRef, Folders: Places(wsp),
		Machine: wsp.Machine, Isolate: wsp.Sandbox, Labels: asking.Spec.Labels,
		Command: body.Command, CommandTimeout: int64(minutes) * 60}
	deliver := &queue.Delivery{Session: c.Session, Maker: queue.Maker{Session: c.Session, Turn: c.Turn}}
	job, created, err := a.cfg.Queue.CreateCommand(queue.DefaultTeam, body.IdempotencyKey, spec, askedLinks(c.Grant), queueLimits(c.rules), deliver)
	var limit *queue.LimitError
	var removed *queue.RunnerRemovedError
	switch {
	case errors.As(err, &limit):
		writeError(w, http.StatusForbidden, limit.Msg)
		return
	case errors.As(err, &removed):
		writeError(w, http.StatusConflict, removed.Error())
		return
	case errors.Is(err, queue.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key already made a command with other fields; send a new key")
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusConflict, "this workspace is closed; it takes no more work")
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, commandWaitingTooMany)
		return
	case err != nil:
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusAccepted, CommandStarted{Command: job.Turn, State: commandState(job), Existing: !created})
}

// refuseLimit refuses a command that would get more than its turn may do.
func refuseLimit(w http.ResponseWriter, limit string) {
	writeError(w, http.StatusForbidden, commandBeyondTurn+limit)
}

// quoteRule names a forbid rule in a refusal, or says none matched.
func quoteRule(rule string) string {
	if rule == "" {
		return "for commands on no list"
	}
	return `"` + rule + `"`
}

// commandState is what the API says of a command whose job is j.
func commandState(j queue.Job) string { return queue.TurnState(j.State) }

// GET /api/v0/commands/{id}: one command and, once it ended, how.
func (a *API) getCommand(w http.ResponseWriter, _ *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, err := a.cfg.Queue.JobByCommand(queue.DefaultTeam, id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noCommand)
		return
	case err != nil:
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, commandView(j))
}

// commandView is exec job j as the API shows it.
func commandView(j queue.Job) CommandView {
	v := CommandView{ID: j.Turn, Line: j.Spec.Command, State: commandState(j), Reason: j.Reason,
		Session: string(j.Spec.Session), Workspace: string(j.Spec.Workspace), Timeout: j.Spec.CommandTimeout / 60,
		Created: j.Created, Ended: j.Ended}
	if ch := j.Links.Chain; ch != nil {
		v.Turn = ch.By
	}
	if j.Ended != nil {
		r, _ := queue.ReadResult(j.Result)
		v.ExitCode, v.TimedOut, v.Output, v.OutputBytes, v.DurationMS = r.ExitCode, r.TimedOut, r.Output, r.OutputBytes, r.DurationMS
		if r.Refused != "" {
			v.Error = r.Refused
		} else {
			v.Error = r.Error
		}
	}
	return v
}

// POST /api/v0/commands/{id}/stop: stop a queued or running command.
func (a *API) stopCommand(w http.ResponseWriter, _ *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, err := a.cfg.Queue.JobByCommand(queue.DefaultTeam, id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, noCommand)
		return
	case err != nil:
		a.internal(w)
		return
	}
	if j.Ended != nil {
		writeError(w, http.StatusConflict, commandNotRunning)
		return
	}
	if _, err := a.cfg.Queue.Cancel(queue.DefaultTeam, j.ID); err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusAccepted, CommandStarted{Command: j.Turn, State: "stopping"})
}

// commandRoutes are the routes of commands an agent hands to AgentBus.
var commandRoutes = []route{
	{method: "POST", path: "/api/v0/commands", op: "runCommand", action: run.ActRun,
		tag:     "Commands",
		summary: "Run a command",
		doc: "Run a long shell command, such as a 10 to 15 minute gate, as AgentBus work, so your turn can end while it runs. Only an agent's turn token may call it (the run_command tool). " +
			"The command runs in your turn's workspace, on the runner that holds it, under your turn's command rules: a command your rules allow runs; one a forbid rule matches is refused (403); one that would be held for a person (an ask rule, or a command on no list) is refused too (403), since no one is there to answer.\n\n" +
			"The reply comes at once with the command's id, queued. It starts once your turn and the work before it in the workspace have ended, and never beside another command there. While it runs, a turn sent to the workspace does not wait for it; a discard, update, close or put-away does. " +
			"When it ends, AgentBus starts a new turn in your session, on your agent, whose prompt holds the command, how it ended, its exit code and the end of its output (16 KiB): do not wait or poll for it. command.ended records it. " +
			"Its delivery is an on (GET /api/v0/ons lists it, event command.ended), so it moves with the session's other ons. It counts as work your turn asked for under the caps in Settings, Limits.\n\n" +
			"It never gets more than your turn may do: AgentBus has no wall of its own for a command yet, so it is refused (403, saying why) when your turn is limited in a way the command would escape: your turn may not edit (edits forbidden, or read-only), or your harness runs your shell commands in its own sandbox (Claude Code in a sandbox copy; codex always). Your runner decides this when your turn starts, from the limits your harness started with. It gets the runner's environment, without AgentBus's variables (no turn token).",
		body: CommandRequest{},
		at:   atBody,
		replies: []reply{ok(202, CommandStarted{}, "The command, queued."), badRequest,
			fail(403, "Not an agent's turn token, the command is refused or would need a person under the turn's rules, the copy's wall cannot hold it, or a cap on asked work was reached: error says which."),
			fail(404, "This service keeps no queue."),
			fail(409, "The turn's session has no workspace, the turn is not one this service queued, the workspace is closed or its machine was removed, or the idempotency key names another command."),
			fail(http.StatusTooManyRequests, "Too many jobs are waiting; wait for some to end."), storeFailed},
		serve: plain((*API).runCommand)},
	{method: "GET", path: "/api/v0/commands/{id}", op: "getCommand", action: run.ActRead,
		idDoc:   "A command's id (c_ and 16 hex digits), as POST /api/v0/commands named it.",
		tag:     "Commands",
		summary: "Get a command",
		doc:     "One command: its line, its state (queued, running, finished, failed or stopped), the turn and session that asked for it, and once it ended its exit code, the end of its output, how long it ran and why it failed. A turn token reads the commands asked for in the sessions it may read.",
		replies: []reply{ok(200, CommandView{}, "The command."), fail(404, "No such command."), storeFailed},
		at:      atCommand,
		serve:   (*API).getCommand},
	{method: "POST", path: "/api/v0/commands/{id}/stop", op: "stopCommand", action: run.ActRun,
		idDoc:   "A command's id (c_ and 16 hex digits), as POST /api/v0/commands named it.",
		tag:     "Commands",
		summary: "Stop a command",
		doc: "Stop a queued or running command. A queued one never starts. A running one's whole process tree gets SIGTERM, then SIGKILL after 5 seconds, once its runner hears the stop (within 2 seconds). " +
			"It ends stopped, and its result, saying so, still comes to the asking session as a new turn. The reply comes at once. No body is read. " +
			"A turn token stops the commands asked for in the sessions it may read.",
		replies: []reply{ok(202, CommandStarted{}, "The command is stopping."), fail(404, "No such command."),
			fail(409, "The command has ended."), storeFailed},
		at:    atCommand,
		serve: (*API).stopCommand},
}
