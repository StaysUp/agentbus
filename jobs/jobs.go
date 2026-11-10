// Package jobs is the HTTP API of the job queue: the jobs API a client
// calls, and the runner protocol a runner calls.
//
// Three tokens:
//   - the main token (Config.Token): the jobs API.
//   - the enrollment token (Config.EnrollToken): POST /api/v0/runners only.
//   - a runner token, made when a runner enrolls and returned once: take for
//     its own runner id, and the attempt calls for attempts it holds.
//
// Tokens are compared by their SHA-256. A runner token is kept only as its
// hash. Error replies use fixed wording and never quote the request.
package jobs

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/staysup/agentbus/agents"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
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

// ProtocolVersion is the runner protocol this server speaks.
const ProtocolVersion = 1

// Config is what the server needs.
type Config struct {
	Queue       *queue.Queue
	Token       string // the main token: the jobs API
	EnrollToken string // adds a runner
	// Lease and Heartbeat override the defaults, for tests. Zero keeps them.
	Lease, Heartbeat, MaxWait time.Duration
	// Agents are the saved agents a job's spec.agent names. Nil takes
	// spec.agent as a harness name, as before saved agents.
	Agents *agents.Store
	// Settings are the policy and the instructions a runner reads for each
	// turn (GET /api/v0/runners/{id}/settings). Nil sends the defaults.
	Settings settings.Store
	// Folders is this machine's connector, in agentbus up: a job's folder
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
}

// New returns the jobs.
func New(cfg Config) (*Server, error) {
	if cfg.Queue == nil || cfg.Token == "" || cfg.EnrollToken == "" || cfg.Token == cfg.EnrollToken {
		return nil, errors.New("server: a queue and two different tokens are required")
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
	s := &Server{cfg: cfg, token: sha256.Sum256([]byte(cfg.Token)), enrol: sha256.Sum256([]byte(cfg.EnrollToken)), mux: http.NewServeMux(),
		tokenKey: sha256.Sum256([]byte("agentbus runner turn tokens\x00" + cfg.Token))}
	main := s.main
	s.mux.HandleFunc("POST /api/v0/jobs", main(s.createJob))
	s.mux.HandleFunc("GET /api/v0/jobs", main(s.listJobs))
	s.mux.HandleFunc("GET /api/v0/jobs/{id}", main(s.getJob))
	s.mux.HandleFunc("POST /api/v0/jobs/{id}/cancel", main(s.cancelJob))
	s.mux.HandleFunc("POST /api/v0/jobs/{id}/review", main(s.reviewJob))
	s.mux.HandleFunc("GET /api/v0/attempts/{id}/events", main(s.attemptEvents))
	s.mux.HandleFunc("GET /api/v0/workspaces", main(s.listWorkspaces))
	s.mux.HandleFunc("POST /api/v0/workspaces/{id}/close", main(s.closeWorkspace))
	s.mux.HandleFunc("POST /api/v0/runners/{id}/removed", s.runner(s.removed))
	s.mux.HandleFunc("POST /api/v0/runners/{id}/heartbeat", s.runner(s.runnerBeat))
	s.mux.HandleFunc("POST /api/v0/runners", s.enroll)
	s.mux.HandleFunc("GET /api/v0/runners/{id}/settings", s.runner(s.runnerSettings))
	s.mux.HandleFunc("POST /api/v0/runners/{id}/take", s.runner(s.take))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/start", s.runner(s.start))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/heartbeat", s.runner(s.heartbeat))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/events", s.runner(s.events))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/finish", s.runner(s.finish))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/snapshot", s.runner(s.snapshot))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/record", s.runner(s.record))
	s.mux.HandleFunc("POST /api/v0/attempts/{id}/calls", s.runner(s.calls))
	s.mux.HandleFunc("POST /api/v0/blobs", s.runner(s.blobsMissing))
	s.mux.HandleFunc("POST /api/v0/packs", s.runner(s.blobPack))
	s.mux.HandleFunc("POST /api/v0/packs/fetch", s.runner(s.blobFetch))
	s.mux.HandleFunc("PUT /api/v0/blobs/{id}", s.runner(s.blobPut))
	s.mux.HandleFunc("GET /api/v0/blobs/{id}", s.runner(s.blobGet))
	return s, nil
}

