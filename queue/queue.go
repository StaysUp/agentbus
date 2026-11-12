// Package queue keeps jobs, their attempts, runners and attempt events in one
// SQLite file.
//
// A job is a requested turn. An attempt is one execution of it, and the
// attempt is the turn: its id is the turn's id. A retry adds an attempt and
// never overwrites an earlier one. A turn job's session is named when the job
// is made, so its first turn opens that session. Every call a runner makes after
// take names its attempt id and number, and a call for an attempt that is not
// the job's current one is refused (fencing). Lease expiry is decided on this
// process's clock.
//
// The lost-runner rule: a lease that expires while the attempt is taken puts
// the job back in the queue, and the next take makes a new attempt. A lease
// that expires while it is running fails the job with reason runner_lost. It
// is never retried by itself.
package queue

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/workspace"
	_ "modernc.org/sqlite"
)

// Job states.
const (
	Queued    = "queued"
	Taken     = "taken"
	Running   = "running"
	Finished  = "finished"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Turn states: what the API says of a turn, whose job is in the queue.
const (
	TurnQueued   = "queued"
	TurnRunning  = "running"
	TurnFinished = "finished"
	TurnFailed   = "failed"
	TurnStopped  = "stopped"
)

// TurnState is the state of a turn whose job is in state: queued until a
// runner starts it, stopped when it was cancelled.
func TurnState(state string) string {
	switch state {
	case Queued, Taken:
		return TurnQueued
	case Cancelled:
		return TurnStopped
	}
	return state
}

// Attempt states, besides Taken, Running, Finished, Failed and Cancelled.
const (
	// Lost: the attempt's lease expired. Its job went back to the queue
	// (while taken) or failed with runner_lost (while running).
	Lost = "lost"
)

// Job kinds.
const (
	KindTurn   = "turn"
	KindReview = "review"
	// KindApply puts a session's changes, up to one turn, into the person's
	// folder, on the runner of the folder's machine (apply.go). It starts no
	// turn.
	KindApply = "apply"
	// KindSnapshot takes a snapshot of a folder, on the runner of its
	// machine, for a runner that builds a copy from it. Since 0.5 a session
	// runs on its folder's machine, so the runner of this build asks for
	// none; one stored before still runs. It starts no turn.
	KindSnapshot = "snapshot"
	// KindFolder checks a folder a person typed, on the runner of the machine
	// named, and registers it there: its result is the folder's reference
	// (folder.go). It starts no turn.
	KindFolder = "folder"
	// A "command" job (the person's "!" in 0.4) is retired: a "!" is now a
	// message to the session's agent. One stored before is read as it is,
	// and a runner refuses to run it.
)

// Failure reasons.
const (
	ReasonSetup      = "setup"       // the runner could not start the work
	ReasonAgent      = "agent"       // the agent's turn ended in failure
	ReasonRunnerLost = "runner_lost" // the runner's lease expired while running
	ReasonTimeout    = "timeout"
	ReasonConflict   = "conflict" // an apply refused: files changed in the person's folder
)

// DefaultTeam is the one team every job and runner belongs to for now. The id
// is on every row from the start, so teams do not need a migration later.
const DefaultTeam = "team_default"

// MaxQueued bounds a team's jobs that have not ended.
const MaxQueued = 1000

const schema = `
CREATE TABLE IF NOT EXISTS teams (
	id      TEXT PRIMARY KEY,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS runners (
	id         TEXT PRIMARY KEY,
	team       TEXT NOT NULL REFERENCES teams(id),
	name       TEXT NOT NULL,
	labels     TEXT NOT NULL,
	capacity   INTEGER NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created    INTEGER NOT NULL,
	seen       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT PRIMARY KEY,
	team         TEXT NOT NULL REFERENCES teams(id),
	kind         TEXT NOT NULL,
	state        TEXT NOT NULL,
	idem_key     TEXT,
	request_hash TEXT NOT NULL,
	spec         TEXT NOT NULL,
	links        TEXT NOT NULL,
	attempt      TEXT,
	turn         TEXT,
	session      TEXT,
	cancel       INTEGER NOT NULL DEFAULT 0,
	reason       TEXT NOT NULL DEFAULT '',
	verdict      TEXT,
	findings     TEXT,
	result       TEXT,
	created      INTEGER NOT NULL,
	updated      INTEGER NOT NULL,
	ended        INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_key ON jobs(team, idem_key) WHERE idem_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(team, state, created);
CREATE INDEX IF NOT EXISTS jobs_session ON jobs(session, state);
CREATE INDEX IF NOT EXISTS jobs_reviews ON jobs(json_extract(links, '$.reviews_job')) WHERE kind = 'review';
-- A session's workspace lives on one runner: the session's later jobs go to
-- it. Written by the finish of the session's first job.
CREATE TABLE IF NOT EXISTS sessions (
	id      TEXT PRIMARY KEY,
	team    TEXT NOT NULL REFERENCES teams(id),
	runner  TEXT NOT NULL REFERENCES runners(id),
	kind    TEXT NOT NULL,
	folder  TEXT NOT NULL,
	repo    TEXT NOT NULL DEFAULT '',
	base    TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	closed  INTEGER,
	removed INTEGER,
	removal_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_runner ON sessions(runner, closed, removed);
CREATE TABLE IF NOT EXISTS attempts (
	id          TEXT PRIMARY KEY,
	job         TEXT NOT NULL REFERENCES jobs(id),
	number      INTEGER NOT NULL,
	runner      TEXT NOT NULL REFERENCES runners(id),
	state       TEXT NOT NULL,
	lease_until INTEGER NOT NULL,
	taken       INTEGER NOT NULL,
	started     INTEGER,
	ended       INTEGER,
	reason      TEXT NOT NULL DEFAULT '',
	finish_hash TEXT,
	result      TEXT,
	last_seq    INTEGER NOT NULL DEFAULT 0,
	expected_seq INTEGER,
	UNIQUE(job, number)
);
CREATE INDEX IF NOT EXISTS attempts_runner ON attempts(runner, state);
CREATE INDEX IF NOT EXISTS attempts_lease ON attempts(state, lease_until);
-- One row per asked job whose result was delivered (asked.go): the turn job
-- that carries it, or why none could be made.
CREATE TABLE IF NOT EXISTS deliveries (
	job      TEXT PRIMARY KEY REFERENCES jobs(id),
	delivery TEXT NOT NULL DEFAULT '',
	error    TEXT NOT NULL DEFAULT '',
	created  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS attempt_events (
	attempt TEXT NOT NULL REFERENCES attempts(id),
	seq     INTEGER NOT NULL,
	time    INTEGER NOT NULL,
	kind    TEXT NOT NULL,
	data    TEXT NOT NULL,
	PRIMARY KEY (attempt, seq)
);
`

// Errors the protocol answers with.
var (
	// ErrNotFound: no such job, attempt or runner for this caller. An
	// attempt of another runner, or a job of another team, is not found.
	ErrNotFound = errors.New("queue: not found")
	// ErrStale: the attempt is no longer the job's current one, its lease
	// expired, or its number is wrong. The runner must stop its work.
	ErrStale = errors.New("queue: the attempt is not current")
	// ErrKeyConflict: an idempotency key already names a job with other
	// fields.
	ErrKeyConflict = errors.New("queue: the idempotency key names another job")
	// ErrFinishConflict: the attempt already finished with another result.
	ErrFinishConflict = errors.New("queue: the attempt already finished with another result")
	// ErrFull: the team has MaxQueued jobs that have not ended.
	ErrFull = errors.New("queue: too many jobs waiting")
	// ErrBadState: the request does not fit the attempt's state, such as
	// finishing with a status that is not an end.
	ErrBadState = errors.New("queue: not allowed in this state")
	// ErrSessionClosed: the session was closed; it takes no more jobs.
	ErrSessionClosed = errors.New("queue: the session is closed")
	// ErrNotReviewable: the job is not a turn that ended with its changed
	// files recorded in a known workspace.
	ErrNotReviewable = errors.New("queue: the job cannot be reviewed")
	// ErrSessionBusy: the session has a job that has not ended, so it cannot
	// be closed.
	ErrSessionBusy = errors.New("queue: the session has a job that has not ended")
)

// GapError: an events batch starts after the next expected sequence number.
type GapError struct{ Next int64 }

func (e *GapError) Error() string { return fmt.Sprintf("queue: events must start at %d", e.Next) }

// Spec is what a turn job asks for. It is frozen when the job is made. A
// session's first job names its workspace: Repo and Base (a git worktree the
// runner makes), or Folder (used as it is, or with Isolate an isolated copy
// of it, as the isolation setting says: package isolation). A later job names Session, and may name an
// agent: its turn then runs on that agent, in the session's workspace.
//
// Harness is the harness, the program that runs the turn, such as claude; its
// JSON name is agent, as the runner protocol has always named it. AgentID, Access
// and KeyEnv come from the saved agent the job was made for; the API fills
// them, never the caller.
type Spec struct {
	Harness string `json:"agent,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Access  string `json:"access,omitempty"`
	KeyEnv  string `json:"key_env,omitempty"`
	// Folder and Repo are paths, as a request names them. The control
	// plane never keeps them: the folder's connector gives it a reference
	// (FolderRef, RepoRef, and Connector, the connector that owns it), and
	// only the reference is stored. A job stored before references may
	// still hold a path; the runner that takes it registers it.
	Folder    string `json:"folder,omitempty"`
	FolderRef string `json:"folder_ref,omitempty"`
	Isolate   bool   `json:"isolate,omitempty"`
	Repo      string `json:"repo,omitempty"`
	RepoRef   string `json:"repo_ref,omitempty"`
	Connector string `json:"connector,omitempty"`
	Base      string `json:"base,omitempty"`
	// Labels: a runner takes the job only if it has every one.
	Labels   []string `json:"labels,omitempty"`
	Session  string   `json:"session,omitempty"`
	Prompt   string   `json:"prompt"`
	Model    string   `json:"model,omitempty"`
	ReadOnly bool     `json:"read_only,omitempty"`
	// ModelFromAgent: Model came from the saved agent, not the request. It
	// is never stored; it only keeps the request's hash what was asked.
	ModelFromAgent bool `json:"-"`
	// AgentModel: a session's next turn that names a saved agent and no
	// model gets that agent's model here, filled by the control plane, for
	// the runner to use only if the turn switches agent: a turn that names
	// the session's own latest agent keeps the session's model.
	AgentModel string `json:"agent_model,omitempty"`
	// ApplyFrom and ApplyTo, for an apply job: the changesets
	// (workspace.Changeset) it goes from and to, by their blobs in the
	// control plane's store. ApplyFrom empty is the session's base: no turn
	// of it was applied before.
	ApplyFrom string `json:"apply_from,omitempty"`
	ApplyTo   string `json:"apply_to,omitempty"`
	// Typed and TypedConnector: a new session's folder as the request typed
	// it, and the connector it named. They are never stored; they only keep
	// the request's hash what was asked, so a retry with the same key gets
	// its turn without its folder being checked again.
	Typed          string `json:"-"`
	TypedConnector string `json:"-"`
}

// asked is spec as the request named it, for the idempotency hash: the
// saved agent by its id, without what the control plane filled in from it.
// A retry after the agent was edited then still matches, and gets the job
// the first request made, with what was frozen then.
func (spec Spec) asked() Spec {
	a := spec
	if a.AgentID != "" {
		a.Harness = a.AgentID
	}
	a.AgentID, a.Access, a.KeyEnv, a.AgentModel = "", "", "", ""
	if a.Typed != "" {
		a.Folder, a.FolderRef, a.Connector = a.Typed, "", a.TypedConnector
	}
	if a.ModelFromAgent {
		a.Model = ""
	}
	return a
}

// Links ties a job to other work.
type Links struct {
	// ContinuesTurn: the framework turn this job's turn continues.
	ContinuesTurn string `json:"continues_turn,omitempty"`
	// ReviewsJob: for a review job, the job it reviews.
	ReviewsJob string `json:"reviews_job,omitempty"`
	// RedoOf: the job this one does again.
	RedoOf string `json:"redo_of,omitempty"`
	// Asker: the turn whose agent asked for this job with its turn token
	// (asked.go). Its result is delivered to that turn's session.
	Asker *Asker `json:"asker,omitempty"`
	// Chain: the request tree this job is in, for asked work and the turns
	// that deliver its results.
	Chain *Chain `json:"chain,omitempty"`
	// ResultOf: this turn job delivers that job's result. AgentBus started
	// it, not the person.
	ResultOf *ResultOf `json:"result_of,omitempty"`
	// AppliesTurn and AppliesJob: for an apply job, the turn whose changes
	// it applies, and that turn's job.
	AppliesTurn string `json:"applies_turn,omitempty"`
	AppliesJob  string `json:"applies_job,omitempty"`
	// AppliesTurns: for an apply job, every turn whose changes it carries,
	// oldest first: the session's turns after its last apply, up to
	// AppliesTurn. ApplyBy: who asked for it.
	AppliesTurns []string `json:"applies_turns,omitempty"`
	ApplyBy      *By      `json:"apply_by,omitempty"`
	// ReviewSession: for a review job, the session its turn opens, named
	// by control when the job is made. The runner opens it, and a runner
	// may send that turn's record for this session only.
	ReviewSession string `json:"review_session,omitempty"`
}

// Job is one requested turn.
type Job struct {
	ID             string `json:"id"`
	Team           string `json:"team"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Spec           Spec   `json:"spec"`
	Links          Links  `json:"links"`
	Attempt        string `json:"attempt,omitempty"`
	// Turn, for a turn or review job: the id of the turn it runs, named when
	// the job is made, so the turn is known while it waits. The runner
	// starts the turn with this id, whichever attempt runs it.
	Turn            string `json:"turn,omitempty"`
	CancelRequested bool   `json:"cancel_requested,omitempty"`
	Reason          string `json:"reason,omitempty"`
	// Verdict, for a review: pass, fail or unknown, read by the control
	// plane from the reviewer's reply. Null until the review has finished.
	Verdict *string `json:"verdict"`
	// Findings, for a review with a verdict: what the reviewer named.
	Findings []Finding       `json:"findings,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	// Session: the framework session the job's turn ran in, once known; for
	// a review, the session whose workspace it reads.
	Session  string     `json:"session,omitempty"`
	Created  time.Time  `json:"created"`
	Updated  time.Time  `json:"updated"`
	Ended    *time.Time `json:"ended,omitempty"`
	Attempts []Attempt  `json:"attempts,omitempty"`
	// Workspace, for a job of a known session: where its runner keeps it.
	Workspace *Workspace `json:"workspace,omitempty"`
}

// Workspace kinds.
const (
	WorkspaceWorktree = "worktree" // a git worktree the runner made, and removes on close
	WorkspaceCopy     = "copy"     // a copy of the job's folder the runner made, and removes on close
	WorkspaceFolder   = "folder"   // a folder the job named, never removed
)

// Workspace is where a session's turns run, on its runner.
type Workspace struct {
	Session string `json:"session"`
	Runner  string `json:"runner"`
	Kind    string `json:"kind"`
	// Space: a copy or worktree, by its id on its runner. FolderRef: a
	// folder used as it is, by its reference. Folder: a path on the
	// runner, kept only by a session made before these ids. The control
	// plane reads no file through any of them.
	Space     string `json:"space,omitempty"`
	FolderRef string `json:"folder_ref,omitempty"`
	Folder    string `json:"folder,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Base      string `json:"base,omitempty"`
	// Snapshot: the snapshot id of the session's copy as it was made, the
	// root hash of its first checkpoint's tree. Empty for a folder used as
	// it is, and for a session made before snapshot ids.
	Snapshot string     `json:"snapshot,omitempty"`
	Created  time.Time  `json:"created"`
	Closed   *time.Time `json:"closed,omitempty"`
	Removed  *time.Time `json:"removed,omitempty"`
	// RemovalError: the runner could not remove the worktree. It is left on
	// disk, and the session counts as removed so it is not tried forever.
	RemovalError string `json:"removal_error,omitempty"`
	// DiffFrom: the checkpoint the session's latest turn whose changes
	// control holds (its changeset) ended at, as a job's workspace names
	// it. A later turn's diff starts there, so whatever the copy holds
	// since, from a turn whose runner was lost or whose finish was refused,
	// shows in that diff, as an apply would write it.
	DiffFrom string `json:"diff_from,omitempty"`
}

// Attempt is one execution of a job.
type Attempt struct {
	ID         string          `json:"id"`
	Job        string          `json:"job"`
	Number     int             `json:"number"`
	Runner     string          `json:"runner"`
	State      string          `json:"state"`
	LeaseUntil time.Time       `json:"lease_until"`
	Taken      time.Time       `json:"taken"`
	Started    *time.Time      `json:"started,omitempty"`
	Ended      *time.Time      `json:"ended,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	// LastSeq is the highest event sequence number stored.
	LastSeq int64 `json:"last_seq"`
	// ExpectedSeq is the last sequence number the runner said it sent, in
	// its finish. When it is above LastSeq, events are missing.
	ExpectedSeq *int64 `json:"expected_seq,omitempty"`
	// TurnStarted: the runner reported the attempt's turn started. The
	// attempt id is then a turn in the record. Read from its events, never
	// kept.
	TurnStarted bool `json:"turn_started,omitempty"`
}

// Runner is one enrolled runner.
type Runner struct {
	ID       string    `json:"id"`
	Team     string    `json:"team"`
	Name     string    `json:"name"`
	Labels   []string  `json:"labels"`
	Capacity int       `json:"capacity"`
	Created  time.Time `json:"created"`
	Seen     time.Time `json:"seen"`
	// Caps: what the runner reported it can do; nil before its first report.
	Caps *Capabilities `json:"capabilities,omitempty"`
}

// AttemptEvent is one runner event of an attempt.
type AttemptEvent struct {
	Seq  int64           `json:"seq"`
	Time time.Time       `json:"time"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Finish is how a runner ends an attempt.
type Finish struct {
	Status  string          `json:"status"` // Finished, Failed or Cancelled
	Reason  string          `json:"reason,omitempty"`
	Verdict *string         `json:"verdict,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	// LastSeq is the last event sequence the runner sent, so the record
	// shows whether it is complete.
	LastSeq int64 `json:"last_seq"`
	// Session and Workspace, from a session's first job: the framework
	// session the turn opened, and where its workspace is. They tie the
	// session's later jobs to this runner.
	Session   string     `json:"session,omitempty"`
	Workspace *Workspace `json:"workspace,omitempty"`
	// Kept, when set, is the result the job keeps in place of Result: the
	// same result with its outcomes already in control's record and only
	// named here (the Result.*Event pointers). It is not part of the
	// finish's hash, so a finish sent again still matches the first.
	Kept json.RawMessage `json:"-"`
}

// CheckFinish says whether f can end job j, before anything is written:
// End refuses the same finishes, with the same errors.
func CheckFinish(j Job, f Finish) error {
	if f.Status != Finished && f.Status != Failed && f.Status != Cancelled {
		return ErrBadState
	}
	if j.Kind == KindTurn && f.Session != "" && f.Session == j.Session && f.Workspace != nil && j.Spec.Session == "" {
		w := f.Workspace
		if w.Kind != WorkspaceWorktree && w.Kind != WorkspaceCopy && w.Kind != WorkspaceFolder {
			return ErrBadState
		}
		if w.Snapshot != "" && !SnapshotShape.MatchString(w.Snapshot) {
			return ErrBadState
		}
		if w.Space != "" && !SpaceShape.MatchString(w.Space) || w.FolderRef != "" && !FolderRefShape.MatchString(w.FolderRef) {
			return ErrBadState
		}
	}
	return nil
}

// Queue is one jobs file. It is safe for concurrent use.
type Queue struct {
	db  *sql.DB
	now func() time.Time

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
	// applyMu makes Apply's read of a session's applies and its new job
	// one step. One process keeps a queue (the record's lock says so).
	applyMu sync.Mutex
	// typed holds the path of each folder job that waits, by job id: in
	// memory only, never in the file (folder.go).
	typedMu sync.Mutex
	typed   map[string]string
	// changes reads a job's changes from the record (ReadChangesWith).
	changes ChangesReader
}

// By says who asked for a job, by the kind of token it came with (the
// record's event.By). Never the token.
type By struct {
	Kind    string `json:"kind"` // person or agent
	Session string `json:"session,omitempty"`
	Turn    string `json:"turn,omitempty"`
}

// ChangesReader reads what a turn job changed, from where it is kept.
type ChangesReader func(j Job) (*workspace.Changes, error)

// ReadChangesWith sets how the queue reads a job's changes kept in the
// record (Result.ChangesEvent), for the prompts of a review and of asked
// work's result. Set it before the queue is used.
func (q *Queue) ReadChangesWith(r ChangesReader) { q.changes = r }

// changesOf is what job j changed: from its result for a job from before
// 0.5, else from the record. nil when it holds none.
func (q *Queue) changesOf(j Job) (*workspace.Changes, error) {
	r, err := ReadResult(j.Result)
	if err != nil {
		return nil, err
	}
	if r.ChangesEvent == "" || q.changes == nil {
		return r.Changes, nil
	}
	return q.changes(j)
}

// Open opens or creates the queue at path. now is the clock; nil is time.Now.
func Open(path string, now func() time.Time) (*Queue, error) {
	if now == nil {
		now = time.Now
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("queue: open: %w", err)
	}
	// One connection: every transaction is serialised, so a claim and its
	// capacity check can never interleave with another.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + loginsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("queue: create tables: %w", err)
	}
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO teams(id, created) VALUES(?, ?) ON CONFLICT DO NOTHING`,
		DefaultTeam, now().UnixNano()); err != nil {
		db.Close()
		return nil, fmt.Errorf("queue: make the default team: %w", err)
	}
	return &Queue{db: db, now: now, subs: map[chan struct{}]struct{}{}}, nil
}

// Close closes the file.
func (q *Queue) Close() error { return q.db.Close() }

// Notify returns a channel that is signalled when a job may have become
// takeable. Call cancel when done.
func (q *Queue) Notify() (ch <-chan struct{}, cancel func()) {
	c := make(chan struct{}, 1)
	q.mu.Lock()
	q.subs[c] = struct{}{}
	q.mu.Unlock()
	return c, func() {
		q.mu.Lock()
		delete(q.subs, c)
		q.mu.Unlock()
	}
}

func (q *Queue) wake() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for c := range q.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// NewID is a prefix and 16 random hex characters.
func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// HashToken is how a token is kept: its SHA-256, in hex.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func hashOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nanos(t time.Time) int64 { return t.UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func optTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}

// committed is an error whose transaction still commits: the call is
// refused, but what it settled on the way (an expired lease) is kept.
type committed struct{ err error }

func (c committed) Error() string { return c.err.Error() }
func (c committed) Unwrap() error { return c.err }

// tx runs f in one transaction. An error rolls it back, except committed,
// which commits and then returns the error it wraps.
func (q *Queue) tx(f func(*sql.Tx) error) error {
	tx, err := q.db.Begin()
	if err != nil {
		return fmt.Errorf("queue: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ferr := f(tx)
	var keep committed
	if ferr != nil && !errors.As(ferr, &keep) {
		return ferr
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queue: commit: %w", err)
	}
	if ferr != nil {
		return keep.err
	}
	return nil
}

// RegisterRunner enrolls a runner in team, with the hash of its token and
// what it can do (nil: not reported yet).
func (q *Queue) RegisterRunner(team, name string, labels []string, capacity int, tokenHash string, caps *Capabilities) (Runner, error) {
	if labels == nil {
		labels = []string{}
	}
	now := q.now().UTC()
	r := Runner{ID: NewID("run_"), Team: team, Name: name, Labels: labels, Capacity: capacity, Created: now, Seen: now, Caps: caps}
	lb, _ := json.Marshal(labels)
	var cb any
	if caps != nil {
		b, _ := json.Marshal(caps)
		cb = string(b)
	}
	_, err := q.db.Exec(`INSERT INTO runners(id, team, name, labels, capacity, token_hash, created, seen, caps) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, team, name, string(lb), capacity, tokenHash, nanos(now), nanos(now), cb)
	if err != nil {
		return r, fmt.Errorf("queue: register runner: %w", err)
	}
	return r, nil
}

// RunnerByToken finds the runner a token hash belongs to.
func (q *Queue) RunnerByToken(tokenHash string) (Runner, error) {
	// A removed runner's token works no more (RemoveRunner).
	return q.runner(`token_hash = ? AND removed IS NULL`, tokenHash)
}

// GetRunner reads one runner of team.
func (q *Queue) GetRunner(team, id string) (Runner, error) {
	r, err := q.runner(`id = ?`, id)
	if err == nil && r.Team != team {
		return Runner{}, ErrNotFound
	}
	return r, err
}

func (q *Queue) runner(where string, arg string) (Runner, error) {
	r, err := scanRunner(q.db.QueryRow(`SELECT `+runnerCols+` FROM runners WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("queue: read runner: %w", err)
	}
	return r, nil
}

const runnerCols = `id, team, name, labels, capacity, created, seen, caps`

func scanRunner(s scanner) (Runner, error) {
	var r Runner
	var labels string
	var caps sql.NullString
	var created, seen int64
	if err := s.Scan(&r.ID, &r.Team, &r.Name, &labels, &r.Capacity, &created, &seen, &caps); err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(labels), &r.Labels)
	if caps.Valid {
		var c Capabilities
		if json.Unmarshal([]byte(caps.String), &c) == nil {
			r.Caps = &c
		}
	}
	r.Created, r.Seen = fromNanos(created), fromNanos(seen)
	return r, nil
}

// Create makes a queued job. With a key, a second call with the same key and
// the same fields returns the first job and created false; other fields under
// the same key get ErrKeyConflict.
func (q *Queue) Create(team, kind, key string, spec Spec, links Links) (job Job, created bool, err error) {
	return q.create(team, kind, key, spec, links, Limits{}, spec.Session)
}

// HasKey reports whether a job was already made with this idempotency key.
// An empty key is never known.
func (q *Queue) HasKey(team, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	var n int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND idem_key = ?`, team, key).Scan(&n); err != nil {
		return false, fmt.Errorf("queue: read key: %w", err)
	}
	return n > 0, nil
}

// reviewIn says review job o runs its turn in session j.session: a review
// runs in a session of its own, which its runner names in the attempt's
// turn.created event. A turn job sent to that session waits behind it.
const reviewIn = `o.kind = 'review' AND EXISTS (SELECT 1 FROM attempts a JOIN attempt_events e ON e.attempt = a.id
	WHERE a.job = o.id AND e.kind = 'turn.created' AND json_extract(e.data, '$.session') = j.session)`

// laneTake is Take's one rule of a session's lane, for a job j and another
// job o: j waits for o. A session's turns are one lane, first in, first out:
// each waits for one that runs, and for an older one still queued. A turn job
// sent to a review's own session waits behind the review (reviewIn).
// Snapshot, apply and folder jobs use no copy and never wait.
const laneTake = `(o.state IN ('taken','running') AND (
		(j.kind = 'turn' AND o.session = j.session AND o.kind = 'turn')
		OR (j.kind = 'turn' AND (` + reviewIn + `))))
	OR (o.state = 'queued' AND j.kind = 'turn' AND o.session = j.session AND o.kind = 'turn'
		AND (o.created < j.created OR (o.created = j.created AND o.id < j.id)))`

// SessionHasWork says the session has a job that has not ended: a turn job
// of it, or a review whose turn runs in it.
func (q *Queue) SessionHasWork(team, session string) (bool, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs o, (SELECT ? AS session) j WHERE o.team = ? AND o.ended IS NULL
		AND ((o.kind = 'turn' AND o.session = j.session) OR (`+reviewIn+`))`, session, team).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("queue: read session work: %w", err)
	}
	return n > 0, nil
}

