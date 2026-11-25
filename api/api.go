// Package api is the client API: what cockpits and programs call, under
// /api, on the gateway's own address. The route table (routes.go) and its
// description (spec.go) are the contract.
//
// The whole /api namespace is ours: /api, /s/<session>/api and everything under
// either is answered here and never forwarded to a vendor. Reserved says which
// requests those are.
//
// The token is compared and never repeated: not in a URL, a log, an event or an
// error. The one reply that carries it is POST /api/v0/signin's, to a caller
// on this service's own address that sent a one-time code (signin.go). Error
// bodies use fixed wording.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/toolreg"
)

// Config is what the API needs.
type Config struct {
	Store *store.Store
	// Token is the bearer token every request but an allowed preflight sends.
	Token string
	// TurnsToken, when set, is a second token that may only start, stop and
	// read turns: the routes whose turns field is set. A job queue or a
	// runner uses it in place of Token. Empty turns it off.
	TurnsToken string
	// Port is the port the service listens on. A request's Host must be
	// 127.0.0.1:<Port>, localhost:<Port> or [::1]:<Port>, unless AnyHost.
	Port string
	// AnyHost turns the Host check off. Set it when the service listens on
	// an address other machines can reach, such as 0.0.0.0. See ServeHTTP.
	AnyHost bool
	// BaseURL is the address a run's agent reaches the service on, such as
	// http://127.0.0.1:8787. The agent runs on this machine, so it is a
	// loopback address whenever the service listens on one.
	// A run's session has the base URL BaseURL/s/<id>.
	BaseURL string
	// Origin, when set, is the one cross-origin caller allowed, such as
	// http://localhost:5173. Empty allows every origin. See ServeHTTP.
	Origin string
	// Build is which build of AgentBus this is, for GET /api/v0/version.
	// The zero value answers an empty version.
	Build Version

	// Ping is how often a stream sends a comment to keep the connection
	// open. Zero means 15 seconds.
	Ping time.Duration
	// BodyTimeout bounds how long a request that is not a stream may take to
	// send its body. Zero means 10 seconds.
	BodyTimeout time.Duration
	// ToolHoldTimeout and ToolHoldPoll are how long the tool gate holds an Ask
	// tool call before it refuses it for want of an answer, and how often it
	// reads control's record for the decision meanwhile (B6, toolgate.go). Zero
	// means H1's own gateway.HoldTimeout and gateway.PollInterval, so a tool
	// call and a shell command wait the same; tests shorten them.
	ToolHoldTimeout time.Duration
	ToolHoldPoll    time.Duration
	// ToolHoldMax bounds how many Ask tool calls the gate holds at once across
	// all turns; past it a call is refused rather than held. Zero means
	// maxToolHolds.
	ToolHoldMax int
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Queue is the job queue: POST /api/v0/turns makes a turn job in it,
	// and a stop cancels the job. Nil turns starting turns off: POST
	// /api/v0/turns answers 404.
	Queue *queue.Queue
	// TurnWait bounds how long a changes preview or a turn's diff waits on a
	// turn's job. Zero means DefaultTurnWait.
	TurnWait time.Duration
	// Engine is this process's own turn engine, when it runs turns: its live
	// turns' own tokens are accepted here. Nil accepts none.
	Engine *run.Engine

	// Getenv reads this service's environment by name, the same source the
	// relay resolves an agent's key_env from, so GET /api/v0/subscriptions
	// reports whether a key variable is set (env_keys) without ever reading
	// its value. Nil reads nothing.
	Getenv func(string) string

	// Hosts are more names, without a port, that the Host header may name
	// while the service listens on loopback: host.lima.internal, by which
	// a Lima VM reaches the Mac's loopback. The token still guards every
	// request from there.
	Hosts []string

	// Folders are this machine's folders, in agentbus up: a folder a
	// request names by path is kept as its reference (queue.ResolveFolders).
	// Nil, as in agentbus server, takes references only.
	Folders queue.FolderIssuer

	// Agents are the saved agents: /api/v0/agents, and the agent a turn
	// names. Nil lists none, saves none, and lets a turn name a harness only.
	Agents *agents.Store

	// Jobs serves the runner protocol (api/jobs.go), and closes a session's
	// copy: package jobs. Nil answers the runner protocol 404.
	Jobs *jobs.Server
	// RunnerTokens says what the turn token of a runner's turn may do,
	// while its attempt is current: control minted it (jobs.Server.TurnGrant).
	// Nil: only this process's own turns have tokens.
	RunnerTokens func(tok string) (run.Grant, bool)
	// Subscriptions connects the person's own subscriptions on control, for
	// their runners (subscriptions.go). Nil: none here.
	Subscriptions *credentials.Manager
	// Models are where GET /api/v0/harnesses/{id}/models reads each list.
	// Nil: it lists none.
	Models *ModelSources

	// Settings hold the policy and the instructions. The policy's
	// [agent_api] says what a turn token may call and its [limits] cap the
	// work turns ask for, as the rules its turn took at its start say
	// (rulesOf): a change applies from the next turn. The
	// settings routes read and write them (settings.go). Nil: the defaults,
	// and the settings routes answer 404.
	Settings settings.Store

	// Tools is the tool registry's overlay store, where the person's added
	// and removed tools are kept on control (toolreg). Nil keeps the overlay
	// in memory: the built-in tools are still served, and add and remove last
	// only this process. New sets the built-in tools on it.
	Tools *toolreg.Registry

	// Logf writes one line to the service log for each change the API
	// makes to control's own state, such as a settings file written.
	// Never a value. Nil: none.
	Logf func(format string, args ...any)
}

