package replay

// What each format knows for the history budget (budget.go): where a turn's
// person's message is, where tool results and tool-call inputs sit, what a
// message says for a summary, and where the summary goes.

import (
	"encoding/json"
	"strings"

	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/body"
)

const (
	resultKind = "tool result"
	inputKind  = "tool call's input"
)

// setField is the object o with key's value set to v, every other field kept
// in its order. ok is false when o is not an object.
func setField(o json.RawMessage, key string, v json.RawMessage) (json.RawMessage, bool) {
	fs, err := body.Fields(o)
	if err != nil {
		return o, false
	}
	for i, f := range fs {
		if f.Key == key {
			fs[i].Value = v
			return body.Object(fs), true
		}
	}
	return o, false
}

// mapContent is m with each element of its key list passed through fn, and
// how many fn changed. m stays byte for byte when none changed.
func mapContent(m json.RawMessage, key string, fn func(b json.RawMessage) (json.RawMessage, bool)) (json.RawMessage, int) {
	els, ok := body.Elements(field(m, key))
	if !ok {
		return m, 0
	}
	n := 0
	for i, b := range els {
		if nb, changed := fn(b); changed {
			els[i] = nb
			n++
		}
	}
	if n == 0 {
		return m, 0
	}
	out, ok := setField(m, key, joinArray(els))
	if !ok {
		return m, 0
	}
	return out, n
}

// partsText is the text of a content value: a string, or a list of parts
// whose text is under "text" (an image or a file part says so).
func partsText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	els, ok := body.Elements(c)
	if !ok {
		return string(c)
	}
	var b strings.Builder
	for _, p := range els {
		var t string
		if json.Unmarshal(field(p, "text"), &t) == nil {
			b.WriteString(t)
			continue
		}
		b.WriteString("[" + body.Label(p, "type") + "]")
	}
	return b.String()
}

// Claude Code: Anthropic Messages.

func (claude) isPrompt(m json.RawMessage, prompt string) bool {
	return role(m) == "user" && hasPrompt([]json.RawMessage{m}, prompt)
}

// shorten: a tool_result block's content (in a user message), or a tool_use
// block's input (in an assistant message), which stays an object.
func (claude) shorten(m json.RawMessage, inputs bool, over int, stub func(string, int, string) string) (json.RawMessage, int) {
	if want := map[bool]string{false: "user", true: "assistant"}[inputs]; role(m) != want {
		return m, 0
	}
	return mapContent(m, "content", func(b json.RawMessage) (json.RawMessage, bool) {
		switch t := body.Label(b, "type"); {
		case !inputs && t == "tool_result":
			c := field(b, "content")
			if len(c) <= over {
				return b, false
			}
			return setField(b, "content", jsonString(stub(resultKind, len(c), partsText(c))))
		case inputs && t == "tool_use":
			in := field(b, "input")
			if len(in) <= over {
				return b, false
			}
			return setField(b, "input", stubInput(stub(inputKind, len(in), string(in))))
		}
		return b, false
	})
}

func (claude) said(m json.RawMessage) (texts, calls []string) {
	if role(m) != "assistant" {
		return nil, nil
	}
	blocks, _ := body.Elements(field(m, "content"))
	for _, b := range blocks {
		switch body.Label(b, "type") {
		case "text":
			if t := textOf(field(b, "text")); strings.TrimSpace(t) != "" {
				texts = append(texts, t)
			}
		case "tool_use", "server_tool_use":
			calls = append(calls, callLine(textOf(field(b, "name")), string(field(b, "input"))))
		}
	}
	return texts, calls
}

// summaryIn: the summary is the first text block of a user message.
func (claude) summaryIn(m json.RawMessage) (string, json.RawMessage, bool) {
	if role(m) != "user" {
		return "", nil, false
	}
	blocks, ok := body.Elements(field(m, "content"))
	if !ok || len(blocks) == 0 || body.Label(blocks[0], "type") != "text" {
		return "", nil, false
	}
	t := textOf(field(blocks[0], "text"))
	if !strings.HasPrefix(t, summaryMark) {
		return "", nil, false
	}
	if len(blocks) == 1 {
		return t, nil, true
	}
	rest, _ := setField(m, "content", joinArray(blocks[1:]))
	return t, rest, true
}

