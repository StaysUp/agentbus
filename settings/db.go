package settings

// DB is control's settings store: settings.db in the data folder, beside
// events.db and jobs.db. It holds only what the person changed: the policy as
// an overlay on the defaults (defaults.go), the instructions
// when they differ from the default text, and the saved agents the person
// added, changed or removed. The API is the only way in; nothing reads a
// settings file once the import has run (upgrade.go).
//
// Every change is one transaction that also writes the change's row in the
// changes table and moves the settings version up by one. A row is the
// settings.changed event waiting to be stored in the record: after the
// commit it is handed to the sink, and a sink that fails is tried again
// until it takes the row. So a change is never kept without its event, and
// the event names keys, never values, so no secret reaches the record.

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DBFile is the settings database in the data folder.
const DBFile = "settings.db"

// The settings a change names.
const (
	SettingPermissions  = "permissions"
	SettingInstructions = "instructions"
	SettingAgents       = "agents"
)

// What a turn's refusal says of a stored setting that cannot be used.
const (
	dbPolicyWhere       = "the permissions in the settings database"
	dbPolicyFix         = "Change them with PUT /api/v0/settings/permissions"
	dbInstructionsWhere = "the instructions in the settings database"
	dbInstructionsFix   = "Change them with PUT /api/v0/settings/instructions"
)

// Change is one change of the settings, as the record keeps it
// (settings.changed): which setting, the names of the keys that changed, and
// the settings version it made. Never a value.
type Change struct {
	ID      string
	Time    time.Time
	Setting string   // permissions, instructions or agents
	Keys    []string // such as limits.max_depth, or an agent id
	File    string   // on an import: the file the setting came from
	Version int64    // the settings version after the change
}

// AgentRow is a saved agent as the database keeps it: Data is the agent as
// JSON. A Removed row is an agent that is not there: one the person
// removed, whose Data then says so (agents.Store.Remove), or a default a
// new data folder started without, with no Data.
type AgentRow struct {
	ID      string
	Data    json.RawMessage
	Removed bool
}

// Info is the settings database's state, for GET /api/v0/settings.
type Info struct {
	Version  int64     // moves up by one with every change
	Created  time.Time // when the database was made
	Changed  time.Time // when the last change was made; zero for none
	Imported time.Time // when the old files were imported; zero for none
	Files    []string  // the files imported
}

