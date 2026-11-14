// Package anthropic reads the Anthropic Messages API.
package anthropic

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

func (Adapter) Vendor() string { return "anthropic" }

// Calls: the Messages API's model call, and HEAD /api/hello. Claude Code
// 2.1.323 posts the model call as /v1/messages?beta=true, through a base URL
// and on a setup-token alike (measured 2026-11-13 against a local listener).
func (Adapter) Calls() []adapter.Call {
	return []adapter.Call{{Method: "POST", Path: "/v1/messages", Query: map[string][]string{"beta": {"true"}},
		Replies: adapter.JSONOrSSE},
		// Claude Code 2.1.x sends HEAD /api/hello, with no credential, once
		// a turn (2026-11-13). Anthropic answers it 200, application/json,
		// without one (checked 2026-11-14).
		{Method: "HEAD", Path: "/api/hello", Replies: adapter.JSONOnly, Service: true, NoCredential: true}}
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// CacheCreation splits CacheCreationInputTokens by cache lifetime. Each
	// lifetime has its own price. Claude Code 2.1.318 writes one-hour cache
	// (seen 2026-11-05). Nil when the vendor sent no split.
	CacheCreation *cacheCreation `json:"cache_creation"`
	// ServiceTier and Speed say how the call was served: fast mode reports
	// speed "fast". Either may be absent.
	ServiceTier string `json:"service_tier"`
	Speed       string `json:"speed"`
	// InferenceGeo says where the call ran: "global" on Claude Code 2.1.318
	// replies (seen 2026-11-05, in message_start's usage), "us" for data
	// residency.
	InferenceGeo string `json:"inference_geo"`
	// ServerToolUse counts the tools Anthropic ran itself, such as
	// web_search_requests. Kept whole: new counts appear as new tools do.
	ServerToolUse map[string]int `json:"server_tool_use"`
	// Cost is OpenRouter's charge for the call, in US dollars (its credits).
	// cost_details next to it splits the upstream cost and is not read.
	// Seen 2026-11-13 on OpenRouter replies (TODO fw 0.5.6).
	Cost             *float64 `json:"cost"`
	outputIncomplete bool
	// unknown holds the field names this adapter does not know, for
	// schema.unexpected.
	unknown []event.Unexpected
}

// UnmarshalJSON reads usage and notes any field name not in knownUsage, and
// any cache lifetime not in knownCacheCreation.
func (u *usage) UnmarshalJSON(b []byte) error {
	type plain usage
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*u = usage(p)
	u.unknown = adapter.UnknownFields(b, "usage", knownUsage, nil)
	var raw struct {
		CacheCreation json.RawMessage `json:"cache_creation"`
	}
	if json.Unmarshal(b, &raw) == nil && len(raw.CacheCreation) > 0 {
		u.unknown = adapter.UnknownFields(raw.CacheCreation, "usage.cache_creation", knownCacheCreation, u.unknown)
	}
	return nil
}

type cacheCreation struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

// The names this adapter knows. Anything else the vendor sends in these places
// becomes a schema.unexpected event, so a new harness or API release is
// noticed. "Seen" means in Claude Code 2.1.318 traffic through the gateway on
// 2026-11-05 (three /v1/messages request bodies and the streamed replies of
// the usage probe). "Read" means a case this adapter's code handles.
// "Documented" means named in Anthropic's Messages API reference, unseen here;
// they are listed so ordinary use of a documented feature reports nothing.
var (
	knownBlocks = adapter.Names(
		// seen; tool_addition and tool_removal ride in Claude Code's trailing
		// system message
		"text", "thinking", "tool_use", "tool_result", "tool_addition", "tool_removal",
		// documented
		"image", "document", "search_result", "redacted_thinking",
		"server_tool_use", "web_search_tool_result", "web_fetch_tool_result",
		"code_execution_tool_result", "bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result", "tool_search_tool_result",
		"mcp_tool_use", "mcp_tool_result", "container_upload")
	// knownStops: the stop reasons turnEnd maps.
	knownStops = adapter.Names("end_turn", "stop_sequence", "max_tokens", "refusal", "tool_use", "pause_turn")
	// knownStreamEvents: read, plus content_block_stop and ping (seen,
	// ignored).
	knownStreamEvents = adapter.Names("message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop", "ping", "error")
	// knownDeltas: text_delta and input_json_delta are read; thinking_delta
	// and signature_delta are seen; citations_delta is documented.
	knownDeltas = adapter.Names("text_delta", "input_json_delta", "thinking_delta", "signature_delta", "citations_delta")
	// knownRoles in a request: system is Claude Code's trailing message (seen).
	knownRoles = adapter.Names("user", "assistant", "system")
	// knownReplyRoles: a reply's message role.
	knownReplyRoles = adapter.Names("assistant")
	// knownUsage: seen in message_start and message_delta, plus
	// server_tool_use (read). iterations and output_tokens_details are seen
	// and not read.
	knownUsage = adapter.Names("input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "cache_creation", "service_tier", "speed", "inference_geo",
		"server_tool_use", "iterations", "output_tokens_details", "cost", "cost_details")
	knownCacheCreation = adapter.Names("ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")
)

