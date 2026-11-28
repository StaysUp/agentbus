//go:build integration

package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

// askGuard asks for git push on every turn, and lets anything else run.
type askGuard struct{}

func (askGuard) Forbid(string, string) []string    { return nil }
func (askGuard) Ask(string, string) []string       { return []string{"git push"} }
func (askGuard) Allow(string, string) []string     { return nil }
func (askGuard) AllowOnce(string, string) []string { return nil }
func (askGuard) CopyWrite(string, string) []string { return nil }
func (askGuard) Unlisted(string, string) Decision  { return Allow }

// stalledAgent is the agent's end of its connection once it stops reading
// mid-hold: the reply up to the hold passes, and from the first ping on a
// write waits as one into a full socket does, until the write deadline set
// on it passes, or for good when none is set.
type stalledAgent struct {
	http.ResponseWriter
	mu       sync.Mutex
	deadline time.Time
	stalled  bool
	gone     chan struct{} // closed by the test's cleanup
}

func (a *stalledAgent) SetWriteDeadline(t time.Time) error {
	a.mu.Lock()
	a.deadline = t
	a.mu.Unlock()
	return nil
}

func (a *stalledAgent) Write(p []byte) (int, error) {
	a.mu.Lock()
	a.stalled = a.stalled || bytes.Contains(p, []byte(`"type": "ping"`))
	stalled := a.stalled
	a.mu.Unlock()
	if !stalled {
		return a.ResponseWriter.Write(p)
	}
	for {
		a.mu.Lock()
		d := a.deadline
		a.mu.Unlock()
		if !d.IsZero() && !time.Now().Before(d) {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-a.gone:
			return 0, io.ErrClosedPipe
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (a *stalledAgent) Flush() { a.ResponseWriter.(http.Flusher).Flush() }

// holdServer is a gateway that asks for git push, in front of a vendor that
// streams a reply calling it, served to the agent through wrap.
func holdServer(t *testing.T, d Decider, timeout time.Duration, wrap func(http.ResponseWriter) http.ResponseWriter) (*httptest.Server, *event.Memory) {
	t.Helper()
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicAskStream)
	}))
	t.Cleanup(vendor.Close)
	u, _ := url.Parse(vendor.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	g.SetGuard(askGuard{})
	g.SetDecider(d)
	g.SetHoldTiming(timeout, 20*time.Millisecond, 10*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.ServeHTTP(wrap(w), r) }))
	t.Cleanup(func() { srv.Close(); g.Close() })
	return srv, mem
}

// An agent that stops reading while its call is held cannot keep the hold
// past its time: the ping it does not read fails at the hold's deadline, and
// the hold ends as its timeout does, the call refused and the approval
// decided with "no one answered in time". Before, the ping's write blocked
// the hold's next step, so the approval stayed pending for as long as the
// agent kept the connection open (#129).
func TestHoldEndsOnTimeWhenTheAgentStopsReading(t *testing.T) {
	const timeout = 400 * time.Millisecond
	gone := make(chan struct{})
	srv, mem := holdServer(t, &fakeDecider{}, timeout, func(w http.ResponseWriter) http.ResponseWriter {
		return &stalledAgent{ResponseWriter: w, gone: gone}
	})
	// After holdServer's, so it runs first: the stalled write lets go, and
	// the server can close.
	t.Cleanup(func() { close(gone) })
	go func() {
		resp, err := http.Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(runBody))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	var held time.Time
	for limit := time.Now().Add(5 * time.Second); time.Now().Before(limit); time.Sleep(5 * time.Millisecond) {
		req, dec, text := decisions(mem)
		if req == 1 && held.IsZero() {
			held = time.Now()
		}
		if dec == 1 {
			if text != "no one answered in time" {
				t.Fatalf("the hold ended with %q", text)
			}
			if took := time.Since(held); took > timeout+500*time.Millisecond {
				t.Fatalf("the hold ended %v after it began, past its %v", took, timeout)
			}
			refused := 0
			for _, e := range mem.Events() {
				if e.Kind == event.ToolRefused && e.ToolID == "toolu_1" {
					refused++
				}
			}
			if refused != 1 {
				t.Fatalf("%d tool.refused for the held call, want 1", refused)
			}
			return
		}
	}
	req, _, _ := decisions(mem)
	t.Fatalf("the hold was still pending 5 s after the call (%d approval.requested)", req)
}

// An agent that keeps reading gets the hold's pings and then the reply: the
// hold's write deadline is cleared once it ends, so a person's allow passes
// the call unchanged.
func TestHoldAllowReachesAReadingAgent(t *testing.T) {
	d := &fakeDecider{}
	srv, mem := holdServer(t, d, 2*time.Second, func(w http.ResponseWriter) http.ResponseWriter { return w })
	time.AfterFunc(300*time.Millisecond, func() { d.set(true) })
	resp, err := http.Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, `"type": "ping"`) || strings.Contains(out, RefusedPrefix) || !strings.Contains(out, "toolu_1") {
		t.Fatalf("the agent's reply:\n%s", out)
	}
	if _, dec, _ := decisions(mem); dec != 0 {
		t.Fatalf("the gateway recorded %d decisions for a person's allow", dec)
	}
}
