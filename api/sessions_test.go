package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// GET /api/v0/sessions pages like GET /turns: newest activity first, limit per
// page, next_before to continue, so the list is never the whole snapshot.
func TestListSessionsPages(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for i, tc := range []struct{ id, turn string }{{"s1", "t1"}, {"s2", "t2"}, {"s3", "t3"}} {
		if _, _, err := e.store.Append(event.Event{ID: tc.turn, Kind: event.TurnStarted, Session: tc.id, Turn: tc.turn, Text: "x",
			Time: base.Add(time.Duration(i+1) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	var p1 Snapshot
	if getJSON(t, e, "/api/v0/sessions?limit=2", &p1) != http.StatusOK {
		t.Fatal("page one")
	}
	if len(p1.Sessions) != 2 || p1.Sessions[0].ID != "s3" || p1.Sessions[1].ID != "s2" || p1.NextBefore == nil {
		t.Fatalf("page one: %+v next %v", p1.Sessions, p1.NextBefore)
	}
	// s1 gains activity after page one; the pinned page two still includes it.
	if _, _, err := e.store.Append(event.Event{ID: "t1b", Kind: event.TurnStarted, Session: "s1", Turn: "t1b", Text: "y",
		Time: base.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var p2 Snapshot
	page2 := "/api/v0/sessions?limit=2&before=" + strconv.FormatInt(*p1.NextBefore, 10) +
		"&store=" + p1.Store + "&after=" + strconv.FormatInt(p1.Seq, 10)
	if getJSON(t, e, page2, &p2) != http.StatusOK {
		t.Fatal("page two")
	}
	if len(p2.Sessions) != 1 || p2.Sessions[0].ID != "s1" || p2.NextBefore != nil {
		t.Fatalf("page two: %+v next %v", p2.Sessions, p2.NextBefore)
	}
	// A pin beyond the store's latest seq is refused, not served against the
	// newest (which would let later events skip a session).
	var dummy Snapshot
	if getJSON(t, e, "/api/v0/sessions?limit=2&store="+p1.Store+"&after=9999999", &dummy) != http.StatusBadRequest {
		t.Fatal("a future pin was not refused")
	}
}

// An explicit after=0 pins to the empty snapshot, not the latest: a session
// that arrives after is not in the page, and seq stays 0. (Round-3 P2: zero
// was read as "no pin".)
func TestListSessionsPinZero(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.store.Append(event.Event{ID: "t1", Kind: event.TurnStarted, Session: "s1", Turn: "t1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	var p Snapshot
	if getJSON(t, e, "/api/v0/sessions?store="+e.store.ID()+"&after=0", &p) != http.StatusOK {
		t.Fatal("after=0")
	}
	if len(p.Sessions) != 0 || p.Seq != 0 {
		t.Fatalf("after=0 should pin to the empty snapshot: %+v seq %d", p.Sessions, p.Seq)
	}
}

// The session query of the event stream and the turn list is checked for
// every token, the owner's too (O2.3 review P2): the door skipped the place
// check for a grant that reaches everywhere, so the owner's ?session= opened
// a stream of no session instead of 400.
func TestSessionQueryShapeOwner(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/api/v0/events?session=",
		"/api/v0/events?session=a&session=b",
		"/api/v0/events?session=a%20b",
		"/api/v0/turns?session=",
		"/api/v0/turns?session=a&session=b",
	} {
		// A stream the door lets through stays open: the deadline fails it
		// rather than hang the test.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := http.DefaultClient.Do(e.req(t, "GET", path, nil).WithContext(ctx))
		if err != nil {
			t.Errorf("GET %s: %v, want 400", path, err)
		} else {
			resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Errorf("GET %s: %d, want 400", path, resp.StatusCode)
			}
		}
		cancel()
	}
}
