package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// TestWindowFor: current Claude models and Gemini hold 1M; Haiku 4.5 and older
// Claude, an unrecognised model and an empty id take the 200k fallback (Claude
// API model table, cached 2026-11-03).
func TestWindowFor(t *testing.T) {
	for _, c := range []struct {
		model string
		want  int
	}{
		{"claude-opus-5-5-20261001", 1000000}, // dated current Opus
		{"claude-sonnet-5-5", 1000000},        // current Sonnet (the one Codex was right about)
		{"claude-haiku-5-5", 1000000},         // current Haiku
		{"claude-fable-5-2", 1000000},         // Fable 5.x
		{"claude-opus-4-6", 1000000},          // 4.6 generation is current
		{"claude-haiku-4-5", 200000},          // Haiku 4.5 held 200k
		{"claude-opus-4-1-20250805", 200000},  // the 4.1 generation held 200k, dated
		{"claude-sonnet-4-0", 200000},         // the 4.0 generation held 200k
		{"claude-3-5-sonnet", 200000},         // the Claude 3 generation held 200k
		{"gemini-3-pro", 1000000},
		{"gpt-6.1-sol", defaultWindow}, // unrecognised vendor id
		{"some-new-model", defaultWindow},
		{"", defaultWindow},
	} {
		if got := windowFor(c.model); got != c.want {
			t.Errorf("windowFor(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// TestContextWindow: the practical number. A turn with usage reports percent
// full and, for a known harness, where it compacts with the point's source. No
// usage is no window.
func TestContextWindow(t *testing.T) {
	a := &API{} // nil Settings: the built-in compaction defaults

	if w := a.contextWindow("claude-opus-5-5", "claude", 0); w != nil {
		t.Errorf("used 0 should give no window, got %+v", w)
	}

	w := a.contextWindow("claude-opus-5-5", "claude", 100000)
	if w == nil {
		t.Fatal("no window for a used prompt")
	}
	// A current Claude model holds 1M, an assumed default never claimed known
	// until a harness reports its own (0.7): 100k of 1M is 10% full.
	if w.WindowTokens != 1000000 || w.WindowKnown {
		t.Errorf("window = %d known %v, want 1000000 assumed (known false)", w.WindowTokens, w.WindowKnown)
	}
	if w.PercentFull != 10 {
		t.Errorf("percent_full = %d, want 10", w.PercentFull)
	}
	if w.CompactionPoint != 0.80 || w.CompactionPct != 80 || w.CompactionSource != "default" || !w.CompactionIsDefault {
		t.Errorf("compaction = %v/%d/%s/default=%v, want 0.8/80/default/true", w.CompactionPoint, w.CompactionPct, w.CompactionSource, w.CompactionIsDefault)
	}

	// opencode's point is read from its own source (settings/context.go),
	// labelled vendor.
	w = a.contextWindow("claude-opus-5-5", "opencode", 10000)
	if w.CompactionPoint != 0.85 || w.CompactionSource != "vendor" {
		t.Errorf("opencode compaction = %v/%s, want 0.85/vendor", w.CompactionPoint, w.CompactionSource)
	}

	// An unknown harness has no compaction mark, but still a window.
	w = a.contextWindow("claude-opus-5-5", "", 10000)
	if w.CompactionPoint != 0 || w.CompactionSource != "" {
		t.Errorf("unknown harness carried a compaction mark: %+v", w)
	}
	if w.WindowTokens != 1000000 {
		t.Errorf("unknown harness lost its window: %+v", w)
	}

	// An unrecognised model takes the 200k fallback, assumed like the rest.
	w = a.contextWindow("mystery", "claude", 10000)
	if w.WindowTokens != defaultWindow || w.WindowKnown {
		t.Errorf("unrecognised model = %d known %v, want %d assumed", w.WindowTokens, w.WindowKnown, defaultWindow)
	}

	// Haiku 4.5 is one of the older 200k Claude models.
	w = a.contextWindow("claude-haiku-4-5", "claude", 10000)
	if w.WindowTokens != 200000 {
		t.Errorf("haiku-4-5 window = %d, want 200000", w.WindowTokens)
	}

	// Gemini is 1M.
	w = a.contextWindow("gemini-3-pro", "gemini", 100000)
	if w.WindowTokens != 1000000 || w.PercentFull != 10 {
		t.Errorf("gemini window/percent = %d/%d, want 1000000/10", w.WindowTokens, w.PercentFull)
	}
}

// startedTurn records a turn.started with a harness, so the context view can
// name where that vendor's harness compacts.
func turnWithHarness(t *testing.T, e *env, session, turn, harness string) {
	t.Helper()
	if _, err := e.store.AppendOnce(event.Event{ID: "th-" + session + "-" + turn, Kind: event.TurnStarted,
		Time: time.Now().UTC(), Session: session, Turn: turn, Harness: harness}); err != nil {
		t.Fatal(err)
	}
}

// TestTurnContextWindowRoute: the turn's context view carries the window block,
// with percent full and the harness's compaction point.
func TestTurnContextWindowRoute(t *testing.T) {
	e := newEnv(t)
	turnWithHarness(t, e, "sw", "tw", "claude")
	// A kept request piece makes the peak call's body available, so the model
	// (and thus the window) resolves from it, as in a real turn.
	call(t, e, "sw", "tw", "xw", "claude-opus-5-5", event.Usage{InputTokens: 20000, OutputTokens: 100, CacheReadTokens: 80000},
		piece(body.System, "", `{"type":"text","text":"you are a careful agent"}`))

	resp, bodyStr := do(t, e.req(t, "GET", "/api/v0/turns/tw/context", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, bodyStr)
	}
	var tc TurnContext
	if err := json.Unmarshal([]byte(bodyStr), &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Window == nil {
		t.Fatal("window is null")
	}
	// used = 20000 + 80000 = 100000; claude-opus-5-5 window 1M → 10%.
	if tc.Window.UsedTokens != 100000 || tc.Window.PercentFull != 10 {
		t.Errorf("used=%d percent=%d, want 100000/10", tc.Window.UsedTokens, tc.Window.PercentFull)
	}
	if tc.Window.Harness != "claude" || tc.Window.CompactionPct != 80 || tc.Window.CompactionSource != "default" {
		t.Errorf("window harness/compaction = %s/%d/%s, want claude/80/default", tc.Window.Harness, tc.Window.CompactionPct, tc.Window.CompactionSource)
	}
}

// TestCompactionSettingsRoute: read the points, change one with If-Match, read
// it back changed, and a share out of range is refused.
func TestCompactionSettingsRoute(t *testing.T) {
	e, _ := settingsEnv(t)

	resp, out := send(t, e, "GET", "/api/v0/settings/compaction", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET %d %s", resp.StatusCode, out)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the compaction points")
	}
	var c map[string]float64
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c["opencode"] != 0.85 || c["claude"] != 0.80 {
		t.Errorf("defaults = %+v, want opencode 0.85 claude 0.8", c)
	}

	// Out of range is refused.
	if resp, _ := send(t, e, "PUT", "/api/v0/settings/compaction", etag, `{"claude":2}`); resp.StatusCode != 400 {
		t.Errorf("out-of-range share: %d, want 400", resp.StatusCode)
	}

	// A valid change with the current ETag.
	resp, out = send(t, e, "PUT", "/api/v0/settings/compaction", etag, `{"claude":0.6}`)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT %d %s", resp.StatusCode, out)
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c["claude"] != 0.6 || c["opencode"] != 0.85 {
		t.Errorf("after change = %+v, want claude 0.6 opencode 0.85", c)
	}
	if !strings.Contains(out, "codex") {
		t.Error("the reply should carry every vendor filled")
	}
}
