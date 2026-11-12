package jobs

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A command job's finish stores its command.ran in control's record, once,
// and only one of the job's own session with no turn.
func TestCommandFinishKeepsEvent(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Record: st})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	a := enroll(t, srv, 1)
	if _, _, err := q.Create(queue.DefaultTeam, queue.KindCommand, "", queue.Spec{Session: "s1", Command: "ls"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 || taken.Job.Kind != queue.KindCommand {
		t.Fatalf("take: %d %+v", code, taken.Job)
	}
	base := "/api/v0/runner/attempts/" + taken.Attempt.ID
	if code := send(t, srv, a.Token, "POST", base+"/start", AttemptCall{Number: 1}, nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	finish := func(e event.Event) int {
		ran, _ := json.Marshal(e)
		res, _ := json.Marshal(queue.Result{Ran: ran})
		return send(t, srv, a.Token, "POST", base+"/finish", FinishCall{Number: 1, Status: queue.Finished, Result: res}, nil)
	}
	good := event.Event{ID: "evt_c1", Kind: event.CommandRan, Time: time.Now().UTC(), Session: "s1", Folder: "/runner/copy",
		Command: &event.Command{Line: "ls", Stdout: "a\n"}}
	for name, e := range map[string]event.Event{
		"another session": func() event.Event { x := good; x.Session = "s2"; return x }(),
		"a turn":          func() event.Event { x := good; x.Turn = "t_1"; return x }(),
		"another kind":    func() event.Event { x := good; x.Kind = event.TurnStarted; return x }(),
		"no command":      func() event.Event { x := good; x.Command = nil; return x }(),
		"no id":           func() event.Event { x := good; x.ID = ""; return x }(),
	} {
		if code := finish(e); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	// A finish the queue refuses keeps nothing: a bad status, then a
	// second finish with another event once the first was accepted.
	bad := good
	bad.ID = "evt_bad"
	badRan, _ := json.Marshal(bad)
	badRes, _ := json.Marshal(queue.Result{Ran: badRan})
	if code := send(t, srv, a.Token, "POST", base+"/finish", FinishCall{Number: 1, Status: "running", Result: badRes}, nil); code != 400 {
		t.Fatalf("a finish with a bad status: %d", code)
	}
	if code := finish(good); code != 200 {
		t.Fatalf("finish: %d", code)
	}
	other := good
	other.ID = "evt_other"
	if code := finish(other); code != 409 {
		t.Fatalf("another finish: %d", code)
	}
	if code := finish(good); code != 200 {
		t.Fatalf("the same finish again: %d", code)
	}
	// The record holds the command's output once; the job keeps its id.
	if j, err := q.Get(queue.DefaultTeam, taken.Job.ID); err != nil {
		t.Fatal(err)
	} else if res, _ := queue.ReadResult(j.Result); len(res.Ran) != 0 || res.RanEvent != "evt_c1" {
		t.Fatalf("the job keeps %s", j.Result)
	}
	// A command cancelled while it ran keeps what ran.
	if _, _, err := q.Create(queue.DefaultTeam, queue.KindCommand, "", queue.Spec{Session: "s1", Command: "sleep 9"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 {
		t.Fatalf("take: %d", code)
	}
	base = "/api/v0/runner/attempts/" + taken.Attempt.ID
	send(t, srv, a.Token, "POST", base+"/start", AttemptCall{Number: 1}, nil)
	cancelled := good
	cancelled.ID = "evt_c2"
	ran, _ := json.Marshal(cancelled)
	res, _ := json.Marshal(queue.Result{Ran: ran})
	if code := send(t, srv, a.Token, "POST", base+"/finish", FinishCall{Number: 1, Status: queue.Cancelled, Result: res}, nil); code != 200 {
		t.Fatalf("cancelled finish: %d", code)
	}
	// A command that did not run ends as command.failed, with why.
	if _, _, err := q.Create(queue.DefaultTeam, queue.KindCommand, "", queue.Spec{Session: "s1", CommandID: "cmd_3", Command: "ls"}, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 {
		t.Fatalf("take: %d", code)
	}
	base = "/api/v0/runner/attempts/" + taken.Attempt.ID
	send(t, srv, a.Token, "POST", base+"/start", AttemptCall{Number: 1}, nil)
	refused, _ := json.Marshal(queue.Result{Refused: "the session's copy is not on its runner any more, so the command did not run", Status: 409})
	if code := send(t, srv, a.Token, "POST", base+"/finish", FinishCall{Number: 1, Status: queue.Failed, Reason: queue.ReasonSetup, Result: refused}, nil); code != 200 {
		t.Fatalf("refused finish: %d", code)
	}
	recs, _ := st.After(0, store.Filter{Session: "s1", Only: true}, 100)
	kinds := map[event.Kind]int{}
	var first, failed event.Event
	for _, r := range recs {
		kinds[r.Kind]++
		if r.Kind == event.CommandRan && first.ID == "" {
			json.Unmarshal(r.Data, &first)
		}
		if r.Kind == event.CommandFailed {
			json.Unmarshal(r.Data, &failed)
		}
	}
	if len(recs) != 6 || kinds[event.CommandStarted] != 3 || kinds[event.CommandRan] != 2 || kinds[event.CommandFailed] != 1 {
		t.Fatalf("stored %v", kinds)
	}
	if first.Folder != "" || first.Command.Stdout != "a\n" {
		t.Fatalf("stored %+v", first)
	}
	if failed.Command.ID != "cmd_3" || failed.Error != "the session's copy is not on its runner any more, so the command did not run" {
		t.Fatalf("command.failed: %+v %q", failed.Command, failed.Error)
	}
}
