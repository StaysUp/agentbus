package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// endedRecord stands in for the record's turn.ended events in queue tests,
// which have no store: each ended turn job gets the next seq the first time it
// is seen, as control writes turn.ended once per ended turn. last is the seq of
// the newest, what an on made now starts reading after.
func endedRecord(t *testing.T, q *Queue) (read RecordReader, last func() int64) {
	t.Helper()
	seqOf := map[string]int64{}
	var order []string
	scan := func() {
		ended, err := q.EndedTurns(nil, time.Now().Add(100*365*24*time.Hour), 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range ended {
			if _, ok := seqOf[j.Turn]; !ok {
				order = append(order, j.Turn)
				seqOf[j.Turn] = int64(len(order))
			}
		}
	}
	read = func(kind string, after int64, limit int) ([]RecordEvent, error) {
		if kind != "turn.ended" {
			return nil, nil
		}
		scan()
		var out []RecordEvent
		for i := after; i < int64(len(order)) && len(out) < limit; i++ {
			out = append(out, RecordEvent{ID: "te_" + order[i], Seq: i + 1, Turn: order[i]})
		}
		return out, nil
	}
	last = func() int64 { scan(); return int64(len(order)) }
	return read, last
}

// runEnd takes jobs, starting and ending each finished, until it has ended
// want. A delivery into a session no on watches may come first; ending it
// changes nothing these tests read.
func runEnd(t *testing.T, q *Queue, r Runner, want Job) {
	t.Helper()
	for range 10 {
		j, a := take(t, q, r)
		if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
			t.Fatal(err)
		}
		if _, err := q.End(r.ID, a.ID, a.Number, Finish{Status: Finished, Result: json.RawMessage(`{"session":"` + j.Session + `","state":"finished"}`)}); err != nil {
			t.Fatal(err)
		}
		if j.ID == want.ID {
			return
		}
	}
	t.Fatalf("job %s was never taken", want.ID)
}

