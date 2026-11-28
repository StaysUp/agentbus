package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
)

// The harness's standing context goes through every step of the history
// budget byte for byte, in every format: Claude Code's system-reminder blocks
// in the session's first message, codex's context items, OpenCode's system
// message. (2026-11-28: on the coordinator session 2bcc782f239aa890 a step
// summarised its first message, and with it ~/.claude/CLAUDE.md, the memory
// index and AGENTS.md; it ran a day without its rules.)

// The shape of Claude Code's first request, from the coordinator session's
// first call (x-a7d7f0c9-1516, Claude Code 2.1.3xx, 2026-11-26): three text
// blocks, each a "<system-reminder>", in front of the person's words, then a
// role-system message holding the environment. The texts are cut; the
// openings and the trailing newline of the third are as recorded.
var recordedReminders = []string{
	`{"type":"text","text":` + jsonQuote("<system-reminder>\nCodebase and user instructions are shown below. Be sure to adhere to these instructions. IMPORTANT: These instructions OVERRIDE any default behavior and you MUST follow them exactly as written.\n\nContents of /Users/x/.claude/CLAUDE.md (user's private global instructions for all projects):\n\n# the person's rules\n\nContents of /Users/x/Sites/agentbus/CLAUDE.md (project instructions, checked into the codebase):\n\n# AGENTS.md: working in this repository\n\nContents of /Users/x/.agents/memory/MEMORY.md (user's auto-memory, persists across conversations):\n\n# Memory map\n</system-reminder>") + `}`,
	`{"type":"text","text":` + jsonQuote("<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# userEmail\nThe user's email address is a@example.com.\n</system-reminder>") + `}`,
	`{"type":"text","text":` + jsonQuote("<system-reminder>\nAttribution for git commits and pull requests you create from here on:\n- none\n</system-reminder>\n") + `}`,
}

// freshReminder is what each later fresh process sends: it must never be
// what the history carries once turn 1's are there.
const freshReminder = `{"type":"text","text":"<system-reminder>\nA later process's copy.\n</system-reminder>"}`

func claudeStandingSession(firstCtx []string) session {
	s := claudeSession
	lead := func(n int) string {
		if n == 1 {
			return strings.Join(firstCtx, ",")
		}
		return freshReminder
	}
	open := func(n int, p string) string {
		blocks := []string{`{"type":"text","text":` + jsonQuote(p) + `}`}
		if l := lead(n); l != "" {
			blocks = append([]string{l}, blocks...)
		}
		return `{"role":"user","content":[` + strings.Join(blocks, ",") + `]}`
	}
	s.first = func(n int, p string) string {
		return `{"model":"claude-x","messages":[` + open(n, p) + `,` +
			`{"role":"system","content":[{"type":"text","text":"` + sys2 + `","cache_control":` + bp1h + `}]}` +
			`],` + top + `,"stream":true}`
	}
	s.loop = func(n int, p string) string {
		return `{"model":"claude-x","messages":[` + open(n, p) + `,` +
			`{"role":"system","content":"` + sys2 + `"},` +
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":%s}}]},`, n, jsonQuote(bigIn)) +
			fmt.Sprintf(`{"role":"user","content":[{"tool_use_id":"toolu_%d","type":"tool_result","content":%s,"is_error":false}]}`, n, jsonQuote(bigOut)) +
			`],` + top + `,"stream":true}`
	}
	return s
}

// stepsIn counts the history steps of a played session, and fails when there
// are fewer than want.
func stepsIn(t *testing.T, name string, ps []played, want int) []*event.HistoryStep {
	t.Helper()
	var steps []*event.HistoryStep
	for _, p := range ps {
		for _, e := range p.events {
			if e.Kind == event.HistoryShortened {
				steps = append(steps, e.History)
			}
		}
	}
	if len(steps) < want {
		t.Fatalf("%s: %d steps, want at least %d", name, len(steps), want)
	}
	return steps
}

