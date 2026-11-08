package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

const secret = "sk-ant-oat-THIS-MUST-NEVER-APPEAR"

func newGateway(t *testing.T, upstream string) (*httptest.Server, *event.Memory) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	mem := &event.Memory{}
	srv := httptest.NewServer(New(u, anthropic.Adapter{}, mem))
	t.Cleanup(srv.Close)
	return srv, mem
}

// waitFor polls, because reply events are emitted after the body is handed on.
func waitFor(t *testing.T, mem *event.Memory, n int) []event.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if evs := mem.Events(); len(evs) >= n {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("got %d events, want %d: %+v", len(mem.Events()), n, mem.Events())
	return nil
}

func TestPassesThroughAndStreams(t *testing.T) {
	reqBody := `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	first := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"usage\":{\"input_tokens\":5}}}\n\n"
	rest := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	clientGotFirst := make(chan struct{})
	var gotPath, gotQuery, gotAuth, gotBeta, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		// The rest is held back until the client has the first part. A gateway
		// that buffered the reply would never let the client see it.
		select {
		case <-clientGotFirst:
		case <-time.After(2 * time.Second):
		}
		io.WriteString(w, rest)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)

	req, _ := http.NewRequest("POST", gw.URL+"/s/sess-1/v1/messages?beta=true", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("anthropic-beta", "oauth-capability")
	req.Header.Set("User-Agent", "claude-cli/2.1.318 (external, cli)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	var got strings.Builder
	for got.Len() < len(first) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the first part: %v (got %q)", err, got.String())
		}
		got.WriteString(line)
	}
	close(clientGotFirst)
	tail, _ := io.ReadAll(br)
	got.Write(tail)

	if got.String() != first+rest {
		t.Errorf("client got a changed reply:\n%q", got.String())
	}
	if gotPath != "/v1/messages" || gotQuery != "beta=true" {
		t.Errorf("upstream got %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer "+secret || gotBeta != "oauth-capability" {
		t.Errorf("headers were not passed on unchanged: auth=%q beta=%q", gotAuth, gotBeta)
	}
	if gotBody != reqBody {
		t.Errorf("upstream got a changed body: %q", gotBody)
	}

	evs := waitFor(t, mem, 4)
	want := []event.Kind{event.TurnStarted, event.ModelRequested, event.ModelResponded, event.TurnCompleted}
	for i, k := range want {
		if evs[i].Kind != k {
			t.Fatalf("event %d = %s, want %s", i, evs[i].Kind, k)
		}
	}
	for _, e := range evs {
		if e.Session != "sess-1" || e.Client != "claude-cli/2.1.318" || e.Vendor != "anthropic" {
			t.Errorf("event fields = session %q client %q vendor %q", e.Session, e.Client, e.Vendor)
		}
		if e.Exchange != evs[0].Exchange {
			t.Errorf("one exchange expected, got %q and %q", e.Exchange, evs[0].Exchange)
		}
	}
	if evs[2].Text != "Hello" || evs[2].LatencyMS == nil {
		t.Errorf("model.responded = %+v", evs[2])
	}
	if evs[3].Outcome != event.Finished {
		t.Errorf("outcome = %q", evs[3].Outcome)
	}

	all, _ := json.Marshal(evs)
	if bytes.Contains(all, []byte(secret)) || bytes.Contains(all, []byte("oauth-capability")) {
		t.Fatal("a header value reached an event")
	}
}

func TestReadsGzipReply(t *testing.T) {
	plain := `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, plain)
		zw.Close()
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)

	req, _ := http.NewRequest("POST", gw.URL+"/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Error("the client should get the reply still compressed, as the vendor sent it")
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != plain {
		t.Errorf("client body = %q", b)
	}

	evs := waitFor(t, mem, 4)
	if evs[2].Text != "ok" || evs[3].Outcome != event.Finished {
		t.Errorf("events = %+v", evs)
	}
	if evs[0].Session != DefaultSession || DefaultSession != "default" {
		t.Errorf("no prefix means the default session, got %q", evs[0].Session)
	}
}

func TestOtherPathsPassWithoutEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.Path)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)

	resp, err := http.Post(gw.URL+"/s/sess-1/v1/messages/count_tokens", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "/v1/messages/count_tokens" {
		t.Errorf("upstream saw %q", b)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(mem.Events()); n != 0 {
		t.Errorf("got %d events for a path no adapter watches", n)
	}
}

func TestUpstreamDown(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	addr := upstream.URL
	upstream.Close()
	gw, mem := newGateway(t, addr)

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", resp.StatusCode)
	}
	evs := waitFor(t, mem, 3)
	if evs[2].Kind != event.ModelResponded || evs[2].Status != http.StatusBadGateway || evs[2].Error == "" {
		t.Errorf("event = %+v", evs[2])
	}
}

// An agent that closes its connection before the vendor replies, as a
// stopped agent does, gave the call up itself: its model.responded carries
// no status and says so. Stored as a 502 it read as a vendor failure
// (acceptance soak, 2026-11-06).
func TestAgentClosedBeforeReplyHasNoStatus(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		close(arrived)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	gw, mem := newGateway(t, upstream.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-arrived
	cancel()
	<-done
	evs := waitFor(t, mem, 3)
	e := evs[2]
	if e.Kind != event.ModelResponded || e.Status != 0 || e.Error != AgentClosed {
		t.Errorf("event = %+v, want model.responded with no status and error %q", e, AgentClosed)
	}
}

func TestSplitSession(t *testing.T) {
	cases := []struct{ in, session, turn, rest string }{
		{"/v1/messages", "", "", "/v1/messages"},
		{"/s/abc_1-2/v1/messages", "abc_1-2", "", "/v1/messages"},
		{"/s/bad.id/v1/messages", "", "", "/s/bad.id/v1/messages"},
		{"/s//v1/messages", "", "", "/s//v1/messages"},
		{"/s/abc/t/t_0123456789abcdef/v1/messages", "abc", "t_0123456789abcdef", "/v1/messages"},
		{"/s/abc/t/bad.id/v1/messages", "abc", "", "/t/bad.id/v1/messages"},
		{"/t/t_1/v1/messages", "", "", "/t/t_1/v1/messages"},
	}
	for _, c := range cases {
		s, tu, r := SplitSession(c.in)
		if s != c.session || tu != c.turn || r != c.rest {
			t.Errorf("SplitSession(%q) = %q, %q, %q", c.in, s, tu, r)
		}
	}
}

const okRequest = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
const okReply = `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`

func rawClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 3 * time.Second}
}

// No header value reaches an event or an error, including through Go's own
// error text.
func TestNoHeaderValuesInEventsOrErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "RESPONSE_ENCODING_MARKER")
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	req, _ := http.NewRequest("POST", gw.URL+"/v1/messages", strings.NewReader(okRequest))
	req.Header.Set("User-Agent", "USER_AGENT_MARKER extra")
	req.Header.Set("Content-Encoding", "REQUEST_ENCODING_MARKER")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	all, _ := json.Marshal(waitFor(t, mem, 2))
	for _, m := range []string{"USER_AGENT_MARKER", "REQUEST_ENCODING_MARKER", "RESPONSE_ENCODING_MARKER"} {
		if bytes.Contains(all, []byte(m)) {
			t.Errorf("%s reached an event: %s", m, all)
		}
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, b, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		b.WriteString("HTTP/1.1 200 OK\r\nX-Review: RESPONSE_HEADER_MARKER\x00\r\nContent-Length: 2\r\n\r\n{}")
		b.Flush()
	}))
	defer bad.Close()
	gw2, mem2 := newGateway(t, bad.URL)
	resp, err = rawClient().Post(gw2.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	all, _ = json.Marshal(waitFor(t, mem2, 3))
	if bytes.Contains(body, []byte("RESPONSE_HEADER_MARKER")) || bytes.Contains(all, []byte("RESPONSE_HEADER_MARKER")) {
		t.Errorf("a malformed header's value leaked: body=%q events=%s", body, all)
	}
}

// No decompression the agent did not ask for.
func TestNoTransparentDecompression(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	io.WriteString(zw, okReply)
	zw.Close()
	var accept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.Write(compressed.Bytes())
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if accept != "" {
		t.Errorf("the gateway added Accept-Encoding %q", accept)
	}
	if !bytes.Equal(body, compressed.Bytes()) || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("the client got changed bytes: %d bytes, encoding %q", len(body), resp.Header.Get("Content-Encoding"))
	}
	if evs := waitFor(t, mem, 4); evs[2].Text != "ok" {
		t.Errorf("events = %+v", evs)
	}
}

