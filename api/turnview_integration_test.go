//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A turn token reads any turn of its request tree by the turn id alone, as
// the person does: the baseline's 404 for another session's turn without
// ?session= is gone. A turn outside the tree is refused, by id or by query.
func TestTurnTokenReadsTreeTurnsByID(t *testing.T) {
	e, dir := askEnv(t)
	p := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"parent"}`)
	ptok := tokenOf(t, dir, "parent")
	c := ask(t, e, ptok, `{"agent":"claude","prompt":"child"}`)
	ctok := tokenOf(t, dir, "child")
	q := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"outside"}`)
	tokenOf(t, dir, "outside")
	for _, tc := range []struct {
		tok, path string
		want      int
	}{
		{ptok, "/api/v0/turns/" + c.Turn, 200},
		{ptok, "/api/v0/turns/" + c.Turn + "/diff", 200},
		{ctok, "/api/v0/turns/" + p.Turn, 200},
		{ctok, "/api/v0/turns/" + p.Turn + "/diff", 200},
		{ptok, "/api/v0/turns/" + q.Turn, 403},
		{ptok, "/api/v0/turns/" + q.Turn + "/diff", 403},
		{ptok, "/api/v0/turns/" + c.Turn + "?session=" + q.Session, 403},
		{ptok, "/api/v0/turns/t_0000000000000000", 404},
	} {
		code, out := as(t, e, tc.tok, "GET", tc.path, "")
		if code != tc.want {
			t.Errorf("%s GET %s: %d %s, want %d", tc.tok[:8], tc.path, code, out, tc.want)
		}
		if code == 200 && strings.HasSuffix(tc.path, c.Turn) {
			var one OneTurn
			if json.Unmarshal([]byte(out), &one) != nil || one.Turn.Session != c.Session || one.Turn.Origin == nil || one.Turn.Origin.Turn != p.Turn {
				t.Errorf("the child's turn read by the parent: %s", out)
			}
		}
	}
	for _, n := range []string{"parent", "child", "outside"} {
		release(dir, n)
	}
}

// A turn token's search leaves out its own turn, so the search call itself is
// never a hit. The main token's search finds every turn.
func TestSearchLeavesOutTheSearchingTurn(t *testing.T) {
	e, dir := childEnv(t)
	s := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"the first needle"}`)
	tok := childVars(t, dir, 0)["AGENTBUS_TOKEN"]
	appendAll(t, e.store, event.Event{ID: "prev", Kind: event.TurnStarted, Time: time.Now().UTC(), Session: s.Session, Turn: "t_prev", Text: "an older needle"})
	turns := func(tok string) []string {
		t.Helper()
		code, out := as(t, e, tok, "GET", "/api/v0/sessions/"+s.Session+"/search?q=needle", "")
		var res store.SearchResult
		if code != 200 || json.Unmarshal([]byte(out), &res) != nil {
			t.Fatalf("search: %d %s", code, out)
		}
		var got []string
		for _, h := range res.Hits {
			got = append(got, h.Turn)
		}
		return got
	}
	if got := turns(tok); len(got) != 1 || got[0] != "t_prev" {
		t.Errorf("the turn token's search: %v, want t_prev only", got)
	}
	got := turns(token)
	if len(got) != 2 || !strings.Contains(strings.Join(got, " "), s.Turn) {
		t.Errorf("the main token's search: %v, want both turns", got)
	}
}

// A review is a turn: POST /turns/{id}/review answers it at once, and GET
// /turns/{id} shows it as kind review of the reviewed turn, with its verdict
// once it ends.
func TestReviewIsATurn(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
	w := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"work"}`)
	tokenOf(t, dir, "work")
	os.WriteFile(filepath.Join(pwdOf(t, dir, "work"), "a.txt"), []byte("two\n"), 0o644)
	// Running: it cannot be reviewed yet.
	if code, out := as(t, e, token, "POST", "/api/v0/turns/"+w.Turn+"/review", `{"agent":"codex"}`); code != 409 {
		t.Errorf("review of a running turn: %d %s", code, out)
	}
	release(dir, "work")
	waitEnded(t, e, w.Turn)
	code, out := as(t, e, token, "POST", "/api/v0/turns/"+w.Turn+"/review", `{"agent":"codex","idempotency_key":"rv-1"}`)
	var rv Started
	if code != 202 || json.Unmarshal([]byte(out), &rv) != nil || rv.Turn == "" || rv.Turn == w.Turn || rv.Session == w.Session || rv.Existing {
		t.Fatalf("review: %d %s", code, out)
	}
	if code, out := as(t, e, token, "POST", "/api/v0/turns/"+w.Turn+"/review", `{"agent":"codex","idempotency_key":"rv-1"}`); code != 202 ||
		!strings.Contains(out, rv.Turn) || !strings.Contains(out, `"existing":true`) {
		t.Errorf("a retry of the review: %d %s", code, out)
	}
	// The same key with other fields is another request, and says so.
	if code, out := as(t, e, token, "POST", "/api/v0/turns/"+w.Turn+"/review", `{"agent":"codex","model":"other-model","idempotency_key":"rv-1"}`); code != 409 ||
		!strings.Contains(out, "already used for a different request") {
		t.Errorf("the key with another model: %d %s", code, out)
	}
	waitEnded(t, e, rv.Turn)
	var one OneTurn
	if code := getJSON(t, e, "/api/v0/turns/"+rv.Turn, &one); code != 200 || one.Turn.Kind != queue.KindReview || one.Turn.Reviews != w.Turn ||
		one.Turn.Session != rv.Session || one.Turn.Verdict == nil || one.Turn.Findings == nil {
		t.Fatalf("the review turn: %d %+v", code, one.Turn)
	}
	// Findings is a list once the review ended, [] when it named nothing,
	// never null.
	if _, out := as(t, e, token, "GET", "/api/v0/turns/"+rv.Turn, ""); strings.Contains(out, `"findings":null`) || !strings.Contains(out, `"findings":[`) {
		t.Errorf("the review's findings: %s", out)
	}
	if _, out := as(t, e, token, "GET", "/api/v0/turns/"+w.Turn, ""); strings.Contains(out, `"findings"`) {
		t.Errorf("a turn that is not a review has findings: %s", out)
	}
	var list TurnList
	if code := getJSON(t, e, "/api/v0/turns?session="+rv.Session, &list); code != 200 || len(list.Turns) != 1 || list.Turns[0].Kind != queue.KindReview {
		t.Fatalf("the review's session: %d %+v", code, list.Turns)
	}
}