// One on over a builder's session delivers EVERY turn of it that ends to the
// coordinator: the builder's own turn, and the later turn that carries the
// review it asked for, so the coordinator hears "reviewed" with no on per
// review (0.6.8; the hand-registered on per review of 2026-11-27). A turn that
// ended before the on was made is not delivered, a second check delivers
// nothing more, and the on stays.
func TestWatchOnDeliversEveryTurnOfASession(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	read, last := endedRecord(t, q)
	early := job(t, q, "")
	runEnd(t, q, r, early)

	coord := job(t, q, "")
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	builder, err := askTurn(q, asked(coord.Session, coord.Turn, coord.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	// The builder's copy is kept (its runner holds it), so its session takes
	// more turns after its first.
	if _, err := q.db.Exec(`INSERT INTO copies(id, team, runner, kind, folder, created) VALUES(?,?,?,?,?,0)`,
		string(builder.Session), DefaultTeam, r.ID, "overlay", "/x"); err != nil {
		t.Fatal(err)
	}
	on, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: string(builder.Session)}, coord.Session, "",
		Maker{Session: coord.Session, Turn: coord.Turn}, &testLimits, last())
	if err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) { return "reply of " + turn, nil })
	fire := func() int {
		t.Helper()
		n, err := q.FireWatchOns(DefaultTeam, read, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The builder's turn ends: its reply goes to the coordinator.
	runEnd(t, q, r, builder)
	if n := fire(); n != 1 {
		t.Fatalf("after the builder's turn: %d delivered, want 1", n)
	}
	// The builder asks for a review, with its own on over the reviewer's
	// session (what review_turn registers), and the reviewer ends.
	review, err := askTurn(q, asked(string(builder.Session), builder.Turn, coord.Session, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: string(review.Session)}, string(builder.Session), "",
		Maker{Session: string(builder.Session), Turn: builder.Turn}, &testLimits, last()); err != nil {
		t.Fatal(err)
	}
	runEnd(t, q, r, review)
	if n := fire(); n != 1 {
		t.Fatalf("after the review: %d delivered, want 1 (into the builder)", n)
	}
	intoBuilder := deliveriesOf(t, q, string(builder.Session))
	if len(intoBuilder) != 1 || intoBuilder[0].Links.ResultOf.Turn != review.Turn {
		t.Fatalf("the review's result into the builder: %+v", intoBuilder)
	}
	// The builder's turn that carries the review ends: the coordinator hears it.
	runEnd(t, q, r, intoBuilder[0])
	if n := fire(); n != 1 {
		t.Fatalf("after the builder read the review: %d delivered, want 1", n)
	}
	if n := fire(); n != 0 {
		t.Fatalf("a second check delivered %d more", n)
	}
	got := deliveriesOf(t, q, coord.Session)
	if len(got) != 2 || got[0].Links.ResultOf.Turn == got[1].Links.ResultOf.Turn {
		t.Fatalf("the coordinator got %d results, want 2: %+v", len(got), got)
	}
	carried := map[string]bool{got[0].Links.ResultOf.Turn: true, got[1].Links.ResultOf.Turn: true}
	if !carried[builder.Turn] || !carried[intoBuilder[0].Turn] || carried[early.Turn] {
		t.Fatalf("the coordinator's results carry %v, want the builder's turn %s and its review turn %s", carried, builder.Turn, intoBuilder[0].Turn)
	}
	// Into the coordinator's own root session, from its own turn's on: free,
	// the person's root-level turns. Into the builder, from the builder's on:
	// a turn the builder asked for, at the builder's own depth.
	for _, d := range got {
		if string(d.Spec.Session) != coord.Session || d.Links.ResultOf.Session != string(builder.Session) || d.Links.Chain != nil {
			t.Fatalf("a delivery to the coordinator: %+v %+v %+v", d.Spec, d.Links.ResultOf, d.Links.Chain)
		}
	}
	if c := intoBuilder[0].Links.Chain; c == nil || c.By != builder.Turn || c.Depth != 1 || c.Root != coord.Session {
		t.Fatalf("the review's delivery into the builder has chain %+v, want asked by the builder at its depth 1", c)
	}
	live, err := isLive(q, on.ID)
	if err != nil || !live {
		t.Fatalf("the on over the builder's session is not live after firing: %v %v", live, err)
	}
}

// isLive says on id is still live (not off).
func isLive(q *Queue, id string) (bool, error) {
	ons, err := q.ListOns(DefaultTeam)
	for _, o := range ons {
		if o.ID == id {
			return true, err
		}
	}
	return false, err
}

// An on over a session repeats, so it may not deliver in a circle: into the
// session it watches, or into one whose replies already come back to it.
// Without the check, two such ons would deliver to each other for ever, and
// an on is not held to the job caps.
func TestWatchOnRefusesACircle(t *testing.T) {
	q, _ := open(t)
	reg := func(watch, to string) error {
		_, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: watch}, to, "", Maker{}, nil, 0)
		return err
	}
	if err := reg("s_a", "s_a"); !errors.Is(err, ErrCycle) {
		t.Fatalf("an on into the session it watches: %v, want ErrCycle", err)
	}
	if err := reg("s_a", "s_b"); err != nil {
		t.Fatal(err)
	}
	if err := reg("s_b", "s_c"); err != nil {
		t.Fatal(err)
	}
	if err := reg("s_b", "s_a"); !errors.Is(err, ErrCycle) {
		t.Fatalf("b into a while a delivers into b: %v, want ErrCycle", err)
	}
	if err := reg("s_c", "s_a"); !errors.Is(err, ErrCycle) {
		t.Fatalf("c into a while a reaches c through b: %v, want ErrCycle", err)
	}
	if err := reg("s_c", "s_d"); err != nil {
		t.Fatalf("c into d closes no circle: %v", err)
	}
	// A removed on no longer counts.
	ons, err := q.ListOns(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ons {
		if o.Filter.Session == "s_a" {
			if err := q.RemoveOn(DefaultTeam, o.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := reg("s_c", "s_a"); err != nil {
		t.Fatalf("c into a once a no longer delivers into b: %v", err)
	}
}

// One live on per watched session and target: a second returns the first, so
// each reply is delivered once. A key binds as POST /api/v0/turns' does.
func TestWatchOnOncePerPair(t *testing.T) {
	q, _ := open(t)
	first, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: "s_b"}, "s_c", "k1", Maker{Session: "s_c", Turn: "t_1"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: "s_b"}, "s_c", "", Maker{Session: "s_c", Turn: "t_2"}, nil, 0)
	if err != nil || again.ID != first.ID || again.Maker.Turn != "t_1" {
		t.Fatalf("a second on for the same pair: %+v %v, want %s", again, err, first.ID)
	}
	if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: "s_x"}, "s_c", "k1", Maker{}, nil, 0); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a used key with another filter: %v, want ErrKeyConflict", err)
	}
	if _, err := q.RegisterWatchOn(DefaultTeam, "turn.exploded", OnFilter{Session: "s_x"}, "s_c", "", Maker{}, nil, 0); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("an unknown event: %v", err)
	}
	if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{}, "s_c", "", Maker{}, nil, 0); !errors.Is(err, ErrBadState) {
		t.Fatalf("no watched session: %v", err)
	}
}

