package settings

// The compaction points: per vendor, the share of a model's context window at
// which that vendor's harness compacts its context. The context view shows it
// as "compacts at ~Y%". It is a setting, not a fixed number, because the
// vendors disagree and move: opencode and Gemini CLI publish a real rule in
// their own open source (opencode keeps a token reserve free, ~85% of a 200k
// window; Gemini CLI a share); Claude Code and Codex publish none, so
// their point here is a stated default the person can change, never a hidden
// invented number. A person reads it at GET /api/v0/settings/compaction and
// changes it at PUT. Nothing here runs a harness's compaction.
//
// The same setting holds AgentBus's own history budget (Budget): the share of
// the model's window that own replay keeps each turn's carried history
// within, well below every harness's point (replay/budget.go), and the
// window table both read (ModelWindow).

import (
	"errors"
	"fmt"
)

// Compaction holds the per-vendor compaction points. A nil field is a value
// the person left out; once read from a store each is filled with its default
// (DefaultCompaction). A value is a share of the window in (0, 1].
type Compaction struct {
	Claude   *float64 `toml:"claude" json:"claude,omitempty" doc:"The share (0 to 1) of the model's window at which Claude Code compacts. Claude Code publishes no number, so this is a stated default you can change, not a value read from the vendor. Absent in a PUT: 0.8."`
	Codex    *float64 `toml:"codex" json:"codex,omitempty" doc:"The share (0 to 1) of the model's window at which Codex CLI compacts. Codex publishes no fixed number, so this is a stated default you can change. Absent in a PUT: 0.8."`
	Opencode *float64 `toml:"opencode" json:"opencode,omitempty" doc:"The share (0 to 1) of the model's window at which opencode compacts. opencode's own open source compacts when a turn's tokens reach the window less a reserve of up to 32k tokens for its reply: 0.84 of a 200k window, more of a larger one. Absent in a PUT: 0.85."`
	Gemini   *float64 `toml:"gemini" json:"gemini,omitempty" doc:"The share (0 to 1) of the model's window at which Gemini CLI compacts. Gemini CLI's config default is 0.7. Absent in a PUT: 0.7."`
	Budget   *float64 `toml:"budget" json:"budget,omitempty" doc:"AgentBus's own history budget: the share (0 to 1) of the model's window that the history own replay builds for a turn is kept within, by shortening older turns in frozen steps (long tool results and tool inputs to stubs the agent can fetch back, then the oldest turns to AgentBus's summary). Kept well below every harness's point, so a harness rarely compacts. 1 turns the budget off in practice. Absent in a PUT: 0.4."`
}

// The built-in defaults. opencode and Gemini are read from their own open
// source (SourceVendor); claude and codex are a stated choice, since neither
// vendor publishes a number (SourceDefault). research/2026-11-18-context-
// compaction-points.md holds the provenance of each.
const (
	defaultClaudePoint = 0.80
	defaultCodexPoint  = 0.80 // codex caps at min(setting, 0.9 of window); 0.8 a stated default under that cap
	// opencode compacts on a token reserve, not a share (opencode v1.19.6,
	// packages/opencode/src/session/overflow.ts, isOverflow and usable): at
	// limit.input - min(20000, maxOutputTokens) when the model declares an
	// input limit, else at limit.context - maxOutputTokens, where
	// maxOutputTokens = min(limit.output, 32000) (provider/transform.ts). For
	// a 200k window and a 32k reply that is 0.84, which 0.85 stands for; a
	// larger window compacts later (about 0.97 of 1M).
	defaultOpencodePoint = 0.85
	defaultGeminiPoint   = 0.70
	// defaultBudget is AgentBus's own history budget: half of the lowest
	// default point (Gemini's 0.7) would be 0.35, Claude Code's 0.8 halved
	// 0.4. 0.4 leaves a turn the other half of the window before any
	// harness compacts (his call, 2026-11-26: start at 40%).
	defaultBudget = 0.40
)

// Where a compaction point's number comes from, so the view never shows a
// stated default as if the vendor published it.
const (
	// SourceVendor: the vendor's own open source gives the number.
	SourceVendor = "vendor"
	// SourceDefault: the vendor publishes none, so this is our stated default.
	SourceDefault = "default"
)

// CompactionSource is where harness's built-in point comes from: SourceVendor
// for opencode and gemini, SourceDefault for claude and codex. Empty for a
// harness with no built-in point.
func CompactionSource(harness string) string {
	switch harness {
	case "opencode", "gemini":
		return SourceVendor
	case "claude", "codex":
		return SourceDefault
	default:
		return ""
	}
}

// DefaultCompaction is the compaction points of a store that holds none: every
// field set to its built-in default.
func DefaultCompaction() Compaction {
	cl, co, op, ge, b := defaultClaudePoint, defaultCodexPoint, defaultOpencodePoint, defaultGeminiPoint, defaultBudget
	return Compaction{Claude: &cl, Codex: &co, Opencode: &op, Gemini: &ge, Budget: &b}
}

