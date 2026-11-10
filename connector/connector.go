// Package connector is the folder side: the one part of AgentBus that reads
// and writes the person's folder. It owns the folders of one machine
// (package folders), takes a snapshot of one for a runner to build a copy
// from, puts a session's changes into one when the person applies them, and,
// on the local fast path only, lends a runner in the same process the base
// content of a file its copy no longer has.
//
// A connector is a runner that takes only snapshot and apply jobs (package
// runner, Config.Folder). File data goes through the control plane's blob
// store, never straight to a runner: a snapshot's files and trees go there,
// and an apply reads the files a turn wrote from there. In agentbus up the
// store is the control plane's own on the same disk, and nothing is sent
// over HTTP.
//
// Control packages never import it (cmd/agentbus/layers_test.go).
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// Connector is one machine's folder side.
type Connector struct {
	// Folders are the folders it owns.
	Folders *folders.Registry
	// Store is where it keeps blobs: the control plane's own store in
	// agentbus up, its own otherwise.
	Store *blobs.Store
	// Remote is the control plane's store when it is not Store: blobs
	// go there after Store, and are read from there into Store.
	Remote interface {
		blobs.Source
		blobs.Sink
	}
	// State is the connector's own folder: what each folder's last
	// snapshot found, so the next hashes only what changed.
	State string
	// Link clones a file to a new path where the disk can, for a blob kept
	// in Store: no bytes copied. Nil copies.
	Link func(src, dst string) error
	// Prepare, when not nil, readies a folder before it is read or
	// written: a folder inside a runner's overlay copy on this machine is
	// mounted again there (agentbus up wires it).
	Prepare func(path string) error
}

// Taken is a snapshot the connector took.
type Taken struct {
	ID    string `json:"id"`              // the snapshot id: its top content tree
	Times string `json:"times,omitempty"` // the blob of its files' times (workspace.FormatTimes)
	Files int    `json:"files"`
	// Git is the content tree of the folder's .git folder, when it has
	// one: carried beside the snapshot so git works in a copy built from
	// it, and never part of its id, as a checkpoint never holds .git.
	Git string `json:"git,omitempty"`
}

// Errors, in fixed wording for the person: none names a path.
var (
	ErrUnknown   = errors.New("this connector does not own the folder")
	ErrNotFolder = errors.New("the folder is not an existing directory")
	ErrTooMany   = errors.New("the folder has more files than a snapshot holds")
	ErrChanged   = errors.New("the folder changed while its snapshot was taken; send the turn again")
)

// locks keeps one snapshot or apply of a folder at a time.
var locks sync.Map // ref -> *sync.Mutex

