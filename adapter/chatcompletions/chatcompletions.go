// Package chatcompletions reads the OpenAI Chat Completions format,
// POST /v1/chat/completions, as local servers (llama.cpp's llama-server,
// Ollama, LM Studio) and many OpenAI-compatible vendors speak it.
//
// The fixtures in testdata are llama.cpp llama-server b12578 traffic
// (Qwen2.5-0.5B-Instruct, --jinja), captured 2026-11-20. What they showed,
// which this file relies on:
//   - a streamed reply is "data: {chunk}" lines ending in "data: [DONE]"; a
//     chunk's choices[].delta carries the text, and a tool call's id and name
//     come in its first delta, its arguments as JSON text split over later
//     deltas of the same tool_calls[].index;
//   - finish_reason arrives on its own chunk with an empty delta: "stop",
//     "tool_calls" or "length";
//   - with stream_options.include_usage the usage comes on one more chunk
//     after it, with an empty choices list; prompt_tokens include
//     prompt_tokens_details.cached_tokens, as in OpenAI's reference;
//   - a whole reply puts the same in choices[].message, and an assistant
//     message that calls a tool has content "".
package chatcompletions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

type Adapter struct{}

func (Adapter) Vendor() string { return "chatcompletions" }

// Calls: the model call. OpenAI, llama-server, Ollama and LM Studio all take
// it at /v1/chat/completions with no query; it streams when the request says
// "stream": true, and replies with one JSON object otherwise.
func (Adapter) Calls() []adapter.Call {
	return []adapter.Call{
		{Method: "POST", Path: "/v1/chat/completions", Replies: adapter.JSONOrSSEOrNone},
	}
}

// message is one entry of a request's messages.
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // a string, a list of parts, or null
	// ToolCallID ties a tool message to the call it answers.
	ToolCallID string     `json:"tool_call_id"`
	ToolCalls  []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is JSON text inside a string.
		Arguments string `json:"arguments"`
	} `json:"function"`
	// Custom is a custom tool's call: free text, not JSON (OpenAI's
	// reference, type "custom").
	Custom *struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"custom"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
	Custom struct {
		Name string `json:"name"`
	} `json:"custom"`
}

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools"`
	// Functions is the older way to offer tools, still documented.
	Functions []struct {
		Name string `json:"name"`
	} `json:"functions"`
}

type part struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// text reads a content field: a string, or a list of parts whose text and
// refusal parts are joined with sep. Other parts (image_url, input_audio,
// file) are kept as their JSON after the text, so nothing is lost.
func text(raw json.RawMessage, sep string) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	}
	var ps []json.RawMessage
	if err := json.Unmarshal(raw, &ps); err != nil {
		return "", err
	}
	var texts, other []string
	for _, r := range ps {
		var p part
		if err := json.Unmarshal(r, &p); err != nil {
			return "", err
		}
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "refusal":
			texts = append(texts, p.Refusal)
		default:
			other = append(other, string(r))
		}
	}
	out := strings.Join(texts, sep)
	if len(other) > 0 {
		if out != "" {
			out += "\n"
		}
		out += strings.Join(other, "\n")
	}
	return out, nil
}

// turnStart is the one rule for where a turn starts, used by RequestEvents
// and TurnBasis alike: start is the index of the last user message, -1 if
// there is none, and starts says it comes after the last assistant message,
// so this request starts that turn. System and developer messages never hide
// it.
func turnStart(ms []message) (start int, starts bool) {
	start, lastModel := -1, -1
	for i, m := range ms {
		switch m.Role {
		case "user":
			start = i
		case "assistant":
			lastModel = i
		}
	}
	return start, start > lastModel
}

func toolNames(req request) []string {
	names := []string{}
	for _, t := range req.Tools {
		switch {
		case t.Function.Name != "":
			names = append(names, t.Function.Name)
		case t.Custom.Name != "":
			names = append(names, t.Custom.Name)
		case t.Type != "":
			names = append(names, t.Type)
		}
	}
	for _, f := range req.Functions {
		if f.Name != "" {
			names = append(names, f.Name)
		}
	}
	return names
}

