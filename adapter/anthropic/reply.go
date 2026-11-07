package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"sort"
	"strings"

	"github.com/staysup/agentbus/body"
)

// Build rebuilds the assistant message a reply carried, as Claude Code sends
// it back in its next request: every content block in full, in order, with
// each block's fields in the order the vendor sent them. A streamed block
// starts from its content_block_start and gets its deltas: text, thinking and
// signature are appended to their fields, and input_json_delta becomes input.
// Fields the gateway does not know, such as tool_use's caller, are kept as
// sent. A replay of the turn needs this message exactly, so anything the
// gateway cannot rebuild whole is an error, never a guess.
//
// Checked against Claude Code 2.1.319 on 2026-11-06 (the own-replay
// research): for 14 of 14 replies, the message rebuilt this way equalled the
// one Claude Code sent back in its next request, thinking signatures and
// tool_use caller included. The thinking text came back empty; only the
// signature is kept, and the API accepted that.
func (Adapter) Build(status int, contentType string, data []byte) (json.RawMessage, string, error) {
	if status != 200 {
		return nil, "", errors.New("anthropic reply: not a 200")
	}
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "text/event-stream" {
		return buildStream(data)
	}
	// A reply that is not a stream is the message itself.
	fields, err := body.Fields(data)
	if err != nil {
		return nil, "", errors.New("anthropic reply: not one JSON object")
	}
	var content json.RawMessage
	var stop string
	for _, f := range fields {
		switch f.Key {
		case "content":
			content = f.Value
		case "stop_reason":
			_ = json.Unmarshal(f.Value, &stop)
		}
	}
	if _, ok := body.Elements(content); !ok {
		return nil, "", errors.New("anthropic reply: no content list")
	}
	return assistant(content), stop, nil
}

// assistant writes {"role":"assistant","content":<content>}.
func assistant(content json.RawMessage) json.RawMessage {
	return body.Object([]body.Field{{Key: "role", Value: json.RawMessage(`"assistant"`)}, {Key: "content", Value: content}})
}

// streamBlock is one content block being rebuilt.
type streamBlock struct {
	fields []body.Field
	// grown holds text, thinking and signature as they grow; the field is
	// written once the block is done.
	grown map[string]*strings.Builder
	input strings.Builder
	json  bool // an input_json_delta came
}

func buildStream(data []byte) (json.RawMessage, string, error) {
	blocks := map[int]*streamBlock{}
	var stop string
	stopped := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		if stopped {
			break
		}
		line, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type  string          `json:"type"`
			Index int             `json:"index"`
			Block json.RawMessage `json:"content_block"`
			Delta json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
			return nil, "", errors.New("anthropic reply: a stream event is not JSON")
		}
		switch ev.Type {
		case "content_block_start":
			if ev.Index < 0 || blocks[ev.Index] != nil {
				return nil, "", fmt.Errorf("anthropic reply: content block %d started twice or below 0", ev.Index)
			}
			f, err := body.Fields(ev.Block)
			if err != nil {
				return nil, "", errors.New("anthropic reply: a content block is not an object")
			}
			blocks[ev.Index] = &streamBlock{fields: f, grown: map[string]*strings.Builder{}}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return nil, "", fmt.Errorf("anthropic reply: a delta for block %d before its start", ev.Index)
			}
			if err := b.apply(ev.Delta); err != nil {
				return nil, "", err
			}
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(strings.TrimSpace(line)), &d)
			if d.Delta.StopReason != "" {
				stop = d.Delta.StopReason
			}
		case "message_stop":
			stopped = true
		case "error":
			return nil, "", errors.New("anthropic reply: the stream reported an error")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, "", errors.New("anthropic reply: the stream could not be read")
	}
	if !stopped {
		return nil, "", errors.New("anthropic reply: the stream ended before message_stop")
	}
	idx := make([]int, 0, len(blocks))
	for i := range blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var content bytes.Buffer
	content.WriteByte('[')
	for n, i := range idx {
		raw, err := blocks[i].done()
		if err != nil {
			return nil, "", err
		}
		if n > 0 {
			content.WriteByte(',')
		}
		content.Write(raw)
	}
	content.WriteByte(']')
	return assistant(content.Bytes()), stop, nil
}

// maxLine is the longest stream line read, as in readStream.
const maxLine = 64 * 1024 * 1024

// grownFields are the string fields a delta appends to, by delta type.
var grownFields = map[string]string{
	"text_delta":      "text",
	"thinking_delta":  "thinking",
	"signature_delta": "signature",
}

func (b *streamBlock) apply(raw json.RawMessage) error {
	var d map[string]json.RawMessage
	if json.Unmarshal(raw, &d) != nil {
		return errors.New("anthropic reply: a delta is not an object")
	}
	var typ string
	_ = json.Unmarshal(d["type"], &typ)
	if field, ok := grownFields[typ]; ok {
		var s string
		if json.Unmarshal(d[field], &s) != nil {
			return fmt.Errorf("anthropic reply: a %s without its %s", typ, field)
		}
		g := b.grown[field]
		if g == nil {
			g = &strings.Builder{}
			// The start block's own value comes first, usually "".
			if v, ok := b.get(field); ok {
				var start string
				if json.Unmarshal(v, &start) != nil {
					return fmt.Errorf("anthropic reply: a block's %s is not a string", field)
				}
				g.WriteString(start)
			}
			b.grown[field] = g
		}
		g.WriteString(s)
		return nil
	}
	if typ == "input_json_delta" {
		var s string
		if json.Unmarshal(d["partial_json"], &s) != nil {
			return errors.New("anthropic reply: an input_json_delta without partial_json")
		}
		b.input.WriteString(s)
		b.json = true
		return nil
	}
	// citations_delta and any later delta: the gateway cannot say where it
	// goes in the block, so the reply is not rebuilt.
	return fmt.Errorf("anthropic reply: a delta of type %q the gateway cannot rebuild", typ)
}

func (b *streamBlock) get(key string) (json.RawMessage, bool) {
	for _, f := range b.fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// set replaces a field's value in place, or appends the field.
func (b *streamBlock) set(key string, v json.RawMessage) {
	for i, f := range b.fields {
		if f.Key == key {
			b.fields[i].Value = v
			return
		}
	}
	b.fields = append(b.fields, body.Field{Key: key, Value: v})
}

func (b *streamBlock) done() (json.RawMessage, error) {
	for _, k := range []string{"text", "thinking", "signature"} {
		if g := b.grown[k]; g != nil {
			b.set(k, jsonString(g.String()))
		}
	}
	if b.json {
		in := b.input.String()
		if in == "" {
			in = "{}"
		}
		var c bytes.Buffer
		if json.Compact(&c, []byte(in)) != nil {
			return nil, errors.New("anthropic reply: a tool input is not JSON")
		}
		b.set("input", c.Bytes())
	}
	return body.Object(b.fields), nil
}

// jsonString encodes s without escaping <, > and &, as Claude Code's own
// JSON does.
func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