// DB keeps the settings in settings.db.
type DB struct {
	db   *sql.DB
	now  func() time.Time
	mu   sync.Mutex // one change at a time
	pubM sync.Mutex // one publish at a time

	sinkMu sync.Mutex
	sink   func(Change) error
	logf   func(string, ...any)
	stop   chan struct{}
	done   chan struct{}

	protect []string
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS settings(name TEXT PRIMARY KEY, value TEXT NOT NULL, updated TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents(id TEXT PRIMARY KEY, ord INTEGER NOT NULL, data TEXT, removed INTEGER NOT NULL DEFAULT 0, updated TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS changes(seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, time TEXT NOT NULL,
	setting TEXT NOT NULL, keys TEXT NOT NULL, file TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL, published TEXT);
CREATE INDEX IF NOT EXISTS changes_unpublished ON changes(seq) WHERE published IS NULL;
`

// OpenDB opens or makes the settings database at path. A new one starts on
// the defaults as they are, at settings version 0. now nil is time.Now.
func OpenDB(path string, now func() time.Time) (*DB, error) {
	return openDB(path, now, len(upgradeSteps))
}

// openDB is OpenDB, with a new database counting done upgrade steps of
// upgradeSteps as written: all of them for a new data folder, none for one
// from 0.4, whose old defaults the upgrade then writes out.
func openDB(path string, now func() time.Time, done int) (*DB, error) {
	if now == nil {
		now = time.Now
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("settings: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("settings: the database %s could not be made: %w", path, err)
	}
	t := now().UTC().Format(time.RFC3339Nano)
	// The count is set only when the database is new: one made before the
	// count was kept is read by upgradesDone.
	if _, err := db.Exec(`INSERT INTO meta(key, value) SELECT 'upgrades', ? WHERE NOT EXISTS (SELECT 1 FROM meta WHERE key = 'created')`,
		strconv.Itoa(done)); err != nil {
		db.Close()
		return nil, fmt.Errorf("settings: the database %s could not be made: %w", path, err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('version', '0'), ('created', ?)`, t); err != nil {
		db.Close()
		return nil, fmt.Errorf("settings: the database %s could not be made: %w", path, err)
	}
	return &DB{db: db, now: now}, nil
}

// SetProtected sets what Protected answers: the data folder's own files.
func (d *DB) SetProtected(dir string) { d.protect = protected(dir) }

// Protected are the data folder's own files.
func (d *DB) Protected() []string { return d.protect }

// Close stops publishing and closes the database. Changes not yet stored in
// the record stay, and are stored after the next start.
func (d *DB) Close() error {
	d.sinkMu.Lock()
	stop, done := d.stop, d.done
	d.stop = nil
	d.sinkMu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	return d.db.Close()
}

// state is what one read of the database holds.
type state struct {
	version      int64
	overlay      flat
	instructions *string // nil: the default text
	created      time.Time
}

// querier is a *sql.DB or a *sql.Tx.
type querier interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func meta(q querier, key string) (string, error) {
	var v string
	err := q.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func readState(q querier) (state, error) {
	var s state
	v, err := meta(q, "version")
	if err != nil {
		return s, err
	}
	if s.version, err = strconv.ParseInt(v, 10, 64); err != nil {
		return s, fmt.Errorf("settings: the stored version %q is not a number", v)
	}
	c, err := meta(q, "created")
	if err != nil {
		return s, err
	}
	s.created, _ = time.Parse(time.RFC3339Nano, c)
	var raw string
	switch err := q.QueryRow(`SELECT value FROM settings WHERE name = ?`, SettingPermissions).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		s.overlay = flat{}
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal([]byte(raw), &s.overlay); err != nil {
			s.overlay = nil // read as broken by policyOf
		}
	}
	switch err := q.QueryRow(`SELECT value FROM settings WHERE name = ?`, SettingInstructions).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return s, err
	default:
		var body struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal([]byte(raw), &body) == nil {
			s.instructions = body.Text
		}
	}
	return s, nil
}

// policyOf is the policy s holds, and its version. A stored overlay that
// cannot be used is an *Error whose version is the overlay's, so a writer
// can replace it.
func (s state) policyOf() (Policy, Version, error) {
	if s.overlay == nil {
		v := Version("broken")
		return Policy{}, v, &Error{Setting: SettingPermissions, Where: dbPolicyWhere, Why: "they are not readable", Fix: dbPolicyFix, Version: v}
	}
	p, err := applyOverlay(defaultPolicy(), s.overlay)
	if err != nil {
		b, _ := json.Marshal(s.overlay)
		v := versionOf(b)
		return Policy{}, v, &Error{Setting: SettingPermissions, Where: dbPolicyWhere, Why: err.Error(), Fix: dbPolicyFix, Version: v}
	}
	p.where, p.fix = dbPolicyWhere, dbPolicyFix
	return p, policyVersion(p), nil
}

// instructionsOf is the text s holds, and its version.
func (s state) instructionsOf() (string, Version, error) {
	text := DefaultInstructions
	if s.instructions != nil {
		text = *s.instructions
	}
	v := versionOf([]byte(text))
	if err := CheckInstructions([]byte(text)); err != nil {
		return "", v, &Error{Setting: SettingInstructions, Where: dbInstructionsWhere, Why: err.Error(), Fix: dbInstructionsFix, Version: v}
	}
	return text, v, nil
}

func (d *DB) read() (state, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := readState(d.db)
	if err != nil {
		return s, &Error{Setting: "settings", Where: "the settings database", Why: err.Error(), Fix: "Check the data folder"}
	}
	return s, nil
}

// Policy is the policy: the defaults with the person's changes on top.
func (d *DB) Policy() (Policy, Version, error) {
	s, err := d.read()
	if err != nil {
		return Policy{}, "", err
	}
	return s.policyOf()
}

// Instructions are the instructions: the person's text, or the default.
func (d *DB) Instructions() (string, Version, error) {
	s, err := d.read()
	if err != nil {
		return "", "", err
	}
	return s.instructionsOf()
}