// RequestEvents: a request whose last user message comes after the last
// assistant message starts a turn. A request ending in tool messages
// continues one; only the tool messages after the last assistant message are
// new, the earlier ones were reported by an earlier request.
func (Adapter) RequestEvents(body []byte) ([]event.Event, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("chat completions request: %w", err)
	}
	var evs []event.Event
	first := len(req.Messages)
	for first > 0 && req.Messages[first-1].Role != "assistant" {
		first--
	}
	for _, m := range req.Messages[first:] {
		if m.Role != "tool" {
			continue
		}
		out, err := text(m.Content, "")
		if err != nil {
			out = string(m.Content)
		}
		// A tool message has no error flag, so ToolError stays nil: unknown.
		evs = append(evs, event.Event{Kind: event.ToolCompleted, ToolID: m.ToolCallID, ToolOutput: out})
	}
	if start, starts := turnStart(req.Messages); starts {
		prompt, err := text(req.Messages[start].Content, "\n")
		if err != nil {
			return nil, fmt.Errorf("chat completions request content: %w", err)
		}
		evs = append(evs, event.Event{Kind: event.TurnStarted, Text: prompt})
	}
	evs = append(evs, event.Event{Kind: event.ModelRequested, Model: req.Model, ToolsOffered: toolNames(req)})
	var u adapter.Unknowns
	for _, m := range req.Messages {
		checkName(&u, knownRoles, "request.messages[].role", m.Role)
	}
	return append(evs, u.Events()...), nil
}

// TurnBasis is the messages up to and including the user message that
// started the turn, each re-encoded with sorted keys and no spacing. In the
// captures the request that sent the tool result repeated the turn's first
// messages unchanged, so nothing is removed.
func (Adapter) TurnBasis(body []byte) []byte {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	msgs := make([]message, len(req.Messages))
	for i, raw := range req.Messages {
		if err := json.Unmarshal(raw, &msgs[i]); err != nil {
			return nil
		}
	}
	start, _ := turnStart(msgs)
	if start < 0 {
		return nil
	}
	ms := make([]any, 0, start+1)
	for _, raw := range req.Messages[:start+1] {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		ms = append(ms, v)
	}
	out, err := json.Marshal(ms)
	if err != nil {
		return nil
	}
	return out
}

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// Cost is OpenRouter's charge for the call, in US dollars.
	Cost    *float64 `json:"cost"`
	unknown []event.Unexpected
}

// UnmarshalJSON reads usage and notes any field name the known lists do not
// hold, at the top and in the two details objects.
func (u *usage) UnmarshalJSON(b []byte) error {
	type plain usage
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*u = usage(p)
	u.unknown = adapter.UnknownFields(b, "usage", knownUsage, nil)
	var raw struct {
		In  json.RawMessage `json:"prompt_tokens_details"`
		Out json.RawMessage `json:"completion_tokens_details"`
	}
	if json.Unmarshal(b, &raw) == nil {
		if len(raw.In) > 0 {
			u.unknown = adapter.UnknownFields(raw.In, "usage.prompt_tokens_details", knownPromptDetails, u.unknown)
		}
		if len(raw.Out) > 0 {
			u.unknown = adapter.UnknownFields(raw.Out, "usage.completion_tokens_details", knownCompletionDetails, u.unknown)
		}
	}
	return nil
}

// The names this adapter knows. Anything else becomes a schema.unexpected
// event. "Seen" means in the llama-server captures; "documented" means named
// in OpenAI's Chat Completions reference (openai-python 2.46.0's
// types/chat), unseen here.
var (
	// knownRoles: system, user, assistant and tool are seen; developer and
	// function (the older tool result) are documented.
	knownRoles = adapter.Names("system", "developer", "user", "assistant", "tool", "function")
	// knownFinish: stop, tool_calls and length are seen; content_filter and
	// function_call are documented.
	knownFinish = adapter.Names("stop", "length", "tool_calls", "content_filter", "function_call")
	// knownUsage: the three counts and prompt_tokens_details are seen;
	// completion_tokens_details is documented; cost, cost_details and
	// is_byok are OpenRouter's.
	knownUsage = adapter.Names("prompt_tokens", "completion_tokens", "total_tokens",
		"prompt_tokens_details", "completion_tokens_details", "cost", "cost_details", "is_byok")
	knownPromptDetails     = adapter.Names("cached_tokens", "audio_tokens")
	knownCompletionDetails = adapter.Names("reasoning_tokens", "audio_tokens",
		"accepted_prediction_tokens", "rejected_prediction_tokens")
)

