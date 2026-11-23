package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/store"
)

// A fake control for the record route: it keeps the event ids it is sent, and
// refuses a batch over the protocol's limit as the real one does.
type fakeControl struct {
	mu      sync.Mutex
	events  map[string]bool
	refused int
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/record") {
		var b jobs.RecordBatch
		if json.NewDecoder(r.Body).Decode(&b) != nil || len(b.Events) > jobs.MaxBatch {
			f.refused++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, e := range b.Events {
			var ev struct{ ID string }
			json.Unmarshal(e, &ev)
			f.events[ev.ID] = true
		}
		json.NewEncoder(w).Encode(jobs.RecordAck{Stored: len(b.Events)})
	}
}

// A runner holds no secret, so it sends none of its model calls: control's
// relay recorded each from its own copy. It sends the turn's own events and,
// of a call's events, only those its gateway made itself (a refused tool
// call, a failed replay, a call it refused before the relay).
func TestForwardLeavesRelayedCallsToControl(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.KeepCall(body.Call{Exchange: "x-run-1", Session: "s1", Vendor: "anthropic", Time: now, Size: 2,
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"a":1}`)}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.KeepReply(body.Reply{Exchange: "x-run-1", Status: 200, Stop: "end_turn", Time: now, Message: json.RawMessage(`{"b":2}`)}); err != nil {
		t.Fatal(err)
	}
	evs := []event.Event{
		{ID: "own-start", Kind: event.TurnStarted},
		{ID: "gw-end", Kind: event.TurnCompleted, Exchange: "x-run-1"},
		{ID: "gw-req", Kind: event.ModelRequested, Exchange: "x-run-1"},
		{ID: "gw-tool", Kind: event.ToolRequested, Exchange: "x-run-1"},
		{ID: "gw-refused", Kind: event.ToolRefused, Exchange: "x-run-1"},
		{ID: "gw-replay", Kind: event.ReplayFailed, Exchange: "x-run-1"},
		{ID: "gw-resp", Kind: event.ModelResponded, Exchange: "x-run-1"},
		{ID: "un-req", Kind: event.ModelRequested, Exchange: "x-run-2"},
		{ID: "un-resp", Kind: event.ModelResponded, Exchange: "x-run-2", Status: 502, Error: "the vendor could not be reached (dial)"},
		// A call the runner's gateway refused on its own, before the relay:
		// no credential, sent nowhere, so control never saw it (relayOnly).
		{ID: "gw-callrefused", Kind: event.CallRefused, Exchange: "x-run-2"},
		{ID: "own-closed", Kind: event.RunEnded},
	}
	for _, e := range evs {
		e.Time, e.Session, e.Turn = now, "s1", "t_1"
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeControl{events: map[string]bool{}}
	srv := httptest.NewServer(fc)
	r := &Runner{cfg: Config{Queue: srv.URL, Store: st, Forward: true, NoSecrets: true, Logf: t.Logf}, short: srv.Client(), token: "tok"}
	at := &attempt{r: r}
	at.a.ID, at.a.Number = "t_1", 1
	at.forward(context.Background(), "s1", true)
	srv.Close()
	st.Close()
	var sent []string
	for id := range fc.events {
		sent = append(sent, id)
	}
	want := map[string]bool{"own-start": true, "gw-refused": true, "gw-replay": true, "gw-callrefused": true, "own-closed": true}
	if len(fc.events) != len(want) {
		t.Fatalf("sent events %v, want the %d a no-secret runner sends", sent, len(want))
	}
	for id := range want {
		if !fc.events[id] {
			t.Fatalf("%s was not sent: %v", id, sent)
		}
	}
}

// approval.requested carries an exchange, so it is forwarded to control even in
// no-secrets mode (else a remote runner's ask never reaches control).
func TestRelayOnlyHasApprovalRequested(t *testing.T) {
	if !relayOnly[event.ApprovalRequested] {
		t.Fatal("approval.requested is not forwarded to control")
	}
}

// call.refused carries an exchange, so a runner's gateway that refuses a call
// on its own (no credential, sent nowhere) must forward it even in no-secrets
// mode, else that refusal is missing from control's record.
func TestRelayOnlyHasCallRefused(t *testing.T) {
	if !relayOnly[event.CallRefused] {
		t.Fatal("call.refused is not forwarded to control")
	}
}
