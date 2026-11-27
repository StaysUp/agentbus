package replay

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// The history budget, for each of the three formats: a long session of turns
// that each run one tool with a long input and a long result. The history
// stays within the budget by itself, in rare steps, each step frozen so the
// next turns carry its bytes unchanged; the newest turn is always whole; and
// what was shortened can be found again in the record.

var (
	bigIn  = strings.Repeat("echo kiwi; ", 300)           // ~3.3 KB of tool input
	bigOut = strings.Repeat("output line of kiwi\n", 300) // ~6 KB of tool result
)

// session drives a format through turns.
type session struct {
	name   string
	model  string
	newR   func(Source) *Replayer
	first  func(n int, prompt string) string // a fresh process's first request
	loop   func(n int, prompt string) string // the same, after one tool call and its result
	add    func(t *testing.T, src *fakeSource, turn, x, req string, n int)
	key    string // the request's conversation field
	paired func(t *testing.T, msgs []json.RawMessage)
}

func turnPrompt(n int) string { return fmt.Sprintf("Turn %d: remember the number %d.", n, n*111) }

// run plays turns 1..n with the budget at budget tokens, and returns, per
// turn from 2 on, the messages of its first and its loop request as sent,
// and the events of its first request.
type played struct {
	first, loop []json.RawMessage
	events      []event.Event
	// fitted is the size of the history the turn started with, by the
	// budget's own count: what fit returned, the one thing it guarantees.
	fitted int
}

func (s session) run(t *testing.T, n, budget int) []played {
	ps, _ := s.runSrc(t, n, budget)
	return ps
}

func (s session) runSrc(t *testing.T, n, budget int) ([]played, *fakeSource) {
	t.Helper()
	src := newSource()
	r := s.newR(src)
	share := float64(budget) / float64(settings.ModelWindow(s.model))
	r.SetBudget(func() float64 { return share })
	out := make([]played, n+1)
	for i := 1; i <= n; i++ {
		id, p := fmt.Sprintf("t%d", i), turnPrompt(i)
		from := ""
		if i > 1 {
			plan, err := r.Base("s")
			if err != nil {
				t.Fatalf("%s %s: base: %v", s.name, id, err)
			}
			from = plan.Turn
		}
		src.addTurn(id, p, from)
		loop := s.loop(i, p)
		if i > 1 {
			rw := r.Rewrite("s", id, []byte(s.first(i, p)))
			if rw.Body == nil || rw.Refuse != nil {
				t.Fatalf("%s %s: first request not spliced: %+v", s.name, id, rw.Events)
			}
			out[i].first = mustElements(t, field(rw.Body, s.key))
			out[i].events = rw.Events
			r.mu.Lock()
			for _, m := range r.fits[[2]string{"s", id}].messages {
				out[i].fitted += approxTokens(m)
			}
			r.mu.Unlock()
			rw2 := r.Rewrite("s", id, []byte(loop))
			if rw2.Body == nil || rw2.Refuse != nil || len(rw2.Events) != 0 {
				t.Fatalf("%s %s: loop request: %+v", s.name, id, rw2.Events)
			}
			loop = string(rw2.Body)
			out[i].loop = mustElements(t, field(rw2.Body, s.key))
			s.paired(t, out[i].loop)
		}
		s.add(t, src, id, "x-"+id, loop, i)
		src.setEnd(id, store.TurnEnd{Closed: true, ExitCode: intp(0)})
	}
	return out, src
}

