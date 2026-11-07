package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

type rewriteFunc func(session, turn string, body []byte) Rewrite

func (f rewriteFunc) Rewrite(session, turn string, body []byte) Rewrite {
	return f(session, turn, body)
}

// keepAll keeps calls and replies in memory.
type keepAll struct {
	mu      sync.Mutex
	calls   []body.Call
	replies []body.Reply
}

func (k *keepAll) KeepCall(c body.Call) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, c)
	return nil
}

func (k *keepAll) KeepReply(r body.Reply) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.replies = append(k.replies, r)
	return nil
}

func rewriteGateway(t *testing.T, rw Rewriter) (*httptest.Server, *event.Memory, *keepAll, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okReply)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	k := &keepAll{}
	g.KeepBodies(k)
	g.SetRewriter(rw)
	srv := httptest.NewServer(g)
	t.Cleanup(func() { srv.Close(); g.Close() })
	return srv, mem, k, &got
}

const runBody = `{"model":"m","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"hi"}]}`

// A run's request the rewriter changes goes on changed, is kept changed, and
// its model.requested says how many messages were replayed. Its reply is
// kept, rebuilt. A request without a run's turn is not given to the rewriter.
func TestRewriterChangesRunRequests(t *testing.T) {
	changed := `{"model":"m","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"before"},{"role":"assistant","content":[{"type":"text","text":"ok"}]},{"role":"user","content":"hi"}]}`
	var seen []string
	srv, mem, k, got := rewriteGateway(t, rewriteFunc(func(s, turn string, b []byte) Rewrite {
		seen = append(seen, s+"/"+turn)
		return Rewrite{Body: []byte(changed), Replayed: 2}
	}))
	resp, err := rawClient().Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != okReply {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	evs := waitFor(t, mem, 3)
	if len(*got) != 1 || (*got)[0] != changed {
		t.Errorf("upstream got %v, want the changed body", *got)
	}
	for _, e := range evs {
		if e.Kind == event.ModelRequested && e.Replayed != 2 {
			t.Errorf("model.requested replayed %d, want 2", e.Replayed)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		k.mu.Lock()
		n, r := len(k.calls), len(k.replies)
		k.mu.Unlock()
		if n == 1 && r == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	k.mu.Lock()
	if len(k.calls) != 1 || len(k.calls[0].Pieces) != 4 {
		t.Errorf("kept %d calls, want the changed one with 4 pieces", len(k.calls))
	}
	if len(k.replies) != 1 || string(k.replies[0].Message) != `{"role":"assistant","content":[{"type":"text","text":"ok"}]}` ||
		k.replies[0].Stop != "end_turn" || k.replies[0].Exchange != k.calls[0].Exchange {
		t.Errorf("kept replies %+v", k.replies)
	}
	k.mu.Unlock()

	// Hand-pointed traffic names no turn in its URL: never rewritten.
	resp, err = rawClient().Post(srv.URL+"/s/s1/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(seen) != 1 || seen[0] != "s1/t_1" {
		t.Errorf("rewriter saw %v, want only s1/t_1", seen)
	}
}

// A refused request never reaches the vendor: the agent gets the refusal,
// replay.failed is emitted, and the agent's own body is kept.
func TestRewriterRefusal(t *testing.T) {
	srv, mem, k, got := rewriteGateway(t, rewriteFunc(func(string, string, []byte) Rewrite {
		return Rewrite{Refuse: &Refusal{Status: 400, ContentType: "application/json", Body: []byte(`{"type":"error"}`)},
			Events: []event.Event{{Kind: event.ReplayFailed, Error: "layout"}}}
	}))
	resp, err := rawClient().Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || string(b) != `{"type":"error"}` || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	evs := waitFor(t, mem, 1)
	if evs[0].Kind != event.ReplayFailed || evs[0].Session != "s1" || evs[0].Turn != "t_1" || evs[0].Exchange == "" || evs[0].Error != "layout" {
		t.Errorf("event %+v", evs[0])
	}
	time.Sleep(50 * time.Millisecond)
	if len(*got) != 0 {
		t.Errorf("upstream got %d requests, want none", len(*got))
	}
	if n := len(mem.Events()); n != 1 {
		t.Errorf("%d events, want only replay.failed", n)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.calls) != 1 {
		t.Errorf("kept %d calls, want the refused one", len(k.calls))
	} else {
		var last json.RawMessage = k.calls[0].Pieces[len(k.calls[0].Pieces)-1].JSON
		if string(last) != `{"role":"user","content":"hi"}` {
			t.Errorf("kept %s, want the agent's own body", last)
		}
	}
}

// slowSink holds every event until released.
type slowSink struct {
	release chan struct{}
	mu      sync.Mutex
	got     []event.Event
}

func (s *slowSink) Emit(e event.Event) {
	<-s.release
	s.mu.Lock()
	s.got = append(s.got, e)
	s.mu.Unlock()
}

// A reply's events and its kept reply are written off the reply's path, so
// they can land after the agent has its last bytes and exits. Settle returns
// only once a run turn's calls are fully written: every event handed to the
// sink and the reply kept. Another turn does not wait, and a turn that never
// settles gives false at the timeout.
func TestSettleWaitsForTheTurnsWrites(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okReply)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	sink := &slowSink{release: make(chan struct{})}
	g := New(u, anthropic.Adapter{}, sink)
	k := &keepAll{}
	g.KeepBodies(k)
	srv := httptest.NewServer(g)
	t.Cleanup(func() { srv.Close(); g.Close() })

	resp, err := rawClient().Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	// The agent has its reply; the events are still held by the sink.
	if g.Settle("s1", "t_1", 50*time.Millisecond) {
		t.Fatal("settled while the turn's events were still held")
	}
	if !g.Settle("s1", "t_other", 50*time.Millisecond) {
		t.Error("a turn with no calls did not settle at once")
	}
	done := make(chan bool)
	go func() { done <- g.Settle("s1", "t_1", 5*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	close(sink.release)
	if !<-done {
		t.Fatal("did not settle after the sink took the events")
	}
	sink.mu.Lock()
	var responded bool
	for _, e := range sink.got {
		responded = responded || e.Kind == event.ModelResponded
	}
	sink.mu.Unlock()
	k.mu.Lock()
	replies := len(k.replies)
	k.mu.Unlock()
	if !responded || replies != 1 {
		t.Errorf("settled with model.responded %v and %d replies kept, want both written", responded, replies)
	}
}

// Every way an exchange ends releases it: a refusal and an unreachable vendor.
func TestSettleAfterRefusalAndUpstreamDown(t *testing.T) {
	srv, _, _, _ := rewriteGateway(t, rewriteFunc(func(string, string, []byte) Rewrite {
		return Rewrite{Refuse: &Refusal{Status: 400, ContentType: "application/json", Body: []byte(`{}`)}}
	}))
	resp, err := rawClient().Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	g := srv.Config.Handler.(*Gateway)
	if !g.Settle("s1", "t_1", 2*time.Second) {
		t.Error("a refused exchange never settled")
	}
	u, _ := url.Parse("http://127.0.0.1:1")
	down := New(u, anthropic.Adapter{}, &event.Memory{})
	dsrv := httptest.NewServer(down)
	t.Cleanup(func() { dsrv.Close(); down.Close() })
	resp, err = rawClient().Post(dsrv.URL+"/s/s1/t/t_2/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !down.Settle("s1", "t_2", 2*time.Second) {
		t.Error("an exchange the vendor never answered did not settle")
	}
}

// Barriers count against the queues' bounds like their own entries. Behind
// a blocked keeper or sink, a barrier past the bound is not queued and
// reports false; it is never kept without limit.
func TestBarriersAreBounded(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	first := true
	keep := body.KeeperFunc(func(body.Call) error {
		if first {
			first = false
			close(started)
		}
		<-release
		return nil
	})
	q := newBodyQueue(emptySplit{}, keep, 1<<20, 2)
	q.push(pendingBody{exchange: "x-0"})
	<-started
	queued := 0
	for i := 0; i < 10000; i++ {
		if q.barrier(func() {}) {
			queued++
		}
	}
	q.mu.Lock()
	waiting := len(q.pending)
	q.mu.Unlock()
	if queued != 2 || waiting != 2 {
		t.Errorf("body queue: %d barriers queued, %d waiting, want 2 and 2", queued, waiting)
	}
	close(release)
	q.close()

	hold := make(chan struct{})
	eq := newQueue(sinkFunc(func(event.Event) { <-hold }), 4096, dropReport)
	eq.push(event.Event{Kind: event.ModelRequested}) // taken by the sink, which holds it
	time.Sleep(20 * time.Millisecond)
	queued = 0
	for i := 0; i < 10000; i++ {
		if eq.barrier(func() {}) {
			queued++
		}
	}
	if queued == 0 || queued > 4096/barrierSize {
		t.Errorf("event queue: %d barriers queued, want between 1 and %d", queued, 4096/barrierSize)
	}
	close(hold)
	eq.close()
}

// An exchange whose writes could not be confirmed leaves its turn unsettled:
// Settle reports false, never settled.
func TestSettleReportsUnconfirmedWrites(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	g := New(u, anthropic.Adapter{}, &event.Memory{})
	t.Cleanup(g.Close)
	ex := &exchange{}
	g.track(ex, "s1", "t_1")
	ex.done(false)
	if g.Settle("s1", "t_1", time.Second) {
		t.Error("settled although a call's writes were not confirmed")
	}
	ex2 := &exchange{}
	g.track(ex2, "s1", "t_2")
	g.flushed(ex2.done)
	if !g.Settle("s1", "t_2", time.Second) {
		t.Error("a confirmed call did not settle")
	}
}

// Between the queue taking its lost count and the events.dropped report
// reaching the sink, a barrier must not fire: Settle would report settled
// before the store knows of the loss.
func TestBarrierWaitsForTheDropReport(t *testing.T) {
	entered, hold := make(chan struct{}), make(chan struct{})
	q := newQueue(sinkFunc(func(e event.Event) {
		if e.Kind == event.EventsDropped {
			close(entered)
			<-hold
		}
	}), 600, dropReport)
	q.push(event.Event{Kind: event.ModelRequested, Text: strings.Repeat("x", 1000)}) // over the bound: lost
	<-entered                                                                        // the report is taken and being written
	fired := make(chan struct{})
	if !q.barrier(func() { close(fired) }) {
		t.Fatal("barrier not queued")
	}
	select {
	case <-fired:
		t.Fatal("the barrier fired before the drop report was written")
	case <-time.After(30 * time.Millisecond):
	}
	close(hold)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the barrier did not fire after the report")
	}
	q.close()
}
