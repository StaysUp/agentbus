package api

// POST /api/v0/workspaces with from: a workspace made from another
// workspace's copy, as that copy was at one of its checkpoints
// (plans/DESIGN_one_writer.md section 2). Control only records it: the
// workspace's first turn makes its copy, on the runner that holds the source's
// copy (queue's forkTake, isolation's fork.go). Its folders, machine and mode
// are the source's; its changes stay in the record and are never applied to
// the folders, since its copy did not start from them.

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// WorkspaceFromRequest is the from of POST /api/v0/workspaces.
type WorkspaceFromRequest struct {
	Workspace  string `json:"workspace" doc:"The workspace whose copy the new one is made from (ws_ and 16 hex digits). It must work in a copy (sandbox true) and not be closed. Required."`
	Checkpoint string `json:"checkpoint" doc:"The checkpoint of that copy the new copy starts from: base (the copy as it was first made), or a turn of that workspace that changed files. 1 to 128 characters of A-Z, a-z, 0-9 and . _ -. Required."`
}

// checkpointShape is a checkpoint's name as a copy keeps it: base or a turn
// id, a valid git ref name (isolation's checkpointName and gitRefSafe).
var checkpointShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func checkpointOK(name string) bool {
	return checkpointShape.MatchString(name) && !strings.Contains(name, "..") &&
		!strings.HasSuffix(name, ".") && !strings.HasSuffix(name, ".lock")
}

// Refusals of a workspace made from another's copy. Fixed wording.
const (
	badFrom          = "from must be an object with workspace (ws_...) and checkpoint (base, or a turn of that workspace that changed files)"
	fromWithFolders  = "send from, or folder, folder_ref or folders, not both: a workspace made from another's copy has that workspace's folders"
	fromDirect       = "a workspace made from another's copy works in a copy: sandbox must be true or left out"
	fromNoWorkspace  = "from names no workspace"
	fromNotCopy      = "from names a workspace that works in its folder as it is, so it has no copy to make one from"
	fromClosed       = "from names a workspace that is closed, so it has no copy to make one from"
	fromMachine      = "machine must be the machine of the workspace named in from, or left out"
	fromOutsideReach = "a turn token makes a workspace only from a workspace in its authority"
	fromConflict     = "idempotency_key already made a workspace with other fields; send a new key"
)

// createForkWorkspace serves POST /api/v0/workspaces with from.
func (a *API) createForkWorkspace(w http.ResponseWriter, r *http.Request, body WorkspaceRequest) {
	f := body.From
	switch {
	case body.Folder != "" || body.FolderRef != "" || len(body.Folders) > 0:
		writeError(w, http.StatusBadRequest, fromWithFolders)
		return
	case !sessionID.MatchString(f.Workspace) || !checkpointOK(f.Checkpoint):
		writeError(w, http.StatusBadRequest, badFrom)
		return
	case body.Sandbox != nil && !*body.Sandbox:
		writeError(w, http.StatusBadRequest, fromDirect)
		return
	case body.Machine != "" && !queue.ConnectorShape.MatchString(body.Machine):
		writeError(w, http.StatusBadRequest, "machine must be "+queue.ConnectorShape.String())
		return
	}
	c := callerOf(r)
	src := ids.WorkspaceID(f.Workspace)
	// The new copy holds the source's files, so making it reads the source:
	// a turn token may make one only from a workspace it may read.
	may, err := c.May(run.ActRead, run.InWorkspace(src))
	if err != nil {
		a.internal(w)
		return
	}
	if !may {
		writeError(w, http.StatusForbidden, fromOutsideReach)
		return
	}
	maker := c.origin()
	hash := requestHash(withMaker([]string{"from", f.Workspace, f.Checkpoint, body.Machine}, maker)...)
	id := queue.NewID("ws_")
	a.idempotentCreate(w, event.WorkspaceCreated, "$.workspace", body.IdempotencyKey, hash, fromConflict, id,
		func() (event.Event, bool) {
			ws, found, err := a.cfg.Store.Workspace(src)
			switch {
			case err != nil:
				a.internal(w)
				return event.Event{}, false
			case !found:
				writeError(w, http.StatusNotFound, fromNoWorkspace)
				return event.Event{}, false
			case !ws.Sandbox:
				writeError(w, http.StatusBadRequest, fromNotCopy)
				return event.Event{}, false
			case ws.Closed != nil:
				writeError(w, http.StatusConflict, fromClosed)
				return event.Event{}, false
			case body.Machine != "" && body.Machine != a.machineOf(ws):
				writeError(w, http.StatusBadRequest, fromMachine)
				return event.Event{}, false
			}
			sb := true
			e := event.Event{ID: "wc_" + id, Kind: event.WorkspaceCreated, Time: a.cfg.Now().UTC(),
				Workspace: id, Machine: ws.Machine, Sandbox: &sb, Origin: maker,
				MadeFrom: &event.WorkspaceFrom{Workspace: ws.ID, Checkpoint: f.Checkpoint}}
			if len(ws.Folders) > 1 {
				for _, wf := range ws.Folders {
					e.Folders = append(e.Folders, event.WorkspaceFolderEntry{Name: string(wf.Name), Folder: wf.Folder, FolderRef: wf.FolderRef})
				}
			} else {
				e.Folder, e.FolderRef = ws.Folder, ws.FolderRef
			}
			return e, true
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

// forkNotApplied: an apply or an update of a workspace made from another's
// copy. Its copy started from that copy's checkpoint, not from the folders,
// so its changes have no base in them to apply against or merge with.
const forkNotApplied = "this workspace is made from another workspace's copy: its changes stay in the record and are not applied to the folders, and the folders are not merged into it"
