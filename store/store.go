// Package store keeps every event in one SQLite file, in order.
//
// Each event gets a seq when it is written: 1, 2, 3 …, without gaps. Each store
// file has a random id, made once when the file is created. A cursor is the
// pair of the two, so a cockpit holding a seq from another file can tell.
//
// There is one writer. The store is an event.Sink, so the gateway writes into
// it through its own queue, and the client API writes through Append.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	seq     INTEGER PRIMARY KEY,
	session TEXT    NOT NULL,
	turn    TEXT    NOT NULL,
	kind    TEXT    NOT NULL,
	time    INTEGER NOT NULL,
	client  TEXT    NOT NULL,
	data    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS events_session ON events(session, seq);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind, seq);
CREATE INDEX IF NOT EXISTS events_turn ON events(session, turn);
CREATE INDEX IF NOT EXISTS events_by_turn ON events(turn, seq);
CREATE INDEX IF NOT EXISTS events_time ON events(time);
-- A retry sends the same conversation, so the same turn: turn.started is kept
-- once per (session, turn).
CREATE UNIQUE INDEX IF NOT EXISTS events_turn_started ON events(session, turn)
	WHERE kind = 'turn.started' AND turn <> '';
-- POST /api/v0/turns finds the turn an idempotency key started (TurnByKey).
CREATE INDEX IF NOT EXISTS events_idempotency ON events(json_extract(data, '$.idempotency_key'))
	WHERE kind = 'turn.started';
