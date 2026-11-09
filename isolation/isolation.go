// Package isolation gives a session its own copy of the person's folder, so
// its turns change nothing of theirs until a change is applied.
//
// A Backend makes the copy (Open), records it as it is at a moment
// (Checkpoint), says what changed between two checkpoints (Diff), puts those
// changes into the person's folder (Apply), and removes the copy and its
// checkpoints (Close). One backend owns the whole lifecycle of a Space, so
// nothing above this package makes, finds or removes copies by itself.
//
// Two backends today. copy, the default, copies the folder: a copy-on-write
// clone where the disk has one (APFS on macOS, a reflink on Linux), a plain
// copy otherwise. git makes a git worktree of the folder's repo at its HEAD
// and carries the folder's uncommitted edits into it. Both start from the
// folder exactly as it is now, and both keep checkpoints the same way: a
// manifest of every file's hash, and a copy of each file a checkpoint saw
// change, so a checkpoint can be applied after later turns changed the file
// again. A VM backend is one more implementation of Backend.
//
// On disk, under the runner's workdir: the copy at <workdir>/<id>, and the
// space's own record at <workdir>/.isolation/<id>: space.json, the
// checkpoints, and the saved files.
package isolation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// Backend names, as the isolation setting names them.
const (
	Copy = "copy"
	Git  = "git"
)

// Base is the name of a space's first checkpoint, taken when it is opened:
// the person's folder as it was then.
const Base = "base"

// Backend makes and keeps isolated copies of a folder.
type Backend interface {
	Name() string
	// Open makes the copy r asks for and takes its Base checkpoint. Opening
	// the same r.ID again, as a retried job does, returns the space already
	// there. A Refusal says, in words for the person, why it cannot be made.
	Open(r Request) (*Space, error)
	// Checkpoint records the space's files under name. When nothing changed
	// since the latest checkpoint it returns that one and records nothing,
	// so a checkpoint at each turn's start and end costs a walk of the
	// folder when the turn changed nothing. A name already taken returns
	// that checkpoint.
	Checkpoint(s *Space, name string) (Checkpoint, error)
	// Checkpoints are the space's checkpoints, oldest first.
	Checkpoints(s *Space) ([]Checkpoint, error)
	// Diff is what changed from one checkpoint to a later one.
	Diff(s *Space, from, to string) (workspace.Changes, error)
	// Apply puts the changes from one checkpoint to a later one into the
	// person's folder. A file there that is neither as it was at from nor
	// already as it is at to refuses the whole apply with a *Conflict, and
	// nothing is written. Applying from the oldest checkpoint kept also
	// removes what the checkpoints up to to kept: to becomes the oldest.
	// An apply already done is recorded: the same from and to again answer
	// as it did and write nothing, even once its checkpoints were pruned.
	Apply(s *Space, from, to string) (Applied, error)
	// Close removes the copy and everything the space kept.
	Close(s *Space) error
}

// Request is the copy a session asks for.
type Request struct {
	ID      string // the space's name under Workdir: the job that opens it
	Workdir string // the runner's folder for copies
	// Folder is the person's folder, copied as it is now. Repo and Base,
	// for git only and instead of Folder: a worktree of Repo at Base.
	Folder     string
	Repo, Base string
}

// Space is one session's isolated copy.
type Space struct {
	ID      string `json:"id"`
	Backend string `json:"backend"`
	// Source is the person's folder, which Apply writes to. Empty for a
	// worktree made from a repo and a base.
	Source string `json:"source,omitempty"`
	// Root is the copy, or the worktree, at <workdir>/<id>. Dir is where
	// turns run: Root, or the same subfolder inside it as Source is inside
	// its repo.
	Root string `json:"root"`
	Dir  string `json:"dir"`
	// Repo and Base, for git: the repo and the commit the worktree began at.
	Repo string `json:"repo,omitempty"`
	Base string `json:"base,omitempty"`
	// Method, for copy: clone (copy-on-write), or copy (a plain copy).
	Method string `json:"method,omitempty"`

	Workdir string `json:"-"`
}

// Checkpoint is one recorded state of a space.
type Checkpoint struct {
	Name  string    `json:"name"`
	Seq   int       `json:"seq"`
	Time  time.Time `json:"time"`
	Files int       `json:"files"`
}

// Applied is what an Apply changed in the person's folder.
type Applied struct {
	// Files were written or removed, sorted.
	Files []string `json:"files"`
	// Pruned: the checkpoints before to and their saved files were removed.
	Pruned bool `json:"pruned,omitempty"`
}

// Conflict refuses an Apply: these files of the person's folder changed
// since the checkpoint the changes start from.
type Conflict struct {
	Files []ConflictFile `json:"files"`
}

// ConflictFile is one file Apply would not overwrite, and why.
type ConflictFile struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

