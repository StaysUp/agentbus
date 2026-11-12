// Package jobs is the runner protocol, under /api/v0/runner/: how runners,
// one on each machine, take jobs from control and report them. The client API (package
// api) makes and reads the jobs; this package never answers a client.
//
// Two tokens:
//   - the enrollment token (Config.EnrollToken): POST /api/v0/runner/register only.
//     The old way to add a machine; agentbus login is the new (login.go).
//   - a runner token, made when a runner registers or signs in and returned
//     once: every other call, for its own runner and the attempts it holds,
//     and only once its runner exists (a sign-in the person approved) and is
//     not removed.
//
// POST /api/v0/runner/login takes no token: anyone who reaches control may
// ask to sign a machine in, and the person decides.
//
// The main token (Config.Token) is not taken here; it only keys the turn
// tokens control signs for runners' turns (tokens.go).
//
// Tokens are compared by their SHA-256. A runner token is kept only as its
// hash. Error replies use fixed wording and never quote the request.
package jobs

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/staysup/agentbus/agents"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/workspace"
)

// The protocol's timings.
const (
	// Lease is how long an attempt is held without a heartbeat.
	Lease = 30 * time.Second
	// Heartbeat is how often a runner should send one, so a cancel, and so
	// a stop of a turn, is heard within about this long.
	Heartbeat = 2 * time.Second
	// MaxWait bounds a take's long poll.
	MaxWait = 30 * time.Second
	// MaxBody bounds a request body. A prompt can be 256 KiB.
	MaxBody = 2 << 20
	// MaxCapacity bounds what one runner may say it can run at once.
	MaxCapacity = 64
	// MaxBatch bounds one events batch.
	MaxBatch = 500
)

// ProtocolVersion is the runner protocol this server speaks. 2: the routes
// under /api/v0/runner/, and a job's turn named when the job is made. 3: the
// relay records every model call it carries, so a runner that holds no
// secret sends none of them (relay.go). Every register and every take names
// it, and a runner of another version is refused, so a runner and a server
// of different builds never both record a call, or both leave it out. 4: one
// runner per machine holds that machine's folders, takes their folder jobs
// with take's folders, and is the only runner of a job that names its
// machine; agentbus connector is retired.
const ProtocolVersion = 4

// Config is what the server needs.
type Config struct {
	Queue       *queue.Queue
	Token       string // the main token: it keys the turn tokens of runners' turns (tokens.go)
	EnrollToken string // adds a runner
	// Lease and Heartbeat override the defaults, for tests. Zero keeps them.
	Lease, Heartbeat, MaxWait time.Duration
	// Agents are the saved agents a job's spec.agent names. Nil takes
	// spec.agent as a harness name, as before saved agents.
	Agents *agents.Store
	// Settings are the policy and the instructions a runner reads for each
	// turn (GET /api/v0/runner/settings). Nil sends the defaults.
	Settings settings.Store
	// Folders are this machine's folders, in agentbus up: a job's folder
	// path becomes its reference there (queue.ResolveFolders). Nil, as in
	// agentbus server, takes references only.
	Folders queue.FolderIssuer
	// Blobs is the control plane's blob store (blobs.go). Nil keeps none.
	Blobs interface {
		blobs.Source
		blobs.Sink
		Size(h string) (int64, error)
	}
	// Record is the control plane's record: a runner's turn is sent to it
	// at the turn's end (record.go). Nil: none is kept.
	Record Recorder
	// Sweeper cleans Blobs; a closed session asks it for a sweep. Nil:
	// none.
	Sweeper *Sweeper
	// Relay is where control sends runners' model calls, by the name in
	// their path (relay.go), with the credential Credentials gives. Nil:
	// no relay.
	Relay       map[string]Upstream
	Credentials credentials.Source
}

