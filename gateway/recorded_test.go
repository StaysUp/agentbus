package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/gemini"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

const ruleCred = "Bearer control-credential-for-tests"

// ruleVendor answers every call with contentType and body, and keeps each
// call it got: method, path, query, and whether it carried the credential.
type ruleVendor struct {
	*httptest.Server
	mu  sync.Mutex
	got []string
}

func newRuleVendor(t *testing.T, contentType, body string) *ruleVendor {
	v := &ruleVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		v.mu.Lock()
		v.got = append(v.got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" cred="+boolWord(r.Header.Get("Authorization") == ruleCred))
		v.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, body)
	}))
	t.Cleanup(v.Close)
	return v
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (v *ruleVendor) calls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.got...)
}

const (
	anthReply   = `{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	anthRequest = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	oaiReply    = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	oaiRequest  = `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	gemReply    = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"pong\"}]},\"finishReason\":\"STOP\"}]}}\n\n"
	gemRequest  = `{"model":"m","project":"p","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
)

type ruleCase struct {
	method, path, body string
	// sent: the vendor got the call, with the credential. Otherwise it got
	// nothing and the answer is 403.
	sent bool
}

// The rule, per vendor and on both ways a call gets a credential of
// control's (a relayed call, and a gateway of agentbus up): every call the
// adapter records gets the credential and is recorded; an unrecorded path,
// a wrong method, or a query the adapter does not read gets 403, reaches no
// vendor, never has its credential read, and leaves call.refused.
func TestOnlyRecordedCallsGetTheCredential(t *testing.T) {
	vendors := []struct {
		a           adapter.Adapter
		contentType string
		reply       string
		cases       []ruleCase
	}{
		{anthropic.Adapter{}, "application/json", anthReply, []ruleCase{
			{"POST", "/v1/messages", anthRequest, true},
			{"POST", "/v1/messages?beta=true", anthRequest, true},
			{"DELETE", "/v1/messages", "", false},
			{"GET", "/v1/messages", "", false},
			{"POST", "/v1/messages/count_tokens", anthRequest, false},
			{"GET", "/v1/models", "", false},
			{"POST", "/v1/files", "x", false},
			{"POST", "/v1/messages?beta=true&other=1", anthRequest, false},
			{"POST", "/v1/messages/batches", anthRequest, false},
			// OpenRouter's other API format, which neither adapter reads.
			{"POST", "/v1/chat/completions", anthRequest, false},
		}},
		{openai.Adapter{}, "text/event-stream", oaiReply, []ruleCase{
			{"POST", "/backend-api/codex/responses", oaiRequest, true},
			{"POST", "/v1/responses", oaiRequest, true},
			{"POST", "/backend-api/other", "{}", false},
			{"GET", "/backend-api/codex/models", "", false},
			{"POST", "/backend-api/codex/responses/compact", oaiRequest, false},
			{"POST", "/v1/responses/compact", oaiRequest, false},
			{"PUT", "/v1/responses", oaiRequest, false},
			{"POST", "/v1/chat/completions", "{}", false},
			{"POST", "/backend-api/codex/responses?alt=proto", oaiRequest, false},
		}},
		{gemini.Adapter{}, "text/event-stream", gemReply, []ruleCase{
			{"POST", "/v1internal:streamGenerateContent?alt=sse", gemRequest, true},
			{"POST", "/v1internal:streamGenerateContent?alt=proto", gemRequest, false},
			{"POST", "/v1internal:streamGenerateContent?alt=sse&key=x", gemRequest, false},
			{"POST", "/v1internal:generateContent?alt=proto", gemRequest, false},
			{"GET", "/v1internal:streamGenerateContent", "", false},
			{"POST", "/v1internal:onboardUser", "{}", false},
			{"POST", "/v1internal:anyOtherMethod", "{}", false},
			{"POST", "/v1internal:loadCodeAssist?alt=proto", "{}", false},
		}},
	}
	for _, v := range vendors {
		for _, how := range []string{"relayed", "up"} {
			vendor := newRuleVendor(t, v.contentType, v.reply)
			do, mem, g, reads := ruleGateway(t, how, v.a, vendor.URL)
			for _, c := range v.cases {
				before := len(mem.Events())
				status, body := do(c.method, c.path, c.body)
				if !g.Settle("s1", "t_1", 3*time.Second) {
					t.Fatalf("%s %s %s %s: the turn did not settle", v.a.Vendor(), how, c.method, c.path)
				}
				evs := mem.Events()[before:]
				name := c.method + " " + strings.SplitN(c.path, "?", 2)[0]
				var refused, requested int
				for _, e := range evs {
					if e.Session != "s1" || e.Turn != "t_1" {
						t.Errorf("%s %s %s: an event outside the call's turn: %+v", v.a.Vendor(), how, name, e)
					}
					switch e.Kind {
					case event.CallRefused:
						refused++
						if e.Call != name || e.Status != http.StatusForbidden || e.Error == "" || strings.Contains(e.Call, "?") {
							t.Errorf("%s %s %s: call.refused %+v", v.a.Vendor(), how, name, e)
						}
					case event.ModelRequested:
						requested++
					}
				}
				if c.sent {
					if status != http.StatusOK || refused != 0 || requested != 1 {
						t.Errorf("%s %s %s: status %d, %d refused, %d requested: %s", v.a.Vendor(), how, c.path, status, refused, requested, body)
					}
					continue
				}
				if status != http.StatusForbidden || refused != 1 || requested != 0 || !strings.Contains(body, "control adds a credential only to the calls") {
					t.Errorf("%s %s %s %s: status %d, %d refused, %d requested: %s", v.a.Vendor(), how, c.method, c.path, status, refused, requested, body)
				}
			}
			var want []string
			for _, c := range v.cases {
				if c.sent {
					p, q, _ := strings.Cut(c.path, "?")
					want = append(want, c.method+" "+p+"?"+q+" cred=yes")
				}
			}
			if got := vendor.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s %s: the vendor got\n%s\nwant\n%s", v.a.Vendor(), how, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if how == "up" && int(reads.Load()) != len(want) {
				t.Errorf("%s up: the credential was read %d times, for %d calls sent", v.a.Vendor(), reads.Load(), len(want))
			}
		}
	}
}

// agy's Code Assist methods besides its model calls get the credential and
// are recorded as vendor.called, by status; their bodies are not read.
func TestServiceCallIsRecordedByStatus(t *testing.T) {
	for _, how := range []string{"relayed", "up"} {
		vendor := newRuleVendor(t, "application/json; charset=UTF-8", `{"cloudaicompanionProject":"p"}`)
		do, mem, g, _ := ruleGateway(t, how, gemini.Adapter{}, vendor.URL)
		status, body := do("POST", "/v1internal:loadCodeAssist", `{"metadata":{}}`)
		g.Settle("s1", "t_1", 3*time.Second)
		if status != http.StatusOK || body != `{"cloudaicompanionProject":"p"}` {
			t.Fatalf("%s: %d %s", how, status, body)
		}
		evs := mem.Events()
		if len(evs) != 1 || evs[0].Kind != event.VendorCalled || evs[0].Call != "POST /v1internal:loadCodeAssist" ||
			evs[0].Status != 200 || evs[0].LatencyMS == nil || evs[0].Turn != "t_1" {
			t.Fatalf("%s: events %+v", how, evs)
		}
		if got := vendor.calls(); len(got) != 1 || got[0] != "POST /v1internal:loadCodeAssist? cred=yes" {
			t.Fatalf("%s: the vendor got %v", how, got)
		}
	}
}

// A reply in a format the adapter does not read is not passed on: the
// agent gets 502, and the call is recorded as refused, with its
// model.responded (or vendor.called) saying why.
func TestUnreadableReplyIsNotPassedOn(t *testing.T) {
	const proto = "\x0a\x05proto-bytes-the-record-cannot-read"
	for _, c := range []struct {
		a           adapter.Adapter
		path, body  string
		contentType string
		kind        event.Kind
	}{
		{gemini.Adapter{}, "/v1internal:streamGenerateContent?alt=sse", gemRequest, "application/x-protobuf", event.ModelResponded},
		{gemini.Adapter{}, "/v1internal:loadCodeAssist", "{}", "application/x-protobuf", event.VendorCalled},
		{gemini.Adapter{}, "/v1internal:loadCodeAssist", "{}", "", event.VendorCalled},
		{anthropic.Adapter{}, "/v1/messages", anthRequest, "text/html", event.ModelResponded},
		{openai.Adapter{}, "/backend-api/codex/responses", oaiRequest, "application/grpc", event.ModelResponded},
	} {
		for _, how := range []string{"relayed", "up"} {
			vendor := newRuleVendor(t, c.contentType, proto)
			do, mem, g, _ := ruleGateway(t, how, c.a, vendor.URL)
			status, body := do("POST", c.path, c.body)
			g.Settle("s1", "t_1", 3*time.Second)
			if status != http.StatusBadGateway || strings.Contains(body, "proto-bytes") || !strings.Contains(body, unreadable) {
				t.Errorf("%s %s %q: %d %q", how, c.path, c.contentType, status, body)
			}
			var refused, responded int
			for _, e := range mem.Events() {
				switch e.Kind {
				case event.CallRefused:
					refused++
				case c.kind:
					if e.Error == unreadable && e.Status == http.StatusBadGateway {
						responded++
					}
				}
			}
			if refused != 1 || responded != 1 {
				t.Errorf("%s %s %q: %d call.refused, %d %s with why: %+v", how, c.path, c.contentType, refused, responded, c.kind, mem.Events())
			}
		}
	}
}

// A gateway that passes the agent's own sign-in on is not held to the rule:
// a call its adapter does not read passes through, unrecorded, as before.
func TestOwnSignInPassesThrough(t *testing.T) {
	vendor := newRuleVendor(t, "application/json", `{}`)
	u, _ := url.Parse(vendor.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	defer g.Close()
	srv := httptest.NewServer(g)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/s/s1/v1/messages/count_tokens", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(vendor.calls()) != 1 || len(mem.Events()) != 0 {
		t.Fatalf("%d, the vendor got %v, events %+v", resp.StatusCode, vendor.calls(), mem.Events())
	}
}

// ruleGateway is a gateway of adapter a to upstream that adds ruleCred to
// each call it lets through: as control's relay does (relayed), or as a
// gateway of agentbus up does (up), for session s1 and turn t_1. do sends a
// call and returns the answer; reads counts the credential reads of up.
func ruleGateway(t *testing.T, how string, a adapter.Adapter, upstream string) (do func(method, path, body string) (int, string), mem *event.Memory, g *Gateway, reads *atomic.Int64) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	mem = &event.Memory{}
	g = New(u, a, mem)
	reads = &atomic.Int64{}
	h := ruleHandler(how, g, reads)
	prefix := map[string]string{"relayed": "/relay/x", "up": "/s/s1/t/t_1"}[how]
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(g.Close)
	do = func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+prefix+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer placeholder")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	return do, mem, g, reads
}

// A refused call whose upload is still coming holds its turn open, as every
// call does: the turn does not settle (and control stores no session.closed)
// until its call.refused is recorded. Before, the body was closed first,
// which waits for the upload, with nothing held: the turn settled with no
// refusal in the record, and the refusal landed after.
func TestRefusedUploadHoldsItsTurn(t *testing.T) {
	for _, how := range []string{"relayed", "up"} {
		vendor := newRuleVendor(t, "application/json", anthReply)
		_, mem, g, reads := ruleGateway(t, how, anthropic.Adapter{}, vendor.URL)
		srv := httptest.NewServer(ruleHandler(how, g, reads))
		prefix := map[string]string{"relayed": "/relay/x", "up": "/s/s1/t/t_1"}[how]
		body, upload := io.Pipe()
		req, _ := http.NewRequest("POST", srv.URL+prefix+"/v1/files", body)
		req.Header.Set("Authorization", "Bearer placeholder")
		done := make(chan int, 1)
		go func() {
			resp, err := rawClient().Do(req)
			if err != nil {
				done <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			done <- resp.StatusCode
		}()
		if _, err := upload.Write([]byte(strings.Repeat("x", 1024))); err != nil {
			t.Fatal(err)
		}
		held := false
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if !g.Settle("s1", "t_1", 50*time.Millisecond) {
				held = true
				break
			}
		}
		if !held {
			t.Fatalf("%s: the turn settled while the refused call's upload was still coming", how)
		}
		upload.Close()
		if code := <-done; code != http.StatusForbidden {
			t.Fatalf("%s: status %d", how, code)
		}
		if !g.Settle("s1", "t_1", 3*time.Second) {
			t.Fatalf("%s: the turn did not settle once the upload ended", how)
		}
		var refused int
		for _, e := range mem.Events() {
			if e.Kind == event.CallRefused {
				refused++
			}
		}
		if refused != 1 || len(vendor.calls()) != 0 || reads.Load() != 0 {
			t.Fatalf("%s: %d call.refused, the vendor got %v, %d credential reads", how, refused, vendor.calls(), reads.Load())
		}
		srv.Close()
	}
}

// ruleHandler serves g as control's relay (relayed) or as a gateway of
// agentbus up (up) does, adding ruleCred; reads counts credential reads.
// For up it sets g's credential, once.
func ruleHandler(how string, g *Gateway, reads *atomic.Int64) http.Handler {
	set := func(h http.Header) { h.Set("Authorization", ruleCred) }
	if how == "relayed" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.ServeRelayed(w, r, Relayed{Session: "s1", Turn: "t_1", Path: strings.TrimPrefix(r.URL.Path, "/relay/x"),
				Prepare: func(rl *Relayed) { reads.Add(1); rl.Header = set }})
		})
	}
	g.SetCredential(Credential{
		Turn: func(*http.Request, string, string) *Refusal { return nil },
		Read: func(*http.Request) (func(http.Header), *Refusal) { reads.Add(1); return set, nil },
	})
	return g
}