// TurnsAhead are the jobs the turn job waits behind, oldest
// first: its session's turn jobs that have not ended and were
// made before it, and a review whose turn runs in its session. None for a
// job of no session.
func (q *Queue) TurnsAhead(team, id string) ([]Job, error) {
	rows, err := q.db.Query(`SELECT o.id FROM jobs o JOIN jobs j ON j.id = ? AND j.team = ?
		WHERE o.team = j.team AND o.ended IS NULL AND o.id != j.id AND j.session IS NOT NULL
		AND ((o.kind = 'turn' AND o.session = j.session AND (o.created < j.created OR (o.created = j.created AND o.id < j.id)))
			OR (`+reviewIn+`))
		ORDER BY o.created, o.id`, id, team)
	if err != nil {
		return nil, fmt.Errorf("queue: list turns ahead: %w", err)
	}
	var ids []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, fmt.Errorf("queue: list turns ahead: %w", err)
		}
		ids = append(ids, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list turns ahead: %w", err)
	}
	jobs := make([]Job, 0, len(ids))
	for _, s := range ids {
		j, err := q.Get(team, s)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// create is Create with the session the job is tied to: spec.Session for a
// turn, the reviewed job's session for a review. A turn job that opens a new
// session gets its id here. A session whose turn runs is never refused: Take
// starts its turn jobs one at a time, in the order they were made.
func (q *Queue) create(team, kind, key string, spec Spec, links Links, lim Limits, session string) (job Job, created bool, err error) {
	hash := hashOf(struct {
		Kind  string `json:"kind"`
		Spec  Spec   `json:"spec"`
		Links Links  `json:"links"`
	}{kind, spec.asked(), hashLinks(links)})
	err = q.tx(func(tx *sql.Tx) error {
		if key != "" {
			var id, h string
			err := tx.QueryRow(`SELECT id, request_hash FROM jobs WHERE team = ? AND idem_key = ?`, team, key).Scan(&id, &h)
			if err == nil {
				if h != hash {
					return ErrKeyConflict
				}
				job.ID = id
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("queue: read job: %w", err)
			}
		}
		if session != "" {
			var closed sql.NullInt64
			err := tx.QueryRow(`SELECT closed FROM sessions WHERE id = ? AND team = ?`, session, team).Scan(&closed)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("queue: read session: %w", err)
			}
			if closed.Valid {
				return ErrSessionClosed
			}
			if errors.Is(err, sql.ErrNoRows) && kind == KindTurn {
				var ended bool
				if err := tx.QueryRow(`SELECT `+sessionEnded("?1"), session).Scan(&ended); err != nil {
					return fmt.Errorf("queue: read session: %w", err)
				}
				if ended {
					return ErrSessionEnded
				}
			}
		}
		if err := checkAsked(tx, team, links, lim); err != nil {
			return err
		}
		if kind == KindTurn && session == "" {
			session = NewID("") // the new session its first turn opens
		}
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND state IN ('queued','taken','running')`, team).Scan(&open); err != nil {
			return fmt.Errorf("queue: count jobs: %w", err)
		}
		if open >= MaxQueued {
			return ErrFull
		}
		now := nanos(q.now())
		job.ID = NewID("job_")
		var turn any
		if kind == KindTurn || kind == KindReview {
			turn = NewID("t_")
		}
		sb, _ := json.Marshal(spec)
		lb, _ := json.Marshal(links)
		var k, sess any
		if key != "" {
			k = key
		}
		if session != "" {
			sess = session
		}
		if _, err := tx.Exec(`INSERT INTO jobs(id, team, kind, state, idem_key, request_hash, spec, links, turn, session, created, updated)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, job.ID, team, kind, Queued, k, hash, string(sb), string(lb), turn, sess, now, now); err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		return Job{}, false, err
	}
	if created {
		q.wake()
	}
	job, err = q.Get(team, job.ID)
	return job, created, err
}

