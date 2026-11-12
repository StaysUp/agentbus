package api

// GET /api/v0/turns/{id}/calls/{call}: one model call's request, as pieces.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/staysup/agentbus/store"
)

func (a *API) callContext(w http.ResponseWriter, r *http.Request, exchange string) {
	full := true
	if v := r.URL.Query().Get("full"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "full must be true or false")
			return
		}
		full = b
	}
	c, found, err := a.cfg.Store.CallContext(exchange, full)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no request body is stored for this call")
		return
	}
	out, err := encodeContext(c)
	if err != nil {
		a.internal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// encodeContext writes the reply with each piece's json and the request as the
// stored bytes, unchanged. encoding/json would compact a RawMessage and escape
// <, > and & in it, so the bytes returned would no longer match the piece's
// hash and size (40 bytes stored, 45 returned). Every other field goes through
// encoding/json. The stored bytes are valid JSON: they came out of a JSON
// decoder.
func encodeContext(c store.CallContext) ([]byte, error) {
	pieces, request, reply := c.Pieces, c.Request, c.Reply
	c.Pieces, c.Request, c.Reply = nil, nil, nil
	head, err := marshal(c)
	if err != nil {
		return nil, err
	}
	// head ends with "pieces":null} (request is first set to null, below).
	head, ok := bytes.CutSuffix(head, []byte(`"pieces":null}`))
	if !ok {
		return nil, errEncode
	}
	head, ok = bytes.CutSuffix(head, []byte(`"request":null,`))
	if !ok {
		return nil, errEncode
	}
	var b bytes.Buffer
	b.Write(head)
	if reply != nil {
		b.WriteString(`"reply":`)
		if err := writePiece(&b, *reply); err != nil {
			return nil, err
		}
		b.WriteByte(',')
	}
	b.WriteString(`"request":`)
	b.Write(request)
	b.WriteString(`,"pieces":[`)
	for i, p := range pieces {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writePiece(&b, p); err != nil {
			return nil, err
		}
	}
	b.WriteString("]}\n")
	return b.Bytes(), nil
}

// writePiece writes one piece with its json as the stored bytes.
func writePiece(b *bytes.Buffer, p store.Piece) error {
	raw := p.JSON
	p.JSON = nil
	m, err := marshal(p)
	if err != nil {
		return err
	}
	if raw == nil {
		b.Write(m)
		return nil
	}
	b.Write(m[:len(m)-1]) // without its closing brace
	b.WriteString(`,"json":`)
	b.Write(raw)
	b.WriteByte('}')
	return nil
}

var errEncode = errors.New("api: the call could not be encoded")

// marshal encodes v without HTML escaping and without the trailing newline.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// callContextDoc is the route's description.
const callContextDoc = "The gateway keeps each model request's body, split into pieces: each system block, " +
	"tool definition and message (Anthropic Messages), or the instructions, each tool and each input item " +
	"(OpenAI Responses). Each piece is stored once under its SHA-256, so a system prompt sent on every call is kept once. " +
	"request holds every other top-level field. reply, when kept, is the model's reply rebuilt as the message the harness sends back next. " +
	"For a replayed turn the stored request is the one the vendor received, with the earlier turn's history in front. Bodies are kept in full, unencrypted, until the data folder is removed; headers are never stored. A call has no stored body when it is not a " +
	"model call, when the gateway could not read the request, or when the store fell behind."
