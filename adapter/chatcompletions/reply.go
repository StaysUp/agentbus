package chatcompletions

// Chat Completions replies kept for own replay. The layout was measured
// against llama.cpp llama-server b12578 (Qwen2.5-0.5B-Instruct, --jinja) on
// 2026-11-20, and against OpenCode 1.19.6's @ai-sdk/openai-compatible
// provider (2026-11-21): a resumed turn sends the earlier assistant message
// back as a messages entry, with content a string ("" when the reply only
// called a tool) and tool_calls as the vendor sent them.

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

// Build keeps a Chat Completions reply as the assistant message the harness
// would send back: choices[0].message of a whole reply, its bytes as the
// vendor sent them; or the same object rebuilt from a stream's deltas. The
// stop reason is finish_reason, as on model.responded.
//
// Only a whole reply is kept. A stream that ended before finish_reason, or
// reported an error, gives an error: a replay never carries part of a reply.
func (Adapter) Build(status int, contentType string, data []byte) (json.RawMessage, string, error) {
	if status != 200 {
		return nil, "", errors.New("chat completions reply: not a 200")
	}
	if mt, _, _ := mime.ParseMediaType(contentType); mt == "text/event-stream" || isStream(contentType, data) {
		return buildStream(data)
	}
	return buildWhole(data)
}

func buildWhole(data []byte) (json.RawMessage, string, error) {
	var whole struct {
		Choices []struct {
			Index        int             `json:"index"`
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &whole); err != nil {
		return nil, "", errors.New("chat completions reply: not one JSON object")
	}
	for _, c := range whole.Choices {
		if c.Index != 0 {
			continue
		}
		if len(c.Message) == 0 || c.Message[0] != '{' {
			return nil, "", errors.New("chat completions reply: no assistant message")
		}
		if c.FinishReason == "" {
			return nil, "", errors.New("chat completions reply: no finish_reason")
		}
		return c.Message, c.FinishReason, nil
	}
	return nil, "", errors.New("chat completions reply: no choices")
}

func buildStream(data []byte) (json.RawMessage, string, error) {
	var content, refusal strings.Builder
	calls := map[int]*toolCall{}
	var finish string
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
		var ch struct {
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
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			return nil, "", errors.New("chat completions reply: a stream chunk is not JSON")
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			return nil, "", errors.New("chat completions reply: the stream reported an error")
		}
		for _, c := range ch.Choices {
			if c.Index != 0 {
				continue
			}
			if c.Delta.Content != nil {
				content.WriteString(*c.Delta.Content)
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
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, "", errors.New("chat completions reply: the stream could not be read")
	}
	if finish == "" {
		return nil, "", errors.New("chat completions reply: the stream ended before finish_reason")
	}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var tcs []toolCall
	for _, i := range idx {
		tc := *calls[i]
		if tc.Type == "" {
			tc.Type = "function"
		}
		tcs = append(tcs, tc)
	}
	text := content.String()
	if text == "" {
		text = refusal.String()
	}
	return assistantMessage(text, tcs), finish, nil
}

func assistantMessage(content string, calls []toolCall) json.RawMessage {
	fields := []body.Field{
		{Key: "role", Value: json.RawMessage(`"assistant"`)},
		{Key: "content", Value: jsonText(content)},
	}
	if len(calls) > 0 {
		raw := make([]json.RawMessage, len(calls))
		for i, c := range calls {
			raw[i] = encodeToolCall(c)
		}
		fields = append(fields, body.Field{Key: "tool_calls", Value: joinArray(raw)})
	}
	return body.Object(fields)
}

func encodeToolCall(c toolCall) json.RawMessage {
	fn := body.Object([]body.Field{
		{Key: "name", Value: jsonText(c.Function.Name)},
		{Key: "arguments", Value: jsonText(c.Function.Arguments)},
	})
	typ := c.Type
	if typ == "" {
		typ = "function"
	}
	return body.Object([]body.Field{
		{Key: "id", Value: jsonText(c.ID)},
		{Key: "type", Value: jsonText(typ)},
		{Key: "function", Value: fn},
	})
}

func jsonText(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func joinArray(els []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range els {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(e)
	}
	b.WriteByte(']')
	return b.Bytes()
}
