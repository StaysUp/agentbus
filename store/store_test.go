package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

var t0 = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newStore(t *testing.T) *Store {
	return open(t, filepath.Join(t.TempDir(), "events.db"))
}

var n int

func ev(kind event.Kind, session string) event.Event {
	n++
	return event.Event{ID: fmt.Sprintf("e-%d", n), Kind: kind, Session: session, Time: t0}
}

func mustAppend(t *testing.T, s *Store, e event.Event) event.Event {
	t.Helper()
	out, ok, err := s.Append(e)
	if err != nil || !ok {
		t.Fatalf("append %s: stored=%v err=%v", e.Kind, ok, err)
	}
	return out
}

func seqs(rs []Record) []int64 {
	var out []int64
	for _, r := range rs {
		out = append(out, r.Seq)
	}
	return out
}

func TestSeqGaplessAndIDKeptAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := open(t, path)
	id := s.ID()
	for i := 1; i <= 3; i++ {
		if got := mustAppend(t, s, ev(event.ModelRequested, "a")).Seq; got != int64(i) {
			t.Fatalf("seq %d, want %d", got, i)
		}
	}
	s.Close()
	s2 := open(t, path)
	if s2.ID() != id {
		t.Fatalf("store id changed on reopen: %q then %q", id, s2.ID())
	}
	if got := mustAppend(t, s2, ev(event.ModelRequested, "a")).Seq; got != 4 {
		t.Fatalf("seq after reopen %d, want 4", got)
	}
	other := newStore(t)
	if other.ID() == id {
		t.Fatal("two store files share an id")
	}
}

func TestStoredJSONCarriesSeq(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, ev(event.ModelRequested, "a"))
	rs, err := s.After(0, Filter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var e event.Event
	if err := json.Unmarshal(rs[0].Data, &e); err != nil || e.Seq != 1 {
		t.Fatalf("stored JSON seq=%d err=%v", e.Seq, err)
	}
}

