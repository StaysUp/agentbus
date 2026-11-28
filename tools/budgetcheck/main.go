// Command budgetcheck measures the history budget on a session already
// recorded, offline, with no model call. For each turn it rebuilds the
// history own replay would have put in front of it with the budget off and
// with it on (replay.WhatIf, the same fit the gateway runs), and prices both
// against what the session really paid, with the prompt cache modelled from
// the session's own recorded usage.
//
//	go run ./tools/budgetcheck -data ~/.local/share/agentbus -session <id>
//
// It opens the record read-only (store.OpenReadOnly) and may run beside the
// service. See tools/budgetcheck/README.md for the model and its limits.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/staysup/agentbus/cost"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/replay"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "budgetcheck:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("budgetcheck", flag.ContinueOnError)
	data := fs.String("data", "", "the record: a data folder, its events.db, or its jobs.db (events.db beside it is read)")
	session := fs.String("session", "", "the session to measure")
	share := fs.Float64("budget", settings.DefaultCompaction().BudgetShare(), "the budget for the on arm, a share of each model's window")
	format := fs.String("format", "", "the history format (anthropic, openai, chatcompletions); default: the session's harness's")
	jsonOut := fs.String("json", "", "also write the whole report to this file, as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" || *session == "" {
		return errors.New("-data and -session are needed")
	}
	st, err := store.OpenReadOnly(eventsPath(*data))
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := measure(st, *session, *share, *format)
	if err != nil {
		return err
	}
	render(stdout, rep)
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(*jsonOut, b, 0o600)
	}
	return nil
}

// eventsPath is the record's file: path itself, or events.db in the folder
// path names or beside the jobs.db it names.
func eventsPath(path string) string {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return filepath.Join(path, "events.db")
	}
	if filepath.Base(path) == "jobs.db" {
		return filepath.Join(filepath.Dir(path), "events.db")
	}
	return path
}

// Report is one session measured.
type Report struct {
	Session string  `json:"session"`
	Format  string  `json:"format"`
	Share   float64 `json:"budget_share"`
	// Scale is the session's real tokens per budget token, over the calls
	// that carried a history: the vendor's count of a request over
	// approxTokens of its bytes. A history's change in tokens is priced as
	// its change in budget tokens times Scale.
	Scale float64 `json:"scale"`
	// ScaleMeasured is false when no call that carried a history had usage:
	// Scale is then 1, assumed, not measured.
	ScaleMeasured bool `json:"scale_measured"`
	// WriteAsInput counts the calls of a vendor other than Anthropic that
	// wrote nothing to the cache, whose arm's new tokens were billed as
	// uncached input (armCall). That is how OpenAI bills; for another
	// vendor it is an assumption.
	WriteAsInput int `json:"calls_write_as_input"`
	// TTL is the cache lifetime the session wrote, used where the record
	// cannot say whether the cache was warm.
	TTL   string `json:"cache_ttl"`
	Turns []Turn `json:"turns"`
	Real  Totals `json:"real"`
	Off   Totals `json:"off"`
	On    Totals `json:"on"`
	// NoUsage counts the calls with no recorded usage, priced at nothing.
	NoUsage int `json:"calls_without_usage"`
	// OffMatches and OnMatches count the turns whose rebuilt history is
	// within 1% of what the record carried: on a session that ran without
	// the budget the off arm must match every turn, with it the on arm.
	OffMatches, OnMatches, Replayed int
}

// Turn is one turn's row.
type Turn struct {
	replay.WhatIfTurn `json:"history"`
	Start             time.Time `json:"start"`
	// Recorded is the history.shortened the gateway recorded at this turn,
	// if any.
	Recorded           *event.HistoryStep `json:"recorded_step,omitempty"`
	Real, Off, On      Usage
	CarryingCalls, All int
}

// Usage is what one arm sent and paid, for a turn or a session.
type Usage struct {
	Input, CacheRead, CacheWrite, Output int64
	USD                                  float64
}