// Ons do not count against the turn budget (0.6.8; the coordinator's gap of
// 2026-11-27, whose hand-registered ons used its whole tree budget): neither
// the ons a turn registers now nor ones an older build marked as asked work.
// The turn may still ask for as many jobs as its caps allow.
func TestOnsDoNotCountAsAskedWork(t *testing.T) {
	q, _ := open(t)
	lim := Limits{MaxDepth: 2, MaxPerTurn: 1, MaxPerTree: 1}
	p := job(t, q, "")
	mk := Maker{Session: p.Session, Turn: p.Turn}
	for _, s := range []string{"s_1", "s_2", "s_3"} {
		if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: s}, p.Session, "", mk, &lim, 0); err != nil {
			t.Fatal(err)
		}
	}
	// An on an older build counted (asked = 1), by the same turn, in its tree.
	if _, err := q.db.Exec(`INSERT INTO ons(id, team, event, subject, session, maker, created, asked) VALUES('on_old', ?, 'turn.completed', 't_x', ?, ?, 0, 1)`,
		DefaultTeam, p.Session, `{"session":"`+p.Session+`","turn":"`+p.Turn+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.CreateTurnLinked(DefaultTeam, "", Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"},
		asked(p.Session, p.Turn, p.Session, 1), lim, nil); err != nil {
		t.Fatalf("an ask by a turn with ons and caps of 1: %v, want it to start", err)
	}
}

