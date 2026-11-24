//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/store"
)

const turnsPath = "/api/v0/turns"

type turnEvent struct {
	Kind     string `json:"kind"`
	Session  string `json:"session"`
	Turn     string `json:"turn"`
	Text     string `json:"text"`
	Exchange string `json:"exchange"`
}

func turnEvents(t *testing.T, st *store.Store, session string) []turnEvent {
	t.Helper()
	recs, err := st.After(0, store.Filter{Session: session, Only: true}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []turnEvent
	for _, r := range recs {
		var e turnEvent
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func count(evs []turnEvent, kind string) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// The run is the turn: POST /turns names the turn, and
// every event of that turn carries the name, through the real gateway. The
// fake agent makes two model calls with different conversations, as codex
// does when it compacts mid-turn: the gateway's hash would have started a
// second turn, and with the run's id it stays one.
func TestRunTurnIDOnEveryEvent(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_CALL", "2")
	t.Setenv("AGENTBUS_FAKE_STDOUT", claudeResultA)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "first words"})
	if code != http.StatusAccepted {
		t.Fatalf("start %d %s", code, raw)
	}
	session, _ := out["session"].(string)
	turn, _ := out["turn"].(string)
	waitClosed(t, svc.store, session)
	var evs []turnEvent
	waitFor(t, "both model replies", func() bool {
		evs = turnEvents(t, svc.store, session)
		return count(evs, "model.responded") == 2
	})
	if turn == "" || !strings.HasPrefix(turn, "t_") {
		t.Errorf("the reply %s has no turn id", raw)
	}
	for _, e := range evs {
		if e.Kind == "session.created" {
			continue // a session-level event, not a turn's: it carries no turn id
		}
		if e.Turn == "" || e.Turn != turn {
			t.Errorf("%s carries turn %q, want the reply's %q", e.Kind, e.Turn, turn)
		}
	}
	if n := count(evs, "turn.started"); n != 1 {
		t.Errorf("%d turn.started in one run, want 1", n)
	}
	for _, e := range evs {
		if e.Kind == "turn.started" && e.Text != "first words" {
			t.Errorf("turn.started text %q, want the prompt", e.Text)
		}
	}

	// The session's next turn gets a new id of its own.
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	b, _ := json.Marshal(map[string]any{"session": session, "agent": "claude", "prompt": "second words"})
	code, out, raw = postAPI(t, svc, tok, turnsPath, b)
	if code != http.StatusAccepted || out["session"] != session {
		t.Fatalf("next %d %s", code, raw)
	}
	second, _ := out["turn"].(string)
	if second == "" || second == turn {
		t.Fatalf("second turn id %q, first %q: want two different ids", second, turn)
	}
	waitClosedN(t, svc, session, 2)
	// Both turns are done once their jobs are: each job ends only after its
	// outcome is in the record, so nothing moves the seq read below.
	waitEnded(t, svc, turn)
	waitEnded(t, svc, second)
	waitFor(t, "the second reply", func() bool {
		evs = turnEvents(t, svc.store, session)
		return count(evs, "model.responded") == 3
	})
	seen := map[string]int{}
	for _, e := range evs {
		if e.Turn == "" {
			continue // the session.created of the session, not a turn's event
		}
		seen[e.Turn]++
	}
	if len(seen) != 2 || seen[turn] == 0 || seen[second] == 0 {
		t.Fatalf("turn ids %v, want only %s and %s", seen, turn, second)
	}
	if n := count(evs, "turn.started"); n != 2 {
		t.Errorf("%d turn.started over two runs, want 2", n)
	}

	// Traffic by hand, with no turn's token, is refused in agentbus up and
	// recorded nowhere, so it never shows among the turns below.
	resp, err := http.Post("http://"+svc.addr+"/s/byhand/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"by hand"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(turnEvents(t, svc.store, "byhand")) != 0 {
		t.Fatalf("a hand-pointed call: %d, %d events", resp.StatusCode, len(turnEvents(t, svc.store, "byhand")))
	}

	// A turn's job ends before its turn.ended reaches the record: the finisher
	// appends turn.ended AFTER it ends the job, so a reactor reads the job in
	// its final state, and a slow one is backfilled by the sweep's reconciler
	// within its grace (jobs/ended.go, C0). turn.ended carries no session, so it
	// is not among the session's events above, but it moves the store seq. Wait
	// for both turns' turn.ended, or the seq read below races the one that is
	// still being appended.
	for deadline := time.Now().Add(jobs.TurnEndedGrace + 10*time.Second); ; {
		a, _ := svc.store.TurnEnded(turn)
		b, _ := svc.store.TurnEnded(second)
		if a && b {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for both turns' turn.ended in the record")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// GET /turns: newest first, with state and cost, filtered by session.
	var list store.TurnList
	getJSON(t, svc, tok, turnsPath+"?session="+session, &list)
	if list.Store != svc.store.ID() || list.Seq != svc.store.Last() || len(list.Turns) != 2 {
		t.Fatalf("GET /turns?session: store %q seq %d, %d turns", list.Store, list.Seq, len(list.Turns))
	}
	if a, b := list.Turns[0], list.Turns[1]; a.ID != second || b.ID != turn || a.Session != session ||
		a.Harness != "claude" ||
		a.State != store.TurnFinished || b.State != store.TurnFinished || a.Ended == nil {
		t.Fatalf("turns %+v", list.Turns)
	}
	// The list carries no prompt; the single turn serves the whole text.
	if p, _, _, err := svc.store.TurnText(session, second); err != nil || p != "second words" {
		t.Fatalf("second turn text %q: %v", p, err)
	}
	if p, _, _, err := svc.store.TurnText(session, turn); err != nil || p != "first words" {
		t.Fatalf("first turn text %q: %v", p, err)
	}
	// The replies came from an upstream that cannot be reached: no usage, so
	// the cost is a lower bound.
	if b := list.Turns[1]; b.CostUSD != 0 || b.CostComplete || b.Usage != nil {
		t.Errorf("first turn's cost %v complete %v usage %v, want 0, incomplete, null", b.CostUSD, b.CostComplete, b.Usage)
	}
	getJSON(t, svc, tok, turnsPath, &list)
	if len(list.Turns) != 2 || list.Turns[0].Session != session {
		t.Fatalf("GET /turns: %+v, want the two run turns only", list.Turns)
	}

	// GET /sessions/{id}/events?turn=: only that turn's events, in order.
	var page struct {
		Store  string            `json:"store"`
		Events []json.RawMessage `json:"events"`
	}
	getJSON(t, svc, tok, "/api/v0/sessions/"+session+"/events?turn="+turn, &page)
	want := 0
	for _, e := range evs {
		if e.Turn == turn {
			want++
		}
	}
	if page.Store != svc.store.ID() || len(page.Events) != want {
		t.Fatalf("GET /sessions/%s/events?turn=%s: %d events, want %d", session, turn, len(page.Events), want)
	}
	last := 0.0
	for _, raw := range page.Events {
		var e struct {
			Seq  float64 `json:"seq"`
			Turn string  `json:"turn"`
		}
		json.Unmarshal(raw, &e)
		if e.Turn != turn || e.Seq <= last {
			t.Errorf("event %s: turn %q after seq %v", raw, e.Turn, last)
		}
		last = e.Seq
	}
}

func getJSON(t *testing.T, svc *service, tok, path string, v any) {
	t.Helper()
	code, raw := getAPI(t, svc, tok, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, raw)
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}