// Totals is a session's sums for one arm.
type Totals struct {
	Usage
	Sent       int64   `json:"tokens_sent"`
	CacheShare float64 `json:"cache_share"`
	Steps      int     `json:"steps"`
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
	u.USD += o.USD
}

func measure(st *store.Store, session string, share float64, format string) (*Report, error) {
	if format == "" {
		name, err := st.SessionHarness(session)
		if err != nil {
			return nil, err
		}
		h, ok := harness.Get(name)
		if !ok || h.Info().Replay == "" {
			return nil, fmt.Errorf("session %s: harness %q has no own replay; name one with -format", session, name)
		}
		format = h.Info().Replay
	}
	var r *replay.Replayer
	switch format {
	case harness.FormatAnthropic:
		r = replay.New(st)
	case harness.FormatOpenAI:
		r = replay.NewCodex(st)
	case harness.FormatChatCompletions:
		r = replay.NewChatCompletions(st)
	default:
		return nil, fmt.Errorf("no own replay reads format %q", format)
	}
	turns, err := r.WhatIf(session, share)
	if err != nil {
		return nil, err
	}
	responded, steps, err := readEvents(st, session)
	if err != nil {
		return nil, err
	}
	return price(session, format, share, turns, responded, steps, cost.Default()), nil
}

// readEvents is the session's model.responded by exchange, and its
// history.shortened by turn.
func readEvents(st *store.Store, session string) (map[string]event.Event, map[string]*event.HistoryStep, error) {
	responded := map[string]event.Event{}
	steps := map[string]*event.HistoryStep{}
	f := store.Filter{Session: session, Only: true, Kinds: []string{string(event.ModelResponded), string(event.HistoryShortened)}}
	var after int64
	for {
		recs, more, err := st.Page(after, f, 1000, store.PageBytes)
		if err != nil {
			return nil, nil, err
		}
		for _, rec := range recs {
			after = rec.Seq
			var e event.Event
			if err := json.Unmarshal(rec.Data, &e); err != nil {
				return nil, nil, fmt.Errorf("event %d: %w", rec.Seq, err)
			}
			switch e.Kind {
			case event.ModelResponded:
				if e.Exchange != "" && !e.Probe {
					responded[e.Exchange] = e
				}
			case event.HistoryShortened:
				if e.History != nil {
					steps[e.Turn] = e.History
				}
			}
		}
		if !more {
			return responded, steps, nil
		}
	}
}

// warmShare is how much of the history a call's cache read must cover for
// the cache to count as warm over it.
const warmShare = 0.8

// warmFloor is the cached history, in real tokens, below which the reads
// cannot tell a warm cache from the system prompt alone; the gap in time
// from the previous carrying call decides then.
const warmFloor = 1024

