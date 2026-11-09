package gateway

// The guard refuses a shell command a run's turn may never run: an entry of
// forbid in permissions.toml. It works on the model's reply, before the agent
// sees the call, for Claude Code (Anthropic Messages, its Bash tool) and codex
// (OpenAI Responses, its exec tool).
//
// The method was measured with real agents (Claude Code 2.1.319 on sonnet,
// codex-cli 0.164.0, 2026-11-08): the tool call's name is renamed in the
// reply, to AgentBusRefused_<name>, so the agent has no such tool and does
// not run it. On every later request the gateway puts the real name back and
// replaces that call's output with a plain refusal naming the rule. Both
// agents accepted it and did not try another route. The prefix is the only
// marker, so the history is fixed on any request that carries it, whatever
// turn it belongs to.
//
// A call is held from its first event until its last, the shortest span that
// shows the whole command before the agent can act on it: codex runs a call
// at its response.output_item.done, not at the reply's end. Nothing else is
// held. Unstreamed replies are guarded too: Claude Code sends one after a
// stream has been idle 300 seconds.
//
// The guard reads text. A command built at run time, or run from a script
// written earlier, is not seen; for a hard boundary, also leave out what the
// command needs, such as push credentials.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/staysup/agentbus/event"
)

// Guard gives the forbid entries of a run's turn: commands it may never run.
type Guard interface {
	Forbid(session, turn string) []string
}

// SetGuard sets the Guard. Call it before serving.
func (g *Gateway) SetGuard(gd Guard) { g.guard = gd }

// RefusedPrefix marks a refused tool call's name in the reply the agent gets.
const RefusedPrefix = "AgentBusRefused_"

// refusalText is what the model reads in place of a refused call's output.
func refusalText(rule string) string {
	if rule == "" {
		return "Refused by the AgentBus gateway: a forbid rule in permissions.toml matched this command. The command did not run."
	}
	return "Refused by the AgentBus gateway: the forbid rule " + strconv.Quote(rule) +
		" in permissions.toml matched this command. The command did not run."
}

// refusals remembers which rule refused each call id, for the refusal text.
// A call it does not know, as after a restart, gets the text without a rule.
type refusals struct {
	mu sync.Mutex
	m  map[string]string
}

// maxRefusals bounds the memory; past it the oldest names are forgotten.
const maxRefusals = 4096

func (r *refusals) add(id, rule string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil || len(r.m) >= maxRefusals {
		r.m = map[string]string{}
	}
	r.m[id] = rule
}