func lockRef(ref string) func() {
	m, _ := locks.LoadOrStore(ref, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (c *Connector) path(ref string) (string, error) {
	p, ok := c.Folders.Path(ref)
	if !ok {
		return "", ErrUnknown
	}
	if c.Prepare != nil {
		if err := c.Prepare(p); err != nil {
			return "", ErrNotFolder
		}
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", ErrNotFolder
	}
	return p, nil
}

// Snapshot takes a snapshot of the folder ref: every file hashed (but one
// with the size, time and mode the folder's last snapshot saw, which keeps
// that hash), every file and content tree kept as a blob, in Store and in
// Remote. It answers the snapshot id.
func (c *Connector) Snapshot(ctx context.Context, ref string) (Taken, error) {
	defer lockRef(ref)()
	dir, err := c.path(ref)
	if err != nil {
		return Taken{}, err
	}
	prev := c.lastSnapshot(ref)
	snap, err := workspace.WalkParallel(dir, prev, hashUntil(ctx), 0)
	if err := ctx.Err(); err != nil {
		return Taken{}, err
	}
	if err != nil {
		return Taken{}, ErrNotFolder
	}
	if snap.Truncated() {
		return Taken{}, ErrTooMany
	}
	root, trees, ok := workspace.ContentTrees(snap)
	if !ok {
		return Taken{}, ErrChanged
	}
	var hashes []string
	for _, rel := range snap.Paths() {
		if err := ctx.Err(); err != nil {
			return Taken{}, err
		}
		f, _ := snap.File(rel)
		hashes = append(hashes, f.Hash)
		if c.Store.Has(f.Hash) {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if f.Mode&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return Taken{}, ErrChanged
			}
			if h, err := c.Store.PutBytes(workspace.LinkContent(target)); err != nil || h != f.Hash {
				return Taken{}, ErrChanged
			}
			continue
		}
		if _, err := c.Store.PutFile(p, f.Hash, c.Link); err != nil {
			if errors.Is(err, blobs.ErrBadHash) || errors.Is(err, fs.ErrNotExist) {
				return Taken{}, ErrChanged
			}
			return Taken{}, err
		}
	}
	for h, b := range trees {
		if _, err := c.Store.PutBytes(b); err != nil {
			return Taken{}, err
		}
		hashes = append(hashes, h)
	}
	times, err := c.Store.PutBytes(workspace.FormatTimes(snap))
	if err != nil {
		return Taken{}, err
	}
	hashes = append(hashes, times)
	gitTree := ""
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		h, more, err := c.keepTree(ctx, filepath.Join(dir, ".git"))
		if err != nil {
			return Taken{}, err
		}
		gitTree, hashes = h, append(hashes, more...)
	}
	if c.Remote != nil {
		if err := blobs.Copy(ctx, c.Store, c.Remote, hashes); err != nil {
			return Taken{}, err
		}
	}
	c.keepSnapshot(ref, snap)
	return Taken{ID: root, Times: times, Files: snap.Len(), Git: gitTree}, nil
}

// keepTree keeps every file under dir, and its content trees, in Store, and
// answers its top tree and every blob it names.
func (c *Connector) keepTree(ctx context.Context, dir string) (string, []string, error) {
	snap, err := workspace.WalkParallel(dir, nil, hashUntil(ctx), 0)
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err != nil || snap.Truncated() {
		return "", nil, ErrTooMany
	}
	root, trees, ok := workspace.ContentTrees(snap)
	if !ok {
		return "", nil, ErrChanged
	}
	var hashes []string
	for _, rel := range snap.Paths() {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		f, _ := snap.File(rel)
		hashes = append(hashes, f.Hash)
		if c.Store.Has(f.Hash) {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if f.Mode&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return "", nil, ErrChanged
			}
			if h, err := c.Store.PutBytes(workspace.LinkContent(target)); err != nil || h != f.Hash {
				return "", nil, ErrChanged
			}
			continue
		}
		if _, err := c.Store.PutFile(p, f.Hash, c.Link); err != nil {
			return "", nil, ErrChanged
		}
	}
	for h, b := range trees {
		if _, err := c.Store.PutBytes(b); err != nil {
			return "", nil, err
		}
		hashes = append(hashes, h)
	}
	return root, hashes, nil
}

// hashUntil hashes as workspace.Walk does until ctx ends, and then hashes
// no more: the walk ends at once, and its caller sees ctx's end.
func hashUntil(ctx context.Context) workspace.Hasher {
	return func(rel, p string, info fs.FileInfo) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return workspace.Hash(rel, p, info)
	}
}

func (c *Connector) snapFile(ref string) string {
	return filepath.Join(c.State, "snapshots", ref+".json")
}