// price models each call of each turn in both arms and prices it.
//
// The model, per call that carried a history (the rest are the same in every
// arm, as recorded):
//   - The request grows or shrinks by the arm's history less the real one,
//     times Scale. Output is as recorded.
//   - The cache: a call's recorded cache read covers the system prompt and
//     tools, then the history, then (inside a turn) the turn's own messages.
//     What the arm changes is the history's share of it. At a turn's first
//     carrying call that is the history's cached prefix (WhatIfHistory.Prefix:
//     a step breaks it at the first message it changed); at a later one the
//     whole history. When the recorded read shows the cache warm over the
//     real history, the arm reads its own cached part instead: read - real +
//     arm. When it was cold, the arm reads what the call read.
//   - What is not read is written to the cache, with the call's own split of
//     lifetimes; uncached input stays as recorded. A vendor that wrote
//     nothing to the cache (OpenAI) bills the rest as input.
//   - The cost is the call's recorded cost plus the table's price of the
//     arm's usage less its price of the recorded usage, so the off arm of a
//     session that ran without the budget costs what the session paid.
func price(session, format string, share float64, turns []replay.WhatIfTurn,
	responded map[string]event.Event, steps map[string]*event.HistoryStep, table *cost.Table) *Report {
	rep := &Report{Session: session, Format: format, Share: share}
	var sent, approx float64
	ttl1h, ttl5m := int64(0), int64(0)
	for _, w := range turns {
		for _, c := range w.Calls {
			e, ok := responded[c.Exchange]
			if !ok || e.Usage == nil {
				continue
			}
			u := e.Usage
			ttl1h += int64(u.CacheWrite1hTokens)
			ttl5m += int64(u.CacheWrite5mTokens)
			if c.Carrier && w.From != "" {
				sent += float64(u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens)
				approx += float64(c.RequestTokens)
			}
		}
	}
	rep.Scale = 1
	if approx > 0 && sent > 0 {
		rep.Scale, rep.ScaleMeasured = sent/approx, true
	}
	ttl := 5 * time.Minute
	if ttl1h > ttl5m {
		ttl = time.Hour
	}
	rep.TTL = ttl.String()

	var lastCarrier time.Time
	for _, w := range turns {
		t := Turn{WhatIfTurn: w, Recorded: steps[w.Turn], All: len(w.Calls)}
		if w.On.Step != nil {
			rep.On.Steps++
		}
		if t.Recorded != nil {
			rep.Real.Steps++
		}
		if w.From != "" && w.Problem == "" {
			rep.Replayed++
			if near(w.Off.Tokens, w.Real.Tokens) {
				rep.OffMatches++
			}
			if near(w.On.Tokens, w.Real.Tokens) {
				rep.OnMatches++
			}
		}
		first := true
		for _, c := range w.Calls {
			if t.Start.IsZero() && !c.Time.IsZero() {
				t.Start = c.Time
			}
			e, ok := responded[c.Exchange]
			if !ok || e.Usage == nil {
				rep.NoUsage++
				continue
			}
			real := usageOf(e)
			t.Real.add(real)
			if !c.Carrier || w.From == "" || w.Problem != "" {
				t.Off.add(real)
				t.On.add(real)
				if c.Carrier {
					lastCarrier = c.Time
				}
				continue
			}
			t.CarryingCalls++
			gapWarm := !lastCarrier.IsZero() && !c.Time.IsZero() && c.Time.Sub(lastCarrier) <= ttl
			for _, a := range []struct {
				h   replay.WhatIfHistory
				out *Usage
			}{{w.Off, &t.Off}, {w.On, &t.On}} {
				a.out.add(armCall(e, real, w.Real, a.h, first, gapWarm, rep.Scale, ttl == time.Hour, table))
			}
			if writesAsInput(e, real) {
				rep.WriteAsInput++
			}
			first = false
			lastCarrier = c.Time
		}
		rep.Real.add(t.Real)
		rep.Off.add(t.Off)
		rep.On.add(t.On)
		rep.Turns = append(rep.Turns, t)
	}
	for _, x := range []*Totals{&rep.Real, &rep.Off, &rep.On} {
		x.Sent = x.Input + x.CacheRead + x.CacheWrite
		if x.Sent > 0 {
			x.CacheShare = float64(x.CacheRead) / float64(x.Sent)
		}
	}
	return rep
}

// near says a is within 1% of b, or within 50 tokens.
func near(a, b int) bool {
	d := math.Abs(float64(a - b))
	return d <= 50 || d <= 0.01*float64(b)
}

// usageOf is a call's recorded usage and cost.
func usageOf(e event.Event) Usage {
	u := e.Usage
	out := Usage{Input: int64(u.InputTokens), CacheRead: int64(u.CacheReadTokens),
		CacheWrite: int64(u.CacheWriteTokens), Output: int64(u.OutputTokens)}
	if e.CostUSD != nil {
		out.USD = *e.CostUSD
	}
	return out
}

