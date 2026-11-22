package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

type toolGatedKey struct{}

// filesElsewhere refuses a file tool to a session whose folder is on another
// machine: these routes open files in the process that serves the API, which
// is control, so they would act on control's disk, not the session's machine.
const filesElsewhere = "read_file, list_folder and write_file work only for a session whose folder is on control's own machine; this session's folder is on another machine"

// refuseFilesElsewhere answers w with 409 and says true when g's session has
// its folder on a machine other than control's own (agentbus up's). A call
// without a session (the main token, the turns token) is not refused, and nor
// is one on a service with no queue, which runs every session itself.
func (a *API) refuseFilesElsewhere(w http.ResponseWriter, g *run.Grant) bool {
	if g == nil || g.Session == "" || a.cfg.Queue == nil {
		return false
	}
	_, machine, _, err := a.cfg.Queue.SessionFolder(queue.DefaultTeam, g.Session)
	if err != nil {
		a.internal(w)
		return true
	}
	if own := a.cfg.Folders; own != nil && machine == own.Machine() {
		return false
	}
	writeError(w, http.StatusConflict, filesElsewhere)
	return true
}

// ReadFileRequest is the body of POST /api/v0/files/read.
type ReadFileRequest struct {
	Path string `json:"path" doc:"The absolute path of the file to read."`
}

// ReadFileResponse is the response to POST /api/v0/files/read.
type ReadFileResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
}

// ListFolderRequest is the body of POST /api/v0/files/list.
type ListFolderRequest struct {
	Path string `json:"path" doc:"The absolute path of the folder to list."`
}

// FolderEntry is one entry in a folder.
type FolderEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind" doc:"file or folder"`
	Size int64  `json:"size,omitempty"`
}

// ListFolderResponse is the response to POST /api/v0/files/list.
type ListFolderResponse struct {
	Path    string        `json:"path"`
	Entries []FolderEntry `json:"entries"`
}

// WriteFileRequest is the body of POST /api/v0/files/write.
type WriteFileRequest struct {
	Path    string  `json:"path" doc:"The absolute path of the file to write."`
	Content *string `json:"content" doc:"The file content to write."`
}

// WriteFileResponse is the response to POST /api/v0/files/write.
type WriteFileResponse struct {
	Path    string `json:"path"`
	Written int    `json:"written"`
}

func resolveSymlinkPath(p string) (string, error) {
	clean := filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved, nil
	}
	return resolveSymlinkPathBounded(clean, 0)
}

func resolveSymlinkPathBounded(p string, hops int) (string, error) {
	if hops > 32 {
		return "", errors.New("too many levels of symbolic links")
	}
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	parts := strings.Split(rest, string(filepath.Separator))
	curr := vol + string(filepath.Separator)
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(curr, part)
		fi, err := os.Lstat(next)
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(curr, target)
			}
			rem := parts[i+1:]
			if len(rem) > 0 {
				target = filepath.Join(append([]string{target}, rem...)...)
			}
			return resolveSymlinkPathBounded(target, hops+1)
		}
		curr = next
	}
	return curr, nil
}

func (a *API) openFolderAccess(w http.ResponseWriter, r *http.Request, path, toolName string) (*os.Root, string, string, bool) {
	g := grantOf(r)
	if a.refuseFilesElsewhere(w, g) {
		return nil, "", "", false
	}
	resolved, err := resolveSymlinkPath(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path: "+err.Error())
		return nil, "", "", false
	}
	if g == nil || g.Session == "" {
		root, err := os.OpenRoot("/")
		if err != nil {
			a.internal(w)
			return nil, "", "", false
		}
		rel := strings.TrimPrefix(resolved, "/")
		if rel == "" {
			rel = "."
		}
		return root, rel, "/", true
	}

	grantFolder := ""
	if gf, ok, err := a.cfg.Store.MatchingFolderGrant(g.Session, resolved); err == nil && ok {
		grantFolder = gf
	} else if approved, ok := r.Context().Value(toolGatedKey{}).(string); ok {
		if approved == "" || approved == "*" {
			writeError(w, http.StatusForbidden, "folder access not granted for this session: "+path)
			return nil, "", "", false
		}
		if approved == resolved {
			if toolName == "list_folder" {
				grantFolder = resolved
			} else {
				dir := filepath.Dir(resolved)
				for dir != "/" && dir != "." {
					if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
						break
					}
					parent := filepath.Dir(dir)
					if parent == dir {
						break
					}
					dir = parent
				}
				grantFolder = dir
			}
		} else {
			writeError(w, http.StatusForbidden, "path changed to ungranted target while approval was pending: "+path)
			return nil, "", "", false
		}
	} else {
		writeError(w, http.StatusForbidden, "folder access not granted for this session: "+path)
		return nil, "", "", false
	}

	rules, err := a.toolRules()
	if err == nil {
		dec, rule := gateway.DecideTool(rules, toolName)
		if dec == gateway.Refuse {
			writeError(w, http.StatusForbidden, "tool forbidden by policy: "+rule)
			return nil, "", "", false
		}
	}

	if toolName == "write_file" {
		if fi, err := os.Stat(grantFolder); err != nil || !fi.IsDir() {
			dir := grantFolder
			for dir != "/" && dir != "." {
				if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
			canDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				canDir = dir
			}
			if ancRoot, err := os.OpenRoot(canDir); err == nil {
				relGrant, err := filepath.Rel(canDir, grantFolder)
				if err == nil && relGrant != ".." && !strings.HasPrefix(relGrant, ".."+string(filepath.Separator)) {
					_ = ancRoot.MkdirAll(relGrant, 0o755)
				}
				ancRoot.Close()
			}
		}
	}

	canonicalGrant, err := filepath.EvalSymlinks(grantFolder)
	if err != nil {
		canonicalGrant = grantFolder
	}
	root, err := os.OpenRoot(canonicalGrant)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot access folder %s: %v", grantFolder, err))
		return nil, "", "", false
	}
	if rootF, err := root.Open("."); err == nil {
		defer rootF.Close()
		if actualRootPath, err := fdPath(rootF); err == nil && actualRootPath != "" && grantFolder != "/" {
			if !store.IsInside(actualRootPath, grantFolder) {
				root.Close()
				writeError(w, http.StatusForbidden, "folder was replaced by ungranted target: "+path)
				return nil, "", "", false
			}
		}
	}
	rel, err := filepath.Rel(canonicalGrant, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		root.Close()
		writeError(w, http.StatusForbidden, "path is outside granted folder: "+path)
		return nil, "", "", false
	}
	if rel == "" {
		rel = "."
	}
	return root, rel, grantFolder, true
}