func (c *Conflict) Error() string {
	paths := make([]string, len(c.Files))
	for i, f := range c.Files {
		paths[i] = f.Path
	}
	return fmt.Sprintf("apply refused: %d file(s) in your folder changed since the checkpoint: %s",
		len(c.Files), strings.Join(paths, ", "))
}

// Refusal is a space that cannot be made, in words for the person.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

// Errors of this package. Each is fixed wording: none quotes a path.
var (
	ErrNoSpace      = errors.New("isolation: no isolated copy is kept for this folder")
	ErrNoCheckpoint = errors.New("isolation: no checkpoint of that name")
	ErrBadName      = errors.New("isolation: a checkpoint name is letters, digits, _ . and -")
	ErrOrder        = errors.New("isolation: from must be an earlier checkpoint than to")
	ErrNoSource     = errors.New("isolation: this space was not made from a folder of yours, so there is nowhere to apply")
	ErrTruncated    = errors.New("isolation: the folder has more files than a checkpoint holds, so it cannot be applied")
	ErrOutside      = errors.New("isolation: the folder is not under this runner's workdir")
)

var registry = map[string]Backend{Copy: copyBackend{}, Git: gitBackend{}}

// Get is the backend of that name.
func Get(name string) (Backend, bool) {
	b, ok := registry[name]
	return b, ok
}

// Names are the backends, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// For is the backend a new session in folder gets under the isolation
// setting: copy (also for ""), git, or none. git in a folder that is not in
// a git repo with a commit is copy: such a folder needs no special case.
// none, or a setting it does not know, is false: the session works in the
// folder itself.
func For(setting, folder string) (Backend, bool) {
	switch setting {
	case "", Copy:
		return registry[Copy], true
	case Git:
		if _, _, _, ok := workspace.RepoOf(folder); ok {
			return registry[Git], true
		}
		return registry[Copy], true
	}
	return nil, false
}

// stateFolder holds each space's record, under the workdir. Its name starts
// with a dot, so it can never be a job id.
const stateFolder = ".isolation"

func stateDir(workdir, id string) string { return filepath.Join(workdir, stateFolder, id) }

func (s *Space) state() string { return stateDir(s.Workdir, s.ID) }

// Load finds the space whose copy holds dir, a folder at or inside a copy
// under workdir, as a session's workspace names it. A folder with no space
// record, such as a worktree made before this package kept one, is
// ErrNoSpace.
func Load(workdir, dir string) (*Space, error) {
	id, err := spaceID(workdir, dir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(stateDir(workdir, id), "space.json"))
	if err != nil {
		return nil, ErrNoSpace
	}
	var s Space
	if err := json.Unmarshal(b, &s); err != nil || s.ID != id {
		return nil, ErrNoSpace
	}
	s.Workdir = workdir
	return &s, nil
}

// Close removes s with its own backend.
func Close(s *Space) error {
	b, ok := Get(s.Backend)
	if !ok {
		return errors.New("isolation: unknown backend")
	}
	return b.Close(s)
}

// spaceID is the first folder of dir under workdir.
func spaceID(workdir, dir string) (string, error) {
	rel, err := filepath.Rel(workdir, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ErrOutside
	}
	id, _, _ := strings.Cut(rel, string(filepath.Separator))
	if !spaceName.MatchString(id) {
		return "", ErrOutside
	}
	return id, nil
}

// spaceName is a space's id: a job id. It never starts with a dot.
var spaceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// save writes s's record.
func (s *Space) save() error {
	if err := os.MkdirAll(s.state(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return writeFileAtomic(filepath.Join(s.state(), "space.json"), b)
}

// writeFileAtomic writes b to p by rename, so a crash never leaves half a
// file.
func writeFileAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// refusedHoldsWorkdir: the copy would be made inside the folder it copies.
const refusedHoldsWorkdir = "the folder holds AgentBus's data folder, so it cannot be copied into it; " +
	"pick a folder inside it, or start the session with isolate false to use the folder as it is"

// holds says whether folder is workdir or a folder above it, once every
// symlink is resolved. workdir need not exist yet.
func holds(folder, workdir string) bool {
	f, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return false
	}
	w := filepath.Clean(workdir)
	// The nearest folder of workdir that exists, resolved, then the rest.
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(w); err == nil {
			w = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(w)
		if parent == w {
			break
		}
		rest = filepath.Join(filepath.Base(w), rest)
		w = parent
	}
	rel, err := filepath.Rel(f, w)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// checkOpen checks what every backend's Open needs.
func checkOpen(r Request) error {
	if r.Workdir == "" || !filepath.IsAbs(r.Workdir) {
		return errors.New("isolation: the workdir must be an absolute path")
	}
	if !spaceName.MatchString(r.ID) {
		return errors.New("isolation: the space id is letters, digits, _ and -")
	}
	return nil
}
