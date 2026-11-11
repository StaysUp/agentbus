// Package openai reads the OpenAI Responses API, as codex uses it.
//
// Quirks below were seen on codex-cli 0.164.0 signed in with a ChatGPT plan,
// talking to https://chatgpt.com/backend-api/codex, on 2026-11-05. The
// fixtures in testdata are that traffic.
package openai

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

func (Adapter) Vendor() string { return "openai" }

// Watches the path codex 0.164.0 posted model calls to with its own sign-in,
// and the Responses API's own path, /v1/responses, which codex posts to with a
// provider of its own such as OpenRouter (codex-cli 0.167.0, 2026-11-08). On
// the first codex tries a WebSocket (a GET with Upgrade), and posts only when
// that fails; the WebSocket is not read here.
func (Adapter) Watches(method, path string) bool {
	return method == "POST" && (path == "/backend-api/codex/responses" || path == "/v1/responses")
}

// item is one entry of a request's input or a reply's output.
type item struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	CallID  string          `json:"call_id"`
	Name    string          `json:"name"`
	// Arguments is JSON text inside a string for function_call, and a JSON
	// object for tool_search_call (codex-rs protocol/src/models.rs at
	// rust-v0.164.0), so it is kept raw and read by toolArgs.
	Arguments json.RawMessage `json:"arguments"`
	Input     *string         `json:"input"`  // custom_tool_call: free text
	Output    json.RawMessage `json:"output"` // *_call_output: a string or parts
	// Tools is kept raw: additional_tools and tool_search_output carry full
	// definitions (description, parameters), and a tool search's result is
	// those definitions. toolsOf reads the names.
	Tools json.RawMessage `json:"tools"`
	// EndTurn false on an output message means codex will continue the turn
	// (codex-rs core/src/session/turn.rs at rust-v0.164.0).
	EndTurn *bool `json:"end_turn"`
	// Execution "client" on a tool call means the harness runs it and sends
	// the result back, as for tool_search_call; "server" means the vendor ran
	// it already.
	Execution string `json:"execution"`
	// Status is an output item's own status, read only to report an unknown
	// one, so it is kept raw: any shape still parses.
	Status json.RawMessage `json:"status"`
}

// toolArgs returns a call's arguments as JSON: the text of a string, or an
// object as it is. ok is false when the text is not valid JSON.
func toolArgs(raw json.RawMessage) (json.RawMessage, bool) {
	raw = json.RawMessage(bytes.TrimSpace(raw))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil, false
		}
		if text == "" {
			return nil, true
		}
		if !json.Valid([]byte(text)) {
			return nil, false
		}
		return json.RawMessage(text), true
	}
	return raw, true
}

type tool struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Tools []tool `json:"tools"` // a namespace holds tools
}

type request struct {
	Model string `json:"model"`
	Input []item `json:"input"`
	Tools []tool `json:"tools"`
}

type part struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"` // a refusal part keeps its text here
}

// text reads a content or output field: a string, or a list of parts.
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
	var ps []part
	if err := json.Unmarshal(raw, &ps); err != nil {
		return "", err
	}
	var out []string
	for _, p := range ps {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, p.Text)
		case "refusal":
			out = append(out, p.Refusal)
		}
	}
	return strings.Join(out, sep), nil
}

// nonTextParts returns, as raw JSON, the parts of a parts list that text does
// not read. A string or an unreadable value has none.
func nonTextParts(raw json.RawMessage) []string {
	var ps []json.RawMessage
	if json.Unmarshal(raw, &ps) != nil {
		return nil
	}
	var out []string
	for _, p := range ps {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p, &head)
		switch head.Type {
		case "input_text", "output_text", "text", "refusal":
		default:
			out = append(out, string(p))
		}
	}
	return out
}

// toolNames flattens namespaces into the names the model calls. A call item
// carries the bare name ("exec"), not the namespace, so the bare name is kept.
// toolsOf decodes an item's tools for their names; a shape it cannot read
// gives none.
func toolsOf(raw json.RawMessage) []tool {
	var ts []tool
	if json.Unmarshal(raw, &ts) != nil {
		return nil
	}
	return ts
}

func toolNames(ts []tool, out []string) []string {
	for _, t := range ts {
		switch {
		case t.Type == "namespace":
			out = toolNames(t.Tools, out)
		case t.Name != "":
			out = append(out, t.Name)
		case t.Type != "":
			// A built-in tool has no name, only a type: {"type":"web_search"}
			// (OpenAI's Responses reference). Its type is the name it is
			// offered under.
			out = append(out, t.Type)
		}
	}
	return out
}

