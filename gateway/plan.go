package gateway

// Plan usage: how much of the person's subscription plan is used, as the
// vendor says in a few headers of every model reply. It is the one place a
// header's value reaches an event besides the User-Agent's first token, and
// only from the names in planHeaders, on a call that carried a subscription
// of control's (Relayed.Signin, Credential.Read). Each value is parsed
// into a number, a time or one short word; anything else is left out.
//
// Seen on (2026-11-14, agentbus-notes research/2026-11-14-vendor-quota-signals.md):
//   - claude's subscription: anthropic-ratelimit-unified-7d-utilization
//     (a fraction, 0..1), -7d-reset (Unix seconds) and -7d-status, measured
//     on one Claude Code reply. The 5h names are read the same way; no reply
//     has carried them yet.
//   - codex's ChatGPT sign-in: x-codex-primary-used-percent (0..100),
//     -window-minutes and -reset-at (Unix seconds), and the same for
//     secondary, from codex-rs/codex-api/src/rate_limits.rs at
//     048431dcbecc510f1879c9421a97220016ecac44. Not yet seen through the
//     gateway.

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/staysup/agentbus/event"
)

// planWindow is one window's headers: the percent used, and when it resets.
// scale turns the used value into a percent; minutes is the window's length
// when the vendor fixes it by name, else minutesHeader says it.
type planWindow struct {
	name                string
	used, reset, status string
	minutesHeader       string
	minutes             int
	scale               float64
}

// planHeaders is every header the gateway reads a value from for plan usage.
// No other header is read.
var planHeaders = []planWindow{
	{name: "5h", used: "Anthropic-Ratelimit-Unified-5h-Utilization", reset: "Anthropic-Ratelimit-Unified-5h-Reset",
		status: "Anthropic-Ratelimit-Unified-5h-Status", minutes: 5 * 60, scale: 100},
	{name: "7d", used: "Anthropic-Ratelimit-Unified-7d-Utilization", reset: "Anthropic-Ratelimit-Unified-7d-Reset",
		status: "Anthropic-Ratelimit-Unified-7d-Status", minutes: 7 * 24 * 60, scale: 100},
	{name: "primary", used: "X-Codex-Primary-Used-Percent", reset: "X-Codex-Primary-Reset-At",
		minutesHeader: "X-Codex-Primary-Window-Minutes", scale: 1},
	{name: "secondary", used: "X-Codex-Secondary-Used-Percent", reset: "X-Codex-Secondary-Reset-At",
		minutesHeader: "X-Codex-Secondary-Window-Minutes", scale: 1},
}

// planWord is the shape a status word must have to be kept.
var planWord = regexp.MustCompile(`^[a-z_]{1,32}$`)

// planReadings reads si's plan usage from a reply's headers: one reading
// per window whose used value is a number. Nothing else of h is read.
func planReadings(h http.Header, si Signin) []event.Plan {
	var out []event.Plan
	for _, w := range planHeaders {
		used, ok := planNumber(h.Get(w.used))
		if !ok || used < 0 {
			continue
		}
		p := event.Plan{Subscription: si.Subscription, Connection: si.Connection, Window: w.name, WindowMinutes: w.minutes,
			UsedPercent: math.Round(used*w.scale*100) / 100}
		if w.minutesHeader != "" {
			if m, err := strconv.Atoi(h.Get(w.minutesHeader)); err == nil && m > 0 && m <= 366*24*60 {
				p.WindowMinutes = m
			}
		}
		if s, err := strconv.ParseInt(h.Get(w.reset), 10, 64); err == nil && s > 0 && s < 1<<35 {
			t := time.Unix(s, 0).UTC()
			p.ResetsAt = &t
		}
		if w.status != "" {
			if s := h.Get(w.status); planWord.MatchString(s) {
				p.Status = s
			}
		}
		out = append(out, p)
	}
	return out
}

// planNumber parses a header's number, finite only.
func planNumber(s string) (float64, bool) {
	if s == "" || len(s) > 32 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// planRefresh is how long a reading that has not changed goes unstored:
// past it, the next reply that carries it stores it again, so the record
// says how recent it is.
const planRefresh = 10 * time.Minute

// planSeen is the last reading stored, per subscription, connect and
// window: a new connect's first reading is stored even when it is the same.
type planSeen struct {
	mu   sync.Mutex
	last map[[3]string]planStored
}

type planStored struct {
	plan event.Plan
	at   time.Time
}

// changed says p should be stored at now: it differs from the last stored
// for its subscription, connect and window, or that one is planRefresh old. It
// records p as stored when so.
func (s *planSeen) changed(p event.Plan, now time.Time) bool {
	k := [3]string{p.Subscription, p.Connection, p.Window}
	s.mu.Lock()
	defer s.mu.Unlock()
	if was, ok := s.last[k]; ok && samePlan(was.plan, p) && now.Sub(was.at) < planRefresh {
		return false
	}
	if s.last == nil {
		s.last = map[[3]string]planStored{}
	}
	s.last[k] = planStored{plan: p, at: now}
	return true
}

func samePlan(a, b event.Plan) bool {
	if (a.ResetsAt == nil) != (b.ResetsAt == nil) || (a.ResetsAt != nil && !a.ResetsAt.Equal(*b.ResetsAt)) {
		return false
	}
	return a.UsedPercent == b.UsedPercent && a.WindowMinutes == b.WindowMinutes && a.Status == b.Status
}

// readPlan stores the plan usage a reply to ex carries, when ex's call used
// a subscription of control's. The event names the call it came from and
// no session: the plan is the sign-in's, not a session's.
func (g *Gateway) readPlan(ex *exchange, h http.Header) {
	if ex.signin.Subscription == "" {
		return
	}
	now := g.now()
	for _, p := range planReadings(h, ex.signin) {
		if !g.plans.changed(p, now) {
			continue
		}
		g.queue.push(event.Event{
			ID:       "e-" + g.run + "-" + strconv.FormatUint(g.seq.Add(1), 10),
			Kind:     event.PlanUsage,
			Time:     now,
			Exchange: ex.id,
			Vendor:   g.adapter.Vendor(),
			Plan:     &p,
		})
	}
}
