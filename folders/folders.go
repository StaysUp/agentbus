// Package folders is the machine's side of a folder's identity: the folders
// this machine's runner holds, each known to the control plane only by its reference.
// A reference is fld_ and 16 hex digits of the SHA-256 of the cleaned path,
// so the same folder always gets the same reference. The path never leaves
// this machine: the file folders.json in the data folder maps each
// reference back to it.
//
// The machine itself has an id (con_ and 16 hex digits, still named
// connector), made once and kept in the same file. A job that names it goes
// only to the runner that reports it (queue.Take), so a session runs on its
// folder's machine. A runner that keeps its data folder keeps the id.
//
// It imports nothing of the module. Control packages never import it
// (cmd/agentbus/layers_test.go): in agentbus up the API reaches it through
// an interface (queue.FolderIssuer) wired by the command.
package folders

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxPath bounds a folder's path, in bytes.
const MaxPath = 4096

// ErrNotAbsolute: a folder is named by an absolute path.
var ErrNotAbsolute = errors.New("folders: the folder must be an absolute path, at most 4096 bytes")

// Why a typed folder cannot be used, one sentence each. None names the path:
// the refusal is stored on the control plane.
var (
	ErrNoFolder   = errors.New("the folder does not exist on the chosen machine")
	ErrNotDir     = errors.New("the folder is a file, not a directory")
	ErrUnreadable = errors.New("the folder cannot be read by the user the machine's runner runs as")
)

// Check says why the folder at path cannot be used, or nil: it must be an
// absolute path to an existing directory this user can read.
func Check(path string) error {
	if len(path) > MaxPath || !filepath.IsAbs(path) {
		return ErrNotAbsolute
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrNoFolder
	case err != nil:
		return ErrUnreadable
	case !fi.IsDir():
		return ErrNotDir
	}
	d, err := os.Open(path)
	if err != nil {
		return ErrUnreadable
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return ErrUnreadable
	}
	return nil
}

// Add checks the folder at path (Check), then registers it (Register): a
// folder a person typed, used from now on by its reference.
func (r *Registry) Add(path string) (string, error) {
	if err := Check(path); err != nil {
		return "", err
	}
	return r.Register(path)
}

// Registry is the folders of one machine. It is safe for concurrent use.
type Registry struct {
	path string

	mu        sync.Mutex
	connector string
	folders   map[string]entry
	copies    []string // the folders that hold session copies (CopiesIn)
}

type entry struct {
	Path string `json:"path"`
	// Used: when it was last registered, so the newest are reported first.
	Used time.Time `json:"used"`
}

type file struct {
	Connector string           `json:"connector"`
	Folders   map[string]entry `json:"folders"`
}

