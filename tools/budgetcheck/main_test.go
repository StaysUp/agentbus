package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/cost"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/replay"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// The pricing, worked by hand. One replayed turn of two calls on
// claude-opus-5-5 (per million: input $4, 1h write $8, read $0.20, output
// $20), scale 1. The record carried 100k of history, 95k of it cached at the
// turn's start; the on arm carries 40k after a step, none of it cached at the
// start.
//
// First call, recorded 10 in, 100,000 read, 1,000 written (1h), 500 out:
// $0.03804. On: 41,010 sent; the read is 100,000 - 95,000 + 0 = 5,000, 10
// in, 36,000 written: 40 + 1,000 + 288,000 + 10,000 = $0.29904.
//
// Second call, recorded 10 in, 101,000 read, 2,000 written, 300 out:
// $0.04224. On: 43,010 sent; the whole history is cached now, so the read is
// 101,000 - 100,000 + 40,000 = 41,000, 2,000 written: $0.03024.
//
// The off arm carries what the record carried, so it costs what was paid.
func TestPriceByHand(t *testing.T) {
	table := cost.Default()
	call := func(x string, in, read, write, out int, at time.Time) event.Event {
		e := event.Event{Kind: event.ModelResponded, Exchange: x, Vendor: "anthropic", Model: "claude-opus-5-5", Time: at,
			Usage: &event.Usage{InputTokens: in, CacheReadTokens: read, CacheWriteTokens: write, CacheWrite1hTokens: write,
				OutputTokens: out, ServiceTier: "standard"}}
		usd := table.Event(e).USD
		e.CostUSD = &usd
		return e
	}
	t0 := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	responded := map[string]event.Event{
		"x1": call("x1", 10, 100_000, 1_000, 500, t0),
		"x2": call("x2", 10, 101_000, 2_000, 300, t0.Add(time.Minute)),
	}
	real := replay.WhatIfHistory{Tokens: 100_000, Prefix: 95_000}
	w := replay.WhatIfTurn{Turn: "t2", From: "t1", Real: real, Off: real,
		On: replay.WhatIfHistory{Tokens: 40_000, Prefix: 0, Step: &event.HistoryStep{BeforeTokens: 100_000, AfterTokens: 40_000}},
		Calls: []replay.WhatIfCall{
			{Exchange: "x1", Time: t0, Carrier: true, RequestTokens: 101_010, HistoryTokens: 100_000},
			{Exchange: "x2", Time: t0.Add(time.Minute), Carrier: true, RequestTokens: 103_010, HistoryTokens: 100_000},
		}}
	rep := price("s", "anthropic", 0.4, []replay.WhatIfTurn{w}, responded, nil, table)
	if rep.Scale != 1 {
		t.Fatalf("scale %v", rep.Scale)
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: $%.6f, want $%.6f", what, got, want)
		}
	}
	near("real", rep.Real.USD, 0.03804+0.04224)
	near("off", rep.Off.USD, 0.03804+0.04224)
	near("on", rep.On.USD, 0.29904+0.03024)
	if got := rep.On.Usage; got.CacheRead != 46_000 || got.CacheWrite != 38_000 || got.Input != 20 || got.Output != 800 {
		t.Errorf("on usage %+v", got)
	}
	if rep.Off.Usage != rep.Real.Usage {
		t.Errorf("off %+v, real %+v", rep.Off.Usage, rep.Real.Usage)
	}
	if rep.On.Steps != 1 || rep.Replayed != 1 || rep.OffMatches != 1 || rep.OnMatches != 0 {
		t.Errorf("report %+v", rep)
	}
}

// A cold cache stays cold in every arm: when the recorded read did not
// cover the history (a long pause), the arm reads what the call read and
// writes its own history.
func TestPriceColdCache(t *testing.T) {
	table := cost.Default()
	e := event.Event{Kind: event.ModelResponded, Exchange: "x1", Vendor: "anthropic", Model: "claude-opus-5-5",
		Usage: &event.Usage{InputTokens: 10, CacheReadTokens: 20_000, CacheWriteTokens: 81_000, CacheWrite1hTokens: 81_000, ServiceTier: "standard"}}
	usd := table.Event(e).USD
	e.CostUSD = &usd
	got := armCall(e, usageOf(e), replay.WhatIfHistory{Tokens: 80_000, Prefix: 79_000},
		replay.WhatIfHistory{Tokens: 30_000}, true, false, 1, true, table)
	if got.CacheRead != 20_000 || got.CacheWrite != 31_000 || got.Input != 10 {
		t.Fatalf("cold arm %+v", got)
	}
}