const jobCols = `id, team, kind, state, COALESCE(idem_key, ''), spec, links, COALESCE(attempt, ''), COALESCE(turn, ''), cancel, reason, verdict, findings, result, COALESCE(session, ''), created, updated, ended`

type scanner interface{ Scan(...any) error }

func scanJob(s scanner) (Job, error) {
	var j Job
	var spec, links string
	var verdict, findings, result sql.NullString
	var created, updated int64
	var ended sql.NullInt64
	if err := s.Scan(&j.ID, &j.Team, &j.Kind, &j.State, &j.IdempotencyKey, &spec, &links, &j.Attempt, &j.Turn, &j.CancelRequested,
		&j.Reason, &verdict, &findings, &result, &j.Session, &created, &updated, &ended); err != nil {
		return j, err
	}
	if findings.Valid {
		_ = json.Unmarshal([]byte(findings.String), &j.Findings)
	}
	_ = json.Unmarshal([]byte(spec), &j.Spec)
	_ = json.Unmarshal([]byte(links), &j.Links)
	if verdict.Valid {
		v := verdict.String
		j.Verdict = &v
	}
	if result.Valid {
		j.Result = json.RawMessage(result.String)
	}
	j.Created, j.Updated, j.Ended = fromNanos(created), fromNanos(updated), optTime(ended)
	return j, nil
}