func TestTurnStartedStoredOncePerTurn(t *testing.T) {
	s := newStore(t)
	start := func(session, turn string) bool {
		e := ev(event.TurnStarted, session)
		e.Turn = turn
		_, ok, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !start("a", "t1") {
		t.Fatal("first turn.started not stored")
	}
	if start("a", "t1") {
		t.Fatal("a retry's turn.started was stored a second time")
	}
	if !start("b", "t1") {
		t.Fatal("the same turn id in another session must be stored")
	}
	if !start("a", "t2") {
		t.Fatal("a new turn must be stored")
	}
	// Without a turn id there is nothing to match on, so each is kept.
	if !start("a", "") || !start("a", "") {
		t.Fatal("turn.started without a turn id must be stored")
	}
	// Other kinds of the same turn are not limited.
	e := ev(event.ModelRequested, "a")
	e.Turn = "t1"
	mustAppend(t, s, e)
	mustAppend(t, s, e)

	rs, _ := s.After(0, Filter{}, 100)
	if got := seqs(rs); fmt.Sprint(got) != "[1 2 3 4 5 6 7]" {
		t.Fatalf("seqs %v: a skipped turn.started must not leave a gap", got)
	}
	snap, _ := s.Snapshot()
	for _, x := range snap.Sessions {
		if x.ID == "a" && x.Turns != 4 {
			t.Fatalf("session a turns = %d, want 4", x.Turns)
		}
	}
}

func dropped(sessions []string, all bool) event.Event {
	e := ev(event.EventsDropped, "")
	e.Dropped, e.Sessions, e.AllSessions = 1, sessions, all
	return e
}

func TestSessionFilterIncludesMatchingDrops(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, ev(event.ModelRequested, "a"))             // 1
	mustAppend(t, s, ev(event.ModelRequested, "b"))             // 2
	mustAppend(t, s, dropped([]string{"b", "c"}, false))        // 3
	mustAppend(t, s, dropped([]string{"a"}, false))             // 4
	mustAppend(t, s, dropped(nil, true))                        // 5
	mustAppend(t, s, dropped([]string{"default", "ab"}, false)) // 6
	mustAppend(t, s, ev(event.ModelRequested, "default"))       // 7
	mustAppend(t, s, ev(event.ModelResponded, "a"))             // 8

	cases := map[string]string{
		"a":       "[1 4 5 8]",
		"b":       "[2 3 5]",
		"c":       "[3 5]",
		"default": "[5 6 7]",
		"zz":      "[5]",
	}
	for session, want := range cases {
		rs, err := s.After(0, Filter{Session: session, Only: true}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(seqs(rs)); got != want {
			t.Errorf("session %q: %s, want %s", session, got, want)
		}
	}
	rs, _ := s.After(4, Filter{Session: "a", Only: true}, 100)
	if got := fmt.Sprint(seqs(rs)); got != "[5 8]" {
		t.Errorf("after 4: %s", got)
	}
	rs, _ = s.After(0, Filter{}, 3)
	if got := fmt.Sprint(seqs(rs)); got != "[1 2 3]" {
		t.Errorf("unfiltered with limit: %s", got)
	}
}

func session(t *testing.T, snap Snapshot, id string) Session {
	t.Helper()
	for _, x := range snap.Sessions {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("session %q not in snapshot %+v", id, snap.Sessions)
	return Session{}
}

func responded(session string, u *event.Usage) event.Event {
	e := ev(event.ModelResponded, session)
	e.Usage = u
	return e
}

func TestSnapshotUsageAbsentIsNotZero(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, responded("absent", nil))
	mustAppend(t, s, responded("zero", &event.Usage{}))
	mustAppend(t, s, responded("mixed", &event.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}))
	mustAppend(t, s, responded("mixed", nil))
	mustAppend(t, s, responded("mixed", &event.Usage{InputTokens: 1, OutputTokens: 1, OutputIncomplete: true}))

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if x := session(t, snap, "absent"); x.Usage != nil || x.UsageMissing != 1 {
		t.Errorf("absent: usage %+v missing %d; absent usage must stay unknown", x.Usage, x.UsageMissing)
	}
	if x := session(t, snap, "zero"); x.Usage == nil || !reflect.DeepEqual(*x.Usage, event.Usage{}) || x.UsageMissing != 0 {
		t.Errorf("zero: usage %+v missing %d; known zeros must be kept", x.Usage, x.UsageMissing)
	}
	x := session(t, snap, "mixed")
	want := event.Usage{InputTokens: 11, OutputTokens: 3, CacheReadTokens: 3, CacheWriteTokens: 4, OutputIncomplete: true}
	if x.Usage == nil || !reflect.DeepEqual(*x.Usage, want) || x.UsageMissing != 1 || !x.OutputIncomplete {
		t.Errorf("mixed: usage %+v missing %d", x.Usage, x.UsageMissing)
	}
	if x.CostComplete {
		t.Error("cost_complete must be false when a call had no usage")
	}

	b, _ := json.Marshal(session(t, snap, "absent"))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["usage"]; !ok || v != nil {
		t.Errorf("absent usage encodes as %v (present %v), want null", v, ok)
	}
}

func hold(kind event.Kind, session, id string) event.Event {
	e := ev(kind, session)
	e.Hold = id
	return e
}

func TestOpenCountsHoldsByID(t *testing.T) {
	s := newStore(t)
	open := func() bool { snap, _ := s.Snapshot(); return session(t, snap, "s").Open }
	opened := hold(event.SessionOpened, "s", "h1")
	opened.Agent, opened.Folder = "claude", "/x"
	mustAppend(t, s, opened)
	if !open() {
		t.Fatal("open hold h1, session closed")
	}
	// A next turn's child can start before the store has its previous
	// child's session.closed: each opened is closed by its own hold.
	mustAppend(t, s, hold(event.SessionOpened, "s", "h2"))
	mustAppend(t, s, hold(event.SessionClosed, "s", "h1"))
	if !open() {
		t.Fatal("h2 is still open, but the session reads closed: holds must be counted by id")
	}
	mustAppend(t, s, hold(event.SessionClosed, "s", "h2"))
	if open() {
		t.Fatal("every hold closed, session still open")
	}
	snap, _ := s.Snapshot()
	if x := session(t, snap, "s"); x.Agent != "claude" || x.Folder != "/x" {
		t.Fatalf("agent %q folder %q", x.Agent, x.Folder)
	}
}

// Only a run's child makes a session open. The guess that a model call in the
// last 30 minutes meant open is gone.
func TestOpenOnlyWhileARunsChildRuns(t *testing.T) {
	s := newStore(t)
	e := ev(event.ModelRequested, "free")
	e.Client = "claude-cli/2.1.318"
	e.Time = time.Now()
	mustAppend(t, s, e)
	snap, _ := s.Snapshot()
	if x := session(t, snap, "free"); x.Open || x.Client != "claude-cli/2.1.318" {
		t.Fatalf("a session with a model call just now and no run: open %v client %q", x.Open, x.Client)
	}
}