`

// Store is one events file. It is safe for concurrent use.
type Store struct {
	w    *sql.DB  // the one writer
	lock *os.File // held with flock for the store's life
	r    *sql.DB  // readers
	id   string

	mu     sync.Mutex // serialises writes, guards last, subs and closed
	last   int64
	subs   map[chan struct{}]struct{}
	closed bool

	// OnError, if set, hears about an event Emit could not write. Emit has no
	// way to return the error. Set it before the first Emit.
	OnError func(error)
}

// ErrInUse means another process holds the store: two services writing one
// file would each number events on their own and lose some.
var ErrInUse = errors.New("store: another agentbus service is using this data folder")

// Open opens or creates the store at path. It takes an exclusive lock on
// path+".lock", next to the file, held until Close. A second Open of the same
// store, from this process or another, fails with ErrInUse.
func Open(path string) (*Store, error) {
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open the lock file: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("store: lock: %w", err)
	}
	s, err := openLocked(path)
	if err != nil {
		lock.Close() // closing the file releases the lock
		return nil, err
	}
	s.lock = lock
	return s, nil
}

func openLocked(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	w, err := sql.Open("sqlite", fileURI(path, q))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	w.SetMaxOpenConns(1)
	s := &Store{w: w, subs: map[chan struct{}]struct{}{}}
	if err := s.init(); err != nil {
		_ = w.Close()
		return nil, err
	}
	rq := url.Values{}
	rq.Add("_pragma", "busy_timeout(5000)")
	rq.Add("_pragma", "query_only(1)")
	r, err := sql.Open("sqlite", fileURI(path, rq))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open: %w", err)
	}
	r.SetMaxOpenConns(4)
	s.r = r
	return s, nil
}

// fileURI names the file as a SQLite file: URI with its path escaped. A bare
// path would not do: SQLite and the driver read everything after a ? as
// parameters, so /data?one/events.db would open /data.
func fileURI(path string, q url.Values) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}

func (s *Store) init() error {
	tx, err := s.w.Begin()
	if err != nil {
		return fmt.Errorf("store: init: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("store: create tables: %w", err)
	}
	if _, err := tx.Exec(callsSchema); err != nil {
		return fmt.Errorf("store: create tables: %w", err)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("store: make id: %w", err)
	}
	// Made once: a second Open finds the row and keeps it.
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('store', ?) ON CONFLICT DO NOTHING`,
		"st_"+hex.EncodeToString(b)); err != nil {
		return fmt.Errorf("store: write id: %w", err)
	}
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'store'`).Scan(&s.id); err != nil {
		return fmt.Errorf("store: read id: %w", err)
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&s.last); err != nil {
		return fmt.Errorf("store: read last seq: %w", err)
	}
	return tx.Commit()
}

// ID is the store's random id, made when the file was created.
func (s *Store) ID() string { return s.id }

// Last is the highest seq written so far; 0 for an empty store.
func (s *Store) Last() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// ErrClosed is returned by Append after Close.
var ErrClosed = errors.New("store: closed")

// Append writes e and returns it with its Seq. stored is false, with no error,
// for a turn.started whose (session, turn) is already stored: that request was
// a retry.
func (s *Store) Append(e event.Event) (out event.Event, stored bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(e)
}

// AppendOnce is Append of an event another service already stored under
// its own seq, such as a runner's turn sent to the control plane: one whose
// id this store holds for the same session and turn is not stored again, so
// a batch sent twice is stored once. e must have an ID.
func (s *Store) AppendOnce(e event.Event) (stored bool, err error) {
	if e.ID == "" {
		return false, errors.New("store: an event sent on needs its id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.w.QueryRow(`SELECT COUNT(*) FROM events WHERE session = ? AND turn = ? AND json_extract(data, '$.id') = ?`,
		e.Session, e.Turn, e.ID).Scan(&n); err != nil {
		return false, fmt.Errorf("store: read %s: %w", e.Kind, err)
	}
	if n > 0 {
		return false, nil
	}
	_, stored, err = s.appendLocked(e)
	return stored, err
}

// AppendFirst is Append of an event a turn holds once, such as tools.listed:
// when the store already holds an event of e's kind for e's session and turn,
// nothing is stored and stored is false. The check and the write are one step
// under the store's lock, so two at once store one. e must name a turn.
func (s *Store) AppendFirst(e event.Event) (stored bool, err error) {
	if e.Turn == "" {
		return false, errors.New("store: an event a turn holds once needs its turn")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.w.QueryRow(`SELECT COUNT(*) FROM events WHERE session = ? AND turn = ? AND kind = ?`,
		e.Session, e.Turn, string(e.Kind)).Scan(&n); err != nil {
		return false, fmt.Errorf("store: read %s: %w", e.Kind, err)
	}
	if n > 0 {
		return false, nil
	}
	_, stored, err = s.appendLocked(e)
	return stored, err
}

// appendLocked is Append with s.mu held.
func (s *Store) appendLocked(e event.Event) (out event.Event, stored bool, err error) {
	if s.closed {
		return e, false, ErrClosed
	}
	e.Seq = s.last + 1
	data, err := json.Marshal(e)
	if err != nil {
		return e, false, fmt.Errorf("store: encode %s: %w", e.Kind, err)
	}
	res, err := s.w.Exec(`INSERT INTO events(seq, session, turn, kind, time, client, data)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session, turn) WHERE kind = 'turn.started' AND turn <> '' DO NOTHING`,
		e.Seq, e.Session, e.Turn, string(e.Kind), e.Time.UnixNano(), e.Client, string(data))
	if err != nil {
		return e, false, fmt.Errorf("store: write %s: %w", e.Kind, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return e, false, fmt.Errorf("store: write %s: %w", e.Kind, err)
	} else if n == 0 {
		e.Seq = 0
		return e, false, nil
	}
	s.last = e.Seq
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // already signalled
		}
	}
	return e, true, nil
}

// Emit implements event.Sink.
func (s *Store) Emit(e event.Event) {
	if _, _, err := s.Append(e); err != nil && s.OnError != nil {
		s.OnError(err)
	}
}

// Notify returns a channel that receives a signal after each write. A signal
// can stand for several writes, so the reader then reads everything after its
// own cursor. Call cancel when done.
func (s *Store) Notify() (ch <-chan struct{}, cancel func()) {
	c := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[c] = struct{}{}
	s.mu.Unlock()
	return c, func() {
		s.mu.Lock()
		delete(s.subs, c)
		s.mu.Unlock()
	}
}

// Close stops writes and closes the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	// The lock goes last, once the file is closed.
	return errors.Join(s.r.Close(), s.w.Close(), s.lock.Close())
}

// Record is one stored event: its seq, kind and JSON, which includes the seq.
type Record struct {
	Seq  int64
	Kind event.Kind
	Data json.RawMessage
}

// Filter picks events.
type Filter struct {
	// Session, when Only is set, keeps that session's events plus every
	// events.dropped that lists the session or says all_sessions.
	Session string
	Only    bool
	// Turn, when set, keeps that turn's events only, and no events.dropped:
	// a drop names sessions, never turns. It takes the place of Session and
	// Only, except that TurnSession, when set, keeps only that session's
	// events of the turn: the same turn id can be in two sessions.
	Turn        string
	TurnSession string
}

// PageBytes bounds the event JSON one page holds in memory: 1,000 events of
// 128 KiB would otherwise hold over 130 MB. A page always holds at least one
// event, so a single event over the budget still pages forward.
const PageBytes = 8 << 20

// After returns up to limit events with seq greater than after, oldest first,
// within PageBytes.
func (s *Store) After(after int64, f Filter, limit int) ([]Record, error) {
	recs, _, err := s.Page(after, f, limit, PageBytes)
	return recs, err
}

// Page returns events with seq greater than after, oldest first: at most
// limit of them, and no more event JSON than maxBytes unless the first event
// alone is larger. more says whether further events match, so the next page
// starts after the last seq returned.
func (s *Store) Page(after int64, f Filter, limit, maxBytes int) (recs []Record, more bool, err error) {
	var rows *sql.Rows
	if f.Turn != "" && f.TurnSession != "" {
		rows, err = s.r.Query(`SELECT seq, kind, data FROM events WHERE turn = ? AND session = ? AND seq > ? ORDER BY seq LIMIT ?`,
			f.Turn, f.TurnSession, after, limit+1)
	} else if f.Turn != "" {
		rows, err = s.r.Query(`SELECT seq, kind, data FROM events WHERE turn = ? AND seq > ? ORDER BY seq LIMIT ?`,
			f.Turn, after, limit+1)
	} else if f.Only {
		rows, err = s.r.Query(`SELECT seq, kind, data FROM events
			WHERE seq > ? AND (
				(session = ? AND kind <> 'events.dropped')
				OR (kind = 'events.dropped' AND (
					json_extract(data, '$.all_sessions') = 1
					OR EXISTS (SELECT 1 FROM json_each(data, '$.sessions') WHERE value = ?))))
			ORDER BY seq LIMIT ?`, after, f.Session, f.Session, limit+1)
	} else {
		rows, err = s.r.Query(`SELECT seq, kind, data FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit+1)
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	size := 0
	for rows.Next() {
		if len(recs) == limit {
			return recs, true, nil
		}
		var r Record
		var data string
		if err := rows.Scan(&r.Seq, &r.Kind, &data); err != nil {
			return nil, false, fmt.Errorf("store: read events: %w", err)
		}
		if len(recs) > 0 && size+len(data) > maxBytes {
			return recs, true, nil
		}
		size += len(data)
		r.Data = json.RawMessage(data)
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: read events: %w", err)
	}
	return recs, false, nil
}