// Server is the http.Handler.
type Server struct {
	cfg          Config
	token, enrol [32]byte
	tokenKey     [32]byte // signs runners' turn tokens (tokens.go)
	mux          *http.ServeMux
	reads        readScopes // what each runner may read (blobscope.go)
	fin          *finisher  // control's one ender of jobs, with a record (finisher.go)
	// Logf writes one line to the service log for each runner that
	// enrolls. Set before serving; nil: none.
	Logf func(format string, args ...any)
}

// New returns the jobs.
func New(cfg Config) (*Server, error) {
	if cfg.Queue == nil || cfg.Token == "" || cfg.EnrollToken == "" || cfg.Token == cfg.EnrollToken {
		return nil, errors.New("server: a queue and two different tokens are required")
	}
	for name, up := range cfg.Relay {
		if len(up.Gateways) == 0 {
			return nil, errors.New("server: the relay " + name + " has no gateway to record its calls")
		}
	}
	if cfg.Lease == 0 {
		cfg.Lease = Lease
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = Heartbeat
	}
	if cfg.MaxWait == 0 {
		cfg.MaxWait = MaxWait
	}
	if cfg.Record != nil {
		// The queue reads the changes a turn's review and asked result
		// describe from the record, where the finish moves them.
		rec := cfg.Record
		cfg.Queue.ReadChangesWith(func(j queue.Job) (*workspace.Changes, error) {
			c, err := Changes(rec, j)
			return WorkspaceChanges(c), err
		})
	}
	s := &Server{cfg: cfg, token: sha256.Sum256([]byte(cfg.Token)), enrol: sha256.Sum256([]byte(cfg.EnrollToken)), mux: http.NewServeMux(),
		tokenKey: sha256.Sum256([]byte("agentbus runner turn tokens\x00" + cfg.Token))}
	if cfg.Record != nil {
		s.fin = newFinisher(cfg.Queue, cfg.Record)
	}
	s.mux.HandleFunc("POST /api/v0/runner/register", s.enroll)
	s.mux.HandleFunc("POST /api/v0/runner/login", s.loginStart)
	s.mux.HandleFunc("GET /api/v0/runner/login", s.loginState)
	s.mux.HandleFunc("POST /api/v0/runner/removed", s.runner(s.removed))
	s.mux.HandleFunc("POST /api/v0/runner/heartbeat", s.runner(s.runnerBeat))
	s.mux.HandleFunc("GET /api/v0/runner/settings", s.runner(s.runnerSettings))
	s.mux.HandleFunc("POST /api/v0/runner/take", s.runner(s.take))
	s.mux.HandleFunc("POST /api/v0/runner/attempts/{id}/start", s.runner(s.start))
	s.mux.HandleFunc("POST /api/v0/runner/attempts/{id}/heartbeat", s.runner(s.heartbeat))
	s.mux.HandleFunc("POST /api/v0/runner/attempts/{id}/events", s.runner(s.events))
	s.mux.HandleFunc("POST /api/v0/runner/attempts/{id}/finish", s.runner(s.finish))
	s.mux.HandleFunc("POST /api/v0/runner/attempts/{id}/record", s.runner(s.record))
	s.mux.HandleFunc("POST /api/v0/runner/attempts/{id}/calls", s.runner(s.calls))
	s.mux.HandleFunc("POST /api/v0/runner/blobs", s.runner(s.blobsMissing))
	s.mux.HandleFunc("POST /api/v0/runner/packs", s.runner(s.blobPack))
	s.mux.HandleFunc("POST /api/v0/runner/packs/fetch", s.runner(s.blobFetch))
	s.mux.HandleFunc("PUT /api/v0/runner/blobs/{id}", s.runner(s.blobPut))
	s.mux.HandleFunc("GET /api/v0/runner/blobs/{id}", s.runner(s.blobGet))
	return s, nil
}

// CloseSession closes a session: it takes no more jobs, and its runner
// removes its copy and its home. A session with a job that has not ended is
// refused (queue.ErrSessionBusy).
func (s *Server) CloseSession(session string) (queue.Workspace, error) {
	ws, err := s.cfg.Queue.CloseSession(queue.DefaultTeam, session)
	if err == nil {
		s.cfg.Sweeper.Kick() // its blobs may go now
	}
	return ws, err
}

