package api

// POST /api/v0/workspaces with folders: a workspace with several folders, all
// on one machine. Its copy holds each folder side by side under one top, named
// by the folder's last path part, and the names are stored on the
// workspace.created, never computed again.

import (
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

// createMultiWorkspace serves POST /api/v0/workspaces for a body with two or
// more folders.
func (a *API) createMultiWorkspace(w http.ResponseWriter, r *http.Request, body WorkspaceRequest, maker *event.Origin) {
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, noTurns)
		return
	}
	if len(body.Folders) > MaxWorkspaceFolders {
		writeError(w, http.StatusBadRequest, "a workspace holds at most "+strconv.Itoa(MaxWorkspaceFolders)+" folders")
		return
	}
	// Direct works in the person's folders as they are, with no copy and no
	// wall, so several folders add nothing there (his call, 2026-11-23).
	if body.Sandbox != nil && !*body.Sandbox {
		writeError(w, http.StatusBadRequest, "a workspace with several folders needs sandbox true; a direct workspace has one folder")
		return
	}
	if body.Machine != "" && !queue.ConnectorShape.MatchString(body.Machine) {
		writeError(w, http.StatusBadRequest, "machine must be "+queue.ConnectorShape.String())
		return
	}
	parts := []string{body.Machine, "true"}
	for _, f := range body.Folders {
		if (f.Folder == "") == (f.FolderRef == "") {
			writeError(w, http.StatusBadRequest, "each of folders needs folder or folder_ref, not both and not neither")
			return
		}
		if f.FolderRef != "" && !queue.FolderRefShape.MatchString(f.FolderRef) {
			writeError(w, http.StatusBadRequest, "folder_ref must be "+queue.FolderRefShape.String())
			return
		}
		if f.Folder != "" && !typedFolderShape(f.Folder) {
			writeError(w, http.StatusBadRequest, badFolder)
			return
		}
		parts = append(parts, f.Folder, f.FolderRef)
	}
	hash := requestHash(withMaker(parts, maker)...)
	id := queue.NewID("ws_")
	a.idempotentCreate(w, event.WorkspaceCreated, "$.workspace", body.IdempotencyKey, hash, workspaceConflict, id,
		func() (event.Event, bool) {
			folders, conn, ok := a.resolveFolders(w, r, body)
			if !ok {
				return event.Event{}, false
			}
			sb := true
			return event.Event{ID: "wc_" + id, Kind: event.WorkspaceCreated, Time: a.cfg.Now().UTC(),
				Workspace: id, Machine: conn, Sandbox: &sb, Folders: folders, Origin: maker}, true
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

// resolveFolders turns the body's folders into references on one machine, and
// names each. It writes the refusal and answers ok false when one cannot be
// used.
func (a *API) resolveFolders(w http.ResponseWriter, r *http.Request, body WorkspaceRequest) ([]event.WorkspaceFolderEntry, string, bool) {
	conn := body.Machine
	if conn == "" {
		if own := a.cfg.Folders; own != nil {
			conn = own.Machine()
		}
	}
	var listed []queue.RunnerFolder // read once, when a folder_ref needs it
	out := make([]event.WorkspaceFolderEntry, 0, len(body.Folders))
	seen := map[string]bool{}
	used := map[string]bool{}
	for _, f := range body.Folders {
		ref, name := f.FolderRef, ""
		if f.Folder != "" {
			// The first typed folder fixes the machine when none was named; every
			// later one is checked on that machine, so all are on one.
			got, c, status, msg := a.typedFolder(r.Context(), f.Folder, conn)
			switch {
			case status == 0 && msg != "":
				return nil, "", false // the caller went away
			case status == http.StatusInternalServerError:
				a.internal(w)
				return nil, "", false
			case msg != "":
				writeError(w, status, msg)
				return nil, "", false
			}
			ref, conn = got, c
			name = lastPart(f.Folder)
		} else {
			if conn == "" {
				writeError(w, http.StatusBadRequest, "a folder_ref needs machine, from GET /api/v0/folders")
				return nil, "", false
			}
			if listed == nil {
				var err error
				if listed, err = a.cfg.Queue.Folders(queue.DefaultTeam); err != nil {
					a.internal(w)
					return nil, "", false
				}
			}
			found := false
			for _, l := range listed {
				if l.Ref == ref && l.Machine == conn {
					found, name = true, l.Name
					break
				}
			}
			if !found {
				if p := queue.DisplayFolder(a.cfg.Folders, ref, conn); p != "" {
					found, name = true, lastPart(p)
				}
			}
			if !found {
				writeError(w, http.StatusBadRequest, "folder_ref "+ref+" is not a folder of machine "+conn+"; all of a workspace's folders are on one machine")
				return nil, "", false
			}
		}
		if seen[ref] {
			writeError(w, http.StatusBadRequest, "folders names the same folder twice")
			return nil, "", false
		}
		seen[ref] = true
		display := queue.DisplayFolder(a.cfg.Folders, ref, conn)
		if display != "" {
			name = lastPart(display)
		}
		name = uniqueName(folderName(name), used)
		out = append(out, event.WorkspaceFolderEntry{Name: name, Folder: display, FolderRef: ref})
	}
	return out, conn, true
}

// Places are a workspace's folders as a job names them, when it has several
// (queue.Spec.Folders): one folder goes in FolderRef, as always.
func Places(w store.Workspace) []workspace.Place {
	if len(w.Folders) < 2 {
		return nil
	}
	out := make([]workspace.Place, len(w.Folders))
	for i, f := range w.Folders {
		out[i] = workspace.Place{Name: f.Name, Ref: f.FolderRef}
	}
	return out
}

// ForkOf is where a workspace made from another workspace's copy comes from,
// as a job names it, or nil for a workspace made from its folders.
func ForkOf(w store.Workspace) *queue.Fork {
	if w.From == nil {
		return nil
	}
	return &queue.Fork{Workspace: ids.WorkspaceID(w.From.Workspace), Checkpoint: w.From.Checkpoint}
}

// lastPart is the last part of a folder's path, as typed or as kept: ~ alone is
// the home.
func lastPart(p string) string {
	if p == "~" {
		return "home"
	}
	return path.Base(filepath.ToSlash(filepath.Clean(p)))
}

// folderName makes n a name a folder can sit under in a copy on any disk: one
// path part of letters, digits, '.', '_' and '-' (each other character is a
// '-', so two names that differ only in accents or case form never reach one
// disk as one folder), at most maxFolderName bytes, and not . or .. or .git.
func folderName(n string) string {
	b := []byte{}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b = append(b, byte(r))
		default:
			b = append(b, '-')
		}
		if len(b) == maxFolderName {
			break
		}
	}
	out := string(b)
	if out == "" || strings.Trim(out, ".") == "" || strings.EqualFold(out, ".git") {
		return "folder"
	}
	return out
}

// maxFolderName bounds a folder's name, so a suffix still fits in one path
// part (255 bytes on every disk we copy to).
const maxFolderName = 64

// uniqueName is n, or n-2, n-3 and so on, whichever used does not hold yet,
// matched without case: a Mac's disk does not tell Foo from foo.
func uniqueName(n string, used map[string]bool) string {
	out := n
	for i := 2; used[strings.ToLower(out)]; i++ {
		out = n + "-" + strconv.Itoa(i)
	}
	used[strings.ToLower(out)] = true
	return out
}
