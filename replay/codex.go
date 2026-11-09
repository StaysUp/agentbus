package replay

// codex's format: the OpenAI Responses API as codex-cli sends it to
// chatgpt.com. Measured against codex-cli 0.164.0 on 2026-11-06:
//
//   - A resumed codex turn sends the whole earlier conversation as input
//     items, with store false and no previous_response_id: the earlier
//     turn's input items, then its reply's output items, then the new
//     prompt. The encrypted reasoning goes back byte for byte.
//   - A fresh process puts its context first (the tools item, developer
//     messages, AGENTS.md and the environment as a user message), then the
//     prompt, a user message whose content kinds say user.text, then the
//     turn's own tool loop.
//   - The backend took a spliced request from a fresh process: the earlier
//     turn's items in front of the fresh request's prompt. It read the
//     history from cache only when the fresh thread id was replaced by the
//     earlier one, in the body (prompt_cache_key, client_metadata) and in
//     the headers (Session-Id, Thread-Id, X-Client-Request-Id,
//     X-Codex-Turn-Metadata, X-Codex-Window-Id): 18,304 of 18,428 input
//     tokens cached against 12,288 without.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/store"
)

type codex struct{}

// read: the input items, the thread id as identity, and the tools: the
// request's tools list and codex's additional_tools item.
func (codex) read(c store.CallContext) ([]json.RawMessage, string, int) {
	var items []json.RawMessage
	tools := 0
	for _, p := range c.Pieces {
		switch p.Kind {
		case body.InputItem:
			items = append(items, p.JSON)
			if p.Label == "additional_tools" {
				tools++
			}
		case body.Tool:
			tools++
		}
	}
	top, _ := body.Fields(c.Request)
	return items, threadOf(top), tools
}

// threadOf is a request's main-thread identity: its thread id. A codex
// subagent runs in a thread of its own.
func threadOf(top []body.Field) string {
	for _, f := range top {
		if f.Key == "prompt_cache_key" {
			return string(f.Value)
		}
	}
	return ""
}

// recoverable is false: a replayed turn's stored requests carry the
// session's thread id in place of the one the agent sent, so they cannot say
// which of the agent's requests is the main thread's.
func (codex) recoverable() bool { return false }

// mainLayout: a request that carries the turn's prompt and no compaction.
func (c codex) mainLayout(items []json.RawMessage, prompt string) bool {
	if !c.hasPrompt(items, prompt) {
		return false
	}
	for _, it := range items {
		if openai.IsCompaction(it) {
			return false
		}
	}
	return true
}

func (codex) hasPrompt(items []json.RawMessage, prompt string) bool {
	if strings.TrimSpace(prompt) == "" {
		return false
	}
	for _, it := range items {
		if openai.IsPrompt(it) {
			if s, ok := openai.PromptText(it); ok && samePrompt(s, prompt) {
				return true
			}
		}
	}
	return false
}

// replyKept: only a completed response is kept (openai.Adapter.Build).
func (codex) replyKept(stop string) bool { return stop == "completed" }

func (codex) carry(h *history, c store.CallContext) {
	top, _ := body.Fields(c.Request)
	h.thread, _ = openai.ThreadID(top)
}

// otherModel: reasoning items go. Their encrypted content is readable only by
// the provider and model that made it.
func (codex) otherModel(items []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(items))
	for _, m := range items {
		if body.Label(m, "type") != "reasoning" {
			out = append(out, m)
		}
	}
	return out, nil
}

// asHistory: an input item goes back as it was sent.
func (codex) asHistory(m json.RawMessage) (json.RawMessage, error) {
	if body.Label(m, "type") == "" && body.Label(m, "role") == "" {
		return nil, errors.New("an input item has neither a type nor a role")
	}
	return m, nil
}

// reply: the reply's output items, each as the vendor sent it. One that
// asked for tools gets an output for each call, marked as not finished; one
// that compacted the conversation cannot be replayed; one that says the turn
// goes on (end_turn false, on the response, which the kept reply's stop
// reason carries, or on an output message) did not finish, as Claude's
// tool_use or pause_turn does not.
func (codex) reply(rep body.Reply, _ string) ([]json.RawMessage, ending, error) {
	items, ok := body.Elements(rep.Message)
	if !ok || len(items) == 0 {
		return nil, ending{}, errors.New("the kept reply is not a list of items")
	}
	outputs, asked, err := openai.Unanswered(items, notFinished)
	if err != nil {
		return nil, ending{}, err
	}
	for _, it := range items {
		if openai.IsCompaction(it) {
			return nil, ending{}, errors.New("the reply compacted the conversation, and a compacted history cannot be replayed")
		}
	}
	switch {
	case asked:
		return items, ending{asked: true, results: outputs}, nil
	case rep.Stop != "completed" || openai.Continues(items):
		return items, ending{}, nil
	}
	return items, ending{finished: true}, nil
}

