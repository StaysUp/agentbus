package queue

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// jobEnded reports a turn's end from the queue's own jobs. It stands in for the
// record's store.TurnEnded (which FireOns reads in production) in queue-level
// tests that have no store, so what these tests assert is unchanged: an on
// fires once its subject's job has ended.
func jobEnded(q *Queue) func(string) (bool, error) {
	return func(turn string) (bool, error) {
		j, err := q.JobByTurn(DefaultTeam, turn)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return j.Ended != nil, nil
	}
}

// deliveriesOf returns the result-carrying turns queued in session.
func deliveriesOf(t *testing.T, q *Queue, session string) []Job {
	t.Helper()
	all, err := q.List(DefaultTeam, Filter{Session: session}, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []Job
	for _, j := range all {
		if j.Links.ResultOf != nil {
			out = append(out, j)
		}
	}
	return out
}

// An `on turn.completed` registered for a turn fires once when that turn
// ends: it queues the delivery turn in the on's session, records the fire so
// a second sweep (or a control restart, which keeps no memory) never delivers
// twice, and is then off. The delivery is byte-for-byte what the old Deliver
// made.
func TestOnDelivers(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	on, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "", Maker{Session: parent.Session, Turn: parent.Turn})
	if err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) {
		if session == child.Session && turn == child.Turn {
			return "the whole answer", nil
		}
		return "", nil
	})
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil || n != 0 {
		t.Fatalf("before the subject turn ends: %d %v", n, err)
	}
	res := `{"session":"` + child.Session + `","state":"finished","folder":"/w/1",` +
		`"changes":{"count":1,"files":[{"path":"a.go","change":"modified"}]}}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	ds := deliveriesOf(t, q, parent.Session)
	if len(ds) != 1 {
		t.Fatalf("delivered %d times, want once", len(ds))
	}
	d := ds[0]
	// Delivery to the person's root session (parent) is a root-level turn: that
	// session has no request tree of its own, so no Chain (his rule).
	if d.State != Queued || string(d.Spec.Session) != parent.Session || d.Spec.Harness != "claude" ||
		d.Links.ResultOf.Job != child.ID || d.Links.ResultOf.Turn != child.Turn || d.Links.ResultOf.Agent != "claude" ||
		d.Links.Chain != nil {
		t.Fatalf("delivery: %+v %+v", d.Spec, d.Links)
	}
	for _, want := range []string{"[AgentBus]", "Agent: claude", "Folder: /w/1", "Changed files: 1", "- a.go (modified)", "the whole answer"} {
		if !strings.Contains(d.Spec.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, d.Spec.Prompt)
		}
	}
	live, err := q.ListOns(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range live {
		if o.ID == on.ID {
			t.Fatalf("the one-shot on is still live after firing: %+v", o)
		}
	}
}

// Two distinct ons (different keys) on the SAME turn and session are two
// distinct instructions: each delivers its own turn (his call A, 2026-11-19).
// Before the delivery key was scoped to the on id they shared one key, so the
// second was deduped away and silently did nothing (found in review). Reverting
// that scoping makes this deliver once, not twice.
func TestTwoOnsSamePairBothDeliver(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	mk := Maker{Session: parent.Session, Turn: parent.Turn}
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "k1", mk); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "k2", mk); err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) {
		if session == child.Session && turn == child.Turn {
			return "the whole answer", nil
		}
		return "", nil
	})
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	res := `{"session":"` + child.Session + `","state":"finished","folder":"/w/1",` +
		`"changes":{"count":1,"files":[{"path":"a.go","change":"modified"}]}}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ds := deliveriesOf(t, q, parent.Session); len(ds) != 2 {
		t.Fatalf("two distinct ons delivered %d turns, want 2", len(ds))
	}
}

