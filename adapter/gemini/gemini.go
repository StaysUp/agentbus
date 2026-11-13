// Package gemini reads Google's Gemini generateContent format, as the
// Antigravity CLI (agy) sends it to Code Assist.
//
// agy 1.5.5 signed in with a Google account sends every model call to
// https://daily-cloudcode-pa.googleapis.com as POST
// /v1internal:streamGenerateContent?alt=sse (measured 2026-11-11 through a
// pass-through tap). Code Assist wraps the Gemini format: a request is
// {"model", "project", "requestId", "request": {contents, systemInstruction,
// tools, generationConfig, ...}} and each streamed chunk is {"response":
// {candidates, usageMetadata, modelVersion, responseId}, "traceId"}. The plain
// Gemini API sends the same objects without the wrapper; both are read.
//
// What the same capture showed, which this file relies on:
//   - a tool's result goes back as a functionResponse part in a content of
//     role "model", not "user";
//   - a reply that calls a tool also ends with finishReason "STOP", so a
//     reply ends the turn only when it calls no tool;
//   - functionCall parts carry an id, and the functionResponse repeats it.
package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

type Adapter struct{}

func (Adapter) Vendor() string { return "gemini" }

// Calls: the model calls, a POST of :generateContent or
// :streamGenerateContent, Code Assist's /v1internal:streamGenerateContent or
// the Gemini API's /v1beta/models/<model>:streamGenerateContent, streamed
// with alt=sse or as a JSON list without it. A reply in another format, as
// alt=proto asks for, is not read, so that query is not one of them.
//
// And the Code Assist methods agy 1.5.6 calls before its first model call,
// each a POST with no query and a JSON reply: without loadCodeAssist it stops
// ("Eligibility check failed"), and it then calls the others (measured
// 2026-11-13 against a local listener). They are recorded by status
// (vendor.called), their bodies unread.
func (Adapter) Calls() []adapter.Call {
	alt := map[string][]string{"alt": {"sse"}}
	cs := []adapter.Call{
		{Method: "POST", Path: "/v1internal:streamGenerateContent", Query: alt, Replies: adapter.JSONOrSSEOrNone},
		{Method: "POST", Path: "/v1internal:generateContent", Replies: adapter.JSONOrSSEOrNone},
		{Method: "POST", Path: "/v1beta/models/*:streamGenerateContent", Query: alt, Replies: adapter.JSONOrSSEOrNone},
		{Method: "POST", Path: "/v1beta/models/*:generateContent", Replies: adapter.JSONOrSSEOrNone},
	}
	for _, m := range []string{"loadCodeAssist", "listExperiments", "fetchAdminControls", "fetchAvailableModels",
		"fetchUserInfo", "retrieveUserQuotaSummary"} {
		cs = append(cs, adapter.Call{Method: "POST", Path: "/v1internal:" + m, Replies: adapter.JSONOnly, Service: true})
	}
	return cs
}

