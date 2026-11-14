// Package gateway is the local base-URL gateway.
//
// An agent is pointed at it instead of at its vendor. Every request goes on to
// the vendor unchanged, headers included, so the agent's own sign-in is used.
// Two exceptions: Accept-Encoding is dropped so the vendor replies in a form
// the gateway can read, and a Rewriter, when set, may change the body of a
// run's model request (own replay: the earlier turn's history in front). Every reply comes back unchanged and streams
// as it arrives. A copy of each
// model call is read by the vendor's adapter and turned into events, after the
// reply has been handed on, so events never slow the agent down.
//
// No header value reaches an event, an error message or a log, with two
// exceptions: the User-Agent's first token, and only when it has the shape
// name/version, which names the harness; and the vendor's plan usage, read
// from a fixed list of reply headers into numbers, times and one word
// (plan.go). Error messages use fixed wording,
// because Go's own error text can quote the header that caused it.
//
// net/http also writes some messages to Go's global logger, and those can quote
// a vendor reply's headers. A program that embeds the gateway should send that
// logger nowhere, as cmd/abgateway does.
package gateway

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

type Gateway struct {
	adapter adapter.Adapter
	proxy   *httputil.ReverseProxy
	run     string
	seq     atomic.Uint64
	now     func() time.Time
	queue   *queue
	bodies  *bodyQueue // nil unless KeepBodies was called
	rewrite Rewriter   // nil unless SetRewriter was called
	guard   Guard      // nil unless SetGuard was called
	refused refusals   // which rule refused each call (guard.go)
	plans   planSeen   // the plan usage last stored (plan.go)
	prefix  string     // cut from the vendor path before anything reads it (SetPrefix)
	// sendExchange: each call's exchange goes upstream in ExchangeHeader
	// (SendExchange).
	sendExchange bool
	// credential, when set, is where each call gets its credential
	// (SetCredential); nil passes the agent's own on.
	credential Credential
	// reads counts the credential reads made through holdThenRead, for the
	// test that every path reads one that way.
	reads atomic.Int64

	// logf is the service log, for why an upstream call failed; nil unless
	// SetLog was called.
	logf func(format string, args ...any)

	// open counts each run turn's exchanges not yet fully written: their
	// events delivered to the sink and their reply handed to the keeper.
	// Settle waits on it.
	openMu      sync.Mutex
	open        map[turnKey]int
	unconfirmed map[turnKey]bool // turns with an exchange whose writes were not confirmed
	opened      *sync.Cond

	// finishing counts replies still being turned into events, so Close can
	// wait for them.
	mu        sync.Mutex
	closed    bool
	finishing sync.WaitGroup
}

// queueLimit bounds the event data waiting for a blocked output.
const queueLimit = 64 << 20

// forwarded are the headers ReverseProxy removes before Rewrite. The agent sent
// them, so they go on.
var forwarded = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// New returns a gateway that forwards to upstream, such as https://api.anthropic.com.
func New(upstream *url.URL, a adapter.Adapter, sink event.Sink) *Gateway {
	g := &Gateway{adapter: a, run: runID(), now: time.Now, open: map[turnKey]int{},
		unconfirmed: map[turnKey]bool{}}
	g.opened = sync.NewCond(&g.openMu)
	g.queue = newQueue(sink, queueLimit, g.dropped)

	// The default transport asks for gzip when the agent did not, and then
	// decompresses the reply, so the agent would get different bytes.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			rl := relayedOf(r.In.Context())
			// ReverseProxy drops query parts it cannot parse before Rewrite,
			// so the agent's query is put back, after the upstream's own.
			switch {
			case upstream.RawQuery == "":
				r.Out.URL.RawQuery = r.In.URL.RawQuery
			case r.In.URL.RawQuery == "":
				r.Out.URL.RawQuery = upstream.RawQuery
			default:
				r.Out.URL.RawQuery = upstream.RawQuery + "&" + r.In.URL.RawQuery
			}
			// A relayed call's hops are the runner's, not the agent's: they
			// stay here, as control's relay always kept them.
			if rl == nil {
				for _, h := range forwarded {
					if v, ok := r.In.Header[h]; ok {
						r.Out.Header[h] = v
					}
				}
			}
			// The one header not passed on. With the agent's Accept-Encoding,
			// Anthropic answered a 429 in br, which the gateway cannot read, so
			// the error text was lost (Claude Code 2.1.318, 2026-11-05). Without
			// it the vendor replies in identity, which the agent can always
			// read. LiteLLM never forwards it to Anthropic either. A vendor that
			// compresses anyway still goes through decode.
			r.Out.Header.Del("Accept-Encoding")
			// The exchange header is the gateways' own: whatever the agent
			// sent under it goes nowhere, and a vendor never gets it.
			r.Out.Header.Del(ExchangeHeader)
			if rl != nil && rl.Header != nil {
				rl.Header(r.Out.Header)
			}
			if c := credOf(r.In.Context()); c != nil && c.header != nil {
				c.header(r.Out.Header)
			}
			if ex, ok := r.In.Context().Value(exchangeKey{}).(*exchange); ok && g.sendExchange && rl == nil {
				r.Out.Header.Set(ExchangeHeader, ex.id)
			}
		},
		Transport:      refuser{transport},
		FlushInterval:  -1,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.errorHandler,
		// Its messages can quote header and trailer values.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return g
}