// check asserts the budget's rules on a played session.
func (s session) check(t *testing.T, ps []played, budget int, stub func(m json.RawMessage) bool) {
	t.Helper()
	steps, summaries := 0, []string{}
	for i := 2; i < len(ps); i++ {
		p := ps[i]
		hist := len(p.first) - 2 // the first request's history: all but its own prompt and system (approx.)
		var step *event.HistoryStep
		for _, e := range p.events {
			if e.Kind == event.HistoryShortened {
				step = e.History
			}
		}
		// The newest earlier turn is always whole: its long result is there.
		if !strings.Contains(string(joinArray(p.first)), fmt.Sprintf("remember the number %d.", (i-1)*111)) ||
			strings.Count(string(joinArray(p.first)), "output line of kiwi") < 300 {
			t.Fatalf("%s t%d: the newest earlier turn is not whole", s.name, i)
		}
		if step != nil {
			steps++
			if step.From != fmt.Sprintf("t%d", i-1) || step.AfterTokens >= step.BeforeTokens ||
				step.BeforeTokens <= step.BudgetTokens || step.BudgetTokens != budget {
				t.Fatalf("%s t%d: step %+v", s.name, i, step)
			}
			if step.AfterTokens > step.BudgetTokens {
				t.Fatalf("%s t%d: still over the budget after the step: %+v", s.name, i, step)
			}
			n := 0
			for _, m := range p.first {
				if stub(m) {
					n++
				}
			}
			if step.ResultsShortened+step.InputsShortened > 0 && n == 0 {
				t.Fatalf("%s t%d: step %+v but no stub in the request", s.name, i, step)
			}
		} else if i > 2 {
			// No step: the history is the previous turn's last request, as
			// sent (frozen), with its reply after it.
			prev := ps[i-1].loop
			s.prefix(t, fmt.Sprintf("t%d after t%d", i, i-1), prev, p.first, len(prev))
		}
		// Inside a turn every request carries the same history.
		s.prefix(t, fmt.Sprintf("t%d loop", i), p.first, p.loop, hist)
		for _, m := range p.first {
			if strings.Contains(string(m), "Earlier turns of this session") {
				summaries = append(summaries, string(m))
			}
		}
	}
	if steps < 2 || steps > (len(ps)-2)/2 {
		t.Fatalf("%s: %d steps over %d turns, want a few, not one a turn", s.name, steps, len(ps)-2)
	}
	if len(summaries) == 0 {
		t.Fatalf("%s: no turn was summarised", s.name)
	}
}

// prefix asserts later starts with earlier's first n messages, cache
// breakpoints aside.
func (s session) prefix(t *testing.T, what string, earlier, later []json.RawMessage, n int) {
	t.Helper()
	if len(earlier) < n || len(later) < n {
		t.Fatalf("%s %s: %d and %d messages, want %d", s.name, what, len(earlier), len(later), n)
	}
	for i := 0; i < n; i++ {
		a, b := string(earlier[i]), string(later[i])
		if s.name == "claude" {
			a, b = stripped(t, earlier[i]), stripped(t, later[i])
		}
		if a != b {
			t.Fatalf("%s %s: message %d differs\n%s\n%s", s.name, what, i, a, b)
		}
	}
}

var claudeSession = session{
	name: "claude", model: "claude-x", key: "messages", newR: New,
	first: func(_ int, p string) string { return t2First(p) },
	loop: func(n int, p string) string {
		return `{"model":"claude-x","messages":[` +
			`{"role":"user","content":[` + reminder + `,{"type":"text","text":` + jsonQuote(p) + `}]},` +
			`{"role":"system","content":"` + sys2 + `"},` +
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lH"},{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":%s}}]},`, n, jsonQuote(bigIn)) +
			fmt.Sprintf(`{"role":"user","content":[{"tool_use_id":"toolu_%d","type":"tool_result","content":%s,"is_error":false}]}`, n, jsonQuote(bigOut)) +
			`],` + top + `,"stream":true}`
	},
	add: func(t *testing.T, src *fakeSource, turn, x, req string, n int) {
		src.addCall(t, turn, x, req, fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"Done with turn %d."}]}`, n), "end_turn")
	},
	paired: func(t *testing.T, msgs []json.RawMessage) {
		t.Helper()
		for i, m := range msgs {
			ids, err := toolUseIDs(m)
			if err != nil || len(ids) == 0 {
				continue
			}
			for _, id := range ids {
				if i+1 >= len(msgs) || !strings.Contains(string(msgs[i+1]), `"tool_use_id":"`+id+`"`) {
					t.Fatalf("tool_use %s has no tool_result after it", id)
				}
			}
		}
		if role(msgs[0]) != "user" {
			t.Fatalf("the history starts with %s", role(msgs[0]))
		}
		for i := 1; i < len(msgs); i++ {
			if role(msgs[i]) == "user" && role(msgs[i-1]) == "user" {
				t.Fatalf("messages %d and %d are both the person's", i-1, i)
			}
		}
	},
}

