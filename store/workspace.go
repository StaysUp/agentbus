package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/staysup/agentbus/event"
)

// Workspace is a workspace as its events describe it. A workspace is where
// turns run: it owns the folder, the machine, the sandbox mode (a copy behind a
// wall, or the person's real folder), its checkpoints, Apply and Discard. One
// workspace backs one session.
//
// The core facts (folder, folder_ref, machine, sandbox, created) come from the
// single workspace.created. The apply and discard history comes from the
// workspace's workspace.applied and workspace.discarded, and, for a workspace
// migrated from a pre-workspace session, its old turn.applied events (read the
// same way, since a migrated workspace's id is its session's id). head_turn is
// the workspace's latest turn that recorded a changeset.
type Workspace struct {
	ID        string    `json:"workspace"`
	Folder    string    `json:"folder,omitempty" doc:"The person's folder the workspace runs in."`
	FolderRef string    `json:"folder_ref,omitempty" doc:"The folder's reference (fld_ and 16 hex digits), as its connector issued it."`
	Machine   string    `json:"machine,omitempty" doc:"The connector that owns the folder (con_ and 16 hex digits). Absent for this service's own machine."`
	Sandbox   bool      `json:"sandbox" doc:"The mode: true for a copy behind a wall, false for the person's real folder."`
	Created   time.Time `json:"created" doc:"When the workspace was created."`
	// HeadTurn is the workspace's latest turn that recorded a changeset
	// (turn.changed), the turn an apply would go up to. Empty when no turn of
	// the workspace has ended with a changeset.
	HeadTurn string `json:"head_turn,omitempty" doc:"The workspace's latest turn that recorded a changeset; an apply goes up to it. Absent when none has."`
	// AppliedTurn is the turn the workspace's latest successful apply went up
	// to (workspace.applied, or a pre-workspace turn.applied with result
	// applied). Empty when nothing was applied.
	AppliedTurn string `json:"applied_turn,omitempty" doc:"The turn the workspace's latest successful apply went up to. Absent when nothing was applied."`
	// DiscardedAt is set when the workspace's latest apply-or-discard action
	// was a discard, so its unapplied changes were dropped and no apply has run
	// since. Nil otherwise.
	DiscardedAt *time.Time `json:"discarded_at,omitempty" doc:"When the workspace's latest action was a discard. Absent when its latest action was an apply, or it has neither."`
}

// ErrNoWorkspace means no workspace.created names the id.
var ErrNoWorkspace = errors.New("store: no such workspace")

// resolvedWorkspace is the id a turn.started names as its workspace, or the
// session's own id when it names none (a pre-workspace turn, whose session is
// its workspace). A workspace's turns are the turns whose resolved workspace is
// its id.
const resolvedWorkspace = `COALESCE(NULLIF(json_extract(ts.data, '$.workspace'), ''), ts.session)`

// Workspace reads one workspace by id. found is false, with no error, when no
// workspace.created names it.
func (s *Store) Workspace(id string) (w Workspace, found bool, err error) {
	w.ID = id
	var folder, ref, machine sql.NullString
	var sandbox int
	var created int64
	err = s.r.QueryRow(`SELECT json_extract(data, '$.folder'), json_extract(data, '$.folder_ref'),
			json_extract(data, '$.machine'), COALESCE(json_extract(data, '$.sandbox'), 0), time
		FROM events WHERE kind = ? AND json_extract(data, '$.workspace') = ? ORDER BY seq LIMIT 1`,
		string(event.WorkspaceCreated), id).Scan(&folder, &ref, &machine, &sandbox, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return w, false, nil
	}
	if err != nil {
		return w, false, fmt.Errorf("store: read workspace: %w", err)
	}
	w.Folder, w.FolderRef, w.Machine = folder.String, ref.String, machine.String
	w.Sandbox = sandbox != 0
	w.Created = time.Unix(0, created).UTC()
	if err := s.workspaceState(&w); err != nil {
		return w, false, err
	}
	return w, true, nil
}

