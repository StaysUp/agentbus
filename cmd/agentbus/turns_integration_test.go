//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
	b, _ := json.Marshal(map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "first words"})
	code, out, raw := postAPI(t, svc, tok, turnsPath, b)
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
	b, _ = json.Marshal(map[string]any{"session": session, "agent": "claude", "prompt": "second words"})
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
		seen[e.Turn]++
	}
	if len(seen) != 2 || seen[turn] == 0 || seen[second] == 0 {
		t.Fatalf("turn ids %v, want only %s and %s", seen, turn, second)
	}
	if n := count(evs, "turn.started"); n != 2 {
		t.Errorf("%d turn.started over two runs, want 2", n)
	}

	// Traffic by hand in another session: a turn of its own, filtered out below.
	resp, err := http.Post("http://"+svc.addr+"/s/byhand/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"by hand"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	waitFor(t, "the hand-pointed reply", func() bool {
		return count(turnEvents(t, svc.store, "byhand"), "model.responded") == 1
	})

	// GET /turns: newest first, with state and cost, filtered by session.
	var list store.TurnList
	getJSON(t, svc, tok, turnsPath+"?session="+session, &list)
	if list.Store != svc.store.ID() || list.Seq != svc.store.Last() || len(list.Turns) != 2 {
		t.Fatalf("GET /turns?session: store %q seq %d, %d turns", list.Store, list.Seq, len(list.Turns))
	}
	if a, b := list.Turns[0], list.Turns[1]; a.ID != second || b.ID != turn || a.Session != session ||
		a.Prompt != "second words" || b.Prompt != "first words" || a.Harness != "claude" ||
		a.State != store.TurnFinished || b.State != store.TurnFinished || a.Ended == nil {
		t.Fatalf("turns %+v", list.Turns)
	}
	// The replies came from an upstream that cannot be reached: no usage, so
	// the cost is a lower bound.
	if b := list.Turns[1]; b.CostUSD != 0 || b.CostComplete || b.Usage != nil {
		t.Errorf("first turn's cost %v complete %v usage %v, want 0, incomplete, null", b.CostUSD, b.CostComplete, b.Usage)
	}
	getJSON(t, svc, tok, turnsPath, &list)
	if len(list.Turns) != 3 || list.Turns[0].Session != "byhand" {
		t.Fatalf("GET /turns: %+v, want three, the hand-pointed one first", list.Turns)
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

// Traffic pointed at the gateway by hand, with no run, keeps the hash id.
func TestHandPointedTrafficKeepsHashTurn(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	for i := 0; i < 2; i++ {
		resp, err := http.Post("http://"+svc.addr+"/s/byhand/v1/messages", "application/json",
			strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"by hand"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	var evs []turnEvent
	waitFor(t, "two replies", func() bool {
		evs = turnEvents(t, svc.store, "byhand")
		return count(evs, "model.responded") == 2
	})
	// A retry of the same conversation is the same turn: one id, one turn.started.
	ids := map[string]bool{}
	for _, e := range evs {
		ids[e.Turn] = true
	}
	if len(ids) != 1 || ids[""] || count(evs, "turn.started") != 1 {
		t.Fatalf("hand-pointed turn ids %v, %d turn.started", ids, count(evs, "turn.started"))
	}
	for id := range ids {
		if !strings.HasPrefix(id, "t_") || len(id) != 18 {
			t.Fatalf("hand-pointed turn id %q, want t_ and 16 hex characters", id)
		}
	}
}