// CreateTurnLinked with a Delivery records the turn.completed on in the SAME
// write as the turn (his call (a), 2026-11-19): start and deliver are one
// atomic step under one authorization, so no separate step can be lost — the
// window found across three review rounds (client disconnect, runner wait,
// caller stopped in the gap). A retry on the turn's own key adds no second on,
// and the reply still delivers when the turn ends.
func TestCreateTurnLinkedRecordsOnAtomically(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	deliver := &Delivery{Session: parent.Session, Maker: Maker{Session: parent.Session, Turn: parent.Turn}}
	link := asked(parent.Session, parent.Turn, parent.Session, 1)
	spec := Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"}
	child, created, err := q.CreateTurnLinked(DefaultTeam, "k-atomic", spec, link, testLimits, deliver)
	if err != nil || !created {
		t.Fatalf("create with delivery: created=%v %v", created, err)
	}
	ons, err := q.ListOns(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(ons) != 1 || ons[0].Subject != child.Turn || ons[0].Session != parent.Session {
		t.Fatalf("want one on watching %s for %s, got %+v", child.Turn, parent.Session, ons)
	}
	// A retry on the same key is the same turn and adds no second on.
	if _, created2, err := q.CreateTurnLinked(DefaultTeam, "k-atomic", spec, link, testLimits, deliver); err != nil || created2 {
		t.Fatalf("retry: created=%v %v, want the existing turn", created2, err)
	}
	if again, _ := q.ListOns(DefaultTeam); len(again) != 1 {
		t.Fatalf("retry added a second on: %+v", again)
	}
	// The reply delivers when the turn ends.
	q.ReadReplyWith(func(session, turn string) (string, error) {
		if session == child.Session && turn == child.Turn {
			return "answer", nil
		}
		return "", nil
	})
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	res := `{"session":"` + child.Session + `","state":"finished","folder":"/w/1","changes":{"count":0,"files":[]}}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ds := deliveriesOf(t, q, parent.Session); len(ds) != 1 {
		t.Fatalf("delivered %d times, want once", len(ds))
	}
}

// A standalone on with a caller-shaped key does not suppress the atomic
// delivery on of a turn created with the same key: the delivery on's key is
// namespaced (delivery/<key>), a different string. The namespace is reserved
// because every route refuses a key with '/' (checkIdempotencyKey, tested in
// api); this queue test covers the namespacing itself, for a caller-shaped key.
func TestDeliveryKeyAvoidsStandaloneOnCollision(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	mk := Maker{Session: parent.Session, Turn: parent.Turn}
	// A standalone on using key "k".
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, parent.Turn, parent.Session, "k", mk); err != nil {
		t.Fatal(err)
	}
	// A turn created with the SAME key and a delivery still records its own on.
	link := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, created, err := q.CreateTurnLinked(DefaultTeam, "k", Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"}, link, testLimits, &Delivery{Session: parent.Session, Maker: mk})
	if err != nil || !created {
		t.Fatalf("create with the same key: created=%v %v", created, err)
	}
	ons, err := q.ListOns(DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	watchChild := false
	for _, o := range ons {
		if o.Subject == child.Turn && o.Session == parent.Session {
			watchChild = true
		}
	}
	if !watchChild {
		t.Fatalf("the delivery on was suppressed by the standalone on's key: %+v", ons)
	}
}

// delivery is part of a turn's idempotent identity (found in review
// 2026-11-19): a retry that names a different destination gets ErrKeyConflict,
// and the same destination returns the existing turn.
func TestDeliveryInRequestHash(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	mk := Maker{Session: parent.Session, Turn: parent.Turn}
	link := asked(parent.Session, parent.Turn, parent.Session, 1)
	spec := Spec{Harness: "claude", Folder: "/x", Isolate: true, Prompt: "hi"}
	if _, _, err := q.CreateTurnLinked(DefaultTeam, "k", spec, link, testLimits, &Delivery{Session: parent.Session, Maker: mk}); err != nil {
		t.Fatal(err)
	}
	// Same key, a DIFFERENT destination: a changed field is a conflict.
	if _, _, err := q.CreateTurnLinked(DefaultTeam, "k", spec, link, testLimits, &Delivery{Session: "s_other", Maker: mk}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("changed delivery: %v, want ErrKeyConflict", err)
	}
	// Same key, same destination: the existing turn, no conflict.
	if _, created, err := q.CreateTurnLinked(DefaultTeam, "k", spec, link, testLimits, &Delivery{Session: parent.Session, Maker: mk}); err != nil || created {
		t.Fatalf("same delivery retry: created=%v %v, want the existing turn", created, err)
	}
}

// RemoveOn before the subject turn ends stops the delivery: the on is off, so
// FireOns ignores it.
func TestOnRemoved(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	on, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "", Maker{Session: parent.Session, Turn: parent.Turn})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveOn(DefaultTeam, on.ID); err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil || n != 0 {
		t.Fatalf("a removed on delivered: %d %v", n, err)
	}
	if ds := deliveriesOf(t, q, parent.Session); len(ds) != 0 {
		t.Fatalf("a removed on delivered %d turns", len(ds))
	}
	// Removing an unknown on, or one already off, is not found / a no-op.
	if err := q.RemoveOn(DefaultTeam, "on_nope"); err == nil {
		t.Fatal("removing an unknown on should be ErrNotFound")
	}
}

// RegisterOn refuses an event it does not know, so a typo is caught at
// registration, not swallowed by a sweep that never fires.
func TestOnUnknownEvent(t *testing.T) {
	q, _ := open(t)
	if _, err := q.RegisterOn(DefaultTeam, "turn.exploded", "t_1", "s", "", Maker{Session: "s", Turn: "t_0"}); err == nil {
		t.Fatal("an unknown event should be refused")
	}
}

// An on with no maker turn (the main token registers one, naming no asking
// turn) still delivers: the reply runs on the agent that did the work, the
// subject's own.
func TestOnMainTokenDelivers(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	// No maker turn, as a main-token registration leaves it.
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "", Maker{}); err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ds := deliveriesOf(t, q, parent.Session)
	if len(ds) != 1 {
		t.Fatalf("a main-token on delivered %d times, want once", len(ds))
	}
	if ds[0].Spec.Harness != "claude" {
		t.Fatalf("delivery ran on %q, want the subject's own agent", ds[0].Spec.Harness)
	}
}

// B15: TestOnAtomicWithAsk retired. Starting a turn no longer registers an on
// in the same transaction; the ask_agent and review_turn apps register it
// through the public registerOn API, each step idempotent on a retry.

// The on lives in the record, not in memory, so a control restart delivers the
// result once and no more: FireOns on a fresh handle to the same store sees
// the on, delivers, and the on_fires row then stops any later fire.
func TestOnSurvivesRestart(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "jobs.db")
	q, err := Open(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	links := asked(parent.Session, parent.Turn, parent.Session, 1)
	child, err := askTurn(q, links, "")
	if err != nil {
		t.Fatal(err)
	}
	onFor(t, q, child, links)
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished,
		Result: json.RawMessage(`{"session":"` + child.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil { // control stops, before the on fired
		t.Fatal(err)
	}
	q2, err := Open(path, c.now) // control starts again on the same store
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()
	total := 0
	for range 3 {
		n, err := q2.FireOns(DefaultTeam, jobEnded(q2), nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 1 {
		t.Fatalf("after a restart the result was delivered %d times, want once", total)
	}
	if ds := deliveriesOf(t, q2, parent.Session); len(ds) != 1 {
		t.Fatalf("delivered %d turns, want one", len(ds))
	}
}

// A registered on with an idempotency key is a no-op on a retry: the same key
// returns the on it already made, as POST /api/v0/turns does (his call,
// 2026-11-19). The key wins even for a different subject, so the mechanism does
// not rest on the subject conflict.
func TestRegisterOnIdempotentKey(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	mk := Maker{Session: parent.Session, Turn: parent.Turn}
	on1, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "k1", mk)
	if err != nil {
		t.Fatal(err)
	}
	// Same key, same subject: the same on.
	on2, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "k1", mk)
	if err != nil {
		t.Fatal(err)
	}
	if on2.ID != on1.ID {
		t.Fatalf("same key made a second on: %s vs %s", on2.ID, on1.ID)
	}
	// Same key, a DIFFERENT subject: a conflict, never the other registration
	// handed back (the rule POST /api/v0/turns applies to a key with other
	// fields). Found in review 2026-11-19.
	child2, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child2.Turn, parent.Session, "k1", mk); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("same key with a new subject: %v, want ErrKeyConflict", err)
	}
	// Same key, same subject, a different delivery session: also a conflict.
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, "s_other", "k1", mk); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("same key with a new session: %v, want ErrKeyConflict", err)
	}
	// The round-3 sequence (A,K1),(A,K2),(B,K2): several ons may watch one turn,
	// so a second key for subject A makes a SECOND on, not blocked by the subject;
	// that key is bound to it, so reusing it for subject B is a conflict, never a
	// silently new on (the unbound-key bug the single key mechanism removes).
	aK2, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "kA2", mk)
	if err != nil {
		t.Fatal(err)
	}
	if aK2.ID == on1.ID {
		t.Fatalf("a second key for subject A returned A's first on: %s", aK2.ID)
	}
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child2.Turn, parent.Session, "kA2", mk); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("reusing kA2 for a different subject: %v, want ErrKeyConflict", err)
	}
	// A fresh key for the second subject makes a new on.
	on4, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child2.Turn, parent.Session, "k2", mk)
	if err != nil {
		t.Fatal(err)
	}
	if on4.ID == on1.ID {
		t.Fatalf("a fresh key returned the old on: %s", on4.ID)
	}
}

