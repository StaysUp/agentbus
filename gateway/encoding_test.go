package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// Anthropic answered a 429 in br when the agent's Accept-Encoding went on, and
// the gateway cannot read br, so the error text was lost (Claude Code 2.1.318,
// 2026-11-05). The gateway asks for no encoding, for every upstream.
func TestAcceptEncodingNotForwarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
		gw   func(*testing.T, string) (*httptest.Server, *event.Memory)
	}{
		{"anthropic", "/v1/messages", okRequest, newGateway},
		{"chatgpt", "/backend-api/codex/responses", codexRequest, newOpenAIGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			seen := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				got, seen = r.Header.Values("Accept-Encoding"), true
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			gw, _ := tc.gw(t, upstream.URL)
			req, _ := http.NewRequest("POST", gw.URL+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Accept-Encoding", "br, gzip")
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if !seen {
				t.Fatal("the upstream got no request")
			}
			if len(got) != 0 {
				t.Errorf("the vendor got Accept-Encoding %q, want none", got)
			}
		})
	}
}

// The vendor answers in br only when asked for it, as Anthropic did. Without
// Accept-Encoding the 429's message reaches model.responded, and the agent
// still gets the vendor's reply.
func TestErrorMessageReachesEvent(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Too many requests, slow down"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "br") {
			w.Header().Set("Content-Encoding", "br")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "\x83\x38\x00not-readable-br")
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, errBody)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	req, _ := http.NewRequest("POST", gw.URL+"/v1/messages", strings.NewReader(okRequest))
	req.Header.Set("Accept-Encoding", "br, gzip")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || string(body) != errBody {
		t.Errorf("the agent got %d %q, want the vendor's 429 as sent", resp.StatusCode, body)
	}
	evs := waitFor(t, mem, 3)
	last := evs[len(evs)-1]
	if last.Kind != event.ModelResponded || last.Status != 429 || last.Error != "Too many requests, slow down" {
		t.Errorf("model.responded = %+v, want status 429 and the vendor's message", last)
	}
}

// Claude Code 2.1.318's startup quota check (2026-11-05) starts no turn, and
// both its events are marked as the harness's own check. The 429 it got still
// reaches model.responded.
func TestQuotaCheckIsAProbeNotATurn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	evs := postFor(t, gw, mem, "s1", `{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}],"metadata":{"user_id":"u"}}`, 2)
	if len(evs) != 2 || evs[0].Kind != event.ModelRequested || evs[1].Kind != event.ModelResponded {
		t.Fatalf("events = %+v, want model.requested and model.responded only", evs)
	}
	for _, e := range evs {
		if !e.Probe || e.Turn != "" {
			t.Errorf("%s: probe %v turn %q, want probe and no turn", e.Kind, e.Probe, e.Turn)
		}
	}
	if evs[1].Status != 429 || evs[1].Error != "Error" {
		t.Errorf("model.responded = status %d error %q", evs[1].Status, evs[1].Error)
	}
	// A normal request on the same gateway is not marked.
	normal := postFor(t, gw, mem, "s1", okRequest, 3)
	for _, e := range normal {
		if e.Probe {
			t.Errorf("%s of a normal request marked probe", e.Kind)
		}
	}
}

// A quota check answered 200 with end_turn must not claim a turn end for a
// turn that never started. A probe exchange reports model.requested and
// model.responded only.
func TestQuotaCheckAnsweredClaimsNoTurnEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"I"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	evs := postFor(t, gw, mem, "s1", `{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}],"metadata":{"user_id":"u"}}`, 2)
	time.Sleep(100 * time.Millisecond) // let any later event arrive
	evs = mem.Events()
	if len(evs) != 2 || evs[0].Kind != event.ModelRequested || evs[1].Kind != event.ModelResponded {
		t.Fatalf("kinds = %v, want model.requested and model.responded only", kindsOf(evs))
	}
	for _, e := range evs {
		if !e.Probe || e.Turn != "" {
			t.Errorf("%s: probe %v turn %q, want probe and no turn", e.Kind, e.Probe, e.Turn)
		}
	}
}

func kindsOf(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
