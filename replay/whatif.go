package replay

// WhatIf measures the history budget on a session already recorded, offline
// and with no model call: for each turn, the history own replay would have put
// in front of it with the budget off and with it on, built by the same fit the
// gateway runs. tools/budgetcheck prices the two against what the session
// really paid.
//
// How a turn's history is rebuilt in each arm. Own replay gives turn t the
// history of the turn it continues (from), fitted: fit(full(from)), where
// full(from) is what from carried, then from's own part (its person's message,
// its tool loop, its last reply, AgentBus's note when it did not finish). The
// own part does not depend on what the turn carried, so it is read from the
// record once (history, from the person's message on), and each arm chains
// it onto its own carried history:
//
//	full_arm(t) = carried_arm(t) + own(t)
//	carried_arm(t) = fit_arm(full_arm(from))
//
// The off arm never fits; the on arm fits with the share given. A turn that
// carries no history (the session's first, one whose turn.started names no
// replay_from, one that got the earlier turns as text) starts both arms over.
// A turn whose history cannot be rebuilt from the record (missing calls, a
// layout the gateway does not know) is named in Problem, and the turns after
// it go on from what the record holds.
//
// Prefix is what the prompt cache can still read at the turn's first request:
// the history's leading messages that are, byte for byte, the leading
// messages of the previous turn's last main request. Without a step it is the
// whole history but the previous turn's last reply; a step breaks it at the
// first message it changed.

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// WhatIfTurn is one turn of a session, as WhatIf rebuilt it.
type WhatIfTurn struct {
	Turn string
	// From is the turn it continues (its replay_from), "" for a turn that
	// carries no history.
	From string
	// Model is the model its first main request asked for.
	Model string
	// Calls are the turn's model calls, oldest request first.
	Calls []WhatIfCall
	// Real is the history the turn's main requests carried, read from the
	// record; Off and On are the two arms. All three are counted by the
	// budget's own count (approxTokens) on the history's layout.
	Real, Off, On WhatIfHistory
	// Problem says why the arms could not be rebuilt for this turn. They
	// then carry what the record carried, and the next turns go on from it.
	Problem string
}

// WhatIfHistory is the history one turn carried in one arm.
type WhatIfHistory struct {
	Messages int
	Tokens   int
	// Prefix is how much of Tokens opens the previous turn's last main
	// request byte for byte: what a warm prompt cache reads.
	Prefix int
	// Step is the shortening fit made at this turn, nil when none.
	Step *event.HistoryStep
}

// WhatIfCall is one model call of a turn.
type WhatIfCall struct {
	Exchange string
	Time     time.Time
	// Carrier says the call is the turn's main thread and carried the
	// history: the turn's person's message is in it, behind the history.
	Carrier bool
	// RequestTokens counts the whole request; HistoryTokens the part of it
	// in front of the person's message (0 in a turn that carries none).
	RequestTokens int
	HistoryTokens int
}

