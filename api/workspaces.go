package api

// POST /api/v0/workspaces, GET /api/v0/workspaces, GET /api/v0/workspaces/{id}
// and POST /api/v0/sessions. A workspace is where turns run: it owns the
// folder, the machine and the sandbox mode. A session is a label that groups a
// workspace's turns into a conversation. Each is created by its own POST, and a
// turn (POST /api/v0/turns) names the session its workspace comes from.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// requestHash is the SHA-256 of a request's fields, stored on the created
// event so a retry with the same idempotency_key and other fields is refused.
func requestHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// idempotentCreate is the one idempotency protocol every store-backed POST
// shares (his rule, 2026-11-21, after two review rounds found each hand-rolled
// copy drifting from POST /api/v0/turns). It is key-first: a key already used
// returns its noun through reply with no side effect (resolve is not called),
// or refuses with 409 (conflict) when the key carries other fields. On a fresh
// key it calls resolve to validate the rest, do any side effect and build the
// event with its id and idPath value set; the event is appended atomically
// under the key, and a concurrent first call that lost the race is reconciled
// the same way. hash is over the request's raw fields; resolve returns ok false
// once it has written its own error, and reply writes the created or found
// noun by id.
func (a *API) idempotentCreate(w http.ResponseWriter, kind event.Kind, idPath, key, hash, conflict, newID string,
	resolve func() (event.Event, bool), reply func(id string)) {
	if key != "" {
		id, hitHash, found, err := a.cfg.Store.CreatedByKey(kind, idPath, key)
		if err != nil {
			a.internal(w)
			return
		}
		if found {
			if hitHash != hash {
				writeError(w, http.StatusConflict, conflict)
				return
			}
			reply(id)
			return
		}
	}
	e, ok := resolve()
	if !ok {
		return
	}
	e.IdempotencyKey, e.RequestHash = key, hash
	existingID, existingHash, stored, err := a.cfg.Store.AppendForKey(e, idPath)
	if err != nil {
		a.internal(w)
		return
	}
	if !stored {
		if existingHash != hash {
			writeError(w, http.StatusConflict, conflict)
			return
		}
		reply(existingID)
		return
	}
	reply(newID)
}

const (
	workspaceConflict = "idempotency_key already made a workspace with other fields; send a new key"
	sessionConflict   = "idempotency_key already made a session with other fields; send a new key"
)

const (
	badWorkspace = "the body must be one JSON object with folder (an absolute path) or folder_ref, and optionally machine, sandbox and idempotency_key"
	badSession   = "the body must be one JSON object with workspace, and optionally title and idempotency_key"
)

// WorkspaceRequest is the body of POST /api/v0/workspaces.
type WorkspaceRequest struct {
	Folder    string `json:"folder,omitempty" doc:"An absolute path to a directory on the machine named by machine, that the runner can read. Typing it is the grant: that machine's runner checks and registers it, and the workspace keeps only its reference. One of folder or folder_ref is required."`
	FolderRef string `json:"folder_ref,omitempty" doc:"A folder's reference (fld_ and 16 hex digits), from GET /api/v0/folders, in place of folder."`
	Machine   string `json:"machine,omitempty" doc:"The machine that holds the folder (con_ and 16 hex digits). With folder: the machine whose runner checks it. Absent: this service's own machine, or the only one online."`
	Sandbox   *bool  `json:"sandbox,omitempty" doc:"True (the default): turns run in a copy of the folder behind a wall, and their changes are applied on request. False: turns run in the folder as it is."`
	// IdempotencyKey makes a retry safe: the same key returns the same workspace.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key returns the workspace the key first created and makes nothing new."`
}

// SessionRequest is the body of POST /api/v0/sessions.
type SessionRequest struct {
	Workspace      string `json:"workspace" doc:"The workspace the session's turns run in (ws_ and 16 hex digits), from POST /api/v0/workspaces. Required."`
	Title          string `json:"title,omitempty" doc:"An optional name for the session, at most 120 characters. GET /api/v0/sessions shows it until a later rename or, without a title, the first message replaces it."`
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key returns the session the key first created and makes nothing new."`
}

// SessionCreated is the answer to POST /api/v0/sessions.
type SessionCreated struct {
	Session   string `json:"session" doc:"The new session's id. Name it as session on POST /api/v0/turns to run its turns."`
	Workspace string `json:"workspace" doc:"The workspace the session's turns run in."`
	Title     string `json:"title,omitempty" doc:"The session's name, when one was given."`
}