// API is the client API's http.Handler.
type API struct {
	cfg   Config
	token [32]byte
	// turnsToken is the SHA-256 of Config.TurnsToken, when hasTurnsToken.
	turnsToken    [32]byte
	hasTurnsToken bool
	hosts         []string

	// streams admits at most MaxStreams at once.
	streams chan struct{}
	// toolHolds admits at most maxToolHolds held Ask tool calls at once, across
	// all turns (toolgate.go). Each hold keeps a request goroutine and polls the
	// record for up to the hold window, so an agent firing many parallel tool
	// calls cannot hold them all open: past the cap a call is refused at once
	// rather than held.
	toolHolds chan struct{}

	// spec is the OpenAPI description, built from the route table, and
	// runnerSpec the runner protocol's.
	spec, runnerSpec []byte
	// registry is the tools control serves: the built-in apps (the 13 routes
	// that were tools), plus what the person added or removed (tools.go,
	// tools_registry.go). toolSnap is the served set, rebuilt on every change
	// and read without a lock.
	registry *toolreg.Registry
	toolSnap atomic.Pointer[toolSnapshot]
	// toolsMu serialises a tool change with the snapshot it rebuilds, so two
	// changes at once cannot publish a snapshot that misses one of them
	// (tools_registry.go).
	toolsMu sync.Mutex
	// routes is the route table by "METHOD path", so a tool's run resolves to
	// the route it makes without the route table's initializer referring to
	// itself (tools_registry.go).
	routes map[string]*route
	// byOp is the route table by operationId, so an app's step resolves the
	// operation it names (apps.go).
	byOp map[string]*route

	// signin holds the one-time sign-in codes (signin.go).
	signin signIns

	// deciding holds the ids of approvals an answer is in flight for, so two
	// at once (two tabs, a retry) do not both move the settings or start two
	// continuations. approval.decided in the record is the durable "answered"
	// mark; this serialises only within this process (approvals.go).
	decidingMu sync.Mutex
	deciding   map[string]bool

	// previews serialises running and storing a session's preview, so two GETs
	// of its changes do not run two previews and race to store them: the second
	// waits for the first and reuses its result (changes.go).
	previews *sessionGate
}

