package gateway

// A model call relayed by control for a runner's turn (jobs/relay.go). The
// runner's own gateway sends the call on to control's relay; control's
// gateway for that vendor records it, as a gateway on the agent's own machine
// does, and sends it to the vendor with the credential control adds. So
// control's record holds every relayed call from control's own copy, and the
// runner never has to send it.
//
// The runner's gateway names each call's exchange in ExchangeHeader, and
// control's gateway records the call under that name, so what only the
// runner sees of a call (a refused tool call, a replay that failed) names the
// same exchange in control's record. The header never reaches a vendor.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

// ExchangeHeader carries the runner's exchange id of a model call to
// control's relay.
const ExchangeHeader = "X-Agentbus-Exchange"

// Relayed is a model call control's relay took from a runner's gateway: the
// turn its token names, and what control adds to it.
type Relayed struct {
	// Session and Turn are the job's, from the token: every event of the
	// call carries them, whatever the request says.
	Session, Turn string
	// Exchange is the runner's id for the call (ExchangeHeader), checked by
	// the relay; empty: the gateway names the call itself.
	Exchange string
	// Path is the vendor path, decoded, such as /v1/messages.
	Path string
	// Header edits the request that goes to the vendor: it adds the
	// credential. It never reads a value into an event or a log.
	Header func(http.Header)
	// Signin is the subscription Header adds, or zero for any other
	// credential: the reply's plan usage is stored under it (plan.go).
	Signin Signin
	// Refuse, when set, is the answer in the vendor's place: nothing goes to
	// the vendor, and the call is recorded with this as its reply.
	Refuse *Refusal
	// Prepare, when set, fills Exchange, Header or Refuse once the call
	// holds its turn open (Settle waits for it): reading the record or a
	// credential can take a while.
	Prepare func(*Relayed)
}

// relayed is a Relayed call on its way, with the writer that answers the
// runner.
type relayed struct {
	Relayed
	w http.ResponseWriter
}

type relayedKey struct{}

func relayedOf(ctx context.Context) *relayed {
	rl, _ := ctx.Value(relayedKey{}).(*relayed)
	return rl
}

// Records says whether the gateway's adapter records calls of method on
// path (adapter.Call), so a relay with several gateways for one vendor host
// can pick one.
func (g *Gateway) Records(method, path string) bool {
	_, ok := adapter.Find(g.adapter, method, path)
	return ok
}

// Names says whether exchange is an id this gateway gave, so a relay never
// lets a runner name a call with an id of control's own.
func (g *Gateway) Names(exchange string) bool { return strings.HasPrefix(exchange, "x-"+g.run+"-") }

// SendExchange makes the gateway name each model call's exchange to its
// upstream in ExchangeHeader: for a runner whose upstream is control's relay.
// Call it before serving.
func (g *Gateway) SendExchange() { g.sendExchange = true }

// ServeRelayed records and forwards a call control's relay took (rl). It
// does what ServeHTTP does for a run's turn, except what the runner's own
// gateway did already: no history is put in front (own replay), no tool call
// is refused (the guard), and no earlier refusal is undone.
//
// Only a call the adapter records gets control's credential (recorded.go):
// any other is refused, with its credential never read, and recorded. A
// refused call holds its turn open first too, as every call does.
func (g *Gateway) ServeRelayed(w http.ResponseWriter, r *http.Request, rl Relayed) {
	r.URL.Path, r.URL.RawPath = rl.Path, ""
	var refused string
	defer g.holdThenRead(rl.Session, rl.Turn, func() {
		if refused = g.unrecorded(r); refused != "" {
			return
		}
		if rl.Prepare != nil {
			rl.Prepare(&rl)
		}
	})()
	if refused != "" {
		g.refuseCall(w, r, rl.Session, rl.Turn, true, refused)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), relayedKey{}, &relayed{Relayed: rl, w: w}))
	g.serve(w, r, rl.Session, rl.Turn)
}

// RequestCut is the error of a relayed call whose request did not arrive
// whole: the runner's connection ended during it. No vendor got it.
const RequestCut = "the request did not reach control whole, so no vendor got it"

