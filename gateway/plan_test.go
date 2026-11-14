package gateway

import (
	"encoding/json"
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

// The headers of claude's subscription reply as measured (2026-11-14, one
// Claude Code reply through a local proxy), with the 5h names the community
// clients read, and codex's from rate_limits.rs. A marker in every header
// not on the list.
const planMarker = "NOT_A_PLAN_HEADER_MARKER"

func claudePlanHeaders(used7d string) http.Header {
	return http.Header{
		"Anthropic-Ratelimit-Unified-Status":                  {"allowed"},
		"Anthropic-Ratelimit-Unified-Reset":                   {"1794762000"},
		"Anthropic-Ratelimit-Unified-Representative-Claim":    {"seven_day"},
		"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":          {used7d},
		"Anthropic-Ratelimit-Unified-7d-Reset":                {"1794762000"},
		"Anthropic-Ratelimit-Unified-Fallback-Percentage":     {"0.5"},
		"Anthropic-Ratelimit-Unified-Overage-Status":          {"rejected"},
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {planMarker},
		"X-Other":       {planMarker},
		"Set-Cookie":    {"session=" + planMarker},
		"Request-Id":    {planMarker},
		"Cf-Ray":        {planMarker},
		"Anthropic-Org": {planMarker},
	}
}

func TestPlanReadingsClaude(t *testing.T) {
	h := claudePlanHeaders("0.69")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.38")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1794700800")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed_warning")
	got := planReadings(h, Signin{Subscription: "claude"})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	five, week := got[0], got[1]
	if five.Window != "5h" || five.WindowMinutes != 300 || five.UsedPercent != 38 || five.Status != "allowed_warning" ||
		five.ResetsAt == nil || five.ResetsAt.Unix() != 1794700800 || five.Subscription != "claude" {
		t.Errorf("5h: %+v", five)
	}
	if week.Window != "7d" || week.WindowMinutes != 10080 || week.UsedPercent != 69 || week.Status != "allowed" ||
		week.ResetsAt == nil || week.ResetsAt.Unix() != 1794762000 {
		t.Errorf("7d: %+v", week)
	}
}

// The measured reply carried no 5h headers: only 7d is read.
func TestPlanReadingsClaudeWeekOnly(t *testing.T) {
	got := planReadings(claudePlanHeaders("0.69"), Signin{Subscription: "claude"})
	if len(got) != 1 || got[0].Window != "7d" {
		t.Fatalf("got %+v", got)
	}
}

func TestPlanReadingsCodex(t *testing.T) {
	h := http.Header{
		"X-Codex-Primary-Used-Percent":     {"12.5"},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {"1704069000"},
		"X-Codex-Secondary-Used-Percent":   {"40"},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {"1704670000"},
		"X-Codex-Credits-Balance":          {planMarker},
		"X-Codex-Promo-Message":            {planMarker},
	}
	got := planReadings(h, Signin{Subscription: "codex"})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if p := got[0]; p.Window != "primary" || p.WindowMinutes != 300 || p.UsedPercent != 12.5 || p.ResetsAt.Unix() != 1704069000 || p.Status != "" {
		t.Errorf("primary: %+v", p)
	}
	if p := got[1]; p.Window != "secondary" || p.WindowMinutes != 10080 || p.UsedPercent != 40 || p.ResetsAt.Unix() != 1704670000 {
		t.Errorf("secondary: %+v", p)
	}
}

// A value that is not the shape the vendor sends is left out: a used value
// that is not a finite number drops the window; a bad reset, length or
// status drops that field only.
func TestPlanReadingsMalformed(t *testing.T) {
	for _, used := range []string{"", "abc", "NaN", "Inf", "-1", "0.5 " + planMarker, strings.Repeat("1", 40)} {
		if got := planReadings(http.Header{"X-Codex-Primary-Used-Percent": {used}}, Signin{Subscription: "codex"}); len(got) != 0 {
			t.Errorf("used %q: %+v", used, got)
		}
	}
	h := http.Header{
		"X-Codex-Primary-Used-Percent":               {"5"},
		"X-Codex-Primary-Window-Minutes":             {"-300"},
		"X-Codex-Primary-Reset-At":                   {"2026-11-14T10:00:00Z"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"},
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed " + planMarker},
	}
	got := planReadings(h, Signin{Subscription: "x"})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Status != "" {
		t.Errorf("7d status kept: %q", got[0].Status)
	}
	if got[1].WindowMinutes != 0 || got[1].ResetsAt != nil {
		t.Errorf("primary: %+v", got[1])
	}
}

// planVendor answers every model call with reply and the headers set last.
type planVendor struct {
	*httptest.Server
	mu sync.Mutex
	h  http.Header
}

func newPlanVendor(t *testing.T, contentType, reply string) *planVendor {
	v := &planVendor{h: http.Header{}}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		v.mu.Lock()
		for k, vs := range v.h {
			w.Header()[k] = vs
		}
		v.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, reply)
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *planVendor) set(h http.Header) {
	v.mu.Lock()
	v.h = h
	v.mu.Unlock()
}

