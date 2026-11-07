package openai

// Codex replies kept for own replay, and what own replay needs to know of a
// codex request's input items. The layout was measured against codex-cli
// 0.164.0 on 2026-11-06:
// a resumed codex turn sends the earlier turn's input items and its reply's
// output items as its own input, with store false and no
// previous_response_id, and the backend took the reply's raw items back as
// input, encrypted reasoning included.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"mime"
	"sort"
	"strings"

	"github.com/staysup/agentbus/body"
)

// Build keeps a codex reply as the list of items it gave: the output of
// response.completed, each item's bytes exactly as the vendor sent them, so
// encrypted reasoning goes back byte for byte. A stream also gives each item
// in response.output_item.done; those are used only when the completed
// response's output is empty. The stop reason is the response's status,
// "completed", as on model.responded; or ContinuesStop when the response
// itself set end_turn to false, a flag that lives outside the output items:
// codex goes on with the turn after such a reply, so it is not the turn's
// last.
//
// Only a whole, completed reply is kept. A stream that ended before
// response.completed, reported an error, or ended incomplete or failed gives
// an error: a replay never carries part of a reply.
func (Adapter) Build(status int, contentType string, data []byte) (json.RawMessage, string, error) {
	if status != 200 {
		return nil, "", errors.New("openai reply: not a 200")
	}
	if mt, _, _ := mime.ParseMediaType(contentType); mt == "text/event-stream" || isStream(contentType, data) {
		return buildStream(data)
	}
	var r terminal
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, "", errors.New("openai reply: not one JSON object")
	}
	return completed(r, nil)
}

