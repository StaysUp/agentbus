package api

// POST /api/v0/sessions/{id}/commands: the person runs a shell command in a
// session's copy, on the runner that holds it, with no model call: the "!" of
// a chat. It is a command job (queue.KindCommand) in the session's lane, as a
// turn is. The route never waits: it stores command.queued and answers with
// the command's id; the record says the rest (jobs/command.go). Only the main
// token runs one: it is the person's command, not an agent's, so the agents'
// allow and forbid lists do not apply. The policy's commands switch turns
// them all off.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// CommandRequest is the body of POST /api/v0/sessions/{id}/commands.
type CommandRequest struct {
	Command        string `json:"command" doc:"The command line, run by /bin/sh -c in the session's copy: 1 byte to 8 KiB, with no NUL byte."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" doc:"The time limit, 1 to 600 seconds. 120 when absent. Past it the command and everything it started are ended."`
}

// CommandQueued is the answer to POST /api/v0/sessions/{id}/commands.
type CommandQueued struct {
	Session string `json:"session"`
	Command string `json:"command" doc:"The command's id. Its events (command.queued, command.started, then command.ran or command.failed) carry it as command.id."`
	State   string `json:"state" doc:"queued: it waits in the session's lane, behind what runs there."`
}

// The refusals of a command, in plain words.
const (
	commandNoCopy   = "this session has no copy on a runner yet; wait for its first turn to end"
	commandClosed   = "this session is closed; it runs no more commands"
	commandsOff     = "commands are off in the settings (commands = false); turn them on in the permissions to run one"
	commandNoPolicy = "the permissions cannot be used, so no command runs; fix them"
)

// POST /api/v0/sessions/{id}/commands
func (a *API) runCommand(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	if a.cfg.Settings != nil {
		p, _, err := a.cfg.Settings.Policy()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, commandNoPolicy)
			return
		}
		if !*p.Commands {
			writeError(w, http.StatusForbidden, commandsOff)
			return
		}
	}
	var body CommandRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be one JSON object with command, and optionally timeout_seconds")
		return
	}
	if msg := run.CheckCommand(body.Command); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if body.TimeoutSeconds < 0 || time.Duration(body.TimeoutSeconds)*time.Second > run.MaxCommandTimeout {
		writeError(w, http.StatusBadRequest, "timeout_seconds must be from 1 to 600")
		return
	}
	ws, ok, err := a.cfg.Queue.Workspace(queue.DefaultTeam, id)
	if err != nil {
		a.internal(w)
		return
	}
	if !ok {
		known, err := a.cfg.Store.Known(id)
		switch {
		case err != nil:
			a.internal(w)
		case known:
			writeError(w, http.StatusConflict, commandNoCopy)
		default:
			writeError(w, http.StatusNotFound, "no such session")
		}
		return
	}
	if ws.Closed != nil {
		writeError(w, http.StatusConflict, commandClosed)
		return
	}
	cmdID := queue.NewID("cmd_")
	spec := queue.Spec{Session: id, CommandID: cmdID, Command: body.Command, TimeoutMS: int64(body.TimeoutSeconds) * 1000}
	// command.queued first: every end the record stores for the job then
	// follows it.
	if _, _, err := a.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.CommandQueued, Time: a.cfg.Now().UTC(), Session: id,
		Command: &event.Command{ID: cmdID, Line: body.Command}}); err != nil {
		a.internal(w)
		return
	}
	j, _, err := a.cfg.Queue.Create(queue.DefaultTeam, queue.KindCommand, "", spec, queue.Links{})
	if err != nil {
		// No job: the record says it did not run.
		j = queue.Job{ID: "nojob_" + cmdID, Session: id, Spec: spec, State: queue.Failed}
		status, why := http.StatusInternalServerError, "the queue could not be written"
		switch {
		case errors.Is(err, queue.ErrSessionClosed):
			status, why = http.StatusConflict, commandClosed
		case errors.Is(err, queue.ErrFull):
			status, why = http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end and retry"
		}
		j.Result, _ = json.Marshal(queue.Result{Refused: why})
		if err := jobs.CommandEnded(a.cfg.Store, j, nil); err != nil {
			a.internal(w)
			return
		}
		writeError(w, status, why)
		return
	}
	writeJSON(w, http.StatusAccepted, CommandQueued{Session: j.Session, Command: cmdID, State: queue.TurnQueued})
}
