// Package folders is the connector's side of a folder's identity: the folders
// this machine owns, each known to the control plane only by its reference.
// A reference is fld_ and 16 hex digits of the SHA-256 of the cleaned path,
// so the same folder always gets the same reference. The path never leaves
// this machine: the file folders.json in the data folder maps each
// reference back to it.
//
// The connector itself has an id (con_ and 16 hex digits), made once and
// kept in the same file. A job that names a folder goes only to the runner
// that reports this connector (queue.Take).
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
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MaxPath bounds a folder's path, in bytes.
const MaxPath = 4096

// ErrNotAbsolute: a folder is named by an absolute path.
var ErrNotAbsolute = errors.New("folders: the folder must be an absolute path, at most 4096 bytes")

// Registry is the folders of one connector. It is safe for concurrent use.
type Registry struct {
	path string

	mu        sync.Mutex
	connector string
	folders   map[string]entry
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

// Open reads the registry at path, or makes one with a new connector id.
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

// Connector is this connector's id.
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

// Path is the folder a reference names, when this connector owns it.
func (r *Registry) Path(ref string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.folders[ref]
	return e.Path, ok
}

// Folder is one folder as the control plane may know it.
type Folder struct {
	Ref  string
	Name string // the last part of its path
}

// List is the folders, newest registered first, at most max.
func (r *Registry) List(max int) []Folder {
	r.mu.Lock()
	type kv struct {
		ref string
		e   entry
	}
	all := make([]kv, 0, len(r.folders))
	for k, v := range r.folders {
		all = append(all, kv{k, v})
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
