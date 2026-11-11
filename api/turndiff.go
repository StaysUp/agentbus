package api

// A turn's full diff and its apply: GET /api/v0/turns/{id}/diff and
// POST /api/v0/turns/{id}/apply. The diff is read from the record: the turn's
// job result holds its file changes, and its events hold its model calls,
// tool calls and commands. An apply is a job (queue.KindApply) that the
// runner holding the turn's session runs through its isolation backend.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// TurnDiff is the answer to GET /api/v0/turns/{id}/diff: everything the
// record holds about what one turn did.
type TurnDiff struct {
	Turn       string     `json:"turn"`
	Session    string     `json:"session"`
	Agent      string     `json:"agent" doc:"The harness that ran the turn, from its turn.started."`
	AgentID    string     `json:"agent_id,omitempty" doc:"The saved agent that ran the turn, from its turn.started."`
	Model      string     `json:"model" doc:"The model the turn's last call asked for. Empty when it made no calls."`
	State      string     `json:"state" doc:"running, finished, failed or stopped, as GET /api/v0/turns/{id} says."`
	Started    time.Time  `json:"started"`
	Ended      *time.Time `json:"ended,omitempty" doc:"Absent while running."`
	DurationMS *int64     `json:"duration_ms" doc:"From started to ended. Null while running."`
	Files      *TurnFiles `json:"files" doc:"What the turn changed in its session's copy, or in the folder it ran in. Null when the record holds none: the turn is running, or this service did not run it."`
	ModelCalls TurnCalls  `json:"model_calls"`
	Tools      TurnTools  `json:"tools"`
	Commands   []string   `json:"commands" doc:"The shell commands the agent asked to run, in order, as the gateway's guard reads them. A command built at run time, or run from a script, is not seen."`
	Apply      ApplyState `json:"apply" doc:"Whether the turn's changes were applied to the person's folder. state none when no apply was asked for."`
}

// TurnFiles is a turn's file changes, from its job result.
type TurnFiles struct {
	Isolated bool   `json:"isolated" doc:"The turn ran in an isolated copy, between two checkpoints: POST /api/v0/turns/{id}/apply can put its changes in the person's folder."`
	From     string `json:"from,omitempty" doc:"The checkpoint the turn started from. In an isolated copy only."`
	To       string `json:"to,omitempty" doc:"The checkpoint the turn ended with. The same as from: the turn changed nothing."`
	Kind     string `json:"kind" doc:"Always snapshot: the files were compared by their SHA-256."`
	// Count is every change; Changes holds at most workspace.MaxChanges.
	Count          int          `json:"count" doc:"Every file the turn added, modified or deleted."`
	Truncated      bool         `json:"truncated,omitempty" doc:"changes holds fewer files than count."`
	Changes        []FileChange `json:"changes" doc:"Each changed file, sorted by path."`
	ContentLeftOut bool         `json:"content_left_out,omitempty" doc:"Some small text files have no content here, to keep the result small."`
	Error          string       `json:"error,omitempty" doc:"The workspace could not be read at the end of the turn."`
}

// FileChange is one changed file.
type FileChange struct {
	Path        string  `json:"path"`
	Change      string  `json:"change" doc:"added, modified or deleted."`
	Size        int64   `json:"size" doc:"The file at the end; for a deleted file, at the start."`
	SHA256      string  `json:"sha256"`
	Diff        *string `json:"diff,omitempty" doc:"The file's unified diff, from the turn's start to its end, for small text files whose earlier content is kept."`
	DiffLeftOut bool    `json:"diff_left_out,omitempty" doc:"The content changed and no diff is here: a binary file, a link, a file over 256 KiB, past the 512 KiB budget of all diffs, or its earlier content is no longer kept."`
	Content     *string `json:"content,omitempty" doc:"The whole file at the end, when it is small UTF-8 text."`
}

// TurnCalls is a turn's model calls.
type TurnCalls struct {
	Count        int          `json:"count" doc:"The turn's model.requested events."`
	Usage        *event.Usage `json:"usage" doc:"Summed over the replies that carried usage. Null when none did."`
	CostUSD      float64      `json:"cost_usd"`
	CostComplete bool         `json:"cost_complete" doc:"False when a reply lacks a complete cost."`
	Models       []ModelCount `json:"models" doc:"Calls per model, in the order each model was first asked for."`
	Calls        []ModelCall  `json:"calls" doc:"Each call, oldest first, at most 500."`
	Truncated    bool         `json:"truncated,omitempty" doc:"calls holds the first 500 only."`
}