// lastSnapshot is what the folder's last snapshot found, or nil.
func (c *Connector) lastSnapshot(ref string) *workspace.Snapshot {
	if c.State == "" {
		return nil
	}
	b, err := os.ReadFile(c.snapFile(ref))
	if err != nil {
		return nil
	}
	var s workspace.Snapshot
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func (c *Connector) keepSnapshot(ref string, s *workspace.Snapshot) {
	if c.State == "" {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	p := c.snapFile(ref)
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// Lend opens the file rel of the folder ref while it is a regular file with
// f's size, modification time and mode (isolation.Lender).
func (c *Connector) Lend(ref, rel string, f workspace.File) (io.ReadCloser, func() bool, bool) {
	dir, err := c.path(ref)
	if err != nil || !f.Mode.IsRegular() {
		return nil, nil, false
	}
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, false
	}
	name := filepath.FromSlash(rel)
	// same opens the folder again, so it can be asked after the reader is
	// closed.
	same := func() bool {
		r2, err := os.OpenRoot(dir)
		if err != nil {
			return false
		}
		defer r2.Close()
		return sameStat(r2, name, f)
	}
	if !same() {
		rt.Close()
		return nil, nil, false
	}
	file, err := rt.Open(name)
	if err != nil {
		rt.Close()
		return nil, nil, false
	}
	return &lent{File: file, rt: rt}, same, true
}

type lent struct {
	*os.File
	rt *os.Root
}

func (l *lent) Close() error {
	err := l.File.Close()
	l.rt.Close()
	return err
}

// Applied is what an apply changed in the folder.
type Applied struct {
	// Files were written or removed, sorted.
	Files []string `json:"files"`
}

// Conflict refuses an apply: these files of the folder are neither as the
// changes start from nor as they end. Nothing was written.
type Conflict struct {
	Files []ConflictFile `json:"files"`
}

// ConflictFile is one file an apply would not overwrite, and why.
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

// Why a file conflicts, in the refusal.
const (
	WhyChanged = "changed in your folder since the checkpoint"
	WhyAdded   = "added in your folder since the checkpoint"
	WhyRemoved = "removed from your folder since the checkpoint"
	WhyNotFile = "is a folder or cannot be read in your folder"
)

// More apply refusals, in fixed wording.
var (
	ErrTruncated = errors.New("the copy had more files than a checkpoint holds, so its changes cannot be applied")
	ErrChangeset = errors.New("the turn's changes could not be read from the control plane's store")
	ErrPartial   = errors.New("the changes could not all be written to your folder; files lists those that were")
)

// MaxChangeset bounds a changeset blob.
const MaxChangeset = 256 << 20

// applyMu keeps one apply at a time in this process, whatever folder it
// writes: two folders, one inside the other, must not both pass the
// conflict check and then write over each other.
var applyMu sync.Mutex

// Apply makes the folder ref as the changeset to holds it, from as the
// changeset from holds it ("" is the copy's base): every file the two
// differ on. It is all or nothing for conflicts: each file must be as at
// from, or already as at to; any other refuses the whole apply with a
// *Conflict, and nothing is written. Then every new file is written out of
// sight (staged) before any is put in place, so a file that cannot be read
// from the store writes nothing either. Only a failure while files are put
// in place leaves some written; ErrPartial says so, and Applied lists them.
func (c *Connector) Apply(ctx context.Context, ref, from, to string) (Applied, error) {
	defer lockRef(ref)()
	applyMu.Lock()
	defer applyMu.Unlock()
	dir, err := c.path(ref)
	if err != nil {
		return Applied{}, err
	}
	var csFrom *workspace.Changeset
	if from != "" {
		if csFrom, err = c.changeset(ctx, from); err != nil {
			return Applied{}, err
		}
	}
	csTo, err := c.changeset(ctx, to)
	if err != nil {
		return Applied{}, err
	}
	if csTo.Truncated || (csFrom != nil && csFrom.Truncated) {
		return Applied{}, ErrTruncated
	}
	steps, err := workspace.Between(csFrom, csTo)
	if err != nil {
		return Applied{}, ErrChangeset
	}
	// Every write goes through the folder as a root, so no symbolic link in
	// it can take a write outside it.
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return Applied{}, ErrNotFolder
	}
	defer rt.Close()
	type op struct {
		rel        string
		to         *workspace.File
		chmod      bool
		staged     string
		removeOnly bool
	}
	var ops []*op
	var conflict Conflict
	for _, st := range steps {
		name := filepath.FromSlash(st.Path)
		fa, fb := st.From, st.To
		// A file the base recorded without a hash is as the base saw it,
		// by size, modification time and mode: one still so in the folder
		// is as at from, without reading it.
		if fa != nil && fa.Hash == "" && sameStat(rt, name, *fa) {
			ops = append(ops, &op{rel: st.Path, to: fb, removeOnly: fb == nil})
			continue
		}
		cur, mode, exists, err := hashIn(rt, name)
		// The same content is the same file only as the same kind: a
		// regular file holding "link:x" is not the link to x.
		is := func(f *workspace.File) bool {
			return f != nil && exists && cur == f.Hash && mode&fs.ModeSymlink == f.Mode&fs.ModeSymlink
		}
		switch {
		case err != nil:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyNotFile})
		case is(fb):
			if mode.IsRegular() && mode.Perm() != fb.Mode.Perm() {
				ops = append(ops, &op{rel: st.Path, to: fb, chmod: true})
			}
		case fb == nil && !exists:
		case is(fa), fa == nil && !exists:
			ops = append(ops, &op{rel: st.Path, to: fb, removeOnly: fb == nil})
		case fa == nil:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyAdded})
		case !exists:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyRemoved})
		default:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyChanged})
		}
	}
	if len(conflict.Files) > 0 {
		sort.Slice(conflict.Files, func(i, j int) bool { return conflict.Files[i].Path < conflict.Files[j].Path })
		return Applied{}, &conflict
	}
	// Stage: every file to write, out of sight under one folder at the top.
	stage := ".agentbus-apply-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	staged := false
	defer func() {
		if staged {
			removeAllIn(rt, stage)
		}
	}()
	src := blobs.Local{Store: c.Store}
	if c.Remote != nil {
		src.From = c.Remote
	}
	for i, o := range ops {
		if err := ctx.Err(); err != nil {
			return Applied{}, err
		}
		if o.to == nil || o.chmod {
			continue
		}
		if !staged {
			if err := rt.Mkdir(stage, 0o700); err != nil {
				return Applied{}, errors.New("connector: the folder cannot be written")
			}
			staged = true
		}
		o.staged = stage + string(filepath.Separator) + strconv.Itoa(i)
		if err := c.stageFile(ctx, rt, src, o.staged, *o.to); err != nil {
			return Applied{}, err
		}
	}
	// Put in place: every removal first, deepest first, so a file can take
	// the name of a folder the changes emptied; then every write, in path
	// order, so a folder is made before what goes in it. A folder emptied
	// stays, as before.
	if err := ctx.Err(); err != nil {
		return Applied{}, err // stopped before any file is put in place
	}
	out := Applied{Files: []string{}}
	fail := func() (Applied, error) {
		sort.Strings(out.Files)
		return out, ErrPartial
	}
	for i := len(ops) - 1; i >= 0; i-- {
		o := ops[i]
		if o.to != nil {
			continue
		}
		name := filepath.FromSlash(o.rel)
		if err := rt.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail()
		}
		out.Files = append(out.Files, o.rel)
	}
	for _, o := range ops {
		if o.to == nil {
			continue
		}
		name := filepath.FromSlash(o.rel)
		var err error
		if o.chmod {
			err = rt.Chmod(name, o.to.Mode.Perm())
		} else {
			if d := filepath.Dir(name); d != "." {
				err = rt.MkdirAll(d, 0o755)
			}
			if err == nil {
				err = placeStaged(rt, o.staged, name)
			}
		}
		if err != nil {
			return fail()
		}
		out.Files = append(out.Files, o.rel)
	}
	sort.Strings(out.Files)
	return out, nil
}