// ServeHTTP serves the runner protocol. The API in front of
// it (package api) has already checked the Host, answered CORS and bounded
// the body; this checks the tokens.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	limit := int64(MaxBody)
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, BlobPath) {
		limit = blobs.MaxBlob
	} else if r.URL.Path == PackPath {
		limit = blobs.MaxPackBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if _, pattern := s.mux.Handler(r); pattern == "" {
		fail(w, http.StatusNotFound, "no such API path")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// JobError is every error reply.
type JobError struct {
	Error string `json:"error"`
	// Stale says the attempt is no longer current: the runner must stop
	// its work and not retry.
	Stale bool `json:"stale,omitempty"`
	// Next, on a gap in an events batch, is the sequence number to send.
	Next int64 `json:"next,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) { writeJSON(w, status, JobError{Error: msg}) }

func bearer(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func same(tok string, want [32]byte) bool {
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(sum[:], want[:]) == 1
}

// runner admits a runner token, and hands the handler its runner.
func (s *Server) runner(h func(http.ResponseWriter, *http.Request, queue.Runner)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok || tok == "" {
			fail(w, http.StatusUnauthorized, "a runner token is required")
			return
		}
		rn, err := s.cfg.Queue.RunnerByToken(queue.HashToken(tok))
		if errors.Is(err, queue.ErrNotFound) {
			fail(w, http.StatusUnauthorized, "a runner token is required")
			return
		}
		if err != nil {
			s.internal(w)
			return
		}
		h(w, r, rn)
	}
}

func (s *Server) internal(w http.ResponseWriter) {
	fail(w, http.StatusInternalServerError, "the queue could not be read or written")
}

// decode reads one JSON object with known fields only.
func decode(r *http.Request, v any) bool {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return false
	}
	_, err := d.Token()
	return errors.Is(err, io.EOF)
}

// queueError answers a queue error from a runner's call.
func (s *Server) queueError(w http.ResponseWriter, err error) {
	var gap *queue.GapError
	switch {
	case errors.Is(err, queue.ErrNotFound):
		fail(w, http.StatusNotFound, "no such attempt for this runner")
	case errors.Is(err, queue.ErrStale):
		writeJSON(w, http.StatusConflict, JobError{Error: "this attempt is no longer current: stop its work", Stale: true})
	case errors.Is(err, queue.ErrFinishConflict):
		fail(w, http.StatusConflict, "this attempt already finished with another result")
	case errors.Is(err, queue.ErrBadState):
		fail(w, http.StatusBadRequest, "status must be finished, failed or cancelled")
	case errors.As(err, &gap):
		writeJSON(w, http.StatusConflict, JobError{Error: "the batch starts after the next expected sequence number", Next: gap.Next})
	default:
		s.internal(w)
	}
}

// RemovedReport is the body of POST /api/v0/runner/removed. Error says the
// workspace could not be removed.
type RemovedReport struct {
	Session string `json:"session"`
	Error   string `json:"error,omitempty"`
}

// RunnerBeat is the body of a runner's idle heartbeat. Labels and
// capabilities, when sent, replace what the runner had before.
type RunnerBeat struct {
	Capacity     int                 `json:"capacity"`
	Labels       []string            `json:"labels,omitempty"`
	Capabilities *queue.Capabilities `json:"capabilities,omitempty"`
}

func (s *Server) runnerBeat(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b RunnerBeat
	if !decode(r, &b) || b.Capacity < 1 || b.Capacity > MaxCapacity {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with capacity, 1 to 64, and optionally capabilities")
		return
	}
	if msg := checkLabels(b.Labels); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if b.Capabilities != nil {
		if msg := b.Capabilities.Check(); msg != "" {
			fail(w, http.StatusBadRequest, msg)
			return
		}
	}
	got, err := s.cfg.Queue.RunnerBeat(rn.ID, b.Capacity, b.Labels, b.Capabilities)
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// runnerSettings sends a runner the policy and the instructions, read fresh,
// for the turn it is about to start: every runner follows the server's.
func (s *Server) runnerSettings(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	snap := settings.RunnerSettings{Permissions: settings.DefaultPolicy(), Instructions: settings.DefaultInstructions}
	if st := s.cfg.Settings; st != nil {
		// One read, so the version names the policy and instructions sent.
		got, err := settings.Read(st)
		if err == nil {
			snap = settings.RunnerSettings{Permissions: got.Policy, PermissionsVersion: got.PolicyVersion,
				Instructions: got.Instructions, InstructionsVersion: got.InstructionsVersion, SettingsVersion: got.Revision}
		}
		var se *settings.Error
		if errors.As(err, &se) {
			fail(w, http.StatusServiceUnavailable, se.Error())
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "the settings could not be read")
			return
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) removed(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b RemovedReport
	if !decode(r, &b) || b.Session == "" || len(b.Error) > 1000 {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with session")
		return
	}
	if err := s.cfg.Queue.Removed(rn.ID, b.Session, b.Error); errors.Is(err, queue.ErrNotFound) {
		fail(w, http.StatusNotFound, "no closed session of this runner by that id")
		return
	} else if err != nil {
		s.internal(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// EnrollRequest is the body of POST /api/v0/runner/register.
type EnrollRequest struct {
	Name     string   `json:"name"`
	Labels   []string `json:"labels,omitempty"`
	Capacity int      `json:"capacity"`
	Protocol int      `json:"protocol"`
	// Capabilities: what the runner can do (queue.Capabilities). A runner
	// sends its harnesses later, on a heartbeat, once it has probed them.
	Capabilities *queue.Capabilities `json:"capabilities,omitempty"`
}

// Enrolled is the answer: the runner, and its token, shown this once.
type Enrolled struct {
	Runner    queue.Runner `json:"runner"`
	Token     string       `json:"token"`
	Heartbeat int64        `json:"heartbeat_ms"`
	Lease     int64        `json:"lease_ms"`
}

// runnerTokenPrefix marks a runner token.
const runnerTokenPrefix = "abr_"

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearer(r)
	if !ok || !same(tok, s.enrol) {
		fail(w, http.StatusUnauthorized, "the enrollment token is required")
		return
	}
	var b EnrollRequest
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with name, capacity, protocol and optionally labels")
		return
	}
	if msg := s.checkEnroll(b); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	rt := newRunnerToken()
	rn, err := s.cfg.Queue.RegisterRunner(queue.DefaultTeam, b.Name, b.Labels, b.Capacity, queue.HashToken(rt), b.Capabilities)
	if err != nil {
		s.internal(w)
		return
	}
	s.enrolled(rn)
	writeJSON(w, http.StatusCreated, Enrolled{rn, rt, s.cfg.Heartbeat.Milliseconds(), s.cfg.Lease.Milliseconds()})
}

// checkEnroll says what is wrong with an enroll or a sign-in's body, or "".
func (s *Server) checkEnroll(b EnrollRequest) string {
	if b.Protocol != ProtocolVersion {
		return protocolWhy
	}
	if b.Name == "" || len(b.Name) > 128 || b.Capacity < 1 || b.Capacity > MaxCapacity || len(b.Labels) > 64 {
		return "name must be 1 to 128 bytes, capacity 1 to 64, and at most 64 labels"
	}
	if msg := checkLabels(b.Labels); msg != "" {
		return msg
	}
	if b.Capabilities != nil {
		return b.Capabilities.Check()
	}
	return ""
}

// newRunnerToken is a new runner token: 256 random bits, four ids of 64
// bits each.
func newRunnerToken() string {
	return runnerTokenPrefix + queue.NewID("") + queue.NewID("") + queue.NewID("") + queue.NewID("")
}

// enrolled logs and records a runner added to control.
func (s *Server) enrolled(rn queue.Runner) {
	if s.Logf != nil {
		s.Logf("runner.enrolled id=%s name=%q", rn.ID, rn.Name)
	}
	if s.cfg.Record != nil {
		if _, err := s.cfg.Record.AppendOnce(event.Event{ID: queue.NewID("evt_"), Kind: event.RunnerEnrolled, Time: rn.Created, Runner: rn.ID}); err != nil && s.Logf != nil {
			s.Logf("store.error what=%s", event.RunnerEnrolled)
		}
	}
}

// TakeRequest is the body of POST /api/v0/runner/take.
type TakeRequest struct {
	// WaitMS is how long to wait for a job, up to 30000. Zero answers at
	// once.
	WaitMS int64 `json:"wait_ms"`
	// Protocol is the runner protocol the runner speaks: ProtocolVersion.
	Protocol int `json:"protocol"`
	// Folders asks for a folder job of the runner's own machine (an apply,
	// a typed folder's check), in one of its queue.FolderCapacity folder
	// places; without it, a turn or a review, in one of its
	// capacity's places.
	Folders bool `json:"folders,omitempty"`
}

// protocolWhy refuses a runner of another protocol version.
var protocolWhy = "this runner speaks another runner protocol than control (" + strconv.Itoa(ProtocolVersion) +
	"): run the same agentbus release on both"

// TakenWork is the answer to a take that found work: a job, or a closed
// session whose workspace the runner must remove and then report removed.
type TakenWork struct {
	Job        *queue.Job       `json:"job,omitempty"`
	Attempt    *queue.Attempt   `json:"attempt,omitempty"`
	LeaseUntil *time.Time       `json:"lease_until,omitempty"`
	Cleanup    *queue.Workspace `json:"cleanup,omitempty"`
}

func (s *Server) take(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b TakeRequest
	if !decode(r, &b) || b.WaitMS < 0 {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with wait_ms, 0 or more")
		return
	}
	if b.Protocol != ProtocolVersion {
		fail(w, http.StatusBadRequest, protocolWhy)
		return
	}
	wait := min(time.Duration(b.WaitMS)*time.Millisecond, s.cfg.MaxWait)
	notify, cancel := s.cfg.Queue.Notify()
	defer cancel()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second) // a lease that ran out frees a job with no signal
	defer tick.Stop()
	for {
		// A runner that hung up gets no claim. One that hangs up after the
		// claim loses it to the lease, which costs nothing: it is taken.
		if r.Context().Err() != nil {
			return
		}
		j, a, ok, cleanup, err := s.cfg.Queue.Take(rn.ID, s.cfg.Lease, b.Folders)
		if err != nil {
			s.internal(w)
			return
		}
		if cleanup != nil {
			writeJSON(w, http.StatusOK, TakenWork{Cleanup: cleanup})
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, TakenWork{Job: &j, Attempt: &a, LeaseUntil: &a.LeaseUntil})
			return
		}
		select {
		case <-notify:
		case <-tick.C:
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// AttemptCall is the body of start and heartbeat: the attempt's number.
type AttemptCall struct {
	Number int `json:"number"`
}

// AttemptStarted is the answer to start.
type AttemptStarted struct {
	Attempt queue.Attempt `json:"attempt"`
	// Cancel: the job was asked to stop. Finish it as cancelled.
	Cancel bool `json:"cancel"`
	// Token is the attempt's turn token, minted by control (tokens.go): the
	// agent's token for control's API, and the placeholder credential of its
	// model calls. It works while the attempt is current.
	Token string `json:"token"`
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b AttemptCall
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number")
		return
	}
	a, c, err := s.cfg.Queue.Start(rn.ID, r.PathValue("id"), b.Number, s.cfg.Lease)
	if err != nil {
		s.queueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, AttemptStarted{Attempt: a, Cancel: c, Token: s.mint(rn.ID, a.ID, a.Number)})
}

// AttemptBeat is the answer to a heartbeat.
type AttemptBeat struct {
	LeaseUntil time.Time `json:"lease_until"`
	Cancel     bool      `json:"cancel"`
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b AttemptCall
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number")
		return
	}
	until, c, err := s.cfg.Queue.Heartbeat(rn.ID, r.PathValue("id"), b.Number, s.cfg.Lease)
	if err != nil {
		s.queueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, AttemptBeat{until, c})
}

// Events is the body of an events batch: the attempt's number, the sequence
// number of the first event, and the events.
type EventBatch struct {
	Number int                  `json:"number"`
	Offset int64                `json:"offset"`
	Events []queue.AttemptEvent `json:"events"`
}

// EventBatchAck is the answer to an events batch.
type EventBatchAck struct {
	Next    int64 `json:"next"`
	Current bool  `json:"current"`
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b EventBatch
	if !decode(r, &b) || len(b.Events) > MaxBatch {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number, offset and at most 500 events")
		return
	}
	for _, e := range b.Events {
		if e.Kind == "" || len(e.Kind) > 64 {
			fail(w, http.StatusBadRequest, "every event needs a kind of 1 to 64 bytes")
			return
		}
	}
	next, cur, err := s.cfg.Queue.AddEvents(rn.ID, r.PathValue("id"), b.Number, b.Offset, b.Events)
	if err != nil {
		s.queueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, EventBatchAck{next, cur})
}

// FinishCall is the body of finish: the attempt's number and how it ended.
type FinishCall struct {
	Number    int              `json:"number"`
	Status    string           `json:"status"`
	Reason    string           `json:"reason,omitempty"`
	Verdict   *string          `json:"verdict,omitempty"`
	Result    json.RawMessage  `json:"result,omitempty"`
	LastSeq   int64            `json:"last_seq"`
	Session   string           `json:"session,omitempty"`
	Workspace *queue.Workspace `json:"workspace,omitempty"`
}

// Finishing is the body of finish for the attempt numbered number.
func Finishing(number int, f queue.Finish) FinishCall {
	return FinishCall{Number: number, Status: f.Status, Reason: f.Reason, Verdict: f.Verdict, Result: f.Result,
		LastSeq: f.LastSeq, Session: f.Session, Workspace: f.Workspace}
}

func (b FinishCall) finish() queue.Finish {
	return queue.Finish{Status: b.Status, Reason: b.Reason, Verdict: b.Verdict, Result: b.Result, LastSeq: b.LastSeq,
		Session: b.Session, Workspace: b.Workspace}
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b FinishCall
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number, status and optionally reason, verdict, result and last_seq")
		return
	}
	f := b.finish()
	if s.fin == nil {
		// No record: nothing to write first, so the finish ends the job.
		j, err := s.cfg.Queue.End(rn.ID, r.PathValue("id"), b.Number, f)
		if err != nil {
			s.queueError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, j)
		return
	}
	// The route only saves the finish (Accept) and waits: control's one
	// finisher writes its outcome to the record and then ends the job
	// (finisher.go). The same finish sent again changes nothing.
	cur, ended, err := s.cfg.Queue.Accept(rn.ID, r.PathValue("id"), b.Number, f)
	if err != nil {
		s.queueError(w, err)
		return
	}
	if ended {
		writeJSON(w, http.StatusOK, cur)
		return
	}
	j, err := s.fin.await(r.Context(), cur.Team, cur.ID, r.PathValue("id"), FinishWait)
	switch {
	case errors.Is(err, errOutcome):
		s.internal(w) // saved: the finisher tries again; so may the runner
		return
	case errors.Is(err, errFinishing):
		fail(w, http.StatusServiceUnavailable, "the finish is saved and is being ended; send it again")
		return
	case err != nil:
		s.queueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// checkLabels says what is wrong with a runner's labels, or "".
func checkLabels(labels []string) string {
	if len(labels) > 64 {
		return "at most 64 labels"
	}
	for _, l := range labels {
		if !queue.LabelOK(l) {
			return "each label is 1 to 64 of A-Z, a-z, 0-9 and . _ : -, starting with a letter or digit"
		}
	}
	return ""
}
