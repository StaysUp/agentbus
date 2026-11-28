package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// A remote turn whose agent exited 0, and whose runner finished the job, but
// whose last record batch (with its run.ended) never reached control
// (control refused it, or its runner was stopping: runner/record.go tries a
// final batch only while the attempt is live). Once the attempt has
// ended its fence refuses the run.ended, CloseLost closes only the holds of
// attempts whose lease ran out, and store.CloseStaleHolds runs only where an
// engine starts, never on a server's record. The hold must still close: the
// turn must not read running for good, nor its session refuse its next turn.
// The finisher closes it with the attempt (closeOpenRuns): failed, since
// control never got how the agent's run ended. The control case is the same
// turn with its run.ended recorded, which reads finished.
func TestEndedTurnClosesItsHold(t *testing.T) {
	for name, recorded := range map[string]bool{"run.ended recorded": true, "run.ended never arrived": false} {
		t.Run(name, func(t *testing.T) {
			e := newOutcomeEnv(t)
			if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Prompt: "add a test",
				FolderRef: "fld_0123456789abcdef", Machine: testConnector, Isolate: true}, queue.Links{}); err != nil {
				t.Fatal(err)
			}
			j, base := e.take(t, queue.KindTurn)
			hold := "hold_" + strings.Repeat("ab", 8)
			ev := func(id string, kind event.Kind) event.Event {
				return event.Event{ID: id, Kind: kind, Time: time.Now().UTC(), Session: j.Session, Turn: j.Turn,
					Harness: "claude", FolderRef: "fld_0123456789abcdef"}
			}
			record := func(evs ...event.Event) int {
				t.Helper()
				b := RecordBatch{Number: 1}
				for _, x := range evs {
					raw, _ := json.Marshal(x)
					b.Events = append(b.Events, raw)
				}
				var ack RecordAck
				code := send(t, e.srv, e.tok, "POST", base+"/record", b, &ack)
				if code == 200 && ack.Stored != len(evs) {
					t.Fatalf("record: stored %d of %d", ack.Stored, len(evs))
				}
				return code
			}
			started, rs := ev("evt_00000000000000a1", event.TurnStarted), ev("evt_00000000000000a2", event.RunStarted)
			started.Text, rs.Hold = "add a test", hold
			if code := record(started, rs); code != 200 {
				t.Fatalf("record the start: %d", code)
			}
			exit := 0
			end := ev("evt_00000000000000a3", event.RunEnded)
			end.Hold, end.ExitCode = hold, &exit
			if recorded {
				if code := record(end); code != 200 {
					t.Fatalf("record the end: %d", code)
				}
			}
			// The finish the runner builds for an agent that exited 0 in its copy.
			res, _ := json.Marshal(queue.Result{Session: j.Session, ExitCode: &exit, State: store.TurnFinished, Changeset: cs('1'),
				Changes: &workspace.Changes{Kind: workspace.KindSnapshot, Count: 1, From: "base", To: j.Turn,
					Files: []workspace.Change{{Path: "a_test.go", Change: "added", Size: 12, SHA256: cs('2')}}}})
			fin := FinishCall{Number: 1, Status: queue.Finished, Result: res, Session: j.Session,
				Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.Session, Snapshot: cs('3')}}
			if code := send(t, e.srv, e.tok, "POST", base+"/finish", fin, nil); code != 200 {
				t.Fatalf("finish: %d", code)
			}
			if got, err := e.q.Get(queue.DefaultTeam, j.ID); err != nil || got.Ended == nil || got.State != queue.Finished {
				t.Fatalf("the job: %s ended %v (%v), want finished", got.State, got.Ended, err)
			}
			if busy, err := e.q.SessionHasWork(queue.DefaultTeam, j.Session); err != nil || busy {
				t.Fatalf("the session has work %v (%v), want none", busy, err)
			}
			// A run.ended the runner sends now is too late: the fence refuses it.
			if code := record(end); code != 409 {
				t.Fatalf("a run.ended after the end: %d, want 409", code)
			}
			// Control's sweep, twice: it may close the hold, once.
			for range 2 {
				if _, err := CloseLost(e.q, e.st, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			holds, err := e.st.OpenHolds(e.st.Last())
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range holds {
				if h.Session == j.Session {
					t.Fatalf("the finished turn's hold %s is still open", h.Hold)
				}
			}
			if k := e.kinds(t, j.Session); k[event.RunEnded] != 1 {
				t.Errorf("the record holds %d run.ended, want 1", k[event.RunEnded])
			}
			if c, err := e.st.Continuation(j.Session); err != nil || c.Open {
				t.Errorf("the session reads open %v (%v)", c.Open, err)
			}
			want := store.TurnFinished
			if !recorded {
				want = store.TurnFailed
			}
			list, err := e.st.Turns(store.TurnQuery{Session: j.Session, Turn: j.Turn, Limit: 1})
			if err != nil || len(list.Turns) != 1 || list.Turns[0].State != want {
				t.Errorf("the turn: %+v (%v), want %s", list.Turns, err, want)
			}
		})
	}
}