// workspaceState fills HeadTurn, AppliedTurn and DiscardedAt from the
// workspace's turn.changed and its apply/discard history.
func (s *Store) workspaceState(w *Workspace) error {
	// The workspace's latest turn that recorded a changeset.
	var head sql.NullString
	err := s.r.QueryRow(`SELECT tc.turn FROM events tc
		JOIN events ts ON ts.kind = 'turn.started' AND ts.session = tc.session AND ts.turn = tc.turn
		WHERE tc.kind = 'turn.changed' AND `+resolvedWorkspace+` = ?
		ORDER BY tc.seq DESC LIMIT 1`, w.ID).Scan(&head)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: read workspace head: %w", err)
	}
	w.HeadTurn = head.String

	// The turn the latest successful apply went up to. A pre-workspace
	// turn.applied of a migrated workspace is keyed by its session, which is
	// the workspace's id.
	var applied sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.turn') FROM events
		WHERE ((kind = 'workspace.applied' AND json_extract(data, '$.workspace') = ?1)
			OR (kind = 'turn.applied' AND session = ?1))
			AND json_extract(data, '$.applied.result') = 'applied'
		ORDER BY seq DESC LIMIT 1`, w.ID).Scan(&applied)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: read workspace applied: %w", err)
	}
	w.AppliedTurn = applied.String

	// When the latest apply-or-discard action was a discard, the workspace's
	// unapplied changes were dropped and no apply has run since.
	var kind sql.NullString
	var when int64
	err = s.r.QueryRow(`SELECT kind, time FROM events
		WHERE (kind IN ('workspace.applied', 'workspace.discarded') AND json_extract(data, '$.workspace') = ?1)
			OR (kind = 'turn.applied' AND session = ?1)
		ORDER BY seq DESC LIMIT 1`, w.ID).Scan(&kind, &when)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: read workspace action: %w", err)
	}
	if kind.String == string(event.WorkspaceDiscarded) {
		t := time.Unix(0, when).UTC()
		w.DiscardedAt = &t
	}
	return nil
}

// Workspaces lists every workspace, newest first. Low volume: one read per
// workspace for its apply/discard state.
func (s *Store) Workspaces() ([]Workspace, error) {
	rows, err := s.r.Query(`SELECT json_extract(data, '$.workspace') FROM events
		WHERE kind = ? ORDER BY seq DESC`, string(event.WorkspaceCreated))
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list workspaces: %w", err)
		}
		if id.String != "" {
			ids = append(ids, id.String)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	rows.Close()
	out := make([]Workspace, 0, len(ids))
	for _, id := range ids {
		w, found, err := s.Workspace(id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, w)
		}
	}
	return out, nil
}

// CreatedByKey reads the id (at idPath) and request_hash of the noun an
// idempotency key first created: the first event of kind whose idempotency_key
// is key. It is the key-first check a store-backed POST runs before any side
// effect, so a retry does no work. found is false for an empty key or a key no
// event of that kind carries.
func (s *Store) CreatedByKey(kind event.Kind, idPath, key string) (id, hash string, found bool, err error) {
	if key == "" {
		return "", "", false, nil
	}
	var gotID, gotHash sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '`+idPath+`'), json_extract(data, '$.request_hash')
		FROM events WHERE kind = ? AND json_extract(data, '$.idempotency_key') = ? ORDER BY seq LIMIT 1`,
		string(kind), key).Scan(&gotID, &gotHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: created by key: %w", err)
	}
	return gotID.String, gotHash.String, true, nil
}

