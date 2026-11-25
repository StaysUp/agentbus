package jobs

// The record holds a workspace's applied state from 0.6.6 (applied, store.Applied):
// a turn.changed names the turn's changeset, a workspace.applied its from and
// to, a workspace.updated its head turn and checkpoints. A record written
// before then holds only part of it, and turns and applies from before 0.5
// have no event at all: the queue's job rows held the rest. MigrateApplied
// moves it, once: for each workspace whose record is short of it, the job rows
// of that time are replayed through the one fold, in the order they ended, as
// the queue read them, and the state that comes out is written as one
// workspace.snapshot. The fold starts from it, so nothing older is read again
// and no case of an older record is bridged one by one.

import (
	"time"

	"github.com/staysup/agentbus/applied"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
)

// snapshotID is the id of a workspace's one workspace.snapshot.
func snapshotID(ws ids.WorkspaceID) string { return "evt_ws_" + string(ws) }

// MigrateApplied writes a workspace.snapshot for each workspace of the queue
// whose record is short of its applied state: a turn with a changeset whose
// turn.changed does not name it (or that has none), an apply whose
// workspace.applied has no to (or none), a clean update whose
// workspace.updated has no checkpoints (or none). It runs at control's start,
// before the finisher. A finish saved but not ended whose outcome an older
// finisher already wrote is replayed as ended: the record holds it, and the
// finisher's End to come writes no other. Idempotent: a workspace with a
// snapshot is left as it is, and one with a complete record never gets one.
// It answers how many it wrote.
func MigrateApplied(rec Recorder, q *queue.Queue) (int, error) {
	ended, err := q.OutcomeJobs(queue.DefaultTeam)
	if err != nil {
		return 0, err
	}
	pending, err := acceptedWithOutcome(rec, q)
	if err != nil {
		return 0, err
	}
	byWS := map[ids.WorkspaceID][]queue.Job{}
	var order []ids.WorkspaceID
	for _, j := range append(ended, pending...) {
		ws := queue.CopyKey(j.Spec.Workspace, j.Session)
		if ws == "" {
			continue
		}
		if _, ok := byWS[ws]; !ok {
			order = append(order, ws)
		}
		byWS[ws] = append(byWS[ws], j)
	}
	n := 0
	for _, ws := range order {
		if _, has, err := rec.EventByID("", "", snapshotID(ws)); err != nil {
			return n, err
		} else if has {
			continue
		}
		short, err := recordShort(rec, byWS[ws])
		if err != nil {
			return n, err
		}
		if !short {
			continue
		}
		sn := replay(byWS[ws]).Snapshot()
		stored, err := rec.AppendOnce(event.Event{ID: snapshotID(ws), Kind: event.WorkspaceSnapshot, Time: time.Now().UTC(),
			Workspace: string(ws), Snapshot: &sn})
		if err != nil {
			return n, err
		}
		if stored {
			n++
		}
	}
	return n, nil
}

// acceptedWithOutcome are the saved finishes not yet ended of turn, apply,
// discard and update jobs whose outcome an older finisher already wrote into
// the record (it writes the outcome, then ends the job), each as the job it
// ends as.
func acceptedWithOutcome(rec Recorder, q *queue.Queue) ([]queue.Job, error) {
	var out []queue.Job
	var after queue.Place
	for {
		page, err := q.Accepted(after, 500)
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			j := a.Job
			id, session, turn := outcomeEvent(j)
			if id == "" {
				continue
			}
			if _, has, err := rec.EventByID(session, turn, id); err != nil {
				return nil, err
			} else if !has {
				continue // the finisher writes it, after the snapshot
			}
			now := time.Now().UTC()
			j.State, j.Reason, j.Result, j.Ended = a.Finish.Status, a.Finish.Reason, a.Finish.Result, &now
			out = append(out, j)
		}
		if len(page) < 500 {
			return out, nil
		}
		after = page[len(page)-1].Place
	}
}

