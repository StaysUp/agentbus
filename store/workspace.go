package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
)

// Workspace is a workspace as its events describe it. A workspace is where
// turns run: it owns the folder, the machine, the sandbox mode (a copy behind a
// wall, or the person's real folder), its checkpoints, Apply and Discard. One
// workspace backs the sessions that run in it: the person's and any it asks,
// which share its one copy.
//
// The core facts (folder, folder_ref, machine, sandbox, created) come from the
// single workspace.created. The apply and discard history comes from the
// workspace's workspace.applied and workspace.discarded, and, for a workspace
// migrated from a pre-workspace session, its old turn.applied events (read the
// same way, since a migrated workspace's id is its session's id). head_turn is
// the workspace's latest turn that recorded a changeset.
type Workspace struct {
	ID        string `json:"workspace"`
	Folder    string `json:"folder,omitempty" doc:"The person's folder the workspace runs in."`
	FolderRef string `json:"folder_ref,omitempty" doc:"The folder's reference (fld_ and 16 hex digits), as its connector issued it."`
	Machine   string `json:"machine,omitempty" doc:"The connector that owns the folder (con_ and 16 hex digits). Absent for this service's own machine."`
	Sandbox   bool   `json:"sandbox" doc:"The mode: true for a copy behind a wall, false for the person's real folder."`
	// Folders are the workspace's folders, in order: one for a workspace made
	// with one folder (also in Folder and FolderRef, as before), several for
	// one made with several (Folder and FolderRef then empty, so nothing reads
	// one folder as the whole workspace). Empty for a workspace with no folder.
	Folders []WorkspaceFolder `json:"folders,omitempty" doc:"The workspace's folders, in order. One for a workspace with one folder, which folder and folder_ref also name; several for a workspace made with several, whose folder and folder_ref are then absent."`
	Created time.Time         `json:"created" doc:"When the workspace was created."`
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
	Closed      *time.Time `json:"closed,omitempty" doc:"When the copy was removed, by Close or by AgentBus putting it away once its work was settled and quiet. Absent once a later turn starts and rebuilds it."`
	// LastAction is the newest outcome of an apply, discard, update or close
	// of the workspace, as the record holds it. The routes answer before the
	// work runs and name the outcome's id; this is where a caller reads it.
	LastAction *event.Event `json:"last_action,omitempty" doc:"The newest outcome of an apply, discard, update or close of the workspace, as the record holds it: workspace.applied (applied, refused with the files and why, or failed), workspace.discarded, workspace.updated (updated, merged or conflict, with the files), workspace.closed, or workspace.failed (the action did nothing, and why). Those routes answer 202 before the work runs and name the event it will be; read your own by that event with GET /api/v0/workspaces/{id}/outcome, since a later action replaces this one. Absent when the workspace has had none."`
}