// ModelCount is how many calls asked for one model.
type ModelCount struct {
	Model string `json:"model"`
	Calls int    `json:"calls"`
}

// ModelCall is one model call of a turn.
type ModelCall struct {
	Exchange  string       `json:"exchange" doc:"GET /api/v0/turns/{id}/calls/{exchange} shows what the call sent."`
	Model     string       `json:"model"`
	Status    int          `json:"status,omitempty" doc:"The HTTP status of the vendor's reply. Absent until it came."`
	Usage     *event.Usage `json:"usage,omitempty"`
	CostUSD   *float64     `json:"cost_usd,omitempty"`
	LatencyMS *int64       `json:"latency_ms,omitempty"`
}

// TurnTools is a turn's tool calls.
type TurnTools struct {
	Count     int        `json:"count" doc:"The turn's tool.requested events."`
	Calls     []ToolCall `json:"calls" doc:"Each tool call, oldest first, at most 1000."`
	Truncated bool       `json:"truncated,omitempty" doc:"calls holds the first 1000 only."`
}

// ToolCall is one tool call of a turn.
type ToolCall struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input,omitempty" doc:"The tool's input as the model sent it, when it is at most 4 KiB."`
	InputCut    bool            `json:"input_left_out,omitempty" doc:"The input is over 4 KiB: GET /api/v0/sessions/{id}/events?turn={turn} has it whole."`
	Commands    []string        `json:"commands,omitempty" doc:"The shell commands the call runs, when it runs any."`
	Error       *bool           `json:"error,omitempty" doc:"From the call's tool.completed. Absent until it came, or when the vendor did not say."`
	RefusedRule string          `json:"refused_rule,omitempty" doc:"The forbid entry the gateway refused the call by."`
}

// ApplyState is an apply of a turn: the answer to POST /api/v0/turns/{id}/apply,
// and apply in GET /api/v0/turns/{id}/diff.
type ApplyState struct {
	State     string                `json:"state" doc:"none (never asked for), queued, running, applied, refused (a file in your folder changed, or the turn cannot be applied: error says why), or failed (the apply did not finish)."`
	Turn      string                `json:"turn,omitempty"`
	Session   string                `json:"session,omitempty"`
	Already   bool                  `json:"already,omitempty" doc:"The turn was applied before this request: nothing more was written, and files are what that apply wrote."`
	Files     []string              `json:"files,omitempty" doc:"The files the apply wrote or removed in the person's folder, sorted."`
	Count     int                   `json:"count,omitempty" doc:"How many files the apply wrote or removed. files may list fewer, with truncated."`
	Truncated bool                  `json:"truncated,omitempty" doc:"files or conflicts was cut to keep the answer small."`
	Pruned    bool                  `json:"pruned,omitempty" doc:"The checkpoints before the turn's end were removed."`
	Conflicts []queue.ApplyConflict `json:"conflicts,omitempty" doc:"On a refusal: each file in your folder that is neither as it was at the turn's start nor as the turn left it, with why. Nothing was written."`
	Error     string                `json:"error,omitempty" doc:"Why it was refused or failed."`
}

// Caps on what a diff lists.
const (
	maxDiffCalls = 500
	maxDiffTools = 1000
	maxToolInput = 4 << 10
	// maxDiffPages bounds the event pages read for one diff.
	maxDiffPages = 200
)