// ServeHTTP serves the jobs API and the runner protocol. The API in front of
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

// admitKey marks a request package api admitted for a turn token.
type admitKey struct{}

// admission is what Admit carries.
type admission struct {
	links queue.Links
	lim   queue.Limits
}

// Admit marks a request that package api has admitted for a turn token, on a
// route and job its checks allow, with the links work it asks for gets (the
// asking turn and its request tree) and the caps it is held to. Only code in
// this binary can set it.
func Admit(ctx context.Context, l queue.Links, lim queue.Limits) context.Context {
	return context.WithValue(ctx, admitKey{}, admission{l, lim})
}

// admitted is the links and caps of a request Admit marked.
func admitted(r *http.Request) (queue.Links, queue.Limits, bool) {
	a, ok := r.Context().Value(admitKey{}).(admission)
	return a.links, a.lim, ok
}

// main admits the main token, and a request package api admitted for a turn
// token (Admit).
func (s *Server) main(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := admitted(r); ok {
			h(w, r)
			return
		}
		tok, ok := bearer(r)
		if !ok || !same(tok, s.token) {
			fail(w, http.StatusUnauthorized, "the main token is required")
			return
		}
		h(w, r)
	}
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

// JobRequest is the body of POST /api/v0/jobs.
type JobRequest struct {
	Kind           string      `json:"kind"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	Spec           queue.Spec  `json:"spec"`
	Links          queue.Links `json:"links"`
}

func validKey(k string) bool {
	if len(k) < 1 || len(k) > 128 {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var b JobRequest
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with kind, spec, and optionally idempotency_key and links")
		return
	}
	if b.Kind != queue.KindTurn && b.Kind != queue.KindReview {
		fail(w, http.StatusBadRequest, "kind must be turn or review")
		return
	}
	if b.IdempotencyKey != "" && !validKey(b.IdempotencyKey) {
		fail(w, http.StatusBadRequest, "idempotency_key must be 1 to 128 of A-Z, a-z, 0-9 and . _ : -")
		return
	}
	if !run.PromptOK(b.Spec.Prompt) {
		fail(w, http.StatusBadRequest, "spec.prompt must be from 1 byte to 256 KiB, with no NUL byte")
		return
	}
	if b.Spec.AgentID != "" || b.Spec.Access != "" || b.Spec.KeyEnv != "" {
		fail(w, http.StatusBadRequest, "spec.agent_id, spec.access and spec.key_env come from the saved agent; name it in spec.agent")
		return
	}
	if msg := checkSpec(b.Spec); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if msg := queue.ResolveFolders(&b.Spec, s.cfg.Folders); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if s.cfg.Agents != nil && (b.Spec.Session == "" || b.Spec.Harness != "") {
		// A retry of a request whose agent was removed since gets the first
		// job (Spec.asked).
		ag, why, err := agents.Lookup(b.Spec.Harness, s.cfg.Agents.Resolve, s.retried(b.IdempotencyKey))
		if err != nil {
			s.internal(w)
			return
		}
		if why != nil {
			fail(w, http.StatusBadRequest, "spec.agent "+strconv.Quote(b.Spec.Harness)+" is not a saved agent; GET /api/v0/agents lists them")
			return
		}
		b.Spec.Harness, b.Spec.AgentID, b.Spec.Access, b.Spec.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		switch {
		case b.Spec.Model != "":
		case b.Spec.Session != "":
			// A session's next turn: the runner uses the agent's model only
			// if the turn switches agent (run.Request.AgentModel).
			b.Spec.AgentModel = ag.Model
		case ag.Model != "":
			b.Spec.Model, b.Spec.ModelFromAgent = ag.Model, true
		}
	}
	if b.Kind == queue.KindReview {
		// A review never writes. The framework enforces it below the prompt.
		b.Spec.ReadOnly = true
	}
	j, created, err := s.cfg.Queue.Create(queue.DefaultTeam, b.Kind, b.IdempotencyKey, b.Spec, b.Links)
	switch {
	case errors.Is(err, queue.ErrKeyConflict):
		fail(w, http.StatusConflict, "idempotency_key already names a job with other fields")
		return
	case errors.Is(err, queue.ErrSessionClosed):
		fail(w, http.StatusConflict, "the session is closed")
		return
	case errors.Is(err, queue.ErrFull):
		fail(w, http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end")
		return
	case err != nil:
		s.internal(w)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, j)
}

// baseShape is a branch, tag or commit: no leading dash, so it is never read
// as a flag, and no spaces.
func baseShape(b string) bool {
	if b == "" || len(b) > 200 || b[0] == '-' || strings.Contains(b, "..") {
		return false
	}
	for _, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-@^~", c)) {
			return false
		}
	}
	return true
}

// checkSpec says what is wrong with a job's spec, or "". A session's first
// job names agent and a workspace: folder, or repo and base. A later job
// names session, and may name agent: its turn runs on that agent.
func checkSpec(sp queue.Spec) string {
	if len(sp.Labels) > 16 {
		return "spec.labels holds at most 16 labels"
	}
	for _, l := range sp.Labels {
		if !queue.LabelOK(l) {
			return "each of spec.labels is 1 to 64 of A-Z, a-z, 0-9 and . _ : -, starting with a letter or digit"
		}
	}
	folder, repo := sp.Folder != "" || sp.FolderRef != "", sp.Repo != "" || sp.RepoRef != ""
	if sp.Session != "" {
		if folder || repo || sp.Connector != "" || sp.Base != "" || sp.Isolate {
			return "spec with session takes no folder, isolate, repo or base: they are the session's"
		}
		return ""
	}
	if sp.Connector != "" && sp.FolderRef == "" && sp.RepoRef == "" {
		return "spec.connector goes with spec.folder_ref or spec.repo_ref"
	}
	if sp.Isolate && !folder {
		return "spec.isolate goes with spec.folder"
	}
	if sp.Harness == "" {
		return "spec needs agent with folder, or with repo and base; or session"
	}
	switch {
	case folder && !repo && sp.Base == "":
		if sp.Folder != "" && !filepath.IsAbs(sp.Folder) {
			return "spec.folder must be an absolute path"
		}
	case !folder && repo:
		if sp.Repo != "" && (!filepath.IsAbs(sp.Repo) || len(sp.Repo) > 4096) {
			return "spec.repo must be an absolute path"
		}
		if !baseShape(sp.Base) {
			return "spec.base must be a branch, tag or commit: 1 to 200 of A-Z, a-z, 0-9 and . _ / - @ ^ ~, not starting with -"
		}
	default:
		return "spec needs folder, or repo and base, not both"
	}
	return ""
}

// WorkspaceList is the answer to GET /api/v0/workspaces: each session's workspace and runner.
type WorkspaceList struct {
	Sessions []queue.Workspace `json:"sessions"`
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Queue.Sessions(queue.DefaultTeam, 1000)
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, WorkspaceList{list})
}

func (s *Server) closeWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.cfg.Queue.CloseSession(queue.DefaultTeam, r.PathValue("id"))
	switch {
	case errors.Is(err, queue.ErrNotFound):
		fail(w, http.StatusNotFound, "no such session")
	case errors.Is(err, queue.ErrSessionBusy):
		fail(w, http.StatusConflict, "the session has a job that has not ended; wait for it or cancel it")
	case err != nil:
		s.internal(w)
	default:
		s.cfg.Sweeper.Kick() // its blobs may go now
		writeJSON(w, http.StatusOK, ws)
	}
}

// RemovedReport is the body of POST /api/v0/runners/{id}/removed. Error says the
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
	if r.PathValue("id") != rn.ID {
		fail(w, http.StatusNotFound, "no such runner for this token")
		return
	}
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
	if r.PathValue("id") != rn.ID {
		fail(w, http.StatusNotFound, "no such runner for this token")
		return
	}
	snap := settings.RunnerSettings{Permissions: settings.DefaultPolicy(), Instructions: settings.DefaultInstructions}
	if st := s.cfg.Settings; st != nil {
		var err error
		if snap.Permissions, snap.PermissionsVersion, err = st.Policy(); err == nil {
			snap.Instructions, snap.InstructionsVersion, err = st.Instructions()
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
	if r.PathValue("id") != rn.ID {
		fail(w, http.StatusNotFound, "no such runner for this token")
		return
	}
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

// JobList is the answer to GET /api/v0/jobs.
type JobList struct {
	Jobs []queue.Job `json:"jobs"`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := queue.Filter{State: q.Get("state"), Kind: q.Get("kind"), Session: q.Get("session"), Reviews: q.Get("reviews")}
	switch f.State {
	case "", queue.Queued, queue.Taken, queue.Running, queue.Finished, queue.Failed, queue.Cancelled:
	default:
		fail(w, http.StatusBadRequest, "state must be queued, taken, running, finished, failed or cancelled")
		return
	}
	switch f.Kind {
	case "", queue.KindTurn, queue.KindReview, queue.KindApply:
	default:
		fail(w, http.StatusBadRequest, "kind must be turn, review or apply")
		return
	}
	if len(f.Session) > 128 || len(f.Reviews) > 128 {
		fail(w, http.StatusBadRequest, "session and reviews are at most 128 bytes")
		return
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			fail(w, http.StatusBadRequest, "limit must be from 1 to 1000")
			return
		}
		limit = n
	}
	jobs, err := s.cfg.Queue.List(queue.DefaultTeam, f, limit)
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, JobList{jobs})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.cfg.Queue.Get(queue.DefaultTeam, r.PathValue("id"))
	if errors.Is(err, queue.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such job")
		return
	}
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.cfg.Queue.Cancel(queue.DefaultTeam, r.PathValue("id"))
	if errors.Is(err, queue.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such job")
		return
	}
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

// ReviewRequest is the body of POST /api/v0/jobs/{id}/review.
type ReviewRequest struct {
	// Agent and Model: the reviewer, any agent the framework can run.
	Agent          string `json:"agent"`
	Model          string `json:"model,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// IncludeReply puts the author's reply in the prompt. Off by default:
	// a reviewer handed the author's conclusion tends to confirm it.
	IncludeReply bool `json:"include_reply,omitempty"`
}