// End to end, on a record made the way the gateway makes it: a Claude Code
// session whose requests went through own replay (Replayer.Rewrite) with
// the budget off, kept in a store file. Read back with the tool, the off arm
// is the record at every turn and costs what the record says was paid; the
// on arm steps and sends less. The same session recorded with the budget on
// is matched by the on arm instead, step for step.
func TestRecordEndToEnd(t *testing.T) {
	const turns, budget = 12, 12000
	share := float64(budget) / float64(settings.ModelWindow("claude-opus-5-5"))
	for _, recorded := range []float64{0, share} {
		path := filepath.Join(t.TempDir(), "events.db")
		paid := record(t, path, turns, recorded)
		var out bytes.Buffer
		if err := run([]string{"-data", filepath.Dir(path), "-session", "s", "-budget", fmt.Sprint(share), "-json", filepath.Join(t.TempDir(), "r.json")}, &out); err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := measure(st, "s", share, "")
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if rep.Replayed != turns-1 || rep.NoUsage != 0 {
			t.Fatalf("recorded at %v: %d replayed, %d without usage\n%s", recorded, rep.Replayed, rep.NoUsage, out.String())
		}
		if math.Abs(rep.Real.USD-paid) > 1e-9 {
			t.Errorf("recorded at %v: real $%.6f, the record paid $%.6f", recorded, rep.Real.USD, paid)
		}
		if rep.On.Steps < 2 || rep.On.Sent >= rep.Off.Sent {
			t.Errorf("recorded at %v: on arm %d steps, sent %d against %d\n%s", recorded, rep.On.Steps, rep.On.Sent, rep.Off.Sent, out.String())
		}
		if recorded == 0 {
			if rep.OffMatches != turns-1 || rep.Real.Steps != 0 || math.Abs(rep.Off.USD-paid) > 1e-9 {
				t.Errorf("budget off: off arm matched %d of %d, $%.6f against $%.6f\n%s", rep.OffMatches, turns-1, rep.Off.USD, paid, out.String())
			}
			continue
		}
		if rep.OnMatches != turns-1 || rep.Real.Steps != rep.On.Steps || math.Abs(rep.On.USD-paid) > 1e-9 {
			t.Errorf("budget on: on arm matched %d of %d, %d steps against %d recorded, $%.6f against $%.6f\n%s",
				rep.OnMatches, turns-1, rep.On.Steps, rep.Real.Steps, rep.On.USD, paid, out.String())
		}
		for _, tr := range rep.Turns {
			if (tr.Recorded == nil) != (tr.WhatIfTurn.On.Step == nil) ||
				tr.Recorded != nil && (tr.Recorded.BeforeTokens != tr.WhatIfTurn.On.Step.BeforeTokens || tr.Recorded.AfterTokens != tr.WhatIfTurn.On.Step.AfterTokens) {
				t.Errorf("%s: recorded step %+v, on arm %+v", tr.Turn, tr.Recorded, tr.WhatIfTurn.On.Step)
			}
		}
		if !strings.Contains(out.String(), "budget on") {
			t.Errorf("no totals:\n%s", out.String())
		}
	}
}

