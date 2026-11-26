package api

// POST /api/v0/workspaces, GET /api/v0/workspaces, GET /api/v0/workspaces/{id}
// and POST /api/v0/sessions. A workspace is where turns run: it owns the
// folder, the machine and the sandbox mode. A session is a label that groups a
// workspace's turns into a conversation. Each is created by its own POST, and a
// turn (POST /api/v0/turns) names the session its workspace comes from.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
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
	badWorkspace = "the body must be one JSON object with folder (an absolute path or ~/...) or folder_ref, and optionally machine, sandbox and idempotency_key"
	badFolder    = "folder must be an absolute path or start with ~/, at most 4096 bytes"
	badSession   = "the body must be one JSON object with workspace, and optionally title and idempotency_key"
)

// WorkspaceRequest is the body of POST /api/v0/workspaces.
type WorkspaceRequest struct {
	Folder    string `json:"folder,omitempty" doc:"An absolute path, or one starting with ~/ for the home of the user that machine's runner runs as, to a directory on the machine named by machine, that the runner can read. Typing it is the grant: that machine's runner checks and registers it, and the workspace keeps only its reference. One of folder or folder_ref is required."`
	FolderRef string `json:"folder_ref,omitempty" doc:"A folder's reference (fld_ and 16 hex digits), from GET /api/v0/folders, in place of folder."`
	Machine   string `json:"machine,omitempty" doc:"The machine that holds the folder (con_ and 16 hex digits). With folder: the machine whose runner checks it. Absent: this service's own machine, or the only one online."`
	// Folders, in place of folder and folder_ref: a workspace with several
	// folders, all on one machine. Its copy holds each folder side by side
	// under one top, named by the folder's last path part.
	Folders []WorkspaceFolderRequest `json:"folders,omitempty" doc:"In place of folder and folder_ref: the folders of a workspace with several, 1 to 8, all on the machine named by machine. The workspace keeps each folder in order with a name: the last part of its path, in letters, digits, . _ and - (any other character becomes -), at most 64 bytes, and -2, -3 for a second of the same name. Its turns run at the top of one copy that holds each folder side by side under its name, and their changes name each file by its folder's name and its path inside the folder. Needs sandbox true."`
	Sandbox *bool                    `json:"sandbox,omitempty" doc:"True (the default): turns run in a copy of the folder behind a wall, and their changes are applied on request. False: turns run in the folder as it is."`
	// IdempotencyKey makes a retry safe: the same key returns the same workspace.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"Optional, 1 to 128 characters of A-Z, a-z, 0-9 and . _ : -. A retry with the same key returns the workspace the key first created and makes nothing new."`
}

// WorkspaceFolderRequest is one folder of POST /api/v0/workspaces' folders.
type WorkspaceFolderRequest struct {
	Folder    string `json:"folder,omitempty" doc:"An absolute path, or one starting with ~/, on the workspace's machine. One of folder or folder_ref is required."`
	FolderRef string `json:"folder_ref,omitempty" doc:"A folder's reference (fld_ and 16 hex digits), from GET /api/v0/folders, in place of folder."`
}

