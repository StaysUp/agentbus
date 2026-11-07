// Package body splits a model request into pieces, so a store can keep each
// piece once under its hash and show a call's whole context later.
//
// A piece is one system block, one tool definition, one message, the
// instructions, or one input item: whatever the API format repeats from call
// to call. The same system prompt and tools arrive on every call of a session,
// and each call repeats the conversation so far, so most pieces of a call are
// already stored.
//
// Only the request body is split. Headers never reach this package: they
// carry the agent's sign-in.
package body

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Piece kinds.
const (
	System       = "system"
	Tool         = "tool"
	Message      = "message"
	Instructions = "instructions"
	InputItem    = "input_item"
)

// Piece is one part of a request.
type Piece struct {
	Kind  string
	Label string // a tool's name, a message's role; may be empty
	// JSON is the piece's bytes exactly as the request carried them.
	JSON json.RawMessage
}

// Hash is the piece's SHA-256, in hex. It names the piece in the store.
func (p Piece) Hash() string {
	h := sha256.Sum256(p.JSON)
	return hex.EncodeToString(h[:])
}

// Request is a request body split: the top-level object without the piece
// fields, and the pieces in the order the request carried them.
type Request struct {
	// Top keeps every other top-level field (model, max_tokens, stream …) in
	// the request's own order, with its value's bytes unchanged.
	Top    json.RawMessage
	Pieces []Piece
}

// Call is one model call's request, ready to keep.
type Call struct {
	Exchange string
	Session  string
	Vendor   string
	Time     time.Time
	// Size is the length of the request body the adapter read, decoded.
	Size int
	Request
}

// Splitter is what an adapter adds to have its requests kept as pieces.
type Splitter interface {
	Split(body []byte) (Request, error)
}

// Keeper keeps calls, such as the store.
type Keeper interface {
	KeepCall(Call) error
}

// KeeperFunc is a function used as a Keeper.
type KeeperFunc func(Call) error

func (f KeeperFunc) KeepCall(c Call) error { return f(c) }

// Field is one top-level field of a JSON object.
type Field struct {
	Key   string
	Value json.RawMessage
}

// ErrNotObject means the body is not one JSON object.
var ErrNotObject = errors.New("body: the request is not one JSON object")

// Fields reads a JSON object's top-level fields in order, each value's bytes
// unchanged.
func Fields(data []byte) ([]Field, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, ErrNotObject
	}
	var out []Field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, ErrNotObject
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, ErrNotObject
		}
		out = append(out, Field{Key: key, Value: v})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, ErrNotObject
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrNotObject
	}
	return out, nil
}

// Elements reads a JSON array's elements, each one's bytes unchanged. ok is
// false when raw is not an array.
func Elements(raw json.RawMessage) (out []json.RawMessage, ok bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// IsString reports whether raw is a JSON string.
func IsString(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '"' }

// Object writes fields back as one JSON object, in order.
func Object(fields []Field) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.Key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.Value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// Label reads the first non-empty string among keys from a JSON object, such
// as a tool's name or a message's role. Empty when there is none.
func Label(raw json.RawMessage, keys ...string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range keys {
		var s string
		if json.Unmarshal(m[k], &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// ApproxTokens estimates a piece's tokens from its size: bytes / 2.66, the
// characters per token measured on one Claude Code request against the API's
// own count (2026-11-05). It was off by up to 15% per piece there, and it is
// not any vendor's tokenizer.
func ApproxTokens(size int) int {
	return (size*100 + 265) / 266
}

// Reply is one model call's reply, rebuilt as the message the harness sends
// back in its next request: for Anthropic, the assistant message with every
// content block in full (text, tool_use with all its fields, thinking with its
// signature). A replay needs it exactly, so it is kept like a request piece.
type Reply struct {
	Exchange string
	Status   int
	// Stop is the vendor's own stop reason, such as end_turn.
	Stop string
	Time time.Time
	// Message is the rebuilt message's JSON.
	Message json.RawMessage
}

// ReplyBuilder is what an adapter adds to have its replies kept. Build gets a
// complete reply body, already decoded, and returns the message the harness
// would send back. It returns an error for a reply it cannot rebuild whole,
// such as a stream that ended early or carried a delta it does not know.
type ReplyBuilder interface {
	Build(status int, contentType string, body []byte) (message json.RawMessage, stop string, err error)
}

// ReplyKeeper keeps replies, such as the store. A Keeper that is also a
// ReplyKeeper is handed every reply the adapter can rebuild.
type ReplyKeeper interface {
	KeepReply(Reply) error
}
