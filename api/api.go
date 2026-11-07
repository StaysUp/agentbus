// Package api is the client API: what cockpits and programs call, under
// /api, on the gateway's own address. docs/CLIENT_API.md is the contract.
//
// The whole /api namespace is ours: /api, /s/<session>/api and everything under
// either is answered here and never forwarded to a vendor. Reserved says which
// requests those are.
//
// The token is compared and never repeated: not in a URL, a log, an event or an
// error. Error bodies use fixed wording.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/store"
)

// ReplayPlan is how a new claude turn continues its session by own replay.
type ReplayPlan struct {
	// Turn is the earlier turn whose history is replayed: the session's
	// newest. Empty when the session cannot be continued by replay.
	Turn string
	// Failed, with Turn, says how that turn ended without finishing, as the
	// history's note tells the model. Empty when it finished.
	Failed string
	// Refused, without Turn, says why the session cannot be continued by
	// replay, naming the turn.
	Refused string
}

// Config is what the API needs.
type Config struct {
	Store *store.Store
	// Token is the bearer token every request but an allowed preflight sends.
	Token string
	// Port is the port the service listens on. A request's Host must be
	// 127.0.0.1:<Port> or localhost:<Port>.
	Port string
	// BaseURL is the service's own address, such as http://127.0.0.1:8787.
	// A run's session has the base URL BaseURL/s/<id>.
	BaseURL string
	// Origin is the one cross-origin caller allowed, such as
	// http://localhost:5173. Empty allows none.
	Origin string

	// Ping is how often a stream sends a comment to keep the connection
	// open. Zero means 15 seconds.
	Ping time.Duration
	// BodyTimeout bounds how long a request that is not a stream may take to
	// send its body. Zero means 10 seconds.
	BodyTimeout time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Wire says how a run reaches each agent's binary. Nil turns runs off:
	// POST /api/v0/turns gets 404.
	Wire Wire
	// ReplayBase turns on own replay for claude and codex: it plans how a
	// new turn of agent's session continues it (replay.Replayer.Base). Nil
	// turns own replay off, and both continue by the vendor's resume
	// (Continuity).
	ReplayBase func(agent, session string) (ReplayPlan, error)
	// Settle, when set, is called after a turn's child exits and before its
	// session.closed is stored. It returns once the gateway has written every
	// model call of the turn (gateway.Settle), so a reader that sees the
	// session closed sees all of the turn's calls: own replay picks the
	// turn's last reply from them. It reports false when that could not be
	// confirmed; session.closed then carries unsettled, and own replay will
	// not continue from that turn. Nil stores session.closed at once.
	Settle func(session, turn string) bool
	// RunGrace is how long StopRuns waits after SIGTERM before SIGKILL. Zero
	// means 5 seconds.
	RunGrace time.Duration

	// LookPath finds an agent's binary for GET /api/v0/agents, the same
	// lookup runs use. Environ is the environment its --version and status
	// commands run with. Nil LookPath answers every agent as not found.
	LookPath func(string) (string, error)
	Environ  func() []string

	// Logf writes one line to the service log: a run's child starting and
	// exiting. Nil writes nothing. It is never
	// given a token, a header, a body, a prompt or the child's output.
	Logf func(format string, args ...any)
	// RunLog keeps a run's child output, once per child exit: a header line
	// and the last RunOutputMax bytes of its stdout and stderr together. Nil
	// discards it. That output is the agent's own and may hold the person's
	// data, so it goes only here: never into an event, a reply or Logf.
	RunLog func(session string, data []byte)
}

// API is the client API's http.Handler.
type API struct {
	cfg   Config
	token [32]byte
	hosts [3]string

	// streams admits at most MaxStreams at once.
	streams chan struct{}

	runs runs

	// turnTokens are the live turns' tokens (tokens.go).
	turnTokens turnTokens

	agents agentCache

	// spec is the OpenAPI description, built from the route table.
	spec []byte
}

// New returns the API. It closes every run's child the service's previous
// process left open: that process can no longer report their exit.
// SetSettle sets Config.Settle, for a gateway made after the API (it needs
// the API's TurnSink). Call it before serving.
func (a *API) SetSettle(f func(session, turn string) bool) { a.cfg.Settle = f }

