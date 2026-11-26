//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One rule decides what a turn token reads (his call, 2026-11-25): the sessions
// of the workspaces in its authority. A Direct session's authority is its
// machine; a Copy session's is its own workspace and the workspaces its session
// made. The request tree no longer decides a read.
func TestTurnTokenReadsItsAuthority(t *testing.T) {
	perm := filepath.Join(t.TempDir(), "permissions.toml")
	os.WriteFile(perm, nil, 0o600)
	e, dir := askEnvWith(t, perm)
	defer func() {
		for _, p := range []string{"elsewhere", "walled", "coord", "builder", "reviewer"} {
			release(dir, p)
		}
	}()
	f1, f2, f3 := t.TempDir(), t.TempDir(), t.TempDir()
	ref2, _ := e.folderRef(t, f2)

	reads := func(who, tok, method, path string) {
		t.Helper()
		if code, out := as(t, e, tok, method, path, ""); code != 200 {
			t.Errorf("%s: %s %s: %d %s, want 200", who, method, path, code, out)
		}
	}
	refused := func(who, tok, method, path, body, want string) {
		t.Helper()
		if code, out := as(t, e, tok, method, path, body); code != 403 || !strings.Contains(out, want) {
			t.Errorf("%s: %s %s: %d %s, want 403 %q", who, method, path, code, out, want)
		}
	}

	// The person's other sessions on this machine, each its own request tree:
	// one working in f3 as it is, one in a copy of f3.
	elsewhere := e.firstTurn(t, f3, "claude", "elsewhere", false)
	walled := e.firstTurn(t, f3, "claude", "walled", true)

	// Case 1: a Direct coordinator reads the person's other sessions through
	// the API, as it can read the same store on disk.
	e.firstTurn(t, f1, "claude", "coord", false)
	coord := tokenOf(t, dir, "coord")
	for _, s := range []Started{elsewhere, walled} {
		reads("direct coordinator", coord, "GET", "/api/v0/sessions/"+s.Session)
		reads("direct coordinator", coord, "GET", "/api/v0/sessions/"+s.Session+"/events")
		reads("direct coordinator", coord, "GET", "/api/v0/sessions/"+s.Session+"/search?q=x")
		reads("direct coordinator", coord, "GET", "/api/v0/turns?session="+s.Session)
		reads("direct coordinator", coord, "GET", "/api/v0/turns/"+s.Turn)
	}
	// Watching follows reading (mayReadTurn).
	if code, out := as(t, e, coord, "POST", "/api/v0/ons", `{"turn":"`+elsewhere.Turn+`"}`); code != 201 {
		t.Errorf("direct coordinator watches another tree's turn: %d %s", code, out)
	}
	// It stops any turn it may read, as its owner may (O2.3): a turn the
	// person started too.
	if code, out := as(t, e, coord, "POST", "/api/v0/turns/"+elsewhere.Turn+"/stop", ""); code == 403 {
		t.Errorf("direct coordinator stops a turn on its machine: %d %s", code, out)
	}

	// Case 2: the coordinator makes a Copy workspace, runs a builder there,
	// then asks a reviewer, whose session opens in the coordinator's own
	// workspace (as review_turn does). The reviewer reads the builder's turn.
	code, out := as(t, e, coord, "POST", "/api/v0/workspaces", `{"folder_ref":"`+ref2+`"}`)
	if code != 201 {
		t.Fatalf("make the copy: %d %s", code, out)
	}
	var ws struct {
		ID string `json:"workspace"`
	}
	if err := json.Unmarshal([]byte(out), &ws); err != nil {
		t.Fatal(err)
	}
	code, out = as(t, e, coord, "POST", "/api/v0/sessions", `{"workspace":"`+ws.ID+`"}`)
	if code != 201 {
		t.Fatalf("session in the copy: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	code, out = as(t, e, coord, "POST", "/api/v0/turns", `{"session":"`+sc.Session+`","agent":"claude","prompt":"builder"}`)
	if code != 202 {
		t.Fatalf("start the builder: %d %s", code, out)
	}
	var builder Started
	if err := json.Unmarshal([]byte(out), &builder); err != nil {
		t.Fatal(err)
	}

	// Case 3: the builder, in a copy, reads no session outside its authority:
	// not the coordinator's, not the person's others.
	btok := tokenOf(t, dir, "builder")
	for _, s := range []Started{elsewhere, walled} {
		refused("builder in a copy", btok, "GET", "/api/v0/sessions/"+s.Session+"/events", "", turnSessionRefused)
		refused("builder in a copy", btok, "GET", "/api/v0/turns/"+s.Turn, "", turnSessionRefused)
		refused("builder in a copy", btok, "PATCH", "/api/v0/sessions/"+s.Session, `{"title":"x"}`, turnSessionRefused)
		refused("builder in a copy", btok, "POST", "/api/v0/ons", `{"turn":"`+s.Turn+`"}`, turnOnRefused)
	}
	refused("builder in a copy", btok, "GET", "/api/v0/turns?session="+elsewhere.Session, "", turnSessionRefused)
	refused("builder in a copy", btok, "POST", "/api/v0/turns/"+walled.Turn+"/stop", "", turnStopRefused)
	// Review P1, 2026-11-25: an empty place is a missing place, refused at
	// the door for every token, never read as "no place" and allowed. The
	// event stream with ?session= streamed the events of no session, the
	// changes.previewed diffs of every workspace among them.
	for who, tok := range map[string]string{"builder in a copy": btok, "direct coordinator": coord} {
		// A stream the door lets through stays open, so the check has a
		// deadline: an open stream fails it rather than hang the test.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		r := e.req(t, "GET", "/api/v0/events?session=", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Errorf("%s: GET /api/v0/events?session=: %v, want 403", who, err)
		} else {
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 403 || !strings.Contains(string(out), turnSessionRefused) {
				t.Errorf("%s: GET /api/v0/events?session=: %d %s, want 403", who, resp.StatusCode, out)
			}
		}
		cancel()
	}

	release(dir, "builder")
	waitEnded(t, e, builder.Turn)
	ask(t, e, coord, `{"agent":"claude","prompt":"reviewer"}`)
	release(dir, "coord")
	rtok := tokenOf(t, dir, "reviewer")
	reads("reviewer", rtok, "GET", "/api/v0/turns/"+builder.Turn)
	reads("reviewer", rtok, "GET", "/api/v0/turns/"+builder.Turn+"/diff")
	reads("reviewer", rtok, "GET", "/api/v0/turns/"+builder.Turn+"/changes")
	reads("reviewer", rtok, "GET", "/api/v0/sessions/"+builder.Session+"/events")
}
