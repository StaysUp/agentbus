package replay

// chatCompletions is OpenCode's format: the OpenAI Chat Completions API as
// OpenCode 1.19.6 sends it through @ai-sdk/openai-compatible on OpenRouter
// (2026-11-21):
//
//   - A fresh process puts its system message first, then the prompt as a
//     user message, then the turn's own tool loop (assistant with tool_calls,
//     then role-tool results).
//   - A resumed OpenCode turn would send the earlier messages, then the new
//     prompt. Own replay starts the child without --session and puts that
//     history in front of the fresh request's prompt.
//   - Subagents (task) use another system prompt, which is the main-thread
//     identity, as Claude Code's system prompt is.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/store"
)

type chatCompletions struct{}

func (chatCompletions) read(c store.CallContext) ([]json.RawMessage, string, int) {
	var msgs, id []json.RawMessage
	tools := 0
	for _, p := range c.Pieces {
		switch p.Kind {
		case body.Message:
			msgs = append(msgs, p.JSON)
			if p.Label == "system" || p.Label == "developer" {
				id = append(id, p.JSON)
			}
		case body.Tool:
			tools++
		}
	}
	return msgs, chatIdentity(id), tools
}

func chatIdentity(msgs []json.RawMessage) string {
	var b []byte
	for _, m := range msgs {
		b = append(b, m...)
		b = append(b, '\n')
	}
	return string(b)
}

func (chatCompletions) recoverable() bool { return true }

func (c chatCompletions) mainLayout(msgs []json.RawMessage, prompt string) bool {
	return c.hasPrompt(msgs, prompt)
}

func (chatCompletions) hasPrompt(msgs []json.RawMessage, prompt string) bool {
	return hasPrompt(msgs, prompt)
}

func (chatCompletions) replyKept(stop string) bool { return stop != "" }

func (chatCompletions) carry(*history, store.CallContext) {}

var chatReasoning = map[string]bool{
	"reasoning": true, "reasoning_content": true, "reasoning_details": true,
	"reasoning_opaque": true, "reasoning_text": true,
}

func (chatCompletions) otherModel(msgs []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		if role(m) != "assistant" {
			out = append(out, m)
			continue
		}
		stripped, err := dropFields(m, chatReasoning)
		if err != nil {
			return nil, err
		}
		out = append(out, stripped)
	}
	return out, nil
}

func dropFields(m json.RawMessage, keys map[string]bool) (json.RawMessage, error) {
	fields, err := body.Fields(m)
	if err != nil {
		return nil, err
	}
	kept := make([]body.Field, 0, len(fields))
	for _, f := range fields {
		if !keys[f.Key] {
			kept = append(kept, f)
		}
	}
	return body.Object(kept), nil
}

func (chatCompletions) asHistory(m json.RawMessage) (json.RawMessage, error) {
	fields, err := body.Fields(m)
	if err != nil {
		return nil, err
	}
	if body.Label(m, "role") == "" {
		return nil, errors.New("a message has no role")
	}
	out := make([]body.Field, 0, len(fields))
	for _, f := range fields {
		if f.Key == "cache_control" {
			continue
		}
		if f.Key == "content" {
			if els, ok := body.Elements(f.Value); ok {
				parts := make([]json.RawMessage, 0, len(els))
				for _, e := range els {
					p, err := dropFields(e, map[string]bool{"cache_control": true})
					if err != nil {
						return nil, err
					}
					parts = append(parts, p)
				}
				f.Value = joinArray(parts)
			}
		}
		out = append(out, f)
	}
	return body.Object(out), nil
}

// reply: the assistant message as the vendor sent it. One that said nothing
// (content null, missing, blank or only blank text, and no tool call, refusal,
// audio or image) keeps its other fields, the reasoning among them, and gets
// the note as its content, as a message with nothing in it is what the API
// refuses. An empty tool_calls list goes: the API refuses that too. Reported on
// PR #106 on 2026-11-27 (OpenCode passed an empty content through): not
// captured live, no OpenCode version recorded. The shapes are in
// emptyreply_test.go.
func (chatCompletions) reply(rep body.Reply, stop string) ([]json.RawMessage, ending, error) {
	if role(rep.Message) != "assistant" {
		return nil, ending{}, errors.New("the kept reply is not an assistant message")
	}
	if stop != "tool_calls" && stop != "function_call" {
		m, err := withContentNote(rep.Message)
		if err != nil {
			return nil, ending{}, err
		}
		return []json.RawMessage{m}, ending{finished: true}, nil
	}
	ids, err := chatToolIDs(rep.Message)
	if err != nil {
		return nil, ending{}, err
	}
	e := ending{asked: true}
	for _, id := range ids {
		e.results = append(e.results, body.Object([]body.Field{
			{Key: "role", Value: json.RawMessage(`"tool"`)},
			{Key: "tool_call_id", Value: jsonString(id)},
			{Key: "content", Value: jsonString(notFinished)},
		}))
	}
	return []json.RawMessage{rep.Message}, e, nil
}

// withContentNote is message m with the note for an empty reply as its
// content when it said nothing, and m as it was otherwise.
func withContentNote(m json.RawMessage) (json.RawMessage, error) {
	fields, err := body.Fields(m)
	if err != nil {
		return nil, err
	}
	if chatSaid(fields) {
		return m, nil
	}
	note := jsonString(emptyReplyNote)
	out := make([]body.Field, 0, len(fields)+1)
	set := false
	for _, f := range fields {
		switch {
		case f.Key == "content":
			f.Value, set = note, true
		case f.Key == "tool_calls" && !present(f.Value):
			continue
		}
		out = append(out, f)
	}
	if !set {
		out = append(out, body.Field{Key: "content", Value: note})
	}
	return body.Object(out), nil
}