// armCall is one carrying call in an arm whose history is h, the record's
// being real (see price). first says it is the turn's first carrying call;
// gapWarm that it came within the cache's lifetime of the previous one.
func armCall(e event.Event, rec Usage, real, h replay.WhatIfHistory, first, gapWarm bool, scale float64, hour bool, table *cost.Table) Usage {
	tok := func(n int) int64 { return int64(math.Round(float64(n) * scale)) }
	total := rec.Input + rec.CacheRead + rec.CacheWrite
	armTotal := max(total+tok(h.Tokens)-tok(real.Tokens), 0)
	realCached, armCached := tok(real.Tokens), tok(h.Tokens)
	if first {
		realCached, armCached = tok(real.Prefix), tok(h.Prefix)
	}
	warm := gapWarm
	if realCached >= warmFloor {
		warm = float64(rec.CacheRead) >= warmShare*float64(realCached)
	}
	read := rec.CacheRead
	if warm {
		read = rec.CacheRead - realCached + armCached
	}
	read = min(max(read, 0), armTotal)
	in := min(rec.Input, armTotal-read)
	write := armTotal - read - in
	if writesAsInput(e, rec) {
		in, write = in+write, 0
	}
	out := Usage{Input: in, CacheRead: read, CacheWrite: write, Output: rec.Output}

	// The arm's usage, as the vendor would report it, lifetimes split as the
	// call split them (or the session's lifetime when it wrote nothing).
	u := *e.Usage
	u.CostUSD, u.CostPartial = nil, false
	u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens = int(in), int(read), int(write)
	if rec.CacheWrite > 0 {
		f := float64(write) / float64(rec.CacheWrite)
		u.CacheWrite5mTokens = int(math.Round(float64(e.Usage.CacheWrite5mTokens) * f))
		u.CacheWrite1hTokens = int(math.Round(float64(e.Usage.CacheWrite1hTokens) * f))
		u.CacheWrite30mTokens = int(math.Round(float64(e.Usage.CacheWrite30mTokens) * f))
		u.CacheWrite12hTokens = int(math.Round(float64(e.Usage.CacheWrite12hTokens) * f))
		// Rounding never leaves a token of no lifetime, nor one too many.
		fix := int(write) - (u.CacheWrite5mTokens + u.CacheWrite1hTokens + u.CacheWrite30mTokens + u.CacheWrite12hTokens)
		switch {
		case u.CacheWrite1hTokens > 0:
			u.CacheWrite1hTokens += fix
		case u.CacheWrite5mTokens > 0:
			u.CacheWrite5mTokens += fix
		case u.CacheWrite30mTokens > 0:
			u.CacheWrite30mTokens += fix
		case u.CacheWrite12hTokens > 0:
			u.CacheWrite12hTokens += fix
		}
	} else {
		u.CacheWrite5mTokens, u.CacheWrite1hTokens, u.CacheWrite30mTokens, u.CacheWrite12hTokens = 0, 0, 0, 0
		if write > 0 && hour {
			u.CacheWrite1hTokens = int(write)
		} else if write > 0 {
			u.CacheWrite5mTokens = int(write)
		}
	}
	plain := *e.Usage
	plain.CostUSD, plain.CostPartial = nil, false
	ae, re := e, e
	ae.Usage, re.Usage = &u, &plain
	out.USD = rec.USD + table.Event(ae).USD - table.Event(re).USD
	return out
}

// writesAsInput says a call's vendor bills what is not read from the cache
// as uncached input: one other than Anthropic that wrote nothing to the
// cache. That is OpenAI's billing (its cache is written for free, so usage
// reports no writes); for any other such vendor it is an assumption, which
// the report counts (WriteAsInput).
func writesAsInput(e event.Event, rec Usage) bool {
	return rec.CacheWrite == 0 && e.Vendor != string(cost.Anthropic)
}