// Rewriter may change the body of a model request of a run's turn, a request
// whose base URL names its turn, before it goes on. It gets the body decoded.
type Rewriter interface {
	Rewrite(session, turn string, body []byte) Rewrite
}

// Rewrite is a Rewriter's answer.
type Rewrite struct {
	// Body is the request to send in place of the agent's. Nil sends the
	// agent's unchanged.
	Body []byte
	// Replayed goes on the request's model.requested.
	Replayed int
	// Header, when set with Body, edits the headers that go on with Body,
	// such as codex's thread id (own replay). It never reads a value into an
	// event or a log.
	Header func(http.Header)
	// Refuse, when set, answers the agent with it and sends nothing to the
	// vendor. Events then say why.
	Refuse *Refusal
	// Events are emitted for the exchange, before model.requested.
	Events []event.Event
}

// Refusal is an answer the gateway gives in the vendor's place.
type Refusal struct {
	Status      int
	ContentType string
	Body        []byte
}

// SetRewriter sets the Rewriter. Call it before serving.
func (g *Gateway) SetRewriter(r Rewriter) { g.rewrite = r }

// SetPrefix makes the gateway serve the vendor's paths under prefix, such as
// /openrouter: /openrouter/v1/messages is read and forwarded as
// /v1/messages. A path not under it is forwarded unchanged. Call it before
// serving.
func (g *Gateway) SetPrefix(prefix string) { g.prefix = prefix }

// SetLog gives the gateway the service log, for what an event cannot hold:
// why an upstream call failed. A line holds ids, Go error types and fixed
// words, never a header, a body or Go's error text. Call it before serving.
func (g *Gateway) SetLog(logf func(format string, args ...any)) { g.logf = logf }

func runID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type exchange struct {
	id      string
	session string
	client  string
	turn    string
	start   time.Time
	// probe: the adapter marked the request as the harness's own check, so
	// every event of the exchange carries Probe.
	probe bool
	// forbid: the run turn's forbid entries, which the guard applies to the
	// reply (guard.go).
	forbid []string
	// relayed: control's relay took the call for a runner's turn
	// (ServeRelayed).
	relayed bool
	// checked: the call gets a credential of control's, so its reply passes
	// on only in a format call reads (recorded.go). call is the adapter's
	// Call it is, service when not a model call (vendor.called).
	checked bool
	call    adapter.Call
	// name is the call's method and vendor path, for call.refused and
	// vendor.called (callName).
	name string
	// model is the model the call's path names (adapter.Call.Model), or
	// "": the record's model for the call when its path names one.
	model string
	// signin: the subscription of control's the call carries, or zero
	// (plan.go).
	signin Signin

	// unknowns keeps each schema.unexpected shape once per exchange, and at
	// most adapter.MaxUnexpected, across the request and the reply.
	mu       sync.Mutex
	unknowns adapter.Unknowns

	// release, set for a run turn's exchange, marks it fully written. It
	// runs once, however many paths reach it.
	releaseOnce sync.Once
	release     func(confirmed bool)
}

// done marks the exchange as finished, once. confirmed false means the
// gateway could not confirm its writes (a barrier could not be queued).
func (ex *exchange) done(confirmed bool) {
	if ex.release != nil {
		ex.releaseOnce.Do(func() { ex.release(confirmed) })
	}
}

type exchangeKey struct{}

// turnKey names a run's turn: its session and the turn its base URL names.
type turnKey struct{ session, turn string }

// hold counts the run turn (session, turn) as open until the returned func
// runs.
func (g *Gateway) hold(session, turn string) func() {
	k := turnKey{session, turn}
	g.openMu.Lock()
	g.open[k]++
	g.openMu.Unlock()
	return func() {
		g.openMu.Lock()
		if g.open[k]--; g.open[k] <= 0 {
			delete(g.open, k)
		}
		g.openMu.Unlock()
		g.opened.Broadcast()
	}
}

// track counts a run turn's exchange as open until ex.done.
func (g *Gateway) track(ex *exchange, session, turn string) {
	k := turnKey{session, turn}
	g.openMu.Lock()
	g.open[k]++
	g.openMu.Unlock()
	ex.release = func(confirmed bool) {
		g.openMu.Lock()
		if g.open[k]--; g.open[k] <= 0 {
			delete(g.open, k)
		}
		if !confirmed {
			g.unconfirmed[k] = true
		}
		g.openMu.Unlock()
		g.opened.Broadcast()
	}
}