// turnStart is the one rule for where a turn starts, used by RequestEvents
// and TurnBasis alike. start is the index of the last person item: the item
// that started the request's turn, or -1 if there is none. starts says this
// request starts that turn: the person item comes after the model's last
// item. Items that are neither (developer context, additional_tools) never
// hide it.
func turnStart(input []item) (start int, starts bool) {
	start, lastModel := -1, -1
	for i, it := range input {
		switch {
		case isPerson(it):
			start = i
		case isModelItem(it):
			lastModel = i
		}
	}
	return start, start > lastModel
}

// isModelItem reports whether an input item is something the model produced
// in an earlier reply: an assistant message, a tool call, or reasoning.
// isModelItem says whether an input item is something the model produced
// earlier. Every tool call type in the Responses API ends in "_call"
// (function_call, custom_tool_call, web_search_call, file_search_call,
// computer_call, code_interpreter_call, local_shell_call, mcp_call, …), so the
// suffix covers them and any new one. The rest are named. A list of tools
// kept missing a model type, and each miss made the adapter take an old
// prompt for a new turn, hence the suffix rule.
func isModelItem(it item) bool {
	if strings.HasSuffix(it.Type, "_call") {
		return true
	}
	switch it.Type {
	case "reasoning", "compaction", "mcp_list_tools", "mcp_approval_request":
		return true
	case "message", "":
		// The Responses API lets an input message omit type, as isPerson
		// allows for the person's.
		return it.Role == "assistant"
	}
	return false
}

// isOutput says whether an input item carries a tool's result back to the
// model. Every such type in the Responses API ends in "_output"
// (function_call_output, custom_tool_call_output, tool_search_output,
// computer_call_output, …), so the suffix covers them and new ones.
func isOutput(t string) bool { return strings.HasSuffix(t, "_output") }

// RequestEvents: a request whose last input item is the person's message
// starts a turn. A request ending in tool outputs continues one.
func (Adapter) RequestEvents(body []byte) ([]event.Event, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	var evs []event.Event

	// The outputs new in this request are those after the model's last item.
	// Earlier calls and outputs of the turn are repeated in every request and
	// were reported by an earlier one. Other items after it (developer
	// context, the person's next words) are stepped over.
	first := len(req.Input)
	for first > 0 && !isModelItem(req.Input[first-1]) {
		first--
	}
	for _, it := range req.Input[first:] {
		if !isOutput(it.Type) {
			continue
		}
		// The parts of one output are pieces of one text: codex 0.164.0 sent
		// "Script completed\n…Output:\n" and "hello123\n" as two parts.
		// An output of a shape text does not know (a tool search returns
		// tools, not text) is kept as its JSON instead of failing the request.
		out, err := text(it.Output, "")
		// When the text read finds nothing in a non-empty output (a shell
		// result holds stdout, stderr and outcome), the output is kept as
		// its JSON instead.
		if err != nil || (out == "" && hasData(it.Output)) {
			out = string(it.Output)
		} else if other := nonTextParts(it.Output); len(other) > 0 {
			// Text with an image (or any other part) keeps the other parts as
			// their JSON after the text, so nothing the tool returned is
			// lost.
			out += "\n" + strings.Join(other, "\n")
		}
		// tool_search_output carries its result in tools, not output, as full
		// definitions (OpenAI's tool-search guide); they are kept whole.
		if out == "" && hasData(it.Tools) {
			out = string(it.Tools)
		}
		// The Responses API has no error flag on an output, so ToolError stays
		// nil: unknown, not false.
		evs = append(evs, event.Event{Kind: event.ToolCompleted, ToolID: it.CallID, ToolOutput: out})
	}
	if start, starts := turnStart(req.Input); starts {
		prompt, err := text(req.Input[start].Content, "\n")
		if err != nil {
			return nil, fmt.Errorf("openai request content: %w", err)
		}
		evs = append(evs, event.Event{Kind: event.TurnStarted, Text: prompt})
	}

	// codex 0.164.0 sends no top-level tools. It offers them in an input item
	// of type additional_tools, grouped in namespaces.
	tools := toolNames(req.Tools, []string{})
	for _, it := range req.Input {
		// A tool search's output loads more tools for the model to call.
		if it.Type == "additional_tools" || it.Type == "tool_search_output" {
			tools = toolNames(toolsOf(it.Tools), tools)
		}
	}
	evs = append(evs, event.Event{Kind: event.ModelRequested, Model: req.Model, ToolsOffered: tools})
	var u adapter.Unknowns
	for _, it := range req.Input {
		if !strings.HasSuffix(it.Type, "_call") && !isOutput(it.Type) {
			checkName(&u, knownInputTypes, "request.input[].type", it.Type)
		}
		if it.Role != "" {
			checkName(&u, knownInputRoles, "request.input[].role", it.Role)
		}
	}
	return append(evs, u.Events()...), nil
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
		// Sent by chatgpt.com's backend (0 in every capture), not in OpenAI's
		// public reference. Whether input_tokens include it is unmeasured.
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	// unknown holds the field names this adapter does not know, for
	// schema.unexpected.
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
		In  json.RawMessage `json:"input_tokens_details"`
		Out json.RawMessage `json:"output_tokens_details"`
	}
	if json.Unmarshal(b, &raw) == nil {
		if len(raw.In) > 0 {
			u.unknown = adapter.UnknownFields(raw.In, "usage.input_tokens_details", knownInputDetails, u.unknown)
		}
		if len(raw.Out) > 0 {
			u.unknown = adapter.UnknownFields(raw.Out, "usage.output_tokens_details", knownOutputDetails, u.unknown)
		}
	}
	return nil
}

