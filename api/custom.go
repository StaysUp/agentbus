package api

// POST /api/v0/events: a program writes its own event. Its kind is always
// x.<program>.<name>, so a program can never write a framework kind.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"github.com/staysup/agentbus/event"
)

var customKind = regexp.MustCompile(`^x\.[a-z0-9][a-z0-9_-]{0,31}\.[a-z0-9][a-z0-9_.-]{0,63}$`)

// Bounds of a program's own event, in bytes.
const (
	MaxEventBody = 128 << 10
	MaxEventText = 64 << 10
	MaxEventData = 64 << 10
	MaxEventTurn = 128
)

const badEvent = "the body must be one JSON object with kind and session, and optionally turn, text and data, at most 128 KiB"

// EventRequest is the body of POST /api/v0/events. omitempty marks a field
// as optional in the API's description.
type EventRequest struct {
	Kind    string          `json:"kind" doc:"x., then the program's name, then the event's name."`
	Session string          `json:"session" doc:"A session the store already has events for, or default."`
	Turn    string          `json:"turn,omitempty"`
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty" doc:"A JSON object."`
}

// Written is the answer to POST /api/v0/events.
type Written struct {
	Seq int64  `json:"seq"`
	ID  string `json:"id"`
}

func (a *API) writeEvent(w http.ResponseWriter, r *http.Request) {
	var body EventRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badEvent)
		return
	}
	if !customKind.MatchString(body.Kind) {
		writeError(w, http.StatusBadRequest, `kind must match ^x\.[a-z0-9][a-z0-9_-]{0,31}\.[a-z0-9][a-z0-9_.-]{0,63}$`)
		return
	}
	if !sessionID.MatchString(body.Session) {
		writeError(w, http.StatusBadRequest, "session must be a session id or default")
		return
	}
	if len(body.Turn) > MaxEventTurn || len(body.Text) > MaxEventText {
		writeError(w, http.StatusBadRequest, "turn is at most 128 bytes and text at most 64 KiB")
		return
	}
	if len(body.Data) > 0 && (len(body.Data) > MaxEventData || body.Data[0] != '{') {
		// The decoder has trimmed the value and checked it is valid JSON.
		writeError(w, http.StatusBadRequest, "data must be a JSON object, at most 64 KiB")
		return
	}
	if body.Session != "default" {
		known, err := a.cfg.Store.Known(body.Session)
		if err != nil {
			a.internal(w)
			return
		}
		if !known {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
	}
	e := event.Event{ID: newEventID(), Kind: event.Kind(body.Kind), Time: a.cfg.Now().UTC(),
		Session: body.Session, Turn: body.Turn, Vendor: "program", Text: body.Text, Data: body.Data}
	out, _, err := a.cfg.Store.Append(e)
	if err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusCreated, Written{out.Seq, out.ID})
}

// decodeObject reads one JSON object, then the end of the body, into v. A body
// cut at its limit, anything after the object, a field v does not have, and a
// value that is not an object (null, an array, a string, a number) are all
// refused.
func decodeObject(r *http.Request, v any) bool {
	data, err := io.ReadAll(r.Body)
	if t := bytes.TrimLeft(data, " \t\r\n"); err != nil || len(t) == 0 || t[0] != '{' {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil && !dec.More() && dec.Decode(&struct{}{}) == io.EOF
}