func New(cfg Config) (*API, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: no store")
	}
	if cfg.Token == "" {
		return nil, errors.New("api: no token")
	}
	if cfg.Port == "" {
		return nil, errors.New("api: no port")
	}
	if cfg.Ping == 0 {
		cfg.Ping = 15 * time.Second
	}
	if cfg.BodyTimeout == 0 {
		cfg.BodyTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunGrace == 0 {
		cfg.RunGrace = 5 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	desc, err := spec()
	if err != nil {
		return nil, err
	}
	a := &API{cfg: cfg, token: sha256.Sum256([]byte(cfg.Token)), spec: desc,
		hosts:   [3]string{"127.0.0.1:" + cfg.Port, "localhost:" + cfg.Port, "[::1]:" + cfg.Port},
		streams: make(chan struct{}, MaxStreams)}
	if _, err := cfg.Store.CloseStaleHolds(cfg.Now().UTC(), newEventID); err != nil {
		return nil, err
	}
	return a, nil
}

// Reserved reports whether a request belongs to the API namespace: its path is
// /api or /s/<session>/api, or under either, or /docs or under it. It is a plain prefix check, so
// the caller must first refuse paths with dot, empty or encoded segments, as
// cmd/agentbus does: with those gone, the path checked is the path served.
func Reserved(r *http.Request) bool {
	p := r.URL.Path
	if isDocs(p) {
		return true // the docs, docs.go
	}
	if tail, ok := strings.CutPrefix(p, "/s/"); ok {
		if _, rest, ok := strings.Cut(tail, "/"); ok {
			p = "/" + rest
		}
	}
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// MaxBody bounds an API request's body.
const MaxBody = 64 << 10

// bodyLimit is MaxBody, except for a run, which carries a prompt, and a
// program's own event.
func bodyLimit(r *http.Request) int64 {
	switch p := r.URL.Path; {
	case p == "/api/v0/turns":
		return MaxRunBody
	case p == "/api/v0/events":
		return MaxEventBody
	}
	return MaxBody
}

// MaxFolder bounds a run's folder, in bytes.
const MaxFolder = 4096

// MaxStreams bounds the event streams open at once, service-wide. Each holds a
// store subscription, a timer and a connection; past the bound a stream gets 429.
const MaxStreams = 32

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A body must arrive whole, soon, and small: a client that sends part of
	// one and waits would otherwise hold this goroutine for good. Only a
	// request with a body gets the deadline; streams have none and stay
	// open. The server clears it before the connection's next request.
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r))
	if r.ContentLength != 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(a.cfg.BodyTimeout))
	}
	if r.Host != a.hosts[0] && r.Host != a.hosts[1] && r.Host != a.hosts[2] {
		// Before anything else, and without Allow-Origin: a page reaching
		// us through DNS rebinding learns nothing.
		writeError(w, http.StatusForbidden, "the Host header must name this service on 127.0.0.1, localhost or [::1]")
		return
	}
	origin := r.Header.Get("Origin")
	allowed := a.cfg.Origin != "" && origin == a.cfg.Origin
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", a.cfg.Origin)
		w.Header().Add("Vary", "Origin")
	}
	if r.Method == http.MethodOptions && origin != "" {
		if !allowed || !strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusForbidden, "this origin is not allowed")
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "GET, POST")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if a.public(w, r) {
		return // the docs and the description, docs.go
	}
	main, g, ok := a.authorized(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "a valid bearer token is required")
		return
	}
	if main {
		a.route(w, r, nil)
		return
	}
	a.route(w, r, &g)
}

// authorized checks the bearer token: the main token (main true), or a live
// turn token and what it may read.
func (a *API) authorized(r *http.Request) (main bool, g grant, ok bool) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false, g, false
	}
	// Hashed first, so the compare takes the same time whatever the length.
	sum := sha256.Sum256([]byte(got))
	if subtle.ConstantTimeCompare(sum[:], a.token[:]) == 1 {
		return true, g, true
	}
	g, ok = a.turnTokens.lookup(got)
	return false, g, ok
}

func notAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers with fixed wording. The text never quotes the request.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, Error{msg})
}

// Error is every error the API answers with.
type Error struct {
	Error string `json:"error" doc:"Fixed wording. It never quotes the request."`
}

// StoreMismatch is the 409 for a cursor of another store.
type StoreMismatch struct {
	Error string `json:"error"`
	Store string `json:"store" doc:"The current store. Start over with GET /api/v0/sessions."`
}

func (a *API) storeMismatch(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, StoreMismatch{
		"the cursor belongs to another store; start over from the snapshot", a.cfg.Store.ID()})
}

func (a *API) internal(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "the store could not be read or written")
}

func newEventID() string { return "evt_" + randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GET /api/v0/sessions
func (a *API) sessions(w http.ResponseWriter, _ *http.Request) {
	snap, err := a.cfg.Store.Snapshot()
	if err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// cursor reads store and after, which come together or not at all. set is
// false when both are absent.
func (a *API) cursor(w http.ResponseWriter, r *http.Request) (after int64, set, ok bool) {
	q := r.URL.Query()
	st, hasStore := q["store"]
	af, hasAfter := q["after"]
	if !hasStore && !hasAfter {
		return 0, false, true
	}
	if !hasStore || !hasAfter || len(st) != 1 || len(af) != 1 {
		writeError(w, http.StatusBadRequest, "store and after are required together")
		return 0, false, false
	}
	n, err := strconv.ParseInt(af[0], 10, 64)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "after must be a whole number, 0 or more")
		return 0, false, false
	}
	if st[0] != a.cfg.Store.ID() {
		a.storeMismatch(w)
		return 0, false, false
	}
	return n, true, true
}