// firstBlocks is the first n content blocks of a Claude message.
func firstBlocks(t *testing.T, m json.RawMessage, n int) []json.RawMessage {
	t.Helper()
	els := mustElements(t, field(m, "content"))
	if len(els) < n {
		t.Fatalf("the first message has %d blocks, want at least %d: %s", len(els), n, m)
	}
	return els[:n]
}

func TestClaudeContextKeptThroughSteps(t *testing.T) {
	s := claudeStandingSession(recordedReminders)
	ps := s.run(t, 16, 12000)
	s.check(t, ps, 12000, isStub)
	steps := stepsIn(t, s.name, ps, 2)
	summarised := false
	for i := 2; i < len(ps); i++ {
		for _, req := range [][]json.RawMessage{ps[i].first, ps[i].loop} {
			got := firstBlocks(t, req[0], len(recordedReminders))
			for k, b := range got {
				if string(b) != recordedReminders[k] {
					t.Fatalf("t%d: block %d of the first message is not turn 1's, byte for byte:\n got %s\nwant %s", i, k, b, recordedReminders[k])
				}
			}
			whole := string(joinArray(req))
			if strings.Contains(whole, "A later process's copy.") {
				t.Fatalf("t%d: a later process's reminder went in beside turn 1's", i)
			}
			if n := strings.Count(whole, "Contents of /Users/x/.claude/CLAUDE.md"); n != 1 {
				t.Fatalf("t%d: CLAUDE.md is in the request %d times, want 1", i, n)
			}
		}
		if _, _, ok := (claude{}).summaryIn(ps[i].first[0]); ok {
			summarised = true
		}
	}
	if !summarised {
		t.Fatal("no request opened with the summary: the steps never took turn 1")
	}
	for _, st := range steps {
		if want := approxTokens(joinArray(mustRaw(recordedReminders))); st.HeadTokens != want {
			t.Fatalf("step %+v: head_tokens %d, want the reminders' %d", st, st.HeadTokens, want)
		}
	}
}

func mustRaw(ss []string) []json.RawMessage {
	out := make([]json.RawMessage, len(ss))
	for i, s := range ss {
		out[i] = json.RawMessage(s)
	}
	return out
}

// A session whose history lost its context (shortened before this was kept,
// or a first turn that never reached the model) gets the fresh process's,
// in the history's first message, and from then on keeps it byte for byte.
func TestClaudeContextRestoredWhenTheHistoryHasNone(t *testing.T) {
	s := claudeStandingSession(nil)
	ps := s.run(t, 16, 12000)
	stepsIn(t, s.name, ps, 2)
	for i := 2; i < len(ps); i++ {
		for _, req := range [][]json.RawMessage{ps[i].first, ps[i].loop} {
			if got := firstBlocks(t, req[0], 1)[0]; string(got) != freshReminder {
				t.Fatalf("t%d: the first message opens with %s, want the fresh process's context", i, got)
			}
			if n := strings.Count(string(joinArray(req)), "A later process's copy."); n != 1 {
				t.Fatalf("t%d: the context is in the request %d times, want 1", i, n)
			}
		}
	}
}

// codex: the context items before turn 1's prompt (the head) open every
// request byte for byte, through every step.
func TestCodexContextKeptThroughSteps(t *testing.T) {
	ps := codexSession.run(t, 16, 12000)
	stepsIn(t, codexSession.name, ps, 2)
	head := headOf(t, codexSession)
	for i := 2; i < len(ps); i++ {
		for _, req := range [][]json.RawMessage{ps[i].first, ps[i].loop} {
			assertOpens(t, fmt.Sprintf("codex t%d", i), req, head)
		}
	}
}

// OpenCode: the system message opens every request byte for byte, once.
func TestOpenCodeContextKeptThroughSteps(t *testing.T) {
	ps := chatSession.run(t, 16, 12000)
	stepsIn(t, chatSession.name, ps, 2)
	head := headOf(t, chatSession)
	for i := 2; i < len(ps); i++ {
		for _, req := range [][]json.RawMessage{ps[i].first, ps[i].loop} {
			assertOpens(t, fmt.Sprintf("opencode t%d", i), req, head)
			if n := strings.Count(string(joinArray(req)), chatSys); n != 1 {
				t.Fatalf("opencode t%d: the system message is in the request %d times", i, n)
			}
		}
	}
}