// New returns the API.
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
	if cfg.ToolHoldMax <= 0 {
		cfg.ToolHoldMax = maxToolHolds
	}
	for _, h := range cfg.Hosts {
		if h == "" || strings.ContainsAny(h, ":/ ") {
			return nil, errors.New("api: a host is a name, without a port")
		}
	}
	if err := checkActions(); err != nil {
		return nil, err
	}
	if err := checkTools(); err != nil {
		return nil, err
	}
	desc, err := spec()
	if err != nil {
		return nil, err
	}
	tools := allTools()
	schemas, err := toolSchemas(tools, desc)
	if err != nil {
		return nil, err
	}
	runnerDesc, err := runnerSpec()
	if err != nil {
		return nil, err
	}
	reg := cfg.Tools
	if reg == nil {
		if reg, err = toolreg.New(nil); err != nil {
			return nil, err
		}
	}
	apps := defaultApps()
	reg.SetDefaults(append(defaultTools(tools, schemas), apps...))
	a := &API{cfg: cfg, token: sha256.Sum256([]byte(cfg.Token)), spec: desc, runnerSpec: runnerDesc,
		registry: reg, routes: routeIndex(), byOp: opIndex(),
		hosts:     []string{"127.0.0.1:" + cfg.Port, "localhost:" + cfg.Port, "[::1]:" + cfg.Port},
		streams:   make(chan struct{}, MaxStreams),
		toolHolds: make(chan struct{}, cfg.ToolHoldMax),
		previews:  newSessionGate()}
	// Validate the default apps against the route table, as createTool does a
	// custom one: a default app that steps through a route since changed (a
	// renamed op, a route a turn token may no longer call) stops the service,
	// rather than listing a tool that cannot run (checkTools does the same for
	// route tools).
	for _, app := range apps {
		if why := toolreg.Check(&app); why != "" {
			return nil, fmt.Errorf("api: default app %s: %s", app.Name, why)
		}
		if why := a.checkApp(app); why != "" {
			return nil, fmt.Errorf("api: default app %s: %s", app.Name, why)
		}
	}
	a.rebuildTools()
	for _, h := range cfg.Hosts {
		a.hosts = append(a.hosts, h+":"+cfg.Port)
	}
	if cfg.TurnsToken != "" {
		if cfg.TurnsToken == cfg.Token {
			return nil, errors.New("api: the turns token is the main token")
		}
		a.turnsToken, a.hasTurnsToken = sha256.Sum256([]byte(cfg.TurnsToken)), true
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

// BlobTimeout bounds how long one blob's upload may take: a blob is a whole
// file of a snapshot or of a turn's changes.
const BlobTimeout = 30 * time.Minute

// bodyLimit is MaxBody, except for a turn, which carries a prompt, a
// program's own event, the settings and the runner protocol.
func bodyLimit(r *http.Request) int64 {
	switch p := r.URL.Path; {
	case p == "/api/v0/turns" || p == "/api/v0/mcp":
		// A tool call can carry a turn's prompt.
		return MaxRunBody
	case p == "/api/v0/events":
		return MaxEventBody
	case strings.HasPrefix(p, "/api/v0/settings/"):
		return MaxSettingsBody
	case r.Method == http.MethodPut && strings.HasPrefix(p, jobs.BlobPath):
		// A blob is a whole file, streamed to disk (package jobs).
		return blobs.MaxBlob
	case p == jobs.PackPath:
		// Many small blobs, streamed to disk (package jobs).
		return blobs.MaxPackBody
	case strings.HasPrefix(p, "/api/v0/runner/"):
		// The runner protocol: a finish carries a result with the reply,
		// and a record batch a turn's events.
		return jobs.MaxBody
	}
	return MaxBody
}

// MaxFolder bounds a turn's folder, in bytes.
const MaxFolder = run.MaxFolder

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
		wait := a.cfg.BodyTimeout
		if (r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, jobs.BlobPath)) || r.URL.Path == jobs.PackPath {
			wait = BlobTimeout
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(wait))
	}
	// The Host check stops DNS rebinding: a web page whose name the attacker
	// points at 127.0.0.1 reaches a loopback service from the person's own
	// browser, and its requests carry the attacker's name in Host. It only
	// helps while the service is on loopback. On 0.0.0.0 any machine that
	// reaches the address can send any Host it likes, and behind a port
	// publish or a proxy the Host names an address the service cannot know.
	// There the token on every API request is the guard, and a rebound page
	// does not have it. The docs and the description need no token either
	// way; they hold nothing of the person's.
	if !a.cfg.AnyHost && !slices.Contains(a.hosts, r.Host) {
		// Before anything else, and without Allow-Origin: a page reaching
		// us through DNS rebinding learns nothing.
		writeError(w, http.StatusForbidden, "the Host header must name this service on 127.0.0.1, localhost or [::1]")
		return
	}
	// The sign-in code's route takes no part in CORS: no page on another
	// origin may read its reply (signin.go).
	if r.URL.Path == SignInPath {
		a.signIn(w, r)
		return
	}
	// CORS mainly guards cookies, and this API has none: every request
	// needs the bearer token in a header, which a page from another origin
	// does not have. So by default every origin is allowed, with "*" and
	// no credentials, and the token stays the guard. "*" rather than the
	// request's own Origin echoed back: the answer is the same for every
	// origin, so it needs no Vary and caches cleanly, and "*" can never be
	// combined with credentials. -origin, when set, locks it to that one
	// origin.
	origin := r.Header.Get("Origin")
	allowed := origin != "" && (a.cfg.Origin == "" || origin == a.cfg.Origin)
	if a.cfg.Origin == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	} else if allowed {
		w.Header().Set("Access-Control-Allow-Origin", a.cfg.Origin)
		w.Header().Add("Vary", "Origin")
	}
	if r.Method == http.MethodOptions && origin != "" {
		if !allowed {
			writeError(w, http.StatusForbidden, "this origin is not allowed")
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusForbidden, "no preflight is answered on this path")
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
		// Named, not "*": the wildcard never covers Authorization.
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if a.public(w, r) {
		return // the docs and the description, docs.go
	}
	if a.self(w, r) {
		return // the runner protocol checks its own tokens
	}
	g, ok := a.authorized(r)
	if !ok {
		if a.turnTokenEnded(r) {
			writeError(w, http.StatusUnauthorized, turnTokenEnded)
			return
		}
		writeError(w, http.StatusUnauthorized, "a valid bearer token is required")
		return
	}
	a.route(w, r, g)
}