// PATCH /sessions/{id} closed true closes a session once no turn of it runs:
// GET /sessions/{id} shows its copy closed, the copy is named after the
// session, and the session takes no more turns.
func TestCloseSession(t *testing.T) {
	e, dir := askEnv(t)
	s := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"hold"}`)
	tokenOf(t, dir, "hold")
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/"+s.Session, `{"closed":true}`); code != 409 {
		t.Errorf("close while a turn runs: %d %s", code, out)
	}
	if !strings.HasSuffix(pwdOf(t, dir, "hold"), filepath.Join("sessions", s.Session)) {
		t.Errorf("the copy is at %s, want it named after the session", pwdOf(t, dir, "hold"))
	}
	release(dir, "hold")
	waitEnded(t, e, s.Turn)
	var one OneSession
	if code := getJSON(t, e, "/api/v0/sessions/"+s.Session, &one); code != 200 || one.Session.ID != s.Session || one.Copy == nil ||
		one.Copy.Kind != queue.WorkspaceCopy || one.Copy.Name != s.Session || one.Copy.Closed != nil ||
		one.Session.Closed != nil || !one.Session.Resumable {
		t.Fatalf("GET the open session: %d %+v %+v", code, one.Session, one.Copy)
	}
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/"+s.Session, `{"closed":true,"title":"Done"}`); code != 200 ||
		!strings.Contains(out, `"closed":true`) || !strings.Contains(out, `"title":"Done"`) {
		t.Fatalf("close: %d %s", code, out)
	}
	// A closed session says so, in the list too, and is not resumable.
	if code := getJSON(t, e, "/api/v0/sessions/"+s.Session, &one); code != 200 || one.Copy == nil || one.Copy.Closed == nil ||
		one.Session.Closed == nil || !one.Session.Closed.Equal(*one.Copy.Closed) || one.Session.Resumable {
		t.Fatalf("GET the closed session: %d %+v %+v", code, one.Session, one.Copy)
	}
	var snap Snapshot
	if code := getJSON(t, e, "/api/v0/sessions", &snap); code != 200 || len(snap.Sessions) != 1 || snap.Sessions[0].ID != s.Session ||
		snap.Sessions[0].Closed == nil || snap.Sessions[0].Resumable {
		t.Fatalf("GET /sessions: %d %+v", code, snap.Sessions)
	}
	if code, out := as(t, e, token, "POST", "/api/v0/turns", `{"agent":"claude","session":"`+s.Session+`","prompt":"more"}`); code != 409 {
		t.Errorf("a turn of a closed session: %d %s", code, out)
	}
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/nobody", `{"closed":true}`); code != 404 {
		t.Errorf("close of no session: %d %s", code, out)
	}
}
