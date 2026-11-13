package jobs

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// calls is a record that knows some calls: their session and turn.
type calls map[string][2]string

func (c calls) AppendOnce(event.Event) (bool, error)            { return true, nil }
func (c calls) KeepSent(store.CallContext) ([]string, error)    { return nil, nil }
func (c calls) KeepReply(body.Reply) error                      { return nil }
func (c calls) Continuation(string) (store.Continuation, error) { return store.Continuation{}, nil }
func (c calls) CallSession(x string) (string, bool, error)      { v, ok := c[x]; return v[0], ok, nil }
func (c calls) EventByID(string, string, string) (event.Event, bool, error) {
	return event.Event{}, false, nil
}
func (c calls) OtherTurns(x, session, turn string) (bool, error) {
	v, ok := c[x]
	return ok && v[0] == session && v[1] != turn, nil
}

// The relay records a call under the runner's exchange name only when that
// name cannot reach another call: the shape of a gateway's id, none of
// control's own gateways' names, no call of another session or of another
// turn of this one.
func TestExchangeOfRunner(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	mem := &event.Memory{}
	g := gateway.New(u, anthropic.Adapter{}, mem)
	defer g.Close()
	rec := calls{"x-0000000a-1": {"other", "t_9"}, "x-0000000a-2": {"s1", "t_0"}, "x-0000000a-3": {"s1", "t_1"}}
	s := &Server{cfg: Config{Relay: map[string]Upstream{"anthropic": {URL: u, Gateways: []*gateway.Gateway{g}}}, Record: rec}}
	// A name control's own gateway gave: its call's events say it.
	req := httptest.NewRequest("POST", "/relay/anthropic/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	g.ServeRelayed(httptest.NewRecorder(), req, gateway.Relayed{Session: "s0", Turn: "t_0", Path: "/v1/messages",
		Refuse: &gateway.Refusal{Status: 403, Body: []byte(`{}`)}})
	g.Settle("s0", "t_0", time.Second)
	own := mem.Events()[0].Exchange
	if !exchangeID.MatchString(own) {
		t.Fatalf("control's own name %q", own)
	}
	for x, want := range map[string]string{
		"x-0123abcd-7":  "x-0123abcd-7", // new
		"x-0000000a-3":  "x-0000000a-3", // this turn's own call
		"x-0000000a-1":  "",             // another session's
		"x-0000000a-2":  "",             // another turn's
		"anything else": "",
		own:             "",
	} {
		r, _ := http.NewRequest("POST", "/", nil)
		r.Header.Set(gateway.ExchangeHeader, x)
		if got := s.exchangeOf(r, "s1", "t_1"); got != want {
			t.Errorf("%q: %q, want %q", x, got, want)
		}
	}
}

// A vendor's API key goes to its own harness's calls only, and in the header
// that vendor reads: the agent's placeholder, in whichever header, goes.
func TestAPIKeyToItsOwnVendor(t *testing.T) {
	src := credentials.Plug{credentials.AccessAPI: fixedKey{"claude": {Token: "fake-anth", Header: "X-Api-Key"}, "codex": {Token: "fake-oai"}}}
	s := &Server{cfg: Config{Credentials: src}}
	claudeJob := queue.Job{Spec: queue.Spec{Harness: "claude", Access: "api"}}
	anth := Upstream{Access: credentials.AccessAPI, Harness: "claude"}
	oai := Upstream{Access: credentials.AccessAPI, Harness: "codex"}
	set, refused := s.turnCredential(claudeJob, anth)
	if refused != nil {
		t.Fatalf("refused: %s", refused.Body)
	}
	h := http.Header{"Authorization": {"Bearer abr_placeholder"}, "X-Goog-Api-Key": {"abr_placeholder"}}
	set(h)
	if h.Get("X-Api-Key") != "fake-anth" || h.Get("Authorization") != "" || h.Get("X-Goog-Api-Key") != "" {
		t.Fatalf("headers: %v", h)
	}
	if _, refused := s.turnCredential(claudeJob, oai); refused == nil || refused.Status != http.StatusForbidden {
		t.Fatal("claude's key went to OpenAI's path")
	}
	// An API-key agent's call on a subscription's path gets nothing either.
	if _, refused := s.turnCredential(claudeJob, Upstream{Access: credentials.AccessSubscription, Harness: "claude"}); refused == nil {
		t.Fatal("an API-key agent got the subscription")
	}
	set, _ = s.turnCredential(queue.Job{Spec: queue.Spec{Harness: "codex", Access: "api"}}, oai)
	h = http.Header{"Authorization": {"Bearer abr_placeholder"}}
	set(h)
	if h.Get("Authorization") != "Bearer fake-oai" {
		t.Fatalf("codex headers: %v", h)
	}
}

// fixedKey gives each harness its key.
type fixedKey map[string]credentials.Cred

func (f fixedKey) Credential(n credentials.Need) (credentials.Cred, error) {
	c, ok := f[n.Harness]
	if !ok {
		return credentials.Cred{}, &credentials.Missing{Why: "none"}
	}
	return c, nil
}
