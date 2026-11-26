package api

// The practical number of the context view: how full a turn's context is
// against the model's window, and where that vendor's harness compacts. The
// window is a per-model fact AgentBus pins (a documented default when the
// model is unknown); the compaction point is a per-vendor setting
// (settings.Compaction). Neither invents a number: an unknown window is
// flagged, and a compaction point the vendor does not publish is labelled a
// stated default. Compaction itself is not built or decided here (that is 0.7).

import "github.com/staysup/agentbus/settings"

// defaultWindow is the window used for a model AgentBus does not recognise.
// Flagged with WindowKnown false so the view says it is assumed, not read.
const defaultWindow = settings.DefaultWindow

// ContextWindow is how full a prompt is against the model's window, and where
// the vendor's harness compacts. Null on a turn or session with no model call.
type ContextWindow struct {
	Model        string `json:"model" doc:"The model the window is for, from the prompt's own request. Empty when no call named one."`
	Harness      string `json:"harness" doc:"The vendor's harness that ran the turn: claude, codex, opencode or gemini. The compaction point is per harness. Empty when no run started the turn."`
	WindowTokens int    `json:"window_tokens" doc:"The model's context window in tokens, the whole prompt it can hold: an assumed default, not a confirmed figure. Current Claude models and Gemini are 1M; Haiku 4.5 and older Claude, and anything unrecognised, take the 200k fallback (Claude API model table, cached 2026-11-03)."`
	WindowKnown  bool   `json:"window_known" doc:"Whether window_tokens is confirmed from the harness's own report rather than an assumed default. Always false until 0.7, when a harness reports its own window; until then every window here is an assumed default and the view says so. Never claims certainty AgentBus does not have."`
	UsedTokens   int    `json:"used_tokens" doc:"How much of the window the prompt used: input + cache_read + cache_write, the vendor's own count. Same as context_tokens."`
	PercentFull  int    `json:"percent_full" doc:"used_tokens as a percent of window_tokens, rounded. The number a person watches: how full the context is."`
	// Compaction, when the harness has a point.
	CompactionPoint     float64 `json:"compaction_point,omitempty" doc:"The share of the window (0 to 1) at which this harness compacts, from settings.Compaction. Absent when the harness has no point (an unknown harness)."`
	CompactionPct       int     `json:"compaction_pct,omitempty" doc:"compaction_point as a rounded percent, where the view draws the 'compacts at ~Y%' mark. Absent with compaction_point."`
	CompactionSource    string  `json:"compaction_source,omitempty" doc:"Where the compaction point comes from: vendor (the vendor's own open source, for opencode and gemini) or default (a stated choice, for claude and codex, which publish none). Absent when the harness has no point."`
	CompactionIsDefault bool    `json:"compaction_is_default" doc:"True when the point is the built-in default, not one the person set. With compaction_source default, the view says the vendor publishes none and this is editable."`
}

// windowFor is a model's assumed context window in tokens (settings.ModelWindow,
// which own replay's history budget reads too).
func windowFor(model string) int { return settings.ModelWindow(model) }

// contextWindow builds the ContextWindow for a prompt of used tokens on model,
// run by harness. Nil when used is 0 (no model call reported usage): there is
// no context to measure.
func (a *API) contextWindow(model, harness string, used int) *ContextWindow {
	if used <= 0 {
		return nil
	}
	// Every window is an assumed default until a harness reports its own (0.7),
	// so window_known is false: AgentBus does not yet confirm any model's window
	// and will not claim it does. The magnitude is the research note's default.
	window := windowFor(model)
	out := &ContextWindow{Model: model, Harness: harness, WindowTokens: window, WindowKnown: false, UsedTokens: used}
	if window > 0 {
		out.PercentFull = int((float64(used)/float64(window))*100 + 0.5)
	}
	if point, source, isDefault, ok := a.compactionFor(harness); ok {
		out.CompactionPoint = point
		out.CompactionPct = int(point*100 + 0.5)
		out.CompactionSource = source
		out.CompactionIsDefault = isDefault
	}
	return out
}

// compactionFor is harness's compaction point, its source and whether it is the
// built-in default, reading the stored setting over the built-in defaults. The
// last return is false when the harness has no point (an unknown harness), so
// the view shows no mark rather than a zero.
func (a *API) compactionFor(harness string) (point float64, source string, isDefault, ok bool) {
	source = settings.CompactionSource(harness)
	if source == "" {
		return 0, "", false, false
	}
	c := settings.DefaultCompaction()
	if a.cfg.Settings != nil {
		if cs, isDB := a.cfg.Settings.(compactionStore); isDB {
			if got, _, err := cs.Compaction(); err == nil {
				c = got
			}
		}
	}
	p, has := c.Point(harness)
	if !has {
		return 0, source, false, false
	}
	return p, source, c.IsDefault(harness), true
}