// GET /api/v0/turns/<id>/diff: one turn's full diff.
func (a *API) turnDiff(w http.ResponseWriter, r *http.Request, id string) {
	tq := store.TurnQuery{Turn: id, Limit: 1}
	if q := r.URL.Query(); q.Has("session") {
		tq.Session = q.Get("session")
		if !sessionID.MatchString(tq.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return
		}
	}
	list, err := a.cfg.Store.Turns(tq)
	if err != nil {
		a.internal(w)
		return
	}
	if len(list.Turns) == 0 {
		writeError(w, http.StatusNotFound, noTurn)
		return
	}
	t := list.Turns[0]
	d := TurnDiff{Turn: t.ID, Session: t.Session, Agent: t.Harness, AgentID: t.AgentID, Model: t.Model, State: t.State,
		Started: t.Started, Ended: t.Ended, Commands: []string{}, Apply: ApplyState{State: "none"},
		ModelCalls: TurnCalls{Count: t.ModelCalls, Usage: t.Usage, CostUSD: t.CostUSD, CostComplete: t.CostComplete,
			Models: []ModelCount{}, Calls: []ModelCall{}},
		Tools: TurnTools{Calls: []ToolCall{}}}
	if t.Ended != nil {
		ms := t.Ended.Sub(t.Started).Milliseconds()
		d.DurationMS = &ms
	}
	if a.cfg.Queue != nil {
		j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		switch {
		case err == nil:
			res, _ := queue.ReadResult(j.Result)
			if j.Session == t.Session || res.Session == t.Session {
				if j.Ended != nil && res.Changes != nil {
					d.Files = turnFiles(*res.Changes)
				}
				if j.Ended == nil && d.State != queue.TurnStopped {
					// As GET /api/v0/turns/{id}: running until its job has
					// ended, unless it was stopped.
					d.State, d.Ended, d.DurationMS = queue.TurnRunning, nil, nil
				}
				if st, err := a.applyState(id); err != nil {
					a.internal(w)
					return
				} else if st != nil {
					d.Apply = *st
				}
			}
		case !errors.Is(err, queue.ErrNotFound):
			a.internal(w)
			return
		}
	}
	if err := a.turnActivity(&d); err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// turnFiles is a job result's changes as the diff shows them.
func turnFiles(c workspace.Changes) *TurnFiles {
	f := &TurnFiles{Isolated: c.From != "" && c.To != "", From: c.From, To: c.To, Kind: c.Kind, Count: c.Count,
		Truncated: c.Truncated, Changes: make([]FileChange, len(c.Files)), ContentLeftOut: c.ContentLeftOut, Error: c.Error}
	for i, ch := range c.Files {
		f.Changes[i] = FileChange{Path: ch.Path, Change: ch.Change, Size: ch.Size, SHA256: ch.SHA256,
			Diff: ch.Diff, DiffLeftOut: ch.DiffLeftOut, Content: ch.Content}
	}
	return f
}

// turnActivity reads the turn's model calls, tool calls and commands from
// its events.
func (a *API) turnActivity(d *TurnDiff) error {
	f := store.Filter{Turn: d.Turn, TurnSession: d.Session}
	calls := map[string]int{} // exchange -> index in Calls
	models := map[string]int{}
	tools := map[string]int{} // tool id -> index in Calls
	// The gateway stores a call's tool.refused before its tool.requested,
	// so a refusal waits here for its call.
	refused := map[string]string{}
	var after int64
	for range maxDiffPages {
		recs, more, err := a.cfg.Store.Page(after, f, 1000, store.PageBytes)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			after = rec.Seq
			switch rec.Kind {
			case event.ModelRequested, event.ModelResponded, event.ToolRequested, event.ToolCompleted, event.ToolRefused:
			default:
				continue
			}
			var e event.Event
			if json.Unmarshal(rec.Data, &e) != nil {
				continue
			}
			switch e.Kind {
			case event.ModelRequested:
				if i, ok := models[e.Model]; ok {
					d.ModelCalls.Models[i].Calls++
				} else {
					models[e.Model] = len(d.ModelCalls.Models)
					d.ModelCalls.Models = append(d.ModelCalls.Models, ModelCount{Model: e.Model, Calls: 1})
				}
				if len(d.ModelCalls.Calls) >= maxDiffCalls {
					d.ModelCalls.Truncated = true
					continue
				}
				calls[e.Exchange] = len(d.ModelCalls.Calls)
				d.ModelCalls.Calls = append(d.ModelCalls.Calls, ModelCall{Exchange: e.Exchange, Model: e.Model})
			case event.ModelResponded:
				if i, ok := calls[e.Exchange]; ok {
					c := &d.ModelCalls.Calls[i]
					c.Status, c.Usage, c.CostUSD, c.LatencyMS = e.Status, e.Usage, e.CostUSD, e.LatencyMS
				}
			case event.ToolRequested:
				d.Tools.Count++
				cmds := gateway.ToolCommands(e.Vendor, e.ToolName, e.ToolInput)
				d.Commands = append(d.Commands, cmds...)
				if len(d.Tools.Calls) >= maxDiffTools {
					d.Tools.Truncated = true
					continue
				}
				tc := ToolCall{ID: e.ToolID, Name: e.ToolName, Commands: cmds, RefusedRule: refused[e.ToolID]}
				if len(e.ToolInput) <= maxToolInput {
					tc.Input = e.ToolInput
				} else {
					tc.InputCut = true
				}
				tools[e.ToolID] = len(d.Tools.Calls)
				d.Tools.Calls = append(d.Tools.Calls, tc)
			case event.ToolCompleted:
				if i, ok := tools[e.ToolID]; ok && e.ToolError != nil {
					d.Tools.Calls[i].Error = e.ToolError
				}
			case event.ToolRefused:
				if i, ok := tools[e.ToolID]; ok {
					d.Tools.Calls[i].RefusedRule = e.Rule
				} else {
					refused[e.ToolID] = e.Rule
				}
			}
		}
		if !more {
			break
		}
	}
	return nil
}