// planGateway serves g as control's relay (relayed) or as a gateway of
// agentbus up (up), adding a credential of subscription si (zero for a key).
func planGateway(t *testing.T, how string, si Signin, a adapter.Adapter, upstream string) (do func(path, body string), mem *event.Memory, g *Gateway) {
	t.Helper()
	u, _ := url.Parse(upstream)
	mem = &event.Memory{}
	g = New(u, a, mem)
	set := func(h http.Header) { h.Set("Authorization", ruleCred) }
	var h http.Handler = g
	if how == "relayed" {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.ServeRelayed(w, r, Relayed{Session: "s1", Turn: "t_1", Path: strings.TrimPrefix(r.URL.Path, "/relay/x"),
				Prepare: func(rl *Relayed) { rl.Header, rl.Signin = set, si }})
		})
	} else {
		g.SetCredential(Credential{
			Turn: func(*http.Request, string, string) *Refusal { return nil },
			Read: func(*http.Request) (func(http.Header), Signin, *Refusal) { return set, si, nil },
		})
	}
	prefix := map[string]string{"relayed": "/relay/x", "up": "/s/s1/t/t_1"}[how]
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(g.Close)
	do = func(path, body string) {
		req, _ := http.NewRequest("POST", srv.URL+prefix+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer placeholder")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if !g.Settle("s1", "t_1", 3*time.Second) {
			t.Fatal("the turn did not settle")
		}
	}
	return do, mem, g
}

func plansIn(evs []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.PlanUsage {
			out = append(out, e)
		}
	}
	return out
}

// On both ways a call gets control's subscription, the reply's plan usage
// is stored under that subscription, in no session, naming its call; once
// per change; and no other header's value reaches any event.
func TestPlanUsageStoredPerSubscription(t *testing.T) {
	for _, how := range []string{"relayed", "up"} {
		v := newPlanVendor(t, "application/json", anthReply)
		do, mem, g := planGateway(t, how, Signin{Subscription: "claude", Connection: "c1"}, anthropic.Adapter{}, v.URL)
		clock := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
		g.now = func() time.Time { return clock }

		v.set(claudePlanHeaders("0.69"))
		do("/v1/messages", anthRequest)
		plans := plansIn(mem.Events())
		if len(plans) != 1 {
			t.Fatalf("%s: %d plan.usage, want 1", how, len(plans))
		}
		p := plans[0]
		if p.Session != "" || p.Turn != "" || p.Exchange == "" || p.Vendor != "anthropic" || p.Plan == nil ||
			p.Plan.Subscription != "claude" || p.Plan.Connection != "c1" || p.Plan.Window != "7d" || p.Plan.UsedPercent != 69 {
			t.Errorf("%s: %+v %+v", how, p, p.Plan)
		}
		var exchanged bool
		for _, e := range mem.Events() {
			if e.Kind == event.ModelRequested && e.Exchange == p.Exchange {
				exchanged = true
			}
		}
		if !exchanged {
			t.Errorf("%s: plan.usage names exchange %s, which no model.requested has", how, p.Exchange)
		}

		// The same reading again: not stored.
		do("/v1/messages", anthRequest)
		if n := len(plansIn(mem.Events())); n != 1 {
			t.Errorf("%s: the same reading stored again: %d", how, n)
		}
		// Changed: stored.
		v.set(claudePlanHeaders("0.70"))
		do("/v1/messages", anthRequest)
		if n := len(plansIn(mem.Events())); n != 2 {
			t.Errorf("%s: a changed reading: %d plan.usage, want 2", how, n)
		}
		// The same past planRefresh: stored again, so its time is recent.
		clock = clock.Add(planRefresh)
		do("/v1/messages", anthRequest)
		if n := len(plansIn(mem.Events())); n != 3 {
			t.Errorf("%s: a reading planRefresh old: %d plan.usage, want 3", how, n)
		}

		all, _ := json.Marshal(mem.Events())
		if strings.Contains(string(all), planMarker) || strings.Contains(string(all), "seven_day") ||
			strings.Contains(string(all), "fallback") || strings.Contains(string(all), "control-credential") {
			t.Errorf("%s: a header not on the list reached an event: %s", how, all)
		}
	}
}