// WhatIf rebuilds every turn of session, oldest first, with the budget off
// and with it at share of each model's window. It reads the record only and
// changes nothing in r.
func (r *Replayer) WhatIf(session string, share float64) ([]WhatIfTurn, error) {
	newest, err := r.src.SessionTurns(session)
	if err != nil {
		return nil, err
	}
	off := newReplayer(r.src, r.f)
	off.SetBudget(func() float64 { return 0 })
	on := newReplayer(r.src, r.f)
	on.SetBudget(func() float64 { return share })

	// What each arm holds after a turn: the history the next turn fits, and
	// the previous request's messages, for the cache prefix.
	type arm struct {
		full *history
		req  []json.RawMessage
	}
	offAfter, onAfter := map[string]arm{}, map[string]arm{}
	realReq := map[string][]json.RawMessage{}
	// whole is the newest first system message of a turn the record holds
	// whole (relayer).
	var whole json.RawMessage

	var out []WhatIfTurn
	for i := len(newest) - 1; i >= 0; i-- {
		t := newest[i]
		ti, err := r.turn(session, t)
		if err != nil {
			return nil, err
		}
		if !ti.found {
			continue
		}
		fresh := ti.from == "" || ti.transcript
		w := WhatIfTurn{Turn: t}
		if !fresh {
			w.From = ti.from
		}

		// The calls, oldest first, and where the person's message is in each.
		calls, err := r.src.TurnCalls(session, t)
		if err != nil {
			return nil, err
		}
		at := map[string]int{} // exchange: index of the person's message
		var last []json.RawMessage
		lastAt := -1
		for j := len(calls) - 1; j >= 0; j-- {
			c, ok, err := r.src.CallContext(calls[j].Exchange, true)
			if err != nil {
				return nil, err
			}
			wc := WhatIfCall{Exchange: calls[j].Exchange}
			if !ok {
				w.Calls = append(w.Calls, wc)
				continue
			}
			wc.Time = c.Time
			for _, p := range c.Pieces {
				if p.Kind == body.Message || p.Kind == body.InputItem {
					wc.RequestTokens += approxTokens(p.JSON)
				} else {
					wc.RequestTokens += int(float64(p.Size) / tokenBytes)
				}
			}
			msgs, _, _ := r.f.read(c)
			k := personAt(r.f, msgs, ti.prompt)
			if k >= 0 {
				wc.Carrier = true
				hm := r.laidOut(msgs)
				at[calls[j].Exchange] = k
				if fresh {
					at[calls[j].Exchange] = 0 // the call carried no history
				}
				if !fresh {
					for _, m := range hm[:k] {
						wc.HistoryTokens += approxTokens(m)
					}
				}
				if w.Model == "" {
					w.Model = body.Label(c.Request, "model")
					if !fresh {
						w.Real = WhatIfHistory{Messages: k, Tokens: wc.HistoryTokens,
							Prefix: prefixTokens(hm[:k], realReq[ti.from])}
					}
				}
				last, lastAt = hm, k
			}
			w.Calls = append(w.Calls, wc)
		}
		if last != nil {
			realReq[t] = last
		}

		// The turn's own part, from the record.
		full, err := r.history(session, t, false)
		var own []json.RawMessage
		var ownSrc []source
		ownAt := 0
		if err == nil {
			if !fresh {
				ownAt = personAt(r.f, full.messages, ti.prompt)
			}
			if ownAt < 0 {
				w.Problem = "the turn's message is not in its history"
			} else {
				own = full.messages[ownAt:]
				ownSrc = make([]source, len(own))
				copy(ownSrc, full.src[min(ownAt, len(full.src)):])
			}
		} else {
			w.Problem = "its history cannot be rebuilt: " + err.Error()
		}
		// How many of own were the last main request's own messages, so the
		// rest (the reply, AgentBus's note) is not in the cache's prefix.
		nReq := 0
		if last != nil {
			base := lastAt
			if fresh {
				base = 0
			}
			nReq = min(len(last)-base, len(own))
		}

		for _, a := range []struct {
			r     *Replayer
			after map[string]arm
			got   *WhatIfHistory
		}{{off, offAfter, &w.Off}, {on, onAfter, &w.On}} {
			var carried *history
			prev, have := a.after[ti.from]
			switch {
			case fresh:
				carried = &history{}
			case have && prev.full != nil:
				carried, a.got.Step = a.r.fit(session, ti.from, prev.full, w.Model)
			default:
				// The arm cannot go on: it carries what the record carried.
				if w.Problem == "" {
					w.Problem = "the turn it continues could not be rebuilt"
				}
				if err == nil && ownAt > 0 {
					carried = &history{messages: full.messages[:ownAt], model: full.model}
				} else {
					carried = &history{}
				}
			}
			a.got.Messages = len(carried.messages)
			for _, m := range carried.messages {
				a.got.Tokens += approxTokens(m)
			}
			if !fresh {
				a.got.Prefix = prefixTokens(carried.messages, prev.req)
			}
			if own == nil {
				a.after[t] = arm{}
				continue
			}
			sent, mine := carried.messages, own
			if rl, ok := r.f.(relayer); ok {
				sent, mine = rl.relay(carried.messages, own, whole)
			}
			next := &history{model: full.model, prior: full.prior, thread: full.thread, tools: full.tools}
			next.messages = append(append([]json.RawMessage{}, sent...), mine...)
			next.src = rebased(t, len(sent), ownSrc, at)
			req := append(append([]json.RawMessage{}, sent...), mine[:nReq]...)
			a.after[t] = arm{full: next, req: req}
		}
		if rl, ok := r.f.(relayer); ok && own != nil {
			if m, ok := rl.whole(own); ok {
				whole = m
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// personAt is the index of the person's message of the turn whose prompt is
// prompt in msgs, searched from the end, or -1.
func personAt(f format, msgs []json.RawMessage, prompt string) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if f.isPrompt(msgs[i], prompt) {
			return i
		}
	}
	return -1
}

// laidOut is a stored request's messages in the history's layout (asHistory:
// no cache breakpoints), so a request and a history compare byte for byte. A
// message that cannot be laid out stays as stored.
func (r *Replayer) laidOut(msgs []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		if h, err := r.f.asHistory(m); err == nil {
			out[i] = h
		} else {
			out[i] = m
		}
	}
	return out
}

// prefixTokens counts the leading messages of a that are byte for byte the
// leading messages of b.
func prefixTokens(a, b []json.RawMessage) int {
	n := 0
	for i := 0; i < len(a) && i < len(b) && bytes.Equal(a[i], b[i]); i++ {
		n += approxTokens(a[i])
	}
	return n
}

// rebased is where each message of carried + own would be in the record had
// turn t's requests carried carried: the history's messages in front of the
// turn's own, in the same call, so a later stub points where the gateway's
// would; carried is how many history messages the arm's request carries.
// A piece index moves by how much longer the arm's history is than
// the one the call really carried (at: per call, how many history messages
// it carried in front of the person's message).
func rebased(t string, carried int, ownSrc []source, at map[string]int) []source {
	out := make([]source, 0, carried+len(ownSrc))
	call, first := "", 0
	for j, s := range ownSrc {
		if s.call != "" && s.piece >= 0 {
			if k, ok := at[s.call]; ok {
				call, first = s.call, s.piece-(k+j)
				break
			}
		}
	}
	for i := 0; i < carried; i++ {
		if call == "" {
			out = append(out, source{})
			continue
		}
		out = append(out, source{turn: t, call: call, piece: first + i})
	}
	for _, s := range ownSrc {
		if k, ok := at[s.call]; ok && s.piece >= 0 && s.turn == t {
			s.piece += carried - k
		}
		out = append(out, s)
	}
	return out
}

// relayer is a format whose turn's own messages depend on the history in
// front of them, as build lays them out. WhatIf lays a turn's own part out
// again for each arm's history.
type relayer interface {
	// relay is carried and own as build would send them together.
	relay(carried, own []json.RawMessage, whole json.RawMessage) (sent, mine []json.RawMessage)
	// whole is own's first system message when the record holds it whole.
	whole(own []json.RawMessage) (json.RawMessage, bool)
}

// relay, for Claude Code, does what build does with the history in front:
//   - the fresh process's first system message is cut to its hook texts when
//     the history holds an environment (cutMessage), and goes whole when it
//     holds none (the turn that had it went into the summary). The record
//     holds whichever the real history called for; the other is made here,
//     the whole one from the newest the record holds (whole), which may be
//     from an earlier turn, so its hook texts are that turn's;
//   - a history that is AgentBus's summary alone goes into the person's
//     message, in front of its words.
func (claude) relay(carried, own []json.RawMessage, whole json.RawMessage) ([]json.RawMessage, []json.RawMessage) {
	sent := carried
	mine := append([]json.RawMessage{}, own...)
	if len(mine) > 0 {
		if _, rest, ok := (claude{}).summaryIn(mine[0]); ok && rest != nil {
			mine[0] = rest
		}
	}
	if len(mine) > 1 && role(mine[1]) == "system" {
		prior := systemText(carried)
		hasEnv := strings.Contains(prior, envMark[2:])
		isWhole := strings.Contains(systemText(mine[1:2]), envMark[2:])
		switch {
		case hasEnv && isWhole:
			if m, err := cutMessage(mine[1], prior); err == nil {
				mine[1] = m
			}
		case !hasEnv && !isWhole && whole != nil:
			mine[1] = whole
		}
	}
	if n := len(carried); n > 0 && len(mine) > 0 {
		if text, rest, ok := (claude{}).summaryIn(carried[n-1]); ok && rest == nil {
			sent = carried[:n-1]
			mine[0] = claude{}.withSummary(mine[0], text)[0]
		}
	}
	return sent, mine
}

func (claude) whole(own []json.RawMessage) (json.RawMessage, bool) {
	if len(own) > 1 && role(own[1]) == "system" && strings.Contains(systemText(own[1:2]), envMark[2:]) {
		return own[1], true
	}
	return nil, false
}
