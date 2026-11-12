package jobs

// A job's outcomes in control's record (from 0.5): jobs.db holds what is
// needed to run work, events.db holds each outcome once. When control
// accepts a job's finish, what the job did goes into the record as an
// event (turn.changed, turn.applied, command.ran), and the job's result
// keeps only that event's id (queue.Result's *Event pointers). Readers
// read the outcome from the record by that id; a job from before 0.5 has
// no pointer and keeps its outcome in its result, which is read as it is.
// Nothing older is moved or rewritten.

import (
	"errors"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// Finder reads one event of control's record by its id.
type Finder interface {
	EventByID(session, turn, id string) (event.Event, bool, error)
}

// ErrOutcomeMissing: a job names an outcome its record does not hold.
var ErrOutcomeMissing = errors.New("jobs: the job's outcome is not in the record")

// The ids of control's own outcome events, one per job, so a finish sent
// again stores each once.
func changedID(j queue.Job) string { return "evt_tc_" + j.ID }
func appliedID(j queue.Job) string { return "evt_ta_" + j.ID }

// keepOutcomes moves ended job j's outcomes from its result into the
// record, then has the job keep only their ids. Each step can be done
// again: the events are stored once by id, and a result already moved has
// nothing left to move. Until the second step, the result still holds the
// outcomes, so a failure between the two loses nothing. ran is a command
// job's command.ran, checked by commandRan.
func keepOutcomes(rec Recorder, q *queue.Queue, j queue.Job, ran *event.Event) error {
	res, err := queue.ReadResult(j.Result)
	if err != nil {
		return nil // not ours to read; End took it as it is
	}
	moved := false
	switch j.Kind {
	case queue.KindTurn, queue.KindReview:
		if res.Changes == nil || j.Turn == "" {
			break
		}
		session := assignedSession(j)
		if session == "" {
			break
		}
		e := event.Event{ID: changedID(j), Kind: event.TurnChanged, Time: endedAt(j), Session: session, Turn: j.Turn,
			Changes: EventChanges(res.Changes)}
		if _, err := rec.AppendOnce(e); err != nil {
			return err
		}
		res.Checkpoint = res.Changes.To
		res.Changes, res.ChangesEvent, moved = nil, e.ID, true
	case queue.KindApply:
		if res.AppliedEvent != "" || j.Links.AppliesTurn == "" {
			break
		}
		e := event.Event{ID: appliedID(j), Kind: event.TurnApplied, Time: endedAt(j), Session: j.Session,
			Turn: j.Links.AppliesTurn, Applied: AppliedOf(j, res)}
		if _, err := rec.AppendOnce(e); err != nil {
			return err
		}
		res.Apply, res.Refused, res.AppliedEvent, moved = nil, "", e.ID, true
	case queue.KindCommand:
		if err := CommandEnded(rec, j, ran); err != nil {
			return err
		}
		if len(res.Ran) > 0 && ran != nil {
			res.Ran, res.RanEvent, moved = nil, ran.ID, true
		}
	}
	if !moved {
		return nil
	}
	_, err = q.Outcomes(j.Team, j.ID, j.Attempt, res)
	return err
}

func endedAt(j queue.Job) time.Time {
	if j.Ended != nil {
		return j.Ended.UTC()
	}
	return time.Now().UTC()
}

// AppliedOf is what apply job j did, from its state and its result res as
// the runner sent it: the turn.applied it is recorded as.
func AppliedOf(j queue.Job, res queue.Result) *event.Applied {
	a := &event.Applied{Turns: j.Links.AppliesTurns}
	if b := j.Links.ApplyBy; b != nil {
		a.By = &event.By{Kind: b.Kind, Session: b.Session, Turn: b.Turn}
	}
	if len(a.Turns) == 0 {
		a.Turns = []string{j.Links.AppliesTurn}
	}
	if res.Apply != nil {
		a.Files, a.Count, a.Truncated, a.Pruned = res.Apply.Files, res.Apply.Count, res.Apply.Truncated, res.Apply.Pruned
		for _, c := range res.Apply.Conflicts {
			a.Conflicts = append(a.Conflicts, event.RefusedFile{Path: c.Path, Why: c.Why})
		}
	}
	switch {
	case j.State == queue.Finished:
		a.Result = "applied"
		if a.Files == nil {
			a.Files = []string{}
		}
	case j.State == queue.Failed && (j.Reason == queue.ReasonConflict || j.Reason == queue.ReasonSetup):
		a.Result, a.Error = "refused", res.Refused
	default:
		a.Result = "failed"
		a.Error = "the apply did not finish: it ended " + queue.TurnState(j.State)
		if j.Reason != "" {
			a.Error += " (" + j.Reason + ")"
		}
		a.Error += ". Ask again: a file it already wrote counts as applied"
	}
	return a
}

// Changes is what turn job j changed: from the record by its pointer, or
// from its result for a job from before 0.5. nil when it holds none.
func Changes(rd Finder, j queue.Job) (*event.TurnChanges, error) {
	res, err := queue.ReadResult(j.Result)
	if err != nil || res.ChangesEvent == "" {
		return EventChanges(res.Changes), nil
	}
	e, ok, err := rd.EventByID(assignedSession(j), j.Turn, res.ChangesEvent)
	switch {
	case err != nil:
		return nil, err
	case !ok || e.Kind != event.TurnChanged || e.Changes == nil:
		return nil, ErrOutcomeMissing
	}
	return e.Changes, nil
}

// EventChanges is c as the record holds it (turn.changed). nil for nil.
func EventChanges(c *workspace.Changes) *event.TurnChanges {
	if c == nil {
		return nil
	}
	out := &event.TurnChanges{Isolated: c.From != "" && c.To != "", From: c.From, To: c.To, Count: c.Count,
		Truncated: c.Truncated, Partial: c.Partial, DiffsCut: c.DiffsCut, ContentLeftOut: c.ContentLeftOut, Error: c.Error,
		Files: make([]event.ChangedFile, len(c.Files))}
	for i, f := range c.Files {
		out.Files[i] = event.ChangedFile{Path: f.Path, Change: f.Change, Size: f.Size, SHA256: f.SHA256,
			SHA256Before: f.SHA256Before, Diff: f.Diff, DiffLeftOut: f.DiffLeftOut, Content: f.Content}
	}
	return out
}

// WorkspaceChanges is c as the queue reads it (EventChanges the other way).
// nil for nil.
func WorkspaceChanges(c *event.TurnChanges) *workspace.Changes {
	if c == nil {
		return nil
	}
	out := &workspace.Changes{Kind: workspace.KindSnapshot, From: c.From, To: c.To, Count: c.Count, Truncated: c.Truncated,
		Partial: c.Partial, DiffsCut: c.DiffsCut, ContentLeftOut: c.ContentLeftOut, Error: c.Error,
		Files: make([]workspace.Change, len(c.Files))}
	for i, f := range c.Files {
		out.Files[i] = workspace.Change{Path: f.Path, Change: f.Change, Size: f.Size, SHA256: f.SHA256,
			SHA256Before: f.SHA256Before, Diff: f.Diff, DiffLeftOut: f.DiffLeftOut, Content: f.Content}
	}
	return out
}

// Applied is what apply job j did, once it has ended: from the record by
// its pointer, or from its state and result for a job from before 0.5 or
// one that ended with no finish (its runner stopped answering). nil while
// it has not ended.
func Applied(rd Finder, j queue.Job) (*event.Applied, error) {
	if j.Ended == nil {
		return nil, nil
	}
	res, err := queue.ReadResult(j.Result)
	if err != nil || res.AppliedEvent == "" {
		return AppliedOf(j, res), nil
	}
	e, ok, err := rd.EventByID(j.Session, j.Links.AppliesTurn, res.AppliedEvent)
	switch {
	case err != nil:
		return nil, err
	case !ok || e.Kind != event.TurnApplied || e.Applied == nil:
		return nil, ErrOutcomeMissing
	}
	return e.Applied, nil
}