// fill sets every field left out to its default.
func (c *Compaction) fill() {
	d := DefaultCompaction()
	if c.Claude == nil {
		c.Claude = d.Claude
	}
	if c.Codex == nil {
		c.Codex = d.Codex
	}
	if c.Opencode == nil {
		c.Opencode = d.Opencode
	}
	if c.Gemini == nil {
		c.Gemini = d.Gemini
	}
	if c.Budget == nil {
		c.Budget = d.Budget
	}
}

// Budgeter is a store that holds the history budget: control's database, or
// a runner's view of its server's (Remote). Own replay reads it at each
// turn's start, the same way on every runner.
type Budgeter interface {
	Budget() (float64, error)
}

// BudgetOf is a reader of st's history budget, read each time it is called.
// A store that holds none gives the default; so does a read that fails, since
// the budget only sizes a history and no rule hangs on it.
func BudgetOf(st Store) func() float64 {
	b, ok := st.(Budgeter)
	return func() float64 {
		if !ok {
			return defaultBudget
		}
		v, err := b.Budget()
		if err != nil || v <= 0 || v > 1 {
			return defaultBudget
		}
		return v
	}
}

// BudgetShare is the history budget of a filled Compaction, or the built-in
// default when it holds none.
func (c Compaction) BudgetShare() float64 {
	if c.Budget == nil {
		return defaultBudget
	}
	return *c.Budget
}

// Point is harness's compaction point and whether one is known. It reads a
// filled Compaction: call fill first. A harness with no field (an unknown
// vendor) returns 0, false.
func (c Compaction) Point(harness string) (float64, bool) {
	var p *float64
	switch harness {
	case "claude":
		p = c.Claude
	case "codex":
		p = c.Codex
	case "opencode":
		p = c.Opencode
	case "gemini":
		p = c.Gemini
	}
	if p == nil {
		return 0, false
	}
	return *p, true
}

// IsDefault is true when harness's point in c equals the built-in default, so
// the view can say the number is the stated default rather than one the person
// set. c must be filled.
func (c Compaction) IsDefault(harness string) bool {
	got, ok := c.Point(harness)
	if !ok {
		return false
	}
	def, _ := DefaultCompaction().Point(harness)
	return got == def
}

// Check refuses a value outside (0, 1], in words that name the vendor.
func (c Compaction) Check() error {
	for _, f := range []struct {
		name string
		v    *float64
	}{{"claude", c.Claude}, {"codex", c.Codex}, {"opencode", c.Opencode}, {"gemini", c.Gemini}, {"budget", c.Budget}} {
		if f.v == nil {
			continue
		}
		if *f.v != *f.v { // NaN
			return fmt.Errorf("%s must be a share between 0 and 1", f.name)
		}
		if *f.v <= 0 || *f.v > 1 {
			return fmt.Errorf("%s must be greater than 0 and at most 1 (a share of the window, such as 0.8)", f.name)
		}
	}
	return nil
}

// overCompaction is c, read with its unset fields left nil, on top of base:
// each field c leaves out takes base's, and the result is filled.
func (c Compaction) overCompaction(base Compaction) Compaction {
	out := base
	if c.Claude != nil {
		out.Claude = c.Claude
	}
	if c.Codex != nil {
		out.Codex = c.Codex
	}
	if c.Opencode != nil {
		out.Opencode = c.Opencode
	}
	if c.Gemini != nil {
		out.Gemini = c.Gemini
	}
	if c.Budget != nil {
		out.Budget = c.Budget
	}
	out.fill()
	return out
}

// onlyChanged is c with every field that equals its default dropped to nil, so
// a store keeps only what the person changed. c must be filled.
func (c Compaction) onlyChanged() Compaction {
	d := DefaultCompaction()
	out := Compaction{}
	if c.Claude != nil && *c.Claude != *d.Claude {
		out.Claude = c.Claude
	}
	if c.Codex != nil && *c.Codex != *d.Codex {
		out.Codex = c.Codex
	}
	if c.Opencode != nil && *c.Opencode != *d.Opencode {
		out.Opencode = c.Opencode
	}
	if c.Gemini != nil && *c.Gemini != *d.Gemini {
		out.Gemini = c.Gemini
	}
	if c.Budget != nil && *c.Budget != *d.Budget {
		out.Budget = c.Budget
	}
	return out
}

// changedCompaction names the vendors whose point differs between a and b,
// for a change's keys. Both must be filled.
func changedCompaction(a, b Compaction) []string {
	var keys []string
	for _, f := range []struct {
		name string
		x, y *float64
	}{{"claude", a.Claude, b.Claude}, {"codex", a.Codex, b.Codex}, {"opencode", a.Opencode, b.Opencode}, {"gemini", a.Gemini, b.Gemini}, {"budget", a.Budget, b.Budget}} {
		if f.x == nil || f.y == nil || *f.x != *f.y {
			keys = append(keys, f.name)
		}
	}
	return keys
}

// errCompaction is the error used when a stored compaction cannot be read.
var errCompaction = errors.New("the stored compaction points are not readable")