// present reports whether a field's value holds anything: not null, not an
// empty string, list or object.
func present(v json.RawMessage) bool {
	if els, ok := body.Elements(v); ok {
		return len(els) > 0
	}
	if fields, err := body.Fields(v); err == nil {
		return len(fields) > 0
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s != ""
	}
	return len(v) > 0 && string(v) != "null"
}

// chatSaid reports whether an assistant message's fields hold anything the
// person would read: a tool call (tool_calls, or the older function_call), a
// refusal, audio, images, a text that is not blank, or a part that is not
// text. A content of another shape counts as said.
func chatSaid(fields []body.Field) bool {
	for _, f := range fields {
		switch f.Key {
		case "tool_calls", "function_call", "audio", "images":
			if present(f.Value) {
				return true
			}
		case "refusal":
			var s string
			if json.Unmarshal(f.Value, &s) == nil && !body.Blank(s) {
				return true
			}
		case "content":
			if chatContentSaid(f.Value) {
				return true
			}
		}
	}
	return false
}

func chatContentSaid(c json.RawMessage) bool {
	if len(c) == 0 || string(c) == "null" {
		return false
	}
	var s string
	if json.Unmarshal(c, &s) == nil {
		return !body.Blank(s)
	}
	parts, ok := body.Elements(c)
	if !ok {
		return true
	}
	for _, p := range parts {
		var text string
		if body.Label(p, "type") != "text" || json.Unmarshal(field(p, "text"), &text) != nil || !body.Blank(text) {
			return true
		}
	}
	return false
}

func chatToolIDs(m json.RawMessage) ([]string, error) {
	calls, ok := body.Elements(field(m, "tool_calls"))
	if !ok {
		return nil, errors.New("an assistant message that asked for tools has no tool_calls")
	}
	var ids []string
	for _, c := range calls {
		var id string
		if json.Unmarshal(field(c, "id"), &id) != nil || id == "" {
			return nil, errors.New("a tool call has no id")
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("an assistant message that asked for tools has no tool_calls")
	}
	return ids, nil
}

func (chatCompletions) canEnd(msgs []json.RawMessage, _ bool) bool {
	last := msgs[len(msgs)-1]
	if role(last) != "assistant" {
		return true
	}
	calls, ok := body.Elements(field(last, "tool_calls"))
	return !ok || len(calls) == 0
}

func (chatCompletions) prompt(text string) json.RawMessage { return chatUserMessage(text) }

func (chatCompletions) note(text string) json.RawMessage { return chatUserMessage(text) }

func chatUserMessage(text string) json.RawMessage {
	return body.Object([]body.Field{
		{Key: "role", Value: json.RawMessage(`"user"`)},
		{Key: "content", Value: jsonString(text)},
	})
}

func (chatCompletions) refusal(msg string) *gateway.Refusal {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg,
		"type": "invalid_request_error", "code": "agentbus_replay_failed"}})
	return &gateway.Refusal{Status: 400, ContentType: "application/json", Body: b}
}

func (c chatCompletions) parse(data []byte) (request, bool) {
	fields, err := body.Fields(data)
	if err != nil {
		return request{}, false
	}
	var msgsRaw json.RawMessage
	tools := 0
	for _, f := range fields {
		switch f.Key {
		case "messages":
			msgsRaw = f.Value
		case "tools", "functions":
			els, _ := body.Elements(f.Value)
			tools += len(els)
		}
	}
	msgs, ok := body.Elements(msgsRaw)
	if !ok {
		return request{}, false
	}
	var id []json.RawMessage
	for _, m := range msgs {
		r := role(m)
		if r == "system" || r == "developer" {
			id = append(id, m)
		}
	}
	return request{fields: fields, msgs: msgs, tools: tools, id: chatIdentity(id)}, true
}

func chatPromptAt(msgs []json.RawMessage, prompt string) int {
	i := 0
	for i < len(msgs) {
		r := role(msgs[i])
		if r == "system" || r == "developer" {
			i++
			continue
		}
		break
	}
	if i < len(msgs) && role(msgs[i]) == "user" && hasPrompt(msgs[i:i+1], prompt) {
		return i
	}
	return -1
}

func (c chatCompletions) fresh(req request, prompt string) bool {
	p := chatPromptAt(req.msgs, prompt)
	if p < 0 {
		return false
	}
	for _, m := range req.msgs[p+1:] {
		switch role(m) {
		case "assistant", "tool", "function":
		default:
			return false
		}
	}
	return true
}

func (chatCompletions) freshRule() string {
	return "(its system prompt) does not carry the turn's prompt as its first user message after system and developer messages"
}

func (c chatCompletions) build(req request, h *history, prompt string) ([]byte, func(http.Header), error) {
	p := chatPromptAt(req.msgs, prompt)
	if p < 0 {
		return nil, nil, errors.New("the request does not carry the turn's prompt as its first user message")
	}
	leading, tail := req.msgs[:p], req.msgs[p:]
	var msgs []json.RawMessage
	if !chatHasSys(h.messages) {
		msgs = append(msgs, leading...)
	}
	msgs = append(msgs, h.messages...)
	msgs = append(msgs, tail...)
	for i, f := range req.fields {
		if f.Key == "messages" {
			req.fields[i].Value = joinArray(msgs)
		}
	}
	return body.Object(req.fields), nil, nil
}

func chatHasSys(msgs []json.RawMessage) bool {
	for _, m := range msgs {
		r := role(m)
		if r == "system" || r == "developer" {
			return true
		}
	}
	return false
}
