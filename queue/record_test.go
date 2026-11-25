package queue

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/applied"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/store"
)

// withRecord gives q a real record to read a workspace's applied state from,
// and stands in for control's finisher (jobs.keepOutcomes), which these tests
// do not run: before each read it writes the outcome of every ended job of the
// workspace not yet in the record, in the order the jobs ended, as the
// finisher would have. The fold itself is the store's own, the one control
// reads (applied.State).
func withRecord(t *testing.T, q *Queue) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	q.ReadAppliedWith(func(ws ids.WorkspaceID) (applied.State, error) {
		recordOutcomes(t, q, st, ws)
		return st.Applied(ws)
	})
	return st
}

// recordOutcomes writes the outcomes of ws's ended turn, apply, discard and
// update jobs into st, the way jobs.keepOutcomes does: ids fixed by the job, so
// each is written once.
func recordOutcomes(t *testing.T, q *Queue, st *store.Store, ws ids.WorkspaceID) {
	t.Helper()
	rows, err := q.db.Query(`SELECT id FROM jobs WHERE team = ? AND `+jobWorkspace+` = ?
		AND kind IN ('turn','apply','discard','update','close') AND ended IS NOT NULL ORDER BY ended, rowid`, DefaultTeam, string(ws))
	if err != nil {
		t.Fatal(err)
	}
	var jobIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		jobIDs = append(jobIDs, id)
	}
	rows.Close()
	now := time.Unix(0, 0).UTC()
	for _, id := range jobIDs {
		j, err := q.Get(DefaultTeam, id)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := ReadResult(j.Result)
		var e event.Event
		switch j.Kind {
		case KindTurn:
			// Every ended turn's changes are recorded, whatever its state.
			if j.Turn == "" || j.Session == "" || j.Attempt == "" {
				continue
			}
			if _, err := st.AppendOnce(event.Event{ID: "started_" + j.ID, Kind: event.TurnStarted, Time: now, Session: j.Session, Turn: j.Turn,
				Workspace: string(ws)}); err != nil {
				t.Fatal(err)
			}
			e = event.Event{ID: "evt_tc_" + j.ID, Kind: event.TurnChanged, Time: now, Session: j.Session, Turn: j.Turn,
				Changes: &event.TurnChanges{Isolated: true, To: r.Checkpoint, Files: []event.ChangedFile{}}, Changeset: r.Changeset, Checkpoint: r.Checkpoint}
		case KindApply:
			if j.Links.AppliesTurn == "" || j.Attempt == "" { // never ran: no outcome
				continue
			}
			res := "refused"
			if j.State == Finished {
				res = "applied"
			}
			e = event.Event{ID: "evt_ta_" + j.ID, Kind: event.WorkspaceApplied, Time: now, Workspace: string(ws), Session: j.Session,
				Turn: j.Links.AppliesTurn, Applied: &event.Applied{Turns: j.Links.AppliesTurns, From: j.Spec.ApplyFrom, To: j.Spec.ApplyTo, Result: res}}
		case KindDiscard:
			if j.State != Finished {
				continue
			}
			e = event.Event{ID: "evt_wd_" + j.ID, Kind: event.WorkspaceDiscarded, Time: now, Workspace: string(ws), Session: j.Session,
				Discarded: &event.Discarded{ResetTo: j.Spec.Restore, DroppedTurns: j.Links.AppliesTurns}}
		case KindClose:
			if j.State != Finished {
				continue
			}
			e = event.Event{ID: "evt_wx_" + j.ID, Kind: event.WorkspaceClosed, Time: now, Workspace: string(ws)}
		case KindUpdate:
			if j.State != Finished {
				continue
			}
			u := &event.Updated{Result: "conflict"}
			if r.Update.Clean() {
				u = &event.Updated{Result: "updated", From: r.Update.From, To: r.Update.To, FoldedTurns: j.Links.AppliesTurns,
					HeadTurn: j.Links.AppliesTurn, TheirsCheckpoint: r.Update.TheirsCheckpoint, MergedCheckpoint: r.Update.MergedCheckpoint,
					Unresolved: r.Update.Unresolved}
			}
			e = event.Event{ID: "evt_wu_" + j.ID, Kind: event.WorkspaceUpdated, Time: now, Workspace: string(ws), Session: j.Session, Updated: u}
		}
		if _, err := st.AppendOnce(e); err != nil {
			t.Fatal(err)
		}
	}
}