// attemptCols reads an attempt, and whether its runner reported its turn
// started (the turn.created event).
const attemptCols = `id, job, number, runner, state, lease_until, taken, started, ended, reason, result, last_seq, expected_seq,
	EXISTS (SELECT 1 FROM attempt_events e WHERE e.attempt = attempts.id AND e.kind = 'turn.created')`

func scanAttempt(s scanner) (Attempt, error) {
	var a Attempt
	var lease, taken int64
	var started, ended sql.NullInt64
	var result sql.NullString
	var expected sql.NullInt64
	if err := s.Scan(&a.ID, &a.Job, &a.Number, &a.Runner, &a.State, &lease, &taken, &started, &ended, &a.Reason, &result, &a.LastSeq, &expected,
		&a.TurnStarted); err != nil {
		return a, err
	}
	if expected.Valid {
		a.ExpectedSeq = &expected.Int64
	}
	a.LeaseUntil, a.Taken, a.Started, a.Ended = fromNanos(lease), fromNanos(taken), optTime(started), optTime(ended)
	if result.Valid {
		a.Result = json.RawMessage(result.String)
	}
	return a, nil
}

// Get reads one job of team, with its attempts, oldest first.
func (q *Queue) Get(team, id string) (Job, error) {
	j, err := scanJob(q.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ? AND team = ?`, id, team))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, fmt.Errorf("queue: read job: %w", err)
	}
	if j.Workspace, err = workspaceOf(q.db, j); err != nil {
		return j, err
	}
	rows, err := q.db.Query(`SELECT `+attemptCols+` FROM attempts WHERE job = ? ORDER BY number`, id)
	if err != nil {
		return j, fmt.Errorf("queue: read attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return j, fmt.Errorf("queue: read attempts: %w", err)
		}
		j.Attempts = append(j.Attempts, a)
	}
	return j, rows.Err()
}

// Filter picks jobs for List. Empty fields pick all.
type Filter struct {
	State   string
	Kind    string
	Session string // jobs of this framework session, reviews of it included
	Reviews string // reviews of this job
	Open    bool   // only jobs that have not ended
}

// List reads team's jobs, newest first, with their attempts.
func (q *Queue) List(team string, f Filter, limit int) ([]Job, error) {
	where, args := `team = ?`, []any{team}
	if f.State != "" {
		where += ` AND state = ?`
		args = append(args, f.State)
	}
	if f.Kind != "" {
		where += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Session != "" {
		where += ` AND session = ?`
		args = append(args, f.Session)
	}
	if f.Reviews != "" {
		where += ` AND kind = 'review' AND json_extract(links, '$.reviews_job') = ?`
		args = append(args, f.Reviews)
	}
	if f.Open {
		where += ` AND ended IS NULL`
	}
	args = append(args, limit)
	rows, err := q.db.Query(`SELECT id FROM jobs WHERE `+where+` ORDER BY created DESC, id LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: list jobs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("queue: list jobs: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(ids))
	for _, id := range ids {
		j, err := q.Get(team, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

const workspaceCols = `id, runner, kind, folder, repo, base, snapshot, created, closed, removed, removal_error`

func scanWorkspace(s scanner) (Workspace, error) {
	var w Workspace
	var created int64
	var closed, removed sql.NullInt64
	var where string
	err := s.Scan(&w.Session, &w.Runner, &w.Kind, &where, &w.Repo, &w.Base, &w.Snapshot, &created, &closed, &removed, &w.RemovalError)
	w.Created, w.Closed, w.Removed = fromNanos(created), optTime(closed), optTime(removed)
	w.setWhere(where)
	return w, err
}

// where is what the sessions table keeps in its folder column: the space
// id, the folder's reference, or, for a session made before them, the path.
func (w Workspace) where() string {
	switch {
	case w.Space != "":
		return w.Space
	case w.FolderRef != "":
		return w.FolderRef
	}
	return w.Folder
}

func (w *Workspace) setWhere(v string) {
	switch {
	case strings.HasPrefix(v, "/") || strings.Contains(v, `\`):
		w.Folder = v
	case FolderRefShape.MatchString(v):
		w.FolderRef = v
	default:
		w.Space = v
	}
}

// workspaceOf is the workspace of the job's session: the one it continues,
// the one its finish recorded, or for a review the reviewed job's. Nil when
// the queue knows none.
func workspaceOf(db querier, j Job) (*Workspace, error) {
	session := j.Session
	if session == "" {
		return nil, nil
	}
	w, err := scanWorkspace(db.QueryRow(`SELECT `+workspaceCols+` FROM sessions WHERE id = ? AND team = ?`, session, j.Team))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: read session: %w", err)
	}
	// As appliableTurns orders them: the last to end.
	// The turn's end checkpoint: Result.Checkpoint, or in a result from
	// before 0.5 its changes' to.
	err = db.QueryRow(`SELECT COALESCE(json_extract(result, '$.checkpoint'), json_extract(result, '$.changes.to'), '') FROM jobs WHERE team = ? AND session = ? AND kind = 'turn'
		AND ended IS NOT NULL AND attempt IS NOT NULL AND COALESCE(json_extract(result, '$.changeset'), '') != ''
		ORDER BY ended DESC, rowid DESC LIMIT 1`, j.Team, session).Scan(&w.DiffFrom)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("queue: read session: %w", err)
	}
	return &w, nil
}

// Take hands the runner one thing to do. First, a closed session of this
// runner whose workspace it must remove (cleanup), once no job of it is left
// running. Else the oldest queued job of the runner's team it may run, if it
// has a free slot, as a new attempt with a lease: a job of a session the queue
// knows goes only to that session's runner, and a session's turn
// jobs run one at a time, so a next turn waits for the one before it. Reviews
// never wait:
// each runs in a session of its own. The capacity check and the claim
// are one transaction. ok is false when there is no job; cleanup may still be
// set.
//
// folders asks for a folder job (folderKinds: an apply, a typed folder's
// check) of the runner's own machine, in one of its FolderCapacity folder
// places; without it the take is for a turn or a review, in one
// of its capacity's places. So an apply never waits behind running turns.
func (q *Queue) Take(runnerID string, lease time.Duration, folders bool) (job Job, att Attempt, ok bool, cleanup *Workspace, err error) {
	err = q.tx(func(tx *sql.Tx) error {
		now := q.now()
		if _, err := sweepTx(tx, now); err != nil {
			return err
		}
		var team string
		var capacity int
		// A runner removed since its token was checked takes nothing.
		err := tx.QueryRow(`SELECT team, capacity FROM runners WHERE id = ? AND removed IS NULL`, runnerID).Scan(&team, &capacity)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read runner: %w", err)
		}
		if _, err := tx.Exec(`UPDATE runners SET seen = ? WHERE id = ?`, nanos(now), runnerID); err != nil {
			return fmt.Errorf("queue: write runner: %w", err)
		}
		w, err := scanWorkspace(tx.QueryRow(`SELECT `+workspaceCols+` FROM sessions s WHERE runner = ? AND closed IS NOT NULL AND removed IS NULL
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.session = s.id AND j.state IN ('queued','taken','running'))
			ORDER BY closed LIMIT 1`, runnerID))
		if err == nil {
			cleanup = &w
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("queue: read sessions: %w", err)
		}
		// Folder jobs and the rest count against their own places.
		kinds, places := `j.kind NOT IN `+folderKinds, capacity
		if folders {
			kinds, places = `j.kind IN `+folderKinds, FolderCapacity
		}
		var busy int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attempts a JOIN jobs j ON j.id = a.job WHERE a.runner = ? AND a.state IN ('taken','running') AND `+kinds,
			runnerID).Scan(&busy); err != nil {
			return fmt.Errorf("queue: count attempts: %w", err)
		}
		if busy >= places {
			return nil
		}
		// A job goes only to a runner that can run it (caps.go): the job's
		// machine, the session's runner, every label it asks for, and its
		// harness. A runner stored as a connector of a build before 0.5
		// takes nothing.
		var id string
		err = tx.QueryRow(`SELECT j.id FROM jobs j, runners r WHERE r.id = ? AND j.team = ? AND j.state = 'queued'
			AND `+kinds+` AND COALESCE(json_extract(r.caps, '$.role'), '') = ''
			AND (j.kind IN `+folderKinds+` OR j.session IS NULL OR NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = j.session AND s.team = j.team)
				OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = j.session AND s.team = j.team AND s.runner = r.id AND s.closed IS NULL))
			AND (j.session IS NULL OR NOT EXISTS (SELECT 1 FROM jobs o WHERE o.team = j.team AND o.id != j.id AND (`+laneTake+`)))
			AND (`+machineTake+`) AND (`+labelsTake+`) AND (`+harnessTake+`)
			ORDER BY j.created, j.id LIMIT 1`, runnerID, team, nanos(now.Add(-RunnerFresh))).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("queue: read queue: %w", err)
		}
		var n int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM attempts WHERE job = ?`, id).Scan(&n); err != nil {
			return fmt.Errorf("queue: number attempt: %w", err)
		}
		// The attempt is the turn: the same id, in the turn id's shape.
		aid := NewID("t_")
		if _, err := tx.Exec(`INSERT INTO attempts(id, job, number, runner, state, lease_until, taken) VALUES(?,?,?,?,?,?,?)`,
			aid, id, n, runnerID, Taken, nanos(now.Add(lease)), nanos(now)); err != nil {
			return fmt.Errorf("queue: write attempt: %w", err)
		}
		if _, err := tx.Exec(`UPDATE jobs SET state = ?, attempt = ?, updated = ? WHERE id = ?`, Taken, aid, nanos(now), id); err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
		if job, err = scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)); err != nil {
			return fmt.Errorf("queue: read job: %w", err)
		}
		if job.Workspace, err = workspaceOf(tx, job); err != nil {
			return err
		}
		if att, err = scanAttempt(tx.QueryRow(`SELECT `+attemptCols+` FROM attempts WHERE id = ?`, aid)); err != nil {
			return fmt.Errorf("queue: read attempt: %w", err)
		}
		ok = true
		return nil
	})
	if ok && job.Kind == KindFolder {
		job.Spec.Folder = q.typedPath(job.ID)
	}
	return job, att, ok, cleanup, err
}

// current loads the attempt and its job for a runner's call, and checks the
// fence: the runner holds the attempt, its number matches, it is the job's
// current attempt, it is taken or running, and its lease has not expired.
// An expired lease is settled here, as Sweep would, before ErrStale.
func current(tx *sql.Tx, now time.Time, runnerID, attemptID string, number int) (Attempt, Job, error) {
	a, err := scanAttempt(tx.QueryRow(`SELECT `+attemptCols+` FROM attempts WHERE id = ? AND runner = ?`, attemptID, runnerID))
	if errors.Is(err, sql.ErrNoRows) {
		return a, Job{}, ErrNotFound
	}
	if err != nil {
		return a, Job{}, fmt.Errorf("queue: read attempt: %w", err)
	}
	j, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, a.Job))
	if err != nil {
		return a, j, fmt.Errorf("queue: read job: %w", err)
	}
	if a.Number != number || j.Attempt != a.ID || (a.State != Taken && a.State != Running) {
		return a, j, ErrStale
	}
	if !now.Before(a.LeaseUntil) && !accepted(tx, a.ID) {
		if err := expire(tx, now, a, j); err != nil {
			return a, j, err
		}
		return a, j, committed{ErrStale}
	}
	return a, j, nil
}

// accepted says attempt id holds a finish control accepted and has not
// ended yet (Accept): a finish hash and no end. Its lease no longer
// counts, since the runner's part is done and control's is to end it
// (Accepted).
func accepted(tx *sql.Tx, id string) bool {
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM attempts WHERE id = ? AND finish_hash IS NOT NULL AND ended IS NULL`, id).Scan(&n)
	return n > 0
}

// expire settles one attempt whose lease ran out: taken goes back to the
// queue (or to cancelled, when a cancel was asked for), running fails with
// runner_lost (or is cancelled).
func expire(tx *sql.Tx, now time.Time, a Attempt, j Job) error {
	if _, err := tx.Exec(`UPDATE attempts SET state = ?, ended = ?, reason = 'lease_expired' WHERE id = ?`, Lost, nanos(now), a.ID); err != nil {
		return fmt.Errorf("queue: write attempt: %w", err)
	}
	var err error
	switch {
	case j.CancelRequested:
		_, err = tx.Exec(`UPDATE jobs SET state = ?, attempt = NULL, reason = ?, updated = ?, ended = ? WHERE id = ?`,
			Cancelled, reasonIf(a.State == Running, ReasonRunnerLost), nanos(now), nanos(now), j.ID)
	case a.State == Taken:
		// Nothing was spent: the next take makes a new attempt.
		_, err = tx.Exec(`UPDATE jobs SET state = ?, attempt = NULL, updated = ? WHERE id = ?`, Queued, nanos(now), j.ID)
	default:
		// The agent may have done work: never retried by itself.
		_, err = tx.Exec(`UPDATE jobs SET state = ?, attempt = NULL, reason = ?, updated = ?, ended = ? WHERE id = ?`,
			Failed, ReasonRunnerLost, nanos(now), nanos(now), j.ID)
	}
	if err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	// An apply that ended here has no finish, so nothing wrote its
	// turn.applied: mark it due (lost.go).
	if j.Kind == KindApply && (j.CancelRequested || a.State != Taken) {
		if err := markOutcomeDue(tx, j.ID); err != nil {
			return err
		}
	}
	return nil
}

func reasonIf(ok bool, r string) string {
	if ok {
		return r
	}
	return ""
}

// sweepTx settles every attempt whose lease ran out, ends the queued
// turns of every ended session (sessionEnded), and ends every folder job
// that waited too long (endFoldersTx). It answers how many attempts it
// settled. A saved finish (Accept) is not one: its lease no longer counts,
// and the finisher ends it.
func sweepTx(tx *sql.Tx, now time.Time) (int, error) {
	rows, err := tx.Query(`SELECT `+attemptCols+` FROM attempts WHERE state IN ('taken','running') AND lease_until <= ?
		AND finish_hash IS NULL`, nanos(now))
	if err != nil {
		return 0, fmt.Errorf("queue: read leases: %w", err)
	}
	var expired []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("queue: read leases: %w", err)
		}
		expired = append(expired, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, a := range expired {
		j, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, a.Job))
		if err != nil {
			return 0, fmt.Errorf("queue: read job: %w", err)
		}
		if err := expire(tx, now, a, j); err != nil {
			return 0, err
		}
		if err := endSnapshotsTx(tx, now, a.ID); err != nil {
			return 0, err
		}
	}
	if err := endFoldersTx(tx, now); err != nil {
		return 0, err
	}
	// Whatever ended a session's first turn, its queued turns end here,
	// before any runner can take one.
	return len(expired), endSessionsTx(tx, now)
}

// endSnapshotsTx stops the snapshot job the attempt asked for (Snapshot),
// once the attempt has ended: no one is left to build a copy from it. A
// queued one is cancelled; a taken or running one is asked to stop.
func endSnapshotsTx(tx *sql.Tx, now time.Time, attempt string) error {
	key := SnapshotKey(attempt)
	if _, err := tx.Exec(`UPDATE jobs SET state = ?, cancel = 1, updated = ?, ended = ? WHERE idem_key = ? AND kind = 'snapshot' AND state = 'queued'`,
		Cancelled, nanos(now), nanos(now), key); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	if _, err := tx.Exec(`UPDATE jobs SET cancel = 1, updated = ? WHERE idem_key = ? AND kind = 'snapshot' AND state IN ('taken','running')`,
		nanos(now), key); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	return nil
}

// A session is ended when its first job, the turn that opened it, has
// ended and the queue holds no workspace for it. (A session the queue
// first saw as a next turn, known only to the record, is not opened here.) A workspace is bound only by the first turn's
// accepted finish; every other way that turn ends (cancelled, lease lost,
// finish refused, refused at setup) leaves nothing to continue in. Every
// queued and later turn of an ended session, and every result for it, gets
// SessionEnded; nothing else waits on it. This is the one place the rule is
// decided: sessionEnded below, read by create and by sweepTx.

// ErrSessionEnded: a turn for an ended session.
var ErrSessionEnded = errors.New("queue: the session ended with its first turn")

// SessionEnded is why a turn of an ended session is refused or ended.
const SessionEnded = "this session's first turn ended before the session was set up (it was stopped, its runner stopped, or it did not start), so the session is ended; start a new session"

// sessionEnded is the rule, as SQL over a session id named by sess (a
// column or a "?" bound twice, once per use).
func sessionEnded(sess string) string {
	return `(NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = ` + sess + `)
		AND EXISTS (SELECT 1 FROM jobs f WHERE f.session = ` + sess + ` AND f.kind = 'turn' AND f.ended IS NOT NULL
			AND COALESCE(json_extract(f.spec, '$.session'), '') = ''
			AND f.rowid = (SELECT MIN(g.rowid) FROM jobs g WHERE g.session = f.session)))`
}

// endSessionsTx ends every queued turn of an ended session, with why.
func endSessionsTx(tx *sql.Tx, now time.Time) error {
	res, _ := json.Marshal(Result{Refused: SessionEnded, Status: 409}) // 409 Conflict, as the API answers it
	if _, err := tx.Exec(`UPDATE jobs SET state = ?, reason = ?, result = ?, updated = ?, ended = ?
		WHERE kind = 'turn' AND state = 'queued' AND session IS NOT NULL AND `+sessionEnded("jobs.session"),
		Failed, ReasonSetup, string(res), nanos(now), nanos(now)); err != nil {
		return fmt.Errorf("queue: write job: %w", err)
	}
	return nil
}

// Lost says which of these turns ended because the lease of the attempt
// that ran them ran out. A turn starts only under a running attempt, so that
// is a lost attempt of the turn's job that had started; a lost one that was
// only taken ran no turn. The job no longer names its attempt once it
// expired (expire), so the attempt is found by its job.
func (q *Queue) Lost(ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		var n int
		if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs j JOIN attempts a ON a.job = j.id WHERE j.turn = ? AND a.state = ? AND a.started IS NOT NULL`, id, Lost).Scan(&n); err != nil {
			return nil, fmt.Errorf("queue: read attempt: %w", err)
		}
		if n > 0 {
			out[id] = true
		}
	}
	return out, nil
}

// Sweep settles every attempt whose lease ran out, and says how many: the
// ones it settled (sweepTx), so a saved finish, which it leaves to the
// finisher, is never reported as expired.
func (q *Queue) Sweep() (int, error) {
	var n int
	err := q.tx(func(tx *sql.Tx) error {
		var err error
		n, err = sweepTx(tx, q.now())
		return err
	})
	if err == nil && n > 0 {
		q.wake()
	}
	return n, err
}

// Start moves a taken attempt to running and renews its lease. Starting a
// running attempt again answers the same. cancel is true when the job was
// asked to stop: the runner then finishes it as cancelled.
func (q *Queue) Start(runnerID, attemptID string, number int, lease time.Duration) (att Attempt, cancel bool, err error) {
	err = q.tx(func(tx *sql.Tx) error {
		now := q.now()
		a, j, err := current(tx, now, runnerID, attemptID, number)
		if err != nil {
			return err
		}
		if a.State == Taken {
			if _, err := tx.Exec(`UPDATE attempts SET state = ?, started = ?, lease_until = ? WHERE id = ?`,
				Running, nanos(now), nanos(now.Add(lease)), a.ID); err != nil {
				return fmt.Errorf("queue: write attempt: %w", err)
			}
			if _, err := tx.Exec(`UPDATE jobs SET state = ?, updated = ? WHERE id = ?`, Running, nanos(now), j.ID); err != nil {
				return fmt.Errorf("queue: write job: %w", err)
			}
		}
		cancel = j.CancelRequested
		att, err = scanAttempt(tx.QueryRow(`SELECT `+attemptCols+` FROM attempts WHERE id = ?`, a.ID))
		return err
	})
	return att, cancel, err
}

// Heartbeat renews the attempt's lease. cancel is true when the job was asked
// to stop.
func (q *Queue) Heartbeat(runnerID, attemptID string, number int, lease time.Duration) (until time.Time, cancel bool, err error) {
	err = q.tx(func(tx *sql.Tx) error {
		now := q.now()
		a, j, err := current(tx, now, runnerID, attemptID, number)
		if err != nil {
			return err
		}
		until = now.Add(lease)
		if _, err := tx.Exec(`UPDATE attempts SET lease_until = ? WHERE id = ?`, nanos(until), a.ID); err != nil {
			return fmt.Errorf("queue: write attempt: %w", err)
		}
		if _, err := tx.Exec(`UPDATE runners SET seen = ? WHERE id = ?`, nanos(now), runnerID); err != nil {
			return fmt.Errorf("queue: write runner: %w", err)
		}
		cancel = j.CancelRequested
		return nil
	})
	if errors.Is(err, ErrStale) {
		q.wake()
	}
	return until, cancel, err
}

// AddEvents stores a batch of an attempt's events, numbered from offset. A
// batch sent again stores nothing new: a sequence number already stored is
// skipped. A batch that starts past the next expected number gets GapError.
// Events of an attempt that is no longer current are still kept on it; they
// never change its job. next is the next expected number; current says the
// attempt is still the job's current one.
func (q *Queue) AddEvents(runnerID, attemptID string, number int, offset int64, evs []AttemptEvent) (next int64, isCurrent bool, err error) {
	err = q.tx(func(tx *sql.Tx) error {
		a, err := scanAttempt(tx.QueryRow(`SELECT `+attemptCols+` FROM attempts WHERE id = ? AND runner = ?`, attemptID, runnerID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read attempt: %w", err)
		}
		if a.Number != number {
			return ErrStale
		}
		var cur string
		if err := tx.QueryRow(`SELECT COALESCE(attempt, '') FROM jobs WHERE id = ?`, a.Job).Scan(&cur); err != nil {
			return fmt.Errorf("queue: read job: %w", err)
		}
		isCurrent = cur == a.ID && (a.State == Taken || a.State == Running) && q.now().Before(a.LeaseUntil)
		if offset < 1 {
			return &GapError{Next: a.LastSeq + 1}
		}
		if offset > a.LastSeq+1 {
			return &GapError{Next: a.LastSeq + 1}
		}
		last := a.LastSeq
		for i, e := range evs {
			seq := offset + int64(i)
			if seq <= a.LastSeq {
				continue // sent before
			}
			data := e.Data
			if len(data) == 0 {
				data = json.RawMessage(`{}`)
			}
			t := e.Time
			if t.IsZero() {
				t = q.now()
			}
			if _, err := tx.Exec(`INSERT INTO attempt_events(attempt, seq, time, kind, data) VALUES(?,?,?,?,?)`,
				a.ID, seq, nanos(t), e.Kind, string(data)); err != nil {
				return fmt.Errorf("queue: write event: %w", err)
			}
			last = seq
		}
		if _, err := tx.Exec(`UPDATE attempts SET last_seq = ? WHERE id = ?`, last, a.ID); err != nil {
			return fmt.Errorf("queue: write attempt: %w", err)
		}
		next = last + 1
		return nil
	})
	return next, isCurrent, err
}

// Events reads an attempt's events of team, oldest first.
func (q *Queue) Events(team, attemptID string) ([]AttemptEvent, error) {
	var t string
	err := q.db.QueryRow(`SELECT j.team FROM attempts a JOIN jobs j ON j.id = a.job WHERE a.id = ?`, attemptID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && t != team) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("queue: read attempt: %w", err)
	}
	rows, err := q.db.Query(`SELECT seq, time, kind, data FROM attempt_events WHERE attempt = ? ORDER BY seq`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("queue: read events: %w", err)
	}
	defer rows.Close()
	evs := []AttemptEvent{}
	for rows.Next() {
		var e AttemptEvent
		var tm int64
		var data string
		if err := rows.Scan(&e.Seq, &tm, &e.Kind, &data); err != nil {
			return nil, fmt.Errorf("queue: read events: %w", err)
		}
		e.Time, e.Data = fromNanos(tm), json.RawMessage(data)
		evs = append(evs, e)
	}
	return evs, rows.Err()
}

// Accept saves finish f of the attempt before control writes the job's
// outcome (jobs/outcome.go). The job's result is f's, in the job's own
// result, where every reader of results reads it; the job stays running,
// with no end, so no reader takes it as done. The attempt holds f's hash
// (its one finish) and f's other fields (its result column, which holds
// nothing else before the end). From then on another finish gets
// ErrFinishConflict, from Accept or End, and writes nothing, and the
// attempt's lease no longer counts: it never expires, and control ends it
// with f (End), from Accepted: one finisher does that (jobs/finisher.go).
// The same finish sent again changes nothing. It answers the attempt's job;
// ended: the attempt ended already, by this same finish. A stale attempt
// gets ErrStale, as End answers it.
func (q *Queue) Accept(runnerID, attemptID string, number int, f Finish) (j Job, ended bool, err error) {
	if err := CheckFinish(Job{}, f); err != nil {
		return Job{}, false, err
	}
	if len(f.Result) == 0 {
		f.Result = nil
	}
	f.Kept = nil
	hash := hashOf(f)
	rest := f
	rest.Result = nil
	fields, err := json.Marshal(rest)
	if err != nil {
		return Job{}, false, err
	}
	err = q.tx(func(tx *sql.Tx) error {
		now := q.now()
		var done sql.NullString
		var number0 int
		err := tx.QueryRow(`SELECT finish_hash, number, ended IS NOT NULL FROM attempts WHERE id = ? AND runner = ?`,
			attemptID, runnerID).Scan(&done, &number0, &ended)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read attempt: %w", err)
		}
		if done.Valid && number0 == number {
			if done.String != hash {
				return ErrFinishConflict
			}
			// Held already, ended or not: nothing changes. The finisher
			// ends it (or has).
			var jobID string
			if err := tx.QueryRow(`SELECT job FROM attempts WHERE id = ?`, attemptID).Scan(&jobID); err != nil {
				return fmt.Errorf("queue: read attempt: %w", err)
			}
			j, err = scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, jobID))
			if err != nil {
				return fmt.Errorf("queue: read job: %w", err)
			}
			return nil
		}
		ended = false
		a, cur, err := current(tx, now, runnerID, attemptID, number)
		if err != nil {
			return err
		}
		if err := CheckFinish(cur, f); err != nil {
			return err
		}
		var result any
		if f.Result != nil {
			result = string(f.Result)
		}
		if _, err := tx.Exec(`UPDATE attempts SET finish_hash = ?, result = ? WHERE id = ?`, hash, string(fields), a.ID); err != nil {
			return fmt.Errorf("queue: write attempt: %w", err)
		}
		if _, err := tx.Exec(`UPDATE jobs SET result = ?, updated = ? WHERE id = ?`, result, nanos(now), cur.ID); err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
		cur.Result = f.Result
		j = cur
		return nil
	})
	return j, ended, err
}

// AcceptedFinish is a finish control accepted (Accept) and has not ended
// the job with yet: control stopped between the two.
type AcceptedFinish struct {
	Runner, Attempt string
	Number          int
	Job             Job
	Finish          Finish
	// Place is where it sits in the order Accepted reads: a later read
	// from here starts past it.
	Place Place
}

// Place is a saved finish's place in the order Accepted reads them: its
// attempt's take, then the attempt's id. The zero Place is before all.
type Place struct {
	taken   int64
	attempt string
}

// Accepted reads the finishes control accepted and has not ended, oldest
// first, past after (Place), at most limit, in one read: each finish's
// fields from its attempt, its result from its job, as Accept saved them.
// A reader that could not end one reads on from its Place, so the
// finishes after it never wait behind it.
func (q *Queue) Accepted(after Place, limit int) ([]AcceptedFinish, error) {
	var out []AcceptedFinish
	err := q.tx(func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.Query(`SELECT a.runner, a.id, a.number, a.job, a.result, a.taken FROM attempts a
			WHERE a.finish_hash IS NOT NULL AND a.ended IS NULL AND (a.taken > ? OR (a.taken = ? AND a.id > ?))
			ORDER BY a.taken, a.id LIMIT ?`, after.taken, after.taken, after.attempt, limit)
		if err != nil {
			return fmt.Errorf("queue: read accepted finishes: %w", err)
		}
		var jobIDs []string
		for rows.Next() {
			var a AcceptedFinish
			var job string
			var fields sql.NullString
			if err := rows.Scan(&a.Runner, &a.Attempt, &a.Number, &job, &fields, &a.Place.taken); err != nil {
				rows.Close()
				return fmt.Errorf("queue: read accepted finishes: %w", err)
			}
			if err := json.Unmarshal([]byte(fields.String), &a.Finish); err != nil {
				rows.Close()
				return fmt.Errorf("queue: read accepted finishes: %w", err)
			}
			a.Place.attempt = a.Attempt
			out, jobIDs = append(out, a), append(jobIDs, job)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, id := range jobIDs {
			j, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
			if err != nil {
				return fmt.Errorf("queue: read job: %w", err)
			}
			out[i].Job, out[i].Finish.Result = j, j.Result
		}
		return nil
	})
	return out, err
}

// End ends the attempt and its job with f. Sending the same f again for an
// attempt that already ended answers the same job. Another f gets
// ErrFinishConflict. A stale attempt gets ErrStale and changes nothing. A
// cancel asked for before the finish does not change it: the finish that
// arrives first is the one outcome. The job and its attempt keep f.Kept as
// their result when it is set, else f.Result.
func (q *Queue) End(runnerID, attemptID string, number int, f Finish) (Job, error) {
	if f.Status != Finished && f.Status != Failed && f.Status != Cancelled {
		return Job{}, ErrBadState
	}
	if len(f.Result) == 0 {
		f.Result = nil
	}
	hash := hashOf(f)
	var team, jobID string
	err := q.tx(func(tx *sql.Tx) error {
		now := q.now()
		var done sql.NullString
		var number0 int
		var ended bool
		err := tx.QueryRow(`SELECT a.job, j.team, a.finish_hash, a.number, a.ended IS NOT NULL FROM attempts a JOIN jobs j ON j.id = a.job
			WHERE a.id = ? AND a.runner = ?`, attemptID, runnerID).Scan(&jobID, &team, &done, &number0, &ended)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read attempt: %w", err)
		}
		if done.Valid && number0 == number {
			if done.String != hash {
				return ErrFinishConflict // another finish ended or was accepted
			}
			if ended {
				return nil // a retry of the same finish
			}
			// Accepted already (Accept): end it now.
		}
		a, j, err := current(tx, now, runnerID, attemptID, number)
		if err != nil {
			return err
		}
		if err := CheckFinish(j, f); err != nil {
			return err
		}
		var result any
		switch {
		case f.Kept != nil:
			result = string(f.Kept)
		case f.Result != nil:
			result = string(f.Result)
		}
		if _, err := tx.Exec(`UPDATE attempts SET state = ?, ended = ?, reason = ?, finish_hash = ?, result = ?, expected_seq = ?
			WHERE id = ?`, f.Status, nanos(now), f.Reason, hash, result, f.LastSeq, a.ID); err != nil {
			return fmt.Errorf("queue: write attempt: %w", err)
		}
		var verdict, findings any
		switch {
		case j.Kind == KindReview && f.Status == Finished:
			// The control plane reads the verdict; a runner's own is
			// ignored. No readable verdict is unknown, never pass.
			rr, _ := ReadResult(f.Result)
			v, fs := ParseVerdict(rr.ReplyText)
			verdict = v
			if fs != nil {
				b, _ := json.Marshal(fs)
				findings = string(b)
			}
		case j.Kind == KindReview:
			// A review that did not finish has no verdict: execution status
			// and verdict are separate.
		case f.Verdict != nil:
			verdict = *f.Verdict
		}
		if _, err := tx.Exec(`UPDATE jobs SET state = ?, reason = ?, verdict = ?, findings = ?, result = ?, updated = ?, ended = ? WHERE id = ?`,
			f.Status, f.Reason, verdict, findings, result, nanos(now), nanos(now), j.ID); err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
		if err := endSnapshotsTx(tx, now, a.ID); err != nil {
			return err
		}
		// A session's first job ties the session to this runner. A later
		// finish naming the same session changes nothing.
		if j.Kind == KindTurn && f.Session != "" && f.Session == j.Session && f.Workspace != nil && j.Spec.Session == "" {
			w := f.Workspace
			if _, err := tx.Exec(`INSERT INTO sessions(id, team, runner, kind, folder, repo, base, snapshot, created) VALUES(?,?,?,?,?,?,?,?,?)
				ON CONFLICT DO NOTHING`, f.Session, j.Team, runnerID, w.Kind, w.where(), w.Repo, w.Base, w.Snapshot, nanos(now)); err != nil {
				return fmt.Errorf("queue: write session: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Job{}, err
	}
	q.wake() // a slot is free
	return q.Get(team, jobID)
}

// Waiting reads team's turn and review jobs that no runner has started yet,
// newest first.
func (q *Queue) Waiting(team string) ([]Job, error) {
	return q.jobsWhere(`SELECT `+jobCols+` FROM jobs WHERE team = ? AND kind IN ('turn', 'review') AND state IN ('queued', 'taken')
		ORDER BY created DESC, rowid DESC LIMIT ?`, team, MaxQueued)
}

// NeverStarted reads team's turn and review jobs that ended before their
// turn started, such as a turn stopped while queued, newest first: those
// asked for at from or later and before to. A zero from or to is no bound.
// session, when set, keeps one session's: a review's own session is the one
// control named for it. The record holds no such turn; only the queue does.
func (q *Queue) NeverStarted(team, session string, from, to time.Time) ([]Job, error) {
	where := `team = ? AND kind IN ('turn', 'review') AND ended IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM attempts a JOIN attempt_events e ON e.attempt = a.id
			WHERE a.job = jobs.id AND e.kind = 'turn.created')`
	args := []any{team}
	if !from.IsZero() {
		where += ` AND created >= ?`
		args = append(args, nanos(from))
	}
	if !to.IsZero() {
		where += ` AND created < ?`
		args = append(args, nanos(to))
	}
	if session != "" {
		where += ` AND CASE kind WHEN 'review' THEN json_extract(links, '$.review_session') ELSE session END = ?`
		args = append(args, session)
	}
	args = append(args, MaxQueued)
	return q.jobsWhere(`SELECT `+jobCols+` FROM jobs WHERE `+where+` ORDER BY created DESC, rowid DESC LIMIT ?`, args...)
}

// JobsByTurns reads the jobs of team that run these turns, by turn id,
// without their attempts.
func (q *Queue) JobsByTurns(team string, turns []string) (map[string]Job, error) {
	out := map[string]Job{}
	for len(turns) > 0 {
		n := min(len(turns), 500)
		args := []any{team}
		marks := strings.Repeat(",?", n)[1:]
		for _, t := range turns[:n] {
			args = append(args, t)
		}
		turns = turns[n:]
		list, err := q.jobsWhere(`SELECT `+jobCols+` FROM jobs WHERE team = ? AND turn IN (`+marks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, j := range list {
			out[j.Turn] = j
		}
	}
	return out, nil
}

// jobsWhere reads the jobs a query of jobCols picks, without their attempts.
func (q *Queue) jobsWhere(query string, args ...any) ([]Job, error) {
	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: read jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: read jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Workspace reads the workspace of a session of team; ok is false when the
// queue holds none.
func (q *Queue) Workspace(team, session string) (w Workspace, ok bool, err error) {
	w, err = scanWorkspace(q.db.QueryRow(`SELECT `+workspaceCols+` FROM sessions WHERE id = ? AND team = ?`, session, team))
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, fmt.Errorf("queue: read session: %w", err)
	}
	return w, true, nil
}

// JobByTurn reads the job of team that runs the turn with this id.
func (q *Queue) JobByTurn(team, turn string) (Job, error) {
	var id string
	err := q.db.QueryRow(`SELECT id FROM jobs WHERE turn = ? AND team = ?`, turn, team).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("queue: read job: %w", err)
	}
	return q.Get(team, id)
}

// Cancel asks for a job of team to stop. A queued job is cancelled at once. A
// taken or running one is marked, and its runner hears it on its next
// heartbeat. A job that has ended is returned as it is.
func (q *Queue) Cancel(team, id string) (Job, error) {
	err := q.tx(func(tx *sql.Tx) error {
		now := nanos(q.now())
		var state string
		err := tx.QueryRow(`SELECT state FROM jobs WHERE id = ? AND team = ?`, id, team).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read job: %w", err)
		}
		switch state {
		case Queued:
			_, err = tx.Exec(`UPDATE jobs SET state = ?, cancel = 1, updated = ?, ended = ? WHERE id = ?`, Cancelled, now, now, id)
		case Taken, Running:
			_, err = tx.Exec(`UPDATE jobs SET cancel = 1, updated = ? WHERE id = ?`, now, id)
		}
		if err != nil {
			return fmt.Errorf("queue: write job: %w", err)
		}
		return nil
	})
	if err != nil {
		return Job{}, err
	}
	return q.Get(team, id)
}

// Sessions lists team's sessions, newest first.
func (q *Queue) Sessions(team string, limit int) ([]Workspace, error) {
	rows, err := q.db.Query(`SELECT `+workspaceCols+` FROM sessions WHERE team = ? ORDER BY created DESC, id LIMIT ?`, team, limit)
	if err != nil {
		return nil, fmt.Errorf("queue: list sessions: %w", err)
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list sessions: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Closed reads when each closed session of team was closed, by session id.
func (q *Queue) Closed(team string) (map[string]time.Time, error) {
	rows, err := q.db.Query(`SELECT id, closed FROM sessions WHERE team = ? AND closed IS NOT NULL`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: list closed sessions: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var closed int64
		if err := rows.Scan(&id, &closed); err != nil {
			return nil, fmt.Errorf("queue: list closed sessions: %w", err)
		}
		out[id] = fromNanos(closed)
	}
	return out, rows.Err()
}

// CloseSession closes a session of team: it takes no more jobs, and its
// runner removes its workspace on a later take. A session with a job that has
// not ended gets ErrSessionBusy. Closing it again answers the same.
func (q *Queue) CloseSession(team, id string) (Workspace, error) {
	var w Workspace
	err := q.tx(func(tx *sql.Tx) error {
		var closed sql.NullInt64
		err := tx.QueryRow(`SELECT closed FROM sessions WHERE id = ? AND team = ?`, id, team).Scan(&closed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read session: %w", err)
		}
		if !closed.Valid {
			var open int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE session = ? AND team = ? AND state IN ('queued','taken','running')`,
				id, team).Scan(&open); err != nil {
				return fmt.Errorf("queue: count jobs: %w", err)
			}
			if open > 0 {
				return ErrSessionBusy
			}
			if _, err := tx.Exec(`UPDATE sessions SET closed = ? WHERE id = ?`, nanos(q.now()), id); err != nil {
				return fmt.Errorf("queue: write session: %w", err)
			}
		}
		w, err = scanWorkspace(tx.QueryRow(`SELECT `+workspaceCols+` FROM sessions WHERE id = ?`, id))
		return err
	})
	if err == nil {
		q.wake() // its runner has a cleanup to take
	}
	return w, err
}