// MaxWorkspaceFolders bounds a workspace's folders.
const MaxWorkspaceFolders = 8

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
	if len(body.Folders) > 0 {
		if body.Folder != "" || body.FolderRef != "" {
			writeError(w, http.StatusBadRequest, "send folders, or folder or folder_ref, not both")
			return
		}
		if len(body.Folders) == 1 {
			// One folder is today's workspace, by whichever field it came in.
			body.Folder, body.FolderRef, body.Folders = body.Folders[0].Folder, body.Folders[0].FolderRef, nil
		}
	}
	// An agent's turn token makes a workspace only within its own authority,
	// checked here, once, for one folder or several; the workspace records the
	// turn that made it, which is how its session later reaches it.
	c := callerOf(r)
	if !c.limits.makes(a, w, &body) {
		return
	}
	maker := c.origin()
	if len(body.Folders) > 0 {
		a.createMultiWorkspace(w, r, body, maker)
		return
	}
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
	hash := requestHash(withMaker([]string{body.Folder, body.FolderRef, body.Machine, strconv.FormatBool(sandbox)}, maker)...)
	id := queue.NewID("ws_")
	a.idempotentCreate(w, event.WorkspaceCreated, "$.workspace", body.IdempotencyKey, hash, workspaceConflict, id,
		func() (event.Event, bool) {
			ref, conn := body.FolderRef, body.Machine
			if body.Folder != "" {
				if a.cfg.Queue == nil {
					writeError(w, http.StatusNotFound, noTurns)
					return event.Event{}, false
				}
				if !typedFolderShape(body.Folder) {
					writeError(w, http.StatusBadRequest, badFolder)
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
				FolderRef: ref, Machine: conn, Sandbox: &sb, Origin: maker}, true
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

// Refusals of a workspace an agent's turn token may not make. Fixed wording.
const (
	agentDirectRefused  = "an agent working in a copy may make only a workspace with sandbox true, a copy behind a wall: a workspace that works in a folder as it is would reach past its own"
	agentNoWorkspace    = "a turn token has no workspace, so it has no folders to make a workspace of"
	agentMachineRefused = "an agent makes workspaces only on its own workspace's machine"
	agentPathRefused    = "an agent working in a copy names folders by folder_ref, from GET /api/v0/workspaces/{id} of its own workspace, never by path"
	agentFolderRefused  = "an agent working in a copy may make a workspace only of folders its own workspace holds"
	// turnWorkspaceRefused: a session opened in a workspace outside the reach.
	turnWorkspaceRefused = "a turn token opens sessions only in the workspaces in its authority: its machine's when its own workspace works in the folder as it is, else its own workspace and the workspaces its session made"
)

// workspaceAuthority checks that the workspace body asks for is within g's
// reach (limit 1 of O2.3), and has written the 403 when it is not. The new
// workspace is on the machine of g's own workspace (machine is set to it when
// absent), and each folder is one g can already reach. A Direct workspace has
// no wall and reaches its whole machine, so any workspace of that machine,
// Direct too, adds no power (O2.3 replaced "copies only", his call
// 2026-11-24). A copy reaches only its own folders: what it makes is a copy, it
// names folders by folder_ref and each must be one of its own; a path is
// refused before any runner reads it, so a walled agent cannot probe the
// machine's disk. A workspace a copy makes is a copy whose folders are within
// its maker's, so a chain of makes never reaches further than the first.
func (a *API) workspaceAuthority(w http.ResponseWriter, g run.Grant, body *WorkspaceRequest) bool {
	refuse := func(msg string) bool {
		writeError(w, http.StatusForbidden, msg)
		return false
	}
	ownID, found, err := a.workspaceOf(g.Session)
	if err != nil {
		a.internal(w)
		return false
	}
	if !found {
		return refuse(agentNoWorkspace)
	}
	own, found, err := a.cfg.Store.Workspace(ownID)
	if err != nil {
		a.internal(w)
		return false
	}
	if !found {
		return refuse(agentNoWorkspace)
	}
	machine := a.machineOf(own)
	switch body.Machine {
	case "":
		body.Machine = machine
	case machine:
	default:
		return refuse(agentMachineRefused)
	}
	if !own.Sandbox {
		return true
	}
	if body.Sandbox != nil && !*body.Sandbox {
		return refuse(agentDirectRefused)
	}
	holds := map[string]bool{}
	for _, f := range own.Folders {
		if f.FolderRef != "" {
			holds[f.FolderRef] = true
		}
	}
	asked := body.Folders
	if len(asked) == 0 {
		asked = []WorkspaceFolderRequest{{Folder: body.Folder, FolderRef: body.FolderRef}}
	}
	for _, f := range asked {
		if f.Folder != "" {
			return refuse(agentPathRefused)
		}
		if !holds[f.FolderRef] {
			return refuse(agentFolderRefused)
		}
	}
	return true
}

// withMaker adds the session of the turn that makes a workspace to its request
// hash's parts: the same idempotency_key sent by another maker is another
// request, never the first maker's workspace handed back. The person's
// requests hash as they always have.
func withMaker(parts []string, o *event.Origin) []string {
	if o == nil {
		return parts
	}
	return append(parts, "maker", o.Session)
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

// DiscardState is the state of a workspace discard (POST
// /api/v0/workspaces/{id}/discard): the copy's unapplied changes dropped.
type DiscardState struct {
	State        string   `json:"state,omitempty" doc:"queued, running, discarded, or failed (error says why)."`
	Already      bool     `json:"already,omitempty" doc:"Nothing was unapplied, so nothing was dropped."`
	DroppedTurns []string `json:"dropped_turns,omitempty" doc:"The turns whose file changes the discard dropped, oldest first. Their conversation stays."`
	Error        string   `json:"error,omitempty" doc:"Why the discard failed."`
}

// discardStateOf reads a discard job's state and the HTTP status that reports
// it: 200 discarded, 202 while it waits behind a running turn, and the runner's
// own status for a failure (400 when the workspace's backend has no discard yet,
// else 500).
func (a *API) discardStateOf(j queue.Job) (DiscardState, int) {
	switch {
	case j.State == queue.Queued:
		return DiscardState{State: "queued"}, http.StatusAccepted
	case j.Ended == nil:
		return DiscardState{State: "running"}, http.StatusAccepted
	case j.State == queue.Finished:
		return DiscardState{State: "discarded", DroppedTurns: j.Links.AppliesTurns}, http.StatusOK
	default:
		st := DiscardState{State: "failed", Error: "the discard did not finish"}
		status := http.StatusInternalServerError
		if r, _ := queue.ReadResult(j.Result); r.Refused != "" {
			st.Error = r.Refused
			if r.Status != 0 {
				status = r.Status
			}
		}
		return st, status
	}
}

// discardWorkspace serves POST /api/v0/workspaces/{id}/discard: drop the
// workspace's unapplied changes, resetting its sandbox copy to the last applied
// state (or base). The person's folder is never touched.
func (a *API) discardWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	wsID := ids.WorkspaceID(id)
	ws, found, err := a.cfg.Store.Workspace(wsID)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	}
	if !ws.Sandbox {
		writeError(w, http.StatusBadRequest, "this session works directly in your folder; only a sandbox copy can be discarded")
		return
	}
	j, created, err := a.cfg.Queue.Discard(queue.DefaultTeam, wsID, ws.Machine, askedBy(r))
	var removed *queue.RunnerRemovedError
	switch {
	case errors.Is(err, queue.ErrAllApplied):
		writeJSON(w, http.StatusOK, DiscardState{Already: true})
		return
	case errors.Is(err, queue.ErrApplyBusy):
		writeJSON(w, http.StatusConflict, DiscardState{State: "failed", Error: "an apply or discard of this workspace is still running or waiting; wait for it to end before discarding"})
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusBadRequest, "this workspace is closed, so its copy cannot be discarded")
		return
	case errors.Is(err, queue.ErrNotApplicable):
		writeError(w, http.StatusBadRequest, "this workspace's copy cannot be reset to its last applied state")
		return
	case errors.As(err, &removed):
		writeError(w, http.StatusServiceUnavailable, "the runner holding this workspace's copy was removed; nothing was discarded")
		return
	case errors.Is(err, queue.ErrMachineOffline):
		writeError(w, http.StatusServiceUnavailable, "the runner holding this workspace's copy has not been seen for a minute; nothing was discarded")
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	_ = created
	// The discard waits behind a running turn in the workspace lane; await it.
	if j.Ended == nil {
		if j, err = a.awaitEnd(r.Context(), j); err != nil {
			if r.Context().Err() == nil {
				a.internal(w)
			}
			return
		}
	}
	st, status := a.discardStateOf(j)
	writeJSON(w, status, st)
}

// CloseState is the state of a workspace close (POST
// /api/v0/workspaces/{id}/close): the copy removed to reclaim disk.
type CloseState struct {
	State   string `json:"state,omitempty" doc:"queued, running, closed, or failed (error says why)."`
	Already bool   `json:"already,omitempty" doc:"The workspace was already closed, so nothing was removed."`
	Error   string `json:"error,omitempty" doc:"Why the close failed."`
}

// closeStateOf reads a close job's state and the HTTP status that reports it:
// 200 closed, 202 while it is queued or running, and the runner's own status
// for a failure (else 500).
func (a *API) closeStateOf(j queue.Job) (CloseState, int) {
	switch {
	case j.State == queue.Queued:
		return CloseState{State: "queued"}, http.StatusAccepted
	case j.Ended == nil:
		return CloseState{State: "running"}, http.StatusAccepted
	case j.State == queue.Finished:
		return CloseState{State: "closed"}, http.StatusOK
	default:
		st := CloseState{State: "failed", Error: "the close did not finish"}
		status := http.StatusInternalServerError
		if r, _ := queue.ReadResult(j.Result); r.Refused != "" {
			st.Error = r.Refused
			if r.Status != 0 {
				status = r.Status
			}
		}
		return st, status
	}
}

// closeWorkspace serves POST /api/v0/workspaces/{id}/close: remove the
// workspace's copy to reclaim its disk. The person's folder is never touched,
// and the next turn rebuilds a fresh copy. Changes not applied when the close
// runs are dropped with the copy; the cockpit signals unapplied work so the
// person can apply or discard on the session first.
func (a *API) closeWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	wsID := ids.WorkspaceID(id)
	ws, found, err := a.cfg.Store.Workspace(wsID)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	}
	if !ws.Sandbox {
		writeError(w, http.StatusBadRequest, "this session works directly in your folder; only a sandbox copy can be closed")
		return
	}
	j, created, err := a.cfg.Queue.CloseWorkspace(queue.DefaultTeam, wsID, ws.Machine, askedBy(r))
	var removed *queue.RunnerRemovedError
	switch {
	case errors.Is(err, queue.ErrApplyBusy):
		writeJSON(w, http.StatusConflict, CloseState{State: "failed", Error: "an apply, discard, update or close of this workspace is still running or waiting; wait for it to end before closing"})
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeJSON(w, http.StatusOK, CloseState{State: "closed", Already: true})
		return
	case errors.As(err, &removed):
		writeError(w, http.StatusServiceUnavailable, "the runner holding this workspace's copy was removed; nothing was closed")
		return
	case errors.Is(err, queue.ErrMachineOffline):
		writeError(w, http.StatusServiceUnavailable, "the runner holding this workspace's copy has not been seen for a minute; nothing was closed")
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	_ = created
	// The close waits behind any queued lane job; await it.
	if j.Ended == nil {
		if j, err = a.awaitEnd(r.Context(), j); err != nil {
			if r.Context().Err() == nil {
				a.internal(w)
			}
			return
		}
	}
	st, status := a.closeStateOf(j)
	writeJSON(w, status, st)
}

// UpdateState is the state of a workspace update (POST
// /api/v0/workspaces/{id}/update): the person's folder merged into the copy.
type UpdateState struct {
	State       string   `json:"state,omitempty" doc:"queued, running, updated (the folder was merged in), conflict (the files in conflicts changed on both sides; nothing changed), or failed (error says why)."`
	Updated     bool     `json:"updated,omitempty" doc:"The folder was merged into the copy; the next apply writes only the session's work."`
	Conflicts   []string `json:"conflicts,omitempty" doc:"On a conflict: the files that changed on both sides, sorted. Nothing changed; ask the agent to resolve them in a turn."`
	Unresolved  []string `json:"unresolved,omitempty" doc:"On a merge (state merged): the files that changed on both sides, sorted. The copy now holds each with both sides between conflict markers, for the agent to combine in its next turn; an apply refuses any file that still holds them."`
	Unmergeable []string `json:"unmergeable,omitempty" doc:"On a merge that changed nothing (state conflict): the conflicting files conflict markers cannot show -- not text, a link, or removed on one side -- sorted. error says so in a sentence. The agent can still be asked to combine them in a turn."`
	Error       string   `json:"error,omitempty" doc:"Why the update failed, or (a merge that changed nothing) why it could not merge."`
}

// UpdateRequest is the optional body of POST /api/v0/workspaces/{id}/update.
type UpdateRequest struct {
	Mode string `json:"mode,omitempty" doc:"Absent or update: a conflict changes nothing. merge: a conflict leaves the conflicting files in the copy with both sides between conflict markers and moves the folder baseline, so the agent's next turn combines them and Apply then writes only the session's work. A merge is refused with 409 while a turn of the workspace runs or waits."`
}

// updateStateOf reads an update job's state and the HTTP status that reports it:
// 200 for a finished update (updated or conflict, both normal outcomes), 202
// while it waits behind a running turn, and the runner's own status for a real
// failure (400 when the workspace's backend has no update, else 500).
func (a *API) updateStateOf(j queue.Job) (UpdateState, int) {
	switch {
	case j.State == queue.Queued:
		return UpdateState{State: "queued"}, http.StatusAccepted
	case j.Ended == nil:
		return UpdateState{State: "running"}, http.StatusAccepted
	case j.State == queue.Finished:
		r, _ := queue.ReadResult(j.Result)
		if r.Update.Clean() && len(r.Update.Unresolved) > 0 {
			return UpdateState{State: "merged", Updated: true, Unresolved: r.Update.Unresolved}, http.StatusOK
		}
		if r.Update.Clean() {
			return UpdateState{State: "updated", Updated: true}, http.StatusOK
		}
		st := UpdateState{State: "conflict"}
		if r.Update != nil {
			st.Conflicts = r.Update.Conflicts
			if len(r.Update.Unmergeable) > 0 {
				st.Unmergeable = r.Update.Unmergeable
				st.Error = "some files cannot be merged with conflict markers (not text, a link, or removed on one side): " +
					strings.Join(r.Update.Unmergeable, ", ") + "; nothing changed"
			}
		}
		return st, http.StatusOK
	default:
		st := UpdateState{State: "failed", Error: "the update did not finish"}
		status := http.StatusInternalServerError
		if r, _ := queue.ReadResult(j.Result); r.Refused != "" {
			st.Error = r.Refused
			if r.Status != 0 {
				status = r.Status
			}
		}
		return st, status
	}
}

// updateWorkspace serves POST /api/v0/workspaces/{id}/update: merge the person's
// folder as it is now into the sandbox copy, so the session's work sits on top
// and the next apply writes only that work. The person's folder is never
// touched.
func (a *API) updateWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	wsID := ids.WorkspaceID(id)
	ws, found, err := a.cfg.Store.Workspace(wsID)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	}
	if !ws.Sandbox {
		writeError(w, http.StatusBadRequest, "this session works directly in your folder; there is nothing to update into a copy")
		return
	}
	body, ok := decodeOptional[UpdateRequest](w, r, "the body must be a JSON object with an optional mode (update or merge), or empty")
	if !ok {
		return
	}
	if body.Mode != "" && body.Mode != "update" && body.Mode != "merge" {
		writeError(w, http.StatusBadRequest, "mode must be update or merge")
		return
	}
	j, created, err := a.cfg.Queue.Update(queue.DefaultTeam, wsID, ws.Machine, askedBy(r), body.Mode == "merge")
	var removed *queue.RunnerRemovedError
	switch {
	case errors.Is(err, queue.ErrApplyBusy):
		writeJSON(w, http.StatusConflict, UpdateState{State: "failed", Error: "an apply, discard or update of this workspace is still running or waiting; wait for it to end before updating"})
		return
	case errors.Is(err, queue.ErrTurnBusy):
		writeJSON(w, http.StatusConflict, UpdateState{State: "failed", Error: "a turn of this workspace is running or waiting; merge once it ends"})
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusBadRequest, "this workspace is closed, so its copy cannot be updated")
		return
	case errors.Is(err, queue.ErrNotApplicable):
		writeError(w, http.StatusBadRequest, "this workspace's copy cannot be updated from its folder")
		return
	case errors.As(err, &removed):
		writeError(w, http.StatusServiceUnavailable, "the runner holding this workspace's copy was removed; nothing was updated")
		return
	case errors.Is(err, queue.ErrMachineOffline):
		writeError(w, http.StatusServiceUnavailable, "the runner holding this workspace's copy has not been seen for a minute; nothing was updated")
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	_ = created
	// The update waits behind a running turn in the workspace lane; await it.
	if j.Ended == nil {
		if j, err = a.awaitEnd(r.Context(), j); err != nil {
			if r.Context().Err() == nil {
				a.internal(w)
			}
			return
		}
	}
	st, status := a.updateStateOf(j)
	writeJSON(w, status, st)
}