// Snapshot is the policy, the instructions and the settings version, read
// at one moment.
func (d *DB) Snapshot() (Snapshot, error) {
	s, err := d.read()
	if err != nil {
		return Snapshot{}, err
	}
	var out Snapshot
	out.Revision = s.version
	if out.Policy, out.PolicyVersion, err = s.policyOf(); err != nil {
		return Snapshot{}, err
	}
	if out.Instructions, out.InstructionsVersion, err = s.instructionsOf(); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// Info is the database's state.
func (d *DB) Info() (Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := readState(d.db)
	if err != nil {
		return Info{}, err
	}
	in := Info{Version: s.version, Created: s.created}
	var last string
	if err := d.db.QueryRow(`SELECT time FROM changes ORDER BY seq DESC LIMIT 1`).Scan(&last); err == nil {
		in.Changed, _ = time.Parse(time.RFC3339Nano, last)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Info{}, err
	}
	if t, _ := meta(d.db, "imported"); t != "" {
		in.Imported, _ = time.Parse(time.RFC3339Nano, t)
	}
	if f, _ := meta(d.db, "imported_files"); f != "" {
		_ = json.Unmarshal([]byte(f), &in.Files)
	}
	return in, nil
}

// SetPolicy stores p as the person's policy when the policy is still at
// match. Only the keys that differ from the defaults are kept.
func (d *DB) SetPolicy(p Policy, match Version) (Policy, Version, error) {
	if err := p.Check(); err != nil {
		return Policy{}, "", &Invalid{Why: err.Error()}
	}
	p.fill()
	var out Policy
	var v Version
	err := d.change(func(tx *sql.Tx, s state) ([]Change, error) {
		cur, curV, err := s.policyOf()
		if err != nil {
			cur = defaultPolicy()
		}
		if match != Any && match != curV {
			return nil, ErrChanged
		}
		out, v = p, policyVersion(p)
		out.where, out.fix = dbPolicyWhere, dbPolicyFix
		if err == nil && curV == v {
			return nil, nil // nothing changes
		}
		if err := putSetting(tx, SettingPermissions, overlayOf(defaultPolicy(), p), d.now()); err != nil {
			return nil, err
		}
		return []Change{{Setting: SettingPermissions, Keys: changedKeys(cur, p)}}, nil
	})
	if err != nil {
		return Policy{}, "", err
	}
	return out, v, nil
}

// ChangePolicy stores the keys change sets, on top of what the person set
// before, when the policy is still at match: PATCH. Each key it sets is kept
// as the person's own, even when its value is the default, so a later build
// that changes that default keeps their value. A key it leaves out keeps
// what it holds. A stored policy that cannot be used is refused here: it is
// replaced whole (SetPolicy, PUT), never changed key by key.
func (d *DB) ChangePolicy(change Policy, match Version) (Policy, Version, error) {
	if err := change.Check(); err != nil {
		return Policy{}, "", &Invalid{Why: err.Error()}
	}
	set, err := change.setKeys()
	if err != nil {
		return Policy{}, "", &Invalid{Why: err.Error()}
	}
	var out Policy
	var v Version
	err = d.change(func(tx *sql.Tx, s state) ([]Change, error) {
		_, curV, err := s.policyOf()
		if err != nil {
			return nil, &Invalid{Why: "the stored policy cannot be used, so it cannot be changed key by key: replace it whole with PUT"}
		}
		if match != Any && match != curV {
			return nil, ErrChanged
		}
		o := flat{}
		for k, val := range s.overlay {
			o[k] = val
		}
		var keys []string
		for k, val := range set {
			if old, had := o[k]; !had || !bytes.Equal(old, val) {
				keys = append(keys, k)
			}
			o[k] = val
		}
		p, err := applyOverlay(defaultPolicy(), o)
		if err != nil {
			return nil, &Invalid{Why: err.Error()}
		}
		out, v = p, policyVersion(p)
		out.where, out.fix = dbPolicyWhere, dbPolicyFix
		if len(keys) == 0 {
			return nil, nil // nothing changes
		}
		sort.Strings(keys)
		if err := putSetting(tx, SettingPermissions, o, d.now()); err != nil {
			return nil, err
		}
		return []Change{{Setting: SettingPermissions, Keys: keys}}, nil
	})
	if err != nil {
		return Policy{}, "", err
	}
	return out, v, nil
}

// SetInstructions stores text as the instructions when they are still at
// match.
func (d *DB) SetInstructions(text string, match Version) (Version, error) {
	if err := CheckInstructions([]byte(text)); err != nil {
		return "", &Invalid{Why: "text: " + err.Error()}
	}
	v := versionOf([]byte(text))
	err := d.change(func(tx *sql.Tx, s state) ([]Change, error) {
		_, curV, _ := s.instructionsOf()
		if match != Any && match != curV {
			return nil, ErrChanged
		}
		if curV == v {
			return nil, nil
		}
		if err := putInstructions(tx, text, d.now()); err != nil {
			return nil, err
		}
		return []Change{{Setting: SettingInstructions, Keys: []string{"text"}}}, nil
	})
	if err != nil {
		return "", err
	}
	return v, nil
}

// Agents are the saved agents the person added, changed or removed, in the
// order they were first stored.
func (d *DB) Agents() ([]AgentRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return readAgents(d.db)
}

func readAgents(q querier) ([]AgentRow, error) {
	rows, err := q.Query(`SELECT id, data, removed FROM agents ORDER BY ord, id`)
	if err != nil {
		return nil, fmt.Errorf("settings: the agents could not be read: %w", err)
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var r AgentRow
		var data sql.NullString
		if err := rows.Scan(&r.ID, &data, &r.Removed); err != nil {
			return nil, fmt.Errorf("settings: the agents could not be read: %w", err)
		}
		if data.Valid {
			r.Data = json.RawMessage(data.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAgent stores one agent's row, and its change with keys.
func (d *DB) SetAgent(r AgentRow, keys []string) error {
	return d.change(func(tx *sql.Tx, _ state) ([]Change, error) {
		if err := putAgent(tx, r, d.now()); err != nil {
			return nil, err
		}
		return []Change{{Setting: SettingAgents, Keys: keys}}, nil
	})
}

// Created is when the database was made: the time the default agents read
// as made.
func (d *DB) Created() time.Time {
	s, err := d.read()
	if err != nil {
		return time.Time{}
	}
	return s.created
}

func putSetting(tx *sql.Tx, name string, v any, now time.Time) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO settings(name, value, updated) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated = excluded.updated`, name, string(b), now.UTC().Format(time.RFC3339Nano))
	return err
}

// putInstructions keeps text only when it differs from the default text.
func putInstructions(tx *sql.Tx, text string, now time.Time) error {
	body := map[string]*string{}
	if text != DefaultInstructions {
		body["text"] = &text
	}
	return putSetting(tx, SettingInstructions, body, now)
}

func putAgent(tx *sql.Tx, r AgentRow, now time.Time) error {
	var data any
	if r.Data != nil {
		data = string(r.Data)
	}
	_, err := tx.Exec(`INSERT INTO agents(id, ord, data, removed, updated)
		VALUES (?, (SELECT COALESCE(MAX(ord), 0) + 1 FROM agents), ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data, removed = excluded.removed, updated = excluded.updated`,
		r.ID, data, r.Removed, now.UTC().Format(time.RFC3339Nano))
	return err
}

// change runs f in one transaction with the database's state, and stores
// each change f returns with the next settings version, in the same
// transaction. f returning no changes writes nothing. The changes are then
// handed to the sink.
func (d *DB) change(f func(tx *sql.Tx, s state) ([]Change, error)) error {
	d.mu.Lock()
	err := func() error {
		tx, err := d.db.Begin()
		if err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		defer tx.Rollback()
		s, err := readState(tx)
		if err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		cs, err := f(tx, s)
		if err != nil || len(cs) == 0 {
			return err
		}
		if err := addChanges(tx, s.version, cs, d.now()); err != nil {
			return err
		}
		return tx.Commit()
	}()
	d.mu.Unlock()
	if err != nil {
		return err
	}
	d.Publish()
	return nil
}

// addChanges stores cs as the next versions after version.
func addChanges(tx *sql.Tx, version int64, cs []Change, now time.Time) error {
	for _, c := range cs {
		version++
		if c.Keys == nil {
			c.Keys = []string{}
		}
		keys, _ := json.Marshal(c.Keys)
		if _, err := tx.Exec(`INSERT INTO changes(id, time, setting, keys, file, version) VALUES (?, ?, ?, ?, ?, ?)`,
			newChangeID(), now.UTC().Format(time.RFC3339Nano), c.Setting, string(keys), c.File, version); err != nil {
			return fmt.Errorf("settings: the change could not be stored: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'version'`, strconv.FormatInt(version, 10)); err != nil {
		return fmt.Errorf("settings: the change could not be stored: %w", err)
	}
	return nil
}

// newChangeID is a change's id, which is its event's id in the record.
func newChangeID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "evt_" + hex.EncodeToString(b)
}

// RetryEvery is how often changes the sink refused are tried again.
var RetryEvery = 5 * time.Second

// SetSink sets where changes go once stored: sink stores one change's event
// in the record, and must store an id once however often it is called with
// it (store.AppendOnce). Changes waiting from before are handed over now,
// and a change the sink refuses is tried again every RetryEvery until it
// takes it. logf, when set, logs a refusal.
func (d *DB) SetSink(sink func(Change) error, logf func(string, ...any)) {
	d.sinkMu.Lock()
	d.sink, d.logf = sink, logf
	start := d.stop == nil
	if start {
		d.stop, d.done = make(chan struct{}), make(chan struct{})
	}
	stop, done := d.stop, d.done
	d.sinkMu.Unlock()
	if start {
		go d.retry(stop, done, RetryEvery)
	}
	d.Publish()
}

func (d *DB) retry(stop, done chan struct{}, every time.Duration) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			d.Publish()
		}
	}
}

