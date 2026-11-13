package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// relayFor serves calls as control's relay does: each through g.ServeRelayed,
// for session s1 and turn t_1, with the runner's exchange name and a
// credential added on the way out.
func relayFor(t *testing.T, upstream string, refuse *Refusal) (*httptest.Server, *Gateway, *event.Memory, *lines) {
	t.Helper()
	return relayWith(t, anthropic.Adapter{}, upstream, refuse)
}

// relayWith is relayFor with the gateway's adapter a.
func relayWith(t *testing.T, a adapter.Adapter, upstream string, refuse *Refusal) (*httptest.Server, *Gateway, *event.Memory, *lines) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	mem := &event.Memory{}
	g := New(u, a, mem)
	logs := &lines{}
	g.SetLog(logs.printf)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rl := Relayed{Session: "s1", Turn: "t_1", Exchange: r.Header.Get(ExchangeHeader), Path: strings.TrimPrefix(r.URL.Path, "/relay/anthropic"), Refuse: refuse,
			Header: func(h http.Header) {
				h.Del("X-Api-Key")
				h.Set("Authorization", "Bearer "+secret)
			}}
		g.ServeRelayed(w, r, rl)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(g.Close)
	return srv, g, mem, logs
}

type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, fmt.Sprintf(format, args...))
}

func (l *lines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.all, "\n")
}

func relayedPost(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/relay/anthropic/v1/messages", strings.NewReader(okRequest))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "abr_placeholder")
	req.Header.Set(ExchangeHeader, "x-0123abcd-7")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A relayed call streams to the runner as it comes, with control's
// credential and without the exchange header; every event of it is the
// relay's turn's, under the runner's exchange name, and the turn settles.
func TestRelayedCallStreamsAndIsRecorded(t *testing.T) {
	first := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"usage\":{\"input_tokens\":5}}}\n\n"
	rest := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	gotFirst := make(chan struct{})
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path != "/v1/messages" {
			t.Errorf("vendor path %q", r.URL.Path)
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-gotFirst:
		case <-time.After(5 * time.Second):
			t.Error("the first piece did not reach the runner before the reply ended")
		}
		io.WriteString(w, rest)
	}))
	defer upstream.Close()
	srv, g, mem, _ := relayFor(t, upstream.URL, nil)
	resp := relayedPost(t, srv.URL)
	buf := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != first {
		t.Fatalf("first piece %q %v", buf, err)
	}
	close(gotFirst)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != rest {
		t.Fatalf("the rest %q", b)
	}
	if seen.Get("Authorization") != "Bearer "+secret || seen.Get("X-Api-Key") != "" || seen.Get(ExchangeHeader) != "" {
		t.Fatalf("the vendor got Authorization %t, X-Api-Key %q, exchange %q", seen.Get("Authorization") != "", seen.Get("X-Api-Key"), seen.Get(ExchangeHeader))
	}
	if !g.Settle("s1", "t_1", 2*time.Second) {
		t.Fatal("the turn did not settle")
	}
	evs := mem.Events()
	var responded *event.Event
	for i, e := range evs {
		if e.Session != "s1" || e.Turn != "t_1" || e.Exchange != "x-0123abcd-7" {
			t.Fatalf("event %+v", e)
		}
		if e.Kind == event.ModelResponded {
			responded = &evs[i]
		}
	}
	// The turn's turn.started is the runner's engine's, not this call's.
	if evs[0].Kind != event.ModelRequested {
		t.Fatalf("first event %s", evs[0].Kind)
	}
	if responded == nil || responded.Text != "Hello" || responded.Usage == nil || responded.Usage.OutputTokens != 3 || responded.Error != "" {
		t.Fatalf("model.responded %+v", responded)
	}
}

// A call the relay refuses goes nowhere, and is recorded with the refusal as
// its reply, so the turn shows why.
func TestRelayedRefusalIsRecorded(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer upstream.Close()
	why := `{"error":"no claude subscription is connected on control"}`
	srv, _, mem, _ := relayFor(t, upstream.URL, &Refusal{Status: http.StatusForbidden, ContentType: "application/json", Body: []byte(why)})
	resp := relayedPost(t, srv.URL)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || string(b) != why || called {
		t.Fatalf("refusal %d %q, vendor called %t", resp.StatusCode, b, called)
	}
	evs := waitFor(t, mem, 2)
	last := evs[len(evs)-1]
	if last.Kind != event.ModelResponded || last.Status != http.StatusForbidden || !strings.Contains(last.Error, "no claude subscription") ||
		last.Session != "s1" || last.Turn != "t_1" {
		t.Fatalf("model.responded %+v", last)
	}
}

// A vendor control cannot reach: the cause goes to the service log, with the
// turn and no credential; the runner gets a 502 that says it plainly; the
// call is recorded as failed.
func TestRelayedUpstreamFailureLoggedAndRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	addr := upstream.URL
	upstream.Close()
	srv, _, mem, logs := relayFor(t, addr, nil)
	resp := relayedPost(t, srv.URL)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Content-Type") != "application/json" ||
		!strings.Contains(string(b), `"error":"control could not reach the model's vendor: the vendor could not be reached (dial); cause: connection_refused"`) {
		t.Fatalf("502 %d %q", resp.StatusCode, b)
	}
	evs := waitFor(t, mem, 2)
	last := evs[len(evs)-1]
	if last.Kind != event.ModelResponded || last.Status != http.StatusBadGateway || last.Error == "" || last.Turn != "t_1" {
		t.Fatalf("model.responded %+v", last)
	}
	log := logs.text()
	if !strings.Contains(log, "gateway.upstream_failed vendor=anthropic prefix= via=relay session=s1 turn=t_1 exchange=x-0123abcd-7") ||
		!strings.Contains(log, "reason=connection_refused") || strings.Contains(log, secret) || strings.Contains(log, "abr_placeholder") {
		t.Fatalf("log %q", log)
	}
}

