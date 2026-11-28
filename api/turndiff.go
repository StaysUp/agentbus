package api

// A turn's diff summary and its apply: GET /api/v0/turns/{id}/diff and
// POST /api/v0/workspaces/{id}/apply. The summary is read from the record: the
// turn's turn.changed holds its file changes (listed at /changes), and its
// events its model calls (/calls) and tool calls (/tools); its turn.applied
// events hold its applies. A turn from before 0.5 has its changes and applies
// in its jobs' results. An apply is a job (queue.KindApply) that the runner
// holding the turn's session runs through its isolation backend.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// TurnDiff is the answer to GET /api/v0/turns/{id}/diff: the summary of what
// one turn did. Its lists each have their own paged route: the file changes
// at GET /api/v0/turns/{id}/changes, the model calls at .../calls, the tool
// calls at .../tools. No list is embedded here, so none is cut.
type TurnDiff struct {
	Turn       string        `json:"turn"`
	Session    string        `json:"session"`
	Agent      string        `json:"agent" doc:"The harness that ran the turn, from its turn.started."`
	AgentID    string        `json:"agent_id,omitempty" doc:"The saved agent that ran the turn, from its turn.started."`
	Model      string        `json:"model" doc:"The model the turn's last call asked for. Empty when it made no calls."`
	State      string        `json:"state" doc:"running, finished, failed or stopped, as GET /api/v0/turns/{id} says."`
	Started    time.Time     `json:"started"`
	Ended      *time.Time    `json:"ended,omitempty" doc:"Absent while running."`
	DurationMS *int64        `json:"duration_ms" doc:"From started to ended. Null while running."`
	Files      *FilesSummary `json:"files" doc:"What the turn changed, in summary: the whole list pages at GET /api/v0/turns/{id}/changes. Null when the record holds none: the turn is running, this service did not run it, or it ran in a direct workspace (sandbox false), whose changes go straight into the person's folder and are not recorded."`
	ModelCalls CallsSummary  `json:"model_calls" doc:"The turn's model calls, in summary: the list pages at GET /api/v0/turns/{id}/calls."`
	Tools      ToolsSummary  `json:"tools" doc:"The turn's tool calls, in summary: the list pages at GET /api/v0/turns/{id}/tools, each tool call with the commands it ran."`
	Apply      ApplyState    `json:"apply" doc:"Whether the turn's changes were applied to the person's folder. state none when no apply was asked for."`
}

// FilesSummary is a turn's file changes in summary, from its turn.changed.
// The list is at GET /api/v0/turns/{id}/changes.
type FilesSummary struct {
	Isolated bool   `json:"isolated" doc:"The turn ran in an isolated copy, between two checkpoints: POST /api/v0/workspaces/{id}/apply can put its changes in the person's folder."`
	From     string `json:"from,omitempty" doc:"The checkpoint the turn started from. In an isolated copy only."`
	To       string `json:"to,omitempty" doc:"The checkpoint the turn ended with. The same as from: the turn changed nothing."`
	Kind     string `json:"kind" doc:"Always snapshot: the files were compared by their SHA-256."`
	Count    int    `json:"count" doc:"The files the turn added, modified or deleted."`
}

// FileChange is one changed file.
type FileChange struct {
	Path        string  `json:"path"`
	Change      string  `json:"change" doc:"added, modified or deleted."`
	Size        int64   `json:"size" doc:"The file at the end; for a deleted file, at the start."`
	SHA256      string  `json:"sha256" doc:"At the end; for a deleted file, at the start."`
	SHA256Prior string  `json:"sha256_before,omitempty" doc:"At the turn's start. Absent for an added file, and for turns from before 0.5."`
	Diff        *string `json:"diff,omitempty" doc:"The file's unified diff, from the turn's start to its end, for small text files whose earlier content is kept."`
	DiffLeftOut bool    `json:"diff_left_out,omitempty" doc:"The content changed and no diff is here: a binary file, a link, a file over 256 KiB, past the 1 MiB of all diffs (512 KiB before 0.5), or its earlier content is no longer kept."`
	Content     *string `json:"content,omitempty" doc:"The whole file at the end, when it is small UTF-8 text."`
}