// The names this adapter knows. Anything else the vendor sends in these places
// becomes a schema.unexpected event, so a new codex or API release is
// noticed. "Seen" means in the codex-cli 0.164.0 captures of 2026-11-05 (the
// fixtures in testdata and the untrimmed capture they came from). "Read"
// means a case this adapter's code handles. "Documented" means named in
// OpenAI's Responses API reference, unseen here. Item types ending in "_call"
// or "_output" are known by that suffix (isModelItem, isOutput) and are not
// listed.
var (
	// knownInputTypes: "" and message (a message may omit its type),
	// additional_tools (seen), and the model items isModelItem names.
	knownInputTypes = adapter.Names("", "message", "additional_tools",
		"reasoning", "compaction", "mcp_list_tools", "mcp_approval_request")
	// knownInputRoles: user and developer are seen; assistant is read;
	// system is documented.
	knownInputRoles = adapter.Names("user", "developer", "assistant", "system")
	// knownOutputTypes: message (seen), reasoning and compaction (read), and
	// the two mcp items isModelItem names.
	knownOutputTypes = adapter.Names("message", "reasoning", "compaction", "mcp_list_tools", "mcp_approval_request")
	knownOutputRoles = adapter.Names("assistant")
	// knownStreamEvents: seen, read, and every server stream event type in
	// openai-python 2.46.0's types/responses (the reasoning family codex
	// receives when reasoning summaries are on, and the hosted tool progress
	// families), so a documented feature reports nothing.
	knownStreamEvents = adapter.Names(
		// seen
		"response.created", "response.in_progress", "response.metadata",
		"response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
		"response.completed",
		// read
		"response.refusal.delta", "response.incomplete", "response.failed", "error",
		// documented (openai-python 2.46.0)
		"response.queued", "response.refusal.done", "response.output_text.annotation.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.audio.delta", "response.audio.done",
		"response.audio.transcript.delta", "response.audio.transcript.done",
		"response.web_search_call.in_progress", "response.web_search_call.searching",
		"response.web_search_call.completed",
		"response.file_search_call.in_progress", "response.file_search_call.searching",
		"response.file_search_call.completed",
		"response.code_interpreter_call.in_progress", "response.code_interpreter_call.interpreting",
		"response.code_interpreter_call.completed",
		"response.code_interpreter_call_code.delta", "response.code_interpreter_call_code.done",
		"response.image_generation_call.in_progress", "response.image_generation_call.generating",
		"response.image_generation_call.partial_image", "response.image_generation_call.completed",
		"response.mcp_call.in_progress", "response.mcp_call.completed", "response.mcp_call.failed",
		"response.mcp_call_arguments.delta", "response.mcp_call_arguments.done",
		"response.mcp_list_tools.in_progress", "response.mcp_list_tools.completed",
		"response.mcp_list_tools.failed")
	// knownStatuses: the response statuses turnEnd and fromResponse read,
	// and cancelled (documented, for background responses).
	knownStatuses = adapter.Names("completed", "incomplete", "failed", "in_progress", "queued", "cancelled")
	// knownItemStatuses: completed is seen; the others are documented.
	knownItemStatuses = adapter.Names("completed", "in_progress", "incomplete")
	// knownIncomplete: the reasons turnEnd maps, and "interrupted", which
	// codex-rs handles (codex-api/src/sse/responses.rs) and turnEnd
	// leaves as unknown.
	knownIncomplete = adapter.Names("max_output_tokens", "content_filter", "interrupted")
	// knownUsage and the details: all seen. attribution and total_tokens are
	// not read.
	knownUsage         = adapter.Names("input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details", "total_tokens", "attribution")
	knownInputDetails  = adapter.Names("cached_tokens", "cache_write_tokens")
	knownOutputDetails = adapter.Names("reasoning_tokens")
)

