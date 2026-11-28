package replay

// The history budget: AgentBus keeps the history it puts in front of a turn
// within a share of the model's window (settings.Compaction.Budget, 0.4 by
// default), by itself, so a long session never grows to the point where its
// harness compacts. It never depends on how a harness compacts.
//
// The rule: what fit returns is within the budget, counted by one count
// (approxTokens) on the bytes it returns, and no turn is exempt. It is a
// guarantee checked on the result, not a goal the steps aim at (Codex
// reviews t_9f7ff3c2866b0f64 and t_9d14aa5c291e0a5f: a summary that only
// grew, a newest turn left out of the bound, and images guessed from text
// each broke the goal version). Where keeping the newest turn whole and the
// budget conflict, the budget wins.
//
// At the start of each turn (the turn's first main request; every later
// request of the turn reuses the same bytes):
//
//   - Under the budget: the history goes in as it is.
//   - Over it: shortened to half the budget, so steps are rare and the
//     prompt cache is rewritten once per step, not every turn. Older turns
//     first, oldest first, the newest recentTurns whole while that is
//     enough: long tool results, then long tool-call inputs, become stubs
//     that say what they were and where the agent fetches them back; then
//     the oldest turns become AgentBus's own summary (the person's words
//     and the model's last words, cut past 2,000 bytes, AgentBus's notes, a
//     line per tool call; a turn's log, once written, stays word for word);
//     then the summary's oldest logs are left out for one line naming how
//     many turns and their range.
//   - Still over the budget: the newest turn too, to within the budget (not
//     half): its stubs, then into the summary, then logs left out down to
//     none. history.shortened says so (newest_shortened).
//
// Only the head, the harness's own context before the first turn (codex's
// context items, OpenCode's system message), is never shortened: every
// fresh request carries it anyway.
//
// A compaction is not judged here: one test (the format's fresh) decides it,
// applied by the gateway as it forwards a request, and read back by the
// history (Replayer.compacted).
//
// Each step is frozen by the record itself: the gateway stores the request it
// sent, so the next turn's history is built from the shortened one, and a
// stub or a summary is never written twice or differently. The record keeps
// every byte of every call; the stubs point at it. history.shortened records
// each step.
//
// No model is called: the summary is a work log AgentBus writes from the
// record, so a step costs no quota and gives the same bytes every time.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
)

const (
	// recentTurns is how many of the newest turns go in whole while older
	// turns can still be shortened.
	recentTurns = 3
	// stubOver is the size in bytes past which a tool result or a tool
	// call's input may become a stub. A short result (an exit code, a test
	// failure) is the point of its call and stays.
	stubOver = 2048
	// headBytes is how much of a stubbed text the stub quotes.
	headBytes = 300
	// callHead is how much of a tool call's input a summary line quotes.
	callHead = 120
	// summaryCalls is how many tool calls a summarised turn lists by name.
	summaryCalls = 20
	// logBytes bounds the person's words and the model's last words in a
	// turn's log; noteBytes bounds each of AgentBus's notes in it. So a turn's
	// log is a few KiB at most, whatever the turn held.
	logBytes  = 2000
	noteBytes = 400
	// imageTokens stands for one image: its base64 bytes say nothing of its
	// tokens (~1.5k for a screenshot, the coordinator's measure, 2026-11-26).
	imageTokens = 1600
	// tokenBytes is bytes per token, as store.Piece's approx_tokens counts.
	tokenBytes = 2.66
)

// summaryMark starts AgentBus's summary of earlier turns, so a later step
// finds it and keeps it word for word.
const summaryMark = notePrefix + "Earlier turns of this session, in short."

// source is where one history message's full bytes are in the record: a
// piece of a stored call's request, or (piece -1) its kept reply.
type source struct {
	turn, call string
	piece      int
}

// SetBudget makes share give the history budget, a share of the model's
// window, read at the start of each turn. Without it the default applies.
// Call it before serving.
func (r *Replayer) SetBudget(share func() float64) { r.budget = share }

func (r *Replayer) budgetShare() float64 {
	if r.budget == nil {
		return settings.DefaultCompaction().BudgetShare()
	}
	return r.budget()
}

// segment is one turn's part of a history: from the person's message on.
type segment struct {
	turn, prompt string
	start        int
}