// changeset reads the changeset blob h.
func (c *Connector) changeset(ctx context.Context, h string) (*workspace.Changeset, error) {
	var src blobs.Source = c.Store
	if c.Remote != nil {
		src = blobs.Local{Store: c.Store, From: c.Remote}
	}
	b, err := blobs.ReadAll(ctx, src, h, MaxChangeset)
	if err != nil {
		return nil, ErrChangeset
	}
	var cs workspace.Changeset
	if json.Unmarshal(b, &cs) != nil {
		return nil, ErrChangeset
	}
	return &cs, nil
}

// stageFile writes the file f, from its blob, as tmp of rt.
func (c *Connector) stageFile(ctx context.Context, rt *os.Root, src blobs.Local, tmp string, f workspace.File) error {
	if f.Mode&fs.ModeSymlink != 0 {
		b, err := blobs.ReadAll(ctx, src, f.Hash, 64<<10)
		if err != nil {
			return ErrChangeset
		}
		target, ok := workspace.LinkTarget(b)
		if !ok {
			return ErrChangeset
		}
		return rt.Symlink(target, tmp)
	}
	if err := src.Fetch(ctx, []string{f.Hash}); err != nil {
		return ErrChangeset
	}
	p := c.Store.Path(f.Hash)
	// A clone of the blob where the disk has them: no bytes copied.
	if !cloneInto(rt, tmp, p) {
		if err := copyInto(rt, tmp, p, f.Mode.Perm()); err != nil {
			return errors.New("connector: the folder cannot be written")
		}
	}
	if err := rt.Chmod(tmp, f.Mode.Perm()); err != nil { // the umask narrowed it
		return errors.New("connector: the folder cannot be written")
	}
	got, _, ok, err := hashIn(rt, tmp)
	if err != nil || !ok || got != f.Hash {
		return ErrChangeset
	}
	return nil
}