// resolveFilePath turns p into a clean absolute path, expanding ~ to user home.
func resolveFilePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		} else {
			return "", errors.New("path must start with / or ~/")
		}
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be an absolute path")
	}
	return p, nil
}

// POST /api/v0/files/read
func (a *API) readFile(w http.ResponseWriter, r *http.Request) {
	var body ReadFileRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with path")
		return
	}
	path, err := resolveFilePath(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	root, rel, grantFolder, ok := a.openFolderAccess(w, r, path, "read_file")
	if !ok {
		return
	}
	defer root.Close()

	if fi, err := root.Lstat(rel); err == nil && !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "path is not a regular file: "+path)
		return
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "file not found: "+path)
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot read file %s: %v", path, err))
		return
	}
	defer f.Close()

	if actualPath, err := fdPath(f); err == nil && actualPath != "" && grantFolder != "/" {
		if !store.IsInside(actualPath, grantFolder) {
			writeError(w, http.StatusForbidden, "path redirects outside granted folder: "+path)
			return
		}
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "path is not a regular file: "+path)
		return
	}
	const maxFileSize = 10 * 1024 * 1024
	if info.Size() > maxFileSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("file is too large to read (%d bytes, max %d): %s", info.Size(), maxFileSize, path))
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read file %s: %v", path, err))
		return
	}
	if int64(len(data)) > maxFileSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("file is too large to read: %s", path))
		return
	}
	writeJSON(w, http.StatusOK, ReadFileResponse{
		Path:    path,
		Content: string(data),
		Size:    int64(len(data)),
	})
}

// POST /api/v0/files/list
func (a *API) listFolder(w http.ResponseWriter, r *http.Request) {
	var body ListFolderRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with path")
		return
	}
	path, err := resolveFilePath(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	root, rel, grantFolder, ok := a.openFolderAccess(w, r, path, "list_folder")
	if !ok {
		return
	}
	defer root.Close()

	if rel != "." {
		if fi, err := root.Lstat(rel); err == nil && !fi.IsDir() {
			writeError(w, http.StatusBadRequest, "path is a file; use read_file: "+path)
			return
		}
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "folder not found: "+path)
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot list folder %s: %v", path, err))
		return
	}
	defer f.Close()

	if actualPath, err := fdPath(f); err == nil && actualPath != "" && grantFolder != "/" {
		if !store.IsInside(actualPath, grantFolder) {
			writeError(w, http.StatusForbidden, "path redirects outside granted folder: "+path)
			return
		}
	}
	info, err := f.Stat()
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a file; use read_file: "+path)
		return
	}
	des, err := f.ReadDir(-1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list folder %s: %v", path, err))
		return
	}
	entries := make([]FolderEntry, 0, len(des))
	for _, de := range des {
		kind := "file"
		if de.IsDir() {
			kind = "folder"
		}
		var size int64
		if fi, err := de.Info(); err == nil && !de.IsDir() {
			size = fi.Size()
		}
		entries = append(entries, FolderEntry{
			Name: de.Name(),
			Kind: kind,
			Size: size,
		})
	}
	writeJSON(w, http.StatusOK, ListFolderResponse{
		Path:    path,
		Entries: entries,
	})
}

// POST /api/v0/files/write
func (a *API) writeFile(w http.ResponseWriter, r *http.Request) {
	var body WriteFileRequest
	if !decodeObject(r, &body) || body.Content == nil {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object with path and content")
		return
	}
	path, err := resolveFilePath(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	root, rel, grantFolder, ok := a.openFolderAccess(w, r, path, "write_file")
	if !ok {
		return
	}
	defer root.Close()

	content := *body.Content
	relDir := filepath.Dir(rel)
	if relDir != "." && relDir != "" {
		if err := root.MkdirAll(relDir, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create directory %s: %v", relDir, err))
			return
		}
	}
	if fi, err := root.Lstat(rel); err == nil && !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "path is not a regular file: "+path)
		return
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write file %s: %v", path, err))
		return
	}
	defer f.Close()

	if actualPath, err := fdPath(f); err == nil && actualPath != "" && grantFolder != "/" {
		if !store.IsInside(actualPath, grantFolder) {
			writeError(w, http.StatusForbidden, "path redirects outside granted folder: "+path)
			return
		}
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "path is not a regular file: "+path)
		return
	}
	if err := f.Truncate(0); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to truncate file %s: %v", path, err))
		return
	}
	if _, err := f.Write([]byte(content)); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write file %s: %v", path, err))
		return
	}
	writeJSON(w, http.StatusOK, WriteFileResponse{
		Path:    path,
		Written: len(content),
	})
}