// Depth counts asking, not hearing back (his call, 2026-11-28). A builder at
// depth 1 asks for a review (depth 2) and hears its result through an on over
// the reviewer's session, as review_turn registers it. The turn that carries
// the result is at the builder's depth, so at the default limits (Depth 2) it
// may ask for a second review. When a delivery went one level deeper, it
// arrived at depth 2 and its second review was refused at depth 3.
func TestBuilderAsksAgainAfterHearingBack(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	read, last := endedRecord(t, q)
	q.ReadReplyWith(func(session, turn string) (string, error) { return "reply of " + turn, nil })
	coord := job(t, q, "")
	builder, err := askTurn(q, asked(coord.Session, coord.Turn, coord.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	// The builder's copy is kept, so its session takes the delivered turn.
	if _, err := q.db.Exec(`INSERT INTO copies(id, team, runner, kind, folder, created) VALUES(?,?,?,?,?,0)`,
		string(builder.Session), DefaultTeam, r.ID, "overlay", "/x"); err != nil {
		t.Fatal(err)
	}
	// The builder's turn asks for the review and has its session watched, as
	// review_turn does.
	review, err := askTurn(q, asked(string(builder.Session), builder.Turn, coord.Session, 2), "")
	if err != nil {
		t.Fatalf("the first review: %v", err)
	}
	if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: string(review.Session)}, string(builder.Session), "",
		Maker{Session: string(builder.Session), Turn: builder.Turn}, &testLimits, last()); err != nil {
		t.Fatal(err)
	}
	runEnd(t, q, r, review)
	if _, err := q.FireWatchOns(DefaultTeam, read, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	heard := deliveriesOf(t, q, string(builder.Session))
	if len(heard) != 1 {
		t.Fatalf("the review's result reached the builder %d times, want 1", len(heard))
	}
	c := heard[0].Links.Chain
	if c == nil || c.Depth != 1 || c.Root != coord.Session || c.By != builder.Turn {
		t.Fatalf("the builder hears back with chain %+v, want root %s at the builder's depth 1, by %s", c, coord.Session, builder.Turn)
	}
	// The turn that heard back asks for the second review, one level below its
	// own depth, as its token reads it (jobs/tokens.go).
	if _, err := askTurn(q, asked(string(heard[0].Session), heard[0].Turn, c.Root, c.Depth+1), ""); err != nil {
		t.Fatalf("the second review, at the default limits: %v", err)
	}
}

// Codex review t_58b5f94f2ace24d3, P1: a turn opened a fresh session and had
// its own session's results delivered there; each delivered turn did the same
// again, six times with both job limits at 1, since a delivery counted against
// nothing. A delivered turn is now a turn of the tree of the on's maker, asked
// by it (his rule, 2026-11-28): the chain stops at the tree cap, and the
// refusal is reported with its plain reason.
func TestWatchChainStopsAtTheLimits(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	read, last := endedRecord(t, q)
	type refusal struct{ on, session, turn, why string }
	var refused []refusal
	q.ReportRefusedWith(func(on, session string, work Job, why string) {
		refused = append(refused, refusal{on, session, work.Turn, why})
	})
	lim := Limits{MaxDepth: 10, MaxPerTurn: 1, MaxPerTree: 1}
	root := job(t, q, "")
	maker := root
	delivered := 0
	for i := 1; i <= 6; i++ {
		next := fmt.Sprintf("s_next%d", i)
		if _, err := q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: maker.Session}, next, "",
			Maker{Session: maker.Session, Turn: maker.Turn}, &lim, last()); err != nil {
			t.Fatal(err)
		}
		runEnd(t, q, r, maker)
		if _, err := q.FireWatchOns(DefaultTeam, read, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		ds := deliveriesOf(t, q, next)
		if len(ds) == 0 {
			break
		}
		delivered++
		c := ds[0].Links.Chain
		if c == nil || c.By != maker.Turn || c.Root != root.Session || c.Depth != delivered || ds[0].Tree != root.Session {
			t.Fatalf("delivery %d has chain %+v tree %q, want asked by %s at depth %d in tree %s", delivered, c, ds[0].Tree, maker.Turn, delivered, root.Session)
		}
		maker = ds[0]
	}
	if delivered != 1 {
		t.Fatalf("the chain delivered %d turns, want 1 (tree cap 1)", delivered)
	}
	if len(refused) != 1 || !strings.Contains(refused[0].why, "(Turns per request tree in Settings, Limits)") || refused[0].session != "s_next2" || refused[0].turn != maker.Turn {
		t.Fatalf("refusals reported: %+v, want one for the second delivery with the tree cap", refused)
	}
	// The depth cap holds the same way: a maker at the cap gets no delivery.
	q2, _ := open(t)
	r2 := runner(t, q2, 5)
	read2, last2 := endedRecord(t, q2)
	p := job(t, q2, "")
	if _, err := q2.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: p.Session}, "s_deep", "",
		Maker{Session: p.Session, Turn: p.Turn}, &Limits{MaxDepth: 0, MaxPerTurn: 10, MaxPerTree: 10}, last2()); err != nil {
		t.Fatal(err)
	}
	runEnd(t, q2, r2, p)
	if n, err := q2.FireWatchOns(DefaultTeam, read2, nil, nil, nil); err != nil || n != 0 || len(deliveriesOf(t, q2, "s_deep")) != 0 {
		t.Fatalf("a delivery past the depth cap: %d %v", n, err)
	}
}

// Codex review t_58b5f94f2ace24d3, P2: an on returned as a duplicate left the
// key sent with it unbound, so another on could take the key, and after the on
// was removed a retry made a new one. The key is bound to the on it returns.
func TestWatchOnBindsAKeySentWithADuplicate(t *testing.T) {
	q, _ := open(t)
	reg := func(watch, key string) (On, error) {
		return q.RegisterWatchOn(DefaultTeam, OnTurnCompleted, OnFilter{Session: watch}, "s_to", key, Maker{}, nil, 0)
	}
	first, err := reg("s_w", "")
	if err != nil {
		t.Fatal(err)
	}
	dup, err := reg("s_w", "k2")
	if err != nil || dup.ID != first.ID {
		t.Fatalf("the duplicate: %+v %v, want %s", dup, err, first.ID)
	}
	if _, err := reg("s_other", "k2"); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("k2 reused with another filter: %v, want ErrKeyConflict", err)
	}
	if err := q.RemoveOn(DefaultTeam, first.ID); err != nil {
		t.Fatal(err)
	}
	again, err := reg("s_w", "k2")
	if err != nil || again.ID != first.ID {
		t.Fatalf("a retry with k2 after the removal: %+v %v, want the removed on %s", again, err, first.ID)
	}
	if live, _ := q.ListOns(DefaultTeam); len(live) != 0 {
		t.Fatalf("a retry after the removal made a new on: %+v", live)
	}
}
