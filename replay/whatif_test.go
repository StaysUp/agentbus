package replay

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
)

// WhatIf has a known answer: the same session played twice, once with the
// budget off and once with it on, gives two records. Read from either one,
// the off arm must be what the first record carried at every turn, and the
// on arm what the second carried, with the very steps the gateway recorded
// there. Each format.
func TestWhatIfKnownAnswer(t *testing.T) {
	const budget, turns = 12000, 16
	for _, s := range []session{claudeSession, codexSession, chatSession} {
		offPlayed, offRec := s.runSrc(t, turns, 100_000_000)
		onPlayed, onRec := s.runSrc(t, turns, budget)
		share := float64(budget) / float64(settings.ModelWindow(s.model))
		steps := 0
		for _, rec := range []struct {
			name string
			src  Source
			real []played
		}{{"off record", offRec, offPlayed}, {"on record", onRec, onPlayed}} {
			got, err := s.newR(rec.src).WhatIf("s", share)
			if err != nil {
				t.Fatalf("%s %s: %v", s.name, rec.name, err)
			}
			if len(got) != turns {
				t.Fatalf("%s %s: %d turns, want %d", s.name, rec.name, len(got), turns)
			}
			for i, w := range got {
				n := i + 1
				what := fmt.Sprintf("%s %s t%d", s.name, rec.name, n)
				if w.Turn != fmt.Sprintf("t%d", n) || w.Problem != "" {
					t.Fatalf("%s: %+v", what, w)
				}
				if n == 1 {
					if w.From != "" || w.Off.Tokens != 0 || w.On.Tokens != 0 || w.Real.Tokens != 0 {
						t.Fatalf("%s: the first turn carries a history: %+v", what, w)
					}
					continue
				}
				if w.From != fmt.Sprintf("t%d", n-1) {
					t.Fatalf("%s: from %q", what, w.From)
				}
				if w.Real.Tokens != rec.real[n].fitted {
					t.Errorf("%s: real %d, the record carried %d", what, w.Real.Tokens, rec.real[n].fitted)
				}
				if w.Off.Tokens != offPlayed[n].fitted {
					t.Errorf("%s: off %d, want %d", what, w.Off.Tokens, offPlayed[n].fitted)
				}
				if w.On.Tokens != onPlayed[n].fitted {
					t.Errorf("%s: on %d, want %d", what, w.On.Tokens, onPlayed[n].fitted)
				}
				var want *event.HistoryStep
				for _, e := range onPlayed[n].events {
					if e.Kind == event.HistoryShortened {
						want = e.History
					}
				}
				if !reflect.DeepEqual(w.On.Step, want) {
					t.Errorf("%s: step %+v, the gateway recorded %+v", what, w.On.Step, want)
				}
				if w.Off.Step != nil {
					t.Errorf("%s: a step with the budget off: %+v", what, w.Off.Step)
				}
				if want != nil {
					steps++
					if w.On.Prefix >= w.On.Tokens {
						t.Errorf("%s: a step kept the whole cache prefix (%d of %d)", what, w.On.Prefix, w.On.Tokens)
					}
				} else if n > 2 && (w.On.Prefix == 0 || w.On.Prefix >= w.On.Tokens) {
					// Without a step everything but the previous reply is
					// still the cached prefix.
					t.Errorf("%s: no step, prefix %d of %d", what, w.On.Prefix, w.On.Tokens)
				}
				if n > 2 && (w.Off.Prefix == 0 || w.Off.Prefix >= w.Off.Tokens) {
					t.Errorf("%s: off prefix %d of %d", what, w.Off.Prefix, w.Off.Tokens)
				}
				carriers := 0
				for _, c := range w.Calls {
					if c.Carrier {
						carriers++
						if c.HistoryTokens != w.Real.Tokens {
							t.Errorf("%s: call %s carried %d, the turn %d", what, c.Exchange, c.HistoryTokens, w.Real.Tokens)
						}
					}
				}
				if carriers != 1 {
					t.Errorf("%s: %d carrying calls, want 1", what, carriers)
				}
			}
		}
		if steps < 4 {
			t.Fatalf("%s: %d steps over both records, want the budget to fire", s.name, steps)
		}
	}
}