func codexThread(n int) string { return fmt.Sprintf("019a0000-0000-7000-8000-0000000001%02d", n) }

var codexSession = session{
	name: "codex", model: "gpt-x", key: "input", newR: NewCodex,
	first: func(n int, p string) string {
		id := fmt.Sprintf("p%d", n)
		return codexReq(codexThread(n), append(context(id, "read-only"), prompt(id, p))...)
	},
	loop: func(n int, p string) string {
		id := fmt.Sprintf("p%d", n)
		call := fmt.Sprintf(`{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"shell","arguments":%s}`, n, n, jsonQuote(`{"command":`+jsonQuote(bigIn)+`}`))
		out := fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":%s}`, n, jsonQuote(bigOut))
		return codexReq(codexThread(n), append(context(id, "read-only"), prompt(id, p), call, out)...)
	},
	add: func(t *testing.T, src *fakeSource, turn, x, req string, n int) {
		src.addCodexCall(t, turn, x, req, completedCall,
			items(fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done with turn %d."}]}`, n, n)))
	},
	paired: func(t *testing.T, items []json.RawMessage) {
		t.Helper()
		calls, outs := map[string]bool{}, map[string]bool{}
		for _, it := range items {
			id := textOf(field(it, "call_id"))
			switch body.Label(it, "type") {
			case "function_call":
				calls[id] = true
				var args map[string]any
				if json.Unmarshal([]byte(textOf(field(it, "arguments"))), &args) != nil {
					t.Fatalf("call %s: arguments are not JSON: %s", id, it)
				}
			case "function_call_output":
				outs[id] = true
			}
		}
		for id := range calls {
			if !outs[id] {
				t.Fatalf("call %s has no output", id)
			}
		}
	},
}

var chatSession = session{
	name: "opencode", model: "glm", key: "messages", newR: NewChatCompletions,
	first: func(_ int, p string) string { return chatReq("glm", chatSysMsg(chatSys), chatUser(p)) },
	loop: func(n int, p string) string {
		call := fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"bash","arguments":%s}}]}`, n, jsonQuote(`{"command":`+jsonQuote(bigIn)+`}`))
		out := fmt.Sprintf(`{"role":"tool","tool_call_id":"call_%d","content":%s}`, n, jsonQuote(bigOut))
		return chatReq("glm", chatSysMsg(chatSys), chatUser(p), call, out)
	},
	add: func(t *testing.T, src *fakeSource, turn, x, req string, n int) {
		src.addChatCall(t, turn, x, req, chatStop, fmt.Sprintf(`{"role":"assistant","content":"Done with turn %d."}`, n))
	},
	paired: func(t *testing.T, msgs []json.RawMessage) {
		t.Helper()
		for i, m := range msgs {
			calls, _ := body.Elements(field(m, "tool_calls"))
			for _, c := range calls {
				id := textOf(field(c, "id"))
				var args map[string]any
				if json.Unmarshal([]byte(textOf(field(field(c, "function"), "arguments"))), &args) != nil {
					t.Fatalf("call %s: arguments are not JSON", id)
				}
				if i+1 >= len(msgs) || textOf(field(msgs[i+1], "tool_call_id")) != id {
					t.Fatalf("tool call %s has no result after it", id)
				}
			}
		}
	},
}

func isStub(m json.RawMessage) bool {
	return strings.Contains(string(m), "was shortened to keep the conversation within its budget") &&
		strings.Contains(string(m), "/calls/x-t")
}

func TestBudgetClaude(t *testing.T) {
	ps := claudeSession.run(t, 16, 12000)
	claudeSession.check(t, ps, 12000, isStub)
	// Once the first turn is in the summary, its environment message went
	// with it: the fresh process's own goes in whole.
	for i := 2; i < len(ps); i++ {
		if strings.Contains(string(ps[i].first[0]), "Earlier turns of this session") {
			sys := ""
			for _, m := range ps[i].first {
				if role(m) == "system" {
					sys += string(m)
				}
			}
			if !strings.Contains(sys, `# Environment`) {
				t.Fatalf("t%d: no environment after the summary", i)
			}
			return
		}
	}
	t.Fatal("no request starts with the summary")
}

