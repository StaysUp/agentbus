package jobs

// turn.ended: control's one record that a turn job ended, whatever its outcome.
// A reactor (the delivery, queue.FireOns) reads it to learn a turn ended from
// the record rather than from the scheduler's jobs table (C0, DESIGN_C0 §1).
//
// It is written in two places, both idempotent by the job's id (endedID), so
// the two never store a second and a crash loses no delivery:
//   - the finisher, as it ends a turn job (endAccepted), for the prompt path;
//   - the sweep's reconciler (RecordTurnEnds), at startup and each tick, for
//     the ends that do not go through the finisher — a lost runner, a cancel —
//     and for any the finisher wrote to jobs.db but could not append before a
//     stop. jobs.db and the record are two files, so the reconciler closes that
//     window: it walks ended turn jobs and appends any turn.ended still missing.

import (
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// appender is the slice of the record turn.ended needs: one idempotent append.
type appender interface {
	AppendOnce(e event.Event) (bool, error)
}

// endedID is the fixed id of a turn job's turn.ended, so a write done twice
// (the finisher and the reconciler, or a retry) stores it once.
func endedID(jobID string) string { return "evt_tn_" + jobID }

// recordTurnEnded appends turn.ended for turn job j ending in state (one of
// finished, failed, stopped) with reason, at now. It is a no-op for a job that
// is not a turn or never got a turn id (nothing to deliver). AppendOnce makes
// it idempotent by endedID; stored says whether this call wrote it.
//
// turn.ended carries NO session, on purpose: it is control's reactor-trigger
// record, keyed on the turn, not part of a session's event stream. A session is
// established by run.started / turn.started, never by a turn.ended. So a
// turn refused before it ever ran (no such session, missing agent) cannot
// conjure a phantom session or shift a real one's snapshot, because every
// session-derived view keys on the session and this event has none (review P1,
// 2026-11-19). FireOns finds it by turn (store.TurnEnded), which needs no
// session.
func recordTurnEnded(rec appender, j queue.Job, state, reason string, now time.Time) (stored bool, err error) {
	if j.Kind != queue.KindTurn || j.Turn == "" {
		return false, nil
	}
	return rec.AppendOnce(event.Event{ID: endedID(j.ID), Kind: event.TurnEnded, Time: now,
		Turn: j.Turn, State: state, Error: reason})
}

// TurnEndedGrace is how long the reconciler leaves a just-ended turn to the
// finisher's own prompt turn.ended before it would write one itself. It only
// backfills ends older than this (and the ends the finisher never writes: a
// lost runner, a cancel), so the two writers never race on the same job and the
// reconciler never writes turn.ended in the window of an unrelated request.
const TurnEndedGrace = 5 * time.Second

// RecordTurnEnds appends turn.ended for ended turn jobs that ended at or before
// atOrBefore, oldest end first, from after (nil to start at the first). It
// returns how many it stored, and the last ended turn job it reached, to pass
// back as after next time: the sweep walks forward, and startup (after nil)
// walks every ended turn job once. It is idempotent — a job whose turn.ended is
// already in the record is appended again as a no-op (AppendOnce), so a
// repeated or reset after costs a lookup, never a double — which is why it is
// safe as the backstop for the finisher's window and for ends that never reach
// the finisher. atOrBefore is the finisher's head start (TurnEndedGrace).
func RecordTurnEnds(q *queue.Queue, rec appender, after *queue.Job, atOrBefore time.Time, limit int) (int, *queue.Job, error) {
	list, err := q.EndedTurns(after, atOrBefore, limit)
	if err != nil {
		return 0, after, err
	}
	n := 0
	last := after
	for i := range list {
		j := list[i]
		stored, err := recordTurnEnded(rec, j, queue.TurnState(j.State), j.Reason, endedAt(j))
		if err != nil {
			return n, last, err
		}
		if stored {
			n++
		}
		last = &list[i]
	}
	return n, last, nil
}
