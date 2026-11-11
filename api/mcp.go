package api

// POST /api/v0/mcp: the API as tools, over the Model Context Protocol's
// Streamable HTTP transport, stateless. Each POST carries one JSON-RPC
// message and gets one JSON answer; no session id is issued and no stream is
// held, so nothing outlives the token: a call after the turn's token stopped
// working is refused 401 by ServeHTTP before it gets here.
//
// tools/call is sent back through ServeHTTP as the HTTP request it names, with
// the same Authorization header, Host and client address (tools.go): the
// token check, [agent_api], [limits] and the handler decide it as they decide
// that HTTP call. Nothing here grants anything.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
)

// mcpVersions are the protocol versions this server speaks, newest first. A
// client asking for another gets the newest, and decides.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// mcpInstructions is the server's own instructions, which a client may show
// its model.
const mcpInstructions = "AgentBus tools: ask other saved agents (any vendor) for work and reviews, " +
	"read, stop and apply turns, and search this session's history. Your folder is your session's own copy; " +
	"apply_turn puts a turn's changes in the person's folder. Work you ask for comes back to your session as a later turn."

// JSON-RPC error codes.
const (
	rpcParse          = -32700
	rpcInvalid        = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternal       = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpTool is one entry of tools/list.
type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// textContent is a tool result's one piece of text.
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// POST /api/v0/mcp
func (a *API) mcp(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body could not be read")
		return
	}
	reply := func(id json.RawMessage, result any, e *rpcError) {
		if id == nil {
			id = json.RawMessage("null")
		}
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
	}
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		reply(nil, nil, &rpcError{rpcInvalid, "send one JSON-RPC message per request; batches are not taken"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(data, &req); err != nil {
		reply(nil, nil, &rpcError{rpcParse, "the body is not one JSON-RPC message"})
		return
	}
	if req.JSONRPC != "2.0" {
		reply(req.ID, nil, &rpcError{rpcInvalid, "jsonrpc must be 2.0"})
		return
	}
	if len(req.ID) == 0 {
		// A notification, such as notifications/initialized: taken, with
		// no answer, and never run.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !rpcID(req.ID) {
		// MCP's id is a string or an integer, never null. Anything else is
		// refused before anything runs (found in review: an object id ran
		// a rename).
		reply(nil, nil, &rpcError{rpcInvalid, "id must be a string or an integer"})
		return
	}
	if req.Method == "" {
		// A response to a request this server never sends.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		reply(req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "agentbus", "version": a.cfg.Build.Version},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		reply(req.ID, map[string]any{"tools": a.toolList(r)}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			reply(req.ID, nil, &rpcError{rpcInvalidParams, "params must be an object with name and arguments"})
			return
		}
		tr, ok := a.toolNamed(p.Name)
		if !ok {
			reply(req.ID, nil, &rpcError{rpcInvalidParams, "no such tool; tools/list names them"})
			return
		}
		reply(req.ID, a.callTool(r, tr, p.Arguments), nil)
	default:
		reply(req.ID, nil, &rpcError{rpcMethodNotFound, "this server answers initialize, ping, tools/list and tools/call"})
	}
}

// rpcID says id is a JSON string or an integer.
func rpcID(id json.RawMessage) bool {
	var v any
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return true
	case json.Number:
		_, err := x.Int64()
		return err == nil
	}
	return false
}

// toolList is the tools r's token sees (listed).
func (a *API) toolList(r *http.Request) []mcpTool {
	g, turnsOnly := grantOf(r), a.turnsOnly(r)
	perm := policyOf(r)
	out := []mcpTool{}
	for _, tr := range a.tools {
		if !listed(tr, g, turnsOnly, perm.AgentAPI) {
			continue
		}
		out = append(out, mcpTool{Name: tr.t.name, Title: tr.rt.summary, Description: tr.t.doc,
			InputSchema: a.toolSchemas[tr.t.name], Annotations: annotations(tr.t.effect)})
	}
	return out
}

// annotations are MCP's hints for what a tool changes. destructiveHint is
// false for every tool: whether a turn may apply is the token's and
// [agent_api]'s to say, and an apply refuses a file the person changed. codex
// asks for approval before a tool marked destructive, and a turn has nobody
// to ask: it refused apply_turn with "MCP tool call requires
// approval, but approval policy is never" (codex-cli 0.164.0, 2026-11-11, the
// fresh-agent test).
func annotations(effect string) map[string]any {
	return map[string]any{
		"readOnlyHint":    effect == effectNone,
		"destructiveHint": false,
		"idempotentHint":  effect == effectNone || effect == effectApply || effect == effectRename,
		"openWorldHint":   false,
	}
}

// callTool sends one tool call through ServeHTTP, as the HTTP request it
// names, and answers what that request answered.
func (a *API) callTool(r *http.Request, tr toolRoute, args map[string]any) toolResult {
	fail := func(msg string) toolResult {
		return toolResult{Content: []textContent{{"text", msg}}, IsError: true}
	}
	own := ""
	if g := grantOf(r); g != nil {
		own = g.Session
	}
	method, target, body, why := toolRequest(tr, a.toolSchemas[tr.t.name], args, own)
	if why != "" {
		return fail(why)
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	inner, err := http.NewRequestWithContext(r.Context(), method, target, rd)
	if err != nil {
		return fail("the call could not be made")
	}
	inner.Host, inner.RemoteAddr = r.Host, r.RemoteAddr
	inner.Header.Set("Authorization", r.Header.Get("Authorization"))
	if ua := r.Header.Get("User-Agent"); ua != "" {
		inner.Header.Set("User-Agent", ua)
	}
	if body != nil {
		inner.Header.Set("Content-Type", "application/json")
	}
	rec := &toolRecorder{header: http.Header{}}
	// Through the http.Handler, as any request: the route table serves this
	// handler, so a direct call would make the table refer to itself.
	http.Handler(a).ServeHTTP(rec, inner)
	text := strings.TrimSpace(rec.body.String())
	if rec.code >= 400 {
		return fail(toolStatus(rec.code) + ": " + text)
	}
	if text == "" {
		text = toolStatus(rec.code)
	}
	return toolResult{Content: []textContent{{"text", text}}}
}

// turnsOnly says r was sent with the turns token.
func (a *API) turnsOnly(r *http.Request) bool {
	who, _, ok := a.authorized(r)
	return ok && who == byTurns
}

// toolRecorder keeps the answer of one tool call's request.
type toolRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (t *toolRecorder) Header() http.Header { return t.header }
func (t *toolRecorder) WriteHeader(code int) {
	if t.code == 0 {
		t.code = code
	}
}
func (t *toolRecorder) Write(b []byte) (int, error) {
	if t.code == 0 {
		t.code = http.StatusOK
	}
	return t.body.Write(b)
}