func TestBudgetCodex(t *testing.T) {
	ps := codexSession.run(t, 16, 12000)
	codexSession.check(t, ps, 12000, isStub)
}

func TestBudgetOpenCode(t *testing.T) {
	ps := chatSession.run(t, 16, 12000)
	chatSession.check(t, ps, 12000, isStub)
}

// Under the budget nothing changes: the request is what it was before the
// budget existed, and no event is recorded.
func TestUnderBudgetUnchanged(t *testing.T) {
	for _, s := range []session{claudeSession, codexSession, chatSession} {
		ps := s.run(t, 4, 1000000)
		for i := 2; i < len(ps); i++ {
			if len(ps[i].events) != 0 {
				t.Errorf("%s t%d: events %+v under the budget", s.name, i, ps[i].events)
			}
			for _, m := range ps[i].first {
				if isStub(m) || strings.Contains(string(m), summaryMark) {
					t.Errorf("%s t%d: shortened under the budget", s.name, i)
				}
			}
		}
	}
}

// A turn's log, once written, is kept word for word in every later summary,
// until a step leaves it out with the oldest; a left-out turn is named in the
// left-out line. And a stub points at the piece of the call that holds the
// full text.
func TestSummaryKeptVerbatimAndStubsPoint(t *testing.T) {
	for _, s := range []session{claudeSession, codexSession, chatSession} {
		ps := s.run(t, 20, 9000)
		seen := map[string]string{} // a turn's log as first written
		grew := 0
		prevN := 0
		for i := 2; i < len(ps); i++ {
			text := ""
			for _, m := range ps[i].first {
				if strings.Contains(string(m), "Earlier turns of this session") {
					text = summaryOf(t, s, m)
				}
			}
			if text == "" {
				continue
			}
			left, logs := parseSummary(text)
			if len(logs) == 0 && left.n == 0 {
				t.Fatalf("%s t%d: an empty summary: %q", s.name, i, text)
			}
			for _, e := range logs {
				if was, ok := seen[e.turn]; ok && was != e.text {
					t.Fatalf("%s t%d: turn %s's log changed\nwas %q\nnow %q", s.name, i, e.turn, was, e.text)
				}
				seen[e.turn] = e.text
			}
			// Left out are the oldest turns: the first kept log comes after them.
			if left.n > 0 && left.oldest != "t1" {
				t.Fatalf("%s t%d: left out from %s, want from t1", s.name, i, left.oldest)
			}
			if n := left.n + len(logs); n > prevN {
				grew++
				prevN = n
			}
			if left.n == 0 && (!strings.Contains(text, "The person wrote:\n"+turnPrompt(1)) || !strings.Contains(text, "Done with turn 1.")) {
				t.Fatalf("%s t%d: the summary lost turn 1's words: %q", s.name, i, text)
			}
		}
		if grew < 2 {
			t.Fatalf("%s: the summary never took more turns", s.name)
		}
		for i := 2; i < len(ps); i++ {
			for _, m := range ps[i].first {
				if isStub(m) && !strings.Contains(string(m), "/pieces/") {
					t.Fatalf("%s t%d: a stub without a piece: %s", s.name, i, m)
				}
			}
		}
	}
}

