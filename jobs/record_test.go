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
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{}, &taken); code != 200 {
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
	batch := RecordBatch{Number: 1, Events: []json.RawMessage{ev("evt_1", event.TurnStarted, turn), ev("evt_2", event.SessionClosed, turn)}}
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

// A runner's calls and replies are of its attempt's own turn: a call whose
// exchange an earlier turn of the session carries is refused, and its reply
// is not stored. Codex found it on the api04 round (evt_5193378050d8d465).
func TestCallsOfAnotherTurnRefused(t *testing.T) {
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
	a := enroll(t, srv, 1)
	makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{}, &taken); code != 200 {
		t.Fatalf("take: %d", code)
	}
	session := taken.Job.Session
	// An earlier turn of the session made the call x_prior.
	if _, _, err := st.Append(event.Event{ID: "evt_p", Kind: event.ModelRequested, Time: time.Now().UTC(), Session: session,
		Turn: "t_prior", Exchange: "x_prior", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v0/runner/attempts/" + taken.Attempt.ID + "/calls"
	call := func(x string) SentCall {
		return SentCall{Exchange: x, Session: session, Vendor: "anthropic", Time: time.Now().UTC(), Top: []byte(`{}`), Pieces: []SentPiece{}}
	}
	if code := send(t, srv, a.Token, "POST", path, CallBatch{Number: 1, Calls: []SentCall{call("x_prior")}}, nil); code != 400 {
		t.Fatalf("a call of another turn: %d, want 400", code)
	}
	if _, found, _ := st.CallSession("x_prior"); found {
		t.Fatal("the other turn's call was stored")
	}
	var ack CallAck
	if code := send(t, srv, a.Token, "POST", path, CallBatch{Number: 1, Calls: []SentCall{call("x_own")}}, &ack); code != 200 || ack.Stored != 1 {
		t.Fatalf("a call of its own turn: %d %+v", code, ack)
	}
}

// A review's turn runs in the session control named for it when it made the
// job (Links.ReviewSession): its record goes there and nowhere else. Each
// case is a reproducer an earlier review round found.
func TestRecordRouteReview(t *testing.T) {
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
	a := enroll(t, srv, 3)
	author := makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	var taken TakenWork
	send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{}, &taken)
	send(t, srv, a.Token, "POST", "/api/v0/runner/attempts/"+taken.Attempt.ID+"/start", AttemptCall{Number: 1}, nil)
	ev := func(id, session, turn string, kind event.Kind) json.RawMessage {
		b, _ := json.Marshal(event.Event{ID: id, Kind: kind, Time: time.Now().UTC(), Session: session, Turn: turn})
		return b
	}
	// attempts are each turn's attempt, by the turn id.
	attempts := map[string]string{author.Turn: taken.Attempt.ID}
	record := func(turn string, evs ...json.RawMessage) int {
		return send(t, srv, a.Token, "POST", "/api/v0/runner/attempts/"+attempts[turn]+"/record", RecordBatch{Number: 1, Events: evs}, nil)
	}
	if code := record(author.Turn, ev("evt_a", author.Session, author.Turn, event.TurnStarted)); code != 200 {
		t.Fatalf("the author's record: %d", code)
	}
	// 1#4: a turn's runner renames another session.
	if code := record(author.Turn, ev("evt_a2", "s_other", author.Turn, event.SessionRenamed)); code != 400 {
		t.Fatalf("a turn's event in another session: %d", code)
	}
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/attempts/"+taken.Attempt.ID+"/finish",
		FinishCall{Number: 1, Status: queue.Finished, Session: author.Session, Workspace: &queue.Workspace{Kind: queue.WorkspaceFolder, Folder: "/x"},
			Result: json.RawMessage(`{"state":"finished","changes":{"kind":"snapshot","files":[{"path":"a.txt","change":"modified","diff":"-a\n+b\n"}],"count":1}}`)}, nil); code != 200 {
		t.Fatalf("finish: %d", code)
	}
	// Two reviews of the author's turn, at once.
	revA, _, err := q.Review(queue.DefaultTeam, author.ID, "claude", "sonnet", "", false)
	if err != nil {
		t.Fatal(err)
	}
	revB, _, err := q.Review(queue.DefaultTeam, author.ID, "claude", "sonnet", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sa, sb := revA.Links.ReviewSession, revB.Links.ReviewSession
	if sa == "" || sb == "" || sa == sb || sa == author.Session {
		t.Fatalf("review sessions %q and %q, the author's %q", sa, sb, author.Session)
	}
	turns := map[string]string{}
	for range 2 {
		var w TakenWork
		if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{}, &w); code != 200 {
			t.Fatalf("take a review: %d", code)
		}
		turns[w.Job.ID] = w.Job.Turn
		attempts[w.Job.Turn] = w.Attempt.ID
	}
	ta, tb := turns[revA.ID], turns[revB.ID]
	// 2#1: a review's events in its own session are taken; in the reviewed
	// session they are not.
	if code := record(ta, ev("evt_r1", author.Session, ta, event.SessionRenamed)); code != 400 {
		t.Fatalf("a review's event in the reviewed session: %d", code)
	}
	// 3#1: in a session a queued job names, with nothing recorded yet.
	queued := makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/y", Prompt: "later"})
	if code := record(ta, ev("evt_r2", queued.Session, ta, event.SessionRenamed)); code != 400 {
		t.Fatalf("a review's event in a queued job's session: %d", code)
	}
	// 3#2: a drop marker, even in its own session.
	if code := record(ta, ev("evt_r3", sa, ta, event.EventsDropped)); code != 400 {
		t.Fatalf("a drop marker: %d", code)
	}
	// 6#1: a session the runner names early, that the record holds another
	// turn of.
	if _, _, err := st.Append(event.Event{ID: "evt_x", Kind: event.TurnStarted, Time: time.Now().UTC(), Session: "s_taken", Turn: "t_x"}); err != nil {
		t.Fatal(err)
	}
	created, _ := json.Marshal(map[string]string{"session": "s_taken", "turn": ta})
	send(t, srv, a.Token, "POST", "/api/v0/runner/attempts/"+attempts[ta]+"/events", EventBatch{Number: 1, Offset: 1,
		Events: []queue.AttemptEvent{{Seq: 1, Time: time.Now().UTC(), Kind: "turn.created", Data: created}}}, nil)
	if code := record(ta, ev("evt_r4", "s_taken", ta, event.SessionRenamed)); code != 400 {
		t.Fatalf("a review's event in a session it named itself: %d", code)
	}
	// 5#1 and 4#2: a turn queued in the review's session, before the first
	// batch and between batches, takes nothing away.
	// (Queued or refused as the queue decides: the rule does not read it.)
	_, _, _ = q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Session: sa, Prompt: "follow up"}, queue.Links{})
	if code := record(ta, ev("evt_r5", sa, ta, event.TurnStarted)); code != 200 {
		t.Fatalf("a review's first batch in its own session: %d", code)
	}
	// 7#2: its session renamed, then its next batch.
	if _, _, err := st.Append(event.Event{ID: "evt_ren", Kind: event.SessionRenamed, Time: time.Now().UTC(), Session: sa}); err != nil {
		t.Fatal(err)
	}
	if code := record(ta, ev("evt_r6", sa, ta, event.SessionClosed)); code != 200 {
		t.Fatalf("a review's next batch after its session was renamed: %d", code)
	}
	// 4#1: the other review cannot write into this one's session, nor this
	// one into the other's.
	if code := record(tb, ev("evt_b1", sa, tb, event.TurnStarted)); code != 400 {
		t.Fatalf("a review's event in another review's session: %d", code)
	}
	if code := record(ta, ev("evt_r7", sb, ta, event.TurnStarted)); code != 400 {
		t.Fatalf("a review's event in another review's session: %d", code)
	}
	if code := record(tb, ev("evt_b2", sb, tb, event.TurnStarted)); code != 200 {
		t.Fatalf("the other review's own session: %d", code)
	}
}

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
		_, a, ok, _, err := q.Take(rn.ID, 30*time.Second)
		if err != nil || !ok {
			t.Fatalf("take: %v %v", ok, err)
		}
		if _, _, err := q.Start(rn.ID, a.ID, a.Number, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.Append(event.Event{ID: queue.NewID("evt_"), Kind: event.SessionOpened, Time: now, Session: j.Session, Turn: j.Turn}); err != nil {
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
