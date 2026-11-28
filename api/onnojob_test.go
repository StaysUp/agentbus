package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// onEnv is an API over seedReach's sessions with a queue, and abr_copy, a turn
// token of s_asker (a Copy session in ws_a: its reach is ws_a and ws_made).
func onEnv(t *testing.T) (*env, *queue.Queue) {
	t.Helper()
	grants := map[string]run.Grant{"abr_copy": {Session: "s_asker", Turn: "t_copy", Root: "s_asker"}}
	e, _ := rulesEnv(t, grants)
	seedReach(t, e.store)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	e.api.cfg.Queue = q
	return e, q
}

// POST /api/v0/ons registers an on over an event kind with a session filter
// (0.6.8). A turn token may register one only when the session it watches and
// the session it delivers to are both in its reach; its own session is the
// target when it names none. An on may not deliver in a circle, a session
// must exist, and the body must name the event and the filter.
func TestOnOverASession(t *testing.T) {
	e, q := onEnv(t)
	post := func(tok, body string, want int, says string) OnView {
		t.Helper()
		code, out := callAs(t, e, tok, "POST", "/api/v0/ons", body)
		if code != want || !strings.Contains(out, says) {
			t.Fatalf("%s POST /api/v0/ons %s: %d %s, want %d %q", tok, body, code, out, want, says)
		}
		var v OnView
		_ = json.Unmarshal([]byte(out), &v)
		return v
	}
	on := func(watch, to string) string {
		b := `{"event":"turn.completed","filter":{"session":"` + watch + `"}`
		if to != "" {
			b += `,"session":"` + to + `"`
		}
		return b + `}`
	}
	v := post("abr_copy", on("s_mine", ""), http.StatusCreated, `"filter":{"session":"s_mine"}`)
	if v.Session != "s_asker" || v.By == nil || v.By.Turn != "t_copy" || v.Turn != "" {
		t.Fatalf("a turn token's on: %+v, want it delivered to its own session, made by its turn", v)
	}
	// The same on again is the same on, so each reply is delivered once.
	if again := post("abr_copy", on("s_mine", "s_asker"), http.StatusCreated, ""); again.ID != v.ID {
		t.Fatalf("the same on again: %s, want %s", again.ID, v.ID)
	}
	post("abr_copy", on("s_made", "s_mine"), http.StatusCreated, `"session":"s_mine"`)
	// Outside its reach: the watched session, or the target.
	post("abr_copy", on("s_other", ""), http.StatusForbidden, turnSessionRefused)
	post("abr_copy", on("s_mine", "s_other"), http.StatusForbidden, turnSessionRefused)
	// In a circle: into the session it watches, or back along s_mine -> s_asker.
	post("abr_copy", on("s_asker", ""), http.StatusConflict, "circle")
	post("abr_copy", on("s_asker", "s_mine"), http.StatusConflict, "circle")
	// The main token names the target; any session it names must exist.
	post(token, on("s_other", ""), http.StatusBadRequest, "session is required")
	post(token, on("s_other", "s_e"), http.StatusCreated, `"session":"s_e"`)
	post(token, on("s_nope", "s_e"), http.StatusNotFound, "no such session")
	post(token, `{"filter":{"session":"s_other"},"session":"s_e"}`, http.StatusBadRequest, "event is required")
	post(token, `{"event":"turn.completed","session":"s_e"}`, http.StatusBadRequest, "filter.session is required")
	ons, err := q.ListOns(queue.DefaultTeam)
	if err != nil || len(ons) != 3 {
		t.Fatalf("ons registered: %d %v, want 3", len(ons), err)
	}
}

// The forms 0.6.8 removed answer 410 with a plain message that says what to
// send instead, for every token, and register nothing: the on over one turn
// (POST /api/v0/ons with turn) and POST /api/v0/turns' delivery.
func TestRemovedOnFormsAnswerGone(t *testing.T) {
	e, q := onEnv(t)
	for _, tok := range []string{token, "abr_copy"} {
		code, out := callAs(t, e, tok, "POST", "/api/v0/ons", `{"turn":"t_x","session":"s_mine"}`)
		if code != http.StatusGone || !strings.Contains(out, "removed in 0.6.8") || !strings.Contains(out, "filter") {
			t.Errorf("%s POST /api/v0/ons with turn: %d %s, want 410 saying what to send", tok, code, out)
		}
		code, out = callAs(t, e, tok, "POST", "/api/v0/turns", `{"session":"s_mine","agent":"claude","prompt":"hi","delivery":"s_asker"}`)
		if code != http.StatusGone || !strings.Contains(out, "delivery was removed in 0.6.8") {
			t.Errorf("%s POST /api/v0/turns with delivery: %d %s, want 410", tok, code, out)
		}
	}
	if ons, err := q.ListOns(queue.DefaultTeam); err != nil || len(ons) != 0 {
		t.Fatalf("ons registered: %v %v", ons, err)
	}
	if jobs, err := q.List(queue.DefaultTeam, queue.Filter{}, 10); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs made: %v %v", jobs, err)
	}
}
