//go:build integration

package api

import (
	"encoding/json"
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

// B15: TestReviewIsATurn retired with the review route and kind. Review is now
// a default app (review_turn), exercised end to end by TestReviewTurnAppDelivers
// in asked_integration_test.go; a review turn shows as an ordinary turn with no
// verdict or findings field on it.

// PATCH /sessions/{id} closed true is the retired close: it now archives the
// session (B14) by recording a session.archived event. Nothing is removed --
// the copy survives, named after the session -- the session stays resumable
// and can still take a turn, archiving is never refused for a running turn,
// and the default list hides it while archived=true still shows it.
func TestCloseSession(t *testing.T) {
	e, dir := askEnv(t)
	s := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"hold"}`)
	tokenOf(t, dir, "hold")
	// Archive while the turn is still running: it is only a view, so it is
	// never refused for busy work. closed:true maps to archive; the response
	// says archived, not closed, and adds the title.
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/"+s.Session, `{"closed":true,"title":"Done"}`); code != 200 ||
		!strings.Contains(out, `"archived":true`) || strings.Contains(out, `"closed":true`) || !strings.Contains(out, `"title":"Done"`) {
		t.Fatalf("archive while a turn runs: %d %s", code, out)
	}
	if !strings.HasSuffix(pwdOf(t, dir, "hold"), filepath.Join("sessions", s.Session)) {
		t.Errorf("the copy is at %s, want it named after the session", pwdOf(t, dir, "hold"))
	}
	// The turn runs on and finishes normally: archiving did not stop it.
	release(dir, "hold")
	waitEnded(t, e, s.Turn)
	// It is archived, not closed: the copy is intact and it stays resumable.
	var one OneSession
	if code := getJSON(t, e, "/api/v0/sessions/"+s.Session, &one); code != 200 || one.Session.ID != s.Session || one.Session.Copy == nil ||
		one.Session.Copy.Kind != queue.WorkspaceCopy || one.Session.Copy.Name != s.Session || one.Session.Copy.Removed != nil ||
		one.Session.Archived == nil || one.Session.Closed != nil || !one.Session.Resumable {
		t.Fatalf("GET the archived session: %d %+v %+v", code, one.Session, one.Session.Copy)
	}
	// The default list hides it; archived=true shows it, still with its copy.
	// Each list read uses a fresh Snapshot: a reused one keeps a field the next
	// response omits (archived is omitempty), which would hide a real change.
	var hidden Snapshot
	if code := getJSON(t, e, "/api/v0/sessions", &hidden); code != 200 || len(hidden.Sessions) != 0 {
		t.Fatalf("default list hides the archived session: %d %+v", code, hidden.Sessions)
	}
	var shown Snapshot
	if code := getJSON(t, e, "/api/v0/sessions?archived=true", &shown); code != 200 || len(shown.Sessions) != 1 ||
		shown.Sessions[0].ID != s.Session || shown.Sessions[0].Archived == nil || !shown.Sessions[0].Resumable {
		t.Fatalf("archived=true shows it: %d %+v", code, shown.Sessions)
	}
	// Un-archive is reversible: it puts the session back in the default list.
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/"+s.Session, `{"archived":false}`); code != 200 || strings.Contains(out, `"archived":true`) {
		t.Fatalf("un-archive: %d %s", code, out)
	}
	var back Snapshot
	if code := getJSON(t, e, "/api/v0/sessions", &back); code != 200 || len(back.Sessions) != 1 ||
		back.Sessions[0].ID != s.Session || back.Sessions[0].Archived != nil {
		t.Fatalf("default list shows the un-archived session: %d %+v", code, back.Sessions)
	}
	// An archived session still takes a turn (archive is only a view). Done
	// last, as a new turn leaves the session open.
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/"+s.Session, `{"archived":true}`); code != 200 {
		t.Fatalf("re-archive: %d %s", code, out)
	}
	if code, out := as(t, e, token, "POST", "/api/v0/turns", `{"agent":"claude","session":"`+s.Session+`","prompt":"more"}`); code/100 != 2 {
		t.Errorf("a turn of an archived session: %d %s", code, out)
	}
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/nobody", `{"closed":true}`); code != 404 {
		t.Errorf("archive of no session: %d %s", code, out)
	}
}
