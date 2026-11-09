package run

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/store"
)

// The transcript a turn gets when it runs on another harness than the
// session's latest turn. That turn's history is in the other vendor's format,
// with tool calls, thinking and encrypted reasoning this harness cannot read,
// so the turn starts fresh and its prompt begins with the earlier turns as
// plain text: each person's message and the agent's final reply, oldest
// first, naming the agent. What the earlier agents did in files is in the
// folder, which a switched agent shares.
//
// It is not a translation: tool calls and their results are left out. Its
// budget keeps the prompt within MaxPrompt and the transcript within
// TranscriptMax; past it the oldest turns are left out, and it says how many.

// TranscriptMax bounds a transcript, in bytes.
const TranscriptMax = 48 << 10

// Each turn's prompt and reply are cut to these, in bytes.
const (
	transcriptPromptMax = 4 << 10
	transcriptReplyMax  = 8 << 10
)

const (
	transcriptHead = "[AgentBus: this conversation started before you joined it. Earlier turns ran on other agents. " +
		"Below is each message the person sent and the final reply the agent gave, oldest first. " +
		"Tool calls are left out. Any files those agents changed are in your current folder.]\n\n"
	transcriptFoot = "[AgentBus: end of the earlier turns. The person's new message to you follows.]\n\n"
)

// transcript is the context for a turn of session on another harness, for a
// prompt of promptLen bytes. It is "" when the session has no earlier turn
// with words in it.
func (e *Engine) transcript(session string, promptLen int) (string, error) {
	turns, err := e.cfg.Store.Transcript(session)
	if err != nil {
		return "", err
	}
	budget := TranscriptMax
	if room := MaxPrompt - promptLen; room < budget {
		budget = room
	}
	budget -= len(transcriptHead) + len(transcriptFoot) + 64 // the note on turns left out
	// The newest turns first, so the budget keeps them; then oldest first.
	var parts []string
	used := 0
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if strings.TrimSpace(t.Prompt) == "" && strings.TrimSpace(t.Reply) == "" {
			continue
		}
		part := transcriptTurn(i+1, e.agentLabel(t.AgentID, t.Agent, t.Model), t.Prompt, t.PromptCut, t.Reply, t.ReplyCut)
		if used+len(part) > budget {
			break
		}
		used += len(part)
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(transcriptHead)
	if left := countWords(turns) - len(parts); left > 0 {
		b.WriteString("[" + strconv.Itoa(left) + " earlier turns are left out: they do not fit.]\n\n")
	}
	for i := len(parts) - 1; i >= 0; i-- {
		b.WriteString(parts[i])
	}
	b.WriteString(transcriptFoot)
	return b.String(), nil
}

// countWords counts the turns with words in them.
func countWords(turns []store.TranscriptTurn) int {
	n := 0
	for _, t := range turns {
		if strings.TrimSpace(t.Prompt) != "" || strings.TrimSpace(t.Reply) != "" {
			n++
		}
	}
	return n
}

// transcriptTurn is one turn of a transcript.
func transcriptTurn(n int, agent, prompt string, promptCut bool, reply string, replyCut bool) string {
	var b strings.Builder
	num := strconv.Itoa(n)
	b.WriteString("--- Turn " + num + ", the person to " + agent + " ---\n")
	b.WriteString(cutTo(strings.TrimSpace(prompt), transcriptPromptMax, promptCut))
	b.WriteString("\n\n--- Turn " + num + ", " + agent + " replied ---\n")
	if r := strings.TrimSpace(reply); r != "" {
		b.WriteString(cutTo(r, transcriptReplyMax, replyCut))
	} else {
		b.WriteString("(no reply: the turn ended without one)")
	}
	b.WriteString("\n\n")
	return b.String()
}

// cutTo is s cut to max bytes on a rune boundary, marked when anything was
// cut, here or before (cut).
func cutTo(s string, max int, cut bool) string {
	if len(s) > max {
		n := max
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s, cut = s[:n], true
	}
	if cut {
		s += " [… cut]"
	}
	return s
}

// agentLabel names an earlier turn's agent: the saved agent's name, else its
// harness, with the model its last call asked for.
func (e *Engine) agentLabel(id, harnessName, model string) string {
	name := ""
	if id != "" && e.cfg.AgentName != nil {
		name = e.cfg.AgentName(id)
	}
	if name == "" {
		name = harnessName
		if h, ok := harness.Get(harnessName); ok {
			name = h.Info().Label
		}
	}
	if name == "" {
		name = "an agent"
	}
	if model != "" {
		name += " (model " + model + ")"
	}
	return name
}