func checkName(u *adapter.Unknowns, known adapter.Set, where, name string) {
	if !known.Has(name) {
		u.Value(where, name)
	}
}

// parsed is what a reply comes to, whole or rebuilt from a stream.
type parsed struct {
	model       string
	text        string
	refusal     string
	finish      string
	calls       []toolCall
	usage       *usage
	serviceTier string
}

// maxError bounds a vendor error message kept in an event.
const maxError = 1024

// shortError keeps at most maxError bytes, cut on a rune boundary, as a copy.
func shortError(msg string) string {
	if len(msg) > maxError {
		msg = strings.ToValidUTF8(msg[:maxError], "") + " (cut)"
	}
	return strings.Clone(msg)
}

// isStream decides whether a reply is server-sent events: by its type, or,
// with no type, by its first line.
func isStream(contentType string, body []byte) bool {
	if strings.HasPrefix(contentType, "text/event-stream") {
		return true
	}
	if contentType != "" {
		return false
	}
	b := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(b, []byte("event:")) || bytes.HasPrefix(b, []byte("data:")) || bytes.HasPrefix(b, []byte(":"))
}

// errorText reads an error body: OpenAI's and llama-server's
// {"error": {"message": ...}}, AgentBus's own relay's {"error": "<text>"},
// or the body as it is.
func errorText(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	var own struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	} else if json.Unmarshal(body, &own) == nil && own.Error != "" {
		msg = own.Error
	}
	return shortError(msg)
}