// Continuation is what a session's next turn needs: the harness and folder of
// its latest turn or run that names them (agentFrom), the resume_id from its
// latest session.closed that carries one, and whether a run's child for it is
// still running.
type Continuation struct {
	Known   bool   // the store has events of the session
	Harness string // the harness, such as claude
	Folder  string
	// FolderRef: the reference of the person's folder, from the same
	// event; "" for a session from before references.
	FolderRef string
	ResumeID  string
	Open      bool
	// AgentID, Access and KeyEnv come from the same event as Harness: the
	// saved agent, how it reaches its model, and the name of its key's
	// variable. Empty for a session started by harness name alone.
	AgentID, Access, KeyEnv string
	// Model is the model the same event's run was started with: "" for the
	// harness's default. A next turn that names no agent and no model keeps
	// it.
	Model string
	// ResumeStale: a turn that got the earlier turns as text (continuity
	// transcript) started after the session.closed that gave ResumeID. That
	// id names a conversation from before the switch, without the turns
	// since, and maybe another vendor's.
	ResumeStale bool
}

// Continuation reads what the session's next turn needs, as of now.
func (s *Store) Continuation(id string) (Continuation, error) {
	var c Continuation
	var agent, folder, folderRef, resume, agentID, access, keyEnv, model sql.NullString
	err := s.r.QueryRow(`WITH latest AS (SELECT data FROM events
				WHERE session = ?1 AND `+agentFrom+` ORDER BY seq DESC LIMIT 1)
		SELECT
			EXISTS(SELECT 1 FROM events WHERE session = ?1),
			(SELECT json_extract(data, '$.agent') FROM latest),
			(SELECT json_extract(data, '$.folder') FROM latest),
			(SELECT json_extract(data, '$.folder_ref') FROM latest),
			(SELECT json_extract(data, '$.agent_id') FROM latest),
			(SELECT json_extract(data, '$.access') FROM latest),
			(SELECT json_extract(data, '$.key_env') FROM latest),
			(SELECT json_extract(data, '$.model') FROM latest),
			(SELECT json_extract(data, '$.resume_id') FROM events
				WHERE session = ?1 AND kind = 'session.closed' AND json_extract(data, '$.resume_id') IS NOT NULL
				ORDER BY seq DESC LIMIT 1),
			EXISTS(SELECT 1 FROM events t WHERE t.session = ?1 AND t.kind = 'turn.started'
				AND json_extract(t.data, '$.continuity') = 'transcript'
				AND t.seq > (SELECT seq FROM events WHERE session = ?1 AND kind = 'session.closed'
					AND json_extract(data, '$.resume_id') IS NOT NULL ORDER BY seq DESC LIMIT 1))`,
		id).Scan(&c.Known, &agent, &folder, &folderRef, &agentID, &access, &keyEnv, &model, &resume, &c.ResumeStale)
	if err != nil {
		return c, fmt.Errorf("store: read session: %w", err)
	}
	c.Harness, c.Folder, c.FolderRef, c.ResumeID = agent.String, folder.String, folderRef.String, resume.String
	c.AgentID, c.Access, c.KeyEnv, c.Model = agentID.String, access.String, keyEnv.String, model.String
	holds, err := s.OpenHolds(s.Last())
	if err != nil {
		return c, err
	}
	for _, h := range holds {
		if h.Session == id {
			c.Open = true
		}
	}
	return c, nil
}