// The path and query reach the vendor as the agent sent them.
func TestPathAndQueryUnchanged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.RequestURI)
	}))
	defer upstream.Close()
	gw, _ := newGateway(t, upstream.URL)
	cases := map[string]string{
		"/s/demo/files/a%2Fb":             "/files/a%2Fb",
		"/s/demo/%2e%2e/admin":            "/%2e%2e/admin",
		"/files/a%2Fb":                    "/files/a%2Fb",
		"/v1/check?keep=1&opaque=a;b&z=2": "/v1/check?keep=1&opaque=a;b&z=2",
		"/s/demo/v1/x?x=%zz&keep=1":       "/v1/x?x=%zz&keep=1",
		"/s/demo%2Fextra/v1/messages":     "/s/demo%2Fextra/v1/messages",
	}
	for in, want := range cases {
		resp, err := http.Get(gw.URL + in)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("%s reached the vendor as %s, want %s", in, b, want)
		}
	}
}

// Forwarding headers the agent sent go on.
func TestForwardedHeadersKept(t *testing.T) {
	got := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	gw, _ := newGateway(t, upstream.URL)
	req, _ := http.NewRequest("POST", gw.URL+"/v1/messages", strings.NewReader(okRequest))
	sent := map[string]string{"Forwarded": "for=192.0.2.1", "X-Forwarded-For": "192.0.2.1",
		"X-Forwarded-Host": "agent.example", "X-Forwarded-Proto": "https"}
	for k, v := range sent {
		req.Header.Set(k, v)
	}
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := <-got
	for k, v := range sent {
		if h.Get(k) != v {
			t.Errorf("%s = %q upstream, want %q", k, h.Get(k), v)
		}
	}
}

// The transport half: a body cut short claims no turn end. The cut
// falls inside the reply, so the adapter cannot see its end. A body cut after
// a complete reply keeps its events (cut rule, 2026-11-05; cut_test.go).
func TestCutBodyClaimsNoTurnEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(okReply)))
		io.WriteString(w, okReply[:len(okReply)-10])
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	evs := waitFor(t, mem, 3)
	time.Sleep(50 * time.Millisecond)
	for _, e := range mem.Events() {
		if e.Kind == event.TurnCompleted {
			t.Fatal("a cut body must not end the turn")
		}
	}
	if evs[2].Kind != event.ModelResponded || evs[2].Error == "" {
		t.Errorf("model.responded = %+v", evs[2])
	}
}

type gate struct{ arrived, release chan struct{} }

func (g *gate) Emit(e event.Event) {
	if e.Kind == event.ModelResponded {
		close(g.arrived)
		<-g.release
	}
}

// A slow event sink never holds back the reply.
func TestSlowSinkDoesNotHoldReply(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	sink := &gate{make(chan struct{}), make(chan struct{})}
	gw := httptest.NewServer(New(u, anthropic.Adapter{}, sink))
	defer gw.Close()
	defer close(sink.release)

	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != okReply {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	select {
	case <-sink.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("model.responded never reached the sink")
	}
}

// The gateway half: a reply the adapter cannot read still reaches the
// agent in full, and the event says so.
func TestUnreadableReplyStillReachesAgent(t *testing.T) {
	payload := "data: {\"type\":\"content_block_start\",\"index\":-1,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, payload)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != payload {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	if evs := waitFor(t, mem, 3); evs[2].Kind != event.ModelResponded || evs[2].Error == "" {
		t.Errorf("event = %+v", evs[2])
	}
}

type panicky struct{ anthropic.Adapter }

func (panicky) ResponseEvents(int, string, []byte) ([]event.Event, error) { panic("broken adapter") }

// An adapter that panics costs one event, not the gateway.
func TestPanickingAdapter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	gw := httptest.NewServer(New(u, panicky{}, mem))
	defer gw.Close()
	for i := 0; i < 2; i++ {
		resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != okReply {
			t.Fatalf("call %d: body = %q", i, body)
		}
	}
	failed := 0
	for _, e := range waitFor(t, mem, 6) {
		if e.Kind == event.ModelResponded && e.Error != "" {
			failed++
		}
	}
	if failed != 2 {
		t.Errorf("%d failed model.responded events, want 2", failed)
	}
}

type blockAll struct{ release chan struct{} }

func (b *blockAll) Emit(event.Event) { <-b.release }

