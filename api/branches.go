package api

// POST /api/v0/folders/branches: the branches a workspace may start from
// (WorkspaceRequest.Base). The folder's machine lists them, as it resolves a
// base: control never runs git. A cockpit fills its branch picker with them.

import (
	"net/http"
	"path/filepath"

	"github.com/staysup/agentbus/queue"
)

// BranchesRequest is the body of POST /api/v0/folders/branches.
type BranchesRequest struct {
	Folder    string `json:"folder,omitempty" doc:"An absolute path to a directory on the machine named by machine, as POST /api/v0/workspaces takes it. Typing it is the grant: that machine's runner checks and registers it. One of folder or folder_ref is required."`
	FolderRef string `json:"folder_ref,omitempty" doc:"A folder's reference (fld_ and 16 hex digits), from GET /api/v0/folders, in place of folder."`
	Machine   string `json:"machine,omitempty" doc:"The folder's machine (con_ and 16 hex digits). Absent: this service's own machine, or the only one online."`
	Remote    string `json:"remote,omitempty" doc:"Optional: a remote of the folder's repo, such as origin. Its branches as it has them now are listed, asked over the network with the machine's own git credentials; nothing is fetched. Absent: the repo's own (local) branches."`
}

const badBranches = "the body must be one JSON object with folder (an absolute path) or folder_ref, and optionally machine and remote"

// listBranches serves POST /api/v0/folders/branches.
func (a *API) listBranches(w http.ResponseWriter, r *http.Request) {
	var body BranchesRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badBranches)
		return
	}
	switch {
	case (body.Folder == "") == (body.FolderRef == ""):
		writeError(w, http.StatusBadRequest, "send folder or folder_ref, not both and not neither")
		return
	case body.Folder != "" && (len(body.Folder) > MaxFolder || !filepath.IsAbs(body.Folder)):
		writeError(w, http.StatusBadRequest, "folder must be an absolute path to an existing directory, at most 4096 bytes")
		return
	case body.FolderRef != "" && !queue.FolderRefShape.MatchString(body.FolderRef):
		writeError(w, http.StatusBadRequest, "folder_ref must be "+queue.FolderRefShape.String())
		return
	case body.Machine != "" && !queue.ConnectorShape.MatchString(body.Machine):
		writeError(w, http.StatusBadRequest, "machine must be "+queue.ConnectorShape.String())
		return
	case body.Remote != "" && !remoteShape.MatchString(body.Remote):
		writeError(w, http.StatusBadRequest, badRemote)
		return
	}
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	machine := body.Machine
	if body.FolderRef != "" && machine == "" {
		if own := a.cfg.Folders; own != nil {
			machine = own.Machine()
		} else {
			writeError(w, http.StatusBadRequest, "a folder_ref needs machine, from GET /api/v0/folders")
			return
		}
	}
	res, _, status, msg := a.folderJob(r.Context(), machine,
		queue.FolderCheck{Path: body.Folder, Ref: body.FolderRef, Remote: body.Remote, Branches: true})
	switch {
	case status == 0 && msg != "":
		return // the caller went away
	case status == http.StatusInternalServerError:
		a.internal(w)
		return
	case msg != "":
		writeError(w, status, msg)
		return
	case res.Branches == nil:
		writeError(w, http.StatusServiceUnavailable, "the machine's runner did not list the branches; it may run an older build: update it, and try again")
		return
	}
	writeJSON(w, http.StatusOK, res.Branches)
}