// fit is h within the budget for a turn on model, and the step it took, if
// any. h is never changed.
//
// The budget is a guarantee on what fit returns, counted by approxTokens on
// the bytes it returns, and no turn is exempt from it: where keeping the
// newest turn whole and the budget conflict, the budget wins. The only part
// fit never shortens is the head, the harness's own context before the first
// turn (codex's context items, OpenCode's system message), which every fresh
// request carries anyway; a head alone over the budget is the one way out,
// and step.AfterTokens then says so.
//
// The ladder, each rung only while the history is still over its goal:
//   - older turns (all but the newest recentTurns, then fewer), goal half the
//     budget: result stubs, input stubs, then into the summary;
//   - the summary's oldest logs left out, goal half the budget;
//   - the newest turn, goal the budget itself: its stubs, then the summary
//     takes it too, then the summary's logs are left out down to none.
func (r *Replayer) fit(session, from string, h *history, model string) (*history, *event.HistoryStep) {
	if model == "" {
		model = h.model
	}
	window := settings.ModelWindow(model)
	budget := int(r.budgetShare() * float64(window))
	msgs := append([]json.RawMessage{}, h.messages...)
	est := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		est[i] = approxTokens(m)
		total += est[i]
	}
	if budget <= 0 || total <= budget {
		return h, nil
	}
	step := &event.HistoryStep{From: from, Model: model, WindowTokens: window, BudgetTokens: budget, BeforeTokens: total}
	segs := r.segments(session, from, msgs)
	if len(segs) == 0 {
		// No turn of the history is found by its person's message: nothing
		// is a turn to shorten, and a step that changed nothing is not
		// recorded, as everywhere else in fit.
		return h, nil
	}
	start := func(i int) int {
		if i >= len(segs) {
			return len(msgs)
		}
		return segs[i].start
	}
	src := h.src
	pointer := func(i int) string {
		if i < len(src) && src[i].call != "" {
			p := "reply"
			if src[i].piece >= 0 {
				p = strconv.Itoa(src[i].piece)
			}
			return apiGet("/api/v0/turns/" + src[i].turn + "/calls/" + src[i].call + "/pieces/" + p)
		}
		return apiGet("/api/v0/sessions/" + session + "/search?q=<words from it>")
	}
	// made counts each message's stubs, so a stub in a turn the summary then
	// takes is not counted.
	made := make([][2]int, len(msgs))
	shorten := func(from, to int, inputs bool, goal int) {
		for i := from; i < to && total > goal; i++ {
			out, n := r.f.shorten(msgs[i], inputs, stubOver, func(kind string, size int, head string) string {
				return stubText(kind, size, head, pointer(i))
			})
			if n == 0 {
				continue
			}
			made[i][map[bool]int{false: 0, true: 1}[inputs]] += n
			e := approxTokens(out)
			total += e - est[i]
			msgs[i], est[i] = out, e
		}
	}
	summarised := -1 // the newest segment the summary covers
	var sum *summary
	summarise := func(last, goal int) {
		if sum == nil {
			sum = newSummary(r.f, msgs, segs)
		}
		for j := summarised + 1; j < last && total > goal; j++ {
			summarised = j
			total = sum.total(r.f, msgs, est, segs, j)
		}
	}
	fold := func(goal int) {
		if sum == nil {
			sum = newSummary(r.f, msgs, segs)
		}
		for sum.fold < len(sum.entries(summarised)) && total > goal {
			sum.fold++
			total = sum.total(r.f, msgs, est, segs, summarised)
		}
	}
	target := budget / 2
	for keep := recentTurns; keep >= 1 && total > target; keep-- {
		last := len(segs) - keep // the first segment kept whole
		if last <= summarised+1 {
			continue
		}
		lo := start(summarised + 1)
		shorten(lo, start(last), false, target)
		shorten(lo, start(last), true, target)
		summarise(last, target)
	}
	if total > target && summarised >= 0 {
		fold(target)
	}
	if total > budget {
		// The budget wins over keeping the newest turn whole.
		lo := start(summarised + 1)
		shorten(lo, len(msgs), false, budget)
		shorten(lo, len(msgs), true, budget)
		summarise(len(segs), budget)
		fold(budget)
	}
	laid := summarised >= 0 || (sum != nil && sum.fold > 0)
	for _, m := range made[start(summarised+1):] {
		step.ResultsShortened += m[0]
		step.InputsShortened += m[1]
	}
	if laid {
		msgs = sum.layout(r.f, msgs, segs, summarised)
		step.TurnsSummarised = summarised + 1
		step.TurnsLeftOut = sum.fold
	}
	if step.ResultsShortened+step.InputsShortened+step.TurnsSummarised+step.TurnsLeftOut == 0 {
		return h, nil
	}
	total = 0
	for _, m := range msgs {
		total += approxTokens(m)
	}
	step.AfterTokens = total
	if summarised < len(segs)-1 {
		step.TurnsWhole = len(segs) - 2 - summarised // neither the newest nor in the summary
	}
	step.NewestShortened = summarised == len(segs)-1
	for _, m := range made[start(len(segs)-1):] {
		step.NewestShortened = step.NewestShortened || m != [2]int{}
	}
	out := *h
	out.messages = msgs
	out.src = nil
	return &out, step
}