// WorkspaceFolder is one folder of a workspace.
type WorkspaceFolder struct {
	Name      ids.FolderName `json:"name" doc:"The folder's name: where it sits under the copy's top when the workspace has several folders, and the group its files are listed under. Empty for a workspace with one folder, whose copy is the folder itself."`
	Folder    string         `json:"folder,omitempty" doc:"The folder's path, kept only on the machine that owns it."`
	FolderRef string         `json:"folder_ref,omitempty" doc:"The folder's reference (fld_ and 16 hex digits)."`
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
func (s *Store) Workspace(id ids.WorkspaceID) (w Workspace, found bool, err error) {
	w.ID = string(id)
	var folder, ref, machine, list sql.NullString
	var sandbox int
	var created int64
	err = s.r.QueryRow(`SELECT json_extract(data, '$.folder'), json_extract(data, '$.folder_ref'),
			json_extract(data, '$.machine'), COALESCE(json_extract(data, '$.sandbox'), 0), time,
			json_extract(data, '$.folders')
		FROM events WHERE kind = ? AND json_extract(data, '$.workspace') = ? ORDER BY seq LIMIT 1`,
		string(event.WorkspaceCreated), string(id)).Scan(&folder, &ref, &machine, &sandbox, &created, &list)
	if errors.Is(err, sql.ErrNoRows) {
		return w, false, nil
	}
	if err != nil {
		return w, false, fmt.Errorf("store: read workspace: %w", err)
	}
	w.Folder, w.FolderRef, w.Machine = folder.String, ref.String, machine.String
	if list.String != "" {
		var fs []event.WorkspaceFolderEntry
		if err := json.Unmarshal([]byte(list.String), &fs); err != nil {
			return w, false, fmt.Errorf("store: read workspace folders: %w", err)
		}
		for _, f := range fs {
			w.Folders = append(w.Folders, WorkspaceFolder{Name: ids.FolderName(f.Name), Folder: f.Folder, FolderRef: f.FolderRef})
		}
	} else if w.Folder != "" || w.FolderRef != "" {
		// A workspace with one folder, and every one made before several: a list
		// of one, with no name, as its copy is the folder itself.
		w.Folders = []WorkspaceFolder{{Folder: w.Folder, FolderRef: w.FolderRef}}
	}
	w.Sandbox = sandbox != 0
	w.Created = time.Unix(0, created).UTC()
	if err := s.workspaceState(&w); err != nil {
		return w, false, err
	}
	return w, true, nil
}

// workspaceState fills HeadTurn, AppliedTurn and DiscardedAt from the
// workspace's applied state (Applied), the one fold the queue reads too.
func (s *Store) workspaceState(w *Workspace) error {
	a, err := s.Applied(ids.WorkspaceID(w.ID))
	if err != nil {
		return err
	}
	w.HeadTurn, w.AppliedTurn, w.DiscardedAt = a.Head(), a.AppliedTurn, a.DiscardedAt
	w.Closed = a.ClosedAt
	last, found, err := s.lastAction(w.ID)
	if err != nil {
		return err
	}
	if found {
		w.LastAction = &last
	}
	return nil
}

// An outcome of a workspace's action, by its id, and its latest: each one
// read of events_workspace.
const (
	workspaceOutcomeSQL = `SELECT data FROM events WHERE kind IN ('workspace.applied', 'workspace.discarded',
		'workspace.updated', 'workspace.closed', 'workspace.failed')
		AND json_extract(data, '$.workspace') = ? AND json_extract(data, '$.id') = ? ORDER BY seq LIMIT 1`
	lastActionSQL = `SELECT data FROM events WHERE kind IN ('workspace.applied', 'workspace.discarded',
		'workspace.updated', 'workspace.closed', 'workspace.failed')
		AND json_extract(data, '$.workspace') = ? ORDER BY seq DESC LIMIT 1`
)

// WorkspaceOutcome reads the outcome of one apply, discard, update or close
// of the workspace by its event id, the id the route's 202 named. found is
// false while the action has not run (or the id names none of ws's outcomes).
// last_action holds only the newest, so a caller whose action was followed by
// another reads its own here.
func (s *Store) WorkspaceOutcome(ws, id string) (e event.Event, found bool, err error) {
	var data string
	err = s.r.QueryRow(workspaceOutcomeSQL, ws, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("store: read outcome: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, false, fmt.Errorf("store: read outcome: %w", err)
	}
	return e, true, nil
}

// lastAction reads the workspace's newest outcome of an apply, discard, update
// or close (LastAction). A pre-workspace turn.applied is not one: it names a
// session, and its workspace's routes never queued it.
func (s *Store) lastAction(ws string) (e event.Event, found bool, err error) {
	var data string
	err = s.r.QueryRow(lastActionSQL, ws).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("store: read last action: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return e, false, fmt.Errorf("store: read last action: %w", err)
	}
	return e, true, nil
}

// Workspaces lists every workspace, newest first. Low volume: one read per
// workspace for its apply/discard state.
func (s *Store) Workspaces() ([]Workspace, error) {
	rows, err := s.r.Query(`SELECT json_extract(data, '$.workspace') FROM events
		WHERE kind = ? ORDER BY seq DESC`, string(event.WorkspaceCreated))
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	var wsIDs []ids.WorkspaceID
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list workspaces: %w", err)
		}
		if id.String != "" {
			wsIDs = append(wsIDs, ids.WorkspaceID(id.String))
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	rows.Close()
	out := make([]Workspace, 0, len(wsIDs))
	for _, id := range wsIDs {
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

// WorkspaceOf resolves the workspace a session's turns run in: the workspace
// its session.created or turn.started names, or, for a session migrated from
// before workspaces (no workspace field on its events), the session's own id,
// which the migration records as its workspace. found is false for a session
// the record does not know. It is the single resolver a turn uses to find where
// it runs, so the API and the runner agree with the snapshot's fold.
func (s *Store) WorkspaceOf(session ids.SessionID) (workspace ids.WorkspaceID, found bool, err error) {
	var ws sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.workspace') FROM events
		WHERE session = ? AND kind IN ('session.created', 'turn.started')
			AND COALESCE(json_extract(data, '$.workspace'), '') <> '' ORDER BY seq DESC LIMIT 1`, string(session)).Scan(&ws)
	if err == nil {
		return ids.WorkspaceID(ws.String), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("store: workspace of: %w", err)
	}
	// A session with no workspace field is a migrated one: its workspace is its
	// own id. It is the session only when the record knows the session. This is
	// the one place a session becomes its own workspace id by conversion.
	known, err := s.Known(string(session))
	if err != nil {
		return "", false, err
	}
	if known {
		return ids.WorkspaceID(session), true, nil
	}
	return "", false, nil
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

// SessionAsker reads the session that opened session (its session.created
// origin), set when a turn token created the session so only that token's tree
// may start the session's turn. found is false (asker "") when the session has
// no session.created or it was opened by the main token (no asker). It is the
// one source askChecks uses to tell an asked session from any other session in
// the same workspace.
func (s *Store) SessionAsker(session string) (asker string, found bool, err error) {
	var a sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.origin.session') FROM events
		WHERE kind = ? AND session = ? ORDER BY seq LIMIT 1`, string(event.SessionCreated), session).Scan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read session asker: %w", err)
	}
	return a.String, a.String != "", nil
}

// WorkspaceMaker is the session whose turn made workspace id (its
// workspace.created origin), or "" for a workspace the person made, or none.
func (s *Store) WorkspaceMaker(id ids.WorkspaceID) (session string, err error) {
	var m sql.NullString
	err = s.r.QueryRow(`SELECT json_extract(data, '$.origin.session') FROM events
		WHERE kind = ? AND json_extract(data, '$.workspace') = ? ORDER BY seq LIMIT 1`,
		string(event.WorkspaceCreated), string(id)).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read workspace maker: %w", err)
	}
	return m.String, nil
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

// EnsureDirectWorkspace creates a direct (sandbox false) workspace for a
// session that has none, using the session's own id as its workspace id, the
// way the startup migration does. It is how a gateway or hook session gets its
// workspace at birth: whatever records such a session's first turn creates its
// workspace explicitly, so the one workspace reader never has a missing case
// (his rule, 2026-11-21). Idempotent (AppendOnce under wc_<session>), so a
// later turn of the same session changes nothing.
//
// machine is the connector that owns the folder, so the one workspace record
// carries where a turn runs (folder and machine), the way POST /workspaces
// does, and a multi-runner apply or preview of a direct workspace routes to the
// runner that holds it instead of misrouting on an empty machine. Empty means
// this service's own machine, as it does on every workspace.created.
func (s *Store) EnsureDirectWorkspace(session, folder, folderRef, machine string) error {
	if session == "" {
		return nil
	}
	sb := false
	_, err := s.AppendOnce(event.Event{ID: "wc_" + session, Kind: event.WorkspaceCreated,
		Time: time.Now().UTC(), Workspace: session, Folder: folder, FolderRef: folderRef, Machine: machine, Sandbox: &sb})
	return err
}

// sessionFolder reads a session's folder and folder reference from its latest
// agent event (turn.started, run.started or session.registered).
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