func (s *Server) reviewJob(w http.ResponseWriter, r *http.Request) {
	var b ReviewRequest
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with agent, and optionally model, idempotency_key and include_reply")
		return
	}
	if !agentShape.MatchString(b.Agent) || (b.Model != "" && !run.ModelShape.MatchString(b.Model)) {
		fail(w, http.StatusBadRequest, "agent must match "+agentShape.String()+" and model "+run.ModelShape.String())
		return
	}
	// The reviewer is a saved agent, as for a turn; without saved agents its
	// name is the harness.
	as := queue.Spec{Harness: b.Agent, Model: b.Model}
	if s.cfg.Agents != nil {
		// A retry of a review whose reviewer was removed since gets the
		// first review job, as a turn's retry does.
		ag, why, err := agents.Lookup(b.Agent, s.cfg.Agents.Resolve, s.retried(b.IdempotencyKey))
		if err != nil {
			s.internal(w)
			return
		}
		if why != nil {
			fail(w, http.StatusBadRequest, "agent "+strconv.Quote(b.Agent)+" is not a saved agent; GET /api/v0/agents lists them")
			return
		}
		as.Harness, as.AgentID, as.Access, as.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
		if as.Model == "" && ag.Model != "" {
			as.Model, as.ModelFromAgent = ag.Model, true
		}
	}
	if b.IdempotencyKey != "" && !validKey(b.IdempotencyKey) {
		fail(w, http.StatusBadRequest, "idempotency_key must be 1 to 128 of A-Z, a-z, 0-9 and . _ : -")
		return
	}
	links, lim, _ := admitted(r) // a review a turn asked for names that turn, within its caps
	j, created, err := s.cfg.Queue.ReviewLinked(queue.DefaultTeam, r.PathValue("id"), as, b.IdempotencyKey, b.IncludeReply, links, lim)
	var limit *queue.LimitError
	switch {
	case errors.As(err, &limit):
		fail(w, http.StatusForbidden, limit.Msg)
	case errors.Is(err, queue.ErrNotFound):
		fail(w, http.StatusNotFound, "no such job")
	case errors.Is(err, queue.ErrNotReviewable):
		fail(w, http.StatusConflict, "this job cannot be reviewed: it must be a turn that has ended and recorded its changed files in a known workspace")
	case errors.Is(err, queue.ErrSessionClosed):
		fail(w, http.StatusConflict, "the job's session is closed, so its workspace is gone")
	case errors.Is(err, queue.ErrKeyConflict):
		fail(w, http.StatusConflict, "idempotency_key already names a job with other fields")
	case errors.Is(err, queue.ErrFull):
		fail(w, http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end")
	case err != nil:
		s.internal(w)
	case created:
		writeJSON(w, http.StatusCreated, j)
	default:
		writeJSON(w, http.StatusOK, j)
	}
}