// A blocked output holds up no request, the first or any after it.
func TestBlockedSinkHoldsUpNoRequest(t *testing.T) {
	reached := make(chan struct{}, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	sink := &blockAll{make(chan struct{})}
	gw := httptest.NewServer(New(u, anthropic.Adapter{}, sink))
	defer gw.Close()
	defer close(sink.release)

	for i := 0; i < 3; i++ {
		resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != okReply {
			t.Fatalf("request %d: body = %q", i, body)
		}
		select {
		case <-reached:
		default:
			t.Fatalf("request %d never reached the vendor", i)
		}
	}
}

// A query in the upstream address is kept, with the agent's after it.
func TestUpstreamQueryKept(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.RequestURI)
	}))
	defer upstream.Close()
	for in, want := range map[string]string{
		"/s/demo/v1/x":       "/base/v1/x?api-version=7",
		"/s/demo/v1/x?a=1;b": "/base/v1/x?api-version=7&a=1;b",
	} {
		gw, _ := newGateway(t, upstream.URL+"/base?api-version=7")
		resp, err := http.Get(gw.URL + in)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("%s reached the vendor as %s, want %s", in, b, want)
		}
	}
}

// Events keep the order they happened in, across requests.
func TestEventsInOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	for i := 0; i < 5; i++ {
		resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}
	evs := waitFor(t, mem, 20)
	for i := 1; i < len(evs); i++ {
		if evs[i].Time.Before(evs[i-1].Time) {
			t.Fatalf("event %d is earlier than event %d", i, i-1)
		}
	}
}