func checkName(u *adapter.Unknowns, known adapter.Set, where, name string) {
	if !known.Has(name) {
		u.Value(where, name)
	}
}

// checkResponse notes the names in a final response that this adapter does
// not know.
func checkResponse(u *adapter.Unknowns, r response) {
	if r.Status != "" {
		checkName(u, knownStatuses, "response.status", r.Status)
	}
	if d := r.IncompleteDetails; d != nil && d.Reason != "" {
		checkName(u, knownIncomplete, "response.incomplete_details.reason", d.Reason)
	}
	for _, it := range r.Output {
		if !strings.HasSuffix(it.Type, "_call") {
			checkName(u, knownOutputTypes, "response.output[].type", it.Type)
		}
		if it.Role != "" {
			checkName(u, knownOutputRoles, "response.output[].role", it.Role)
		}
		if st, ok := adapter.String(it.Status); ok && st != "" {
			checkName(u, knownItemStatuses, "response.output[].status", st)
		}
	}
	if r.Usage != nil {
		for _, x := range r.Usage.unknown {
			u.Admit(x)
		}
	}
}

type response struct {
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []item `json:"output"`
	Usage  *usage `json:"usage"` // nil when the vendor sent none
	// ServiceTier is the tier that served the call, on the final response:
	// "default", or "priority" for codex's fast tier. response.created
	// echoes the request's "auto" (codex 0.164.0 streams, captured).
	ServiceTier string `json:"service_tier"`
	// EndTurn false on the response means codex continues the turn
	// (codex-rs codex-api/src/sse/responses.rs at rust-v0.164.0).
	EndTurn           *bool `json:"end_turn"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
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

// isStream decides whether a reply is server-sent events. chatgpt.com sent
// codex 0.164.0's streamed replies with no Content-Type at all, so a missing
// type falls back to the body's first line. A stream may open with a ":"
// comment line, which the server-sent events format allows; a JSON body
// never starts with one.
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
	if isStream(contentType, body) {
		return readStream(body)
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai reply: %w", err)
	}
	var u adapter.Unknowns
	checkResponse(&u, r)
	return append(fromResponse(r, ""), u.Events()...), nil
}

// fromResponse turns a reply into events. A reply with a problem reports only
// model.responded: what it asked for and how it ended cannot be trusted.
func fromResponse(r response, problem string) []event.Event {
	stop := r.Status
	if r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "" {
		stop = "incomplete:" + r.IncompleteDetails.Reason
	}
	if problem == "" {
		switch r.Status {
		case "failed":
			problem = "the response failed"
			if r.Error != nil && r.Error.Message != "" {
				problem = shortError(r.Error.Message)
			}
		case "":
			problem = "the reply has no status"
		}
	}
	var parts []string
	for _, it := range r.Output {
		if it.Type == "message" && it.Role == "assistant" {
			if s, err := text(it.Content, ""); err == nil && s != "" {
				parts = append(parts, s)
			}
		}
	}
	resp := event.Event{
		Kind:       event.ModelResponded,
		Model:      r.Model,
		Text:       strings.Join(parts, "\n"),
		StopReason: stop,
		Status:     200,
		Error:      problem,
	}
	// A reply without usage keeps Usage nil: absent is not zero. OpenAI's
	// input_tokens include the cached tokens; the framework's do not.
	if u := r.Usage; u != nil {
		resp.Usage = &event.Usage{
			InputTokens:      u.InputTokens - u.InputTokensDetails.CachedTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.InputTokensDetails.CachedTokens,
			CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
			ServiceTier:      r.ServiceTier,
			ServerToolUse:    hostedCalls(r.Output),
		}
	}
	evs := []event.Event{resp}
	if problem != "" {
		return evs
	}
	calls := 0
	for _, it := range r.Output {
		switch it.Type {
		case "function_call":
			calls++
			e := event.Event{Kind: event.ToolRequested, ToolID: it.CallID, ToolName: it.Name}
			if args, ok := toolArgs(it.Arguments); ok {
				e.ToolInput = args
			} else {
				e.Error = "the tool input is not valid JSON"
			}
			evs = append(evs, e)
		case "custom_tool_call":
			// A custom tool takes free text, not JSON: codex 0.164.0's exec
			// tool takes JavaScript. It is kept as a JSON string.
			calls++
			e := event.Event{Kind: event.ToolRequested, ToolID: it.CallID, ToolName: it.Name}
			if it.Input != nil {
				e.ToolInput, _ = json.Marshal(*it.Input)
			}
			evs = append(evs, e)
		default:
			// Any other call the client must run (tool_search_call with
			// execution "client", and new ones) is a tool request too.
			if !strings.HasSuffix(it.Type, "_call") || it.Execution != "client" {
				continue
			}
			calls++
			name := it.Name
			if name == "" {
				name = it.Type
			}
			e := event.Event{Kind: event.ToolRequested, ToolID: it.CallID, ToolName: name}
			if args, ok := toolArgs(it.Arguments); ok {
				e.ToolInput = args
			} else {
				e.Error = "the tool input is not valid JSON"
			}
			evs = append(evs, e)
		}
	}
	if outcome, ends := turnEnd(r, calls); ends {
		evs = append(evs, event.Event{Kind: event.TurnCompleted, Outcome: outcome, StopReason: stop})
	}
	return evs
}

// hasRefusal reports whether the reply's message holds a refusal part. OpenAI
// documents a refusal as status "completed" with a content part
// {"type":"refusal","refusal":"..."} (structured-outputs guide, read
// 2026-11-05), so the status alone would call it finished.
// hasCompaction says whether a completed response is codex compacting its
// history mid-turn: the work goes on after it, so it is not the turn's end
// (codex-rs core/src/compact_remote_v2.rs at rust-v0.164.0).
func hasCompaction(r response) bool {
	for _, it := range r.Output {
		if it.Type == "compaction" {
			return true
		}
	}
	return false
}

// continues says whether the response, or an output message in it, set
// end_turn to false.
func continues(r response) bool {
	if r.EndTurn != nil && !*r.EndTurn {
		return true
	}
	for _, it := range r.Output {
		if it.EndTurn != nil && !*it.EndTurn {
			return true
		}
	}
	return false
}

func hasRefusal(r response) bool {
	for _, it := range r.Output {
		if it.Type != "message" {
			continue
		}
		var ps []part
		if json.Unmarshal(it.Content, &ps) != nil {
			continue
		}
		for _, p := range ps {
			if p.Type == "refusal" {
				return true
			}
		}
	}
	return false
}

// turnEnd maps the response's status to the framework's outcome. A completed
// response that asks for tools is not the end: the status says "completed"
// both for a tool request and for the final answer (codex 0.164.0), so the
// tool calls in the output decide.
func turnEnd(r response, calls int) (event.Outcome, bool) {
	switch r.Status {
	case "in_progress", "queued":
		return "", false
	case "completed":
		if hasRefusal(r) {
			return event.Refused, true
		}
		if calls > 0 || hasCompaction(r) || continues(r) {
			return "", false
		}
		return event.Finished, true
	case "incomplete":
		reason := ""
		if r.IncompleteDetails != nil {
			reason = r.IncompleteDetails.Reason
		}
		switch reason {
		case "max_output_tokens":
			return event.Partial, true
		case "content_filter":
			return event.Refused, true
		}
		return event.Unknown, true
	default:
		return event.Unknown, true
	}
}

// readStream rebuilds a reply from server-sent events. The terminal event
// (response.completed, response.incomplete or response.failed) carries the
// whole response with its output and usage. Usage arrives only there:
// response.created carries "usage":null. A stream cut before it has no count
// at all, so its Usage stays nil, and its text is rebuilt from the deltas.
// So output_incomplete never applies in this format: no partial count exists.
func readStream(body []byte) ([]event.Event, error) {
	var final *response
	var model, streamErr string
	var u adapter.Unknowns
	deltas := map[int]*strings.Builder{}

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		// The terminal event ends the reply. What follows is not read:
		// codex-cli 0.164.0 stops reading after response.completed, so a
		// copy can end in a partial line such as "data: [DO".
		if final != nil {
			break
		}
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			continue
		}
		var ev struct {
			Type        string          `json:"type"`
			Response    *response       `json:"response"`
			OutputIndex int             `json:"output_index"`
			Delta       json.RawMessage `json:"delta"` // text for text deltas; other events carry objects
			Message     string          `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			// The names seen before the error are still reported.
			return u.Events(), fmt.Errorf("openai stream event: %w", err)
		}
		checkName(&u, knownStreamEvents, "stream.event", ev.Type)
		switch ev.Type {
		case "response.created", "response.in_progress":
			if ev.Response != nil {
				model = ev.Response.Model
			}
		case "response.output_text.delta", "response.refusal.delta":
			b, ok := deltas[ev.OutputIndex]
			if !ok {
				b = &strings.Builder{}
				deltas[ev.OutputIndex] = b
			}
			var piece string
			if json.Unmarshal(ev.Delta, &piece) == nil {
				b.WriteString(piece)
			}
		case "response.completed", "response.incomplete", "response.failed":
			final = ev.Response
		case "error":
			streamErr = shortError(ev.Message)
			if streamErr == "" {
				streamErr = "the stream reported an error"
			}
		}
	}
	if err := sc.Err(); err != nil {
		return u.Events(), fmt.Errorf("openai stream: %w", err)
	}
	if final != nil {
		checkResponse(&u, *final)
		return append(fromResponse(*final, streamErr), u.Events()...), nil
	}
	indexes := make([]int, 0, len(deltas))
	for i := range deltas {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	var parts []string
	for _, i := range indexes {
		parts = append(parts, deltas[i].String())
	}
	problem := streamErr
	if problem == "" {
		problem = "the stream ended before the response completed"
	}
	// No usage: none had arrived, and zeros would claim a known zero input.
	return append([]event.Event{{
		Kind:   event.ModelResponded,
		Model:  model,
		Text:   strings.Join(parts, "\n"),
		Status: 200,
		Error:  problem,
	}}, u.Events()...), nil
}