func render(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "Session %s (%s), budget %.0f%% of the window. %d turns, %d carried a history.\n",
		rep.Session, rep.Format, rep.Share*100, len(rep.Turns), rep.Replayed)
	scale := fmt.Sprintf("%.2f", rep.Scale)
	if !rep.ScaleMeasured {
		scale = "1 (NOT MEASURED: no call that carried a history had usage, so 1 is assumed)"
	}
	fmt.Fprintf(w, "History in budget tokens (bytes / 2.66). Real tokens per budget token: %s. Cache lifetime: %s.\n\n", scale, rep.TTL)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "#\tturn\tstart\tcalls\treal hist\toff hist\ton hist\ton cached\ton step\trecorded step\treal $\toff $\ton $\t")
	for i, t := range rep.Turns {
		start := ""
		if !t.Start.IsZero() {
			start = t.Start.Local().Format("01-02 15:04")
		}
		cached := ""
		if t.From != "" && t.WhatIfTurn.On.Tokens > 0 {
			cached = fmt.Sprintf("%.0f%%", 100*float64(t.WhatIfTurn.On.Prefix)/float64(t.WhatIfTurn.On.Tokens))
		}
		real := k(t.WhatIfTurn.Real.Tokens)
		if t.From == "" {
			real = "fresh"
		}
		if t.Problem != "" {
			real = "! " + real
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%.2f\t%.2f\t\n", i+1, t.Turn, start, t.CarryingCalls, t.All,
			real, k(t.WhatIfTurn.Off.Tokens), k(t.WhatIfTurn.On.Tokens), cached, stepText(t.WhatIfTurn.On.Step), stepText(t.Recorded),
			t.Real.USD, t.Off.USD, t.On.USD)
	}
	tw.Flush()
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "\ttokens sent\tcache read\tcache written\tuncached input\toutput\tcache share\tsteps\tUSD\t")
	for _, a := range []struct {
		name string
		t    Totals
	}{{"real", rep.Real}, {"budget off", rep.Off}, {"budget on", rep.On}} {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.1f%%\t%d\t%.2f\t\n", a.name, k(int(a.t.Sent)), k(int(a.t.CacheRead)), k(int(a.t.CacheWrite)),
			k(int(a.t.Input)), k(int(a.t.Output)), 100*a.t.CacheShare, a.t.Steps, a.t.USD)
	}
	tw.Flush()
	fmt.Fprintf(w, "\nRebuilt within 1%% of what the record carried: off arm %d of %d turns, on arm %d of %d.\n",
		rep.OffMatches, rep.Replayed, rep.OnMatches, rep.Replayed)
	fmt.Fprintln(w, "A session that ran without the budget must match the off arm at every turn, one that ran with it the on arm (at the same budget).")
	if rep.WriteAsInput > 0 {
		fmt.Fprintf(w, "%d calls of a vendor other than Anthropic wrote nothing to the cache: their arms bill new tokens as uncached input, as OpenAI does. For another vendor that is an assumption.\n", rep.WriteAsInput)
	}
	if rep.NoUsage > 0 {
		fmt.Fprintf(w, "%d calls have no recorded usage and are priced at nothing in every arm.\n", rep.NoUsage)
	}
	var probs []string
	for _, t := range rep.Turns {
		if t.Problem != "" {
			probs = append(probs, t.Turn+": "+t.Problem)
		}
	}
	if len(probs) > 0 {
		fmt.Fprintf(w, "Turns rebuilt from the record instead (marked !):\n  %s\n", strings.Join(probs, "\n  "))
	}
}

// k is n tokens, short: 438k, 1.2M.
func k(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", int(math.Round(float64(n)/1000)))
	}
	return fmt.Sprint(n)
}

// stepText is a step in short: what it shortened and how far.
func stepText(s *event.HistoryStep) string {
	if s == nil {
		return ""
	}
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{s.ResultsShortened, "R"}, {s.InputsShortened, "I"}, {s.TurnsSummarised, "S"}, {s.TurnsLeftOut, "L"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", p.what, p.n))
		}
	}
	if s.NewestShortened {
		parts = append(parts, "N")
	}
	return fmt.Sprintf("%s>%s %s", k(s.BeforeTokens), k(s.AfterTokens), strings.Join(parts, " "))
}