func (r *refusals) rule(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

// decodeJSON reads b keeping numbers as written.
func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// encodeJSON writes v without escaping <, > and &.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// fixHistory puts back the real name of every refused call in a request and
// replaces its output with the refusal. ok is false when nothing changed.
func (g *Gateway) fixHistory(vendor string, body []byte) ([]byte, bool) {
	if !bytes.Contains(body, []byte(RefusedPrefix)) {
		return body, false
	}
	var m map[string]any
	if decodeJSON(body, &m) != nil {
		return body, false
	}
	ids := map[string]bool{}
	restore := func(c map[string]any, idKey string) {
		name, _ := c["name"].(string)
		if real, ok := strings.CutPrefix(name, RefusedPrefix); ok {
			c["name"] = real
			if id, _ := c[idKey].(string); id != "" {
				ids[id] = true
			}
		}
	}
	switch vendor {
	case "anthropic":
		msgs, _ := m["messages"].([]any)
		blocks := func(f func(map[string]any)) {
			for _, mm := range msgs {
				msg, _ := mm.(map[string]any)
				content, _ := msg["content"].([]any)
				for _, cb := range content {
					if c, ok := cb.(map[string]any); ok {
						f(c)
					}
				}
			}
		}
		blocks(func(c map[string]any) {
			if c["type"] == "tool_use" {
				restore(c, "id")
			}
		})
		blocks(func(c map[string]any) {
			if id, _ := c["tool_use_id"].(string); c["type"] == "tool_result" && ids[id] {
				c["content"] = refusalText(g.refused.rule(id))
				c["is_error"] = true
			}
		})
	case "openai":
		items, _ := m["input"].([]any)
		for _, it := range items {
			if c, ok := it.(map[string]any); ok && isCallItem(c["type"]) {
				restore(c, "call_id")
			}
		}
		for _, it := range items {
			c, _ := it.(map[string]any)
			t, _ := c["type"].(string)
			if id, _ := c["call_id"].(string); strings.HasSuffix(t, "_call_output") && ids[id] {
				// A plain string: codex sent a list of parts, and the API
				// took the string every time (2026-11-08).
				c["output"] = refusalText(g.refused.rule(id))
			}
		}
	}
	if len(ids) == 0 {
		return body, false
	}
	out, err := encodeJSON(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func isCallItem(t any) bool {
	return t == "custom_tool_call" || t == "function_call" || t == "local_shell_call"
}

// commandsOf returns the shell commands of one tool call, by vendor and tool.
// A tool that runs no shell command gives none.
func commandsOf(vendor string, item map[string]any) []string {
	if vendor == "anthropic" {
		// Claude Code's shell tool (2.1.319).
		if item["name"] != "Bash" {
			return nil
		}
		in, _ := item["input"].(map[string]any)
		if c, ok := in["command"].(string); ok {
			return []string{c}
		}
		return nil
	}
	switch item["type"] {
	case "custom_tool_call":
		// codex-cli 0.164.0 runs commands through its custom tool exec, a
		// JavaScript program calling tools.exec_command({cmd: …}).
		if s, ok := item["input"].(string); ok && item["name"] == "exec" {
			return execCommands(s)
		}
	case "function_call":
		// A function tool with a cmd or command argument, such as codex's
		// exec_command or shell.
		s, _ := item["arguments"].(string)
		var args map[string]any
		if decodeJSON([]byte(s), &args) == nil {
			for _, k := range []string{"cmd", "command"} {
				if c := commandValue(args[k]); c != "" {
					return []string{c}
				}
			}
		}
	case "local_shell_call":
		if a, ok := item["action"].(map[string]any); ok {
			if c := commandValue(a["command"]); c != "" {
				return []string{c}
			}
		}
	}
	return nil
}

// ToolCommands are the shell commands of a stored tool.requested, read the
// way the guard reads a call: vendor, tool name and input as the record holds
// them. A tool that runs no shell command gives none.
func ToolCommands(vendor, name string, input json.RawMessage) []string {
	item := map[string]any{"name": name}
	var in any
	if decodeJSON(input, &in) != nil {
		return nil
	}
	if vendor == "anthropic" {
		item["input"] = in
		return commandsOf(vendor, item)
	}
	switch v := in.(type) {
	case string:
		// A custom tool's free text, kept as a JSON string (codex's exec).
		item["type"], item["input"] = "custom_tool_call", v
	case map[string]any:
		item["type"], item["arguments"] = "function_call", string(input)
	default:
		return nil
	}
	return commandsOf(vendor, item)
}

// commandValue is a command given as a string or as a list of words, the
// list joined back into a quoted command line.
func commandValue(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		words := make([]string, 0, len(c))
		for _, w := range c {
			s, _ := w.(string)
			words = append(words, shellQuote(s))
		}
		return strings.Join(words, " ")
	}
	return ""
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\;&|()<>$`*?[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// guardBody is a reply as the agent gets it: every refused call renamed.
type guardBody struct {
	g      *Gateway
	ex     *exchange
	vendor string
	rules  []string
	src    io.ReadCloser
	br     *bufio.Reader
	end    func()
	ended  sync.Once

	sniffed bool
	out     bytes.Buffer
	err     error

	held    [][]byte
	open    map[string]*openCall // by Anthropic block index or OpenAI item id
	refused map[string]string    // the same keys, refused: the rule
}

type openCall struct {
	name, id string
	input    strings.Builder // Anthropic: the partial JSON so far
}

// guardReply wraps src. end runs once, when the guard has read the whole
// reply or the reply is closed: every tool.refused is emitted by then.
func (g *Gateway) guardReply(ex *exchange, rules []string, src io.ReadCloser, end func()) io.ReadCloser {
	return &guardBody{g: g, ex: ex, vendor: g.adapter.Vendor(), rules: rules, src: src, end: end,
		br: bufio.NewReaderSize(src, 64<<10), open: map[string]*openCall{}, refused: map[string]string{}}
}

func (b *guardBody) Close() error {
	err := b.src.Close()
	b.finished()
	return err
}

func (b *guardBody) finished() {
	if b.end != nil {
		b.ended.Do(b.end)
	}
}

func (b *guardBody) Read(p []byte) (int, error) {
	for b.out.Len() == 0 && b.err == nil {
		b.step()
	}
	if b.err != nil {
		b.finished()
	}
	if b.out.Len() > 0 {
		return b.out.Read(p)
	}
	return 0, b.err
}

// step reads one SSE frame, or the whole of an unstreamed reply.
func (b *guardBody) step() {
	if !b.sniffed {
		b.sniffed = true
		// A JSON object is an unstreamed reply; anything else is a stream.
		// codex's replies can come without a Content-Type (2026-11-05).
		// Byte by byte: a stream may send a short first frame and then
		// wait, so Peek never asks for more than the next byte.
		if b.firstByte() == '{' {
			all, err := io.ReadAll(b.br)
			b.out.Write(b.rewriteJSON(all))
			b.err = err
			if b.err == nil {
				b.err = io.EOF
			}
			return
		}
	}
	frame, err := readFrame(b.br)
	if len(frame) > 0 {
		b.frame(frame)
	}
	if err != nil {
		// A reply that ends inside a call: what is held goes on as it is.
		// The call never ended, so the agent does not run it.
		b.flush()
		b.err = err
	}
}

// firstByte is the reply's first byte that is not white space, or 0.
func (b *guardBody) firstByte() byte {
	for n := 1; ; n++ {
		p, _ := b.br.Peek(n)
		if len(p) < n {
			return 0
		}
		if c := p[n-1]; c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return c
		}
	}
}

// readFrame reads one SSE frame, up to and including its blank line.
func readFrame(br *bufio.Reader) ([]byte, error) {
	var f []byte
	for {
		line, err := br.ReadBytes('\n')
		f = append(f, line...)
		if err != nil {
			return f, err
		}
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return f, nil
		}
	}
}

// frameData is the JSON of a frame's data lines, and the frame's event name.
func frameData(f []byte) (data []byte, ev string) {
	for _, line := range bytes.Split(f, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(d, []byte(" "))...)
		} else if e, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			ev = string(bytes.TrimSpace(e))
		}
	}
	return data, ev
}

func (b *guardBody) frame(f []byte) {
	data, _ := frameData(f)
	var d map[string]any
	_ = decodeJSON(data, &d) // a frame that is not JSON, such as [DONE], has no type
	typ, _ := d["type"].(string)
	if b.vendor == "anthropic" {
		idx := string(jsonText(d["index"]))
		switch typ {
		case "content_block_start":
			if cb, _ := d["content_block"].(map[string]any); cb["type"] == "tool_use" {
				name, _ := cb["name"].(string)
				id, _ := cb["id"].(string)
				b.open[idx] = &openCall{name: name, id: id}
			}
		case "content_block_delta":
			if c := b.open[idx]; c != nil {
				dl, _ := d["delta"].(map[string]any)
				s, _ := dl["partial_json"].(string)
				c.input.WriteString(s)
			}
		case "content_block_stop":
			if c := b.open[idx]; c != nil {
				delete(b.open, idx)
				in := json.RawMessage(c.input.String())
				var input any
				if decodeJSON(in, &input) != nil {
					input = nil
				}
				b.decide(idx, c.id, c.name, in, map[string]any{"name": c.name, "input": input})
			}
		}
	} else {
		switch typ {
		case "response.output_item.added":
			if it, _ := d["item"].(map[string]any); isCallItem(it["type"]) {
				id, _ := it["id"].(string)
				b.open[id] = &openCall{}
			}
		case "response.output_item.done":
			it, _ := d["item"].(map[string]any)
			id, _ := it["id"].(string)
			if b.open[id] != nil {
				delete(b.open, id)
				callID, _ := it["call_id"].(string)
				name, _ := it["name"].(string)
				b.decide(id, callID, name, inputOf(it), it)
			}
		}
	}
	if len(b.open) > 0 || len(b.held) > 0 {
		b.held = append(b.held, f)
		if len(b.open) == 0 {
			b.flush()
		}
		return
	}
	b.pass(f)
}

// inputOf is an OpenAI call item's input, as JSON, for the event.
func inputOf(it map[string]any) json.RawMessage {
	for _, k := range []string{"input", "arguments", "action"} {
		if v, ok := it[k]; ok {
			if s, ok := v.(string); ok && k == "arguments" {
				return json.RawMessage(s)
			}
			return jsonText(v)
		}
	}
	return nil
}

func jsonText(v any) []byte {
	b, _ := encodeJSON(v)
	return b
}

// decide refuses a finished call whose command a rule forbids, and records
// the refusal as an event.
func (b *guardBody) decide(key, callID, name string, input json.RawMessage, item map[string]any) {
	for _, c := range commandsOf(b.vendor, item) {
		rule, ok := forbidden(b.rules, c)
		if !ok {
			continue
		}
		b.refused[key] = rule
		if callID != "" {
			b.g.refused.add(callID, rule)
		}
		if !json.Valid(input) {
			input = nil
		}
		b.g.emit(b.ex, []event.Event{{Kind: event.ToolRefused, ToolID: callID, ToolName: name,
			ToolInput: input, Rule: rule, Text: refusalText(rule)}})
		return
	}
}

// flush hands on every held frame, renamed where refused.
func (b *guardBody) flush() {
	for _, f := range b.held {
		b.pass(f)
	}
	b.held = nil
}

// pass writes a frame, renamed when it names a refused call.
func (b *guardBody) pass(f []byte) {
	if len(b.refused) > 0 {
		f = b.rename(f)
	}
	b.out.Write(f)
}

func (b *guardBody) rename(f []byte) []byte {
	data, ev := frameData(f)
	var d map[string]any
	if decodeJSON(data, &d) != nil {
		return f
	}
	changed := false
	ren := func(it map[string]any, key string) {
		if rule, ok := b.refused[key]; ok && refuseItem(it, rule) {
			changed = true
		}
	}
	item := func(v any) {
		if it, ok := v.(map[string]any); ok {
			id, _ := it["id"].(string)
			ren(it, id)
		}
	}
	switch d["type"] {
	case "content_block_start":
		if cb, ok := d["content_block"].(map[string]any); ok {
			ren(cb, string(jsonText(d["index"])))
		}
	case "response.output_item.added", "response.output_item.done":
		item(d["item"])
	case "response.completed", "response.incomplete", "response.done", "response.failed":
		resp, _ := d["response"].(map[string]any)
		outs, _ := resp["output"].([]any)
		for _, o := range outs {
			item(o)
		}
	}
	if !changed {
		return f
	}
	j, err := encodeJSON(d)
	if err != nil {
		return f
	}
	var s bytes.Buffer
	if ev != "" {
		s.WriteString("event: " + ev + "\n")
	}
	s.WriteString("data: ")
	s.Write(j)
	s.WriteString("\n\n")
	return s.Bytes()
}

// refuseItem makes a refused call one the agent cannot run as written, and
// says whether it changed it. A named call is renamed. A local_shell_call has
// no name, so its command becomes one that prints the refusal and fails.
func refuseItem(it map[string]any, rule string) bool {
	if name, ok := it["name"].(string); ok {
		if strings.HasPrefix(name, RefusedPrefix) {
			return false
		}
		it["name"] = RefusedPrefix + name
		return true
	}
	if a, ok := it["action"].(map[string]any); ok && it["type"] == "local_shell_call" {
		a["command"] = []any{"sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", refusalText(rule)}
		return true
	}
	return false
}

// rewriteJSON renames each refused call of an unstreamed reply: Anthropic's
// content, OpenAI's output.
func (b *guardBody) rewriteJSON(all []byte) []byte {
	var m map[string]any
	if decodeJSON(all, &m) != nil {
		return all
	}
	key := "output"
	if b.vendor == "anthropic" {
		key = "content"
	}
	list, _ := m[key].([]any)
	changed := false
	for _, v := range list {
		it, ok := v.(map[string]any)
		if !ok {
			continue
		}
		var callID string
		switch {
		case b.vendor == "anthropic" && it["type"] == "tool_use":
			callID, _ = it["id"].(string)
		case b.vendor == "openai" && isCallItem(it["type"]):
			callID, _ = it["call_id"].(string)
		default:
			continue
		}
		name, _ := it["name"].(string)
		var input json.RawMessage
		if b.vendor == "anthropic" {
			input = jsonText(it["input"])
		} else {
			input = inputOf(it)
		}
		k := callID
		b.decide(k, callID, name, input, it)
		if rule, ok := b.refused[k]; ok && refuseItem(it, rule) {
			changed = true
		}
	}
	if !changed {
		return all
	}
	out, err := encodeJSON(m)
	if err != nil {
		return all
	}
	return out
}
