package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

func inTurn(kind event.Kind, session, turn string) event.Event {
	e := ev(kind, session)
	e.Turn = turn
	return e
}

func turnCost(session, turn string, cost float64, complete bool) event.Event {
	e := inTurn(event.ModelResponded, session, turn)
	e.Usage = &event.Usage{InputTokens: 10, OutputTokens: 2}
	e.CostUSD, e.CostComplete = &cost, &complete
	return e
}

func runOpened(session, turn, holdID string) event.Event {
	e := inTurn(event.SessionOpened, session, turn)
	e.Hold, e.Harness, e.Folder = holdID, "claude", "/f"
	return e
}

func runClosed(session, turn, holdID string, code *int) event.Event {
	e := inTurn(event.SessionClosed, session, turn)
	e.Hold, e.ExitCode = holdID, code
	return e
}

func started(session, turn, text string) event.Event {
	e := inTurn(event.TurnStarted, session, turn)
	e.Text, e.Harness, e.Folder = text, "claude", "/f"
	return e
}

func byID(l TurnList) map[string]Turn {
	m := map[string]Turn{}
	for _, t := range l.Turns {
		m[t.ID] = t
	}
	return m
}

// GET /turns' reading: state from the run's exit and from turn.completed,
// cost summed per turn with the session's completeness rules, newest first,
// a session filter and pages.
func TestTurnsStateCostAndOrder(t *testing.T) {
	s := newStore(t)
	zero, one := 0, 1
	// a: a run that finished, two model calls.
	mustAppend(t, s, runOpened("s1", "t_a", "h1"))
	mustAppend(t, s, started("s1", "t_a", "first"))
	mustAppend(t, s, turnCost("s1", "t_a", 0.25, true))
	mustAppend(t, s, turnCost("s1", "t_a", 0.5, true))
	done := inTurn(event.TurnCompleted, "s1", "t_a")
	done.Outcome = event.Finished
	mustAppend(t, s, done)
	mustAppend(t, s, runClosed("s1", "t_a", "h1", &zero))
	// b: the next run of s1, exited 1, with a reply that had no usage.
	mustAppend(t, s, runOpened("s1", "t_b", "h2"))
	mustAppend(t, s, started("s1", "t_b", "second"))
	mustAppend(t, s, inTurn(event.ModelResponded, "s1", "t_b"))
	mustAppend(t, s, runClosed("s1", "t_b", "h2", &one))
	// c: a run still running, in s2.
	mustAppend(t, s, runOpened("s2", "t_c", "h3"))
	mustAppend(t, s, started("s2", "t_c", "third"))
	// d: hand-pointed traffic, no run, completed.
	mustAppend(t, s, inTurn(event.TurnStarted, "s3", "t_d"))
	mustAppend(t, s, inTurn(event.TurnCompleted, "s3", "t_d"))
	// e: a run the service closed on restart, no exit code.
	mustAppend(t, s, runOpened("s4", "t_e", "h4"))
	mustAppend(t, s, started("s4", "t_e", "fourth"))
	mustAppend(t, s, runClosed("s4", "t_e", "h4", nil))
	// A probe's turn.started has no turn and is no turn.
	mustAppend(t, s, ev(event.TurnStarted, "s3"))

	l, err := s.Turns(TurnQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if l.Store != s.ID() || l.Seq != s.Last() || l.NextBefore != nil {
		t.Errorf("list store %q seq %d next %v", l.Store, l.Seq, l.NextBefore)
	}
	var order []string
	for _, x := range l.Turns {
		order = append(order, x.ID)
	}
	if fmt.Sprint(order) != "[t_e t_d t_c t_b t_a]" {
		t.Fatalf("order %v, want newest first", order)
	}
	m := byID(l)
	a := m["t_a"]
	if a.State != TurnFinished || a.Session != "s1" || a.Harness != "claude" || a.Folder != "/f" ||
		a.CostUSD != 0.75 || !a.CostComplete || a.Usage == nil || a.Usage.InputTokens != 20 || a.Outcome != "finished" ||
		a.Ended == nil || a.ExitCode == nil || *a.ExitCode != 0 {
		t.Errorf("finished run %+v", a)
	}
	if b := m["t_b"]; b.State != TurnFailed || b.CostComplete || b.Usage != nil || b.ExitCode == nil || *b.ExitCode != 1 {
		t.Errorf("failed run %+v", b)
	}
	if c := m["t_c"]; c.State != TurnRunning || c.Ended != nil || !c.CostComplete || c.Usage != nil {
		t.Errorf("running run %+v", c)
	}
	if d := m["t_d"]; d.State != TurnFinished || d.Ended == nil || d.Harness != "" {
		t.Errorf("hand-pointed turn %+v", d)
	}
	if e := m["t_e"]; e.State != TurnFailed || e.ExitCode != nil || e.Ended == nil {
		t.Errorf("run closed on restart %+v", e)
	}

	// A drop naming s1 makes its turns' costs incomplete.
	mustAppend(t, s, dropped([]string{"s1"}, false))
	l, _ = s.Turns(TurnQuery{Limit: 100})
	if byID(l)["t_a"].CostComplete || !byID(l)["t_c"].CostComplete {
		t.Error("a drop naming s1 did not mark only s1's turns incomplete")
	}

	// The session filter.
	l, _ = s.Turns(TurnQuery{Session: "s1", Limit: 100})
	if len(l.Turns) != 2 || l.Turns[0].ID != "t_b" || l.Turns[1].ID != "t_a" {
		t.Errorf("session s1: %+v", l.Turns)
	}

	// Pages: two, two, one, each older than the last.
	var paged []string
	before := int64(0)
	for i := 0; i < 5; i++ {
		l, err = s.Turns(TurnQuery{Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range l.Turns {
			paged = append(paged, x.ID)
		}
		if l.NextBefore == nil {
			break
		}
		before = *l.NextBefore
	}
	if fmt.Sprint(paged) != "[t_e t_d t_c t_b t_a]" {
		t.Errorf("paged %v", paged)
	}
}

func startedIn(session, turn, ws string) event.Event {
	e := started(session, turn, "x")
	e.Workspace = ws
	return e
}

func discardDropping(ws string, turns ...string) event.Event {
	e := ev(event.WorkspaceDiscarded, "")
	e.Workspace = ws
	e.Discarded = &event.Discarded{DroppedTurns: turns}
	return e
}

// A turn a discard dropped carries discarded, matched to the turn's own
// workspace (not by id alone, since a turn id may recur across sessions), and a
// migrated workspace (no workspace field) matches by its session id.
func TestTurnDiscardedMark(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, startedIn("s1", "t_a", "ws1"))
	mustAppend(t, s, startedIn("s1", "t_b", "ws1"))
	mustAppend(t, s, startedIn("s2", "t_b", "ws2")) // same turn id, another workspace
	mustAppend(t, s, started("s3", "t_c", "x"))     // migrated: no workspace field
	mustAppend(t, s, discardDropping("ws1", "t_b"))
	mustAppend(t, s, discardDropping("s3", "t_c"))

	m1 := byID(mustTurns(t, s, TurnQuery{Session: "s1", Limit: 100}))
	if !m1["t_b"].Discarded {
		t.Error("ws1's t_b should be marked discarded")
	}
	if m1["t_a"].Discarded {
		t.Error("t_a was not dropped, so it must not be marked")
	}
	if byID(mustTurns(t, s, TurnQuery{Session: "s2", Limit: 100}))["t_b"].Discarded {
		t.Error("ws2's t_b must not be marked by ws1's discard (turn-id collision)")
	}
	if !byID(mustTurns(t, s, TurnQuery{Session: "s3", Limit: 100}))["t_c"].Discarded {
		t.Error("a migrated workspace's t_c should be marked by a discard keyed on the session id")
	}
}

func mustTurns(t *testing.T, s *Store, q TurnQuery) TurnList {
	t.Helper()
	l, err := s.Turns(q)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// One turn's events, oldest first, and no other turn's, nor drops.
func TestTurnFilter(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, started("s1", "t_a", "one"))
	mustAppend(t, s, started("s1", "t_b", "two"))
	mustAppend(t, s, turnCost("s1", "t_a", 0.1, true))
	mustAppend(t, s, dropped([]string{"s1"}, false))
	mustAppend(t, s, inTurn(event.TurnCompleted, "s1", "t_a"))
	rs, err := s.After(0, Filter{Turn: "t_a"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	last := int64(0)
	for _, r := range rs {
		var e event.Event
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatal(err)
		}
		if e.Turn != "t_a" || r.Seq <= last {
			t.Errorf("event %s turn %q seq %d after %d", e.Kind, e.Turn, r.Seq, last)
		}
		last = r.Seq
		kinds = append(kinds, string(e.Kind))
	}
	if fmt.Sprint(kinds) != "[turn.started model.responded turn.completed]" {
		t.Errorf("kinds %v", kinds)
	}
}

// A run's child the previous service left running is closed at start with its
// turn, so the turn reads failed, not running.
func TestRestartClosesStaleHoldWithItsTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := open(t, path)
	mustAppend(t, s, runOpened("s", "t_x", "h1"))
	mustAppend(t, s, started("s", "t_x", "hi"))
	s.Close()
	s2 := open(t, path)
	if n, err := s2.CloseStaleHolds(time.Unix(0, 0), func() string { return "c-1" }); err != nil || n != 1 {
		t.Fatalf("closed %d, err %v", n, err)
	}
	l, _ := s2.Turns(TurnQuery{Limit: 10})
	if len(l.Turns) != 1 || l.Turns[0].State != TurnFailed {
		t.Fatalf("turns %+v, want t_x failed", l.Turns)
	}
}

// A page of turns is bounded: it stops at TurnPageBytes with next_before set,
// and every turn is still reached by paging. The list carries facts only (no
// prompt or reply text), so a long folder is what makes the rows large here.
func TestTurnsPageIsBounded(t *testing.T) {
	s := newStore(t)
	// Folders are near their 4096-byte bound: about 4.3 KB a row, so these
	// rows pass 4 MiB over more than one page.
	folder := "/" + strings.Repeat("f", 4000)
	const n = 2000
	for i := 0; i < n; i++ {
		e := started("s1", fmt.Sprintf("t_%04d", i), "a prompt")
		e.Folder = folder
		mustAppend(t, s, e)
	}
	seen := map[string]bool{}
	before, pages := int64(0), 0
	for {
		l, err := s.Turns(TurnQuery{Limit: n, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		b, _ := json.Marshal(l)
		if len(b) > TurnPageBytes+64<<10 {
			t.Errorf("page %d is %d bytes, over the %d budget", pages, len(b), TurnPageBytes)
		}
		for _, x := range l.Turns {
			seen[x.ID] = true
		}
		if l.NextBefore == nil {
			break
		}
		before = *l.NextBefore
	}
	if pages < 2 {
		t.Errorf("one page held all %d large rows; the budget never stopped it", n)
	}
	if len(seen) != n {
		t.Errorf("paging reached %d turns, want %d", len(seen), n)
	}
}

func modelCall(session, turn, exchange, model string) event.Event {
	e := inTurn(event.ModelRequested, session, turn)
	e.Exchange, e.Model = exchange, model
	return e
}

func modelReply(session, turn, exchange, text string) event.Event {
	e := inTurn(event.ModelResponded, session, turn)
	e.Exchange, e.Text, e.Model = exchange, text, "served-"+exchange
	return e
}

// A listed turn carries the model its last model call asked for and how many
// calls it made (facts); the reply text itself is served by store.TurnText
// (the single turn), paired with that same last call.
func TestTurnReplyModelAndCalls(t *testing.T) {
	s := newStore(t)
	// done: two calls on two models; the last reply is the answer.
	mustAppend(t, s, started("s1", "t_done", "fix it"))
	mustAppend(t, s, modelCall("s1", "t_done", "x1", "claude-haiku-5-5"))
	mustAppend(t, s, modelReply("s1", "t_done", "x1", "Let me look."))
	mustAppend(t, s, modelCall("s1", "t_done", "x2", "claude-opus-5-5"))
	mustAppend(t, s, modelReply("s1", "t_done", "x2", "Fixed."))
	// none: a turn with no model calls.
	mustAppend(t, s, started("s1", "t_none", "hello"))
	// waiting: its last call has no reply yet, so no reply, though an earlier
	// call has one.
	mustAppend(t, s, started("s1", "t_wait", "go on"))
	mustAppend(t, s, modelCall("s1", "t_wait", "w1", "m-a"))
	mustAppend(t, s, modelReply("s1", "t_wait", "w1", "First step."))
	mustAppend(t, s, modelCall("s1", "t_wait", "w2", "m-b"))
	// order: replies land in the other order; the last call's reply still wins.
	mustAppend(t, s, started("s1", "t_order", "both"))
	mustAppend(t, s, modelCall("s1", "t_order", "o1", "m"))
	mustAppend(t, s, modelCall("s1", "t_order", "o2", "m"))
	mustAppend(t, s, modelReply("s1", "t_order", "o2", "second"))
	mustAppend(t, s, modelReply("s1", "t_order", "o1", "first"))
	// noexchange: a request without an exchange takes the next reply without one.
	mustAppend(t, s, started("s1", "t_nox", "old"))
	mustAppend(t, s, modelCall("s1", "t_nox", "", "m-old"))
	mustAppend(t, s, modelReply("s1", "t_nox", "", "plain"))
	// The same turn id in two sessions: each row reads its own session.
	mustAppend(t, s, started("s1", "t_same", "one"))
	mustAppend(t, s, modelCall("s1", "t_same", "a1", "m-one"))
	mustAppend(t, s, modelReply("s1", "t_same", "a1", "reply one"))
	mustAppend(t, s, started("s2", "t_same", "two"))
	mustAppend(t, s, modelCall("s2", "t_same", "b1", "m-two"))
	mustAppend(t, s, modelCall("s2", "t_same", "b2", "m-two"))
	mustAppend(t, s, modelReply("s2", "t_same", "b2", "reply two"))

	l, err := s.Turns(TurnQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		model string
		calls int
	}
	got := map[string]want{}
	for _, x := range l.Turns {
		got[x.Session+"/"+x.ID] = want{x.Model, x.ModelCalls}
	}
	// The list carries the model and the call count (facts), not the reply.
	for k, w := range map[string]want{
		"s1/t_done":  {"claude-opus-5-5", 2},
		"s1/t_none":  {"", 0},
		"s1/t_wait":  {"m-b", 2},
		"s1/t_order": {"m", 2},
		"s1/t_nox":   {"m-old", 1},
		"s1/t_same":  {"m-one", 1},
		"s2/t_same":  {"m-two", 2},
	} {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}

	// The reply text is served by the single turn (store.TurnText), paired with
	// the turn's last model call just as the list pairs the model.
	for k, reply := range map[string]string{
		"s1/t_done":  "Fixed.",
		"s1/t_none":  "",
		"s1/t_wait":  "",
		"s1/t_order": "second",
		"s1/t_nox":   "plain",
		"s1/t_same":  "reply one",
		"s2/t_same":  "reply two",
	} {
		session, turn, _ := strings.Cut(k, "/")
		_, gotReply, found, err := s.TurnText(session, turn)
		if err != nil || !found {
			t.Fatalf("%s: TurnText found %v err %v", k, found, err)
		}
		if gotReply != reply {
			t.Errorf("%s: reply %q, want %q", k, gotReply, reply)
		}
	}

	// A turn with no calls carries facts only in its JSON: an empty model and
	// zero calls, and no prompt or reply field at all.
	b, _ := json.Marshal(byID(l)["t_none"])
	for _, f := range []string{`"model":""`, `"model_calls":0`} {
		if !strings.Contains(string(b), f) {
			t.Errorf("t_none JSON %s lacks %s", b, f)
		}
	}
	for _, f := range []string{`"reply"`, `"prompt"`, "reply_truncated", "prompt_truncated"} {
		if strings.Contains(string(b), f) {
			t.Errorf("t_none JSON %s has %s; the list carries facts only", b, f)
		}
	}

	// With the session filter, the same turn id reads its own session.
	l2, err := s.Turns(TurnQuery{Session: "s1", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if x := byID(l2)["t_same"]; x.Model != "m-one" || x.ModelCalls != 1 {
		t.Errorf("session filter: t_same %+v", x)
	}
	if _, reply, _, _ := s.TurnText("s1", "t_same"); reply != "reply one" {
		t.Errorf("session filter TurnText: t_same reply %q, want %q", reply, "reply one")
	}
}

// store.TurnText reads the whole prompt and whole reply of a turn — never cut
// — pairing the reply with the turn's last model call just as the list pairs
// the model. It serves the single turn and the queue's reply reads. found is
// false for a turn with no turn.started.
func TestTurnText(t *testing.T) {
	s := newStore(t)
	// A long prompt and a long reply: both come back whole, not cut.
	longPrompt := strings.Repeat("é", 1000) + strings.Repeat("x", 256<<10)
	longReply := strings.Repeat("ж", 300<<10)
	mustAppend(t, s, started("s1", "t_long", longPrompt))
	mustAppend(t, s, modelCall("s1", "t_long", "x1", "m"))
	mustAppend(t, s, modelReply("s1", "t_long", "x1", "first"))
	mustAppend(t, s, modelCall("s1", "t_long", "x2", "m"))
	mustAppend(t, s, modelReply("s1", "t_long", "x2", longReply))
	// A turn with no model call has the prompt and an empty reply.
	mustAppend(t, s, started("s1", "t_none", "just a prompt"))
	// The same turn id in two sessions: each reads its own.
	mustAppend(t, s, started("s2", "t_long", "other"))
	mustAppend(t, s, modelCall("s2", "t_long", "y1", "m"))
	mustAppend(t, s, modelReply("s2", "t_long", "y1", "other reply"))

	if p, r, found, err := s.TurnText("s1", "t_long"); err != nil || !found || p != longPrompt || r != longReply {
		t.Errorf("t_long: found %v err %v, prompt %d bytes (want %d), reply %d bytes (want %d)",
			found, err, len(p), len(longPrompt), len(r), len(longReply))
	}
	if p, r, found, _ := s.TurnText("s1", "t_none"); !found || p != "just a prompt" || r != "" {
		t.Errorf("t_none: prompt %q reply %q, want the prompt and an empty reply", p, r)
	}
	if p, r, found, _ := s.TurnText("s2", "t_long"); !found || p != "other" || r != "other reply" {
		t.Errorf("session filter: t_long in s2 prompt %q reply %q", p, r)
	}
	if _, _, found, err := s.TurnText("s1", "t_missing"); err != nil || found {
		t.Errorf("missing turn: found %v err %v, want not found and no error", found, err)
	}
}

// GET /turns/{id}/events?session=: the same turn id in two sessions, and the
// filter keeps one session's events.
func TestTurnEventsSessionFilter(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, started("s1", "t_same", "one"))
	mustAppend(t, s, started("s2", "t_same", "two"))
	mustAppend(t, s, modelCall("s1", "t_same", "a", "m"))
	mustAppend(t, s, modelCall("s2", "t_same", "b", "m"))
	mustAppend(t, s, started("s1", "t_other", "x"))
	sessionsOf := func(f Filter) []string {
		t.Helper()
		recs, more, err := s.Page(0, f, 100, PageBytes)
		if err != nil || more {
			t.Fatalf("page: more %v err %v", more, err)
		}
		var out []string
		for _, r := range recs {
			var e event.Event
			if err := json.Unmarshal(r.Data, &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e.Session+"/"+e.Turn)
		}
		return out
	}
	if got := strings.Join(sessionsOf(Filter{Turn: "t_same"}), " "); got != "s1/t_same s2/t_same s1/t_same s2/t_same" {
		t.Errorf("no session: %s", got)
	}
	if got := strings.Join(sessionsOf(Filter{Turn: "t_same", TurnSession: "s2"}), " "); got != "s2/t_same s2/t_same" {
		t.Errorf("session s2: %s", got)
	}
	if got := sessionsOf(Filter{Turn: "t_same", TurnSession: "s3"}); len(got) != 0 {
		t.Errorf("session s3: %v, want none", got)
	}
}

// A failed run whose child gave no reason takes the refusal of its last
// model call as its error; a refusal of an earlier call, a 200's error, or
// a finished run does not.
func TestFailedTurnTakesItsLastCallsRefusal(t *testing.T) {
	s := newStore(t)
	one, zero := 1, 0
	call := func(session, turn, exchange string) event.Event {
		e := inTurn(event.ModelRequested, session, turn)
		e.Exchange = exchange
		return e
	}
	answer := func(session, turn, exchange string, status int, msg string) event.Event {
		e := inTurn(event.ModelResponded, session, turn)
		e.Exchange, e.Status, e.Error = exchange, status, msg
		return e
	}
	refusal := "no codex subscription is connected on control; connect it once"
	for _, e := range []event.Event{
		// a: failed, its one call refused.
		runOpened("s", "t_a", "h1"), started("s", "t_a", "hi"),
		call("s", "t_a", "x1"), answer("s", "t_a", "x1", 403, refusal),
		runClosed("s", "t_a", "h1", &one),
		// b: failed, an earlier call refused and the last one answered.
		runOpened("s", "t_b", "h2"), started("s", "t_b", "hi"),
		call("s", "t_b", "x2"), answer("s", "t_b", "x2", 429, "slow down"),
		call("s", "t_b", "x3"), answer("s", "t_b", "x3", 200, ""),
		runClosed("s", "t_b", "h2", &one),
		// c: finished, its last reply a 200 the adapter could not read.
		runOpened("s", "t_c", "h3"), started("s", "t_c", "hi"),
		call("s", "t_c", "x4"), answer("s", "t_c", "x4", 200, "openai reply: not JSON"),
		runClosed("s", "t_c", "h3", &zero),
		// d: failed, refused, and its child gave its own reason.
		runOpened("s", "t_d", "h4"), started("s", "t_d", "hi"),
		call("s", "t_d", "x5"), answer("s", "t_d", "x5", 403, refusal),
	} {
		mustAppend(t, s, e)
	}
	closed := runClosed("s", "t_d", "h4", nil)
	closed.Error = "the agent could not start"
	mustAppend(t, s, closed)
	// e: failed, refused, and its agent wrote its own error line (0.5.7):
	// the vendor's words win.
	for _, e := range []event.Event{runOpened("s", "t_e", "h5"), started("s", "t_e", "hi"),
		call("s", "t_e", "x6"), answer("s", "t_e", "x6", 403, refusal)} {
		mustAppend(t, s, e)
	}
	closed = runClosed("s", "t_e", "h5", &one)
	closed.Error = "ERROR codex_api::endpoint: something else went wrong"
	mustAppend(t, s, closed)
	// f: failed, no refusal: its agent's own error line.
	for _, e := range []event.Event{runOpened("s", "t_f", "h6"), started("s", "t_f", "hi")} {
		mustAppend(t, s, e)
	}
	closed = runClosed("s", "t_f", "h6", &one)
	closed.Error = event.ExitError(1)
	mustAppend(t, s, closed)
	l, err := s.Turns(TurnQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	m := byID(l)
	if a := m["t_a"]; a.State != TurnFailed || a.Error != refusal {
		t.Errorf("refused turn %+v", a)
	}
	if b := m["t_b"]; b.State != TurnFailed || b.Error != "" {
		t.Errorf("earlier refusal %+v", b)
	}
	if c := m["t_c"]; c.State != TurnFinished || c.Error != "" {
		t.Errorf("finished turn %+v", c)
	}
	if d := m["t_d"]; d.State != TurnFailed || d.Error != "the agent could not start" {
		t.Errorf("child's own reason %+v", d)
	}
	if e := m["t_e"]; e.State != TurnFailed || e.Error != refusal {
		t.Errorf("refused, with the agent's own line %+v", e)
	}
	if f := m["t_f"]; f.State != TurnFailed || f.Error != event.ExitError(1) {
		t.Errorf("exit code alone %+v", f)
	}
}
