package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// renameReq sends PATCH /api/v0/sessions/{id} with the main token.
func renameReq(t *testing.T, e *env, id, body string) (int, string) {
	t.Helper()
	r := e.req(t, "PATCH", "/api/v0/sessions/"+id, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	resp, out := do(t, r)
	return resp.StatusCode, out
}

// sessionRow is the session's row in GET /api/v0/sessions.
func sessionRow(t *testing.T, e *env, id string) store.Session {
	t.Helper()
	resp, out := do(t, e.req(t, "GET", "/api/v0/sessions", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("sessions: %d %s", resp.StatusCode, out)
	}
	var snap store.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	for _, s := range snap.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s not listed: %s", id, out)
	return store.Session{}
}

// A rename stores a session.renamed with the old and new title. The list
// names the session by its latest rename, its last activity stays, and every
// earlier name is still in the session's events.
func TestRenameSession(t *testing.T) {
	e := newEnv(t)
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if _, _, err := e.store.Append(event.Event{ID: "evt_t1", Kind: event.TurnStarted, Time: at,
		Session: "s1", Turn: "t1", Text: "Fix the login bug"}); err != nil {
		t.Fatal(err)
	}
	if got := sessionRow(t, e, "s1").Title; got != "Fix the login bug" {
		t.Fatalf("title before a rename: %q", got)
	}
	code, out := renameReq(t, e, "s1", `{"title":"  Login fix  "}`)
	var r Renamed
	if code != 200 || json.Unmarshal([]byte(out), &r) != nil || r.Title != "Login fix" || r.OldTitle != "Fix the login bug" || r.Seq == 0 {
		t.Fatalf("first rename: %d %s", code, out)
	}
	code, out = renameReq(t, e, "s1", `{"title":"Settings page"}`)
	if code != 200 || json.Unmarshal([]byte(out), &r) != nil || r.Title != "Settings page" || r.OldTitle != "Login fix" {
		t.Fatalf("second rename: %d %s", code, out)
	}
	row := sessionRow(t, e, "s1")
	if row.Title != "Settings page" || !row.Last.Equal(at) || !row.Started.Equal(at) || row.Turns != 1 {
		t.Errorf("row after two renames: %+v", row)
	}

	resp, out := do(t, e.req(t, "GET", "/api/v0/sessions/s1/events", nil))
	var page struct{ Events []event.Event }
	if resp.StatusCode != 200 || json.Unmarshal([]byte(out), &page) != nil {
		t.Fatalf("events: %d %s", resp.StatusCode, out)
	}
	var names [][2]string
	for _, ev := range page.Events {
		if ev.Kind == event.SessionRenamed {
			if ev.Turn != "" || ev.Vendor != "" {
				t.Errorf("a rename names no turn and no vendor: %+v", ev)
			}
			names = append(names, [2]string{ev.OldTitle, ev.Title})
		}
	}
	want := [][2]string{{"Fix the login bug", "Login fix"}, {"Login fix", "Settings page"}}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("renames in the record: %v, want %v", names, want)
	}
}

// A session with no message has no title; its first rename has no old_title.
// A title of exactly 120 characters is kept whole.
func TestRenameSessionWithoutTitle(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.store.Append(event.Event{ID: "evt_o", Kind: event.SessionOpened, Time: time.Now().UTC(),
		Session: "s2", Hold: "h1"}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", store.TitleRunes)
	code, out := renameReq(t, e, "s2", `{"title":"`+long+`"}`)
	if code != 200 || strings.Contains(out, "old_title") {
		t.Fatalf("first rename: %d %s", code, out)
	}
	if got := sessionRow(t, e, "s2").Title; got != long {
		t.Errorf("title: %q", got)
	}
}

// A rename is refused, and nothing stored, for a bad body or title, or an
// unknown session.
func TestRenameSessionRefused(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.store.Append(event.Event{ID: "evt_t1", Kind: event.TurnStarted, Time: time.Now().UTC(),
		Session: "s1", Turn: "t1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	before := e.store.Last()
	for _, tc := range []struct {
		id, body, want string
		code           int
	}{
		{"s1", ``, badRename, 400},
		{"s1", `[]`, badRename, 400},
		{"s1", `{"title":1}`, badRename, 400},
		{"s1", `{"title":"x","extra":true}`, badRename, 400},
		{"s1", `{}`, badRename, 400},
		{"s1", `{"closed":false}`, "closed may only be true", 400},
		{"s1", `{"title":"   "}`, titleEmpty, 400},
		{"s1", `{"title":"` + strings.Repeat("a", store.TitleRunes+1) + `"}`, titleTooLong, 400},
		{"s1", `{"title":"two\nlines"}`, titleNotLine, 400},
		{"s1", `{"title":"a\ttab"}`, titleNotLine, 400},
		{"s1", `{"title":"two\u2028lines"}`, titleNotLine, 400},
		{"s1", `{"title":"two\u2029paragraphs"}`, titleNotLine, 400},
		{"s1", `{"title":"next\u0085line"}`, titleNotLine, 400},
		{"nobody", `{"title":"x"}`, "no such session", 404},
	} {
		code, out := renameReq(t, e, tc.id, tc.body)
		if code != tc.code || !strings.Contains(out, tc.want) {
			t.Errorf("%s %q: %d %s, want %d %q", tc.id, tc.body, code, out, tc.code, tc.want)
		}
	}
	if e.store.Last() != before {
		t.Errorf("a refused rename stored an event: seq %d, was %d", e.store.Last(), before)
	}
	// Only PATCH renames.
	resp, _ := do(t, e.req(t, "PUT", "/api/v0/sessions/s1", strings.NewReader(`{"title":"x"}`)))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d, want 405", resp.StatusCode)
	}
}