// ApplyRequest is the optional body of POST /api/v0/workspaces/{id}/apply.
type ApplyRequest struct {
	Turn string `json:"turn,omitempty" doc:"Apply the workspace's changes up to this turn (it and every earlier unapplied turn). Absent, or an empty body: up to the workspace's head turn, its latest with changes. A turn of another workspace is refused with 404."`
}

// applyNotSandbox: a direct workspace has nothing to apply.
const applyNotSandbox = "this workspace works in your folder as it is, not in an isolated copy, so its changes are already there: there is nothing to apply"

// applyWorkspace serves POST /api/v0/workspaces/{id}/apply: put the workspace's
// unapplied changes into the person's folder, up to the body's turn or the
// workspace's head turn. Main token, or a turn token for its own workspace when
// Agents may apply is on.
func (a *API) applyWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	wsID := ids.WorkspaceID(id)
	ws, found, err := a.cfg.Store.Workspace(wsID)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	}
	if !ws.Sandbox {
		writeError(w, http.StatusBadRequest, applyNotSandbox)
		return
	}
	body, ok := decodeApplyBody(w, r)
	if !ok {
		return
	}
	// The target turn and every check are resolved in the queue under one lock
	// (ApplyWorkspace): the empty-body head is the record's (store.Applied),
	// the same fold head_turn shows and the preview reads.
	j, created, err := a.cfg.Queue.ApplyWorkspace(queue.DefaultTeam, wsID, body.Turn, askedBy(r))
	switch {
	case errors.Is(err, queue.ErrAllApplied):
		// Nothing the workspace can apply: the folder already holds its changes.
		writeJSON(w, http.StatusOK, ApplyState{State: "applied", Already: true})
		return
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "no turn of this workspace has this id")
		return
	case errors.Is(err, queue.ErrNotApplicable):
		writeError(w, http.StatusBadRequest, a.whyNotApplicable(body.Turn))
		return
	case errors.Is(err, queue.ErrSessionClosed):
		writeError(w, http.StatusBadRequest, applySessionClosed)
		return
	case errors.Is(err, queue.ErrApplyBusy):
		writeError(w, http.StatusConflict, applyBusy)
		return
	case errors.Is(err, queue.ErrMachineOffline):
		writeError(w, http.StatusServiceUnavailable, applyOffline)
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	already := !created && j.Ended != nil
	if j.Ended == nil {
		if j, err = a.awaitEnd(r.Context(), j); err != nil {
			if r.Context().Err() == nil {
				a.internal(w)
			}
			return
		}
	}
	st, err := a.applyStateOf(j)
	if err != nil {
		a.internal(w)
		return
	}
	st.Already = already
	switch st.State {
	case "applied":
		writeJSON(w, http.StatusOK, st)
	case "queued", "running":
		writeJSON(w, http.StatusAccepted, st)
	default:
		writeJSON(w, http.StatusConflict, st)
	}
}