// SessionWorkspace reads the workspace a session.created bound a session to,
// and its title. found is false when the session has no session.created (a
// session the record knows only through its turns, or an unknown one).
func (s *Store) SessionWorkspace(session string) (workspace, title string, found bool, err error) {
	var ws, t sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.workspace'), json_extract(data, '$.title') FROM events
		WHERE kind = ? AND session = ? ORDER BY seq LIMIT 1`, string(event.SessionCreated), session).Scan(&ws, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read session workspace: %w", err)
	}
	return ws.String, t.String, true, nil
}

// WorkspaceSeed is one existing session's facts for the workspace migration,
// read from the job queue's sessions table by the caller (so the store does not
// import the queue). Folder and FolderRef are left for the migration to read
// from the session's own events when they are empty here.
type WorkspaceSeed struct {
	Session   string
	Sandbox   bool
	Machine   string
	Folder    string
	FolderRef string
}

// MigrateWorkspaces writes a workspace.created for every session that has none,
// using the session's own id as its workspace id so on-disk copies at
// <workdir>/<session> and their checkpoints keep resolving without a rename.
//
// seeds carries the sandbox mode (and machine/folder, when known) of the
// sessions with a job-queue row. A session with no seed -- hook or gateway
// traffic, which the queue never recorded -- gets a direct (sandbox false)
// workspace. folder and folder_ref come from the seed, or from the session's
// own first agent event when the seed left them empty.
//
// It is idempotent: each workspace.created is written with AppendOnce under the
// event id wc_<session>, so a session already migrated is skipped and a re-run
// on every start changes nothing. Nothing is deleted or moved on disk.
func (s *Store) MigrateWorkspaces(seeds []WorkspaceSeed) (int, error) {
	bySession := make(map[string]WorkspaceSeed, len(seeds))
	order := make([]string, 0, len(seeds))
	for _, sd := range seeds {
		if sd.Session == "" {
			continue
		}
		if _, dup := bySession[sd.Session]; !dup {
			order = append(order, sd.Session)
		}
		bySession[sd.Session] = sd
	}

	// Every session the record knows, so hook and gateway sessions (no queue
	// row, no seed) are migrated too.
	rows, err := s.r.Query(`SELECT DISTINCT session FROM events WHERE session <> ''`)
	if err != nil {
		return 0, fmt.Errorf("store: migrate workspaces: %w", err)
	}
	seen := make(map[string]bool, len(order))
	for _, id := range order {
		seen[id] = true
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: migrate workspaces: %w", err)
		}
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("store: migrate workspaces: %w", err)
	}
	rows.Close()

	// Sessions that already name a workspace of their own (a session created
	// explicitly, whose session.created or turn.started carries a non-empty
	// workspace): the migration is only for pre-workspace sessions, so it
	// leaves these alone.
	// Without this guard, such a session -- whose workspace.created is keyed on
	// its own ws_ id, not wc_<session> -- would get a second, phantom
	// workspace.created (workspace = the session's id) on the next start, and
	// the append-only log would keep it.
	explicit := map[string]bool{}
	rows, err = s.r.Query(`SELECT DISTINCT session FROM events
		WHERE kind IN ('session.created', 'turn.started') AND COALESCE(json_extract(data, '$.workspace'), '') <> ''`)
	if err != nil {
		return 0, fmt.Errorf("store: migrate workspaces: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: migrate workspaces: %w", err)
		}
		explicit[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("store: migrate workspaces: %w", err)
	}
	rows.Close()

	n := 0
	for _, id := range order {
		if explicit[id] {
			continue // already has a workspace of its own
		}
		sd := bySession[id] // zero value (sandbox false) when no seed
		// The queue and the folder registry are the authoritative source of a
		// queued session's folder, reference and machine: the caller resolves
		// them and fills the seed. The session's own events are the fallback
		// only when the queue knows nothing (a hook or gateway session, which
		// runs directly, so its event folder is the real one) -- never on top
		// of a queue reference, because an isolated session's event folder is
		// the runner's scratch copy path, not the person's folder.
		folder, ref, machine := sd.Folder, sd.FolderRef, sd.Machine
		if folder == "" && ref == "" {
			folder, ref, err = s.sessionFolder(id)
			if err != nil {
				return n, err
			}
		}
		sandbox := sd.Sandbox
		e := event.Event{
			ID:        "wc_" + id,
			Kind:      event.WorkspaceCreated,
			Time:      time.Now().UTC(),
			Workspace: id,
			Folder:    folder,
			FolderRef: ref,
			Machine:   machine,
			Sandbox:   &sandbox,
		}
		stored, err := s.AppendOnce(e)
		if err != nil {
			return n, fmt.Errorf("store: migrate workspace %s: %w", id, err)
		}
		if stored {
			n++
		}
	}
	return n, nil
}

// sessionFolder reads a session's folder and folder reference from its latest
// agent event (turn.started, session.opened or session.registered).
func (s *Store) sessionFolder(session string) (folder, ref string, err error) {
	var f, r sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.folder'), json_extract(data, '$.folder_ref') FROM events
		WHERE session = ? AND `+agentFrom+` ORDER BY seq DESC LIMIT 1`, session).Scan(&f, &r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("store: read session folder: %w", err)
	}
	return f.String, r.String, nil
}