// A call with no subscription of control's (an API key, or the agent's own
// sign-in passed on) stores no plan usage, whatever its reply carries.
func TestPlanUsageNeedsASubscription(t *testing.T) {
	for _, how := range []string{"relayed", "up"} {
		v := newPlanVendor(t, "text/event-stream", oaiReply)
		v.set(http.Header{"X-Codex-Primary-Used-Percent": {"12"}})
		do, mem, _ := planGateway(t, how, Signin{}, openai.Adapter{}, v.URL)
		do("/backend-api/codex/responses", oaiRequest)
		if n := len(plansIn(mem.Events())); n != 0 {
			t.Errorf("%s: %d plan.usage on a call with no subscription", how, n)
		}
	}
	// No credential of control's at all: the agent's own sign-in.
	v := newPlanVendor(t, "application/json", anthReply)
	v.set(claudePlanHeaders("0.5"))
	u, _ := url.Parse(v.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	srv := httptest.NewServer(g)
	defer srv.Close()
	resp, err := rawClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(anthRequest))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	g.Close()
	if n := len(plansIn(mem.Events())); n != 0 {
		t.Errorf("own sign-in: %d plan.usage", n)
	}
}

// codex's two windows, on the ChatGPT path, through the gateway.
func TestPlanUsageCodex(t *testing.T) {
	v := newPlanVendor(t, "text/event-stream", oaiReply)
	v.set(http.Header{
		"X-Codex-Primary-Used-Percent":     {"38"},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {"1794700800"},
		"X-Codex-Secondary-Used-Percent":   {"9"},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {"1795000000"},
	})
	do, mem, _ := planGateway(t, "up", Signin{Subscription: "codex", Connection: "c1"}, openai.Adapter{}, v.URL)
	do("/backend-api/codex/responses", oaiRequest)
	plans := plansIn(mem.Events())
	if len(plans) != 2 || plans[0].Plan.Window != "primary" || plans[0].Plan.UsedPercent != 38 ||
		plans[1].Plan.Window != "secondary" || plans[1].Plan.WindowMinutes != 10080 || plans[0].Vendor != "openai" {
		t.Fatalf("%+v", plans)
	}
}

// A new connect's first reading is stored even when it is the same as the
// last one of the connect before: it is that connect's first.
func TestPlanUsageNewConnectStoredAgain(t *testing.T) {
	v := newPlanVendor(t, "application/json", anthReply)
	v.set(claudePlanHeaders("0.69"))
	si := Signin{Subscription: "claude", Connection: "c1"}
	u, _ := url.Parse(v.URL)
	mem := &event.Memory{}
	g := New(u, anthropic.Adapter{}, mem)
	g.SetCredential(Credential{
		Turn: func(*http.Request, string, string) *Refusal { return nil },
		Read: func(*http.Request) (func(http.Header), Signin, *Refusal) {
			return func(h http.Header) { h.Set("Authorization", ruleCred) }, si, nil
		},
	})
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Cleanup(g.Close)
	call := func() {
		req, _ := http.NewRequest("POST", srv.URL+"/s/s1/t/t_1/v1/messages", strings.NewReader(anthRequest))
		req.Header.Set("Content-Type", "application/json")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if !g.Settle("s1", "t_1", 3*time.Second) {
			t.Fatal("the turn did not settle")
		}
	}
	call()
	si.Connection = "c2"
	call()
	plans := plansIn(mem.Events())
	if len(plans) != 2 || plans[0].Plan.Connection != "c1" || plans[1].Plan.Connection != "c2" {
		t.Fatalf("%+v", plans)
	}
}