// isPerson reports whether an input item is a person's message. Tool outputs
// are their own item types, so a user message is never a tool output.
func isPerson(it item) bool {
	return (it.Type == "message" || it.Type == "") && it.Role == "user"
}

// Known limit: when codex compacts its history in the middle of a turn, the
// items before the prompt change, so the rest of that turn gets a new id
// without a new turn.started. The spec accepts a new id chain after
// compaction, and tool results stay matched by tool_id. A basis compaction
// cannot change (the prompt item's own id, if codex keeps it) is unmeasured.
//
// TurnBasis is the input up to and including the item that started the turn:
// the last user message. codex 0.164.0 also sends a user message with the
// AGENTS.md text and an <environment_context> block before the real prompt;
// the real prompt is the later one, so taking the last user message is right.
//
// Each item is re-encoded in full with sorted keys and no spacing. Nothing is
// removed: in the captures (2026-11-05) the request that sent a tool output
// repeated the first seven items of the turn's first request byte for byte,
// ids and create_time included, so no field was seen to move.
func (Adapter) TurnBasis(body []byte) []byte {
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	parsed := make([]item, len(req.Input))
	for i, raw := range req.Input {
		if err := json.Unmarshal(raw, &parsed[i]); err != nil {
			return nil
		}
	}
	start, _ := turnStart(parsed)
	if start < 0 {
		return nil
	}
	items := make([]any, 0, start+1)
	for _, raw := range req.Input[:start+1] {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // numbers stay as written
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		items = append(items, v)
	}
	// json.Marshal writes map keys in sorted order.
	out, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return out
}

// hasData says whether a raw JSON value holds anything beyond null, an empty
// string, or an empty list.
func hasData(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "[]", "{}":
		return false
	}
	return true
}

// hostedCalls counts the tool calls OpenAI ran itself (web_search_call,
// file_search_call, code_interpreter_call, …) by item type. Some are charged
// per use. function_call, custom_tool_call and any call with execution
// "client" are run by the harness and are not counted. Nil when there are
// none.
func hostedCalls(output []item) map[string]int {
	var n map[string]int
	for _, it := range output {
		if !strings.HasSuffix(it.Type, "_call") || it.Type == "function_call" ||
			it.Type == "custom_tool_call" || it.Execution == "client" {
			continue
		}
		if n == nil {
			n = map[string]int{}
		}
		n[it.Type]++
	}
	return n
}
