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
			g.SetCredential(Credential{
				Turn: func(*http.Request, string, string) *Refusal { return nil },
				Read: func(*http.Request) (func(http.Header), *Refusal) {
					read()
					return func(h http.Header) { h.Set("Authorization", "Bearer fake-for-tests") }, nil
				}})
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

// A call whose token is not its path's turn (Credential.Turn refuses) is
// answered before anything is held, recorded or rewritten: no event, no
// credential read, no vendor (review round 3: a refused call still wrote a
// turn.started and its call into the path's session).
func TestRefusedTurnIsRecordedNowhere(t *testing.T) {
	hits := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	t.Cleanup(vendor.Close)
	u, _ := url.Parse(vendor.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	read := 0
	g.SetCredential(Credential{
		Turn: func(*http.Request, string, string) *Refusal {
			return &Refusal{Status: http.StatusForbidden, ContentType: "application/json", Body: []byte(`{"error":"another turn's"}`)}
		},
		Read: func(*http.Request) (func(http.Header), *Refusal) { read++; return nil, nil }})
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/s/other/t/t_9/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "another turn's") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if !g.Settle("other", "t_9", time.Second) {
		t.Fatal("the refused call held a turn")
	}
	if evs := mem.Events(); len(evs) != 0 || read != 0 || hits != 0 || g.reads.Load() != 0 {
		t.Fatalf("recorded %d events, %d credential reads, %d vendor calls", len(evs), read, hits)
	}
}