func (Adapter) ResponseEvents(status int, contentType string, body []byte) ([]event.Event, error) {
	if status != 200 {
		return []event.Event{{Kind: event.ModelResponded, Status: status, Error: errorText(body)}}, nil
	}
	if isStream(contentType, body) {
		return readStream(body)
	}
	var whole struct {
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content   json.RawMessage `json:"content"`
				Refusal   string          `json:"refusal"`
				ToolCalls []toolCall      `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage       *usage `json:"usage"`
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(body, &whole); err != nil {
		return nil, fmt.Errorf("chat completions reply: %w", err)
	}
	r := parsed{model: whole.Model, usage: whole.Usage, serviceTier: whole.ServiceTier}
	problem := "the reply has no choices"
	for _, c := range whole.Choices {
		// An agent asks for one choice; with more, the first is read.
		if c.Index != 0 {
			continue
		}
		problem = ""
		s, err := text(c.Message.Content, "")
		if err != nil {
			return nil, fmt.Errorf("chat completions reply content: %w", err)
		}
		r.text, r.refusal, r.calls, r.finish = s, c.Message.Refusal, c.Message.ToolCalls, c.FinishReason
	}
	var u adapter.Unknowns
	evs := fromReply(r, problem, &u)
	return append(evs, u.Events()...), nil
}

// fromReply turns a reply into events. A reply with a problem reports only
// model.responded: what it asked for and how it ended cannot be trusted.
func fromReply(r parsed, problem string, u *adapter.Unknowns) []event.Event {
	if problem == "" && r.finish == "" {
		problem = "the reply has no finish_reason"
	}
	if r.finish != "" {
		checkName(u, knownFinish, "choices[].finish_reason", r.finish)
	}
	resp := event.Event{
		Kind:       event.ModelResponded,
		Model:      r.model,
		Text:       r.text,
		StopReason: r.finish,
		Status:     200,
		Error:      problem,
	}
	if r.refusal != "" && resp.Text == "" {
		resp.Text = r.refusal
	}
	// A reply without usage keeps Usage nil: absent is not zero. The prompt
	// tokens include the cached ones; the framework's input tokens do not.
	if x := r.usage; x != nil {
		resp.Usage = &event.Usage{
			InputTokens:     x.PromptTokens - x.PromptTokensDetails.CachedTokens,
			OutputTokens:    x.CompletionTokens,
			CacheReadTokens: x.PromptTokensDetails.CachedTokens,
			ServiceTier:     r.serviceTier,
			CostUSD:         x.Cost,
		}
		for _, x := range x.unknown {
			u.Admit(x)
		}
	}
	evs := []event.Event{resp}
	if problem != "" {
		return evs
	}
	for _, c := range r.calls {
		evs = append(evs, toolRequested(c))
	}
	if outcome, ends := turnEnd(r); ends {
		evs = append(evs, event.Event{Kind: event.TurnCompleted, Outcome: outcome, StopReason: r.finish})
	}
	return evs
}

func toolRequested(c toolCall) event.Event {
	if c.Custom != nil {
		// A custom tool takes free text: it is kept as a JSON string.
		e := event.Event{Kind: event.ToolRequested, ToolID: c.ID, ToolName: c.Custom.Name}
		e.ToolInput, _ = json.Marshal(c.Custom.Input)
		return e
	}
	e := event.Event{Kind: event.ToolRequested, ToolID: c.ID, ToolName: c.Function.Name}
	args := strings.TrimSpace(c.Function.Arguments)
	switch {
	case args == "":
	case json.Valid([]byte(args)):
		e.ToolInput = json.RawMessage(args)
	default:
		e.Error = "the tool input is not valid JSON"
	}
	return e
}

// turnEnd maps finish_reason to the framework's outcome. A reply that asks
// for a tool is not the end: the agent runs it and sends the result back.
func turnEnd(r parsed) (event.Outcome, bool) {
	if len(r.calls) > 0 {
		return "", false
	}
	switch r.finish {
	case "tool_calls", "function_call":
		return "", false
	case "stop":
		if r.refusal != "" {
			return event.Refused, true
		}
		return event.Finished, true
	case "length":
		return event.Partial, true
	case "content_filter":
		return event.Refused, true
	}
	return event.Unknown, true
}

// readStream rebuilds a reply from server-sent events. A stream that ends
// before a finish_reason arrived is reported as a problem, with the text it
// had; its usage stays nil unless a usage chunk came.
func readStream(body []byte) ([]event.Event, error) {
	var r parsed
	var u adapter.Unknowns
	var b, refusal strings.Builder
	calls := map[int]*toolCall{}
	streamErr := ""

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}
		var ch struct {
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   *string `json:"content"`
					Refusal   *string `json:"refusal"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage       *usage          `json:"usage"`
			ServiceTier string          `json:"service_tier"`
			Error       json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return u.Events(), fmt.Errorf("chat completions stream chunk: %w", err)
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			streamErr = errorText([]byte(`{"error":` + string(ch.Error) + `}`))
			if streamErr == "" {
				streamErr = "the stream reported an error"
			}
			continue
		}
		if ch.Model != "" {
			r.model = ch.Model
		}
		if ch.ServiceTier != "" {
			r.serviceTier = ch.ServiceTier
		}
		if ch.Usage != nil {
			r.usage = ch.Usage
		}
		for _, c := range ch.Choices {
			if c.Index != 0 {
				continue
			}
			if c.Delta.Content != nil {
				b.WriteString(*c.Delta.Content)
			}
			if c.Delta.Refusal != nil {
				refusal.WriteString(*c.Delta.Refusal)
			}
			for _, d := range c.Delta.ToolCalls {
				tc, ok := calls[d.Index]
				if !ok {
					tc = &toolCall{}
					calls[d.Index] = tc
				}
				if d.ID != "" {
					tc.ID = d.ID
				}
				if d.Type != "" {
					tc.Type = d.Type
				}
				if d.Function.Name != "" {
					tc.Function.Name = d.Function.Name
				}
				tc.Function.Arguments += d.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				r.finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return u.Events(), fmt.Errorf("chat completions stream: %w", err)
	}
	r.text, r.refusal = b.String(), refusal.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		r.calls = append(r.calls, *calls[i])
	}
	problem := streamErr
	if problem == "" && r.finish == "" {
		problem = "the stream ended before the reply finished"
	}
	evs := fromReply(r, problem, &u)
	return append(evs, u.Events()...), nil
}
