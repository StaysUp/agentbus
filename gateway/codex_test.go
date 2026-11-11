package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

const (
	codexRequest = `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"say hi"}]}]}`
	codexReply   = `{"model":"gpt-x","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":1}}`
)

func newOpenAIGateway(t *testing.T, upstream string) (*httptest.Server, *event.Memory) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	mem := &event.Memory{}
	srv := httptest.NewServer(New(u, openai.Adapter{}, mem))
	t.Cleanup(srv.Close)
	return srv, mem
}

func zstdBytes(t *testing.T, plain []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(plain, nil)
}

// codex-cli 0.164.0 sends zstd request bodies (2026-11-05). The adapter reads
// a decoded copy; the vendor gets the compressed bytes as the agent sent them.
func TestZstdRequestReadForAdapterOnly(t *testing.T) {
	compressed := zstdBytes(t, []byte(codexRequest))
	var mu sync.Mutex
	var gotBody []byte
	var gotEnc string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotEnc = b, r.Header.Get("Content-Encoding")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, codexReply)
	}))
	defer upstream.Close()
	gw, mem := newOpenAIGateway(t, upstream.URL)

	req, _ := http.NewRequest("POST", gw.URL+"/s/abc/backend-api/codex/responses", bytes.NewReader(compressed))
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Content-Type", "application/json")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	mu.Lock()
	if !bytes.Equal(gotBody, compressed) || gotEnc != "zstd" {
		t.Errorf("upstream got %d bytes with encoding %q; want the original %d compressed bytes", len(gotBody), gotEnc, len(compressed))
	}
	mu.Unlock()

	evs := waitFor(t, mem, 4)
	if evs[0].Kind != event.TurnStarted || evs[0].Text != "say hi" || evs[0].Turn == "" {
		t.Errorf("first event = %+v, want turn.started with the prompt and a turn id", evs[0])
	}
	if evs[1].Kind != event.ModelRequested || evs[1].Model != "gpt-x" || evs[1].Error != "" {
		t.Errorf("second event = %+v, want model.requested for gpt-x", evs[1])
	}
	if evs[2].Kind != event.ModelResponded || evs[2].Turn != evs[0].Turn || evs[0].Session != "abc" {
		t.Errorf("reply event = %+v", evs[2])
	}
}

// A small compressed body can decode to any size. The adapter's copy is
// bounded; the request still reaches the vendor untouched.
func TestZstdRequestDecodeIsBounded(t *testing.T) {
	compressed := zstdBytes(t, make([]byte, maxDecoded+1))
	var gotLen atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotLen.Store(int64(len(b)))
		io.WriteString(w, codexReply)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	g := New(u, openai.Adapter{}, mem)
	gw := httptest.NewServer(g)

	// No clock: decoding 64 MiB under -race on a loaded machine takes as
	// long as it takes, and the test's own deadline bounds it. The client
	// waits for the reply with no timeout, then closing the server waits
	// for its handlers and closing the gateway delivers every event.
	req, _ := http.NewRequest("POST", gw.URL+"/backend-api/codex/responses", bytes.NewReader(compressed))
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	gw.Close()
	g.Close()
	if gotLen.Load() != int64(len(compressed)) {
		t.Errorf("upstream got %d bytes, want %d", gotLen.Load(), len(compressed))
	}
	evs := mem.Events()
	if len(evs) == 0 {
		t.Fatal("no event")
	}
	if evs[0].Kind != event.ModelRequested || !strings.Contains(evs[0].Error, "more than") {
		t.Errorf("first event = %+v, want model.requested with a size error", evs[0])
	}
}

// codex-cli 0.164.0 first tries a WebSocket on its model-call path and posts
// when refused; a 426 made it fall back (2026-11-05). The upgrade is answered
// here and never reaches the vendor. Upgrades on other paths pass through.
func TestUpgradeOnModelPathAnsweredLocally(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	gw, _ := newOpenAIGateway(t, upstream.URL)

	get := func(path string) int {
		req, _ := http.NewRequest("GET", gw.URL+path, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, p := range []string{"/backend-api/codex/responses", "/s/abc/backend-api/codex/responses"} {
		if code := get(p); code != http.StatusUpgradeRequired {
			t.Errorf("%s: status %d, want 426", p, code)
		}
	}
	mu.Lock()
	if len(paths) != 0 {
		t.Errorf("upstream saw %v; want nothing", paths)
	}
	mu.Unlock()

	if code := get("/backend-api/codex/other"); code != http.StatusTeapot {
		t.Errorf("an upgrade on another path: status %d, want the upstream's 418", code)
	}
}

// A codex request the rewriter changes, zstd or plain, goes on plain with no
// Content-Encoding, and with the rewriter's header edit applied. Headers the
// edit leaves alone go on as sent.
func TestCodexRewriteSentPlainWithHeaderEdit(t *testing.T) {
	const changed = `{"model":"gpt-x","prompt_cache_key":"old","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"say hi"}]}]}`
	for _, zstdIn := range []bool{true, false} {
		var mu sync.Mutex
		var gotBody []byte
		var gotEnc, gotThread, gotAuth string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody, gotEnc, gotThread, gotAuth = b, r.Header.Get("Content-Encoding"), r.Header.Get("Thread-Id"), r.Header.Get("Authorization")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, codexReply)
		}))
		u, _ := url.Parse(upstream.URL)
		mem := &event.Memory{}
		g := New(u, openai.Adapter{}, mem)
		g.SetRewriter(rewriteFunc(func(_, _ string, b []byte) Rewrite {
			if string(b) != codexRequest {
				t.Errorf("the rewriter got %q, not the decoded request", b)
			}
			return Rewrite{Body: []byte(changed), Replayed: 1, Header: func(h http.Header) {
				h.Set("Thread-Id", strings.ReplaceAll(h.Get("Thread-Id"), "fresh", "old"))
			}}
		}))
		srv := httptest.NewServer(g)

		in := []byte(codexRequest)
		if zstdIn {
			in = zstdBytes(t, in)
		}
		req, _ := http.NewRequest("POST", srv.URL+"/s/abc/t/t_1/backend-api/codex/responses", bytes.NewReader(in))
		if zstdIn {
			req.Header.Set("Content-Encoding", "zstd")
		}
		req.Header.Set("Thread-Id", "fresh")
		req.Header.Set("Authorization", "Bearer x")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		mu.Lock()
		if string(gotBody) != changed || gotEnc != "" || gotThread != "old" || gotAuth != "Bearer x" {
			t.Errorf("zstd=%v: upstream got %q, encoding %q, thread %q", zstdIn, gotBody, gotEnc, gotThread)
		}
		mu.Unlock()
		srv.Close()
		g.Close()
		upstream.Close()
	}
}