// cutRequest records a relayed call whose request did not arrive whole, so
// control's record holds every call it took, this one too.
func (g *Gateway) cutRequest(rl *relayed, r *http.Request) {
	id := rl.Exchange
	if id == "" {
		id = fmt.Sprintf("x-%s-%d", g.run, g.seq.Add(1))
	}
	ex := &exchange{id: id, session: rl.Session, client: clientName(r.UserAgent()), turn: rl.Turn, start: g.now(), relayed: true}
	g.track(ex, rl.Session, rl.Turn)
	g.emit(ex, []event.Event{{Kind: event.ModelRequested, Error: RequestCut}, {Kind: event.ModelResponded, Error: RequestCut}})
	g.flushed(ex.done)
}

// refuser answers a call that carries a Refusal in the vendor's place, a
// relayed one's or its credential's; any other goes on to next.
type refuser struct{ next http.RoundTripper }

func (t refuser) RoundTrip(req *http.Request) (*http.Response, error) {
	var f *Refusal
	if rl := relayedOf(req.Context()); rl != nil {
		f = rl.Refuse
	} else if c := credOf(req.Context()); c != nil {
		f = c.refuse
	}
	if f == nil {
		return t.next.RoundTrip(req)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	h := http.Header{}
	if f.ContentType != "" {
		h.Set("Content-Type", f.ContentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(f.Body)))
	return &http.Response{Status: strconv.Itoa(f.Status) + " " + http.StatusText(f.Status), StatusCode: f.Status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: h, Body: io.NopCloser(bytes.NewReader(f.Body)),
		ContentLength: int64(len(f.Body)), Request: req}, nil
}

// relayFailed answers a relayed call whose vendor could not be reached or
// read, as control's relay answers: one JSON object whose error says what
// happened, in fixed words (msg) and one word for the cause (reason).
func relayFailed(w http.ResponseWriter, msg, reason string) {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{"control could not reach the model's vendor: " + msg + "; cause: " + reason})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(b)
}

// holdThenRead is the one way a call reads its credential, on every path
// (ServeRelayed, and serve with SetCredential), or is refused one
// (recorded.go): it holds the call's turn open first, then runs read, and returns what lets the turn go once the
// call is done. A turn's end stored while a credential is read, a slow
// renewal say, would be stored before the call's events.
func (g *Gateway) holdThenRead(session, turn string, read func()) (release func()) {
	release = g.hold(session, turn)
	g.reads.Add(1)
	read()
	return release
}

// Credential is where a gateway's calls get their credential when the
// gateway runs in control's own process (agentbus up): the same answer
// control's relay gives a runner's call, without a hop over HTTP. One rule:
// a call's session and turn come from its token's job, never from its path.
// Neither func puts a value in an event or a log.
type Credential struct {
	// Turn says whether the call's placeholder (its turn's token) is the
	// token of a running turn, and that turn is the session and turn the
	// call's path names. When not (no such token, or another turn's), it is
	// the refusal: the gateway answers it before anything is held, recorded
	// or rewritten, and the call is recorded nowhere.
	Turn func(r *http.Request, session, turn string) *Refusal
	// Read is the credential for a call Turn let through: what sets it on
	// the request to the vendor, the subscription that is (zero for any
	// other credential; plan.go), or the refusal answered in the
	// vendor's place, which is recorded with the call. It may be slow (a
	// renewal), so it is read with the turn held open (holdThenRead).
	Read func(r *http.Request) (header func(http.Header), signin Signin, refuse *Refusal)
}

// Signin is the subscription of control's a call's credential is, for its
// reply's plan usage (plan.go). The zero value is no subscription: a key,
// or no credential of control's.
type Signin struct {
	// Subscription is the harness whose subscription it is, such as claude.
	Subscription string
	// Connection names the connect the credential came from
	// (credentials.Cred.Connection): a reading is shown only while that
	// connect is the subscription's current one.
	Connection string
}

// SetCredential makes every call get its credential from c: the agent's
// own header is replaced, never passed on. Call it before serving.
func (g *Gateway) SetCredential(c Credential) { g.credential = c }

type credKey struct{}

// credCall is what a call's Credential gave.
type credCall struct {
	header func(http.Header)
	signin Signin
	refuse *Refusal
}

func credOf(ctx context.Context) *credCall {
	c, _ := ctx.Value(credKey{}).(*credCall)
	return c
}
