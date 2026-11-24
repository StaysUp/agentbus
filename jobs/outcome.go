package jobs

// A job's outcomes in control's record (from 0.5): jobs.db holds what is
// needed to run work, events.db holds each outcome once. When a runner
// sends a job's finish, control saves it (queue.Accept); its one finisher
// then puts what the job did into the record, as an event (turn.changed,
// turn.applied), and then ends the job
// keeping only that event's id (queue.Result's *Event pointers): the
// record first, the finish second, so "finished" means "its outcome is in
// the record" (keepOutcomes). Readers read the outcome from the record by
// that id; a job from before 0.5 has no pointer and keeps its outcome in
// its result, which is read as it is. Nothing older is moved or rewritten.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
func discardID(j queue.Job) string { return "evt_wd_" + j.ID }
func updateID(j queue.Job) string  { return "evt_wu_" + j.ID }

// registeredID is the id of a folder's folder.registered, by its reference and
// machine rather than by the job, so the many folder jobs of one folder (one
// per workspace that types it) record it once.
func registeredID(ref, machine string) string {
	sum := sha256.Sum256([]byte(ref + "\x00" + machine))
	return "evt_fr_" + hex.EncodeToString(sum[:8])
}

// DiscardedOf is what a discard job dropped, from the job alone: the checkpoint
// its copy was reset to (the job's Restore, or "base" when none was applied),
// the unapplied turns it dropped (its AppliesTurns), and who asked (ApplyBy).
func DiscardedOf(j queue.Job) *event.Discarded {
	d := &event.Discarded{ResetTo: j.Spec.Restore, DroppedTurns: j.Links.AppliesTurns}
	if d.ResetTo == "" {
		d.ResetTo = "base"
	}
	if b := j.Links.ApplyBy; b != nil {
		d.By = &event.By{Kind: b.Kind, Session: b.Session, Turn: b.Turn}
	}
	return d
}

// UpdatedOf is what an update job did, from the job and its result res: the
// workspace.updated it is recorded as. A clean merge carries the new baseline
// (from) and merged (to) changesets and the folded turns; a conflict carries
// the files that conflicted and nothing else.
func UpdatedOf(j queue.Job, res queue.Result) *event.Updated {
	u := &event.Updated{}
	if b := j.Links.ApplyBy; b != nil {
		u.By = &event.By{Kind: b.Kind, Session: b.Session, Turn: b.Turn}
	}
	if res.Update.Clean() {
		u.Result = "updated"
		u.From, u.To = res.Update.From, res.Update.To
		u.FoldedTurns = j.Links.AppliesTurns
		if len(res.Update.Unresolved) > 0 {
			u.Result = "merged"
			u.Unresolved = res.Update.Unresolved
		}
	} else {
		u.Result = "conflict"
		if res.Update != nil {
			u.Conflicts = res.Update.Conflicts
			u.Unmergeable = res.Update.Unmergeable
		}
	}
	return u
}

// errOutcome: the job's outcome could not be written to the record. Its
// finish is saved (queue.Accept), so it is ended later.
var errOutcome = errors.New("jobs: the job's outcome could not be written")