func buildStream(data []byte) (json.RawMessage, string, error) {
	done := map[int]json.RawMessage{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		var ev struct {
			Type        string          `json:"type"`
			Response    json.RawMessage `json:"response"`
			Item        json.RawMessage `json:"item"`
			OutputIndex int             `json:"output_index"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, "", errors.New("openai reply: a stream event is not JSON")
		}
		switch ev.Type {
		case "response.output_item.done":
			done[ev.OutputIndex] = ev.Item
		case "error", "response.failed", "response.incomplete":
			return nil, "", errors.New("openai reply: the response did not complete")
		case "response.completed":
			// What follows the terminal event is not read: codex-cli 0.164.0
			// stops reading there, so a copy can end in a partial line.
			var r terminal
			if err := json.Unmarshal(ev.Response, &r); err != nil {
				return nil, "", errors.New("openai reply: response.completed carries no response")
			}
			return completed(r, done)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, "", errors.New("openai reply: the stream could not be read")
	}
	return nil, "", errors.New("openai reply: the stream ended before response.completed")
}

// ContinuesStop is a kept codex reply's stop reason when the response set
// end_turn to false.
const ContinuesStop = "completed:end_turn_false"

// terminal is what Build reads of a terminal response.
type terminal struct {
	Status  string          `json:"status"`
	Output  json.RawMessage `json:"output"`
	EndTurn *bool           `json:"end_turn"`
}

// completed checks a terminal response and returns its output list.
func completed(r terminal, done map[int]json.RawMessage) (json.RawMessage, string, error) {
	output := r.Output
	if r.Status != "completed" {
		return nil, "", errors.New("openai reply: the response is not completed")
	}
	items, ok := body.Elements(output)
	if !ok {
		return nil, "", errors.New("openai reply: the response has no output list")
	}
	if len(items) == 0 && len(done) > 0 {
		idx := make([]int, 0, len(done))
		for i := range done {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for n, i := range idx {
			if i != n {
				return nil, "", errors.New("openai reply: the stream's output items are not numbered in order")
			}
			items = append(items, done[i])
		}
	}
	if len(items) == 0 {
		return nil, "", errors.New("openai reply: the response has no output items")
	}
	for _, it := range items {
		if body.Label(it, "type") == "" {
			return nil, "", errors.New("openai reply: an output item has no type")
		}
	}
	stop := "completed"
	if r.EndTurn != nil && !*r.EndTurn {
		stop = ContinuesStop
	}
	return list(items), stop, nil
}

// list writes items as one JSON list, each item's bytes unchanged.
func list(items []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// passthrough is the part of codex's internal_chat_message_metadata_passthrough
// own replay reads: what kind of content each part of a message is.
type passthrough struct {
	Kinds []string `json:"content_item_kinds"`
}

// Kinds returns a message item's content kinds, from codex's
// internal_chat_message_metadata_passthrough.content_item_kinds:
// "user.text" on the person's prompt, "model.base_instructions",
// "agents_md.instructions", "environments.environment_context" and others on
// the context codex puts first. Nil when the item has none.
func Kinds(raw json.RawMessage) []string {
	var it struct {
		P *passthrough `json:"internal_chat_message_metadata_passthrough"`
	}
	if json.Unmarshal(raw, &it) != nil || it.P == nil {
		return nil
	}
	return it.P.Kinds
}

func hasKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

func parse(raw json.RawMessage) (item, bool) {
	var it item
	if json.Unmarshal(raw, &it) != nil {
		return item{}, false
	}
	return it, true
}

// IsPrompt reports whether an input item is a person's prompt: a user message
// whose content kinds say user.text. Decided by structure, never by text.
func IsPrompt(raw json.RawMessage) bool {
	it, ok := parse(raw)
	return ok && isPerson(it) && hasKind(Kinds(raw), "user.text")
}

// PromptText returns a prompt item's text: its input_text parts joined.
func PromptText(raw json.RawMessage) (string, bool) {
	it, ok := parse(raw)
	if !ok {
		return "", false
	}
	s, err := text(it.Content, "")
	return s, err == nil
}

// ContextKey names an input item codex puts before the prompt, its context:
// the tools item (additional_tools), or a developer or user message whose
// content kinds codex set and are not the prompt's (base instructions,
// permissions, AGENTS.md, the environment). A message without kinds, such as
// a note AgentBus wrote, is not context. The key is the item's type, role
// and content kinds; content is what the model reads of it, without the
// item's id or codex's passthrough, which differ between processes for the
// same context. ok is false for any other item.
func ContextKey(raw json.RawMessage) (key string, content json.RawMessage, ok bool) {
	it, parsed := parse(raw)
	if !parsed {
		return "", nil, false
	}
	if it.Type == "additional_tools" {
		return "additional_tools", it.Tools, true
	}
	kinds := Kinds(raw)
	if (it.Type != "message" && it.Type != "") || (it.Role != "developer" && it.Role != "user") ||
		len(kinds) == 0 || hasKind(kinds, "user.text") {
		return "", nil, false
	}
	return it.Type + "|" + it.Role + "|" + strings.Join(kinds, ","), it.Content, true
}

// IsModelItem reports whether an input item is something the model produced
// in an earlier reply (isModelItem).
func IsModelItem(raw json.RawMessage) bool {
	it, ok := parse(raw)
	return ok && isModelItem(it)
}

// IsCompaction reports whether an item is codex's compaction of its history.
func IsCompaction(raw json.RawMessage) bool {
	return body.Label(raw, "type") == "compaction"
}

// hosted are the tool calls OpenAI runs itself and answers in the same
// reply: they wait for no result from the harness.
var hosted = map[string]bool{"web_search_call": true, "file_search_call": true,
	"code_interpreter_call": true, "image_generation_call": true, "mcp_call": true}

// Unanswered returns, for a reply's items, one output item for each tool call
// the harness was to run, each saying the call did not finish: the turn
// stopped before its result came back. asked says the reply asked for a
// tool. function_call gets a function_call_output and custom_tool_call a
// custom_tool_call_output, both with the call's call_id and a text output,
// as codex sends them. A call the harness runs whose result shape is not
// known (tool_search_call, local_shell_call, shell_call and new ones) is an
// error: no result is guessed.
func Unanswered(items []json.RawMessage, why string) (outputs []json.RawMessage, asked bool, err error) {
	for _, raw := range items {
		it, ok := parse(raw)
		if !ok {
			return nil, false, errors.New("a reply item is not a JSON object")
		}
		if !strings.HasSuffix(it.Type, "_call") || it.Execution == "server" || (hosted[it.Type] && it.Execution != "client") {
			continue
		}
		asked = true
		switch it.Type {
		case "function_call", "custom_tool_call":
		default:
			return nil, true, errors.New("the reply asked for a " + it.Type + ", whose result the gateway cannot write")
		}
		if it.CallID == "" {
			return nil, true, errors.New("a " + it.Type + " has no call_id")
		}
		outputs = append(outputs, body.Object([]body.Field{
			{Key: "type", Value: jsonText(it.Type + "_output")},
			{Key: "call_id", Value: jsonText(it.CallID)},
			{Key: "output", Value: jsonText(why)}}))
	}
	return outputs, asked, nil
}

// Continues reports whether a reply's items say codex goes on with the turn
// after them: an output message with end_turn false.
func Continues(items []json.RawMessage) bool {
	for _, raw := range items {
		if it, ok := parse(raw); ok && it.EndTurn != nil && !*it.EndTurn {
			return true
		}
	}
	return false
}

// UserMessage is a user message item holding one input_text part.
func UserMessage(s string) json.RawMessage { return message("user", "input_text", s) }

// DeveloperMessage is a developer message item holding one input_text part.
func DeveloperMessage(s string) json.RawMessage { return message("developer", "input_text", s) }

func message(role, part, s string) json.RawMessage {
	p := body.Object([]body.Field{{Key: "type", Value: jsonText(part)}, {Key: "text", Value: jsonText(s)}})
	return body.Object([]body.Field{{Key: "type", Value: jsonText("message")}, {Key: "role", Value: jsonText(role)},
		{Key: "content", Value: list([]json.RawMessage{p})}})
}

func jsonText(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ThreadID returns a request's prompt_cache_key, which codex sets to its
// thread id (codex-cli 0.164.0), and reports whether it has the shape of one:
// letters, digits and dashes, 16 to 64 of them. A value of another shape is
// never rewritten.
func ThreadID(top []body.Field) (string, bool) {
	for _, f := range top {
		if f.Key != "prompt_cache_key" {
			continue
		}
		var s string
		if json.Unmarshal(f.Value, &s) != nil || len(s) < 16 || len(s) > 64 {
			return "", false
		}
		for _, c := range s {
			if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				return "", false
			}
		}
		return s, true
	}
	return "", false
}

// AllAnswered reports whether every tool call the harness runs among items
// (as Unanswered counts them) has an output item with its call_id.
func AllAnswered(items []json.RawMessage) bool {
	answered := map[string]bool{}
	for _, raw := range items {
		if it, ok := parse(raw); ok && isOutput(it.Type) && it.CallID != "" {
			answered[it.CallID] = true
		}
	}
	for _, raw := range items {
		it, ok := parse(raw)
		if !ok {
			return false
		}
		if !strings.HasSuffix(it.Type, "_call") || it.Execution == "server" || (hosted[it.Type] && it.Execution != "client") {
			continue
		}
		if it.CallID == "" || !answered[it.CallID] {
			return false
		}
	}
	return true
}