// placeStaged puts the staged file at name, by rename, so a reader never
// sees half a file. An empty folder in the way goes first: the changes
// emptied it.
func placeStaged(rt *os.Root, staged, name string) error {
	if fi, err := rt.Lstat(name); err == nil && fi.IsDir() {
		if err := rt.Remove(name); err != nil {
			return err
		}
	}
	return rt.Rename(staged, name)
}

// removeAllIn removes the folder name of rt and all it holds.
func removeAllIn(rt *os.Root, name string) {
	_ = rt.RemoveAll(name)
}

// copyInto writes a new file tmp of rt with src's bytes.
func copyInto(rt *os.Root, tmp, src string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// sameStat says whether the file name of rt has f's size, modification time
// and mode.
func sameStat(rt *os.Root, name string, f workspace.File) bool {
	fi, err := rt.Lstat(name)
	return err == nil && fi.Mode() == f.Mode && fi.Size() == f.Size && fi.ModTime().UnixNano() == f.ModTime
}

// hashIn hashes the file name of rt as workspace.Walk does. A missing file
// is not an error: exists is false.
func hashIn(rt *os.Root, name string) (hash string, mode fs.FileMode, exists bool, err error) {
	fi, err := rt.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := rt.Readlink(name)
		if err != nil {
			return "", 0, false, err
		}
		sum := sha256.Sum256(workspace.LinkContent(target))
		return hex.EncodeToString(sum[:]), fi.Mode(), true, nil
	case fi.Mode().IsRegular():
		f, err := rt.Open(name)
		if err != nil {
			return "", 0, false, err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", 0, false, err
		}
		return hex.EncodeToString(h.Sum(nil)), fi.Mode(), true, nil
	}
	return "", 0, false, errors.New("not a file")
}

// Jobs is the connector as a connector's runner calls it (runner.FolderSide):
// the answers in the queue's words.
type Jobs struct{ *Connector }

// SnapshotJob is Snapshot, for a snapshot job.
func (j Jobs) SnapshotJob(ctx context.Context, ref string) (queue.SnapshotTaken, error) {
	t, err := j.Snapshot(ctx, ref)
	if err != nil {
		return queue.SnapshotTaken{}, err
	}
	return queue.SnapshotTaken{ID: t.ID, Times: t.Times, Files: t.Files, Git: t.Git}, nil
}

// ApplyJob is Apply, for an apply job: the files written, or the conflicts
// that refused it; partial when not all files could be put in place.
func (j Jobs) ApplyJob(ctx context.Context, ref, from, to string) (files []string, conflicts []queue.ApplyConflict, partial bool, err error) {
	done, err := j.Apply(ctx, ref, from, to)
	var cf *Conflict
	switch {
	case errors.As(err, &cf):
		out := make([]queue.ApplyConflict, len(cf.Files))
		for i, x := range cf.Files {
			out[i] = queue.ApplyConflict{Path: x.Path, Why: x.Why}
		}
		return nil, out, false, nil
	case errors.Is(err, ErrPartial):
		return done.Files, nil, true, nil
	}
	return done.Files, nil, false, err
}