// Close waits for replies already read to become events.
func TestCloseKeepsEventsOfFinishedReplies(t *testing.T) {
	big := `{"model":"m","content":[{"type":"text","text":"` + strings.Repeat("a", 1<<20) + `"}],"stop_reason":"end_turn","usage":{}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, big)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	srv := httptest.NewServer(g)

	resp, err := rawClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	srv.Close()
	g.Close()

	var got []event.Kind
	for _, e := range mem.Events() {
		got = append(got, e.Kind)
	}
	want := []event.Kind{event.TurnStarted, event.ModelRequested, event.ModelResponded, event.TurnCompleted}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after Close: %v, want %v", got, want)
	}
}

// A blocked output holds a bounded amount, and the loss is reported.
func TestQueueIsBoundedAndReportsLoss(t *testing.T) {
	release := make(chan struct{})
	mem := &event.Memory{}
	var first sync.Once
	q := newQueue(sinkFunc(func(e event.Event) {
		first.Do(func() { <-release }) // hold the output on the first event
		mem.Emit(e)
	}), 10_000, dropReport)
	for i := 0; i < 1000; i++ {
		q.push(event.Event{Kind: event.ModelRequested, Text: strings.Repeat("x", 1000)})
	}
	if held := heldBytes(q); held > 10_000 {
		t.Errorf("the queue holds %d bytes past its limit of 10000", held)
	}
	close(release)
	q.close()
	// There can be several reports.
	kept, dropped := countKeptDropped(mem.Events())
	if dropped == 0 || kept+dropped != 1000 {
		t.Errorf("kept %d + dropped %d, want 1000 with some dropped", kept, dropped)
	}
}

// Every field counts toward the limit, not only the text.
func TestQueueCountsEveryField(t *testing.T) {
	release := make(chan struct{})
	mem := &event.Memory{}
	var first sync.Once
	q := newQueue(sinkFunc(func(e event.Event) {
		first.Do(func() { <-release }) // hold the output on the first event
		mem.Emit(e)
	}), 10_000, dropReport)
	for i := 0; i < 100; i++ {
		q.push(event.Event{Kind: event.ModelRequested, Model: strings.Repeat("m", 5000), ToolName: strings.Repeat("t", 500)})
	}
	close(release)
	q.close()
	// Each event holds about 6 KB, so only one fits under 10 KB. Counting the
	// text alone, as before the fix, let 19 in.
	kept, dropped := countKeptDropped(mem.Events())
	if kept != 1 || dropped != 99 {
		t.Errorf("kept %d and dropped %d, want 1 and 99: the model name and tool name must count", kept, dropped)
	}
}

// An event taken from the queue but stuck behind a blocked output still counts
// toward the limit.
func TestQueueCountsBatchBeingDelivered(t *testing.T) {
	release := make(chan struct{})
	var first sync.Once
	q := newQueue(sinkFunc(func(event.Event) { first.Do(func() { <-release }) }), 10_000, dropReport)
	defer q.close()
	defer close(release)

	q.push(event.Event{Kind: event.ModelRequested, Text: strings.Repeat("x", 8000)})
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		taken := len(q.pending) == 0
		q.mu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event was never taken")
		}
		time.Sleep(time.Millisecond)
	}
	// About 8.5 KB is stuck in delivery, so none of these 2 KB events fits.
	for i := 0; i < 20; i++ {
		q.push(event.Event{Kind: event.ModelRequested, Text: strings.Repeat("x", 1500)})
	}
	q.mu.Lock()
	queued, lost := len(q.pending), q.lost
	q.mu.Unlock()
	if queued != 0 || lost != 20 {
		t.Errorf("queued %d and lost %d behind a blocked delivery, want 0 and 20", queued, lost)
	}
}

func dropReport(n int, sessions []string) event.Event {
	return event.Event{Kind: event.EventsDropped, Dropped: n, Sessions: sessions, AllSessions: sessions == nil}
}

func heldBytes(q *queue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

func countKeptDropped(evs []event.Event) (kept, dropped int) {
	for _, e := range evs {
		if e.Kind == event.EventsDropped {
			dropped += e.Dropped
		} else {
			kept++
		}
	}
	return kept, dropped
}

type sinkFunc func(event.Event)

func (f sinkFunc) Emit(e event.Event) { f(e) }

// The client name is a copy, not a view into the header.
func TestClientNameIsACopy(t *testing.T) {
	ua := "review/1 " + strings.Repeat("p", 512<<10)
	name := clientName(ua)
	if name != "review/1" {
		t.Fatalf("name = %q", name)
	}
	if unsafe.StringData(name) == unsafe.StringData(ua) {
		t.Error("the name shares the header's memory")
	}
}

// The session id is a copy, not a view into the path.
func TestSessionIDIsACopy(t *testing.T) {
	path := "/s/demo/v1/messages" + strings.Repeat("q", 512<<10)
	id, _, _ := SplitSession(path)
	if id != "demo" {
		t.Fatalf("id = %q", id)
	}
	if p := unsafe.StringData(path); uintptr(unsafe.Pointer(unsafe.StringData(id))) >= uintptr(unsafe.Pointer(p)) &&
		uintptr(unsafe.Pointer(unsafe.StringData(id))) < uintptr(unsafe.Pointer(p))+uintptr(len(path)) {
		t.Error("the session id shares the path's memory")
	}
}

// An agent that disconnects mid-stream leaves a reply without its final output
// count. The event keeps the real input count and marks the output incomplete.
func TestDisconnectMidStreamMarksOutputCountIncomplete(t *testing.T) {
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
	gw, mem := newGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	got := ""
	for !strings.Contains(got, "text_delta") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("read before disconnect: %v", err)
		}
	}
	resp.Body.Close()

	evs := waitFor(t, mem, 3)
	r := evs[2]
	if r.Kind != event.ModelResponded || r.Usage == nil {
		t.Fatalf("model.responded = %+v", r)
	}
	if !r.Usage.OutputIncomplete || r.Usage.InputTokens != 120 || r.Text != "Let me " || r.Error == "" {
		t.Errorf("model.responded = %+v, usage = %+v", r, *r.Usage)
	}
}

// A drop report names every session that lost an event, so a cockpit watching
// one session learns its costs are incomplete (client API spec, 2026-11-05).
func TestDropReportNamesSessions(t *testing.T) {
	var mu sync.Mutex
	var reports []event.Event
	release := make(chan struct{})
	var first sync.Once
	q := newQueue(sinkFunc(func(e event.Event) {
		first.Do(func() { <-release })
		if e.Kind == event.EventsDropped {
			mu.Lock()
			reports = append(reports, e)
			mu.Unlock()
		}
	}), 2_000, dropReport)

	q.push(event.Event{Kind: event.ModelRequested, Session: "keep"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		taken := len(q.pending) == 0
		q.mu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event was never taken")
		}
		time.Sleep(time.Millisecond)
	}
	for _, s := range []string{"b", "a", "b", ""} {
		q.push(event.Event{Kind: event.ModelRequested, Session: s, Text: strings.Repeat("x", 3000)})
	}
	close(release)
	q.close()

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("got %d drop reports, want 1", len(reports))
	}
	if r := reports[0]; r.Dropped != 4 || strings.Join(r.Sessions, ",") != ",a,b" {
		t.Errorf("report = dropped %d, sessions %q; want 4 and [\"\" a b]", r.Dropped, r.Sessions)
	}
}

// The names of sessions that lost events are kept outside the queue's byte
// limit, so they need their own bound. Past it, the report says every
// session.
func TestLostSessionsAreBounded(t *testing.T) {
	var mu sync.Mutex
	var reports []event.Event
	release := make(chan struct{})
	var first sync.Once
	q := newQueue(sinkFunc(func(e event.Event) {
		first.Do(func() { <-release })
		if e.Kind == event.EventsDropped {
			mu.Lock()
			reports = append(reports, e)
			mu.Unlock()
		}
	}), 1_024, dropReport)

	q.push(event.Event{Kind: event.ModelRequested, Session: "keep"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		taken := len(q.pending) == 0
		q.mu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event was never taken")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20_000; i++ {
		q.push(event.Event{Kind: event.ModelRequested, Session: fmt.Sprintf("s-%d-%s", i, strings.Repeat("x", 50))})
	}
	q.mu.Lock()
	kept := len(q.lostIn)
	q.mu.Unlock()
	if kept > maxLostSessions {
		t.Errorf("kept %d lost session names, want at most %d", kept, maxLostSessions)
	}
	close(release)
	q.close()

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("got %d drop reports, want 1", len(reports))
	}
	if r := reports[0]; r.Dropped != 20_000 || !r.AllSessions || len(r.Sessions) != 0 {
		t.Errorf("report = dropped %d, all %v, sessions %d; want 20000, true, 0", r.Dropped, r.AllSessions, len(r.Sessions))
	}
}

// postFor sends one model request to a session and returns the events it
// produced, once there are n of them.
func postFor(t *testing.T, gw *httptest.Server, mem *event.Memory, session, body string, n int) []event.Event {
	t.Helper()
	before := len(mem.Events())
	resp, err := rawClient().Post(gw.URL+"/s/"+session+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return waitFor(t, mem, before+n)[before:]
}

// oneTurn checks that every event carries the same, well-formed turn id, and
// returns it.
func oneTurn(t *testing.T, evs []event.Event) string {
	t.Helper()
	id := evs[0].Turn
	if len(id) != 18 || !strings.HasPrefix(id, "t_") {
		t.Fatalf("turn id = %q, want t_ and 16 hex characters", id)
	}
	for _, e := range evs {
		if e.Turn != id {
			t.Errorf("%s has turn %q, want %q", e.Kind, e.Turn, id)
		}
	}
	return id
}

func okUpstream(t *testing.T) *httptest.Server {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okReply)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// The turn id is read from the conversation. cache_control moves between
// requests, so it is not part of it.
func TestTurnIDSameWhenCacheControlMoves(t *testing.T) {
	gw, mem := newGateway(t, okUpstream(t).URL)
	a := `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		{"role":"user","content":[{"type":"text","text":"second"}]}]}`
	b := `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"first"}]},
		{"role":"assistant","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
		{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}`
	idA := oneTurn(t, postFor(t, gw, mem, "s1", a, 4))
	idB := oneTurn(t, postFor(t, gw, mem, "s1", b, 4))
	if idA != idB {
		t.Errorf("moving cache_control changed the turn id: %q then %q", idA, idB)
	}
	if other := oneTurn(t, postFor(t, gw, mem, "s2", a, 4)); other == idA {
		t.Errorf("two sessions got the same turn id %q", other)
	}
}

// Claude Code 2.1.318 sent the same system message as a list of blocks with
// cache_control in one request and as a plain string in the next (seen
// through the gateway, 2026-11-05). Both spell the same conversation.
func TestTurnIDSameWhenContentIsStringOrBlock(t *testing.T) {
	gw, mem := newGateway(t, okUpstream(t).URL)
	a := `{"model":"m","messages":[
		{"role":"user","content":"first"},
		{"role":"system","content":[{"type":"text","text":"# Environment","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":"second"}]}`
	b := `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"first"}]},
		{"role":"system","content":"# Environment"},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		{"role":"user","content":"second"}]}`
	if idA, idB := oneTurn(t, postFor(t, gw, mem, "s1", a, 4)), oneTurn(t, postFor(t, gw, mem, "s1", b, 4)); idA != idB {
		t.Errorf("string and block content gave different turn ids: %q then %q", idA, idB)
	}
}

// A request carrying a tool result belongs to the turn its conversation
// started, and every event of it says so.
func TestToolResultRequestKeepsTurnID(t *testing.T) {
	gw, mem := newGateway(t, okUpstream(t).URL)
	start := `{"model":"m","messages":[{"role":"user","content":"run ls"}]}`
	result := `{"model":"m","messages":[
		{"role":"user","content":"run ls"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go"}]},
		{"role":"system","content":[{"type":"text","text":"reminder"}]}]}`
	idStart := oneTurn(t, postFor(t, gw, mem, "s1", start, 4))
	evs := postFor(t, gw, mem, "s1", result, 4) // tool.completed, model.requested, model.responded, turn.completed
	if evs[0].Kind != event.ToolCompleted {
		t.Fatalf("first event = %s", evs[0].Kind)
	}
	if id := oneTurn(t, evs); id != idStart {
		t.Errorf("the tool result request got turn %q, want %q", id, idStart)
	}
}

// A reply read after the next request started still carries its own
// request's turn id.
func TestLateReplyKeepsItsRequestTurnID(t *testing.T) {
	gotSlow, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "fast") { // fast's conversation holds "slow" too
			close(gotSlow)
			<-release
		}
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free() // a failed check must not leave the upstream handler waiting
	gw, mem := newGateway(t, upstream.URL)

	slow := `{"model":"m","messages":[{"role":"user","content":"slow"}]}`
	fast := `{"model":"m","messages":[{"role":"user","content":"slow"},{"role":"assistant","content":"ok"},{"role":"user","content":"fast"}]}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := rawClient().Post(gw.URL+"/s/s1/v1/messages", "application/json", strings.NewReader(slow))
		if err == nil {
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}()
	<-gotSlow
	// The slow request's 2 events are already out; this waits for fast's 4.
	postFor(t, gw, mem, "s1", fast, 4)
	free()
	<-done
	evs := waitFor(t, mem, 8)

	byExchange := map[string][]event.Event{}
	var order []string
	for _, e := range evs {
		if _, ok := byExchange[e.Exchange]; !ok {
			order = append(order, e.Exchange)
		}
		byExchange[e.Exchange] = append(byExchange[e.Exchange], e)
	}
	if len(order) != 2 {
		t.Fatalf("got %d exchanges, want 2", len(order))
	}
	slowID := oneTurn(t, byExchange[order[0]])
	fastID := oneTurn(t, byExchange[order[1]])
	if slowID == fastID {
		t.Fatalf("both exchanges got turn %q", slowID)
	}
	if last := evs[len(evs)-1]; last.Exchange != order[0] {
		t.Fatalf("the slow reply was not read last; the test did not test a late reply")
	}
}