// A session whose turns are text only (no tool output to stub) stays within
// the budget however long it runs: the summary takes the old turns, then
// leaves the oldest logs out, so it never grows past the budget (review
// t_9f7ff3c2866b0f64: it grew by a log a turn and passed the budget).
func TestTextOnlySessionStaysWithinBudget(t *testing.T) {
	words := strings.Repeat("a long answer about kiwi ", 40) // ~1 KB, under logBytes
	for _, s := range []session{claudeSession, codexSession, chatSession} {
		s.loop = s.first
		s.paired = func(*testing.T, []json.RawMessage) {}
		switch s.name {
		case "claude":
			s.add = func(t *testing.T, src *fakeSource, turn, x, req string, n int) {
				src.addCall(t, turn, x, req, fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"Done with turn %d. %s"}]}`, n, words), "end_turn")
			}
		case "codex":
			s.add = func(t *testing.T, src *fakeSource, turn, x, req string, n int) {
				src.addCodexCall(t, turn, x, req, completedCall,
					items(fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done with turn %d. %s"}]}`, n, n, words)))
			}
		default:
			s.add = func(t *testing.T, src *fakeSource, turn, x, req string, n int) {
				src.addChatCall(t, turn, x, req, chatStop, fmt.Sprintf(`{"role":"assistant","content":"Done with turn %d. %s"}`, n, words))
			}
		}
		const budget = 6000
		ps := s.run(t, 120, budget)
		left := 0
		for i := 2; i < len(ps); i++ {
			n := 0
			for _, m := range ps[i].first {
				n += approxTokens(m)
			}
			// The request is the history, the fresh prompt and (Claude Code)
			// the fresh environment: a few hundred tokens past the history.
			if n > budget+400 {
				t.Fatalf("%s t%d: the request is %d tokens, over the budget of %d", s.name, i, n, budget)
			}
			for _, e := range ps[i].events {
				if e.History != nil && e.History.TurnsLeftOut > 0 {
					left++
				}
			}
		}
		if left == 0 {
			t.Fatalf("%s: no step left a turn out of the summary", s.name)
		}
	}
}

// summaryOf is the summary text a message holds, in s's format.
func summaryOf(t *testing.T, s session, m json.RawMessage) string {
	t.Helper()
	var f format
	switch s.name {
	case "claude":
		f = claude{}
	case "codex":
		f = codex{}
	default:
		f = chatCompletions{}
	}
	text, _, ok := f.summaryIn(m)
	if !ok {
		t.Fatalf("%s: a message names the summary but holds none: %s", s.name, m)
	}
	return text
}

// A stub's pointer is the piece that holds the message in full, in the
// stored call it came from, for each format.
func TestStubPointsAtTheFullPiece(t *testing.T) {
	for _, s := range []session{claudeSession, codexSession, chatSession} {
		_, src := s.runSrc(t, 16, 12000)
		checked := 0
		for x, c := range src.calls {
			for _, p := range c.Pieces {
				text := string(p.JSON)
				for {
					i := strings.Index(text, "/calls/x-t")
					if i < 0 {
						break
					}
					text = text[i+len("/calls/"):]
					var call string
					var n int
					j := strings.Index(text, "/pieces/")
					call = text[:j]
					if _, err := fmt.Sscanf(text[j+len("/pieces/"):], "%d", &n); err != nil {
						t.Fatalf("%s %s: no piece number", s.name, x)
					}
					full := src.calls[call].Pieces[n]
					if full.Kind != body.Message && full.Kind != body.InputItem ||
						!strings.Contains(string(full.JSON), "output line of kiwi") && !strings.Contains(string(full.JSON), "echo kiwi;") ||
						strings.Contains(string(full.JSON), "was shortened") {
						t.Fatalf("%s: piece %d of %s does not hold the full text: %s", s.name, n, call, full.JSON)
					}
					checked++
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no stub pointed at a piece", s.name)
		}
	}
}