// authorized resolves the bearer token to the grant it carries, each naming the
// record's owner: the owner (main) token to every action in every workspace,
// the turns token to its fixed set, and a live turn's own token to its turn's
// actions, whose reach the door works out (authority.go). ok is
// false for any other token. This is the one place a token becomes a grant;
// everything after reads the grant.
func (a *API) authorized(r *http.Request) (g run.Grant, ok bool) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return g, false
	}
	// Hashed first, so the compare takes the same time whatever the length.
	sum := sha256.Sum256([]byte(got))
	owner := a.grantOwner()
	if subtle.ConstantTimeCompare(sum[:], a.token[:]) == 1 {
		return run.OwnerGrant(owner), true
	}
	if a.hasTurnsToken && subtle.ConstantTimeCompare(sum[:], a.turnsToken[:]) == 1 {
		return run.TurnsGrant(owner), true
	}
	if a.cfg.Engine != nil {
		if g, ok = a.cfg.Engine.TokenGrant(got); ok {
			return run.ForTurn(g, owner), true
		}
	}
	if a.cfg.RunnerTokens != nil {
		if g, ok = a.cfg.RunnerTokens(got); ok {
			return run.ForTurn(g, owner), true
		}
	}
	return run.Grant{}, false
}

// turnTokenEnded is the 401 for a turn's token once its turn has ended, so
// a process the turn left running can tell what happened.
const turnTokenEnded = "this token ended with its turn: a turn's token works only while the turn runs"