// headOf is the messages in front of the person's message of a stored call.
// It is read from turn 1's request as the session sent it.
func headOf(t *testing.T, s session) []json.RawMessage {
	t.Helper()
	msgs := mustElements(t, field(json.RawMessage(s.loop(1, turnPrompt(1))), s.key))
	f := s.newR(newSource()).f
	for i, m := range msgs {
		if f.isPrompt(m, turnPrompt(1)) {
			if i == 0 {
				t.Fatalf("%s: turn 1 has no head", s.name)
			}
			return msgs[:i]
		}
	}
	t.Fatalf("%s: turn 1 has no prompt", s.name)
	return nil
}

func assertOpens(t *testing.T, what string, req, head []json.RawMessage) {
	t.Helper()
	if len(req) < len(head) {
		t.Fatalf("%s: %d messages, want the %d of the head first", what, len(req), len(head))
	}
	for k, m := range head {
		if !bytes.Equal(req[k], m) {
			t.Fatalf("%s: message %d is not the head's, byte for byte:\n got %s\nwant %s", what, k, req[k], m)
		}
	}
}

// A standing context over the budget is never cut: the step says so.
func TestContextOverTheBudgetIsKeptAndRecorded(t *testing.T) {
	big := strings.Repeat("Rule: keep data. ", 2500) // ~42 KB, ~16k tokens
	s := chatSession
	s.first = func(_ int, p string) string { return chatReq("glm", chatSysMsg(big), chatUser(p)) }
	loop := s.loop
	s.loop = func(n int, p string) string {
		return strings.Replace(loop(n, p), chatSysMsg(chatSys), chatSysMsg(big), 1)
	}
	ps := s.run(t, 4, 12000)
	steps := stepsIn(t, s.name, ps, 1)
	for _, st := range steps {
		if st.HeadTokens <= st.BudgetTokens || st.AfterTokens < st.HeadTokens {
			t.Fatalf("step %+v: want head_tokens over the budget and in after_tokens", st)
		}
	}
	for i := 2; i < len(ps); i++ {
		if string(ps[i].first[0]) != chatSysMsg(big) {
			t.Fatalf("t%d: the system message is not whole", i)
		}
	}
}

// When a step's summary takes every turn, it goes into the new prompt's
// message (build's merge): turn 1's context still opens it, byte for byte,
// and the next steps keep it.
func TestClaudeContextKeptWhenTheSummaryTakesEveryTurn(t *testing.T) {
	const budget = 12000
	s := heavy(claudeSession, 20)
	loop := s.loop
	s.loop = func(n int, p string) string {
		ctx := freshReminder
		if n == 1 {
			ctx = strings.Join(recordedReminders, ",")
		}
		return strings.Replace(loop(n, p), reminder, ctx, 1)
	}
	s.first = claudeStandingSession(recordedReminders).first
	ps := s.run(t, 5, budget)
	within(t, s, ps, budget)
	alone := 0
	for i := 2; i < len(ps); i++ {
		for k, b := range firstBlocks(t, ps[i].first[0], len(recordedReminders)) {
			if string(b) != recordedReminders[k] {
				t.Fatalf("t%d: block %d of the first message is not turn 1's:\n got %s", i, k, b)
			}
		}
		if strings.Contains(string(joinArray(ps[i].first)), "A later process's copy.") {
			t.Fatalf("t%d: a later process's reminder went in beside turn 1's", i)
		}
		if role(ps[i].first[1]) == "system" { // the summary alone, merged into the prompt's message
			alone++
		}
	}
	if alone == 0 {
		t.Fatal("no step's summary took every turn")
	}
}