// outcomeEvent is the id, session and turn of the event job j's outcome is
// recorded as, or "" for a job with none of the applied state.
func outcomeEvent(j queue.Job) (id, session, turn string) {
	switch j.Kind {
	case queue.KindTurn:
		return changedID(j), assignedSession(j), j.Turn
	case queue.KindApply:
		return appliedID(j), j.Session, j.Links.AppliesTurn
	case queue.KindDiscard:
		return discardID(j), j.Session, ""
	case queue.KindUpdate:
		return updateID(j), j.Session, ""
	}
	return "", "", ""
}

// recordShort reports whether the record lacks part of what the jobs say of
// the applied state: a fact a job holds that its outcome event does not.
func recordShort(rec Recorder, jobs []queue.Job) (bool, error) {
	for _, j := range jobs {
		res, _ := queue.ReadResult(j.Result)
		id, session, turn := outcomeEvent(j)
		var e event.Event
		var has bool
		var err error
		if id != "" {
			if e, has, err = rec.EventByID(session, turn, id); err != nil {
				return false, err
			}
		}
		switch j.Kind {
		case queue.KindTurn:
			if res.Changeset != "" && (!has || e.Changeset == "") {
				return true, nil
			}
		case queue.KindApply:
			// An apply cancelled before it ran has no outcome, then as now.
			if j.Attempt != "" && j.Links.AppliesTurn != "" && (!has || e.Applied == nil || (e.Applied.To == "" && j.Spec.ApplyTo != "")) {
				return true, nil
			}
		case queue.KindDiscard:
			if j.State == queue.Finished && !has {
				return true, nil
			}
		case queue.KindUpdate:
			if j.State == queue.Finished && res.Update.Clean() && (!has || e.Updated == nil ||
				e.Updated.TheirsCheckpoint != res.Update.TheirsCheckpoint || e.Updated.MergedCheckpoint != res.Update.MergedCheckpoint ||
				e.Updated.HeadTurn != j.Links.AppliesTurn) {
				return true, nil
			}
		}
	}
	return false, nil
}

// replay is the applied state jobs add up to, given in the order they ended,
// read the way the queue read them before 0.6.6: a turn with a changeset in
// its result is appliable (a result from before 0.5 keeps its checkpoint in
// its changes); an apply that ran ended with an outcome, and one that finished
// wrote its spec's apply_to; a finished
// discard dropped its links' turns; a finished clean update folded its links'
// turns, its head its links' turn.
func replay(jobs []queue.Job) applied.State {
	var f applied.Fold
	for _, j := range jobs {
		res, _ := queue.ReadResult(j.Result)
		switch j.Kind {
		case queue.KindTurn:
			if j.Attempt == "" {
				continue
			}
			cp := res.Checkpoint
			if cp == "" && res.Changes != nil {
				cp = res.Changes.To
			}
			f.Changed(j.Turn, res.Changeset, cp)
		case queue.KindApply:
			if j.Attempt != "" && j.Links.AppliesTurn != "" { // one that never ran has no outcome
				f.Applied(j.Links.AppliesTurn, j.Spec.ApplyTo, j.State == queue.Finished)
			}
		case queue.KindDiscard:
			if j.State == queue.Finished && j.Ended != nil {
				f.Discarded(j.Ended.UTC(), j.Links.AppliesTurns)
			}
		case queue.KindUpdate:
			if j.State == queue.Finished && res.Update.Clean() {
				f.Updated(applied.UpdateInForce{From: res.Update.From, To: res.Update.To, HeadTurn: j.Links.AppliesTurn,
					TheirsCheckpoint: res.Update.TheirsCheckpoint, MergedCheckpoint: res.Update.MergedCheckpoint},
					j.Links.AppliesTurns, len(res.Update.Unresolved) > 0)
			}
		}
	}
	return f.State()
}