// turnTokenEnded says the request's token is a turn's token that no longer
// works: the shape of this process's own turn tokens (they are random, so
// one that is not live has ended), or a token control minted for an attempt
// that is no longer current.
func (a *API) turnTokenEnded(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	if rest, ok := strings.CutPrefix(got, run.TurnTokenPrefix); ok && len(rest) == 64 && isHex(rest) {
		return true
	}
	return a.cfg.Jobs != nil && a.cfg.Jobs.TurnTokenEnded(got)
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
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

// changed records one change to control's own state: a line in the service
// log and an event in no session.
func (a *API) changed(e event.Event, format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
	e.ID, e.Time = newEventID(), a.cfg.Now().UTC()
	if _, _, err := a.cfg.Store.Append(e); err != nil && a.cfg.Logf != nil {
		a.cfg.Logf("store.error what=%s", e.Kind)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Snapshot is every session: what the record says of each, and whether it
// is closed, which the queue holds.
type Snapshot struct {
	Store    string    `json:"store" doc:"The store id. Send it back as store."`
	Seq      int64     `json:"seq" doc:"The seq the snapshot was taken at. Send it back as after to stream from here."`
	Sessions []Session `json:"sessions" doc:"A page of sessions, newest activity first."`
	// NextBefore is the before for the next, older page.
	NextBefore *int64 `json:"next_before" doc:"The before for the next, older page, or null when there are no older sessions."`
}

// Session is one session: the record's, when it was closed, and -- only on the
// single GET /api/v0/sessions/{id} -- where its turns run.
type Session struct {
	store.Session
	Closed *time.Time `json:"closed,omitempty" doc:"When the session was closed, before close was retired. It took no more turns, and resumable is false. New sessions are never closed; closing now archives. The default list hides a closed session, like an archived one."`
	// Archived (when the session was archived) comes from the embedded
	// store.Session: it is a session.archived event in the record, so it is set
	// here with no help from the queue.
	// Copy is where the session's turns run, filled only by the single
	// GET /api/v0/sessions/{id}; the list leaves it absent, as a turn's prompt
	// and reply are absent on the turn list.
	Copy *SessionCopy `json:"copy,omitempty" doc:"Only on GET /api/v0/sessions/{id}: where the session's turns run, on its runner. Absent on the sessions list, and absent until the session's first turn on this service has ended."`
}

// snapshot is a page of sessions, each with closed and archived set from the
// queue. A closed session is never resumable. Archived sessions are left out
// unless showArchived, which only changes what the page shows: nothing is
// removed. A page may hold fewer than limit once archived sessions are left
// out; NextBefore still steps to the next page.
func (a *API) snapshot(at, before int64, limit int, showArchived bool) (Snapshot, error) {
	snap, err := a.cfg.Store.Sessions(at, before, limit)
	if err != nil {
		return Snapshot{}, err
	}
	closed := map[ids.WorkspaceID]time.Time{}
	if a.cfg.Queue != nil {
		if closed, err = a.cfg.Queue.Closed(queue.DefaultTeam); err != nil {
			return Snapshot{}, err
		}
	}
	out := Snapshot{Store: snap.Store, Seq: snap.Seq, Sessions: make([]Session, 0, len(snap.Sessions)), NextBefore: snap.NextBefore}
	for _, s := range snap.Sessions {
		v := a.withClosed(s, closed)
		// The default list hides a session set aside: archived (a
		// session.archived event in the record) or closed (the retired close,
		// kept for old rows). archived=true shows them.
		if (v.Archived != nil || v.Closed != nil) && !showArchived {
			continue
		}
		out.Sessions = append(out.Sessions, v)
	}
	return out, nil
}

// withClosed is s with when it was closed, from the closed map. Archived is
// already on s, from the store's session.archived.
func (a *API) withClosed(s store.Session, closed map[ids.WorkspaceID]time.Time) Session {
	v := Session{Session: s}
	// closed is keyed by workspace id (the copy's), and s.Workspace is the
	// session's workspace as the snapshot resolved it (a migrated session's is its
	// own id), so one lookup reads the workspace's closed state. s.Workspace is
	// the session id already turned into its workspace, not a cast here.
	c, ok := closed[ids.WorkspaceID(s.Workspace)]
	if ok {
		v.Closed = &c
		v.Resumable = false
	}
	return v
}

// GET /api/v0/sessions
func (a *API) sessions(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	// store and after from the first page pin the snapshot, so later pages do
	// not skip a session that gained activity meanwhile. Absent on the first.
	at, set, ok := a.cursor(w, r)
	if !ok {
		return
	}
	if !set {
		at = -1 // No pin: the latest snapshot. An explicit after=0 is a pin.
	}
	// A pin beyond the store's latest seq is not a snapshot that was ever
	// taken: honouring it would let later events move a session and skip it, so
	// it is refused rather than quietly served against the newest.
	if at > a.cfg.Store.Last() {
		writeError(w, http.StatusBadRequest, "after is beyond the store's latest seq")
		return
	}
	showArchived := r.URL.Query().Get("archived") == "true"
	snap, err := a.snapshot(at, before, limit, showArchived)
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
// at a time. turn keeps one turn's.
func (a *API) sessionEvents(w http.ResponseWriter, r *http.Request, session string) {
	if q := r.URL.Query(); q.Has("turn") {
		turn := q.Get("turn")
		if !sessionID.MatchString(turn) {
			writeError(w, http.StatusBadRequest, "turn must match "+sessionID.String())
			return
		}
		a.eventPage(w, r, store.Filter{Turn: turn, TurnSession: session})
		return
	}
	f := store.Filter{Session: session, Only: true}
	if q := r.URL.Query(); q.Has("kind") {
		f.Kinds = strings.Split(q.Get("kind"), ",")
		for _, k := range f.Kinds {
			if !kindShape.MatchString(k) || len(f.Kinds) > 16 {
				writeError(w, http.StatusBadRequest, "kind must be up to 16 event kinds, comma separated, such as turn.started,turn.completed")
				return
			}
		}
	}
	a.eventPage(w, r, f)
}

// kindShape is an event kind as a filter names it.
var kindShape = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+){1,3}$`)

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

// noBody refuses a stream request that carries a body. Such a request stays
// open, so its body would get no deadline.
func noBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "this request takes no body")
		return false
	}
	return true
}

// checkActions says every route that takes a token names the one action class
// it needs, so no route is open or closed by an omission.
func checkActions() error {
	for _, rt := range routes {
		if rt.public || rt.self {
			continue
		}
		if c := rt.action; c == 0 || c&(c-1) != 0 || !run.AllActions.Has(c) {
			return fmt.Errorf("api: %s %s must name one action class", rt.method, rt.path)
		}
	}
	return nil
}