// createWorkspace serves POST /api/v0/workspaces.
func (a *API) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var body WorkspaceRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badWorkspace)
		return
	}
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// Cheap validation, before the key check: a bad request is refused whether
	// or not it reuses a key, and at no side effect.
	if (body.Folder == "") == (body.FolderRef == "") {
		writeError(w, http.StatusBadRequest, "send folder or folder_ref, not both and not neither")
		return
	}
	if body.Machine != "" && !queue.ConnectorShape.MatchString(body.Machine) {
		writeError(w, http.StatusBadRequest, "machine must be "+queue.ConnectorShape.String())
		return
	}
	if body.FolderRef != "" && !queue.FolderRefShape.MatchString(body.FolderRef) {
		writeError(w, http.StatusBadRequest, "folder_ref must be "+queue.FolderRefShape.String())
		return
	}
	sandbox := body.Sandbox == nil || *body.Sandbox
	hash := requestHash(body.Folder, body.FolderRef, body.Machine, strconv.FormatBool(sandbox))
	id := queue.NewID("ws_")
	a.idempotentCreate(w, event.WorkspaceCreated, "$.workspace", body.IdempotencyKey, hash, workspaceConflict, id,
		func() (event.Event, bool) {
			ref, conn := body.FolderRef, body.Machine
			if body.Folder != "" {
				if a.cfg.Queue == nil {
					writeError(w, http.StatusNotFound, noTurns)
					return event.Event{}, false
				}
				if len(body.Folder) > MaxFolder || !filepath.IsAbs(body.Folder) {
					writeError(w, http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
					return event.Event{}, false
				}
				var status int
				var msg string
				ref, conn, status, msg = a.typedFolder(r.Context(), body.Folder, body.Machine)
				switch {
				case status == 0 && msg != "":
					return event.Event{}, false // the caller went away
				case status == http.StatusInternalServerError:
					a.internal(w)
					return event.Event{}, false
				case msg != "":
					writeError(w, status, msg)
					return event.Event{}, false
				}
			} else if conn == "" {
				// A folder_ref with no machine runs on this service's own
				// machine, which the folder registry names. A service that runs
				// turns (a queue) but owns no folders -- a server -- cannot name
				// the machine, so it refuses here rather than storing an empty
				// machine that fails apply and changes later; send machine. A
				// bare service with no queue (a test) runs no turns and keeps the
				// empty machine.
				if own := a.cfg.Folders; own != nil {
					conn = own.Machine()
				} else if a.cfg.Queue != nil {
					writeError(w, http.StatusBadRequest, "a folder_ref needs machine, from GET /api/v0/folders")
					return event.Event{}, false
				}
			}
			sb := sandbox
			// The folder path is kept only when this machine owns the folder
			// (DisplayFolder); on a server the reference is the only pointer.
			return event.Event{ID: "wc_" + id, Kind: event.WorkspaceCreated, Time: a.cfg.Now().UTC(),
				Workspace: id, Folder: queue.DisplayFolder(a.cfg.Folders, ref, conn),
				FolderRef: ref, Machine: conn, Sandbox: &sb}, true
		},
		func(wsID string) {
			out, ok, err := a.cfg.Store.Workspace(ids.WorkspaceID(wsID))
			if err != nil || !ok {
				a.internal(w)
				return
			}
			writeJSON(w, http.StatusCreated, out)
		})
}

// getWorkspace serves GET /api/v0/workspaces/{id}.
func (a *API) getWorkspace(w http.ResponseWriter, _ *http.Request, id string) {
	ws, found, err := a.cfg.Store.Workspace(ids.WorkspaceID(id))
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// WorkspacesPage is a page of workspaces.
type WorkspacesPage struct {
	Workspaces []store.Workspace `json:"workspaces" doc:"Every workspace, newest first."`
}

// listWorkspaces serves GET /api/v0/workspaces.
func (a *API) listWorkspaces(w http.ResponseWriter, _ *http.Request) {
	ws, err := a.cfg.Store.Workspaces()
	if err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, WorkspacesPage{Workspaces: ws})
}

// createSession serves POST /api/v0/sessions.
func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	var body SessionRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badSession)
		return
	}
	if msg := checkIdempotencyKey(body.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// A turn token creates a session only in its own workspace (the ask opens
	// one beside the asker, in the same copy); the main token names it. The
	// asker is recorded on the session so only the token that opened a session
	// may start its turn (askChecks), now that asks share one workspace.
	wsID := ids.WorkspaceID(body.Workspace)
	var asker *event.Origin
	if g := grantOf(r); g != nil {
		ws, found, err := a.cfg.Store.WorkspaceOf(ids.SessionID(g.Session))
		if err != nil {
			a.internal(w)
			return
		}
		if !found {
			writeError(w, http.StatusForbidden, "a turn token has no workspace to create a session in")
			return
		}
		wsID = ws
		asker = &event.Origin{Kind: event.OriginAsked, Session: g.Session, Turn: g.Turn}
	} else if !sessionID.MatchString(body.Workspace) {
		writeError(w, http.StatusBadRequest, "workspace must be "+sessionID.String())
		return
	}
	title := ""
	if body.Title != "" {
		var msg string
		if title, msg = normalizeTitle(body.Title); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	// The asker is part of the request: two turn tokens (or a turn token and the
	// main token) reusing one idempotency_key and title in the same workspace are
	// different asks, so the hash separates them (a shared key would otherwise
	// hand the second caller the first's session, with the wrong asker).
	askerSession := ""
	if asker != nil {
		askerSession = asker.Session
	}
	hash := requestHash(string(wsID), title, askerSession)
	id := queue.NewID("")
	a.idempotentCreate(w, event.SessionCreated, "$.session", body.IdempotencyKey, hash, sessionConflict, id,
		func() (event.Event, bool) {
			ws, ok, err := a.cfg.Store.Workspace(wsID)
			if err != nil {
				a.internal(w)
				return event.Event{}, false
			}
			if !ok {
				writeError(w, http.StatusNotFound, "no such workspace")
				return event.Event{}, false
			}
			return event.Event{ID: "sc_" + id, Kind: event.SessionCreated, Time: a.cfg.Now().UTC(),
				Session: id, Workspace: ws.ID, Title: title, Origin: asker}, true
		},
		func(sID string) {
			sWS, sTitle, ok, err := a.cfg.Store.SessionWorkspace(sID)
			if err != nil || !ok {
				a.internal(w)
				return
			}
			writeJSON(w, http.StatusCreated, SessionCreated{Session: sID, Workspace: sWS, Title: sTitle})
		})
}