// canEnd: the note is a developer message, and the Responses API keeps no
// turn order, so a history may end with any item: an assistant message or a
// reasoning item (a continuation request after an end_turn false reply ends
// with the model's own item), a tool output, or a message. One thing is
// refused: a request whose client tool call has no output in it. codex
// never sends that, and the API answers a call without its result with an
// error, so it is a layout the gateway does not know. A kept reply's calls
// get their outputs from reply.
func (codex) canEnd(items []json.RawMessage, kept bool) bool {
	if kept {
		return true
	}
	return openai.AllAnswered(items)
}

func (codex) prompt(text string) json.RawMessage { return openai.UserMessage(text) }

// note is a developer message: the Responses API has no turn order to keep,
// and a developer message is never taken for the model's own words.
func (codex) note(text string) json.RawMessage { return openai.DeveloperMessage(text) }

// refusal is an OpenAI error. codex-cli does not retry a 400.
func (codex) refusal(msg string) *gateway.Refusal {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg,
		"type": "invalid_request_error", "code": "agentbus_replay_failed"}})
	return &gateway.Refusal{Status: 400, ContentType: "application/json", Body: b}
}

func (codex) parse(data []byte) (request, bool) {
	fields, err := body.Fields(data)
	if err != nil {
		return request{}, false
	}
	var input json.RawMessage
	tools := 0
	for _, f := range fields {
		switch f.Key {
		case "input":
			input = f.Value
		case "tools":
			els, _ := body.Elements(f.Value)
			tools += len(els)
		}
	}
	items, ok := body.Elements(input)
	if !ok {
		return request{}, false
	}
	for _, it := range items {
		if body.Label(it, "type") == "additional_tools" {
			tools++
		}
	}
	return request{fields: fields, msgs: items, tools: tools, id: threadOf(fields)}, true
}

// promptAt is the index of the request's first prompt item, or -1.
func promptAt(items []json.RawMessage) int {
	for i, it := range items {
		if openai.IsPrompt(it) {
			return i
		}
	}
	return -1
}

// fresh: context items only, then the turn's prompt as the first user.text
// item, and no compaction anywhere. Cut by item kinds, never by text: the
// text is only compared with the turn's prompt.
func (c codex) fresh(req request, prompt string) bool {
	p := promptAt(req.msgs)
	if p < 0 {
		return false
	}
	for _, it := range req.msgs[:p] {
		if _, _, ok := openai.ContextKey(it); !ok {
			return false
		}
	}
	if s, ok := openai.PromptText(req.msgs[p]); !ok || !samePrompt(s, prompt) {
		return false
	}
	for _, it := range req.msgs {
		if openai.IsCompaction(it) {
			return false
		}
	}
	return true
}

func (codex) freshRule() string {
	return "(its thread) does not carry codex's context items, then the turn's prompt as its first user.text item, with no compaction"
}

// build: the history, then the fresh request's items from its prompt on: the
// prompt and this turn's tool loop. The fresh request's context items are
// already in the history, from the session's first turn, and are dropped,
// except one that differs from the history's newest item of its kind (a
// changed sandbox, folder or AGENTS.md): it goes right before the prompt, so
// the model reads the context the agent runs in now. A history with no
// context at all (no earlier turn reached the model) gets the fresh context
// first, as a fresh process sends it.
//
// Then the fresh thread id becomes the history's, everywhere but the input
// and in every header, so the vendor reads the history from cache.
func (codex) build(req request, h *history, _ string) ([]byte, func(http.Header), error) {
	p := promptAt(req.msgs)
	ctx, tail := req.msgs[:p], req.msgs[p:]
	newest := map[string]json.RawMessage{}
	for _, it := range h.messages {
		if k, content, ok := openai.ContextKey(it); ok {
			newest[k] = content
		}
	}
	var items []json.RawMessage
	if len(newest) == 0 {
		items = append(items, ctx...)
		items = append(items, h.messages...)
	} else {
		items = append(items, h.messages...)
		for _, it := range ctx {
			k, content, _ := openai.ContextKey(it)
			if old, ok := newest[k]; !ok || !bytes.Equal(old, content) {
				items = append(items, it)
			}
		}
	}
	items = append(items, tail...)

	fresh, okFresh := openai.ThreadID(req.fields)
	rename := okFresh && h.thread != "" && fresh != h.thread
	for i, f := range req.fields {
		switch {
		case f.Key == "input":
			req.fields[i].Value = joinArray(items)
		case rename:
			req.fields[i].Value = bytes.ReplaceAll(f.Value, []byte(fresh), []byte(h.thread))
		}
	}
	var hdr func(http.Header)
	if rename {
		old := h.thread
		hdr = func(hd http.Header) {
			for _, vs := range hd {
				for i, v := range vs {
					vs[i] = strings.ReplaceAll(v, fresh, old)
				}
			}
		}
	}
	return body.Object(req.fields), hdr, nil
}