func agentEvent(kind event.Kind, session, agent, folder string) event.Event {
	e := ev(kind, session)
	e.Agent, e.Folder = agent, folder
	if kind == event.TurnStarted {
		e.Turn = e.ID
	}
	return e
}

// Agent and folder come from the session's latest turn.started or run that
// names them. A turn with neither (a session pointed at the gateway by hand)
// changes nothing, and an older store's session.registered still counts.
func TestAgentAndFolderFromTurns(t *testing.T) {
	s := newStore(t)
	read := func(id string) (string, string) {
		t.Helper()
		snap, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		x := session(t, snap, id)
		c, err := s.Continuation(id)
		if err != nil {
			t.Fatal(err)
		}
		if c.Agent != x.Agent || c.Folder != x.Folder {
			t.Fatalf("%s: the snapshot says %q %q, the next turn would use %q %q", id, x.Agent, x.Folder, c.Agent, c.Folder)
		}
		return x.Agent, x.Folder
	}

	// A run's child started and has not reached a model call yet.
	mustAppend(t, s, agentEvent(event.SessionOpened, "run", "codex", "/a"))
	if a, f := read("run"); a != "codex" || f != "/a" {
		t.Fatalf("from session.opened: %q %q", a, f)
	}
	mustAppend(t, s, agentEvent(event.TurnStarted, "run", "codex", "/a"))
	mustAppend(t, s, agentEvent(event.TurnStarted, "run", "", ""))
	if a, f := read("run"); a != "codex" || f != "/a" {
		t.Fatalf("after an unnamed turn: %q %q", a, f)
	}
	// A later turn names other values: the latest wins.
	mustAppend(t, s, agentEvent(event.TurnStarted, "run", "claude", "/b"))
	if a, f := read("run"); a != "claude" || f != "/b" {
		t.Fatalf("from the latest turn: %q %q", a, f)
	}

	// An older store's registration.
	mustAppend(t, s, agentEvent(event.SessionRegistered, "old", "claude", "/old"))
	if a, f := read("old"); a != "claude" || f != "/old" {
		t.Fatalf("from session.registered: %q %q", a, f)
	}

	// A session pointed at the gateway by hand.
	mustAppend(t, s, agentEvent(event.TurnStarted, "byhand", "", ""))
	if a, f := read("byhand"); a != "" || f != "" {
		t.Fatalf("hand-pointed: %q %q", a, f)
	}
}

func TestRestartClosesStaleHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := open(t, path)
	mustAppend(t, s, hold(event.SessionOpened, "s", "h1"))
	mustAppend(t, s, hold(event.SessionOpened, "s", "h2"))
	mustAppend(t, s, hold(event.SessionClosed, "s", "h2"))
	s.Close()

	s2 := open(t, path)
	ids := 0
	nClosed, err := s2.CloseStaleHolds(t0, func() string { ids++; return fmt.Sprintf("c-%d", ids) })
	if err != nil || nClosed != 1 {
		t.Fatalf("closed %d holds, err %v; want 1", nClosed, err)
	}
	snap, _ := s2.Snapshot()
	if session(t, snap, "s").Open {
		t.Fatal("session still open after restart")
	}
	rs, _ := s2.After(3, Filter{}, 10)
	var e event.Event
	if len(rs) != 1 || json.Unmarshal(rs[0].Data, &e) != nil || e.Kind != event.SessionClosed || e.Hold != "h1" {
		t.Fatalf("stored after restart: %+v", rs)
	}
}

func TestSnapshotOrderAndSeq(t *testing.T) {
	s := newStore(t)
	a := ev(event.ModelRequested, "old")
	a.Time = t0
	b := ev(event.ModelRequested, "new")
	b.Time = t0.Add(time.Minute)
	mustAppend(t, s, a)
	mustAppend(t, s, b)
	snap, _ := s.Snapshot()
	if snap.Seq != 2 || snap.Store != s.ID() || len(snap.Sessions) != 2 || snap.Sessions[0].ID != "new" {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestNotifySignalsAfterWrite(t *testing.T) {
	s := newStore(t)
	ch, cancel := s.Notify()
	defer cancel()
	mustAppend(t, s, ev(event.ModelRequested, "a"))
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no signal")
	}
}

func TestSecondOpenOnOneFolderFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := open(t, path)
	if s2, err := Open(path); err == nil {
		s2.Close()
		t.Fatal("a second store opened on the same folder")
	}
	s.Close()
	// Closing releases the folder.
	open(t, path)
}