type part struct {
	Text             *string         `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	FunctionCall     *functionCall   `json:"functionCall,omitempty"`
	FunctionResponse *functionResult `json:"functionResponse,omitempty"`
}

type functionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type functionResult struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type inner struct {
	Contents []content `json:"contents"`
	Tools    []struct {
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
}

// request is a Code Assist request, or a plain Gemini one: then Request is
// nil and the inner fields are at the top.
type request struct {
	Model   string `json:"model"`
	Request *inner `json:"request"`
	inner
}

func (r *request) body() *inner {
	if r.Request != nil {
		return r.Request
	}
	return &r.inner
}

func decodeRequest(b []byte) (*request, error) {
	var r request
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// results are the functionResponse parts of c.
func results(c content) []*functionResult {
	var out []*functionResult
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			out = append(out, p.FunctionResponse)
		}
	}
	return out
}

// text is the text parts of c joined, thoughts left out.
func text(parts []part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != nil && !p.Thought {
			b.WriteString(*p.Text)
		}
	}
	return b.String()
}

// turnStart is the index of the content that started the current turn: the
// last content of role user that carries no tool result. -1 when there is
// none.
func turnStart(cs []content) int {
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Role == "user" && len(results(cs[i])) == 0 {
			return i
		}
	}
	return -1
}

// personsWords is the person's request inside agy's <USER_REQUEST> tag, which
// agy 1.5.5 puts around it with metadata after it (the time, a changed
// setting). Text without the tag is kept whole.
func personsWords(s string) string {
	const open, closing = "<USER_REQUEST>", "</USER_REQUEST>"
	if i := strings.Index(s, open); i >= 0 {
		rest := s[i+len(open):]
		if j := strings.Index(rest, closing); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	return s
}

// TurnBasis is the conversation up to and including the content that started
// the turn: each content's role and parts, re-encoded so key order and
// spacing do not matter, with thoughtSignature left out, an opaque value the
// harness may drop from earlier contents.
func (Adapter) TurnBasis(b []byte) []byte {
	r, err := decodeRequest(b)
	if err != nil {
		return nil
	}
	cs := r.body().Contents
	start := turnStart(cs)
	if start < 0 {
		return nil
	}
	var raw struct {
		Request *struct {
			Contents []json.RawMessage `json:"contents"`
		} `json:"request"`
		Contents []json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	list := raw.Contents
	if raw.Request != nil {
		list = raw.Request.Contents
	}
	conv := make([]any, 0, start+1)
	for _, c := range list[:start+1] {
		v, err := canon(c)
		if err != nil {
			return nil
		}
		conv = append(conv, v)
	}
	out, err := json.Marshal(conv)
	if err != nil {
		return nil
	}
	return out
}

// canon decodes raw with numbers kept as written and every thoughtSignature
// removed. json.Marshal then writes keys sorted and no spacing.
func canon(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var strip func(any)
	strip = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			delete(t, "thoughtSignature")
			for _, e := range t {
				strip(e)
			}
		case []any:
			for _, e := range t {
				strip(e)
			}
		}
	}
	strip(v)
	return v, nil
}

// RequestEvents: a request whose last content carries tool results continues
// a turn and reports each result. One whose last content is the person's
// words starts a turn.
func (Adapter) RequestEvents(b []byte) ([]event.Event, error) {
	r, err := decodeRequest(b)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	in := r.body()
	var evs []event.Event
	if n := len(in.Contents); n > 0 {
		last := in.Contents[n-1]
		if res := results(last); len(res) > 0 {
			for _, fr := range res {
				evs = append(evs, event.Event{Kind: event.ToolCompleted, ToolID: callID(fr.ID, fr.Name),
					ToolOutput: resultText(fr.Response)})
			}
		} else if last.Role == "user" {
			evs = append(evs, event.Event{Kind: event.TurnStarted, Text: personsWords(text(last.Parts))})
		}
	}
	tools := []string{}
	for _, t := range in.Tools {
		for _, d := range t.FunctionDeclarations {
			tools = append(tools, d.Name)
		}
	}
	var u adapter.Unknowns
	for _, c := range in.Contents {
		if c.Role != "user" && c.Role != "model" {
			u.Value("request.contents[].role", c.Role)
		}
	}
	evs = append(evs, event.Event{Kind: event.ModelRequested, Model: r.Model, ToolsOffered: tools})
	return append(evs, u.Events()...), nil
}

// callID is a tool call's id, or its name when the vendor sent no id: the
// Gemini API's own calls carry none.
func callID(id, name string) string {
	if id != "" {
		return id
	}
	return name
}

// resultText is a functionResponse's output: the "output" string agy puts in
// its response object, else the response as JSON.
func resultText(raw json.RawMessage) string {
	var o struct {
		Output *string `json:"output"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Output != nil {
		return *o.Output
	}
	return string(raw)
}

type usage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

type chunk struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *usage `json:"usageMetadata"`
	ModelVersion  string `json:"modelVersion"`
}

