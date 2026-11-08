//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// The write after the child started was ignored, so with a store that could
// not write the API answered 202 and the child ran with no record.
// session.opened is now written first: a failed write answers 500 and starts
// nothing.
func TestRunRefusedWhenStoreCannotWrite(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "ran")
	e := resumableEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", `touch "$0"`, mark}}, nil
	})
	e.store.Close() // every Append now fails
	resp, body := do(t, e.req(t, "POST", "/api/v0/turns",
		strings.NewReader(`{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"hi"}`)))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("%d %s, want 500", resp.StatusCode, body)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the child started although nothing could be stored")
	}
}

// The API kept every session's agent and folder in memory forever. TurnSink
// now reads them from the store's session.opened, so the API keeps nothing,
// and a turn.started that arrives after its child exited is still named.
func TestTurnSinkNamesTurnsAfterChildExits(t *testing.T) {
	e := resumableEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, nil
	})
	folder := t.TempDir()
	var ids []string
	for i := 0; i < 3; i++ {
		resp, body := do(t, e.req(t, "POST", "/api/v0/turns",
			strings.NewReader(`{"agent":"codex","folder":"`+folder+`","prompt":"hi"}`)))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		var out Started
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, out.ID)
	}
	for _, id := range ids {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if c, _ := e.store.Continuation(id); c.Known && !c.Open {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never closed", id)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	sink := e.eng.TurnSink(e.store)
	sink.Emit(event.Event{ID: "evt_late", Kind: event.TurnStarted, Time: time.Now(), Session: ids[0], Turn: "t1", Text: "late"})
	sink.Emit(event.Event{ID: "evt_hand", Kind: event.TurnStarted, Time: time.Now(), Session: "byhand", Turn: "t1", Text: "hand"})
	for session, want := range map[string][2]string{ids[0]: {"codex", folder}, "byhand": {"", ""}} {
		recs, err := e.store.After(0, store.Filter{Session: session, Only: true}, 100)
		if err != nil {
			t.Fatal(err)
		}
		var got *event.Event
		for _, r := range recs {
			var ev event.Event
			if json.Unmarshal(r.Data, &ev) == nil && ev.Kind == event.TurnStarted {
				got = &ev
			}
		}
		if got == nil || got.Agent != want[0] || got.Folder != want[1] {
			t.Fatalf("%s: turn.started %+v, want agent %q folder %q", session, got, want[0], want[1])
		}
	}
}