// keepOutcomes writes what job j did, as finish f says, into the record,
// and answers the result the job keeps in its place: the same result with
// each outcome named by its event's id (nil when nothing moved), and, for a
// finished review, the verdict and findings it reads from the reviewer's
// reply in the record (nil for any other job). End stores the verdict.
//
// Only the finisher calls it (finisher.go), from a saved finish, and then
// ends the job: End's one jobs.db transaction is the commit point, so no
// job is finished before its outcome is in the record, and no reader ever
// sees a finished job without it. The events' ids are fixed by the job, so
// a write done twice stores each once: a stop or a failed write before End
// leaves an event and an unfinished job, and the finisher's next pass
// writes the same event (no change) and then ends the job. This is the one
// place a job's outcomes are written; every kind goes through it.
//
// j is the job before End, still running. j.Ended is not set yet, so the
// events take control's time now, a moment before End's.
func keepOutcomes(rec Recorder, j queue.Job, f queue.Finish) (kept json.RawMessage, err error) {
	now := time.Now().UTC()
	// B15: a review is a normal turn, so there is no verdict to read from the
	// reply here. Its findings are the reviewer's reply, delivered as a turn.
	j.State, j.Reason, j.Result, j.Ended = f.Status, f.Reason, f.Result, &now
	res, err := queue.ReadResult(j.Result)
	if err != nil {
		return nil, nil // not ours to read; End takes it as it is
	}
	moved := false
	switch j.Kind {
	case queue.KindTurn:
		if res.Changes == nil || j.Turn == "" {
			break
		}
		session := assignedSession(j)
		if session == "" {
			break
		}
		e := event.Event{ID: changedID(j), Kind: event.TurnChanged, Time: now, Session: session, Turn: j.Turn,
			Changes: EventChanges(res.Changes)}
		if _, err := rec.AppendOnce(e); err != nil {
			return nil, err
		}
		res.Checkpoint = res.Changes.To
		res.Changes, res.ChangesEvent, moved = nil, e.ID, true
	case queue.KindApply:
		if res.AppliedEvent != "" || j.Links.AppliesTurn == "" {
			break
		}
		// workspace.applied names the workspace the apply ran in, so a
		// workspace's applied turn resolves by workspace id even when its applying
		// session is not its own id (an asked session's apply). A job with no
		// workspace (a pre-workspace apply) keys it by its session, its workspace.
		ws := queue.CopyKey(j.Spec.Workspace, j.Session)
		e := event.Event{ID: appliedID(j), Kind: event.WorkspaceApplied, Time: now, Workspace: string(ws), Session: j.Session,
			Turn: j.Links.AppliesTurn, Applied: AppliedOf(j, res)}
		if _, err := rec.AppendOnce(e); err != nil {
			return nil, err
		}
		res.Apply, res.Refused, res.AppliedEvent, moved = nil, "", e.ID, true
	case queue.KindDiscard:
		if res.DiscardedEvent != "" || j.State != queue.Finished {
			break
		}
		// workspace.discarded names the workspace whose copy was reset, keyed by
		// the workspace id, so a workspace's discard resolves by it even when the
		// discarding job has no session of its own.
		ws := queue.CopyKey(j.Spec.Workspace, j.Session)
		e := event.Event{ID: discardID(j), Kind: event.WorkspaceDiscarded, Time: now, Workspace: string(ws), Session: j.Session,
			Discarded: DiscardedOf(j)}
		if _, err := rec.AppendOnce(e); err != nil {
			return nil, err
		}
		res.Refused, res.DiscardedEvent, moved = "", e.ID, true
	case queue.KindFolder:
		// A folder a person typed was checked and registered on its machine's
		// runner. folder.registered records it becoming known to control,
		// keyed by its reference and machine (not the job), so repeated
		// registrations of the same folder store it once. Written here, before
		// End, it survives the request that typed the folder going away. No
		// path: it is kept off control's disk. The result is left as it is, so
		// the caller still reads the reference from it.
		if j.State != queue.Finished || !queue.FolderRefShape.MatchString(res.FolderRef) {
			break
		}
		e := event.Event{ID: registeredID(res.FolderRef, j.Spec.Machine), Kind: event.FolderRegistered,
			Time: now, FolderRef: res.FolderRef, Machine: j.Spec.Machine}
		if _, err := rec.AppendOnce(e); err != nil {
			return nil, err
		}
	case queue.KindUpdate:
		if res.UpdatedEvent != "" || j.State != queue.Finished {
			break
		}
		// workspace.updated names the workspace whose copy was merged, keyed by the
		// workspace id, so it resolves by it even when the update job has no session
		// of its own. Recorded on a clean merge and on a conflict alike.
		ws := queue.CopyKey(j.Spec.Workspace, j.Session)
		e := event.Event{ID: updateID(j), Kind: event.WorkspaceUpdated, Time: now, Workspace: string(ws), Session: j.Session,
			Updated: UpdatedOf(j, res)}
		if _, err := rec.AppendOnce(e); err != nil {
			return nil, err
		}
		res.UpdatedEvent, moved = e.ID, true
	}
	if !moved {
		return nil, nil
	}
	return json.Marshal(res)
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
			a.Conflicts = append(a.Conflicts, event.RefusedFile{Path: c.Path, Why: c.Why, Markers: c.Markers})
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
		if j.Reason == queue.ReasonRunnerLost {
			a.Error = "the apply did not finish: its runner stopped answering"
		}
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
	case !ok || (e.Kind != event.WorkspaceApplied && e.Kind != event.TurnApplied) || e.Applied == nil:
		return nil, ErrOutcomeMissing
	}
	return e.Applied, nil
}