// The same words after a reply are a new turn: the conversation now holds the
// reply too.
func TestRepeatedWordsAreANewTurn(t *testing.T) {
	gw, mem := newGateway(t, okUpstream(t).URL)
	first := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	again := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"hi"}]}`
	if a, b := oneTurn(t, postFor(t, gw, mem, "s1", first, 4)), oneTurn(t, postFor(t, gw, mem, "s1", again, 4)); a == b {
		t.Errorf("repeating the words kept turn %q", a)
	}
}

// Two prompts with the same caption and different images are different turns.
func TestDifferentImagesAreDifferentTurns(t *testing.T) {
	gw, mem := newGateway(t, okUpstream(t).URL)
	img := func(data string) string {
		return `{"model":"m","messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}},
			{"type":"text","text":"what is this?"}]}]}`
	}
	if a, b := oneTurn(t, postFor(t, gw, mem, "s1", img("AAAA"), 4)), oneTurn(t, postFor(t, gw, mem, "s1", img("BBBB"), 4)); a == b {
		t.Errorf("different images gave the same turn %q", a)
	}
}

// Key order and whitespace in the JSON do not change the turn id.
func TestTurnIDIgnoresKeyOrderAndSpacing(t *testing.T) {
	gw, mem := newGateway(t, okUpstream(t).URL)
	a := `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","content":"x"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls","timeout":5}}]},{"role":"user","content":"go"}]}`
	b := `{ "messages" : [
		{ "content" : [ { "content" : "x", "tool_use_id" : "t0", "type" : "tool_result" } ], "role" : "user" },
		{ "content" : [ { "input" : { "timeout" : 5, "command" : "ls" }, "name" : "Bash", "id" : "t1", "type" : "tool_use" } ], "role" : "assistant" },
		{ "content" : "go", "role" : "user" } ],
		"model" : "m" }`
	if idA, idB := oneTurn(t, postFor(t, gw, mem, "s1", a, 4)), oneTurn(t, postFor(t, gw, mem, "s1", b, 4)); idA != idB {
		t.Errorf("key order changed the turn id: %q then %q", idA, idB)
	}
}

// Usage's strings and server tool counts come from the vendor, so the queue
// counts them like every other variable-length field.
func TestSizeCountsUsageStrings(t *testing.T) {
	base := size(event.Event{Usage: &event.Usage{}})
	u := &event.Usage{ServiceTier: "standard", Speed: "fast", InferenceGeo: "us",
		ServerToolUse: map[string]int{"web_search_requests": 1}}
	if got, want := size(event.Event{Usage: u}), base+8+4+2+16+len("web_search_requests"); got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
}

// A run names its turn in its base URL, /s/<session>/t/<turn>. Every event of
// every exchange on it carries that turn, tool calls included, even when the
// conversation changes in a way the hash would read as a new turn (codex
// compacting mid-turn). A probe on it still has no turn.
func TestRunBaseURLNamesTheTurn(t *testing.T) {
	toolReply := `{"model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}],"stop_reason":"tool_use","usage":{}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "run ls") && !strings.Contains(string(b), "tool_result") {
			io.WriteString(w, toolReply)
			return
		}
		io.WriteString(w, okReply)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	const turn = "t_0123456789abcdef"
	start := `{"model":"m","messages":[{"role":"user","content":"run ls"}]}`
	result := `{"model":"m","messages":[
		{"role":"user","content":"run ls"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go"}]}]}`
	compacted := `{"model":"m","messages":[{"role":"user","content":"summary: ran ls, saw a.go"}]}`
	var all []event.Event
	all = append(all, postFor(t, gw, mem, "s1/t/"+turn, start, 4)...)     // turn.started, model.requested, model.responded, tool.requested
	all = append(all, postFor(t, gw, mem, "s1/t/"+turn, result, 4)...)    // tool.completed, model.requested, model.responded, turn.completed
	all = append(all, postFor(t, gw, mem, "s1/t/"+turn, compacted, 4)...) // a hash would call this a new turn
	kinds := map[event.Kind]bool{}
	for _, e := range all {
		kinds[e.Kind] = true
		if e.Turn != turn || e.Session != "s1" {
			t.Errorf("%s has session %q turn %q, want s1 and %s", e.Kind, e.Session, e.Turn, turn)
		}
	}
	for _, k := range []event.Kind{event.TurnStarted, event.ModelRequested, event.ModelResponded,
		event.ToolRequested, event.ToolCompleted, event.TurnCompleted} {
		if !kinds[k] {
			t.Errorf("no %s seen", k)
		}
	}
	probe := postFor(t, gw, mem, "s1/t/"+turn,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`, 2)
	for _, e := range probe {
		if e.Turn != "" || !e.Probe {
			t.Errorf("probe %s has turn %q probe %v, want no turn", e.Kind, e.Turn, e.Probe)
		}
	}
}

// With a prefix, the vendor path under it is what the adapter reads and what
// the upstream gets, after the upstream's own path: /s/x/t/t_1/openrouter/v1/messages
// reaches <upstream>/api/v1/messages and is read as a model call.
func TestPrefixCutBeforeReadAndForward(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL + "/api")
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	g.SetPrefix("/openrouter")
	srv := httptest.NewServer(g)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/s/sess-1/t/t_1/openrouter/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if gotPath != "/api/v1/messages" || gotAuth != "Bearer "+secret {
		t.Fatalf("upstream got %s %q", gotPath, gotAuth)
	}
	evs := waitFor(t, mem, 3)
	if evs[1].Kind != event.ModelRequested || evs[1].Session != "sess-1" || evs[1].Turn != "t_1" || evs[2].Kind != event.ModelResponded {
		t.Fatalf("events %+v", evs)
	}
	for _, e := range mem.Events() {
		if b, _ := json.Marshal(e); strings.Contains(string(b), secret) {
			t.Fatalf("an event holds the key: %s", b)
		}
	}
}
