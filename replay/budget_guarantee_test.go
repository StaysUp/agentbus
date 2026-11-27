package replay

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// The guarantee: every turn starts with a history within the budget, by the
// budget's own count, for every format, whatever the turns before it held.
// No turn is exempt: where keeping the newest whole and the budget conflict,
// the budget wins.

// heavy is s with each turn running k tool calls, each with a long input
// and a long result.
func heavy(s session, k int) session {
	switch s.name {
	case "claude":
		s.loop = func(n int, p string) string {
			var b strings.Builder
			b.WriteString(`{"model":"claude-x","messages":[{"role":"user","content":[` + reminder + `,{"type":"text","text":` + jsonQuote(p) + `}]},` +
				`{"role":"system","content":"` + sys2 + `"}`)
			for c := 0; c < k; c++ {
				id := fmt.Sprintf("toolu_%d_%d", n, c)
				fmt.Fprintf(&b, `,{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%s}}]}`, id, jsonQuote(bigIn))
				fmt.Fprintf(&b, `,{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":%s,"is_error":false}]}`, id, jsonQuote(bigOut))
			}
			b.WriteString(`],` + top + `,"stream":true}`)
			return b.String()
		}
	case "codex":
		s.loop = func(n int, p string) string {
			id := fmt.Sprintf("p%d", n)
			its := append(context(id, "read-only"), prompt(id, p))
			for c := 0; c < k; c++ {
				cid := fmt.Sprintf("call_%d_%d", n, c)
				its = append(its,
					fmt.Sprintf(`{"type":"function_call","id":"fc_%d_%d","call_id":%q,"name":"shell","arguments":%s}`, n, c, cid, jsonQuote(`{"command":`+jsonQuote(bigIn)+`}`)),
					fmt.Sprintf(`{"type":"function_call_output","call_id":%q,"output":%s}`, cid, jsonQuote(bigOut)))
			}
			return codexReq(codexThread(n), its...)
		}
	default:
		s.loop = func(n int, p string) string {
			msgs := []string{chatSysMsg(chatSys), chatUser(p)}
			for c := 0; c < k; c++ {
				cid := fmt.Sprintf("call_%d_%d", n, c)
				msgs = append(msgs,
					fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"bash","arguments":%s}}]}`, cid, jsonQuote(`{"command":`+jsonQuote(bigIn)+`}`)),
					fmt.Sprintf(`{"role":"tool","tool_call_id":%q,"content":%s}`, cid, jsonQuote(bigOut)))
			}
			return chatReq("glm", msgs...)
		}
	}
	return s
}

func formats() []session { return []session{claudeSession, codexSession, chatSession} }

func within(t *testing.T, s session, ps []played, budget int) {
	t.Helper()
	for i := 2; i < len(ps); i++ {
		if ps[i].fitted > budget {
			t.Fatalf("%s t%d started with %d tokens of history, over the budget of %d (events %+v)", s.name, i, ps[i].fitted, budget, ps[i].events)
		}
	}
}

// Round 2, finding 1: one tool-heavy turn, the newest, is shortened too. The
// session's first turn alone is far over the budget; the second turn starts
// within it, with a step that says the newest was shortened; the tool calls
// stay paired, and the person's words and the model's answer are kept.
func TestNewestTurnIsNotExempt(t *testing.T) {
	const budget = 12000
	for _, s := range formats() {
		h := heavy(s, 20) // ~20 x 3.6k tokens
		ps := h.run(t, 3, budget)
		within(t, h, ps, budget)
		var step *event.HistoryStep
		for _, e := range ps[2].events {
			if e.Kind == event.HistoryShortened {
				step = e.History
			}
		}
		if step == nil || !step.NewestShortened || step.AfterTokens > budget {
			t.Fatalf("%s: step %+v, want the newest turn shortened to within %d", s.name, step, budget)
		}
		all := string(joinArray(ps[2].first))
		if !strings.Contains(all, "remember the number 111") || !strings.Contains(all, "Done with turn 1.") {
			t.Fatalf("%s: the newest turn lost its words", s.name)
		}
	}
}

// Also when it is not the first: a light session, then one heavy turn.
func TestHeavyTurnLateInASession(t *testing.T) {
	const budget = 12000
	for _, s := range formats() {
		base := s
		h := heavy(s, 20)
		s.loop = func(n int, p string) string {
			if n == 6 {
				return h.loop(n, p)
			}
			return base.first(n, p)
		}
		s.paired = func(*testing.T, []json.RawMessage) {}
		within(t, s, s.run(t, 9, budget), budget)
	}
}

// Every format, many turns of mixed size: no turn ever starts over the
// budget. Round 1, finding 1 (the summary grew without end) and round 2,
// finding 1 (the newest turn was exempt) both broke this.
func TestEveryTurnStartsWithinTheBudget(t *testing.T) {
	const budget = 9000
	for _, s := range formats() {
		base, h3, h12 := s, heavy(s, 3), heavy(s, 12)
		s.loop = func(n int, p string) string {
			switch n % 7 {
			case 0:
				return h12.loop(n, p)
			case 2, 5:
				return h3.loop(n, p)
			}
			return base.first(n, p)
		}
		s.paired = func(*testing.T, []json.RawMessage) {}
		within(t, s, s.run(t, 40, budget), budget)
	}
}

// Round 2, finding 3: a long run of hex or base64 text is text, counted by
// its bytes; only an image block counts as an image.
func TestImagesAreKnownByTheirBlock(t *testing.T) {
	hex := strings.Repeat("0123456789abcdef", 16*1024) // 256 KiB
	res := `{"role":"user","content":[{"tool_use_id":"t","type":"tool_result","content":` + jsonQuote(hex) + `}]}`
	if got, want := approxTokens([]byte(res)), int(float64(len(res))/tokenBytes); got != want {
		t.Errorf("a 256 KiB hex result counts %d, want %d (its bytes)", got, want)
	}
	small := `{"role":"user","content":[{"tool_use_id":"t","type":"tool_result","content":` + jsonQuote(hex[:4096]) + `}]}`
	if approxTokens([]byte(small)) >= approxTokens([]byte(res)) {
		t.Error("a 4 KiB result counts as much as a 256 KiB one")
	}
	b64 := strings.Repeat("QUJD", 200000) // 800 KB of base64
	for _, img := range []string{
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + b64 + `"}]}`,
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}`,
	} {
		if got := approxTokens([]byte(img)); got < imageTokens || got > imageTokens+50 {
			t.Errorf("an image counts %d, want about %d", got, imageTokens)
		}
	}
	// A session of repeated hex results is shortened before it passes the
	// budget, in every format.
	const budget = 12000
	for _, s := range formats() {
		base := s.loop
		s.loop = func(n int, p string) string {
			return strings.ReplaceAll(base(n, p), "output line of kiwi\\n", "0123456789abcdef0123")
		}
		within(t, s, s.run(t, 12, budget), budget)
	}
}

// Round 2, finding 2: OpenCode compacts to its summary followed by the
// person's original prompt. That request still carries the prompt, but it is
// not the fresh layout, so the gateway judges it compacted, and the next
// turn is rebuilt from before the compaction, never from the summary. In a
// first turn and in a replayed one.
func TestOpenCodeSummaryBeforeThePromptIsACompaction(t *testing.T) {
	for _, replayed := range []bool{false, true} {
		src := newSource()
		r := NewChatCompletions(src)
		turn, p, x := "t1", chatP1, "x"
		if replayed {
			src.addTurn("t1", chatP1, "")
			src.through(t, r, "t1", "x0", chatReq("glm", chatSysMsg(chatSys), chatUser(chatP1)), func(b string) { src.addChatCall(t, "t1", "x0", b, chatStop, chatAsst) })
			src.setEnd("t1", store.TurnEnd{Closed: true, ExitCode: intp(0)})
			turn, p, x = "t2", chatP2, "y"
			src.addTurn(turn, p, "t1")
		} else {
			src.addTurn(turn, p, "")
		}
		src.through(t, r, turn, x+"1", chatReq("glm", chatSysMsg(chatSys), chatUser(p)), func(b string) { src.addChatCall(t, turn, x+"1", b, chatTools, chatCall) })
		summarised := chatReq("glm", chatSysMsg(chatSys), chatUser("What we did so far: OPENCODE SUMMARY."), chatUser(p))
		rw := src.through(t, r, turn, x+"2", summarised, func(b string) { src.addChatCall(t, turn, x+"2", b, chatStop, chatAns2) })
		if !judgedCompacted(rw) {
			t.Fatalf("replayed %v: the summary before the prompt was not judged a compaction: %+v", replayed, rw)
		}
		src.setEnd(turn, store.TurnEnd{Closed: true, ExitCode: intp(0)})
		src.addTurn("tn", chatP3, turn)
		next := r.Rewrite("s", "tn", []byte(chatReq("glm", chatSysMsg(chatSys), chatUser(chatP3))))
		if next.Body == nil {
			t.Fatalf("replayed %v: not spliced: %+v", replayed, next.Events)
		}
		all := strings.Join(messagesOf(t, next.Body), "\n")
		if strings.Contains(all, "OPENCODE SUMMARY") || !strings.Contains(all, "compacted its context") || !strings.Contains(all, "kiwi") {
			t.Errorf("replayed %v: the next turn carries the harness's summary or lost the turn:\n%s", replayed, all)
		}
	}
}