// KeyedTurn is the turn an idempotency key started.
type KeyedTurn struct {
	Session, Turn string
	RequestHash   string
}

// TurnByKey finds the turn.started stored with idempotency_key key: the first
// one, which is the only one, since POST /api/v0/turns checks the key before it
// stores a turn.
func (s *Store) TurnByKey(key string) (t KeyedTurn, found bool, err error) {
	var hash sql.NullString
	err = s.r.QueryRow(`SELECT session, turn, json_extract(data, '$.request_hash') FROM events
		WHERE kind = 'turn.started' AND json_extract(data, '$.idempotency_key') = ?
		ORDER BY seq LIMIT 1`, key).Scan(&t.Session, &t.Turn, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, fmt.Errorf("store: read turn by key: %w", err)
	}
	t.RequestHash = hash.String
	return t, true, nil
}

// Known reports whether the store has any event of the session: a session
// seen only through the gateway counts.
func (s *Store) Known(id string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM events WHERE session = ? LIMIT 1)`, id).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: read session: %w", err)
	}
	return n > 0, nil
}

// RunOf is the agent and folder of the session's latest session.opened that
// names them: the run or next turn that started its latest child. Empty for a
// session no run started.
func (s *Store) RunOf(session string) (agent, folder string, err error) {
	var a, f sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.agent'), json_extract(data, '$.folder') FROM events
		WHERE session = ? AND kind = 'session.opened' AND COALESCE(json_extract(data, '$.agent'), '') <> ''
		ORDER BY seq DESC LIMIT 1`, session).Scan(&a, &f)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("store: read session: %w", err)
	}
	return a.String, f.String, nil
}

// agentFrom picks the events that name a session's agent and folder: a
// turn.started a run's turn stamped, a run's session.opened, and an older
// store's session.registered. The latest one wins: they agree within one run,
// and a later run's own values are the current ones.
const agentFrom = `kind IN ('turn.started', 'session.opened', 'session.registered')
	AND COALESCE(json_extract(data, '$.agent'), '') <> ''`

// OpenHold is a hold that has a session.opened and no session.closed: a run's
// child still running, or one the previous service left behind.
type OpenHold struct {
	Session string
	Hold    string
	Turn    string // the turn its session.opened named; empty in an older store
}

// OpenHolds lists the holds opened and not closed, up to seq hi.
func (s *Store) OpenHolds(hi int64) ([]OpenHold, error) {
	rows, err := s.r.Query(`SELECT session, json_extract(data, '$.hold') AS hold, MAX(turn) FROM events
		WHERE seq <= ? AND kind IN ('session.opened', 'session.closed')
		GROUP BY session, hold
		HAVING SUM(kind = 'session.opened') > 0 AND SUM(kind = 'session.closed') = 0
		ORDER BY MIN(seq)`, hi)
	if err != nil {
		return nil, fmt.Errorf("store: read holds: %w", err)
	}
	defer rows.Close()
	var out []OpenHold
	for rows.Next() {
		var h OpenHold
		var hold sql.NullString
		if err := rows.Scan(&h.Session, &hold, &h.Turn); err != nil {
			return nil, fmt.Errorf("store: read holds: %w", err)
		}
		h.Hold = hold.String
		out = append(out, h)
	}
	return out, rows.Err()
}

// CloseStaleHolds stores session.closed for every hold still open. The service
// calls it when it starts: those children were the previous process's, which
// can no longer report their exit. Each is unsettled: the previous process
// stopped without confirming that the turn's calls were stored (its event
// queue may have held some), so own replay never builds a history from what
// is left of such a turn.
func (s *Store) CloseStaleHolds(now time.Time, newID func() string) (int, error) {
	holds, err := s.OpenHolds(s.Last())
	if err != nil {
		return 0, err
	}
	for _, h := range holds {
		if _, _, err := s.Append(event.Event{ID: newID(), Kind: event.SessionClosed, Time: now,
			Session: h.Session, Turn: h.Turn, Hold: h.Hold, Unsettled: true}); err != nil {
			return 0, err
		}
	}
	return len(holds), nil
}
