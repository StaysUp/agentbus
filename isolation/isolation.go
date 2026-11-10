// Package isolation is the runner's side of a session's files: it gives a
// session its own copy, so its turns change nothing of the person's until a
// change is applied, and records the copy as it was at each moment.
//
// It never writes the person's folder. Reading it and writing it is the
// folder side's (package connector): a snapshot of the folder going out, a
// turn's changes coming back. A runner builds its copy from a snapshot
// (Request.Snapshot, the files from a blob store), or, on the local fast
// path, where the connector is in this process and shares the disk, by a
// clone or an overlay of the folder itself (Request.Folder) as before; and
// it reads the base content of a file its turn changed from its own saved
// files, the snapshot's blobs, or, on the local fast path only, through the
// connector (Lender).
//
// A Backend makes the copy (Open), records it as it is at a moment
// (Checkpoint) and says what changed between two checkpoints (Diff), and
// removes the copy and its checkpoints (Close). One backend owns the whole
// lifecycle of a Space, so nothing above this package makes, finds or
// removes copies by itself. What a checkpoint holds that differs from the
// base goes out as a workspace.Changeset (ChangesetOf), with the saved files
// it names (Blob): an apply needs nothing else.
//
// Three backends make copies. copy copies the folder: a copy-on-write clone
// where the disk has one (APFS on macOS, a reflink on Linux), a plain copy
// otherwise, or the files of a snapshot. overlay (Linux) mounts an overlayfs
// over the session's own read-only copy of the folder or snapshot
// (overlay_linux.go). git makes a git worktree of the folder's repo at its
// HEAD and carries the folder's uncommitted edits into it, on the local fast
// path only; it is the only code that runs git (git.go). A fourth, none,
// makes no copy: it checkpoints a folder used as it is for one job, so that
// every job's changes come from checkpoints (none.go). All keep checkpoints
// the same way: a Merkle tree of every file's size, modification time, mode
// and hash, and a copy of each file a checkpoint saw change. Hashes are
// taken only where a file's size or time says it changed (checkpoint), and
// a copy made by clone records its base by size, time and mode only, as the
// clone keeps each file's time: hashing every file of a 50,000-file folder
// costs about 2 s on an APFS Mac (measured 2026-11-10), more than the clone.
// A checkpoint whose files are all hashed has a snapshot id (Checkpoint.Root):
// the same on any machine for the same files. For picks a backend for the
// isolation setting; auto is the fastest one this machine has. A VM backend
// is one more implementation of Backend.
//
// On disk, under the runner's workdir: the copy at <workdir>/<id>, and the
// space's own record at <workdir>/.isolation/<id>: space.json, the
// checkpoints, the saved files, and an overlay's lower layer. A space
// shares nothing with another, but the read-only folders share_read_only
// names (shared.go), so closing it removes everything it made.
package isolation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// Backend names, as the isolation setting names them. Auto is not a backend
// but a choice of one (For).
const (
	Copy    = "copy"
	Git     = "git"
	Overlay = "overlay"
	None    = "none"
	Auto    = "auto"
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
	// Close removes the copy and everything the space kept.
	Close(s *Space) error
}

// Request is the copy a session asks for.
type Request struct {
	ID      string // the space's name under Workdir: the job that opens it
	Workdir string // the runner's folder for copies
	// Folder is the person's folder, copied as it is now: the local fast
	// path, where the folder is on this machine's disk. Snapshot, instead,
	// is a snapshot of it, built from its blobs. Repo and Base, for git
	// only and instead of either: a worktree of Repo at Base.
	Folder     string
	Snapshot   *Snapshot
	Repo, Base string
	// Ref is the folder's reference, kept with the space: the Lender reads
	// the folder by it.
	Ref string
	// Ctx, when not nil, stops a copy built from a snapshot while its
	// blobs are fetched.
	Ctx context.Context
	// Share, for copy: folders at the top of Folder, such as node_modules,
	// that the copy links to one read-only copy shared by every session
	// made from the same folder, instead of copying them (shared.go).
	Share []string
}

// Space is one session's isolated copy.
type Space struct {
	ID      string `json:"id"`
	Backend string `json:"backend"`
	// Ref is the person's folder the copy was made from, by its reference
	// (package folders). Empty for a worktree made from a repo and a base.
	// The path is never kept: a space made before references kept it as
	// source, which Load turns into Ref.
	Ref    string `json:"ref,omitempty"`
	Source string `json:"source,omitempty"`
	// Snapshot: the snapshot id the copy was built from, when it was.
	Snapshot string `json:"snapshot,omitempty"`
	// Root is the copy, or the worktree, at <workdir>/<id>. Dir is where
	// turns run: Root, or the same subfolder inside it as Source is inside
	// its repo.
	Root string `json:"root"`
	Dir  string `json:"dir"`
	// Repo and Base, for git: the repo and the commit the worktree began at.
	Repo string `json:"repo,omitempty"`
	Base string `json:"base,omitempty"`
	// Method, for copy: clone (copy-on-write), reflink, or copy (a plain
	// copy); overlay for overlay.
	Method string `json:"method,omitempty"`
	// Shared, for copy: the read-only shared folders the copy links to.
	Shared []string `json:"shared,omitempty"`
	// Fallback: why the backend asked for could not be used, and which one
	// was, as For's choice fell back. Empty when it was used.
	Fallback string `json:"fallback,omitempty"`

	Workdir string `json:"-"`
	// Blobs, when not nil, reads blobs the space's saved files lack: the
	// snapshot's, for a copy built from one. Lender, when not nil, reads
	// the person's folder on the local fast path. The runner sets both.
	Blobs  BlobSource `json:"-"`
	Lender Lender     `json:"-"`
	// start: when Open began making the copy (baseHasher).
	start time.Time
	// base, while a copy built from a snapshot takes its Base: its files.
	base *workspace.Snapshot
	// temp, for none: Workdir is a temporary folder of this space's own.
	temp bool
}

