package gateway

// One rule for every call that gets a credential of control's, through
// control's relay for a runner's turn (ServeRelayed) or through a gateway of
// agentbus up (SetCredential): the credential goes only on a call the
// vendor's adapter records (adapter.Call: method, path and query), and the
// reply passes on only in a format the adapter reads. Anything else is
// refused, gets no credential, and is recorded as call.refused with why, so
// no call carries a credential of control's that the record does not hold.
// A gateway that passes the agent's own sign-in on is not held to it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

// Why a call with a credential of control's is refused.
const (
	notRecorded = "control adds a credential only to the calls the vendor's adapter records, and this method and path is not one"
	queryUnread = "control adds a credential only to the calls the vendor's adapter records, and this call's query is not one it reads"
	// unreadable: the reply, sent with the credential, is not passed on.
	unreadable = "the vendor's reply is in a format the adapter does not read, so control did not pass it on"
)

// errUnreadable is modifyResponse's answer to such a reply.
var errUnreadable = errors.New(unreadable)

// unrecorded is why a call that would get a credential of control's is
// refused, or "" when the adapter records it. A WebSocket try on a model
// path is left to serve, which answers 426 and sends nothing.
func (g *Gateway) unrecorded(r *http.Request) string {
	c, ok := adapter.Find(g.adapter, r.Method, r.URL.Path)
	switch {
	case r.Header.Get("Upgrade") != "" && adapter.Watches(g.adapter, http.MethodPost, r.URL.Path):
		return ""
	case !ok:
		return notRecorded
	case !c.Admits(r.URL.RawQuery):
		return queryUnread
	}
	return ""
}

// maxCallName bounds a call's name in an event: the path is the agent's.
const maxCallName = 256

// callName is r's method and vendor path, never its query.
func callName(r *http.Request) string {
	n := r.Method + " " + r.URL.Path
	if len(n) > maxCallName {
		n = n[:maxCallName]
	}
	return strings.ToValidUTF8(n, "?")
}

// refuseCall answers a call that gets no credential with 403, sends it
// nowhere, and records it as call.refused in session and turn. Its caller
// holds the turn open (holdThenRead), and the exchange is counted before the
// body is closed: closing waits for an upload still coming, and the turn
// must not settle before its refusal is recorded.
func (g *Gateway) refuseCall(w http.ResponseWriter, r *http.Request, session, turn string, relayed bool, why string) {
	ex := &exchange{id: fmt.Sprintf("x-%s-%d", g.run, g.seq.Add(1)), session: session, client: clientName(r.UserAgent()),
		turn: turn, start: g.now(), relayed: relayed}
	if turn != "" {
		g.track(ex, session, turn)
	}
	_ = r.Body.Close()
	g.emit(ex, []event.Event{{Kind: event.CallRefused, Call: callName(r), Status: http.StatusForbidden, Error: why}})
	g.flushed(ex.done)
	if g.logf != nil {
		g.logf("gateway.call_refused vendor=%s session=%s turn=%s exchange=%s", g.adapter.Vendor(), session, turn, ex.id)
	}
	writeError(w, http.StatusForbidden, why)
}

func writeError(w http.ResponseWriter, status int, why string) {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{why})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// serveService forwards a service call (adapter.Call Service) with its
// credential, and records it by status when the reply comes
// (serviceReplied), or by why it failed (errorHandler).
func (g *Gateway) serveService(w http.ResponseWriter, r *http.Request, session, turn string, call adapter.Call) {
	rl := relayedOf(r.Context())
	id := fmt.Sprintf("x-%s-%d", g.run, g.seq.Add(1))
	if rl != nil && rl.Exchange != "" {
		id = rl.Exchange
	}
	ex := &exchange{id: id, session: session, client: clientName(r.UserAgent()), turn: turn, start: g.now(),
		relayed: rl != nil, checked: true, call: call, name: callName(r)}
	if turn != "" {
		g.track(ex, session, turn)
	}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), exchangeKey{}, ex)))
}

// serviceReplied records a service call's reply by its status.
func (g *Gateway) serviceReplied(ex *exchange, status int) {
	latency := g.now().Sub(ex.start).Milliseconds()
	g.emit(ex, []event.Event{{Kind: event.VendorCalled, Call: ex.name, Status: status, LatencyMS: &latency}})
	g.flushed(ex.done)
}

// readable says whether a reply with header is one the adapter reads for
// call: a media type it lists, in an encoding the gateway decodes.
func readable(call adapter.Call, header http.Header) bool {
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding"))) {
	case "", "identity", "gzip", "deflate", "zstd":
	default:
		return false
	}
	return call.Reads(header.Get("Content-Type"))
}

// unreadableReply answers a call whose reply the adapter cannot read, in
// place of that reply: 502, and the call is recorded as refused.
func (g *Gateway) unreadableReply(w http.ResponseWriter, ex *exchange) {
	kind := event.ModelResponded
	if ex.call.Service {
		kind = event.VendorCalled
	}
	e := event.Event{Kind: kind, Status: http.StatusBadGateway, Error: unreadable}
	if ex.call.Service {
		e.Call = ex.name
	}
	g.emit(ex, []event.Event{e, {Kind: event.CallRefused, Call: ex.name, Status: http.StatusBadGateway, Error: unreadable}})
	g.flushed(ex.done)
	if g.logf != nil {
		g.logf("gateway.reply_refused vendor=%s session=%s turn=%s exchange=%s", g.adapter.Vendor(), ex.session, ex.turn, ex.id)
	}
	writeError(w, http.StatusBadGateway, unreadable)
}