// WhatIf rebuilds what the gateway sent for the Claude Code shapes that carry
// the context through a step (budgetcheck's on arm), and says the context is
// in every arm: per turn, the on arm is the record's history, step for step.
func TestWhatIfKeepsTheContext(t *testing.T) {
	const budget = 12000
	heavyCtx := heavy(claudeSession, 20)
	loop := heavyCtx.loop
	heavyCtx.loop = func(n int, p string) string {
		ctx := freshReminder
		if n == 1 {
			ctx = strings.Join(recordedReminders, ",")
		}
		return strings.Replace(loop(n, p), reminder, ctx, 1)
	}
	heavyCtx.first = claudeStandingSession(recordedReminders).first
	for name, c := range map[string]struct {
		s     session
		turns int
	}{"steps": {claudeStandingSession(recordedReminders), 16}, "summary alone": {heavyCtx, 5}} {
		ps, src := c.s.runSrc(t, c.turns, budget)
		share := float64(budget) / float64(settings.ModelWindow(c.s.model))
		got, err := New(src).WhatIf("s", share)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		steps := 0
		for i, w := range got[1:] {
			n := i + 2
			if w.Problem != "" || w.On.Tokens != ps[n].fitted {
				t.Fatalf("%s t%d: on %d, the record carried %d (%s)", name, n, w.On.Tokens, ps[n].fitted, w.Problem)
			}
			var want *event.HistoryStep
			for _, e := range ps[n].events {
				if e.Kind == event.HistoryShortened {
					want = e.History
				}
			}
			if !reflect.DeepEqual(w.On.Step, want) {
				t.Fatalf("%s t%d: step %+v, the gateway recorded %+v", name, n, w.On.Step, want)
			}
			if want != nil {
				steps++
			}
			if w.ContextTokens == 0 || !w.Real.ContextKept || !w.Off.ContextKept || !w.On.ContextKept {
				t.Fatalf("%s t%d: context %d tokens, kept real %v off %v on %v", name, n, w.ContextTokens, w.Real.ContextKept, w.Off.ContextKept, w.On.ContextKept)
			}
		}
		if steps < 2 {
			t.Fatalf("%s: %d steps", name, steps)
		}
	}
}

// Review t_4fb370a794c27cf3: context put back into a history that lacked it
// is counted by the budget: it is put in before the fit, so the history sent
// is what fit counted (Codex reproduced 12,315 tokens sent against a 12,000
// budget, with no step, when it went in after).
func TestRestoredContextIsWithinTheBudget(t *testing.T) {
	const budget = 12000
	big := `{"type":"text","text":` + jsonQuote("<system-reminder>\nBIG-CONTEXT \n"+strings.Repeat("Rule: keep data. ", 400)+"\n</system-reminder>") + `}` // ~2.5k tokens
	s := claudeStandingSession(nil)
	for _, f := range []*func(int, string) string{&s.first, &s.loop} {
		g := *f
		*f = func(n int, p string) string { return strings.Replace(g(n, p), freshReminder, big, 1) }
	}
	ps := s.run(t, 16, budget)
	within(t, s, ps, budget)
	stepsIn(t, s.name, ps, 2)
	for i := 2; i < len(ps); i++ {
		// Everything of the first request but the system message and the
		// person's words is the fitted history.
		sent := 0
		for _, m := range ps[i].first {
			if role(m) != "system" {
				sent += approxTokens(m)
			}
		}
		sent -= approxTokens(json.RawMessage(`{"type":"text","text":` + jsonQuote(turnPrompt(i)) + `}`))
		// What build adds besides the history is the person's words; the
		// rest is what fit counted, give or take a message's wrapping.
		if d := sent - ps[i].fitted; d > 50 || sent > budget {
			t.Fatalf("t%d: %d tokens of history sent, fit counted %d (budget %d)", i, sent, ps[i].fitted, budget)
		}
		if n := strings.Count(string(joinArray(ps[i].first)), "BIG-CONTEXT"); n != 1 {
			t.Fatalf("t%d: the context is in the request %d times, want 1", i, n)
		}
	}
}