// A main-token on may deliver a turn's reply to a session outside the watched
// turn's tree (B15), but doing so must NOT pull that session into the tree: the
// delivery turn carries no tree when the recipient is not already in it, so
// SessionRoot of the recipient is unchanged and the subject's token cannot reach
// it. Found in review 2026-11-19; reverting the deliverResult fix reopens it.
func TestDeliveryDoesNotPullUnrelatedSessionIntoTree(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	// A person turn, its root; and an asked child in that tree.
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	// Take and start the child now, before any other job exists, so the take is
	// unambiguous.
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated person session: its own root, nothing to do with the
	// child's tree.
	other := job(t, q, "k2")
	if root, err := q.SessionRoot(DefaultTeam, other.Session); err != nil || root != "" {
		t.Fatalf("the unrelated session's root before: %q %v, want empty", root, err)
	}
	// The MAIN token (empty maker) registers an on delivering the child's reply
	// to that unrelated session.
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, other.Session, "", Maker{}); err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) { return "the answer", nil })
	res := `{"session":"` + child.Session + `","state":"finished"}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The reply was delivered to the unrelated session.
	if ds := deliveriesOf(t, q, other.Session); len(ds) != 1 {
		t.Fatalf("delivered %d times to the unrelated session, want once", len(ds))
	}
	// But the session is still its own root: it was NOT pulled into the child's
	// tree (which would let the child's token reach it).
	if root, err := q.SessionRoot(DefaultTeam, other.Session); err != nil || root == parent.Session {
		t.Fatalf("the unrelated session's root after delivery: %q %v, want not %q", root, err, parent.Session)
	}
}

// Several ons may watch one turn, each delivering to a different session, and
// EACH reply lands — the delivery job is keyed by (source turn, destination),
// not the source alone, so two subscriptions do not collide (round-4 finding:
// one delivered, the other recorded a conflict and switched off).
func TestTwoSubscriptionsBothDeliver(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r)
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated session to also receive the reply.
	other := job(t, q, "k2")
	// Two ons on the same subject: the asking session and the other, two keys.
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "kA", Maker{}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, other.Session, "kB", Maker{}); err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) { return "the answer", nil })
	res := `{"session":"` + child.Session + `","state":"finished"}`
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(res)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(deliveriesOf(t, q, parent.Session)); n != 1 {
		t.Fatalf("asking session got %d deliveries, want 1", n)
	}
	if n := len(deliveriesOf(t, q, other.Session)); n != 1 {
		t.Fatalf("the other session got %d deliveries, want 1 (the two subscriptions collided)", n)
	}
}

// Concurrent identical registrations are atomic: one inserts and the rest find
// that one, none errors (round-4 finding: a check-then-insert raced to a
// uniqueness 500 under 64 identical requests).
func TestRegisterOnConcurrentSameKey(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	parent := job(t, q, "")
	_, pa := take(t, q, r)
	if _, _, err := q.Start(r.ID, pa.ID, pa.Number, lease); err != nil {
		t.Fatal(err)
	}
	child, err := askTurn(q, asked(parent.Session, parent.Turn, parent.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 64
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			on, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, child.Turn, parent.Session, "kc", Maker{})
			ids[i], errs[i] = on.ID, err
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d errored: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("request %d got on %s, want the one %s", i, ids[i], ids[0])
		}
	}
}

// A delivery joins the RECEIVING session's own tree, not the subject's: a
// main-token on that watches a turn in tree A and delivers into a session
// already in a DIFFERENT tree B gives the delivery B's root and depth, so it
// keeps B's scope and counts against B's caps (round-5 finding: it had got no
// Chain, losing B's scope and bypassing its limits). His rule, 2026-11-19.
func TestDeliveryKeepsRecipientTree(t *testing.T) {
	q, _ := open(t)
	r := runner(t, q, 5)
	// Tree A: a person turn, its asked child (the turn to watch).
	pA := job(t, q, "a")
	_, aa := take(t, q, r)
	if _, _, err := q.Start(r.ID, aa.ID, aa.Number, lease); err != nil {
		t.Fatal(err)
	}
	cA, err := askTurn(q, asked(pA.Session, pA.Turn, pA.Session, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	_, ca := take(t, q, r) // only cA is queued now
	if _, _, err := q.Start(r.ID, ca.ID, ca.Number, lease); err != nil {
		t.Fatal(err)
	}
	// Tree B: a separate person turn and an asked child whose session is in B,
	// at depth 2. It need not run; SessionChain reads its job's chain.
	pB := job(t, q, "b")
	cB, err := askTurn(q, asked(pB.Session, pB.Turn, pB.Session, 2), "kB")
	if err != nil {
		t.Fatal(err)
	}
	// The MAIN token (empty maker) delivers cA's reply (tree A) into cB's
	// session (tree B).
	if _, err := q.RegisterOn(DefaultTeam, OnTurnCompleted, cA.Turn, cB.Session, "", Maker{}); err != nil {
		t.Fatal(err)
	}
	q.ReadReplyWith(func(session, turn string) (string, error) { return "the answer", nil })
	if _, err := q.End(r.ID, ca.ID, ca.Number, Finish{Status: Finished, Result: json.RawMessage(`{"session":"` + cA.Session + `","state":"finished"}`)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := q.FireOns(DefaultTeam, jobEnded(q), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	ds := deliveriesOf(t, q, cB.Session)
	if len(ds) != 1 {
		t.Fatalf("delivered %d times to the recipient, want 1", len(ds))
	}
	// The delivery carries the RECIPIENT's tree (B), not the subject's (A) and
	// not none.
	if c := ds[0].Links.Chain; c == nil || c.Root != pB.Session || c.Depth != 2 {
		t.Fatalf("delivery chain %+v, want root %s depth 2 (the recipient's tree)", c, pB.Session)
	}
}