// Settle waits until every model call of the run turn (session, turn) that
// reached the gateway is fully written: its events delivered to the sink and
// its reply handed to the reply keeper. The child's exit does not mean that:
// a reply's events are written off the reply's path, after the agent already
// has its last bytes, so they can land after the agent has exited. A service
// that stores a turn's end calls Settle first, so a reader that sees the end
// sees all of the turn's calls. It reports false when it gives up after
// timeout, and when an exchange's writes could not be confirmed because a
// queue was full (flushed): the caller must then not treat the turn as whole.
func (g *Gateway) Settle(session, turn string, timeout time.Duration) bool {
	k := turnKey{session, turn}
	// The deadline is set before the timer, so the timer never fires before
	// it, and the timer broadcasts holding openMu, so its wakeup cannot fall
	// between the deadline check and Wait (a lost wakeup left Settle, and
	// with it session.closed and shutdown, waiting on a stalled sink for
	// ever).
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(time.Until(deadline), func() {
		g.openMu.Lock()
		g.opened.Broadcast()
		g.openMu.Unlock()
	})
	defer timer.Stop()
	g.openMu.Lock()
	defer g.openMu.Unlock()
	for g.open[k] > 0 {
		if !time.Now().Before(deadline) {
			return false
		}
		g.opened.Wait()
	}
	// An exchange whose writes could not be confirmed leaves the turn
	// unsettled: reporting it settled would let a reader take a turn with
	// missing calls for a whole one.
	if g.unconfirmed[k] {
		delete(g.unconfirmed, k)
		return false
	}
	return true
}

// flushed calls f once everything already handed to the event queue and the
// body queue has been delivered. A barrier is bounded like the queue's own
// entries; one that cannot be queued is not waited for, and f gets false:
// the writes could not be confirmed. At most one waiter runs per queued
// barrier, so the waiters are bounded too.
func (g *Gateway) flushed(f func(confirmed bool)) {
	var n sync.WaitGroup
	var failed atomic.Bool
	n.Add(1)
	if !g.queue.barrier(n.Done) {
		failed.Store(true)
		n.Done()
	}
	if g.bodies != nil {
		n.Add(1)
		if !g.bodies.barrier(n.Done) {
			failed.Store(true)
			n.Done()
		}
	}
	go func() { n.Wait(); f(!failed.Load()) }()
}