// CallsSummary is a turn's model calls in summary. The list is at
// GET /api/v0/turns/{id}/calls.
type CallsSummary struct {
	Count        int          `json:"count" doc:"The turn's model.requested events."`
	Usage        *event.Usage `json:"usage" doc:"Summed over the replies that carried usage. Null when none did."`
	CostUSD      float64      `json:"cost_usd"`
	CostComplete bool         `json:"cost_complete" doc:"False when a reply lacks a complete cost."`
	Models       []ModelCount `json:"models" doc:"Calls per model, in the order each model was first asked for."`
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

// ToolsSummary is a turn's tool calls in summary. The list, each tool call
// with the commands it ran, is at GET /api/v0/turns/{id}/tools.
type ToolsSummary struct {
	Count int `json:"count" doc:"The turn's tool.requested events."`
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

// CommandRecord is one shell command a turn ran, as its agent's tool call
// reported it: the machine half of GET /api/v0/turns/{id}/record, derived from
// the turn's tool.requested (the command line) and tool.completed (the output).
// It is what the agent asked its shell tool to run and what the tool returned,
// not an independent reading of the machine.
type CommandRecord struct {
	ID          string `json:"id" doc:"The shell tool call's id (tool.requested / tool.completed), so a command can be found in the turn's tool calls at GET /api/v0/turns/{id}/tools."`
	Line        string `json:"line" doc:"The command line the agent asked its shell tool to run."`
	Running     bool   `json:"running,omitempty" doc:"The tool call has no result yet: the command has not ended, or the turn did not record its end."`
	Error       *bool  `json:"error,omitempty" doc:"From the tool call's result: the shell tool reported the command as an error. Absent until the result came, or when the vendor did not say."`
	Output      string `json:"output,omitempty" doc:"The output the agent's shell tool returned, as the model received it, inline up to the cap. When truncated, the start is here and the whole output is on the tool call's tool.completed at GET /api/v0/sessions/{id}/events?turn={turn}."`
	OutputBytes int64  `json:"output_bytes,omitempty" doc:"The returned output's size in bytes."`
	Truncated   bool   `json:"truncated,omitempty" doc:"The inline output was cut to the cap; output_bytes is the whole size."`
}

// recordOutputCap bounds a command's inline output in the record, cut on a
// rune boundary. The whole output is on the turn's tool.completed. (Keeping it
// as its own rooted, readable blob is a follow-up: framework/TODO.md.)
const recordOutputCap = 16 << 10

// NetworkRefusal is one host a closed-off turn was refused, from a
// network.refused.
type NetworkRefusal struct {
	Host  string `json:"host" doc:"The host and port the turn asked for, such as example.com:443."`
	Error string `json:"error,omitempty" doc:"Why the policy refused it."`
}

// TurnRecord is the answer to GET /api/v0/turns/{id}/record: a turn's two
// halves in one read. The machine half leads, in review order — the files it
// changed, then the commands it ran, then the network it was refused — and the
// agent half follows: its model calls and their cost. Unlike the diff summary,
// the lists are embedded: the files are the changeset without per-file diffs
// (those page at /changes), and each command's output is kept to a cap (the
// whole output is on the tool call's tool.completed in the turn's events). The
// commands are what the agent asked its shell tool to run, not an independent
// reading of the machine.
type TurnRecord struct {
	Store   string `json:"store" doc:"The store these came from, for streaming more from GET /api/v0/events."`
	Seq     int64  `json:"seq" doc:"The store's last seq when this was read."`
	Turn    string `json:"turn"`
	Session string `json:"session"`
	Agent   string `json:"agent" doc:"The harness that ran the turn."`
	State   string `json:"state" doc:"running, finished, failed or stopped."`
	// The machine half, files first.
	Files          []FileChange     `json:"files" doc:"The files the turn changed, the changeset without per-file diffs. Empty for a turn with no checkpoint (a direct-mode turn, which records commands only), or while it runs."`
	FilesTruncated bool             `json:"files_truncated,omitempty" doc:"The file list was cut; the whole list pages at GET /api/v0/turns/{id}/changes."`
	Commands       []CommandRecord  `json:"commands" doc:"Each shell command the turn ran, oldest first, with its output, as the agent's tool calls reported them. A tool call the gateway refused is left out: it never ran."`
	Network        []NetworkRefusal `json:"network" doc:"Each host the turn was refused when closed off from the network, oldest first."`
	// The agent half.
	ModelCalls CallsSummary `json:"model_calls" doc:"The turn's model calls, in summary with their cost; the list pages at GET /api/v0/turns/{id}/calls."`
}

// ApplyState is an apply of a turn: the answer to POST /api/v0/workspaces/{id}/apply,
// and apply in GET /api/v0/turns/{id}/diff.
type ApplyState struct {
	State     string                `json:"state" doc:"none (never asked for), queued, running, applied, refused (a file in your folder changed, or the turn cannot be applied: error says why), or failed (the apply did not finish)."`
	Turn      string                `json:"turn,omitempty"`
	Session   string                `json:"session,omitempty"`
	Turns     []string              `json:"turns,omitempty" doc:"Every turn whose changes the apply carried, oldest first: the session's turns after its previous apply, up to turn. Absent for an apply from before 0.5."`
	By        *event.By             `json:"by,omitempty" doc:"Who asked for the apply: the person or an agent. Absent for an apply from before 0.5."`
	Already   bool                  `json:"already,omitempty" doc:"The turn was applied before this request: nothing more was written, and files are what that apply wrote."`
	Files     []string              `json:"files,omitempty" doc:"The files the apply wrote or removed in the person's folder, sorted."`
	Count     int                   `json:"count,omitempty" doc:"How many files the apply wrote or removed. files may list fewer, with truncated."`
	Truncated bool                  `json:"truncated,omitempty" doc:"files or conflicts was cut to keep the answer small."`
	Pruned    bool                  `json:"pruned,omitempty" doc:"The checkpoints before the turn's end were removed."`
	Conflicts []queue.ApplyConflict `json:"conflicts,omitempty" doc:"On a refusal: each file in your folder that is neither as it was at the turn's start nor as the turn left it, with why. Nothing was written."`
	Error     string                `json:"error,omitempty" doc:"Why it was refused or failed."`
	Event     string                `json:"event,omitempty" doc:"The id the record gives the apply's outcome, workspace.applied, from the moment it is queued: an apply's route answers 202 naming it, and GET /api/v0/workspaces/{id}/outcome?event= reads it once the apply has run. There it holds the result, the files written, and on a refusal each file that refused it with why."`
}

// maxToolInput: a tool call's input is left out past this, with input_left_out.
const maxToolInput = 4 << 10

// activityPageLimit is the events a page of buildActivity reads, 1000 in
// production. A test lowers it to reach many pages with few events, since
// buildActivity now reads every page, with no cap on their number.
var activityPageLimit = 1000

// GET /api/v0/turns/<id>/diff: one turn's summary; its lists page elsewhere.
func (a *API) turnDiff(w http.ResponseWriter, r *http.Request, id string) {
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	d := TurnDiff{Turn: t.ID, Session: t.Session, Agent: t.Harness, AgentID: t.AgentID, Model: t.Model, State: t.State,
		Started: t.Started, Ended: t.Ended, Apply: ApplyState{State: "none"},
		ModelCalls: CallsSummary{Count: t.ModelCalls, Usage: t.Usage, CostUSD: t.CostUSD, CostComplete: t.CostComplete,
			Models: []ModelCount{}}}
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
				if j.Ended != nil {
					c, err := jobs.Changes(a.cfg.Store, j)
					if err != nil {
						a.internal(w)
						return
					}
					d.Files = filesSummary(c)
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
	act, err := a.buildActivity(d.Turn, d.Session)
	if err != nil {
		a.internal(w)
		return
	}
	d.ModelCalls.Models = act.Models
	d.Tools.Count = len(act.Tools)
	writeJSON(w, http.StatusOK, d)
}

// GET /api/v0/turns/{id}/record: a turn's two halves in one read, read from
// the one log. The machine half leads — the files it changed (a sandbox
// checkpoint; a direct-mode turn has none), then the commands it ran with
// their output, then the network it was refused — and the agent half follows.
func (a *API) turnRecord(w http.ResponseWriter, r *http.Request, id string) {
	// The reads below settle state, files and the seq cursor. Take them in an
	// order that cannot tear: sample seq first, so the cursor a client streams
	// from never skips an event left out of the state here; then read the job
	// before the turn, so once the job has ended (and its run.ended is
	// stored) the turn read reflects it — the record can never read running
	// with files already filled.
	seq := a.cfg.Store.Last()
	var j queue.Job
	haveJob := false
	if a.cfg.Queue != nil {
		jj, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		switch {
		case err == nil:
			j, haveJob = jj, true
		case !errors.Is(err, queue.ErrNotFound):
			a.internal(w)
			return
		}
	}
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	rec := TurnRecord{Store: a.cfg.Store.ID(), Seq: seq,
		Turn: t.ID, Session: t.Session, Agent: t.Harness, State: t.State,
		Files: []FileChange{}, Commands: []CommandRecord{}, Network: []NetworkRefusal{},
		ModelCalls: CallsSummary{Count: t.ModelCalls, Usage: t.Usage, CostUSD: t.CostUSD,
			CostComplete: t.CostComplete, Models: []ModelCount{}}}
	// Running until the job has ended, unless it was stopped, as
	// GET /api/v0/turns/{id} and /diff say. Files are the turn's changeset, but
	// only an isolated one: a sandbox checkpoint. A direct-mode turn records
	// commands only, so its (non-isolated) changeset is not shown as files.
	if haveJob {
		res, _ := queue.ReadResult(j.Result)
		if j.Session == t.Session || res.Session == t.Session {
			if j.Ended == nil {
				if rec.State != queue.TurnStopped {
					rec.State = queue.TurnRunning
				}
			} else if c, err := jobs.Changes(a.cfg.Store, j); err != nil {
				a.internal(w)
				return
			} else if c != nil && c.Isolated {
				changes, truncated, _, _, _ := fileChanges(c)
				// The per-file diffs and content page at /changes; the record
				// lists the changeset itself, so it stays bounded.
				for i := range changes {
					changes[i].Diff, changes[i].Content = nil, nil
				}
				if changes != nil {
					rec.Files = changes
				}
				rec.FilesTruncated = truncated
			}
		}
	}
	act, err := a.buildActivity(t.ID, t.Session)
	if err != nil {
		a.internal(w)
		return
	}
	rec.Commands, rec.Network, rec.ModelCalls.Models = act.Commands, act.Network, act.Models
	writeJSON(w, http.StatusOK, rec)
}

// filesSummary is a turn's file changes in summary, for the diff. nil for nil.
func filesSummary(c *event.TurnChanges) *FilesSummary {
	if c == nil {
		return nil
	}
	return &FilesSummary{Isolated: c.Isolated, From: c.From, To: c.To, Kind: workspace.KindSnapshot, Count: c.Count}
}

// fileChanges is a turn's changes as the changes route lists them: the full
// list and the record's completeness flags. nil for nil.
func fileChanges(c *event.TurnChanges) ([]FileChange, bool, bool, bool, string) {
	if c == nil {
		return nil, false, false, false, ""
	}
	out := make([]FileChange, len(c.Files))
	for i, ch := range c.Files {
		out[i] = FileChange{Path: ch.Path, Change: ch.Change, Size: ch.Size, SHA256: ch.SHA256, SHA256Prior: ch.SHA256Before,
			Diff: ch.Diff, DiffLeftOut: ch.DiffLeftOut, Content: ch.Content}
	}
	return out, c.Truncated, c.DiffsCut, c.ContentLeftOut, c.Error
}

// activity is a turn's model calls, tool calls and per-model counts, read
// whole from its events: no cap, so a list route pages them and the summary
// counts them. Models is an aggregate; Calls and Tools are the full lists,
// oldest first (each tool call carries its own commands).
type activity struct {
	Models   []ModelCount
	Calls    []ModelCall
	Tools    []ToolCall
	Commands []CommandRecord  // the machine half: each shell command the turn ran
	Network  []NetworkRefusal // hosts a closed-off turn was refused
}

// noCmdSlot is the FIFO slot a non-shell tool call reserves: it pairs its
// result but has no command record.
const noCmdSlot = -1

// cmdDone is a tool call's result, already cut to the inline cap, waiting in
// buildActivity to pair with the tool.requested of the same tool id. It holds
// only the capped prefix, so a result stored before its request never pins a
// large output.
type cmdDone struct {
	output    string // the inline prefix, cut on a rune boundary
	bytes     int64  // the whole result's size
	truncated bool
	err       *bool
}

// doneOf caps a result's output for the inline record.
func doneOf(output string, err *bool) cmdDone {
	cut, truncated := capRunes(output, recordOutputCap)
	return cmdDone{output: cut, bytes: int64(len(output)), truncated: truncated, err: err}
}

// buildActivity reads a turn's model calls, tool calls and commands from its
// events, every one: it pages to the end, so a later call after many tool
// events is counted and listed, never dropped by a page cap.
func (a *API) buildActivity(turn, session string) (*activity, error) {
	f := store.Filter{Turn: turn, TurnSession: session}
	out := &activity{Models: []ModelCount{}, Calls: []ModelCall{}, Tools: []ToolCall{},
		Commands: []CommandRecord{}, Network: []NetworkRefusal{}}
	callIdx := map[string]int{} // exchange -> index in Calls
	models := map[string]int{}
	toolIdx := map[string]int{} // tool id -> index in Tools
	// A tool call's tool.requested (its line) and tool.completed (its result)
	// can be stored in either order, and a tool id can repeat (plain Gemini
	// parallel run_command calls share the name as their id). So EVERY tool
	// call reserves a slot in a per-id FIFO, and request and result pair
	// one-to-one, oldest to oldest, whatever the order: a shell call's slot is
	// its command's index, any other call's slot is noCmdSlot. cmdWaiting holds
	// reserved slots awaiting a result; doneWaiting holds results awaiting their
	// call. A result is capped before it waits, so an out-of-order one never
	// pins a large output.
	cmdWaiting := map[string][]int{}
	doneWaiting := map[string][]cmdDone{}
	applyDone := func(slot int, d cmdDone) {
		if slot == noCmdSlot {
			return // a non-shell call: its result is not a command
		}
		c := &out.Commands[slot]
		c.Running, c.Error, c.Output, c.OutputBytes, c.Truncated = false, d.err, d.output, d.bytes, d.truncated
	}
	// The gateway stores a call's tool.refused before its tool.requested,
	// so a refusal waits here for its call.
	refused := map[string]string{}
	var after int64
	for {
		recs, more, err := a.cfg.Store.Page(after, f, activityPageLimit, store.PageBytes)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			after = rec.Seq
			switch rec.Kind {
			case event.ModelRequested, event.ModelResponded, event.ToolRequested, event.ToolCompleted,
				event.ToolRefused, event.NetworkRefused:
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
					out.Models[i].Calls++
				} else {
					models[e.Model] = len(out.Models)
					out.Models = append(out.Models, ModelCount{Model: e.Model, Calls: 1})
				}
				callIdx[e.Exchange] = len(out.Calls)
				out.Calls = append(out.Calls, ModelCall{Exchange: e.Exchange, Model: e.Model})
			case event.ModelResponded:
				if i, ok := callIdx[e.Exchange]; ok {
					c := &out.Calls[i]
					c.Status, c.Usage, c.CostUSD, c.LatencyMS = e.Status, e.Usage, e.CostUSD, e.LatencyMS
				}
			case event.ToolRequested:
				cmds := gateway.ToolCommands(e.Vendor, e.ToolName, e.ToolInput)
				tc := ToolCall{ID: e.ToolID, Name: e.ToolName, Commands: cmds, RefusedRule: refused[e.ToolID]}
				if len(e.ToolInput) <= maxToolInput {
					tc.Input = e.ToolInput
				} else {
					tc.InputCut = true
				}
				toolIdx[e.ToolID] = len(out.Tools)
				out.Tools = append(out.Tools, tc)
				// Every tool call reserves a FIFO slot so request and result pair
				// one-to-one. A shell call's slot is its command record (running
				// until its result comes; dropped at the end if it was refused —
				// it never ran); any other call's slot is noCmdSlot.
				slot := noCmdSlot
				if len(cmds) > 0 {
					slot = len(out.Commands)
					out.Commands = append(out.Commands, CommandRecord{
						ID: e.ToolID, Line: strings.Join(cmds, "\n"), Running: true})
				}
				if q := doneWaiting[e.ToolID]; len(q) > 0 {
					applyDone(slot, q[0]) // its result came first
					doneWaiting[e.ToolID] = q[1:]
				} else {
					cmdWaiting[e.ToolID] = append(cmdWaiting[e.ToolID], slot)
				}
			case event.ToolCompleted:
				if i, ok := toolIdx[e.ToolID]; ok && e.ToolError != nil {
					out.Tools[i].Error = e.ToolError
				}
				if q := cmdWaiting[e.ToolID]; len(q) > 0 {
					applyDone(q[0], doneOf(e.ToolOutput, e.ToolError))
					cmdWaiting[e.ToolID] = q[1:]
				} else {
					// Its tool.requested is not in yet (a result stored first);
					// hold the capped result until it arrives.
					doneWaiting[e.ToolID] = append(doneWaiting[e.ToolID], doneOf(e.ToolOutput, e.ToolError))
				}
			case event.ToolRefused:
				if i, ok := toolIdx[e.ToolID]; ok {
					out.Tools[i].RefusedRule = e.Rule
				}
				// Mark it refused whichever order it arrives in; the command it
				// would be is dropped at the end. A refusal with no id (a refused
				// MCP tool) names no call, so it marks none.
				if e.ToolID != "" {
					refused[e.ToolID] = e.Rule
				}
			case event.NetworkRefused:
				out.Network = append(out.Network, NetworkRefusal{Host: e.Host, Error: e.Error})
			}
		}
		if !more {
			break
		}
	}
	// Drop the commands of refused tool calls: the gateway stopped them, so
	// they never ran. A refusal can arrive before or after its tool.requested,
	// so this filters once at the end.
	if len(refused) > 0 {
		kept := out.Commands[:0]
		for _, c := range out.Commands {
			if _, no := refused[c.ID]; !no {
				kept = append(kept, c)
			}
		}
		out.Commands = kept
	}
	return out, nil
}

// capRunes returns s cut to at most max bytes on a rune boundary, and whether
// it was cut, so the inline output is never invalid UTF-8.
func capRunes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Refusals of an apply. Fixed wording.
const (
	applyNotApplicable = "this turn's changes cannot be applied: it did not run in an isolated copy of a folder"
	applyNotEnded      = "this turn has not ended; apply it once it has"
	applyNeverStarted  = "this turn ended before it started, so it has no changes to apply"
	applySessionClosed = "this turn's session was closed, so its copy and checkpoints are gone"
	applyNoJob         = "no turn of this service has this id"
	applyBusy          = "an apply of an earlier turn of this session has not ended; ask again once it has"
	applyOffline       = "the folder's machine is offline, so nothing was written: start agentbus runner there, then ask again"
)

// POST /api/v0/workspaces/<id>/apply: put the turn's changes into the person's
// folder, through the runner that holds its session.
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
	st, err := a.applyStateOf(j)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// applyStateOf is an apply job as the API shows it: its state from the
// job, and once it has ended, what it did from its turn.applied.
func (a *API) applyStateOf(j queue.Job) (ApplyState, error) {
	st := applyHead(j)
	switch {
	case j.State == queue.Queued:
		st.State = "queued"
		return st, nil
	case j.Ended == nil:
		st.State = "running"
		return st, nil
	}
	ap, err := jobs.Applied(a.cfg.Store, j)
	if err != nil {
		return st, err
	}
	st.State, st.Error, st.Turns, st.By = ap.Result, ap.Error, ap.Turns, ap.By
	if len(j.Links.AppliesTurns) == 0 {
		st.Turns = nil // an apply from before 0.5 never named them
	}
	st.Files, st.Count, st.Truncated, st.Pruned = ap.Files, ap.Count, ap.Truncated, ap.Pruned
	for _, c := range ap.Conflicts {
		st.Conflicts = append(st.Conflicts, queue.ApplyConflict{Path: c.Path, Why: c.Why, Markers: c.Markers})
	}
	return st, nil
}

// applyHead is what an apply's answer says of its job whatever its state:
// the turns it applies, who asked, and the event that will be its outcome.
func applyHead(j queue.Job) ApplyState {
	st := ApplyState{Turn: j.Links.AppliesTurn, Session: j.Session, Turns: j.Links.AppliesTurns, Event: jobs.OutcomeID(j)}
	if b := j.Links.ApplyBy; b != nil {
		st.By = &event.By{Kind: b.Kind, Session: b.Session, Turn: b.Turn}
	}
	return st
}

// askedBy is who sent request r, as an apply's turn.applied names it: an
// agent (a turn's token, with its session and turn), else the person (the
// main token). Never the token.
func askedBy(r *http.Request) *queue.By { return callerOf(r).actor() }
