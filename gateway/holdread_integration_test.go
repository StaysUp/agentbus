//go:build integration

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

// One rule, on every path: a call holds its turn open before it reads any
// credential (holdThenRead). A credential read that blocks, as a slow
// renewal does, keeps the turn open, so the turn's end is not stored before
// the call's events; once the read ends and the call is done, the turn
// settles. Both paths, a gateway with SetCredential (agentbus up) and a
// relayed call (control's relay for a runner), read through holdThenRead.
func TestCallHoldsItsTurnOpenWhileItReadsItsCredential(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(vendor.Close)
	u, err := url.Parse(vendor.URL)
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for name, serve := range map[string]func(g *Gateway, read func()) http.Handler{
		// agentbus up: the gateway's own credential.
		"credential": func(g *Gateway, read func()) http.Handler {
			g.SetCredential(func(*http.Request, string, string) (func(http.Header), *Refusal) {
				read()
				return func(h http.Header) { h.Set("Authorization", "Bearer fake-for-tests") }, nil
			})
			return g
		},
		// control's relay: Prepare reads the credential.
		"relayed": func(g *Gateway, read func()) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				g.ServeRelayed(w, r, Relayed{Session: "s1", Turn: "t_1", Path: "/v1/messages",
					Prepare: func(rl *Relayed) {
						read()
						rl.Header = func(h http.Header) { h.Set("Authorization", "Bearer fake-for-tests") }
					}})
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := New(u, anthropic.Adapter{}, &event.Memory{})
			entered, release := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(serve(g, func() { close(entered); <-release }))
			t.Cleanup(srv.Close)
			done := make(chan int, 1)
			go func() {
				resp, err := http.Post(srv.URL+"/s/s1/t/t_1/v1/messages", "application/json", strings.NewReader(body))
				if err != nil {
					done <- 0
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done <- resp.StatusCode
			}()
			<-entered
			if g.Settle("s1", "t_1", 200*time.Millisecond) {
				t.Fatal("the turn settled while its call was reading its credential")
			}
			close(release)
			if code := <-done; code != http.StatusOK {
				t.Fatalf("the call: %d", code)
			}
			if !g.Settle("s1", "t_1", 5*time.Second) {
				t.Fatal("the turn did not settle once the call was done")
			}
			if n := g.reads.Load(); n != 1 {
				t.Fatalf("%d credential reads through holdThenRead, want 1", n)
			}
		})
	}
}