// decodeApplyBody reads the optional apply body: an empty body is no turn, a
// non-empty one must be a JSON object. It writes the 400 and returns false on a
// malformed body.
func decodeApplyBody(w http.ResponseWriter, r *http.Request) (ApplyRequest, bool) {
	return decodeOptional[ApplyRequest](w, r, "the body must be a JSON object with an optional turn, or empty")
}

// decodeOptional reads an optional JSON object body into a T: an empty body is
// the zero T. It writes the 400 with refusal and returns false on a malformed
// body.
func decodeOptional[T any](w http.ResponseWriter, r *http.Request, refusal string) (T, bool) {
	var body T
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body could not be read")
		return body, false
	}
	// A non-empty body must be exactly one JSON object, the same as decodeObject:
	// null decodes into body with no error but is not an object, and a trailing
	// } or ] leaves json.Decoder.More() false, so both are refused here.
	if t := bytes.TrimLeft(data, " \t\r\n"); len(t) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if t[0] != '{' || dec.Decode(&body) != nil || dec.More() || dec.Decode(&struct{}{}) != io.EOF {
			writeError(w, http.StatusBadRequest, refusal)
			return body, false
		}
	}
	return body, true
}

// WorkspacesPage is a page of workspaces.
type WorkspacesPage struct {
	Workspaces []store.Workspace `json:"workspaces" doc:"Every workspace, newest first: for a turn token, those in its authority."`
}