// Open reads the registry at path, or makes one with a new machine id.
func Open(path string) (*Registry, error) {
	r := &Registry{path: path, folders: map[string]entry{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f file
		if json.Unmarshal(b, &f) != nil || !connectorShape(f.Connector) {
			return nil, errors.New("folders: " + filepath.Base(path) + " cannot be read")
		}
		r.connector = f.Connector
		for k, v := range f.Folders {
			r.folders[k] = v
		}
		return r, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, errors.New("folders: " + filepath.Base(path) + " cannot be read")
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	r.connector = "con_" + hex.EncodeToString(raw[:])
	if err := r.save(); err != nil {
		return nil, err
	}
	return r, nil
}

// Connector is this machine's id.
func (r *Registry) Connector() string { return r.connector }

// Ref is the reference of the folder at path. It does not register it.
func Ref(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "fld_" + hex.EncodeToString(sum[:8])
}

// Register keeps path and answers its reference. The folder need not exist
// yet: the turn that names it says so in its own words.
func (r *Registry) Register(path string) (string, error) {
	if len(path) > MaxPath || !filepath.IsAbs(path) {
		return "", ErrNotAbsolute
	}
	path = filepath.Clean(path)
	ref := Ref(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.folders[ref]
	now := time.Now().UTC()
	// The time is written at most once a minute per folder: a busy session
	// does not rewrite the file on every turn.
	if ok && e.Path == path && now.Sub(e.Used) < time.Minute {
		return ref, nil
	}
	r.folders[ref] = entry{Path: path, Used: now}
	if err := r.save(); err != nil {
		if ok {
			r.folders[ref] = e
		} else {
			delete(r.folders, ref)
		}
		return "", err
	}
	return ref, nil
}

// Path is the folder a reference names, when this machine holds it.
func (r *Registry) Path(ref string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.folders[ref]
	return e.Path, ok
}

// Within says the folder at path is the folder ref or inside it, once
// every symbolic link of both is resolved: a link inside the folder that
// leads out of it is not inside. False when ref is not this machine's.
func (r *Registry) Within(path, ref string) bool {
	dir, ok := r.Path(ref)
	if !ok || !filepath.IsAbs(path) {
		return false
	}
	return within(path, dir)
}

// WithinRef is Within for a folder named by its reference.
func (r *Registry) WithinRef(child, ref string) bool {
	p, ok := r.Path(child)
	return ok && r.Within(p, ref)
}

// WithinDir says the folder at path is dir or inside it, once every
// symbolic link of both is resolved.
func (r *Registry) WithinDir(path, dir string) bool {
	return filepath.IsAbs(path) && filepath.IsAbs(dir) && within(path, dir)
}

func within(folder, dir string) bool {
	f, err1 := filepath.EvalSymlinks(folder)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	return f == d || strings.HasPrefix(f, d+string(os.PathSeparator))
}

// CopiesIn names the folders that hold this machine's session copies: the
// data folder's sessions/ and worktrees/. List leaves out each folder inside
// them. A turn may still name one, such as the asked work of a session on a
// worktree, which runs in that session's copy, so it stays registered and a
// job that names it by reference still finds it. It is never one of the
// person's folders to pick.
func (r *Registry) CopiesIn(dirs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.copies = r.copies[:0]
	for _, d := range dirs {
		r.copies = append(r.copies, filepath.Clean(d))
	}
}

// isCopy says path is inside one of dirs, or is a copy made by 0.2 in any
// data folder: worktrees/job_ and 16 hex digits, or a folder inside it.
// 0.3.0's migration registered those (a 0.2 asked turn named the asking
// job's copy), some in a data folder since moved, so its path is not known.
func isCopy(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(path, d+string(os.PathSeparator)) {
			return true
		}
	}
	parts := strings.Split(path, string(os.PathSeparator))
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "worktrees" && oldJobID(parts[i+1]) {
			return true
		}
	}
	return false
}

// oldJobID says s is shaped as a job id: job_ and 16 lowercase hex digits.
func oldJobID(s string) bool {
	if len(s) != 20 || s[:4] != "job_" {
		return false
	}
	for _, c := range s[4:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Folder is one folder as the control plane may know it.
type Folder struct {
	Ref  string
	Name string // the last part of its path
}

// List is the folders, newest registered first, at most max. Session
// copies are left out (CopiesIn).
func (r *Registry) List(max int) []Folder {
	r.mu.Lock()
	type kv struct {
		ref string
		e   entry
	}
	all := make([]kv, 0, len(r.folders))
	for k, v := range r.folders {
		if !isCopy(v.Path, r.copies) {
			all = append(all, kv{k, v})
		}
	}
	r.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		if !all[i].e.Used.Equal(all[j].e.Used) {
			return all[i].e.Used.After(all[j].e.Used)
		}
		return all[i].ref < all[j].ref
	})
	if len(all) > max {
		all = all[:max]
	}
	out := make([]Folder, len(all))
	for i, f := range all {
		name := filepath.Base(f.e.Path)
		if len(name) > 255 {
			name = name[:255]
		}
		out[i] = Folder{Ref: f.ref, Name: name}
	}
	return out
}

// save writes the file with mode 0600, by rename. The lock is held, or the
// registry is not shared yet.
func (r *Registry) save() error {
	b, _ := json.Marshal(file{Connector: r.connector, Folders: r.folders})
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return errors.New("folders: " + filepath.Base(r.path) + " could not be written")
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return errors.New("folders: " + filepath.Base(r.path) + " could not be written")
	}
	return nil
}

func connectorShape(s string) bool {
	if len(s) != 20 || s[:4] != "con_" {
		return false
	}
	_, err := hex.DecodeString(s[4:])
	return err == nil
}