// RunnerBeat is a runner's idle heartbeat: it marks the runner seen and sets
// its capacity and, when not nil, its labels and its capabilities, and
// answers the runner. A runner restarted with other labels keeps its id.
func (q *Queue) RunnerBeat(runnerID string, capacity int, labels []string, caps *Capabilities) (Runner, error) {
	if _, err := q.db.Exec(`UPDATE runners SET seen = ?, capacity = ? WHERE id = ?`, nanos(q.now()), capacity, runnerID); err != nil {
		return Runner{}, fmt.Errorf("queue: write runner: %w", err)
	}
	if labels != nil {
		lb, _ := json.Marshal(labels)
		if _, err := q.db.Exec(`UPDATE runners SET labels = ? WHERE id = ?`, string(lb), runnerID); err != nil {
			return Runner{}, fmt.Errorf("queue: write runner: %w", err)
		}
	}
	if caps != nil {
		cb, _ := json.Marshal(caps)
		if _, err := q.db.Exec(`UPDATE runners SET caps = ? WHERE id = ?`, string(cb), runnerID); err != nil {
			return Runner{}, fmt.Errorf("queue: write runner: %w", err)
		}
	}
	q.wake()
	return q.runner(`id = ?`, runnerID)
}

// Removed records that the runner dealt with a closed session's workspace:
// removed it, or, with removalErr, could not.
func (q *Queue) Removed(runnerID, session, removalErr string) error {
	res, err := q.db.Exec(`UPDATE sessions SET removed = ?, removal_error = ? WHERE id = ? AND runner = ? AND closed IS NOT NULL AND removed IS NULL`,
		nanos(q.now()), removalErr, session, runnerID)
	if err != nil {
		return fmt.Errorf("queue: write session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		err := q.db.QueryRow(`SELECT 1 FROM sessions WHERE id = ? AND runner = ? AND removed IS NOT NULL`, session, runnerID).Scan(&one)
		if err != nil {
			return ErrNotFound
		}
	}
	return nil
}

// Review makes a review job of job id: kind review, read-only, for agent and
// model, in the reviewed job's workspace and so on its runner, with a prompt
// built from what the job recorded. The reviewed job must be a turn that has
// ended and recorded its changed files, in a session that is not closed.
// Several reviews of one job may run at once.
func (q *Queue) Review(team, id, agent, model, key string, includeReply bool) (Job, bool, error) {
	return q.ReviewAs(team, id, Spec{Harness: agent, Model: model}, key, includeReply)
}

// ReviewAs is Review with the reviewer as a saved agent: as names its
// harness in Agent, and its AgentID, Access, KeyEnv and Model.
func (q *Queue) ReviewAs(team, id string, as Spec, key string, includeReply bool) (Job, bool, error) {
	return q.review(team, id, as, key, includeReply, Links{}, Limits{})
}

// review is ReviewAs with links; ReviewsJob is set here.
func (q *Queue) review(team, id string, as Spec, key string, includeReply bool, links Links, lim Limits) (Job, bool, error) {
	j, err := q.Get(team, id)
	if err != nil {
		return Job{}, false, err
	}
	if j.Kind != KindTurn || j.Ended == nil {
		return Job{}, false, ErrNotReviewable
	}
	if j.Workspace == nil || j.Session == "" {
		return Job{}, false, ErrNotReviewable
	}
	if j.Workspace.Closed != nil {
		return Job{}, false, ErrSessionClosed
	}
	c, err := q.changesOf(j)
	if err != nil {
		return Job{}, false, err
	}
	prompt, err := ReviewPrompt(j, c, includeReply)
	if err != nil {
		return Job{}, false, ErrNotReviewable
	}
	spec := Spec{Harness: as.Harness, AgentID: as.AgentID, Access: as.Access, KeyEnv: as.KeyEnv, Model: as.Model,
		ModelFromAgent: as.ModelFromAgent, ReadOnly: true, Prompt: prompt}
	links.ReviewsJob = j.ID
	links.ReviewSession = NewID("")
	return q.create(team, KindReview, key, spec, links, lim, j.Session)
}
