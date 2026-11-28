package queue

// A note is a short text AgentBus attaches to a running turn, such as "your
// folder changed" for the one-writer Update (plans/DESIGN_one_writer.md,
// section 3). It is internal plumbing: no route of the public API makes one.
//
// Control keeps it here, the one record of it, until a turn of its session
// has been given it. The turn's runner hears of it on its attempt's start and
// heartbeat answers (jobs.AttemptStarted, jobs.AttemptBeat), as it hears a
// cancel. The runner's gateway adds it to the turn's next main-thread model
// call, and once the vendor accepts that call (a 2xx) records it as a
// context.added piece of kind note, with its place; control marks the note
// delivered when that event reaches its record (jobs/record.go). A note no call carried (the turn ended first, or its
// harness is one own replay does not serve, agy today) stays here, for the
// session's next turn.
//
// opencode does the same with its "steer" delivery: the message waits in the
// session's inbox and is taken at the running turn's next step
// (packages/core/src/session/runner/llm.ts:94-120).

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const notesSchema = `
-- A note for a session's running turn (notes.go): turn is the turn it was
-- attached to; delivered, once set, the turn that was given it.
CREATE TABLE IF NOT EXISTS notes (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	team      TEXT NOT NULL REFERENCES teams(id),
	session   TEXT NOT NULL,
	turn      TEXT NOT NULL,
	text      TEXT NOT NULL,
	created   INTEGER NOT NULL,
	delivered TEXT
);
CREATE INDEX IF NOT EXISTS notes_pending ON notes(session, delivered);
`

// MaxNote bounds a note's text, in bytes.
const MaxNote = 2 << 10

// MaxNotes bounds a session's notes not yet delivered.
const MaxNotes = 20

var (
	// ErrNoteInvalid: a note's text is empty, over MaxNote or not UTF-8.
	ErrNoteInvalid = errors.New("queue: a note needs 1 to 2048 bytes of UTF-8 text")
	// ErrNotesFull: the session has MaxNotes notes not yet delivered.
	ErrNotesFull = errors.New("queue: the session has too many notes waiting")
)

// Note is one note, as a runner gets it.
type Note struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

// AddNote attaches a note to the turn of team with this id, a turn job's.
// Its session's running turn is given it at its next main-thread model call.
// A turn that has ended passes it on to its session's next turn.
func (q *Queue) AddNote(team, turn, text string) (Note, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > MaxNote || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return Note{}, ErrNoteInvalid
	}
	var n Note
	err := q.tx(func(tx *sql.Tx) error {
		var session string
		err := tx.QueryRow(`SELECT COALESCE(session, '') FROM jobs WHERE turn = ? AND team = ? AND kind = ?`,
			turn, team, KindTurn).Scan(&session)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && session == "") {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue: read job: %w", err)
		}
		var waiting int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM notes WHERE team = ? AND session = ? AND delivered IS NULL`,
			team, session).Scan(&waiting); err != nil {
			return fmt.Errorf("queue: read notes: %w", err)
		}
		if waiting >= MaxNotes {
			return ErrNotesFull
		}
		res, err := tx.Exec(`INSERT INTO notes(team, session, turn, text, created) VALUES(?,?,?,?,?)`,
			team, session, turn, text, nanos(q.now()))
		if err != nil {
			return fmt.Errorf("queue: write note: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("queue: write note: %w", err)
		}
		n = Note{ID: id, Text: text}
		return nil
	})
	return n, err
}

// pendingNotes are the notes of a turn job's session that no turn has been
// given, oldest first. Any other job has none.
func pendingNotes(tx *sql.Tx, j Job) ([]Note, error) {
	if j.Kind != KindTurn || j.Session == "" {
		return nil, nil
	}
	rows, err := tx.Query(`SELECT id, text FROM notes WHERE team = ? AND session = ? AND delivered IS NULL ORDER BY id`,
		j.Team, j.Session)
	if err != nil {
		return nil, fmt.Errorf("queue: read notes: %w", err)
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Text); err != nil {
			return nil, fmt.Errorf("queue: read notes: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotesDelivered marks the notes named delivered to turn, a turn of session:
// its record holds them as given (context.added). A note of another session,
// or one already marked, is left as it is.
func (q *Queue) NotesDelivered(team, session, turn string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return q.tx(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE notes SET delivered = ? WHERE id = ? AND team = ? AND session = ? AND delivered IS NULL`,
				turn, id, team, session); err != nil {
				return fmt.Errorf("queue: write note: %w", err)
			}
		}
		return nil
	})
}

// AttemptNotes are the notes the current attempt's turn has not been given
// (pendingNotes), for its start and heartbeat answers.
func (q *Queue) AttemptNotes(runnerID, attemptID string, number int) ([]Note, error) {
	var out []Note
	err := q.tx(func(tx *sql.Tx) error {
		_, j, err := current(tx, q.now(), runnerID, attemptID, number)
		if err != nil {
			return err
		}
		out, err = pendingNotes(tx, j)
		return err
	})
	return out, err
}

// PendingNotes are the notes of session no turn has been given, oldest
// first.
func (q *Queue) PendingNotes(team, session string) ([]Note, error) {
	var out []Note
	err := q.tx(func(tx *sql.Tx) error {
		var err error
		out, err = pendingNotes(tx, Job{Kind: KindTurn, Team: team, Session: session})
		return err
	})
	return out, err
}