// withSummary: the summary goes in as the first text block of the person's
// message, so the history still alternates and starts with the person, as
// Claude Code's layout does; no message is made up for the model. A summary
// that takes every turn (first nil) is a user message of that one block,
// which build puts in front of the new prompt (mergeSummary).
func (claude) withSummary(first json.RawMessage, text string) []json.RawMessage {
	block := body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)}, {Key: "text", Value: jsonString(text)}})
	if first == nil {
		return []json.RawMessage{body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"user"`)},
			{Key: "content", Value: joinArray([]json.RawMessage{block})}})}
	}
	c := field(first, "content")
	var blocks []json.RawMessage
	if body.IsString(c) {
		blocks = []json.RawMessage{block, body.Object([]body.Field{{Key: "type", Value: json.RawMessage(`"text"`)}, {Key: "text", Value: c}})}
	} else if els, ok := body.Elements(c); ok {
		blocks = append([]json.RawMessage{block}, els...)
	}
	if role(first) == "user" && blocks != nil {
		if out, ok := setField(first, "content", joinArray(blocks)); ok {
			return []json.RawMessage{out}
		}
	}
	return []json.RawMessage{claude{}.prompt(text), first}
}

// codex: OpenAI Responses input items.

func (codex) isPrompt(m json.RawMessage, prompt string) bool {
	if !openai.IsPrompt(m) {
		return false
	}
	s, ok := openai.PromptText(m)
	return ok && samePrompt(s, prompt)
}

// shorten: a function_call_output or custom_tool_call_output's output, or a
// function_call's arguments (a JSON text, which stays one) or a
// custom_tool_call's input.
func (codex) shorten(m json.RawMessage, inputs bool, over int, stub func(string, int, string) string) (json.RawMessage, int) {
	key, kind := "", resultKind
	switch t := body.Label(m, "type"); {
	case !inputs && (t == "function_call_output" || t == "custom_tool_call_output"):
		key = "output"
	case inputs && t == "function_call":
		key, kind = "arguments", inputKind
	case inputs && t == "custom_tool_call":
		key, kind = "input", inputKind
	default:
		return m, 0
	}
	v := field(m, key)
	if len(v) <= over {
		return m, 0
	}
	text := stub(kind, len(v), partsText(v))
	nv := jsonString(text)
	if key == "arguments" {
		nv = jsonString(string(stubInput(text)))
	}
	out, ok := setField(m, key, nv)
	if !ok {
		return m, 0
	}
	return out, 1
}

func (codex) said(m json.RawMessage) (texts, calls []string) {
	switch body.Label(m, "type") {
	case "message", "":
		r := role(m)
		if r != "assistant" && r != "developer" {
			return nil, nil
		}
		parts, _ := body.Elements(field(m, "content"))
		for _, p := range parts {
			t := textOf(field(p, "text"))
			if strings.TrimSpace(t) == "" || (r == "developer" && !strings.HasPrefix(t, notePrefix)) {
				continue
			}
			texts = append(texts, t)
		}
	case "function_call":
		calls = append(calls, callLine(textOf(field(m, "name")), textOf(field(m, "arguments"))))
	case "custom_tool_call":
		calls = append(calls, callLine(textOf(field(m, "name")), textOf(field(m, "input"))))
	case "local_shell_call":
		calls = append(calls, callLine("local_shell", string(field(m, "action"))))
	}
	return texts, calls
}

// summaryIn: the summary is a developer message of its own.
func (codex) summaryIn(m json.RawMessage) (string, json.RawMessage, bool) {
	if role(m) != "developer" {
		return "", nil, false
	}
	parts, _ := body.Elements(field(m, "content"))
	if len(parts) == 1 {
		if t := textOf(field(parts[0], "text")); strings.HasPrefix(t, summaryMark) {
			return t, nil, true
		}
	}
	return "", nil, false
}

// withSummary: a developer message before the person's, as the note is.
func (codex) withSummary(first json.RawMessage, text string) []json.RawMessage {
	if first == nil {
		return []json.RawMessage{openai.DeveloperMessage(text)}
	}
	return []json.RawMessage{openai.DeveloperMessage(text), first}
}

// OpenCode: OpenAI Chat Completions.

func (chatCompletions) isPrompt(m json.RawMessage, prompt string) bool {
	return role(m) == "user" && hasPrompt([]json.RawMessage{m}, prompt)
}

// shorten: a role-tool message's content, or an assistant tool call's
// function.arguments (a JSON text, which stays one).
func (chatCompletions) shorten(m json.RawMessage, inputs bool, over int, stub func(string, int, string) string) (json.RawMessage, int) {
	switch {
	case !inputs && role(m) == "tool":
		c := field(m, "content")
		if len(c) <= over {
			return m, 0
		}
		out, ok := setField(m, "content", jsonString(stub(resultKind, len(c), partsText(c))))
		if !ok {
			return m, 0
		}
		return out, 1
	case inputs && role(m) == "assistant":
		return mapContent(m, "tool_calls", func(c json.RawMessage) (json.RawMessage, bool) {
			fn := field(c, "function")
			args := field(fn, "arguments")
			if len(args) <= over {
				return c, false
			}
			nf, ok := setField(fn, "arguments", jsonString(string(stubInput(stub(inputKind, len(args), textOf(args))))))
			if !ok {
				return c, false
			}
			return setField(c, "function", nf)
		})
	}
	return m, 0
}

func (chatCompletions) said(m json.RawMessage) (texts, calls []string) {
	switch role(m) {
	case "assistant":
		if t := partsText(field(m, "content")); strings.TrimSpace(t) != "" && t != "null" {
			texts = append(texts, t)
		}
		tcs, _ := body.Elements(field(m, "tool_calls"))
		for _, c := range tcs {
			fn := field(c, "function")
			calls = append(calls, callLine(textOf(field(fn, "name")), textOf(field(fn, "arguments"))))
		}
	case "user":
		if t := textOf(field(m, "content")); strings.HasPrefix(t, notePrefix) {
			texts = append(texts, t)
		}
	}
	return texts, calls
}

// summaryIn: the summary is a user message of its own, as the note is.
func (chatCompletions) summaryIn(m json.RawMessage) (string, json.RawMessage, bool) {
	if role(m) != "user" {
		return "", nil, false
	}
	if t := textOf(field(m, "content")); strings.HasPrefix(t, summaryMark) {
		return t, nil, true
	}
	return "", nil, false
}

func (chatCompletions) withSummary(first json.RawMessage, text string) []json.RawMessage {
	if first == nil {
		return []json.RawMessage{chatUserMessage(text)}
	}
	return []json.RawMessage{chatUserMessage(text), first}
}