// Checkpoint is one recorded state of a space.
type Checkpoint struct {
	Name  string    `json:"name"`
	Seq   int       `json:"seq"`
	Time  time.Time `json:"time"`
	Files int       `json:"files"`
	// Root is the checkpoint's snapshot id: the hex SHA-256 of its top
	// folder's tree (tree.go). Equal files give an equal id, on any machine.
	Root string `json:"root,omitempty"`
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
	ErrTruncated    = errors.New("isolation: the folder has more files than a checkpoint holds, so it cannot be applied")
	ErrOutside      = errors.New("isolation: the folder is not under this runner's workdir")
)

var registry = map[string]Backend{Copy: copyBackend{}, Git: gitBackend{}, Overlay: overlayBackend{}, None: noneBackend{}}

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

// Available are the backends this build can use, sorted: overlay only on
// Linux, where it may still fall back to copy when the kernel refuses it.
func Available() []string {
	var out []string
	for _, n := range Names() {
		if n == Overlay && runtime.GOOS != "linux" {
			continue
		}
		out = append(out, n)
	}
	return out
}

// For is the backend a new session in folder gets under the isolation
// setting:
//
//	auto (also "")  macOS: copy, one clonefile(2) of the folder. Linux:
//	                overlay where this runner can mount one, else copy.
//	overlay         overlay, else copy, as auto on Linux.
//	copy            copy: a clone (APFS), reflinks (xfs, btrfs) or a plain copy.
//	git             a git worktree; copy in a folder outside a git repo with
//	                a commit, which needs no special case.
//
// A fallback is not an error: the space's Fallback says why. none, or a
// setting it does not know, is false: the session works in the folder
// itself, and each of its jobs is checkpointed there by the None backend.
func For(setting, folder string) (Backend, bool) {
	switch setting {
	case "", Auto:
		if runtime.GOOS == "linux" {
			return fallback{registry[Overlay], registry[Copy]}, true
		}
		return registry[Copy], true
	case Overlay:
		return fallback{registry[Overlay], registry[Copy]}, true
	case Copy:
		return registry[Copy], true
	case Git:
		if _, _, _, ok := repoOf(folder); ok {
			return registry[Git], true
		}
		return registry[Copy], true
	}
	return nil, false
}

// Unavailable: a backend cannot make copies on this machine, and why, in
// words for the person.
type Unavailable struct{ Backend, Why string }

func (u *Unavailable) Error() string { return u.Backend + " cannot be used here: " + u.Why }

// fallback opens a space with first, or with then when first is
// Unavailable. The space's own backend does everything after Open.
type fallback struct{ first, then Backend }

func (f fallback) Name() string { return f.first.Name() }

func (f fallback) Open(r Request) (*Space, error) {
	s, err := f.first.Open(r)
	var u *Unavailable
	if !errors.As(err, &u) {
		return s, err
	}
	if s, err = f.then.Open(r); err != nil {
		return nil, err
	}
	if s.Fallback == "" {
		s.Fallback = u.Error() + "; " + f.then.Name() + " was used"
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (f fallback) own(s *Space) Backend {
	if b, ok := Get(s.Backend); ok {
		return b
	}
	return f.then
}

func (f fallback) Checkpoint(s *Space, name string) (Checkpoint, error) {
	return f.own(s).Checkpoint(s, name)
}
func (f fallback) Checkpoints(s *Space) ([]Checkpoint, error) { return f.own(s).Checkpoints(s) }
func (f fallback) Diff(s *Space, from, to string) (workspace.Changes, error) {
	return f.own(s).Diff(s, from, to)
}
func (f fallback) Close(s *Space) error { return f.own(s).Close(s) }

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
	if s.Ref == "" && s.Source != "" {
		s.Ref = folderRef(s.Source)
	}
	s.Source = ""
	return &s, nil
}

// LoadID is Load by the space's id: a session's workspace names its space
// by id, not by its folder on the runner.
func LoadID(workdir, id string) (*Space, error) {
	if !spaceName.MatchString(id) {
		return nil, ErrNoSpace
	}
	return Load(workdir, filepath.Join(workdir, id))
}

// folderRef is package folders' reference of a path: fld_ and 16 hex digits
// of the SHA-256 of the cleaned path. A space made before references kept
// the path; its reference is the same one the connector gives that path.
func folderRef(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "fld_" + hex.EncodeToString(sum[:8])
}

// Remount mounts s's copy again where its backend mounts one (overlay) and
// it is not mounted, as after a runner restarted. A job that reads the
// copy without a checkpoint, such as a review of a session's work, needs
// it.
func Remount(s *Space) error {
	if s.Backend != Overlay {
		return nil
	}
	return remount(s)
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

// ctxOf is r's context, or one that never ends.
func ctxOf(r Request) context.Context {
	if r.Ctx != nil {
		return r.Ctx
	}
	return context.Background()
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