// listWorkspaces serves GET /api/v0/workspaces: the workspaces the caller's
// reach holds, every one for the owner.
func (a *API) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	ws, err := a.cfg.Store.Workspaces()
	if err != nil {
		a.internal(w)
		return
	}
	if c := callerOf(r); !c.everywhere(run.ActRead) {
		kept := make([]store.Workspace, 0, len(ws))
		for _, x := range ws {
			in, err := c.reaches(run.InWorkspace(ids.WorkspaceID(x.ID)))
			if err != nil {
				a.internal(w)
				return
			}
			if in {
				kept = append(kept, x)
			}
		}
		ws = kept
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
	// A turn token creates a session in its own workspace (the ask opens one
	// beside the asker, in the same copy) or in one its session made; the main
	// token names any. The
	// asker is recorded on the session so only the token that opened a session
	// may start its turn (askChecks), now that asks share one workspace.
	c := callerOf(r)
	wsID := ids.WorkspaceID(body.Workspace)
	if wsID == "" {
		// The grant's home when the body names none: an ask opens its session
		// beside the asker, in its own workspace.
		home, ok := c.limits.home(a, w)
		if !ok {
			return
		}
		wsID = home
	}
	if wsID != "" {
		may, err := c.May(run.ActSessionCreate, run.InWorkspace(wsID))
		if err != nil {
			a.internal(w)
			return
		}
		if !may {
			writeError(w, http.StatusForbidden, turnWorkspaceRefused)
			return
		}
	}
	if !sessionID.MatchString(string(wsID)) {
		writeError(w, http.StatusBadRequest, "workspace must be "+sessionID.String())
		return
	}
	asker := c.origin()
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

// typedFolderShape is whether a typed folder has a usable shape: absolute, or
// ~ or ~/... for the home of the folder's machine. Control only checks the
// shape and passes ~ on as typed: the folder may be on another machine,
// whose runner expands it against its own home (folders.Expand) and says
// whether the folder exists.
func typedFolderShape(f string) bool {
	return len(f) <= MaxFolder && (filepath.IsAbs(f) || f == "~" || strings.HasPrefix(f, "~/"))
}