// record writes a Claude Code session of n turns to a store at path, each
// turn one tool call with a long input and result, its requests spliced by
// own replay with the budget at share (0: off), and returns what it paid.
// Usage follows the cache as Anthropic reports it: the turn's first request
// reads the previous request's prefix and writes the rest; its second reads
// the first whole.
func record(t *testing.T, path string, n int, share float64) float64 {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := replay.New(st)
	r.SetBudget(func() float64 { return share })
	table := cost.Default()
	t0 := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	seq, paid := 0, 0.0
	add := func(e event.Event) {
		t.Helper()
		seq++
		e.ID, e.Session = fmt.Sprintf("e%d", seq), "s"
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(b []byte) int { return int(float64(len(b)) / 2.66) }
	var prev []byte // the previous request as sent
	from := ""
	for i := 1; i <= n; i++ {
		turn, p := fmt.Sprintf("t%d", i), fmt.Sprintf("Turn %d: remember the number %d.", i, i*111)
		at := t0.Add(time.Duration(i) * 2 * time.Minute)
		add(event.Event{Kind: event.TurnStarted, Turn: turn, Text: p, ReplayFrom: from, Harness: "claude", Time: at})
		for j, req := range []string{firstReq(p), loopReq(i, p)} {
			x := fmt.Sprintf("x-%s-%d", turn, j)
			b := []byte(req)
			if from != "" {
				rw := r.Rewrite("s", turn, b)
				if rw.Body == nil {
					t.Fatalf("%s: not spliced: %+v", turn, rw)
				}
				b = rw.Body
				for _, e := range rw.Events {
					e.Turn, e.Exchange, e.Time = turn, x, at
					add(e)
				}
			}
			split, err := anthropic.Adapter{}.Split(b)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.KeepCall(body.Call{Exchange: x, Session: "s", Vendor: "anthropic", Time: at, Size: len(b), Request: split}); err != nil {
				t.Fatal(err)
			}
			add(event.Event{Kind: event.ModelRequested, Turn: turn, Exchange: x, Vendor: "anthropic", Model: "claude-opus-5-5", Time: at})
			stop, reply := "tool_use", fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":"ls"}}]}`, i)
			if j == 1 {
				stop, reply = "end_turn", fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"Done with turn %d."}]}`, i)
			}
			if err := st.KeepReply(body.Reply{Exchange: x, Status: 200, Stop: stop, Time: at, Message: json.RawMessage(reply)}); err != nil {
				t.Fatal(err)
			}
			read := tok(commonPrefix(prev, b))
			u := &event.Usage{InputTokens: 3, CacheReadTokens: read, CacheWriteTokens: max(tok(b)-read-3, 0),
				OutputTokens: 200, ServiceTier: "standard"}
			u.CacheWrite1hTokens = u.CacheWriteTokens
			e := event.Event{Kind: event.ModelResponded, Turn: turn, Exchange: x, Vendor: "anthropic", Model: "claude-opus-5-5",
				Status: 200, StopReason: stop, Usage: u, Time: at}
			usd := table.Event(e).USD
			e.CostUSD = &usd
			paid += usd
			add(e)
			prev = b
		}
		code := 0
		add(event.Event{Kind: event.RunEnded, Turn: turn, ExitCode: &code, Time: at})
		from = turn
	}
	return paid
}

// commonPrefix is the bytes a and b start with, cut back to the last
// message boundary, as a prompt cache reads by blocks.
func commonPrefix(a, b []byte) []byte {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	if i := bytes.LastIndex(b[:n], []byte(`},{"role"`)); i >= 0 {
		return b[:i]
	}
	return nil
}

var (
	bigIn  = strings.Repeat("echo kiwi; ", 300)
	bigOut = strings.Repeat("output line of kiwi\n", 300)
)

const (
	sysText = `SessionStart hook additional context: hello\n\n# Environment\nenv\n\nUserPromptSubmit hook additional context: ups\n\nToday's date is 2026-11-27.`
	topRest = `"system":[{"type":"text","text":"You are Claude Code."}],"tools":[{"name":"Bash","input_schema":{}}],"stream":true`
)

func q(s string) string { b, _ := json.Marshal(s); return string(b) }

// firstReq is a fresh Claude Code process's first request of a turn.
func firstReq(p string) string {
	return `{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"text","text":` + q(p) + `}]},` +
		`{"role":"system","content":"` + sysText + `"}],` + topRest + `}`
}

// loopReq is the same after one tool call and its result.
func loopReq(n int, p string) string {
	return `{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"text","text":` + q(p) + `}]},` +
		`{"role":"system","content":"` + sysText + `"},` +
		fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":%s}}]},`, n, q(bigIn)) +
		fmt.Sprintf(`{"role":"user","content":[{"tool_use_id":"toolu_%d","type":"tool_result","content":%s,"is_error":false}]}`, n, q(bigOut)) +
		`],` + topRest + `}`
}

// A session with no usage on any carrying call says its scale was not
// measured; a non-Anthropic call that wrote nothing is counted as billed as
// input, and both show in the output.
func TestAssumptionsAreMarked(t *testing.T) {
	table := cost.Default()
	w := replay.WhatIfTurn{Turn: "t2", From: "t1", Real: replay.WhatIfHistory{Tokens: 1000}, Off: replay.WhatIfHistory{Tokens: 1000},
		On: replay.WhatIfHistory{Tokens: 500}, Calls: []replay.WhatIfCall{{Exchange: "x1", Carrier: true, RequestTokens: 1100}}}
	rep := price("s", "openai", 0.4, []replay.WhatIfTurn{w}, map[string]event.Event{}, nil, table)
	var out bytes.Buffer
	render(&out, rep)
	if rep.ScaleMeasured || rep.Scale != 1 || !strings.Contains(out.String(), "NOT MEASURED") {
		t.Fatalf("scale %v measured %v\n%s", rep.Scale, rep.ScaleMeasured, out.String())
	}
	e := event.Event{Kind: event.ModelResponded, Exchange: "x1", Vendor: "openai", Model: "gpt-5.5",
		Usage: &event.Usage{InputTokens: 300, CacheReadTokens: 800, OutputTokens: 10, ServiceTier: "default"}}
	rep = price("s", "openai", 0.4, []replay.WhatIfTurn{w}, map[string]event.Event{"x1": e}, nil, table)
	out.Reset()
	render(&out, rep)
	if !rep.ScaleMeasured || rep.WriteAsInput != 1 || !strings.Contains(out.String(), "as OpenAI does") {
		t.Fatalf("measured %v, write-as-input %d\n%s", rep.ScaleMeasured, rep.WriteAsInput, out.String())
	}
	if rep.On.CacheWrite != 0 {
		t.Fatalf("on arm wrote %d to the cache", rep.On.CacheWrite)
	}
}
