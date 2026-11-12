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

// Watches says whether the gateway's adapter reads calls of method on path,
// so a relay with several gateways for one vendor host can pick one.
func (g *Gateway) Watches(method, path string) bool { return g.adapter.Watches(method, path) }

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
func (g *Gateway) ServeRelayed(w http.ResponseWriter, r *http.Request, rl Relayed) {
	// The turn is open from the call's arrival: a turn's end stored while
	// its credential is read or its request body arrives would be stored
	// before the call.
	defer g.hold(rl.Session, rl.Turn)()
	if rl.Prepare != nil {
		rl.Prepare(&rl)
	}
	r.URL.Path, r.URL.RawPath = rl.Path, ""
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

// Credential is where a gateway's calls get their credential when the
// gateway runs in control's own process (agentbus up): the same answer
// control's relay gives a runner's call, without a hop over HTTP. It reads
// the call's placeholder (its turn's token) and returns what sets the
// credential on the request to the vendor, or the refusal answered in the
// vendor's place. It never puts a value in an event or a log.
type Credential func(r *http.Request) (header func(http.Header), refuse *Refusal)

// SetCredential makes every call get its credential from c: the agent's
// own header is replaced, never passed on. Call it before serving.
func (g *Gateway) SetCredential(c Credential) { g.credential = c }

type credKey struct{}

// credCall is what a call's Credential gave.
type credCall struct {
	header func(http.Header)
	refuse *Refusal
}

func credOf(ctx context.Context) *credCall {
	c, _ := ctx.Value(credKey{}).(*credCall)
	return c
}
