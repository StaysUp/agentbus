package api

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// GET /api/v0/subscriptions shows each connected subscription's latest
// plan usage per window, shortest window first, of its current connect
// only, and none for another harness. A reply on the connect before,
// stored after the new connect because its call was on the way, is left
// out: it may be another account's. Before, readings were kept by time
// against connected_at, and that one showed.
func TestSubscriptionsShowPlanUsage(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	subs := &credentials.Subscriptions{Dir: filepath.Join(dir, "subscriptions"), Now: func() time.Time { return clock }}
	connect := func() string {
		t.Helper()
		if err := subs.Put("claude", "fake-claude-token", 365*24*time.Hour); err != nil {
			t.Fatal(err)
		}
		c, err := subs.Credential(credentials.Need{Access: credentials.AccessSubscription, Harness: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		return c.Connection
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Subscriptions: &credentials.Manager{Store: subs, Logins: map[string]credentials.Login{"claude": {}, "codex": {}}}})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(srv.Close)
	e := &env{srv: srv, api: a, store: st, port: port}

	reset := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	n := 0
	plan := func(at time.Time, sub, connection, window string, minutes int, used float64) {
		t.Helper()
		n++
		if _, _, err := st.Append(event.Event{ID: "p-" + strconv.Itoa(n), Kind: event.PlanUsage, Time: at, Vendor: "anthropic", Exchange: "x-1",
			Plan: &event.Plan{Subscription: sub, Connection: connection, Window: window, WindowMinutes: minutes, UsedPercent: used, ResetsAt: &reset, Status: "allowed"}}); err != nil {
			t.Fatal(err)
		}
	}
	usage := func() map[string][]PlanReading {
		t.Helper()
		resp, raw := do(t, e.req(t, "GET", "/api/v0/subscriptions", nil))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		var l SubscriptionList
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatal(err)
		}
		out := map[string][]PlanReading{}
		for _, s := range l.Subscriptions {
			out[s.Harness] = s.Usage
		}
		return out
	}

	accountA := connect()
	plan(clock.Add(time.Minute), "claude", accountA, "5h", 300, 90)
	clock = clock.Add(10 * time.Minute)
	accountB := connect()
	if accountB == accountA {
		t.Fatal("a new connect kept its connection")
	}
	// A's call was on the way: its reply is stored after B connected.
	plan(clock.Add(time.Minute), "claude", accountA, "7d", 10080, 80)
	if u := usage()["claude"]; len(u) != 0 {
		t.Fatalf("account A's readings show under account B: %+v", u)
	}
	plan(clock.Add(2*time.Minute), "claude", accountB, "7d", 10080, 60)
	plan(clock.Add(3*time.Minute), "claude", accountB, "7d", 10080, 69)
	// The latest reading of A's 7d is stored after B's: it hides none of B's.
	plan(clock.Add(4*time.Minute), "claude", accountA, "7d", 10080, 81)
	plan(clock.Add(5*time.Minute), "codex", "c-codex", "primary", 300, 5) // codex is not connected

	got := usage()
	u := got["claude"]
	if len(u) != 1 || u[0].Window != "7d" || u[0].UsedPercent != 69 || u[0].WindowMinutes != 10080 ||
		u[0].ResetsAt == nil || !u[0].ResetsAt.Equal(reset) || u[0].Status != "allowed" || !u[0].ReadAt.Equal(clock.Add(3*time.Minute)) {
		t.Fatalf("claude usage: %+v", u)
	}
	if len(got["codex"]) != 0 {
		t.Fatalf("codex, not connected, shows usage: %+v", got["codex"])
	}

	// A 5h reading of B comes first.
	plan(clock.Add(6*time.Minute), "claude", accountB, "5h", 300, 38)
	if u := usage()["claude"]; len(u) != 2 || u[0].Window != "5h" || u[0].UsedPercent != 38 || u[1].Window != "7d" {
		t.Fatalf("claude usage: %+v", u)
	}
}
