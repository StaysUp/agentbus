//go:build integration

package gateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

// A call made while the next hop refuses connections (control restarting)
// is held and goes through, body whole, once the hop listens again
// (SetUpstreamWait). Without the wait the agent gets the error at once.
func TestUpstreamWaitHoldsRefusedCall(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens: each dial is refused

	got := make(chan string, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	u, _ := url.Parse("http://" + addr)
	body := `{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`

	// No wait: refused at once.
	plain := httptest.NewServer(New(u, anthropic.Adapter{}, &event.Memory{}))
	defer plain.Close()
	resp, err := http.Post(plain.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Fatalf("without a wait: %d", resp.StatusCode)
	}

	g := New(u, anthropic.Adapter{}, &event.Memory{})
	g.SetUpstreamWait(30 * time.Second)
	gw := httptest.NewServer(g)
	defer gw.Close()
	go func() {
		time.Sleep(2 * time.Second)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		upstream.Listener = l
		upstream.Start()
	}()
	defer func() { upstream.Close() }()
	start := time.Now()
	resp, err = http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("held call: %d", resp.StatusCode)
	}
	if time.Since(start) < time.Second {
		t.Fatal("the call did not wait for the hop")
	}
	if b := <-got; b != body {
		t.Fatalf("the hop got %q", b)
	}
}