// agentShape is a reviewer: a saved agent's id or a harness name.
var agentShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// AttemptEventList is the answer to GET /api/v0/attempts/{id}/events.
type AttemptEventList struct {
	Events []queue.AttemptEvent `json:"events"`
}

func (s *Server) attemptEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.cfg.Queue.Events(queue.DefaultTeam, r.PathValue("id"))
	if errors.Is(err, queue.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such attempt")
		return
	}
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, AttemptEventList{evs})
}

// EnrollRequest is the body of POST /api/v0/runners.
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
	if b.Protocol != ProtocolVersion {
		fail(w, http.StatusBadRequest, "protocol must be 1")
		return
	}
	if b.Name == "" || len(b.Name) > 128 || b.Capacity < 1 || b.Capacity > MaxCapacity || len(b.Labels) > 64 {
		fail(w, http.StatusBadRequest, "name must be 1 to 128 bytes, capacity 1 to 64, and at most 64 labels")
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
	// 256 random bits: four ids of 64 bits each.
	rt := runnerTokenPrefix + queue.NewID("") + queue.NewID("") + queue.NewID("") + queue.NewID("")
	rn, err := s.cfg.Queue.RegisterRunner(queue.DefaultTeam, b.Name, b.Labels, b.Capacity, queue.HashToken(rt), b.Capabilities)
	if err != nil {
		s.internal(w)
		return
	}
	writeJSON(w, http.StatusCreated, Enrolled{rn, rt, s.cfg.Heartbeat.Milliseconds(), s.cfg.Lease.Milliseconds()})
}