func TestSeqConflictIsAnError(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, ev(event.ModelRequested, "a"))
	// Another writer took seq 2 behind this store's back.
	if _, err := s.w.Exec(`INSERT INTO events(seq, session, turn, kind, time, client, data) VALUES(2, 'x', '', 'model.requested', 0, '', '{}')`); err != nil {
		t.Fatal(err)
	}
	var heard error
	s.OnError = func(err error) { heard = err }
	s.Emit(ev(event.ModelRequested, "a"))
	if heard == nil {
		t.Fatal("a seq conflict was suppressed, not reported")
	}
}

func TestQuestionMarkInFolderKeepsFilesApart(t *testing.T) {
	base := t.TempDir()
	one := filepath.Join(base, "data?one")
	two := filepath.Join(base, "data?two")
	for _, d := range []string{one, two} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s1 := open(t, filepath.Join(one, "events.db"))
	s2 := open(t, filepath.Join(two, "events.db"))
	mustAppend(t, s1, ev(event.ModelRequested, "a"))
	if s1.ID() == s2.ID() || s2.Last() != 0 {
		t.Fatalf("two folders share one database: ids %s %s, second last %d", s1.ID(), s2.ID(), s2.Last())
	}
	for _, d := range []string{one, two} {
		for _, f := range []string{"events.db", "events.db.lock"} {
			if _, err := os.Stat(filepath.Join(d, f)); err != nil {
				t.Errorf("%s: %v", filepath.Join(d, f), err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(base, "data")); err == nil {
		t.Error("a database was written at the path cut at the ?")
	}
	// Other characters a URI gives meaning to land where they say, too.
	odd := filepath.Join(base, "a#b%20c d")
	os.MkdirAll(odd, 0o700)
	open(t, filepath.Join(odd, "events.db"))
	if _, err := os.Stat(filepath.Join(odd, "events.db")); err != nil {
		t.Errorf("%v", err)
	}
	// A relative path works as well.
	t.Chdir(base)
	os.MkdirAll("rel?x", 0o700)
	open(t, filepath.Join("rel?x", "events.db"))
	if _, err := os.Stat(filepath.Join(base, "rel?x", "events.db")); err != nil {
		t.Errorf("relative: %v", err)
	}
}

func TestPageByteBudget(t *testing.T) {
	s := newStore(t)
	big := strings.Repeat("x", 1000)
	for i := 0; i < 10; i++ {
		e := ev(event.ModelResponded, "a")
		e.Text = big
		mustAppend(t, s, e)
	}
	huge := ev(event.ModelResponded, "a")
	huge.Text = strings.Repeat("y", 5000) // over the budget on its own
	mustAppend(t, s, huge)

	var got []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging does not move forward")
		}
		recs, more, err := s.Page(after, Filter{Session: "a", Only: true}, 100, 3000)
		if err != nil {
			t.Fatal(err)
		}
		size := 0
		for _, r := range recs {
			size += len(r.Data)
		}
		if len(recs) == 0 {
			t.Fatalf("an empty page with more=%v", more)
		}
		if len(recs) > 1 && size > 3000 {
			t.Fatalf("page of %d events holds %d bytes, over the budget", len(recs), size)
		}
		got = append(got, seqs(recs)...)
		after = recs[len(recs)-1].Seq
		if !more {
			break
		}
	}
	if fmt.Sprint(got) != "[1 2 3 4 5 6 7 8 9 10 11]" {
		t.Fatalf("seqs across pages %v: a gap or a repeat", got)
	}
}

func costed(session string, usd float64, complete bool) event.Event {
	e := responded(session, &event.Usage{InputTokens: 1})
	e.CostUSD, e.CostComplete = &usd, &complete
	return e
}

// cost_usd sums every call's cost. cost_complete is false when any call's
// cost is incomplete or missing, or a drop names the session.
func TestSnapshotCost(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, costed("full", 0.25, true))
	mustAppend(t, s, costed("full", 0.5, true))
	mustAppend(t, s, ev(event.ToolRequested, "full"))
	mustAppend(t, s, costed("partial", 0.25, true))
	mustAppend(t, s, costed("partial", 0.125, false))
	mustAppend(t, s, costed("uncosted", 0.25, true))
	mustAppend(t, s, responded("uncosted", &event.Usage{InputTokens: 1})) // stored before cost was wired
	nilCost := responded("nousage", nil)
	f := false
	nilCost.CostComplete = &f
	mustAppend(t, s, costed("nousage", 1, true))
	mustAppend(t, s, nilCost)
	mustAppend(t, s, costed("named", 1, true))
	mustAppend(t, s, dropped([]string{"named"}, false))
	mustAppend(t, s, ev(event.TurnStarted, "nocalls"))

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id       string
		usd      float64
		complete bool
	}{
		{"full", 0.75, true},
		{"partial", 0.375, false},
		{"uncosted", 0.25, false},
		{"nousage", 1, false},
		{"named", 1, false},
		{"nocalls", 0, true},
	} {
		x := session(t, snap, c.id)
		if x.CostUSD != c.usd || x.CostComplete != c.complete {
			t.Errorf("%s: cost_usd %v complete %v, want %v %v", c.id, x.CostUSD, x.CostComplete, c.usd, c.complete)
		}
	}

	s2 := newStore(t)
	mustAppend(t, s2, costed("a", 1, true))
	mustAppend(t, s2, dropped(nil, true))
	snap, err = s2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if x := session(t, snap, "a"); x.CostComplete {
		t.Error("a drop of all sessions must make cost incomplete")
	}
}