// GET /api/v0/events: replay after the cursor, then live, as server-sent events.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	after, set, ok := a.cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f store.Filter
	if q.Has("session") {
		f = store.Filter{Session: q.Get("session"), Only: true}
	}
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeError(w, http.StatusInternalServerError, "this connection cannot stream")
		return
	}

	// Subscribe before the first read: a write after this point signals, so
	// nothing falls between the replay and the live part. The cursor moves
	// past each event sent, so nothing is sent twice.
	// Admitted before anything is made for the stream.
	select {
	case a.streams <- struct{}{}:
		defer func() { <-a.streams }()
	default:
		writeError(w, http.StatusTooManyRequests, "too many event streams are open; close one and retry")
		return
	}
	notify, cancel := a.cfg.Store.Notify()
	defer cancel()
	if !set {
		after = a.cfg.Store.Last()
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(a.cfg.Ping)
	defer ping.Stop()
	id := a.cfg.Store.ID()
	for {
		for {
			recs, more, err := a.cfg.Store.Page(after, f, 1000, store.PageBytes)
			if err != nil {
				return // the stream ends; the cockpit reconnects with its cursor
			}
			for _, rec := range recs {
				if _, err := fmt.Fprintf(w, "id: %s/%d\nevent: %s\ndata: %s\n\n", id, rec.Seq, oneLine(string(rec.Kind)), rec.Data); err != nil {
					return
				}
				after = rec.Seq
			}
			if len(recs) > 0 {
				flusher.Flush()
			}
			if !more {
				break
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-notify:
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// oneLine keeps a kind from breaking the stream's framing.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

const (
	defaultLimit = 1000
	maxLimit     = 10000
)

// GET /api/v0/sessions/<id>/events: one session's events, oldest first, a page
// at a time.
func (a *API) sessionEvents(w http.ResponseWriter, r *http.Request, session string) {
	a.eventPage(w, r, store.Filter{Session: session, Only: true})
}

// eventPage answers one page of the events f picks, after the cursor.
func (a *API) eventPage(w http.ResponseWriter, r *http.Request, f store.Filter) {
	after, _, ok := a.cursor(w, r)
	if !ok {
		return
	}
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "limit must be from 1 to 10000")
			return
		}
		limit = n
	}
	recs, more, err := a.cfg.Store.Page(after, f, limit, store.PageBytes)
	if err != nil {
		a.internal(w)
		return
	}
	var next *int64
	if more {
		last := recs[len(recs)-1].Seq
		next = &last
	}
	evs := make([]json.RawMessage, 0, len(recs))
	for _, rec := range recs {
		evs = append(evs, rec.Data)
	}
	writeJSON(w, http.StatusOK, Page{a.cfg.Store.ID(), evs, next})
}

// Page is one page of a session's or a turn's events.
type Page struct {
	Store     string            `json:"store"`
	Events    []json.RawMessage `json:"events" doc:"Events, oldest first."`
	NextAfter *int64            `json:"next_after" doc:"The after for the next page, or null when there are no more events right now."`
}

const (
	defaultTurnLimit = 100
	maxTurnLimit     = 1000
)

// GET /api/v0/turns: a page of turns, newest first.
func (a *API) turns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tq := store.TurnQuery{Limit: defaultTurnLimit}
	if q.Has("session") {
		tq.Session = q.Get("session")
		if !sessionID.MatchString(tq.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return
		}
	}
	if q.Has("before") {
		n, err := strconv.ParseInt(q.Get("before"), 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be a whole number, 1 or more")
			return
		}
		tq.Before = n
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxTurnLimit {
			writeError(w, http.StatusBadRequest, "limit must be from 1 to 1000")
			return
		}
		tq.Limit = n
	}
	list, err := a.cfg.Store.Turns(tq)
	if err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /api/v0/turns/<id>/events: one turn's events, oldest first, a page at a
// time. session keeps one session's: the same turn id can be in two.
func (a *API) turnEvents(w http.ResponseWriter, r *http.Request, turn string) {
	f := store.Filter{Turn: turn}
	if q := r.URL.Query(); q.Has("session") {
		f.TurnSession = q.Get("session")
		if !sessionID.MatchString(f.TurnSession) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return
		}
	}
	a.eventPage(w, r, f)
}

// noBody refuses a stream request that carries a body. Such a request stays
// open, so its body would get no deadline.
func noBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "this request takes no body")
		return false
	}
	return true
}
