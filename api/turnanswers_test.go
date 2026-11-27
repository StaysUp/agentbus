package api

import (
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// A turn that answers twice shows both answers, in order with their times
// (0.6.7). Claude Code resumes after its own background command ends, so
// t_5794feb266e80f67 (2026-11-26) had two turn.completed, and GET
// /api/v0/turns/{id} showed only the last as reply: the first answer, a table
// for the person's decision, was out of sight. A call that ends on a tool
// gives no answer.
func TestTurnShowsEveryAnswer(t *testing.T) {
	e := newEnv(t)
	t0 := time.Date(2026, 11, 26, 14, 56, 0, 0, time.UTC)
	ev := func(id string, kind event.Kind, at time.Duration, exchange, text string) event.Event {
		return event.Event{ID: id, Kind: kind, Time: t0.Add(at), Session: "s1", Turn: "t1", Exchange: exchange, Text: text}
	}
	appendAll(t, e.store,
		ev("e1", event.TurnStarted, 0, "", "decide"),
		ev("e2", event.ModelRequested, time.Second, "x1", ""),
		ev("e3", event.ModelResponded, 2*time.Second, "x1", "let me run it"), // ends on a tool: no answer
		ev("e4", event.ModelRequested, 3*time.Second, "x2", ""),
		ev("e5", event.ModelResponded, 49*time.Second, "x2", "| option | cost |"),
		ev("e6", event.TurnCompleted, 49*time.Second, "x2", ""),
		ev("e7", event.ModelRequested, 4*time.Minute, "x3", ""),
		ev("e8", event.ModelResponded, 4*time.Minute+56*time.Second, "x3", "the command finished"),
		ev("e9", event.TurnCompleted, 4*time.Minute+56*time.Second, "x3", ""),
	)
	var one OneTurn
	if code := getJSON(t, e, "/api/v0/turns/t1", &one); code != 200 {
		t.Fatalf("GET the turn: %d", code)
	}
	r := one.Turn.Replies
	if len(r) != 2 || r[0].Text != "| option | cost |" || !r[0].Time.Equal(t0.Add(49*time.Second)) ||
		r[1].Text != "the command finished" || !r[1].Time.Equal(t0.Add(4*time.Minute+56*time.Second)) {
		t.Fatalf("replies %+v, want both answers in order with their times", r)
	}
	if one.Turn.Reply != "the command finished" {
		t.Errorf("reply %q, want the last answer", one.Turn.Reply)
	}
}
