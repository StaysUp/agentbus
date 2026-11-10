package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/store"
)

// A fake control for the record routes: it keeps what it is sent, and
// refuses a batch over the protocol's limits as the real one does.
type fakeControl struct {
	mu      sync.Mutex
	events  map[string]bool
	calls   map[string]jobs.SentCall
	replies map[string]jobs.ReplyRecord
	refused int
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/record"):
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
	case strings.HasSuffix(r.URL.Path, "/calls"):
		var b jobs.CallBatch
		if json.NewDecoder(r.Body).Decode(&b) != nil || len(b.Calls)+len(b.Replies) > jobs.MaxBatch {
			f.refused++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, c := range b.Calls {
			for _, p := range c.Pieces {
				if p.Bytes != nil && (body.Piece{JSON: p.Bytes}).Hash() != p.Hash {
					f.refused++
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			f.calls[c.Exchange] = c
		}
		for _, rp := range b.Replies {
			if _, ok := f.calls[rp.Exchange]; ok {
				f.replies[rp.Exchange] = rp
			}
		}
		json.NewEncoder(w).Encode(jobs.CallAck{Stored: len(b.Calls), Missing: []string{}})
	}
}

// A backlog of more calls than one batch may hold, first sent at the turn's
// end, all reaches control with its replies, bytes exact.
func TestForwardSendsBacklogWithReplies(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const n = jobs.MaxBatch + 1
	now := time.Now().UTC()
	for i := range n {
		x := "x-run-" + strconv.Itoa(i)
		// Whitespace and <, > and &, which JSON's own encoding would change.
		msg := body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role": "user", "content": "<system-reminder> & ` + strconv.Itoa(i) + `"}`)}
		if err := st.KeepCall(body.Call{Exchange: x, Session: "s1", Vendor: "anthropic", Time: now, Size: 10,
			Request: body.Request{Top: json.RawMessage(`{"model": "m"}`), Pieces: []body.Piece{msg}}}); err != nil {
			t.Fatal(err)
		}
		if err := st.KeepReply(body.Reply{Exchange: x, Status: 200, Stop: "end_turn", Time: now, Message: json.RawMessage(`{"role": "assistant", "content": "<ok>"}`)}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.Append(event.Event{ID: "e" + strconv.Itoa(i), Kind: event.ModelRequested, Time: now, Session: "s1", Turn: "t_1", Exchange: x}); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeControl{events: map[string]bool{}, calls: map[string]jobs.SentCall{}, replies: map[string]jobs.ReplyRecord{}}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	r := &Runner{cfg: Config{Queue: srv.URL, Store: st, Forward: true, Logf: t.Logf}, short: srv.Client(), token: "tok"}
	at := &attempt{r: r}
	at.a.ID, at.a.Number = "t_1", 1
	at.forward(context.Background(), "s1", true)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.refused != 0 || len(fc.events) != n || len(fc.calls) != n || len(fc.replies) != n {
		t.Fatalf("refused %d, events %d, calls %d, replies %d of %d", fc.refused, len(fc.events), len(fc.calls), len(fc.replies), n)
	}
	c := fc.calls["x-run-7"]
	if string(c.Top) != `{"model": "m"}` || string(c.Pieces[0].Bytes) != `{"role": "user", "content": "<system-reminder> & 7"}` ||
		string(fc.replies["x-run-7"].Message) != `{"role": "assistant", "content": "<ok>"}` {
		t.Fatalf("bytes changed: %s %s %s", c.Top, c.Pieces[0].Bytes, fc.replies["x-run-7"].Message)
	}
}

// A call of many small pieces is weighed with their fields: a batch never
// goes over the body limit, and the calls after it still go.
func TestForwardWeighsPieceFields(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	var many []body.Piece
	for i := range 16000 {
		many = append(many, body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`)})
	}
	for i, pieces := range [][]body.Piece{many, {{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"after":1}`)}}} {
		x := "x-run-" + strconv.Itoa(i)
		if err := st.KeepCall(body.Call{Exchange: x, Session: "s1", Vendor: "anthropic", Time: now, Request: body.Request{Top: json.RawMessage(`{}`), Pieces: pieces}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.Append(event.Event{ID: "e" + strconv.Itoa(i), Kind: event.ModelRequested, Time: now, Session: "s1", Turn: "t_1", Exchange: x}); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeControl{events: map[string]bool{}, calls: map[string]jobs.SentCall{}, replies: map[string]jobs.ReplyRecord{}}
	srv := httptest.NewServer(http.MaxBytesHandler(fc, jobs.MaxBody))
	defer srv.Close()
	r := &Runner{cfg: Config{Queue: srv.URL, Store: st, Forward: true, Logf: t.Logf}, short: srv.Client(), token: "tok"}
	at := &attempt{r: r}
	at.a.ID, at.a.Number = "t_1", 1
	at.forward(context.Background(), "s1", true)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.refused != 0 {
		t.Fatalf("control refused %d batches", fc.refused)
	}
	if _, ok := fc.calls["x-run-1"]; !ok {
		t.Fatalf("the call after the big one did not go: %d calls", len(fc.calls))
	}
}

// A call well under the body limit, though over half of it, goes whole.
func TestForwardSendsALargeCall(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	big := body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"c":"` + strings.Repeat("a", 800000) + `"}`)}
	if err := st.KeepCall(body.Call{Exchange: "x-big", Session: "s1", Vendor: "anthropic", Time: now, Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{big}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Append(event.Event{ID: "e1", Kind: event.ModelRequested, Time: now, Session: "s1", Turn: "t_1", Exchange: "x-big"}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeControl{events: map[string]bool{}, calls: map[string]jobs.SentCall{}, replies: map[string]jobs.ReplyRecord{}}
	srv := httptest.NewServer(http.MaxBytesHandler(fc, jobs.MaxBody))
	defer srv.Close()
	r := &Runner{cfg: Config{Queue: srv.URL, Store: st, Forward: true, Logf: t.Logf}, short: srv.Client(), token: "tok"}
	at := &attempt{r: r}
	at.a.ID, at.a.Number = "t_1", 1
	at.forward(context.Background(), "s1", true)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, ok := fc.calls["x-big"]; !ok || fc.refused != 0 {
		t.Fatalf("the call did not go: refused %d", fc.refused)
	}
}
