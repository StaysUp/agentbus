// Package folders is the machine's side of a folder's identity: the folders
// this machine's runner holds, each known to the control plane only by its reference.
// A reference is fld_ and 16 hex digits of the SHA-256 of the cleaned path,
// so the same folder always gets the same reference. The path never leaves
// this machine: the file folders.json in the data folder maps each
// reference back to it.
//
// The machine itself has an id (con_ and 16 hex digits), made once and kept
// in the same file, written under the key "machine". A job that names it goes
// only to the runner that reports it (queue.Take), so a session runs on its
// folder's machine. A runner that keeps its data folder keeps the id.
//
// Two processes of one data folder share the file: agentbus up registers the
// folders a person names, and its runner, a process of its own, reads them.
// Each read takes in what the other wrote (refresh), and each write merges
// with the file under a lock beside it, so neither loses the other's folders.
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
	"syscall"
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
	ErrNoHome     = errors.New("the folder starts with ~, and the home folder of the user the machine's runner runs as is not known")
)

// Expand is path with a leading ~ or ~/ read as home, the home of the user
// this machine's runner runs as: a person types ~/project for a folder on
// that machine, and only that machine knows its home. Any other path comes
// back as it is, for CheckShape to judge; ~name (another user's home) is not
// expanded, so it stays a relative path and is refused there.
func Expand(path, home string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	if home == "" {
		return "", ErrNoHome
	}
	return filepath.Join(home, path[1:]), nil
}

// CheckShape says why path cannot name a folder by its shape alone, or nil:
// it must be an absolute path of at most MaxPath bytes. It touches no
// filesystem, so Register uses it to issue a reference without reading the
// folder; existence and readability are Check's, run on the folder's own
// machine through the folder job. The control plane cannot import this
// package, so it makes the same synchronous shape check inline
// (api/workspaces.go), letting ~ through for this machine's Expand.
func CheckShape(path string) error {
	if len(path) > MaxPath || !filepath.IsAbs(path) {
		return ErrNotAbsolute
	}
	return nil
}

// Check says why the folder at path cannot be used, or nil: it must be an
// absolute path to an existing directory this user can read.
func Check(path string) error {
	if err := CheckShape(path); err != nil {
		return err
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

	mu      sync.Mutex
	machine string
	folders map[string]entry
	copies  []string // the folders that hold session copies (CopiesIn)
	seen    stamp    // the file as last read or written (refresh)
}

// stamp tells whether the file changed since it was last read.
type stamp struct {
	mod  time.Time
	size int64
}

type entry struct {
	Path string `json:"path"`
	// Used: when it was last registered, so the newest are reported first.
	Used time.Time `json:"used"`
}

type file struct {
	Machine string `json:"machine"`
	// compat 0.6: drop connector. Read a file a 0.5 runner wrote under the
	// old key; Open prefers Machine and save never writes this one.
	Connector string           `json:"connector,omitempty"`
	Folders   map[string]entry `json:"folders"`
}

// Open reads the registry at path, or makes one with a new machine id.
func Open(path string) (*Registry, error) {
	r := &Registry{path: path, folders: map[string]entry{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f file
		if json.Unmarshal(b, &f) != nil {
			return nil, errors.New("folders: " + filepath.Base(path) + " cannot be read")
		}
		id := f.Machine
		if id == "" {
			id = f.Connector // compat 0.6: drop connector (old key)
		}
		if !connectorShape(id) {
			return nil, errors.New("folders: " + filepath.Base(path) + " cannot be read")
		}
		r.machine = id
		for k, v := range f.Folders {
			r.folders[k] = v
		}
		r.seen = stampOf(path)
		return r, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, errors.New("folders: " + filepath.Base(path) + " cannot be read")
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	r.machine = "con_" + hex.EncodeToString(raw[:])
	if err := r.save(); err != nil {
		return nil, err
	}
	return r, nil
}

// Machine is this machine's id.
func (r *Registry) Machine() string { return r.machine }

// Ref is the reference of the folder at path. It does not register it.
func Ref(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "fld_" + hex.EncodeToString(sum[:8])
}

// Register keeps path and answers its reference. The folder need not exist
// yet: the turn that names it says so in its own words.
func (r *Registry) Register(path string) (string, error) {
	if err := CheckShape(path); err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	ref := Ref(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
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
	r.refresh()
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
	r.refresh()
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

// save writes the file with mode 0600, by rename, under the lock beside it,
// once what another process wrote since is merged in. The lock is held, or the
// registry is not shared yet.
func (r *Registry) save() error {
	unlock := lockFile(r.path + ".lock")
	defer unlock()
	r.merge()
	b, _ := json.Marshal(file{Machine: r.machine, Folders: r.folders})
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return errors.New("folders: " + filepath.Base(r.path) + " could not be written")
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return errors.New("folders: " + filepath.Base(r.path) + " could not be written")
	}
	r.seen = stampOf(r.path)
	return nil
}

// refresh takes in what another process wrote to the file since it was last
// read. r.mu is held.
func (r *Registry) refresh() {
	if st := stampOf(r.path); st != r.seen {
		r.merge()
		r.seen = st
	}
}

// merge adds the file's folders to the registry's: a folder either side holds
// is kept, with the later of its two registrations. The file's machine id is
// the registry's (Open). A file that cannot be read adds nothing. r.mu is held.
func (r *Registry) merge() {
	b, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var f file
	if json.Unmarshal(b, &f) != nil {
		return
	}
	for k, v := range f.Folders {
		if e, ok := r.folders[k]; !ok || v.Used.After(e.Used) {
			r.folders[k] = v
		}
	}
}

func stampOf(path string) stamp {
	fi, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{fi.ModTime(), fi.Size()}
}

// lockFile holds an exclusive lock on path until the answer is called. A lock
// that cannot be taken is no lock: the write still goes, as before the file
// was shared.
func lockFile(path string) func() {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil {
		f.Close()
		return func() {}
	}
	return func() { f.Close() }
}

func connectorShape(s string) bool {
	if len(s) != 20 || s[:4] != "con_" {
		return false
	}
	_, err := hex.DecodeString(s[4:])
	return err == nil
}