var (
	sessionID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	productName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}/[A-Za-z0-9._+-]{1,32}$`)
)

// SplitSession reads an optional /s/<session> prefix from the escaped path, so
// that %2F and %2e in the rest stay encoded, and after it an optional
// /t/<turn>. Each session gets its own base URL, because the traffic alone
// does not say which session it belongs to. A run's base URL also names its
// turn, /s/<session>/t/<turn>, so every model call of the run belongs to that
// turn whatever its conversation holds. session is empty when the path has no
// valid prefix; rest is then the whole path. turn is empty without a valid
// /t/<turn>. A router in front of several gateways uses it to read the vendor
// path the gateway will see.
func SplitSession(escaped string) (session, turn, rest string) {
	tail, ok := strings.CutPrefix(escaped, "/s/")
	if !ok {
		return "", "", escaped
	}
	id, after, _ := strings.Cut(tail, "/")
	if !sessionID.MatchString(id) {
		return "", "", escaped
	}
	rest = "/" + after
	if t, ok := strings.CutPrefix(rest, "/t/"); ok {
		tid, after2, _ := strings.Cut(t, "/")
		if sessionID.MatchString(tid) {
			turn, rest = strings.Clone(tid), "/"+after2
		}
	}
	// Copied: every event keeps the id, and a substring would keep the whole
	// request path in memory with it.
	return strings.Clone(id), turn, rest
}

// clientName keeps the User-Agent's first token, such as "claude-cli/2.1.318",
// and only when it has that shape. It is copied, because a substring would
// keep the whole header in memory for as long as the event lives.
func clientName(ua string) string {
	name, _, _ := strings.Cut(ua, " ")
	if productName.MatchString(name) {
		return strings.Clone(name)
	}
	return ""
}

// DefaultSession is the session of a request without /s/<session>. It is a
// real id, so a cockpit can name it: /s/default and the bare path are the same
// session.
const DefaultSession = "default"

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, runTurn, rest := SplitSession(r.URL.EscapedPath())
	if session != "" {
		path, err := url.PathUnescape(rest)
		if err != nil {
			http.Error(w, "gateway: the path is not validly encoded", http.StatusBadRequest)
			return
		}
		r.URL.Path, r.URL.RawPath = path, rest
	} else {
		session = DefaultSession
	}
	if g.prefix != "" {
		if p, ok := strings.CutPrefix(r.URL.Path, g.prefix); ok && strings.HasPrefix(p, "/") {
			r.URL.Path = p
			if r.URL.RawPath != "" {
				r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, g.prefix)
			}
		}
	}
	g.serve(w, r, session, runTurn)
}

// serve records and forwards a call of session, and of the run turn runTurn
// when it is set; r's path is the vendor's.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, session, runTurn string) {
	rl := relayedOf(r.Context())
	if g.credential.Read != nil && rl == nil {
		// The call is the token's turn's, or it is answered here and goes
		// nowhere: no hold, no event, no rewrite, no vendor.
		if f := g.credential.Turn(r, session, runTurn); f != nil {
			_ = r.Body.Close()
			if f.ContentType != "" {
				w.Header().Set("Content-Type", f.ContentType)
			}
			w.WriteHeader(f.Status)
			_, _ = w.Write(f.Body)
			return
		}
		// Only a call the adapter records gets the credential: any other
		// is refused, with the credential never read. Either way the turn
		// is held open first.
		var c credCall
		var refused string
		defer g.holdThenRead(session, runTurn, func() {
			if refused = g.unrecorded(r); refused != "" {
				return
			}
			c.header, c.signin, c.refuse = g.credential.Read(r)
		})()
		if refused != "" {
			g.refuseCall(w, r, session, runTurn, false, refused)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), credKey{}, &c))
	}
	// An agent may first try a WebSocket on its model-call path, as codex-cli
	// 0.164.0 does with a GET and Upgrade on /backend-api/codex/responses
	// (2026-11-05). The gateway cannot read a WebSocket, so passing it on would
	// hide every model call. A 426 made codex fall back to a streamed POST,
	// which the gateway reads. Upgrades on other paths pass through.
	if r.Header.Get("Upgrade") != "" && adapter.Watches(g.adapter, http.MethodPost, r.URL.Path) {
		http.Error(w, "gateway: a WebSocket on the model-call path cannot be read; use a POST", http.StatusUpgradeRequired)
		return
	}
	call, _ := adapter.Find(g.adapter, r.Method, r.URL.Path)
	checked := rl != nil || g.credential.Read != nil
	if !adapter.Watches(g.adapter, r.Method, r.URL.Path) {
		// With a credential of control's only a call the adapter names gets
		// here (unrecorded): a service call, recorded by its status.
		if checked && call.Service {
			g.serveService(w, r, session, runTurn, call)
			return
		}
		g.proxy.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		if rl != nil {
			g.cutRequest(rl, r)
		}
		http.Error(w, "gateway: the request could not be read", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	id := fmt.Sprintf("x-%s-%d", g.run, g.seq.Add(1))
	if rl != nil && rl.Exchange != "" {
		id = rl.Exchange
	}
	ex := &exchange{
		id:      id,
		session: session,
		client:  clientName(r.UserAgent()),
		turn:    runTurn,
		start:   g.now(),
		relayed: rl != nil,
		checked: checked,
		call:    call,
		name:    callName(r),
		model:   call.Model(r.URL.Path),
	}
	if rl != nil {
		ex.signin = rl.Signin
	} else if c := credOf(r.Context()); c != nil {
		ex.signin = c.signin
	}
	if runTurn != "" {
		g.track(ex, session, runTurn)
	}
	// The adapter reads a decoded copy; the vendor gets the bytes the agent
	// sent. codex-cli 0.164.0 sends its requests with Content-Encoding: zstd
	// (2026-11-05).
	if plain, err := decode(r.Header.Get("Content-Encoding"), body, "request", false); err != nil {
		g.emit(ex, []event.Event{{Kind: event.ModelRequested, Error: err.Error()}})
	} else {
		// Read from the request now, so the reply's events, read later in the
		// background, carry this request's turn whatever happened meanwhile.
		// A run names its turn in its base URL, and every exchange of the run
		// belongs to it: the agent's own loop can send a conversation the hash
		// would read as a new turn, such as one codex compacted mid-turn. Other
		// traffic gets the hash of its conversation.
		if runTurn == "" {
			ex.turn = turnID(session, g.adapter.TurnBasis(plain))
		}
		// A call the guard refused earlier goes back to the model under its
		// real name, with the refusal as its output (guard.go). The vendor
		// then gets the body plain, as after a rewrite.
		// A relayed call was fixed by the runner's gateway already.
		if fixed, ok := g.fixHistory(g.adapter.Vendor(), plain); ok && rl == nil {
			plain = fixed
			r.Body = io.NopCloser(bytes.NewReader(plain))
			r.ContentLength = int64(len(plain))
			r.Header.Del("Content-Encoding")
		}
		if g.guard != nil && runTurn != "" && rl == nil {
			ex.forbid = g.guard.Forbid(session, runTurn)
		}
		var replayed int
		if g.rewrite != nil && runTurn != "" && rl == nil {
			rw := g.rewrite.Rewrite(session, runTurn, plain)
			g.emit(ex, rw.Events)
			if rw.Refuse != nil {
				// The agent's own request is kept: it shows the layout the
				// rewriter could not use.
				g.keepBody(ex, plain)
				g.flushed(ex.done)
				w.Header().Set("Content-Type", rw.Refuse.ContentType)
				w.WriteHeader(rw.Refuse.Status)
				_, _ = w.Write(rw.Refuse.Body)
				return
			}
			if rw.Body != nil {
				// What the vendor gets, sent plain: a compressed request is
				// decoded once and not compressed again.
				plain, replayed = rw.Body, rw.Replayed
				r.Body = io.NopCloser(bytes.NewReader(plain))
				r.ContentLength = int64(len(plain))
				r.Header.Del("Content-Encoding")
				if rw.Header != nil {
					rw.Header(r.Header)
				}
			}
		}
		g.keepBody(ex, plain)
		if evs, err := g.adapter.RequestEvents(plain); err != nil {
			g.emit(ex, []event.Event{{Kind: event.ModelRequested, Error: err.Error(), Replayed: replayed}})
		} else {
			for i, e := range evs {
				ex.probe = ex.probe || (e.Kind == event.ModelRequested && e.Probe)
				if e.Kind == event.ModelRequested {
					evs[i].Replayed = replayed
				}
			}
			// A probe starts no turn and has no turn id, a run's included.
			if ex.probe {
				ex.turn = ""
			}
			g.emit(ex, evs)
		}
	}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), exchangeKey{}, ex)))
}

// turnID names a turn: "t_" and the first 16 hex characters of the SHA-256 of
// the session id and the adapter's turn basis. The session id holds no zero
// byte, so the separator keeps the two parts apart. No basis, no turn.
func turnID(session string, basis []byte) string {
	if basis == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(session))
	h.Write([]byte{0})
	h.Write(basis)
	return "t_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func (g *Gateway) modifyResponse(resp *http.Response) error {
	// chatgpt.com streams codex's reply with no Content-Type. Go's server
	// would then add one it guessed from the first bytes, "text/plain;
	// charset=utf-8", and the runner's gateway would read the stream as one
	// JSON body and record no reply (2026-11-11). A nil value keeps the
	// header out of a relayed reply, as the vendor sent it.
	if rl := relayedOf(resp.Request.Context()); rl != nil {
		if _, ok := resp.Header["Content-Type"]; !ok {
			rl.w.Header()["Content-Type"] = nil
		}
	}
	ex, ok := resp.Request.Context().Value(exchangeKey{}).(*exchange)
	if !ok {
		return nil
	}
	// A reply to a call with a credential of control's passes on only in a
	// format the adapter reads; any other is answered by errorHandler.
	if ex.checked && !readable(ex.call, resp.Header) {
		return errUnreadable
	}
	if ex.call.Service {
		g.serviceReplied(ex, resp.StatusCode)
		return nil
	}
	g.readPlan(ex, resp.Header)
	status, header := resp.StatusCode, resp.Header.Clone()
	// The guard reads the reply as the vendor sent it. The vendor is asked
	// for no encoding (Accept-Encoding is dropped); a reply compressed anyway
	// is not guarded.
	guarded := false
	if len(ex.forbid) > 0 && !ex.probe && status == http.StatusOK {
		switch strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding"))) {
		case "", "identity":
			guarded = true
		}
	}
	start := func(raw []byte, cut bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.closed {
			ex.done(false)
			return
		}
		g.finishing.Add(1)
		// Off the reply's path: the agent gets its last bytes now.
		go func() {
			defer g.finishing.Done()
			g.finish(ex, status, header, raw, cut)
		}()
	}
	// A guarded reply's exchange finishes once the tee has the whole copy and
	// the guard has emitted its refusals, whichever comes last: the tee sees
	// the end of the reply before the guard reads it, and Settle must not
	// report the turn whole while a tool.refused is still to come.
	finish, guardEnd := start, func() {}
	if guarded {
		var (
			mu      sync.Mutex
			waiting = 2
			rawCopy []byte
			wasCut  bool
		)
		arrive := func() {
			mu.Lock()
			waiting--
			last := waiting == 0
			mu.Unlock()
			if last {
				start(rawCopy, wasCut)
			}
		}
		finish = func(raw []byte, cut bool) {
			mu.Lock()
			rawCopy, wasCut = raw, cut
			mu.Unlock()
			arrive()
		}
		var once sync.Once
		guardEnd = func() { once.Do(arrive) }
	}
	resp.Body = &tee{rc: resp.Body, done: finish}
	if guarded {
		// The agent gets the reply with each refused call renamed; the
		// adapter reads the tee, so it sees the calls as the model wrote them.
		resp.Body = g.guardReply(ex, ex.forbid, resp.Body, guardEnd)
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}
	return nil
}

// AgentClosed is the error of a model call the agent gave up on before the
// vendor's reply began: the agent closed its connection, as when its turn is
// stopped. Such a call has no status, since no vendor answered it.
const AgentClosed = "the agent closed the connection before the vendor replied"

// upstreamFailure describes a failed upstream call in fixed words. status is
// the one its model.responded carries: 502 for a vendor the gateway could not
// reach or read, none (0) for a call the agent itself closed.
func upstreamFailure(err error) (msg string, status int) {
	if errors.Is(err, context.Canceled) {
		// Not a vendor failure: before, this was stored as a 502, and the
		// next turn's note said the stopped turn's call "failed with status
		// 502" (acceptance soak, 2026-11-06).
		return AgentClosed, 0
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "the vendor could not be reached (" + op.Op + ")", http.StatusBadGateway
	}
	return "the vendor's reply could not be read", http.StatusBadGateway
}

func (g *Gateway) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	ex, ok := r.Context().Value(exchangeKey{}).(*exchange)
	rl := relayedOf(r.Context())
	if ok && errors.Is(err, errUnreadable) {
		g.unreadableReply(w, ex)
		return
	}
	msg, status := upstreamFailure(err)
	class, reason := causeOf(err)
	// The cause goes to the service log, so a 502 can be traced; the agent
	// and the event get the fixed words. A call the agent closed is no
	// failure.
	if g.logf != nil && status != 0 {
		session, turn, exchange, after, via := "", "", "", int64(0), "agent"
		if rl != nil {
			session, turn, via = rl.Session, rl.Turn, "relay"
		}
		if ok {
			session, turn, exchange, after = ex.session, ex.turn, ex.id, g.now().Sub(ex.start).Milliseconds()
		}
		g.logf("gateway.upstream_failed vendor=%s prefix=%s via=%s session=%s turn=%s exchange=%s after_ms=%d class=%s reason=%s",
			g.adapter.Vendor(), g.prefix, via, session, turn, exchange, after, class, reason)
	}
	if ok {
		e := event.Event{Kind: event.ModelResponded, Status: status, Error: msg}
		if ex.call.Service {
			e.Kind, e.Call = event.VendorCalled, ex.name
		}
		g.emit(ex, []event.Event{e})
		g.flushed(ex.done)
	}
	if rl != nil {
		relayFailed(w, msg, reason)
		return
	}
	// The agent that closed its connection reads no reply; any other gets 502.
	http.Error(w, "gateway: "+msg, http.StatusBadGateway)
}

func (g *Gateway) finish(ex *exchange, status int, header http.Header, raw []byte, cut bool) {
	// Whatever path finish takes, the exchange is fully written once what it
	// handed to the queues is delivered.
	defer g.flushed(ex.done)
	latency := g.now().Sub(ex.start).Milliseconds()
	fail := func(msg string) {
		g.emit(ex, []event.Event{{Kind: event.ModelResponded, Status: status, LatencyMS: &latency, Error: msg}})
	}
	// Adapters come from many authors. A broken one must not stop the gateway,
	// and every agent's model access with it.
	defer func() {
		if p := recover(); p != nil {
			fail(fmt.Sprintf("the %s adapter failed on this reply", g.adapter.Vendor()))
		}
	}()

	body, err := decode(header.Get("Content-Encoding"), raw, "reply", cut)
	if err != nil {
		fail(err.Error())
		return
	}
	g.keepReply(ex, status, header.Get("Content-Type"), body)
	evs, err := g.adapter.ResponseEvents(status, header.Get("Content-Type"), body)
	if err != nil {
		fail(err.Error())
		// Shapes the adapter saw before the error are still reported.
		var seen []event.Event
		for _, e := range evs {
			if e.Kind == event.SchemaUnexpected {
				seen = append(seen, e)
			}
		}
		g.emit(ex, seen)
		return
	}
	// A reply counts as cut only when the adapter could not see all of it. A
	// model.responded with a stop reason and no error means the adapter saw
	// the reply's end (Anthropic's message_stop, OpenAI's response.completed or
	// response.incomplete), so a body the client closed after that keeps every
	// event. codex-cli 0.164.0 stops reading after response.completed while
	// chatgpt.com keeps the stream open, so every codex reply ended in a close
	// (2026-11-05). An adapter that misses the end reports an error, and the
	// cut handling below stays.
	if cut {
		for _, e := range evs {
			if e.Kind == event.ModelResponded && e.StopReason != "" && e.Error == "" {
				cut = false
			}
		}
	}
	out := evs[:0]
	for _, e := range evs {
		// A reply cut off claims no tool call and no turn end. What it showed
		// of the vendor's shapes is still true, so schema.unexpected stays.
		if cut && e.Kind != event.ModelResponded && e.Kind != event.SchemaUnexpected {
			continue
		}
		// A probe exchange started no turn, so its reply claims no tool call
		// and no turn end, whatever it says: a quota check answered 200 with
		// end_turn would otherwise end a turn that never started. The adapter
		// sees only the reply, so the gateway, which keeps the request's probe
		// mark, decides. Shapes stay reported.
		if ex.probe && e.Kind != event.ModelResponded && e.Kind != event.SchemaUnexpected {
			continue
		}
		if e.Kind == event.ModelResponded {
			e.LatencyMS = &latency
			if cut {
				e.Error = strings.TrimPrefix(e.Error+"; the reply was cut off", "; ")
			}
		}
		out = append(out, e)
	}
	g.emit(ex, out)
}

// maxDecoded bounds a decompressed copy, the adapter's and nothing else: a
// small compressed body can expand without limit. It matches queueLimit and
// the largest stream line an adapter reads.
const maxDecoded = 64 << 20

// decode returns the body a copy holds, decompressed for the adapter. what is
// "request" or "reply", for the error wording. The traffic itself is never
// decoded.
//
// cut says the copy ended early. A compressed stream cut before its end (a
// gzip footer, a zstd frame end) fails to decode at the cut, yet the bytes
// before it may hold the whole reply: a client that stops reading after the
// terminal event leaves no footer. So for a cut copy, what decoded before the
// failure goes to the adapter, and the cut rule in finish decides from what
// the adapter saw. A complete copy that fails to decode is an error.
func decode(encoding string, raw []byte, what string, cut bool) ([]byte, error) {
	var r io.Reader
	var err error
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		r, err = gzip.NewReader(bytes.NewReader(raw))
	case "deflate":
		r, err = zlib.NewReader(bytes.NewReader(raw))
	case "zstd":
		var zr *zstd.Decoder
		zr, err = zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(maxDecoded))
		if err == nil {
			defer zr.Close()
			r = zr
		}
	default:
		return nil, fmt.Errorf("the %s uses a content encoding the gateway cannot read", what)
	}
	if err != nil {
		return nil, fmt.Errorf("the compressed %s could not be decoded", what)
	}
	out, err := io.ReadAll(io.LimitReader(r, maxDecoded+1))
	if err != nil && !(cut && len(out) > 0) {
		return nil, fmt.Errorf("the compressed %s could not be decoded", what)
	}
	if len(out) > maxDecoded {
		return nil, fmt.Errorf("the %s decodes to more than %d MiB, more than the gateway reads", what, maxDecoded>>20)
	}
	return out, nil
}

// Close waits for replies already received to become events, delivers every
// queued event, then stops. Requests after Close still pass through, without
// events.
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.finishing.Wait()
	if g.bodies != nil {
		g.bodies.close()
	}
	g.queue.close()
}

// dropped builds the event that reports lost events.
func (g *Gateway) dropped(n int, sessions []string) event.Event {
	return event.Event{
		ID:          fmt.Sprintf("e-%s-%d", g.run, g.seq.Add(1)),
		Kind:        event.EventsDropped,
		Time:        g.now(),
		Vendor:      g.adapter.Vendor(),
		Dropped:     n,
		Sessions:    sessions,
		AllSessions: sessions == nil,
		Error:       "the event output did not keep up",
	}
}

func (g *Gateway) emit(ex *exchange, evs []event.Event) {
	for _, e := range evs {
		// A relayed call's turn has its turn.started from the runner's
		// engine, with the person's words as sent, written before its agent
		// started. On the runner the store kept that one and dropped the
		// gateway's; here the gateway's could arrive first, so it is not made.
		if ex.relayed && e.Kind == event.TurnStarted {
			continue
		}
		if e.Kind == event.SchemaUnexpected {
			x, ok := ex.admit(e.Unexpected)
			if !ok {
				continue
			}
			e.Unexpected = &x
		}
		// One rule for every adapter: a call whose path names its model
		// (adapter.Call.Model) is recorded with that model, which is the
		// one the vendor runs. A reply that names its own model version
		// keeps it.
		switch {
		case ex.model == "":
		case e.Kind == event.ModelRequested:
			e.Model = ex.model
		case e.Kind == event.ModelResponded && e.Model == "":
			e.Model = ex.model
		}
		e.ID = fmt.Sprintf("e-%s-%d", g.run, g.seq.Add(1))
		e.Time = g.now()
		e.Session = ex.session
		e.Exchange = ex.id
		e.Vendor = g.adapter.Vendor()
		e.Client = ex.client
		e.Turn = ex.turn
		e.Probe = ex.probe
		g.queue.push(e)
	}
}

// admit says whether a schema.unexpected shape is new to the exchange and
// within its bound, and returns it cleaned to names only, whichever adapter
// made it. One without a shape is never emitted.
func (ex *exchange) admit(x *event.Unexpected) (event.Unexpected, bool) {
	if x == nil {
		return event.Unexpected{}, false
	}
	c := adapter.Clean(*x)
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return c, ex.unknowns.Admit(c)
}

// queue hands events to the sink from one goroutine, in the order they were
// pushed. push never waits on the sink, so a slow or blocked output never holds
// up an agent. Waiting events are bounded by size. Past the bound, new events
// are counted instead of kept, and one events.dropped event reports the count
// once the output takes events again.
type queue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending []event.Event
	bytes   int
	limit   int
	lost    int
	lostIn  map[string]struct{} // the sessions of the lost events, at most maxLostSessions
	lostAll bool                // more sessions lost events than lostIn may hold
	report  func(n int, sessions []string) event.Event
	closed  bool
	done    chan struct{}
	sink    event.Sink

	// pushed and delivered count kept events; a barrier fires once
	// delivered reaches the pushed count it was set at.
	pushed, delivered uint64
	barriers          []queueBarrier
	// reporting counts lost events taken for an events.dropped report not
	// yet handed to the sink. A barrier waits for it as for q.lost, so it
	// never fires between the loss being taken and its report being
	// written.
	reporting int
}

type queueBarrier struct {
	at uint64
	f  func()
}

// barrier calls f once every event pushed before it has been handed to the
// sink, and any events.dropped report for events lost before it. A waiting
// barrier counts barrierSize bytes against the queue's bound, like an event;
// past the bound it is not queued and barrier reports false. f is then never
// called.
func (q *queue) barrier(f func()) bool {
	q.mu.Lock()
	if q.closed || (q.delivered >= q.pushed && q.lost == 0 && q.reporting == 0) {
		q.mu.Unlock()
		f()
		return true
	}
	if q.bytes+barrierSize > q.limit {
		q.mu.Unlock()
		return false
	}
	q.bytes += barrierSize
	q.barriers = append(q.barriers, queueBarrier{at: q.pushed, f: f})
	q.mu.Unlock()
	return true
}

// barrierSize is what a waiting barrier counts against a queue's byte bound:
// the fixed part an event counts.
const barrierSize = 512

// due takes the barriers that may fire. Called with q.mu held.
func (q *queue) due(all bool) []func() {
	var out []func()
	keep := q.barriers[:0]
	for _, b := range q.barriers {
		if all || (b.at <= q.delivered && q.lost == 0 && q.reporting == 0) {
			out = append(out, b.f)
			q.bytes -= barrierSize
		} else {
			keep = append(keep, b)
		}
	}
	q.barriers = keep
	return out
}

func newQueue(sink event.Sink, limit int, report func(int, []string) event.Event) *queue {
	q := &queue{sink: sink, limit: limit, report: report, done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	go q.run()
	return q
}

// size approximates the memory an event holds: a fixed part plus every
// variable-length field.
func size(e event.Event) int {
	n := 512 + len(e.ID) + len(e.Session) + len(e.Exchange) + len(e.Vendor) + len(e.Client) +
		len(e.Turn) + len(e.Model) + len(e.Text) + len(e.StopReason) + len(e.Error) +
		len(e.ToolID) + len(e.ToolName) + len(e.ToolInput) + len(e.ToolOutput) +
		len(e.Harness) + len(e.Folder) + len(e.Hold) + len(e.ReplayFrom) + len(e.Continuity) +
		len(e.RecoveredFrom) + len(e.RecoveredReason) + len(e.Context) + len(e.Call)
	for _, s := range e.Sessions {
		n += 16 + len(s)
	}
	for _, s := range e.Unkept {
		n += 16 + len(s)
	}
	for _, t := range e.ToolsOffered {
		n += 16 + len(t)
	}
	if p := e.Plan; p != nil {
		n += len(p.Subscription) + len(p.Connection) + len(p.Window) + len(p.Status)
	}
	if u := e.Usage; u != nil {
		n += len(u.ServiceTier) + len(u.Speed) + len(u.InferenceGeo)
		for k := range u.ServerToolUse {
			n += 16 + len(k)
		}
	}
	return n
}

func (q *queue) push(e event.Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if n := size(e); q.bytes+n > q.limit {
		q.lost++
		q.noteLost(e.Session)
	} else {
		q.pending = append(q.pending, e)
		q.bytes += n
		q.pushed++
	}
	q.cond.Signal()
}

// maxLostSessions bounds the session names kept while events are being lost.
// The queue is full when it loses events, so this memory is outside its byte
// limit and needs its own bound: an agent can send any number of /s/<session>
// paths.
const maxLostSessions = 256

// maxLostSessionBytes bounds one kept name; a longer one counts as every
// session too.
const maxLostSessionBytes = 256

// noteLost records the session of a lost event. Past either bound, the report
// says every session instead of listing them. Called with q.mu held.
func (q *queue) noteLost(session string) {
	if q.lostAll {
		return
	}
	if _, ok := q.lostIn[session]; ok {
		return
	}
	if len(q.lostIn) >= maxLostSessions || len(session) > maxLostSessionBytes {
		q.lostAll, q.lostIn = true, nil
		return
	}
	if q.lostIn == nil {
		q.lostIn = map[string]struct{}{}
	}
	q.lostIn[session] = struct{}{}
}

func (q *queue) run() {
	defer close(q.done)
	defer func() {
		q.mu.Lock()
		fs := q.due(true)
		q.mu.Unlock()
		for _, f := range fs {
			f()
		}
	}()
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && q.lost == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.pending) == 0 && q.lost == 0 {
			q.mu.Unlock()
			return
		}
		// The batch stays counted in q.bytes until each event has been taken
		// by the sink, so a batch stuck behind a blocked output still counts.
		batch, lost, lostIn, lostAll := q.pending, q.lost, q.lostIn, q.lostAll
		q.pending, q.lost, q.lostIn, q.lostAll = nil, 0, nil, false
		q.reporting += lost
		q.mu.Unlock()
		for i, e := range batch {
			q.sink.Emit(e)
			batch[i] = event.Event{} // let the delivered event's memory go
			q.mu.Lock()
			q.bytes -= size(e)
			q.delivered++
			var fs []func()
			if lost == 0 {
				fs = q.due(false)
			}
			q.mu.Unlock()
			for _, f := range fs {
				f()
			}
		}
		if lost > 0 {
			var sessions []string // nil means every session: lostAll
			if !lostAll {
				sessions = make([]string, 0, len(lostIn))
				for s := range lostIn {
					sessions = append(sessions, s)
				}
				sort.Strings(sessions)
			}
			q.sink.Emit(q.report(lost, sessions))
			// Barriers set while events were being lost fire only once the
			// loss is reported, so a reader that waited sees the report.
			q.mu.Lock()
			q.reporting -= lost
			fs := q.due(false)
			q.mu.Unlock()
			for _, f := range fs {
				f()
			}
		}
	}
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Signal()
	q.mu.Unlock()
	<-q.done
}

// tee passes a reply through untouched and keeps a copy. When the reply ends,
// or stops early, done gets the copy once, with cut set if it did not end cleanly.
type tee struct {
	rc   io.ReadCloser
	buf  bytes.Buffer
	once sync.Once
	done func(raw []byte, cut bool)
}

func (t *tee) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	t.buf.Write(p[:n])
	switch {
	case err == io.EOF:
		t.once.Do(func() { t.done(t.buf.Bytes(), false) })
	case err != nil:
		t.once.Do(func() { t.done(t.buf.Bytes(), true) })
	}
	return n, err
}

func (t *tee) Close() error {
	err := t.rc.Close()
	t.once.Do(func() { t.done(t.buf.Bytes(), true) })
	return err
}
