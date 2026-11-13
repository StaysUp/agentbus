// Package adapter defines what a vendor adapter does.
//
// An adapter knows one API format: how a request, a reply, a tool call and a
// turn end look on the wire. It turns them into events. It never changes the
// traffic, and it never sees headers, so it cannot see a sign-in.
package adapter

import "github.com/staysup/agentbus/event"

type Adapter interface {
	// Vendor names the API format, such as "anthropic".
	Vendor() string

	// Calls are the calls this adapter's gateway records (calls.go): its
	// model calls, which it reads, and any other call its harness needs that
	// the gateway records by method, path and status. It is the one list of
	// what a gateway adds a credential to. A gateway passes any other call on
	// with the agent's own sign-in, unrecorded, and with a credential of
	// control's refuses it.
	Calls() []Call

	// RequestEvents reads a model request body.
	RequestEvents(body []byte) ([]event.Event, error)

	// TurnBasis returns what names a request's turn: the conversation up to and
	// including the message that started the turn, in a canonical form, so the
	// same conversation always gives the same bytes. It leaves out anything
	// that moves between requests of one conversation, such as cache markers.
	// The gateway hashes it with the session id. It returns nil when the
	// request names no turn.
	TurnBasis(body []byte) []byte

	// ResponseEvents reads a complete reply body, already decompressed. With
	// an error it may still return the schema.unexpected events it collected
	// before the error; the caller keeps those and nothing else.
	ResponseEvents(status int, contentType string, body []byte) ([]event.Event, error)
}
