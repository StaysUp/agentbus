package jobs

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A runner's turn reaches the control plane's record once, without the
// runner's path, and only for the runner's own current attempt.
func TestRecordRoute(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	fr, _ := testFolders(t)
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Folders: fr, Record: st})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	startFinisher(t, s)
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 {
		t.Fatalf("take: %d", code)
	}
	turn, session := taken.Job.Turn, taken.Job.Session
	if session == "" || turn == "" || turn == taken.Attempt.ID {
		t.Fatalf("the turn job's session %q and turn %q", session, turn)
	}
	path := "/api/v0/runner/attempts/" + taken.Attempt.ID + "/record"
	evIn := func(id string, kind event.Kind, session, turn string) json.RawMessage {
		b, _ := json.Marshal(event.Event{ID: id, Kind: kind, Time: time.Now().UTC(), Session: session, Turn: turn,
			Folder: "/runner/worktrees/job_1", FolderRef: "fld_0123456789abcdef", Text: "p1"})
		return b
	}
	ev := func(id string, kind event.Kind, turn string) json.RawMessage { return evIn(id, kind, session, turn) }
	batch := RecordBatch{Number: 1, Events: []json.RawMessage{ev("evt_1", event.TurnStarted, turn), ev("evt_2", event.RunEnded, turn)}}
	var ack RecordAck
	if code := send(t, srv, a.Token, "POST", path, batch, &ack); code != 200 || ack.Stored != 2 {
		t.Fatalf("record: %d %+v", code, ack)
	}
	if code := send(t, srv, a.Token, "POST", path, batch, &ack); code != 200 || ack.Stored != 0 {
		t.Fatalf("the same batch again: %d %+v", code, ack)
	}
	// The two runners' runner.enrolled are in no session.
	recs, _ := st.After(0, store.Filter{Session: session, Only: true}, 100)
	if len(recs) != 2 {
		t.Fatalf("%d events stored, want 2", len(recs))
	}
	if all, _ := st.After(0, store.Filter{}, 100); len(all) != 4 || !strings.Contains(string(all[0].Data), `"kind":"runner.enrolled","time"`) || !strings.Contains(string(all[0].Data), a.Runner.ID) {
		t.Fatalf("%d events, first %s", len(all), all[0].Data)
	}
	for _, r := range recs {
		var e event.Event
		json.Unmarshal(r.Data, &e)
		if e.Folder != "" || e.FolderRef == "" {
			t.Fatalf("stored %s", r.Data)
		}
	}
	for name, c := range map[string]struct {
		tok  string
		body RecordBatch
		want int
	}{
		"another turn":    {a.Token, RecordBatch{Number: 1, Events: []json.RawMessage{ev("evt_3", event.TurnStarted, "t_other")}}, 400},
		"no id":           {a.Token, RecordBatch{Number: 1, Events: []json.RawMessage{ev("", event.TurnStarted, turn)}}, 400},
		"another runner":  {b.Token, RecordBatch{Number: 1, Events: []json.RawMessage{ev("evt_4", event.TurnStarted, turn)}}, 404},
		"another attempt": {a.Token, RecordBatch{Number: 2, Events: []json.RawMessage{ev("evt_5", event.TurnStarted, turn)}}, 409},
		"another session": {a.Token, RecordBatch{Number: 1, Events: []json.RawMessage{evIn("evt_6", event.SessionRenamed, "s_other", turn)}}, 400},
		"a drop marker":   {a.Token, RecordBatch{Number: 1, Events: []json.RawMessage{ev("evt_9", event.EventsDropped, turn)}}, 400},
		"two sessions": {a.Token, RecordBatch{Number: 1, Events: []json.RawMessage{ev("evt_7", event.TurnStarted, turn),
			evIn("evt_8", event.TurnStarted, "s_other", turn)}}, 400},
	} {
		if code := send(t, srv, c.tok, "POST", path, c.body, nil); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
}