// Refusals of an apply. Fixed wording.
const (
	applyNotApplicable = "this turn's changes cannot be applied: it did not run in an isolated copy of a folder"
	applyNotEnded      = "this turn has not ended; apply it once it has"
	applyNeverStarted  = "this turn ended before it started, so it has no changes to apply"
	applySessionClosed = "this turn's session was closed, so its copy and checkpoints are gone"
	applyNoJob         = "no turn of this service has this id"
	applyBusy          = "an apply of an earlier turn of this session has not ended; ask again once it has"
	applyNoConnector   = "the folder's connector is not connected, so nothing was written: start agentbus connector on the folder's machine, then ask again"
)

// POST /api/v0/turns/<id>/apply: put the turn's changes into the person's
// folder, through the runner that holds its session.
func (a *API) applyTurn(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	j, created, err := a.cfg.Queue.Apply(queue.DefaultTeam, id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, applyNoJob)
		return
	case errors.Is(err, queue.ErrNotApplicable):
		writeError(w, http.StatusBadRequest, a.whyNotApplicable(id))
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusBadRequest, applySessionClosed)
		return
	case errors.Is(err, queue.ErrApplyBusy):
		writeError(w, http.StatusConflict, applyBusy)
		return
	case errors.Is(err, queue.ErrNoConnector):
		writeError(w, http.StatusServiceUnavailable, applyNoConnector)
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	already := !created && j.Ended != nil
	if j.Ended == nil {
		if j, err = a.awaitEnd(r.Context(), j); err != nil {
			if r.Context().Err() == nil {
				a.internal(w)
			}
			return
		}
	}
	st := applyStateOf(j)
	st.Already = already
	switch st.State {
	case "applied":
		writeJSON(w, http.StatusOK, st)
	case "queued", "running":
		writeJSON(w, http.StatusAccepted, st)
	default:
		writeJSON(w, http.StatusConflict, st)
	}
}

// whyNotApplicable says why turn id's changes cannot be applied: it has not
// ended, it never started, or it did not run in a copy.
func (a *API) whyNotApplicable(id string) string {
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
	switch {
	case err != nil || j.Kind != queue.KindTurn:
	case j.Ended == nil:
		return applyNotEnded
	case !started(j):
		return applyNeverStarted
	}
	return applyNotApplicable
}

// awaitEnd waits for job to end, up to TurnWait. It answers the job as it
// is then.
func (a *API) awaitEnd(ctx context.Context, job queue.Job) (queue.Job, error) {
	wait := a.cfg.TurnWait
	if wait == 0 {
		wait = DefaultTurnWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil || j.Ended != nil {
			return j, err
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-deadline.C:
			return j, nil
		case <-tick.C:
		}
	}
}

// applyState is the apply that put turn's changes in the folder, its own or
// a later turn's, else the newest apply of turn; nil when none was asked for.
func (a *API) applyState(turn string) (*ApplyState, error) {
	j, ok, err := a.cfg.Queue.AppliedBy(queue.DefaultTeam, turn)
	if err != nil || !ok {
		return nil, err
	}
	st := applyStateOf(j)
	return &st, nil
}

// applyStateOf is an apply job as the API shows it.
func applyStateOf(j queue.Job) ApplyState {
	st := ApplyState{Turn: j.Links.AppliesTurn, Session: j.Session}
	res, _ := queue.ReadResult(j.Result)
	if res.Apply != nil {
		st.Files, st.Pruned, st.Conflicts = res.Apply.Files, res.Apply.Pruned, res.Apply.Conflicts
		st.Count, st.Truncated = res.Apply.Count, res.Apply.Truncated
	}
	switch {
	case j.State == queue.Queued:
		st.State = "queued"
	case j.Ended == nil:
		st.State = "running"
	case j.State == queue.Finished:
		st.State = "applied"
		if st.Files == nil {
			st.Files = []string{}
		}
	case j.State == queue.Failed && (j.Reason == queue.ReasonConflict || j.Reason == queue.ReasonSetup):
		st.State, st.Error = "refused", res.Refused
	default:
		st.State = "failed"
		st.Error = "the apply did not finish: it ended " + queue.TurnState(j.State)
		if j.Reason != "" {
			st.Error += " (" + j.Reason + ")"
		}
		st.Error += ". Ask again: a file it already wrote counts as applied"
	}
	return st
}