// envelope is a Code Assist chunk, or a plain Gemini one: then Response is
// nil and the chunk's fields are at the top.
type envelope struct {
	Response *chunk `json:"response"`
	chunk
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (e *envelope) body() *chunk {
	if e.Response != nil {
		return e.Response
	}
	return &e.chunk
}

// reply is a whole reply, rebuilt from its chunks.
type reply struct {
	model  string
	parts  []part
	finish string
	usage  *usage
}

func (r *reply) add(c *chunk) {
	if c.ModelVersion != "" {
		r.model = c.ModelVersion
	}
	if c.UsageMetadata != nil {
		r.usage = c.UsageMetadata
	}
	// Only the first candidate: agy asks for one.
	if len(c.Candidates) > 0 {
		r.parts = append(r.parts, c.Candidates[0].Content.Parts...)
		if f := c.Candidates[0].FinishReason; f != "" {
			r.finish = f
		}
	}
}

// maxError bounds a vendor error message kept in an event.
const maxError = 1024

func shortError(msg string) string {
	if len(msg) > maxError {
		msg = strings.ToValidUTF8(msg[:maxError], "") + " (cut)"
	}
	return strings.Clone(msg)
}

// errorMessage is the message of a Google error body: an object
// {"error":{"message"}}, or a list of them, as a stream's error comes. Else
// the body as text.
func errorMessage(b []byte) string {
	var one envelope
	if json.Unmarshal(b, &one) == nil && one.Error != nil && one.Error.Message != "" {
		return one.Error.Message
	}
	var list []envelope
	if json.Unmarshal(b, &list) == nil && len(list) > 0 && list[0].Error != nil && list[0].Error.Message != "" {
		return list[0].Error.Message
	}
	return strings.TrimSpace(string(b))
}

// ResponseEvents reads a reply: server-sent events ("data: {...}" lines, as
// alt=sse gives), a JSON list of chunks (a stream without alt=sse), or one
// JSON object (generateContent).
func (Adapter) ResponseEvents(status int, contentType string, b []byte) ([]event.Event, error) {
	if status != 200 {
		return []event.Event{{Kind: event.ModelResponded, Status: status, Error: shortError(errorMessage(b))}}, nil
	}
	var r reply
	var problem string
	trimmed := bytes.TrimSpace(b)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		var list []envelope
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("gemini reply: %w", err)
		}
		for i := range list {
			if list[i].Error != nil {
				problem = shortError(list[i].Error.Message)
				break
			}
			r.add(list[i].body())
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		var one envelope
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, fmt.Errorf("gemini reply: %w", err)
		}
		if one.Error != nil {
			problem = shortError(one.Error.Message)
		} else {
			r.add(one.body())
		}
	default:
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data:")
			if !ok {
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e envelope
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				return nil, fmt.Errorf("gemini reply: a stream event is not JSON")
			}
			if e.Error != nil {
				problem = shortError(e.Error.Message)
				break
			}
			r.add(e.body())
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("gemini reply: %w", err)
		}
	}
	if problem == "" && r.finish == "" {
		problem = "the reply ended without a finish reason"
	}
	var u adapter.Unknowns
	if r.finish != "" && !knownFinish.Has(r.finish) {
		u.Value("response.candidates[].finishReason", strings.ToLower(r.finish))
	}
	return append(fromReply(r, status, problem), u.Events()...), nil
}

// knownFinish are the finish reasons of the Gemini API reference.
var knownFinish = adapter.Names("STOP", "MAX_TOKENS", "SAFETY", "RECITATION", "LANGUAGE", "OTHER", "BLOCKLIST",
	"PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY", "UNEXPECTED_TOOL_CALL",
	"FINISH_REASON_UNSPECIFIED")

// fromReply turns a reply into events. A reply with a problem reports only
// model.responded.
func fromReply(r reply, status int, problem string) []event.Event {
	resp := event.Event{Kind: event.ModelResponded, Model: r.model, Text: text(r.parts), StopReason: r.finish,
		Status: status, Error: problem}
	if u := r.usage; u != nil {
		// promptTokenCount counts the cached tokens too, and thoughts are
		// paid as output: the same split as the other adapters'.
		resp.Usage = &event.Usage{
			InputTokens:     u.PromptTokenCount - u.CachedContentTokenCount,
			OutputTokens:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
			CacheReadTokens: u.CachedContentTokenCount,
		}
	}
	evs := []event.Event{resp}
	if problem != "" {
		return evs
	}
	calls := 0
	for _, p := range r.parts {
		fc := p.FunctionCall
		if fc == nil {
			continue
		}
		calls++
		e := event.Event{Kind: event.ToolRequested, ToolID: callID(fc.ID, fc.Name), ToolName: fc.Name}
		if len(fc.Args) == 0 || json.Valid(fc.Args) {
			e.ToolInput = fc.Args
		} else {
			e.Error = "the tool input is not valid JSON"
		}
		evs = append(evs, e)
	}
	// A reply that calls a tool also ends in STOP: the harness calls the
	// model again with the results, so the turn goes on.
	if calls == 0 {
		evs = append(evs, event.Event{Kind: event.TurnCompleted, Outcome: turnEnd(r.finish), StopReason: r.finish})
	}
	return evs
}

// turnEnd maps a finish reason to the framework's outcome.
func turnEnd(finish string) event.Outcome {
	switch finish {
	case "STOP":
		return event.Finished
	case "MAX_TOKENS":
		return event.Partial
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return event.Refused
	default:
		return event.Unknown
	}
}
