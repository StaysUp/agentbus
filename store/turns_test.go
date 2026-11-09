package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if a.State != TurnFinished || a.Session != "s1" || a.Prompt != "first" || a.Harness != "claude" || a.Folder != "/f" ||
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

// A page of turns is bounded. Each prompt is a 500-character preview, cut on
// a rune boundary, and a page stops at TurnPageBytes with next_before set;
// every turn is still reached by paging.
func TestTurnsPageIsBounded(t *testing.T) {
	s := newStore(t)
	// 2 bytes a rune, so a byte cut would split one; then a 256 KiB tail.
	long := strings.Repeat("é", PromptPreviewRunes+3) + strings.Repeat("x", 256<<10)
	// Control characters encode as \u0001, six bytes each, and folders are near
	// their 4096-byte bound: about 7.6 KB a row, so 1000 rows pass 4 MiB.
	big := strings.Repeat("\x01", PromptPreviewRunes)
	folder := "/" + strings.Repeat("f", 4000)
	const n = 700
	for i := 0; i < n; i++ {
		e := started("s1", fmt.Sprintf("t_%04d", i), big)
		e.Folder = folder
		if i == 0 {
			e.Text = long
		}
		mustAppend(t, s, e)
	}
	seen := map[string]bool{}
	before, pages := int64(0), 0
	for {
		l, err := s.Turns(TurnQuery{Limit: 1000, Before: before})
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
			if x.ID == "t_0000" {
				want := strings.Repeat("é", PromptPreviewRunes)
				if x.Prompt != want || !x.PromptTruncated || !utf8.ValidString(x.Prompt) {
					t.Errorf("long prompt preview: %d runes, valid %v, truncated %v", utf8.RuneCountInString(x.Prompt), utf8.ValidString(x.Prompt), x.PromptTruncated)
				}
			} else if x.Prompt != big || x.PromptTruncated {
				t.Errorf("%s: a 500-character prompt was changed or marked truncated", x.ID)
			}
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

// A NUL in the prompt does not end the preview. SQLite's text substr and length
// stop at one; the preview reads bytes.
func TestTurnPreviewKeepsNUL(t *testing.T) {
	s := newStore(t)
	inside := "before\x00after"
	after := strings.Repeat("é", PromptPreviewRunes) + "\x00tail"
	mustAppend(t, s, started("s1", "t_in", inside))
	mustAppend(t, s, started("s1", "t_after", after))
	l, err := s.Turns(TurnQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	m := byID(l)
	if x := m["t_in"]; x.Prompt != inside || x.PromptTruncated {
		t.Errorf("NUL inside: preview %q truncated %v, want the whole prompt, not truncated", x.Prompt, x.PromptTruncated)
	}
	if x := m["t_after"]; x.Prompt != strings.Repeat("é", PromptPreviewRunes) || !x.PromptTruncated {
		t.Errorf("NUL after the cut: preview of %d runes, truncated %v; want 500 and true",
			utf8.RuneCountInString(x.Prompt), x.PromptTruncated)
	}
}

// A preview keeps whole runes when the byte cap falls inside one.
func TestPreviewDropsACutRune(t *testing.T) {
	full := "ab€" // € is 3 bytes
	if p, cut := preview([]byte(full)[:4], int64(len(full))); p != "ab" || !cut {
		t.Errorf("preview %q cut %v, want ab and true", p, cut)
	}
	if p, cut := preview([]byte(full), int64(len(full))); p != full || cut {
		t.Errorf("preview %q cut %v, want the whole and false", p, cut)
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

// A listed turn carries the reply to its last model call, the model that call
// asked for, and how many calls it made: the conversation shows them for a
// turn whose events are not loaded.
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
		reply, model string
		calls        int
	}
	got := map[string]want{}
	for _, x := range l.Turns {
		if x.ReplyTruncated {
			t.Errorf("%s/%s: a short reply marked truncated", x.Session, x.ID)
		}
		got[x.Session+"/"+x.ID] = want{x.Reply, x.Model, x.ModelCalls}
	}
	for k, w := range map[string]want{
		"s1/t_done":  {"Fixed.", "claude-opus-5-5", 2},
		"s1/t_none":  {"", "", 0},
		"s1/t_wait":  {"", "m-b", 2},
		"s1/t_order": {"second", "m", 2},
		"s1/t_nox":   {"plain", "m-old", 1},
		"s1/t_same":  {"reply one", "m-one", 1},
		"s2/t_same":  {"reply two", "m-two", 2},
	} {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}

	// A turn with no calls still says so in JSON: "reply":"", "model":"", "model_calls":0.
	b, _ := json.Marshal(byID(l)["t_none"])
	for _, f := range []string{`"reply":""`, `"model":""`, `"model_calls":0`} {
		if !strings.Contains(string(b), f) {
			t.Errorf("t_none JSON %s lacks %s", b, f)
		}
	}
	if strings.Contains(string(b), "reply_truncated") {
		t.Errorf("t_none JSON %s has reply_truncated", b)
	}

	// With the session filter, the same turn id reads its own session.
	l2, err := s.Turns(TurnQuery{Session: "s1", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if x := byID(l2)["t_same"]; x.Reply != "reply one" || x.ModelCalls != 1 {
		t.Errorf("session filter: t_same %+v", x)
	}
}

// The reply preview follows the prompt's rule: 500 runes, a NUL does not end
// it, a cut is flagged, and a 256 KiB reply never enters the list whole.
func TestTurnReplyPreviewKeepsNULAndCuts(t *testing.T) {
	s := newStore(t)
	inside := "before\x00after"
	after := strings.Repeat("é", PromptPreviewRunes) + "\x00tail"
	long := strings.Repeat("ж", PromptPreviewRunes+1) + strings.Repeat("x", 256<<10)
	exact := strings.Repeat("€", PromptPreviewRunes)
	for id, text := range map[string]string{"t_in": inside, "t_after": after, "t_long": long, "t_exact": exact} {
		mustAppend(t, s, started("s1", id, "p"))
		mustAppend(t, s, modelCall("s1", id, "x-"+id, "m"))
		mustAppend(t, s, modelReply("s1", id, "x-"+id, text))
	}
	l, err := s.Turns(TurnQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	m := byID(l)
	if x := m["t_in"]; x.Reply != inside || x.ReplyTruncated {
		t.Errorf("NUL inside: reply %q truncated %v, want the whole reply, not truncated", x.Reply, x.ReplyTruncated)
	}
	if x := m["t_after"]; x.Reply != strings.Repeat("é", PromptPreviewRunes) || !x.ReplyTruncated {
		t.Errorf("NUL after the cut: %d runes, truncated %v; want 500 and true", utf8.RuneCountInString(x.Reply), x.ReplyTruncated)
	}
	if x := m["t_long"]; x.Reply != strings.Repeat("ж", PromptPreviewRunes) || !x.ReplyTruncated || !utf8.ValidString(x.Reply) {
		t.Errorf("long reply: %d runes, valid %v, truncated %v", utf8.RuneCountInString(x.Reply), utf8.ValidString(x.Reply), x.ReplyTruncated)
	}
	if x := m["t_exact"]; x.Reply != exact || x.ReplyTruncated {
		t.Errorf("a 500-character reply: %d runes, truncated %v; want it whole and not truncated", utf8.RuneCountInString(x.Reply), x.ReplyTruncated)
	}
}

// The reply preview counts toward the page budget. Each row's prompt and reply
// are 500 control characters, six bytes each in JSON: about 6.5 KB a row, so
// 700 rows pass 4 MiB only when the reply is counted.
func TestTurnsPageCountsReply(t *testing.T) {
	s := newStore(t)
	big := strings.Repeat("\x01", PromptPreviewRunes)
	const n = 700
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t_%04d", i)
		mustAppend(t, s, started("s1", id, big))
		mustAppend(t, s, modelCall("s1", id, "x"+id, "m"))
		mustAppend(t, s, modelReply("s1", id, "x"+id, big))
	}
	seen := map[string]bool{}
	before, pages := int64(0), 0
	for {
		l, err := s.Turns(TurnQuery{Limit: 1000, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		b, _ := json.Marshal(l)
		if len(b) > TurnPageBytes {
			t.Errorf("page %d is %d bytes, over the %d budget", pages, len(b), TurnPageBytes)
		}
		for _, x := range l.Turns {
			seen[x.ID] = true
			if x.Reply != big || x.ReplyTruncated {
				t.Errorf("%s: a 500-character reply was changed or marked truncated", x.ID)
			}
		}
		if l.NextBefore == nil {
			break
		}
		before = *l.NextBefore
	}
	if pages < 2 {
		t.Errorf("one page held all %d rows; the reply was not counted", n)
	}
	if len(seen) != n {
		t.Errorf("paging reached %d turns, want %d", len(seen), n)
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