// TakeRequest is the body of POST /api/v0/runners/{id}/take.
type TakeRequest struct {
	// WaitMS is how long to wait for a job, up to 30000. Zero answers at
	// once.
	WaitMS int64 `json:"wait_ms"`
}

// TakenWork is the answer to a take that found work: a job, or a closed
// session whose workspace the runner must remove and then report removed.
type TakenWork struct {
	Job        *queue.Job       `json:"job,omitempty"`
	Attempt    *queue.Attempt   `json:"attempt,omitempty"`
	LeaseUntil *time.Time       `json:"lease_until,omitempty"`
	Cleanup    *queue.Workspace `json:"cleanup,omitempty"`
}

func (s *Server) take(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if r.PathValue("id") != rn.ID {
		fail(w, http.StatusNotFound, "no such runner for this token")
		return
	}
	var b TakeRequest
	if !decode(r, &b) || b.WaitMS < 0 {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with wait_ms, 0 or more")
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
		j, a, ok, cleanup, err := s.cfg.Queue.Take(rn.ID, s.cfg.Lease)
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
	j, err := s.cfg.Queue.End(rn.ID, r.PathValue("id"), b.Number, b.finish())
	if err != nil {
		s.queueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// retried says whether a request's idempotency key was used before, for
// agents.Lookup; nil for a request without one.
func (s *Server) retried(key string) func() (bool, error) {
	if key == "" {
		return nil
	}
	return func() (bool, error) { return s.cfg.Queue.HasKey(queue.DefaultTeam, key) }
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