// blockTypes reads only the type of each block in a content field. A string
// content has none to check.
func blockTypes(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var bs []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &bs) != nil {
		return nil
	}
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Type
	}
	return out
}

func checkName(u *adapter.Unknowns, known adapter.Set, where, name string) {
	if !known.Has(name) {
		u.Value(where, name)
	}
}

func checkUsage(u *adapter.Unknowns, us *usage) {
	if us == nil {
		return
	}
	for _, x := range us.unknown {
		u.Admit(x)
	}
}

type reply struct {
	Model      string          `json:"model"`
	Role       json.RawMessage `json:"role"` // read only to report an unknown role, so kept raw
	Content    []block         `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      *usage          `json:"usage"` // nil when the vendor sent none
}

// blocks reads a content field, which is either a string or a list of blocks.
func blocks(raw json.RawMessage) ([]block, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []block{{Type: "text", Text: s}}, nil
	}
	var bs []block
	err := json.Unmarshal(raw, &bs)
	return bs, err
}

func joinText(bs []block) string {
	var parts []string
	for _, b := range bs {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// lastTurnMessage is the index of the message that decides whether a request
// starts a turn, or -1 if there is none.
// It skips trailing system messages: Claude Code 2.1.318 sends one after the
// user's message, carrying reminders and tool additions (seen through the
// gateway, 2026-11-04). It also skips an assistant prefill: a final assistant
// message of text only, which the model is asked to continue. A final assistant
// message with tool blocks is not a prefill but a paused turn being resumed
// after pause_turn, so it is returned as is and starts nothing.
func lastTurnMessage(msgs []message) int {
	skippedPrefill := false
	for i := len(msgs) - 1; i >= 0; i-- {
		switch {
		case msgs[i].Role == "system":
		case msgs[i].Role == "assistant" && !skippedPrefill && textOnly(msgs[i].Content):
			skippedPrefill = true
		default:
			return i
		}
	}
	return -1
}

// turnStart is the index of the message that started the request's turn: the
// last message holding the person's words, which is a user message with no
// tool results, at or before lastTurnMessage. It is -1 if there is none.
func turnStart(msgs []message) int {
	for i := lastTurnMessage(msgs); i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		bs, err := blocks(msgs[i].Content)
		if err != nil {
			return -1
		}
		if !hasToolResult(bs) {
			return i
		}
	}
	return -1
}

func hasToolResult(bs []block) bool {
	for _, b := range bs {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// quotaCheck reports whether a request is Claude Code's startup quota check.
// Interactive Claude Code 2.1.318 sends it once at startup, unstreamed, with
// exactly these top-level fields: model, max_tokens 1, metadata, and one user
// message whose content is the string "quota" (captured through a pass-through
// proxy, 2026-11-05). It is the harness's own check, not a person's request.
// Anything more, such as a system prompt, tools, a stream field, a larger
// max_tokens or content as blocks, is a person's message and starts a turn.
func quotaCheck(body []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return false
	}
	for k := range fields {
		switch k {
		case "model", "max_tokens", "messages", "metadata":
		default:
			return false
		}
	}
	if string(bytes.TrimSpace(fields["max_tokens"])) != "1" {
		return false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(fields["messages"], &msgs) != nil || len(msgs) != 1 || len(msgs[0]) != 2 {
		return false
	}
	var role, content string
	return json.Unmarshal(msgs[0]["role"], &role) == nil && role == "user" &&
		json.Unmarshal(msgs[0]["content"], &content) == nil && content == "quota"
}

// TurnBasis is the conversation up to and including the message that started
// the turn. Each message is its role and every block in full, re-encoded so
// key order and spacing do not matter. Two things are made uniform:
//   - cache_control is removed from content blocks: the harness moves it
//     between requests of one conversation.
//   - string content becomes one text block, for a message and for a
//     tool_result alike: Claude Code 2.1.318 sent one
//     system message as blocks in one request and as a string in the next
//     (seen through the gateway, 2026-11-05).
func (Adapter) TurnBasis(body []byte) []byte {
	if quotaCheck(body) {
		return nil
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	start := turnStart(req.Messages)
	if start < 0 {
		return nil
	}
	type canon struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	conv := make([]canon, 0, start+1)
	for _, m := range req.Messages[:start+1] {
		content, err := canonContent(m.Content)
		if err != nil {
			return nil
		}
		conv = append(conv, canon{Role: m.Role, Content: content})
	}
	out, err := json.Marshal(conv)
	if err != nil {
		return nil
	}
	return out
}

// canonContent decodes a content field with numbers kept as written, then
// makes it uniform with canonBlocks. json.Marshal then writes map keys in
// sorted order and no spacing.
func canonContent(raw json.RawMessage) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return canonBlocks(v), nil
}

// canonBlocks makes a content value uniform, only where the Messages API
// defines the shape:
//   - a string becomes one text block (a message's content, or a
//     tool_result's content, may be either form);
//   - cache_control is removed from each content block, and from the blocks
//     inside a tool_result's content, which is where the API puts it;
//   - nothing else is touched. A tool_use input is the application's data, so
//     a key named cache_control inside it stays.
func canonBlocks(v any) any {
	switch t := v.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": t}}
	case []any:
		for _, x := range t {
			b, ok := x.(map[string]any)
			if !ok {
				continue
			}
			delete(b, "cache_control")
			if b["type"] == "tool_result" {
				if c, ok := b["content"]; ok && c != nil {
					b["content"] = canonBlocks(c)
				}
			}
		}
	}
	return v
}

// personsWords keeps the text blocks the person typed. Claude Code 2.1.318
// puts context into the person's message as separate text blocks, each one
// whole <system-reminder>…</system-reminder> (the user's email, git status,
// attribution guidance), before the block with the person's words (seen
// through the gateway, 2026-11-05). A block made only of reminders is dropped;
// any other block is kept unchanged.
func personsWords(bs []block) string {
	var kept []block
	for _, b := range bs {
		if b.Type == "text" && onlyReminders(b.Text) {
			continue
		}
		kept = append(kept, b)
	}
	return joinText(kept)
}

// onlyReminders reports whether s is one or more whole system-reminder
// blocks with nothing but spacing around them.
func onlyReminders(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for s != "" {
		rest, ok := strings.CutPrefix(s, "<system-reminder>")
		if !ok {
			return false
		}
		_, after, ok := strings.Cut(rest, "</system-reminder>")
		if !ok {
			return false
		}
		s = strings.TrimSpace(after)
	}
	return true
}

func textOnly(raw json.RawMessage) bool {
	bs, err := blocks(raw)
	if err != nil {
		return false
	}
	for _, b := range bs {
		if b.Type != "text" {
			return false
		}
	}
	return true
}

// RequestEvents: a request whose last message is a person's words starts a
// turn. A request whose last message carries tool results continues one.
// Claude Code's quota check starts nothing; its model.requested is marked as
// a probe.
func (Adapter) RequestEvents(body []byte) ([]event.Event, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if quotaCheck(body) {
		return []event.Event{{Kind: event.ModelRequested, Model: req.Model, ToolsOffered: []string{}, Probe: true}}, nil
	}
	var evs []event.Event
	if i := lastTurnMessage(req.Messages); i >= 0 && req.Messages[i].Role == "user" {
		bs, err := blocks(req.Messages[i].Content)
		if err != nil {
			return nil, fmt.Errorf("anthropic request content: %w", err)
		}
		results := 0
		for _, b := range bs {
			if b.Type != "tool_result" {
				continue
			}
			results++
			out, err := blocks(b.Content)
			if err != nil {
				return nil, fmt.Errorf("anthropic tool_result content: %w", err)
			}
			isErr := b.IsError
			evs = append(evs, event.Event{
				Kind:       event.ToolCompleted,
				ToolID:     b.ToolUseID,
				ToolError:  &isErr,
				ToolOutput: joinText(out),
			})
		}
		if results == 0 {
			evs = append(evs, event.Event{Kind: event.TurnStarted, Text: personsWords(bs)})
		}
	}
	tools := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, t.Name)
	}
	evs = append(evs, event.Event{Kind: event.ModelRequested, Model: req.Model, ToolsOffered: tools})
	var u adapter.Unknowns
	for _, m := range req.Messages {
		checkName(&u, knownRoles, "request.messages[].role", m.Role)
		for _, t := range blockTypes(m.Content) {
			checkName(&u, knownBlocks, "request.messages[].content[].type", t)
		}
	}
	return append(evs, u.Events()...), nil
}

// maxError bounds a vendor error message kept in an event.
const maxError = 1024

// shortError keeps at most maxError bytes of a vendor error, cut on a rune
// boundary, as a fresh copy: a substring would keep the whole reply in memory.
func shortError(msg string) string {
	if len(msg) > maxError {
		msg = strings.ToValidUTF8(msg[:maxError], "") + " (cut)"
	}
	return strings.Clone(msg)
}

func (Adapter) ResponseEvents(status int, contentType string, body []byte) ([]event.Event, error) {
	if status != 200 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		// AgentBus's own relay refuses as {"error": "<text>"}.
		var own struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		} else if json.Unmarshal(body, &own) == nil && own.Error != "" {
			msg = own.Error
		}
		return []event.Event{{Kind: event.ModelResponded, Status: status, Error: shortError(msg)}}, nil
	}
	var r reply
	var problem string
	var u adapter.Unknowns
	if strings.HasPrefix(contentType, "text/event-stream") {
		var err error
		r, problem, err = readStream(body, &u)
		if err != nil {
			// The names seen before the error are still reported.
			return u.Events(), err
		}
	} else {
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("anthropic reply: %w", err)
		}
		if role, ok := adapter.String(r.Role); ok && role != "" {
			checkName(&u, knownReplyRoles, "response.role", role)
		}
		for _, b := range r.Content {
			checkName(&u, knownBlocks, "response.content[].type", b.Type)
		}
		checkUsage(&u, r.Usage)
	}
	if r.StopReason != "" {
		checkName(&u, knownStops, "response.stop_reason", r.StopReason)
	}
	if problem == "" && r.StopReason == "" {
		problem = "the reply ended without a stop reason"
	}
	return append(fromReply(r, status, problem), u.Events()...), nil
}

// fromReply turns a reply into events. A reply with a problem reports only
// model.responded: what it asked for and how it ended cannot be trusted.
func fromReply(r reply, status int, problem string) []event.Event {
	resp := event.Event{
		Kind:       event.ModelResponded,
		Model:      r.Model,
		Text:       joinText(r.Content),
		StopReason: r.StopReason,
		Status:     status,
		Error:      problem,
	}
	// A reply without usage keeps Usage nil: absent is not the same as zero,
	// and a cost built on zeros would claim the call was free.
	if u := r.Usage; u != nil {
		resp.Usage = &event.Usage{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
			OutputIncomplete: u.outputIncomplete,
			ServiceTier:      u.ServiceTier,
			Speed:            u.Speed,
			InferenceGeo:     u.InferenceGeo,
			ServerToolUse:    u.ServerToolUse,
			CostUSD:          u.Cost,
		}
		if cc := u.CacheCreation; cc != nil {
			resp.Usage.CacheWrite5mTokens = cc.Ephemeral5m
			resp.Usage.CacheWrite1hTokens = cc.Ephemeral1h
		}
	}
	evs := []event.Event{resp}
	if problem != "" {
		return evs
	}
	for _, b := range r.Content {
		if b.Type != "tool_use" {
			continue
		}
		e := event.Event{Kind: event.ToolRequested, ToolID: b.ID, ToolName: b.Name}
		if len(b.Input) == 0 || json.Valid(b.Input) {
			e.ToolInput = b.Input
		} else {
			e.Error = "the tool input is not valid JSON"
		}
		evs = append(evs, e)
	}
	if outcome, ends := turnEnd(r.StopReason); ends {
		evs = append(evs, event.Event{Kind: event.TurnCompleted, Outcome: outcome, StopReason: r.StopReason})
	}
	return evs
}

// turnEnd maps the vendor's stop reason to the framework's outcome.
// tool_use and pause_turn mean the harness will call the model again.
// An empty stop reason means the reply was cut off, so nothing is claimed.
func turnEnd(stop string) (event.Outcome, bool) {
	switch stop {
	case "", "tool_use", "pause_turn":
		return "", false
	case "end_turn", "stop_sequence":
		return event.Finished, true
	case "max_tokens":
		return event.Partial, true
	case "refusal":
		return event.Refused, true
	default:
		return event.Unknown, true
	}
}

// readStream rebuilds a reply from a server-sent event stream. The second
// result names a problem: an error event, or a stream that ended before
// message_stop. Names it does not know go to u.
//
// The real output count arrives only in message_delta. message_start carries a
// placeholder of 1 to 3 tokens, so a stream that ends before message_delta
// marks its output count incomplete rather than passing the placeholder off as
// the count. LiteLLM re-counts the text with a tokenizer instead (its
// anthropic_passthrough_logging_handler.py, read 2026-11-05); we have no
// Claude tokenizer, so we report a lower bound and keep the partial text.
func readStream(body []byte, u *adapter.Unknowns) (reply, string, error) {
	var r reply
	var streamErr string
	stopped := false
	finalCount := false
	type partial struct {
		b    block
		text strings.Builder
		json strings.Builder
	}
	parts := map[int]*partial{}
	at := func(i int) *partial {
		p, ok := parts[i]
		if !ok {
			p = &partial{}
			parts[i] = p
		}
		return p
	}

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		// message_stop ends the reply. What follows is not read: a client
		// that stops reading there leaves a partial line behind.
		if stopped {
			break
		}
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message reply  `json:"message"`
			Block   block  `json:"content_block"`
			Delta   struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *usage `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			return r, "", fmt.Errorf("anthropic stream event: %w", err)
		}
		if (ev.Type == "content_block_start" || ev.Type == "content_block_delta") && ev.Index < 0 {
			return r, "", fmt.Errorf("anthropic stream: a content block with index %d", ev.Index)
		}
		checkName(u, knownStreamEvents, "stream.event", ev.Type)
		checkUsage(u, ev.Usage)
		switch ev.Type {
		case "message_start":
			r.Model = ev.Message.Model
			r.Usage = ev.Message.Usage
			if role, ok := adapter.String(ev.Message.Role); ok && role != "" {
				checkName(u, knownReplyRoles, "response.role", role)
			}
			checkUsage(u, ev.Message.Usage)
		case "content_block_start":
			checkName(u, knownBlocks, "response.content[].type", ev.Block.Type)
			p := at(ev.Index)
			p.b = ev.Block
			p.text.WriteString(ev.Block.Text)
		case "content_block_delta":
			p := at(ev.Index)
			checkName(u, knownDeltas, "stream.delta.type", ev.Delta.Type)
			switch ev.Delta.Type {
			case "text_delta":
				p.text.WriteString(ev.Delta.Text)
			case "input_json_delta":
				p.json.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				r.StopReason = ev.Delta.StopReason
			}
			if u := ev.Usage; u != nil {
				finalCount = true
				if r.Usage == nil {
					r.Usage = &usage{}
				}
				r.Usage.OutputTokens = u.OutputTokens
				if u.InputTokens != 0 {
					r.Usage.InputTokens = u.InputTokens
				}
				if u.CacheReadInputTokens != 0 {
					r.Usage.CacheReadInputTokens = u.CacheReadInputTokens
				}
				if u.CacheCreationInputTokens != 0 {
					r.Usage.CacheCreationInputTokens = u.CacheCreationInputTokens
				}
				// service_tier and speed may arrive here instead of in
				// message_start (LiteLLM's stream assembly reads speed
				// from both).
				if u.ServiceTier != "" {
					r.Usage.ServiceTier = u.ServiceTier
				}
				if u.Speed != "" {
					r.Usage.Speed = u.Speed
				}
				if u.InferenceGeo != "" {
					r.Usage.InferenceGeo = u.InferenceGeo
				}
				// message_delta's server_tool_use is the reply's count
				// (LiteLLM's stream assembly reads it from there).
				if u.ServerToolUse != nil {
					r.Usage.ServerToolUse = u.ServerToolUse
				}
				if u.Cost != nil {
					r.Usage.Cost = u.Cost
				}
				// Like the counts above, a zero split here does not replace
				// message_start's.
				if cc := u.CacheCreation; cc != nil && cc.Ephemeral5m+cc.Ephemeral1h != 0 {
					r.Usage.CacheCreation = cc
				}
			}
		case "message_stop":
			stopped = true
		case "error":
			streamErr = shortError(ev.Error.Message)
			if streamErr == "" {
				streamErr = "the stream reported an error"
			}
		}
	}
	if err := sc.Err(); err != nil {
		return r, "", fmt.Errorf("anthropic stream: %w", err)
	}
	indexes := make([]int, 0, len(parts))
	for i := range parts {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		p := parts[i]
		p.b.Text = p.text.String()
		if p.b.Type == "tool_use" && p.json.Len() > 0 {
			p.b.Input = json.RawMessage(p.json.String())
		}
		r.Content = append(r.Content, p.b)
	}
	if r.Usage != nil {
		r.Usage.outputIncomplete = !finalCount
	}
	switch {
	case streamErr != "":
		return r, streamErr, nil
	case !stopped:
		return r, "the stream ended before message_stop", nil
	}
	return r, "", nil
}