// Publish hands each change not yet in the record to the sink, oldest first,
// and marks it once the sink took it. It stops at the first the sink
// refuses: the next try starts there. It reports how many are still waiting.
func (d *DB) Publish() (waiting int) {
	d.sinkMu.Lock()
	sink, logf := d.sink, d.logf
	d.sinkMu.Unlock()
	if sink == nil {
		return -1
	}
	d.pubM.Lock()
	defer d.pubM.Unlock()
	pending, err := d.pending()
	if err != nil {
		if logf != nil {
			logf("settings.unpublished reason=read")
		}
		return -1
	}
	for i, c := range pending {
		if err := sink(c); err != nil {
			if logf != nil {
				logf("settings.unpublished id=%s setting=%s reason=record", c.ID, c.Setting)
			}
			return len(pending) - i
		}
		d.mu.Lock()
		_, err := d.db.Exec(`UPDATE changes SET published = ? WHERE id = ?`, d.now().UTC().Format(time.RFC3339Nano), c.ID)
		d.mu.Unlock()
		if err != nil {
			if logf != nil {
				logf("settings.unpublished id=%s setting=%s reason=mark", c.ID, c.Setting)
			}
			return len(pending) - i
		}
	}
	return 0
}

func (d *DB) pending() ([]Change, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT id, time, setting, keys, file, version FROM changes WHERE published IS NULL ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var t, keys string
		if err := rows.Scan(&c.ID, &t, &c.Setting, &keys, &c.File, &c.Version); err != nil {
			return nil, err
		}
		c.Time, _ = time.Parse(time.RFC3339Nano, t)
		_ = json.Unmarshal([]byte(keys), &c.Keys)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Changes are every change stored, oldest first, with whether each is in
// the record yet.
func (d *DB) Changes() (cs []Change, published []bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT id, time, setting, keys, file, version, published IS NOT NULL FROM changes ORDER BY seq`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Change
		var t, keys string
		var pub bool
		if err := rows.Scan(&c.ID, &t, &c.Setting, &keys, &c.File, &c.Version, &pub); err != nil {
			return nil, nil, err
		}
		c.Time, _ = time.Parse(time.RFC3339Nano, t)
		_ = json.Unmarshal([]byte(keys), &c.Keys)
		cs = append(cs, c)
		published = append(published, pub)
	}
	return cs, published, rows.Err()
}