// Control BINDS every runner-written approval id to the authenticated attempt
// (scopeToAttempt), so a runner can only ever own its own attempt's approvals
// and a runner never records an allow. A forged or foreign id is bound to the
// SENDER's attempt, not the one it named, so the round 1-3 cases stay
// impossible; a legacy raw id is bound rather than rejected, so an old runner's
// batch still stores (the P2 fix). Each case maps to an earlier finding.
func TestRunnerApprovalDecisionOwnership(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	fr, _ := testFolders(t)
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Folders: fr, Record: st})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	startFinisher(t, s)
	a := enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 {
		t.Fatalf("take: %d", code)
	}
	session, turn := taken.Job.Session, taken.Job.Turn
	path := "/api/v0/runner/attempts/" + taken.Attempt.ID + "/record"
	// own scopes an approval id to this runner's authenticated attempt (as the
	// gateway does); other scopes it to a different attempt (a forgery).
	own := taken.Attempt.ID + "-ap-own"
	other := "t_victim-ap-victim"
	ev := func(id string, kind event.Kind, sess, tn, approval, text string) json.RawMessage {
		b, _ := json.Marshal(event.Event{ID: id, Kind: kind, Time: time.Now().UTC(),
			Session: sess, Turn: tn, Approval: approval, Text: text})
		return b
	}
	post := func(evs ...json.RawMessage) int {
		return send(t, srv, a.Token, "POST", path, RecordBatch{Number: 1, Events: evs}, nil)
	}

	// A victim approval exists under another attempt, as another runner raised
	// it. This runner must never be able to answer it.
	if _, _, err := st.Append(event.Event{ID: "evt_victim_req", Kind: event.ApprovalRequested,
		Time: time.Now().UTC(), Session: "s_victim", Turn: "t_victim", Approval: other, Text: "rm -rf /"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		e    json.RawMessage
		want int
	}{
		{"legit: a request scoped to this attempt is accepted",
			ev("evt_req", event.ApprovalRequested, session, turn, own, "git push"), 200},
		{"legit: a refusal of its own hold is accepted",
			ev("evt_ok", event.ApprovalDecided, session, turn, own, "no one answered in time"), 200},
		{"R1 self-allow: an allow is refused, binding or not",
			ev("evt_self_allow", event.ApprovalDecided, session, turn, own, "allowed once by a person"), 400},
		{"R1 cross-turn allow: an allow is refused",
			ev("evt_cross_allow", event.ApprovalDecided, session, turn, other, "allowed once by a person"), 400},
		{"R2/R3 cross-turn refuse: a refusal naming another attempt is BOUND to this one, so it stores but cannot touch the victim",
			ev("evt_cross_refuse", event.ApprovalDecided, session, turn, other, "no one answered in time"), 200},
		{"R3 duplicate id: a request naming another attempt is bound to this one, so no id is shared across turns",
			ev("evt_dup_req", event.ApprovalRequested, session, turn, other, "git status"), 200},
		{"legacy raw id: a request with no attempt prefix is bound and stored, not rejected",
			ev("evt_legacy", event.ApprovalRequested, session, turn, "ap-legacy", "git push"), 200},
		{"forged body: a decided whose session and turn are not the runner's own is refused",
			ev("evt_forged", event.ApprovalDecided, "s_victim", "t_victim", own, "no one answered in time"), 400},
	} {
		if code := post(c.e); code != c.want {
			t.Fatalf("%s: %d, want %d", c.name, code, c.want)
		}
	}

	// Despite the runner naming the victim's id above, the victim's approval was
	// never touched: those writes were bound to the runner's OWN attempt. So the
	// victim's approval stays pending.
	if state, err := s.approvalState(other); err != nil || state != DecisionPending {
		t.Fatalf("the victim approval state: %q %v, want pending", state, err)
	}
	// A legacy raw id landed owned by this attempt and unique to it: readable
	// under the bound id, and not under its bare form.
	if state, err := s.approvalState(taken.Attempt.ID + "-ap-legacy"); err != nil || state != DecisionPending {
		t.Fatalf("the legacy approval bound state: %q %v, want pending (stored)", state, err)
	}
	// A whole legacy batch — a raw-id request AND the turn's run.ended —
	// stores (both events new), so an old protocol-5 runner is not stuck after
	// an Ask (the P2): binding scoped the raw id rather than rejecting the batch.
	legacy := RecordBatch{Number: 1, Events: []json.RawMessage{
		ev("evt_legacy2", event.ApprovalRequested, session, turn, "ap-legacy2", "git push"),
		ev("evt_legacy_close", event.RunEnded, session, turn, "", ""),
	}}
	var ack RecordAck
	if code := send(t, srv, a.Token, "POST", path, legacy, &ack); code != 200 || ack.Stored != 2 {
		t.Fatalf("a legacy raw-id batch: %d stored %d, want 200 and 2", code, ack.Stored)
	}
}

// B15: TestRecordRouteReview retired with the review kind. A turn's runner may
// record only in the turn's own session; that scoping is covered by
// TestRecordRoute, now that a review is a normal turn.

// A turn whose runner was lost while it ran has its hold closed in the
// record, unsettled, so the session takes its next turn; a turn still
// running keeps its hold.
func TestCloseLost(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close(); st.Close() })
	rn, err := q.RegisterRunner(queue.DefaultTeam, "r", nil, 2, queue.HashToken("tok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var turns, attempts []string
	var sessions []string
	for range 2 {
		j, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"}, queue.Links{})
		if err != nil {
			t.Fatal(err)
		}
		_, a, ok, _, err := q.Take(rn.ID, 30*time.Second, false)
		if err != nil || !ok {
			t.Fatalf("take: %v %v", ok, err)
		}
		if _, _, err := q.Start(rn.ID, a.ID, a.Number, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.Append(event.Event{ID: queue.NewID("evt_"), Kind: event.RunStarted, Time: now, Session: j.Session, Turn: j.Turn}); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, a.ID)
		turns, sessions = append(turns, j.Turn), append(sessions, j.Session)
	}
	if n, err := CloseLost(q, st, now); err != nil || n != 0 {
		t.Fatalf("before any lease ran out: %d %v", n, err)
	}
	// The second turn's runner still beats; the first's does not.
	now = now.Add(20 * time.Second)
	if _, _, err := q.Heartbeat(rn.ID, attempts[1], 1, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	if _, err := q.Sweep(); err != nil {
		t.Fatal(err)
	}
	if n, err := CloseLost(q, st, now); err != nil || n != 1 {
		t.Fatalf("closed %d %v, want 1", n, err)
	}
	for i, open := range []bool{false, true} {
		c, err := st.Continuation(sessions[i])
		if err != nil || c.Open != open {
			t.Fatalf("session %d open %v, want %v (%v)", i, c.Open, open, err)
		}
	}
	if n, _ := CloseLost(q, st, now); n != 0 {
		t.Fatalf("closed again: %d", n)
	}
}

// A runner emits approval.requested when an ask rule triggers, so control must
// accept it in a record batch (else the batch is refused and forwarding wedges).
func TestRunnerKindsHasApprovalRequested(t *testing.T) {
	if !RunnerKinds[event.ApprovalRequested] {
		t.Fatal("approval.requested is not an event a runner may send")
	}
}