// segments finds each turn of from's history by its person's message, newest
// turn first, searching back from the end; returned oldest first. A turn
// whose message is not found (a summarised one, or another harness's) is
// part of the one before it; what comes before the oldest found is the head
// (context items, a system message, the summary), which is never shortened.
func (r *Replayer) segments(session, from string, msgs []json.RawMessage) []segment {
	turns, err := r.src.SessionTurns(session)
	if err != nil {
		return nil
	}
	at := -1
	for i, t := range turns {
		if t == from {
			at = i
		}
	}
	if at < 0 {
		return nil
	}
	end := len(msgs)
	var segs []segment
	for _, t := range turns[at:] {
		ti, err := r.turn(session, t)
		if err != nil || !ti.found {
			continue
		}
		for i := end - 1; i >= 0; i-- {
			if r.f.isPrompt(msgs[i], ti.prompt) {
				segs = append(segs, segment{turn: t, prompt: ti.prompt, start: i})
				end = i
				break
			}
		}
		if ti.transcript {
			break // nothing of an earlier turn stands behind it
		}
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return segs
}

// summary is what AgentBus's summary of a history's oldest turns is made
// of: the head without any earlier summary, that earlier summary read back
// (its left-out line and its turns' logs, each kept word for word), and each
// turn's log, written once. fold is how many of the oldest logs a step leaves
// out, for the left-out line.
type summary struct {
	head    []json.RawMessage
	headTok int
	// firstRest is segment 0's first message without the earlier summary,
	// when the summary rode in it (Claude Code); nil otherwise.
	firstRest json.RawMessage
	left      leftOut
	old       []logEntry
	logs      []logEntry
	fold      int
}

// logEntry is one turn's log in a summary.
type logEntry struct{ turn, text string }

// leftOut is the turns a summary no longer lists: how many, and the oldest
// and newest of them.
type leftOut struct {
	n              int
	oldest, newest string
}

// summaryHead opens every summary, after summaryMark.
var summaryHead = summaryMark + " AgentBus shortened them to keep the conversation within its budget: " +
	"each keeps the person's words, the model's last words and AgentBus's notes, and names the tool calls; " +
	"their results and the model's other words are left out, and a long text is cut. Files may have changed since: read them again before relying on them. " +
	"A turn's full record: " + apiGet("/api/v0/turns/<turn>/record") + ", and its tool calls with their inputs at /api/v0/turns/<turn>/tools."

// logStart finds each turn's log in a summary's text.
var logStart = regexp.MustCompile(`\n\nTurn (\S+)\. The person wrote:\n`)

// leftLine reads a summary's left-out line.
var leftLine = regexp.MustCompile(`^\n\nLeft out here: (\d+) earlier turns?, from turn (\S*) to turn (\S*)\. Their records are at the route above\.`)

// parseSummary reads a summary's text back: its left-out line and its logs.
// A text that does not open with summaryHead (written by another build) is
// one log of no turn, kept as it is until a step leaves it out.
func parseSummary(text string) (leftOut, []logEntry) {
	if !strings.HasPrefix(text, summaryHead) {
		return leftOut{}, []logEntry{{text: text}}
	}
	rest := text[len(summaryHead):]
	var left leftOut
	if m := leftLine.FindStringSubmatch(rest); m != nil {
		left.n, _ = strconv.Atoi(m[1])
		left.oldest, left.newest = m[2], m[3]
		rest = rest[len(m[0]):]
	}
	var out []logEntry
	idx := logStart.FindAllStringSubmatchIndex(rest, -1)
	for i, m := range idx {
		end := len(rest)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out = append(out, logEntry{turn: rest[m[2]:m[3]], text: rest[m[0]+2 : end]})
	}
	return left, out
}

func newSummary(f format, msgs []json.RawMessage, segs []segment) *summary {
	s := &summary{head: append([]json.RawMessage{}, msgs[:segs[0].start]...)}
	old, found := "", false
	for i := range s.head {
		if text, rest, ok := f.summaryIn(s.head[i]); ok {
			old, found = text, true
			if rest == nil {
				s.head = append(s.head[:i], s.head[i+1:]...)
			} else {
				s.head[i] = rest
			}
			break
		}
	}
	if !found {
		// Claude Code's summary rides in the first message of the oldest
		// turn kept, which is now segment 0.
		if text, rest, ok := f.summaryIn(msgs[segs[0].start]); ok {
			old, s.firstRest = text, rest
		}
	}
	if old != "" {
		s.left, s.old = parseSummary(old)
	}
	for _, m := range s.head {
		s.headTok += approxTokens(m)
	}
	for k := range segs {
		end := len(msgs)
		if k+1 < len(segs) {
			end = segs[k+1].start
		}
		s.logs = append(s.logs, logEntry{turn: segs[k].turn, text: turnLog(f, segs[k], msgs[segs[k].start:end])})
	}
	return s
}

// entries are the logs of a summary of segments 0..j, oldest first: the
// earlier summary's, then the new ones.
func (s *summary) entries(j int) []logEntry {
	return append(append([]logEntry{}, s.old...), s.logs[:j+1]...)
}

// text is the summary of segments 0..j, its oldest fold logs left out.
func (s *summary) text(j int) string {
	all := s.entries(j)
	left := s.left
	for _, e := range all[:s.fold] {
		if left.n == 0 {
			left.oldest = e.turn
		}
		left.n++
		left.newest = e.turn
	}
	var b strings.Builder
	b.WriteString(summaryHead)
	if left.n > 0 {
		word := "turns"
		if left.n == 1 {
			word = "turn"
		}
		fmt.Fprintf(&b, "\n\nLeft out here: %d earlier %s, from turn %s to turn %s. Their records are at the route above.", left.n, word, left.oldest, left.newest)
	}
	for _, e := range all[s.fold:] {
		b.WriteString("\n\n" + e.text)
	}
	return b.String()
}

// next is where the history goes on after a summary of segments 0..j: the
// first message of segment j+1, or the end when the summary takes them all.
func next(msgs []json.RawMessage, segs []segment, j int) int {
	if j+1 >= len(segs) {
		return len(msgs)
	}
	return segs[j+1].start
}

// first is the message the summary of segments 0..j goes in front of, or nil
// when it takes every turn and stands alone.
func (s *summary) first(msgs []json.RawMessage, segs []segment, j int) json.RawMessage {
	if j < 0 && s.firstRest != nil {
		return s.firstRest
	}
	if j+1 >= len(segs) {
		return nil
	}
	return msgs[segs[j+1].start]
}

// total is the size of msgs once segments 0..j are summarised, est being
// each message's size.
func (s *summary) total(f format, msgs []json.RawMessage, est []int, segs []segment, j int) int {
	next := next(msgs, segs, j)
	n := s.headTok
	for _, m := range f.withSummary(s.first(msgs, segs, j), s.text(j)) {
		n += approxTokens(m)
	}
	if next < len(msgs) {
		for _, e := range est[next+1:] {
			n += e
		}
	}
	return n
}

// layout is msgs with segments 0..j replaced by the summary, laid out in
// front of the first message of segment j+1.
func (s *summary) layout(f format, msgs []json.RawMessage, segs []segment, j int) []json.RawMessage {
	next := next(msgs, segs, j)
	out := append([]json.RawMessage{}, s.head...)
	out = append(out, f.withSummary(s.first(msgs, segs, j), s.text(j))...)
	if next < len(msgs) {
		out = append(out, msgs[next+1:]...)
	}
	return out
}

// turnLog is one turn in a summary.
func turnLog(f format, s segment, msgs []json.RawMessage) string {
	var texts, calls []string
	var notes []string
	for _, m := range msgs {
		t, c := f.said(m)
		for _, x := range t {
			switch {
			case strings.HasPrefix(x, summaryMark):
			case strings.HasPrefix(x, notePrefix):
				notes = append(notes, strings.TrimPrefix(x, notePrefix))
			default:
				texts = append(texts, x)
			}
		}
		calls = append(calls, c...)
	}
	var b strings.Builder
	b.WriteString("Turn " + s.turn + ". The person wrote:\n" + clipLong(strings.TrimSpace(s.prompt)))
	if len(calls) > 0 {
		shown := calls
		if len(shown) > summaryCalls {
			shown = shown[:summaryCalls]
		}
		b.WriteString("\nTool calls (" + strconv.Itoa(len(calls)) + "): " + strings.Join(shown, "; "))
		if len(calls) > len(shown) {
			b.WriteString("; and " + strconv.Itoa(len(calls)-len(shown)) + " more")
		}
	}
	for _, n := range notes {
		b.WriteString("\nAgentBus noted: " + clip(n, noteBytes))
	}
	if len(texts) > 0 {
		b.WriteString("\nThe model's last words:\n" + clipLong(strings.TrimSpace(texts[len(texts)-1])))
	}
	return b.String()
}

// callLine is one tool call in a summary: its name and how its input began.
func callLine(name, input string) string {
	if strings.Contains(input, `"agentbus_shortened"`) || strings.HasPrefix(input, notePrefix) {
		return name + " (its input was shortened earlier)"
	}
	return name + " " + clip(strings.Join(strings.Fields(input), " "), callHead)
}

// stubText is what a shortened tool result or input says.
func stubText(kind string, size int, head, where string) string {
	advice := "Read the file or run the command again if you need it now."
	if kind == inputKind {
		advice = "The call ran with its full input."
	}
	return fmt.Sprintf("%sThis %s (%d bytes) was shortened to keep the conversation within its budget. It began: %q. "+
		"The full text is in AgentBus's record: %s. %s",
		notePrefix, kind, size, clip(head, headBytes), where, advice)
}

// clipLong is s cut to logBytes, saying the rest is in the record.
func clipLong(s string) string {
	if len(s) <= logBytes {
		return s
	}
	return clip(s, logBytes) + " (cut: the whole text is in the turn's record)"
}

// apiGet is the command that reads path of the API, with the turn's token.
func apiGet(path string) string {
	return `curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL` + path + `"`
}

// clip is s cut to at most n bytes on a rune boundary, with "…" when cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// imageBlocks are the content blocks that hold an image, by their type in
// each format: Anthropic's image, OpenAI Responses' input_image, Chat
// Completions' image_url. The names do not overlap any other block's.
var imageBlocks = map[string]bool{"image": true, "input_image": true, "image_url": true}

// approxTokens is the one count of the history budget, for one message: its
// bytes over tokenBytes, except that each image block counts imageTokens
// whatever its bytes. An image is known by its block's type, never guessed
// from the text: a long run of hex or base64 in a tool result is text, and
// counts by its bytes (review t_9d14aa5c291e0a5f: a 256 KiB hex result
// counted as one image).
func approxTokens(m []byte) int {
	n, images := len(m), 0
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		raw = bytes.TrimLeft(raw, " \t\r\n")
		if len(raw) == 0 {
			return
		}
		switch raw[0] {
		case '[':
			els, _ := body.Elements(raw)
			for _, e := range els {
				walk(e)
			}
		case '{':
			if imageBlocks[body.Label(raw, "type")] {
				n -= len(raw)
				images++
				return
			}
			fs, _ := body.Fields(raw)
			for _, f := range fs {
				walk(f.Value)
			}
		}
	}
	walk(m)
	return int(float64(n)/tokenBytes) + images*imageTokens
}

// stubInput is a tool call's input once shortened: an object, as every
// format's tool input is, holding the stub.
func stubInput(stub string) json.RawMessage {
	return json.RawMessage(`{"agentbus_shortened":` + string(jsonString(stub)) + `}`)
}

// textOf is a JSON string's value, or the raw JSON when it is not a string.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
