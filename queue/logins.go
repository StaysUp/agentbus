package queue

// A machine signing in (agentbus login) and a machine removed. A sign-in is a
// row of logins until the person decides it: no runner row exists for it, so
// nothing of it is listed, routed to or trusted. Approving it makes the
// runner, with the token the machine was given when it asked, so the machine
// holds a working token only from the moment it was approved. Removing a
// runner marks it removed: its token stops working and it leaves the list,
// and its jobs, attempts and sessions stay as they were.

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const loginsSchema = `
CREATE TABLE IF NOT EXISTS logins (
	id         TEXT PRIMARY KEY,
	team       TEXT NOT NULL REFERENCES teams(id),
	code       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL,
	labels     TEXT NOT NULL,
	capacity   INTEGER NOT NULL,
	caps       TEXT,
	created    INTEGER NOT NULL,
	expires    INTEGER NOT NULL,
	state      TEXT NOT NULL,
	runner     TEXT,
	decided    INTEGER
);
`

// The states of a sign-in.
const (
	LoginPending  = "pending"
	LoginApproved = "approved"
	LoginRefused  = "refused"
	// LoginExpired is a pending sign-in past its expiry. It is never stored:
	// a pending row reads as expired once its time is past.
	LoginExpired = "expired"
)

// LoginTTL is how long a sign-in waits for the person.
const LoginTTL = 15 * time.Minute

// MaxPendingLogins bounds the sign-ins waiting at once, so a caller who may
// ask (anyone who reaches control) cannot fill the Runners page.
const MaxPendingLogins = 20

var (
	// ErrTooManyLogins: MaxPendingLogins are waiting.
	ErrTooManyLogins = errors.New("queue: too many sign-ins waiting")
	// ErrWrongCode: the code is not the one the machine shows.
	ErrWrongCode = errors.New("queue: the code does not match")
	// ErrRunnerOnline: an online runner is not removed.
	ErrRunnerOnline = errors.New("queue: the runner is online")
)

// Login is one machine's sign-in.
type Login struct {
	ID       string
	Team     string
	Name     string
	Labels   []string
	Capacity int
	Caps     *Capabilities
	Created  time.Time
	Expires  time.Time
	// State is LoginPending, LoginApproved, LoginRefused or LoginExpired.
	State string
	// Runner is the runner approval made.
	Runner string
}

// StartLogin keeps a machine's sign-in until the person decides it. code is
// what the machine shows, and tokenHash the hash of the runner token the
// machine holds; neither is read back by a list.
func (q *Queue) StartLogin(team, name string, labels []string, capacity int, tokenHash, code string, caps *Capabilities) (Login, error) {
	if labels == nil {
		labels = []string{}
	}
	now := q.now().UTC()
	l := Login{ID: NewID("lgn_"), Team: team, Name: name, Labels: labels, Capacity: capacity, Caps: caps,
		Created: now, Expires: now.Add(LoginTTL), State: LoginPending}
	lb, _ := json.Marshal(labels)
	var cb any
	if caps != nil {
		b, _ := json.Marshal(caps)
		cb = string(b)
	}
	err := q.tx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM logins WHERE team = ? AND state = ? AND expires > ?`,
			team, LoginPending, nanos(now)).Scan(&n); err != nil {
			return fmt.Errorf("queue: read sign-ins: %w", err)
		}
		if n >= MaxPendingLogins {
			return ErrTooManyLogins
		}
		if _, err := tx.Exec(`INSERT INTO logins(id, team, code, token_hash, name, labels, capacity, caps, created, expires, state)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, l.ID, team, code, tokenHash, name, string(lb), capacity, cb,
			nanos(now), nanos(l.Expires), LoginPending); err != nil {
			return fmt.Errorf("queue: keep sign-in: %w", err)
		}
		return nil
	})
	return l, err
}

const loginCols = `id, team, name, labels, capacity, caps, created, expires, state, COALESCE(runner, '')`

func (q *Queue) scanLogin(s scanner) (Login, error) {
	var l Login
	var labels string
	var caps sql.NullString
	var created, expires int64
	if err := s.Scan(&l.ID, &l.Team, &l.Name, &labels, &l.Capacity, &caps, &created, &expires, &l.State, &l.Runner); err != nil {
		return l, err
	}
	_ = json.Unmarshal([]byte(labels), &l.Labels)
	if caps.Valid {
		var c Capabilities
		if json.Unmarshal([]byte(caps.String), &c) == nil {
			l.Caps = &c
		}
	}
	l.Created, l.Expires = fromNanos(created), fromNanos(expires)
	if l.State == LoginPending && !q.now().Before(l.Expires) {
		l.State = LoginExpired
	}
	return l, nil
}