// The split of cache writes by lifetime is summed with the rest of usage.
func TestSnapshotSumsCacheWriteLifetimes(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, responded("a", &event.Usage{CacheWriteTokens: 30, CacheWrite5mTokens: 10, CacheWrite1hTokens: 20}))
	mustAppend(t, s, responded("a", &event.Usage{CacheWriteTokens: 5, CacheWrite1hTokens: 5}))
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := event.Usage{CacheWriteTokens: 35, CacheWrite5mTokens: 10, CacheWrite1hTokens: 25}
	if x := session(t, snap, "a"); x.Usage == nil || !reflect.DeepEqual(*x.Usage, want) {
		t.Errorf("usage = %+v, want %+v", x.Usage, want)
	}
}

func TestSnapshotTitle(t *testing.T) {
	s := newStore(t)
	turn := func(session, id, text string) event.Event {
		e := ev(event.TurnStarted, session)
		e.Turn, e.Text = id, text
		return e
	}
	// "probed": the first turn is a probe (Claude Code's startup quota
	// check). The probe field is not in event.Event yet, so it is set on the
	// stored JSON, as a gateway that sends it would store it.
	mustAppend(t, s, turn("probed", "t0", "quota"))
	if _, err := s.w.Exec(`UPDATE events SET data = json_set(data, '$.probe', json('true')) WHERE session = 'probed'`); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, turn("probed", "t1", "fix the login bug"))
	mustAppend(t, s, turn("probed", "t2", "later words"))
	// "long": 130 runes of a three-byte letter, cut on a rune boundary.
	mustAppend(t, s, turn("long", "t1", strings.Repeat("ж", 130)))
	// "empty": the first turn has no text, the second does.
	mustAppend(t, s, turn("empty", "t1", ""))
	mustAppend(t, s, turn("empty", "t2", "second"))
	// "none": no turn at all.
	mustAppend(t, s, ev(event.ModelRequested, "none"))
	// "exact": exactly 120 runes is not cut.
	mustAppend(t, s, turn("exact", "t1", strings.Repeat("a", 120)))

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"probed": "fix the login bug",
		"long":   strings.Repeat("ж", 119) + "…",
		"empty":  "second",
		"none":   "",
		"exact":  strings.Repeat("a", 120),
	}
	for _, x := range snap.Sessions {
		if x.Title != want[x.ID] {
			t.Errorf("%s: title %q, want %q", x.ID, x.Title, want[x.ID])
		}
		b, _ := json.Marshal(x)
		if x.ID == "none" && strings.Contains(string(b), `"title"`) {
			t.Errorf("a session without a turn sends title: %s", b)
		}
	}
	if len(snap.Sessions) != len(want) {
		t.Fatalf("%d sessions", len(snap.Sessions))
	}
}