// A relayed reply cut off before its end stays visible as incomplete.
func TestRelayedCutReplyIsIncomplete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":120,"output_tokens":1}}}`+"\n\n"+
			"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"+
			"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me "}}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	srv, g, mem, _ := relayFor(t, upstream.URL, nil)
	resp := relayedPost(t, srv.URL)
	buf := make([]byte, 4096)
	got := ""
	for !strings.Contains(got, "text_delta") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("read before the cut: %v", err)
		}
	}
	resp.Body.Close()
	if !g.Settle("s1", "t_1", 3*time.Second) {
		t.Fatal("the turn did not settle")
	}
	var r *event.Event
	for _, e := range mem.Events() {
		if e.Kind == event.ModelResponded {
			r = &e
		}
		if e.Kind == event.TurnCompleted {
			t.Fatalf("a cut reply claimed the turn's end: %+v", e)
		}
	}
	if r == nil || !strings.Contains(r.Error, "cut off") || r.Usage == nil || !r.Usage.OutputIncomplete || r.Turn != "t_1" {
		t.Fatalf("model.responded %+v", r)
	}
}

// A vendor reply with no Content-Type reaches the runner with none, not a
// type Go's server guessed: the runner's gateway reads the stream by it.
// chatgpt.com streams codex's reply so.
func TestRelayedReplyKeepsNoContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		io.WriteString(w, "data: {}\n\n")
	}))
	defer upstream.Close()
	srv, _, _, _ := relayWith(t, openai.Adapter{}, upstream.URL, nil)
	resp, err := rawClient().Post(srv.URL+"/relay/anthropic/backend-api/codex/responses", "application/json", strings.NewReader(`{"model":"m","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct, ok := resp.Header["Content-Type"]; ok {
		t.Fatalf("Content-Type %q", ct)
	}
}

// A runner's gateway names each call's exchange to its upstream, the relay,
// only when told to (SendExchange); on one machine the vendor never gets the
// header, even one the agent sent.
func TestExchangeHeaderOnlyToRelay(t *testing.T) {
	for _, send := range []bool{false, true} {
		var got []string
		var mu sync.Mutex
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, r.Header.Get(ExchangeHeader))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		u, _ := url.Parse(upstream.URL)
		mem := &event.Memory{}
		g := New(u, anthropic.Adapter{}, mem)
		if send {
			g.SendExchange()
		}
		gw := httptest.NewServer(g)
		req, _ := http.NewRequest("POST", gw.URL+"/s/s1/t/t_1/v1/messages", strings.NewReader(okRequest))
		req.Header.Set(ExchangeHeader, "x-agent-sent-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		g.Settle("s1", "t_1", 2*time.Second)
		gw.Close()
		upstream.Close()
		evs := mem.Events()
		if len(got) != 1 || len(evs) == 0 {
			t.Fatalf("send %t: upstream saw %v, %d events", send, got, len(evs))
		}
		want := ""
		if send {
			want = evs[0].Exchange
		}
		if got[0] != want {
			t.Fatalf("send %t: upstream got exchange %q, want %q", send, got[0], want)
		}
	}
}

// A relayed call holds its turn open from its arrival, while control reads
// its credential: a turn's end waits for it.
func TestRelayedCallHoldsTurnWhilePrepared(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	defer g.Close()
	reading, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.ServeRelayed(w, r, Relayed{Session: "s1", Turn: "t_1", Path: "/v1/messages", Prepare: func(*Relayed) {
			close(reading)
			<-release
		}})
	}))
	defer srv.Close()
	go func() {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(okRequest))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-reading
	if g.Settle("s1", "t_1", 100*time.Millisecond) {
		t.Fatal("the turn settled while its call's credential was read")
	}
	close(release)
	if !g.Settle("s1", "t_1", 2*time.Second) {
		t.Fatal("the turn did not settle after its call")
	}
	responded := false
	for _, e := range mem.Events() {
		responded = responded || e.Kind == event.ModelResponded
	}
	if !responded {
		t.Fatal("the call's reply was not recorded")
	}
}

// cutBody gives some bytes, then fails, as a runner's upload cut off does.
type cutBody struct{ sent bool }

func (b *cutBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.ErrUnexpectedEOF
	}
	b.sent = true
	return copy(p, `{"model":"m","messa`), nil
}

// A relayed call whose request did not arrive whole reached no vendor, and
// is recorded as such: control's record holds every call it took.
func TestRelayedCutRequestIsRecorded(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	defer g.Close()
	r := httptest.NewRequest("POST", "/relay/anthropic/v1/messages", &cutBody{})
	w := httptest.NewRecorder()
	g.ServeRelayed(w, r, Relayed{Session: "s1", Turn: "t_1", Exchange: "x-0123abcd-9", Path: "/v1/messages"})
	if w.Code != http.StatusBadRequest || called {
		t.Fatalf("answer %d, vendor called %t", w.Code, called)
	}
	if !g.Settle("s1", "t_1", 2*time.Second) {
		t.Fatal("the turn did not settle")
	}
	evs := mem.Events()
	if len(evs) != 2 || evs[0].Kind != event.ModelRequested || evs[1].Kind != event.ModelResponded || evs[1].Error != RequestCut ||
		evs[1].Exchange != "x-0123abcd-9" || evs[1].Turn != "t_1" || evs[1].Status != 0 {
		t.Fatalf("events %+v", evs)
	}
}