// LoginByToken is the sign-in of the token a machine was given.
func (q *Queue) LoginByToken(tokenHash string) (Login, error) {
	l, err := q.scanLogin(q.db.QueryRow(`SELECT `+loginCols+` FROM logins WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, fmt.Errorf("queue: read sign-in: %w", err)
	}
	return l, nil
}

// PendingLogins is every sign-in of team still waiting, oldest first.
func (q *Queue) PendingLogins(team string) ([]Login, error) {
	rows, err := q.db.Query(`SELECT `+loginCols+` FROM logins WHERE team = ? AND state = ? AND expires > ? ORDER BY created, id`,
		team, LoginPending, nanos(q.now()))
	if err != nil {
		return nil, fmt.Errorf("queue: read sign-ins: %w", err)
	}
	defer rows.Close()
	out := []Login{}
	for rows.Next() {
		l, err := q.scanLogin(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: read sign-ins: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ApproveLogin makes the runner of a waiting sign-in, when code is the one
// its machine shows. It answers ErrNotFound for no such sign-in of team,
// ErrBadState for one decided or expired, and ErrWrongCode.
func (q *Queue) ApproveLogin(team, id, code string) (Runner, error) {
	var rn Runner
	err := q.tx(func(tx *sql.Tx) error {
		l, want, hash, err := q.pendingLogin(tx, team, id)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) != 1 {
			return ErrWrongCode
		}
		now := q.now().UTC()
		rn = Runner{ID: NewID("run_"), Team: team, Name: l.Name, Labels: l.Labels, Capacity: l.Capacity, Created: now, Seen: now, Caps: l.Caps}
		lb, _ := json.Marshal(l.Labels)
		var cb any
		if l.Caps != nil {
			b, _ := json.Marshal(l.Caps)
			cb = string(b)
		}
		if _, err := tx.Exec(`INSERT INTO runners(id, team, name, labels, capacity, token_hash, created, seen, caps) VALUES(?,?,?,?,?,?,?,?,?)`,
			rn.ID, team, l.Name, string(lb), l.Capacity, hash, nanos(now), nanos(now), cb); err != nil {
			return fmt.Errorf("queue: register runner: %w", err)
		}
		if _, err := tx.Exec(`UPDATE logins SET state = ?, runner = ?, decided = ? WHERE id = ?`, LoginApproved, rn.ID, nanos(now), id); err != nil {
			return fmt.Errorf("queue: write sign-in: %w", err)
		}
		return nil
	})
	return rn, err
}

// RefuseLogin refuses a waiting sign-in: its token never becomes a runner's.
func (q *Queue) RefuseLogin(team, id string) error {
	return q.tx(func(tx *sql.Tx) error {
		if _, _, _, err := q.pendingLogin(tx, team, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE logins SET state = ?, decided = ? WHERE id = ?`, LoginRefused, nanos(q.now()), id); err != nil {
			return fmt.Errorf("queue: write sign-in: %w", err)
		}
		return nil
	})
}

// pendingLogin reads a sign-in of team that is still waiting, with its code
// and its token's hash.
func (q *Queue) pendingLogin(tx *sql.Tx, team, id string) (Login, string, string, error) {
	var code, hash string
	var l Login
	var labels string
	var caps sql.NullString
	var expires int64
	err := tx.QueryRow(`SELECT name, labels, capacity, caps, expires, state, code, token_hash FROM logins WHERE id = ? AND team = ?`, id, team).
		Scan(&l.Name, &labels, &l.Capacity, &caps, &expires, &l.State, &code, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return l, "", "", ErrNotFound
	}
	if err != nil {
		return l, "", "", fmt.Errorf("queue: read sign-in: %w", err)
	}
	if l.State != LoginPending || !q.now().Before(fromNanos(expires)) {
		return l, "", "", ErrBadState
	}
	_ = json.Unmarshal([]byte(labels), &l.Labels)
	if caps.Valid {
		var c Capabilities
		if json.Unmarshal([]byte(caps.String), &c) == nil {
			l.Caps = &c
		}
	}
	return l, code, hash, nil
}

// RemoveRunner removes a runner of team that is offline: its token stops
// working at once and it is no longer listed. Its jobs, attempts and sessions
// stay, but its sessions take no more work: their queued turns and reviews
// end with why (removed.go). It answers ErrNotFound for no such runner (or
// one removed already) and ErrRunnerOnline for one seen within RunnerFresh.
func (q *Queue) RemoveRunner(team, id string) error {
	return q.tx(func(tx *sql.Tx) error {
		var seen int64
		err := tx.QueryRow(`SELECT seen FROM runners WHERE id = ? AND team = ? AND removed IS NULL`, id, team).Scan(&seen)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read runner: %w", err)
		}
		now := q.now()
		if now.Sub(fromNanos(seen)) <= RunnerFresh {
			return ErrRunnerOnline
		}
		if _, err := tx.Exec(`UPDATE runners SET removed = ? WHERE id = ?`, nanos(now), id); err != nil {
			return fmt.Errorf("queue: remove runner: %w", err)
		}
		return endRemovedTx(tx, now)
	})
}
