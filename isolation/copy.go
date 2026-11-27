package isolation

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// How a copy was made, in Space.Method.
const (
	MethodClone   = "clone"   // one copy-on-write clone of the whole folder (APFS)
	MethodReflink = "reflink" // every file a copy-on-write reflink (btrfs, xfs)
	MethodCopy    = "copy"    // a plain copy of some or all files (ext4, or across disks)
	// MethodSnapshot: every file written from a snapshot's blobs, a clone
	// of each where the runner's store is on the same disk.
	MethodSnapshot = "snapshot"
)

// copyBackend copies the person's folder, whatever it is: in a git repo or
// not, a repo's top or a folder inside one. A .git folder at the top is
// copied with it, so git works in the copy and a commit there stays there.
// A .git file at the top (a linked worktree's, or a submodule's) is left
// out: it names the person's own repo, which git in the copy would change.
type copyBackend struct{}

func (copyBackend) Name() string { return Copy }

func (copyBackend) Open(r Request) (*Space, error) {
	if len(r.Places) > 0 {
		return openPlaces(r)
	}
	if r.Snapshot != nil {
		return openSnapshot(r)
	}
	if err := checkFolder(r); err != nil {
		return nil, err
	}
	if s, err := Load(r.Workdir, filepath.Join(r.Workdir, r.ID)); err == nil {
		return s, nil // a retry of the job that made it
	}
	// The folder's real path: a folder named through a link is copied, never
	// the link, which would point the copy into the person's folder.
	source, err := filepath.EvalSymlinks(filepath.Clean(r.Folder))
	if err != nil {
		return nil, &Refusal{"the folder is not an existing directory"}
	}
	s := &Space{ID: r.ID, Backend: Copy, Ref: r.Ref, Root: filepath.Join(r.Workdir, r.ID), Workdir: r.Workdir, stop: ctxOf(r)}
	s.Dir = s.Root
	// What a failed attempt or a failed close left, with no record, is this
	// job's own: it is moved aside, never used (remove.go).
	if err := retire(s); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Workdir, 0o700); err != nil {
		return nil, err
	}
	s.start = time.Now()
	method, err := cloneTree(ctxOf(r), source, s.Root)
	if err != nil {
		retire(s)
		return nil, err
	}
	s.Method = method
	if fi, err := os.Lstat(filepath.Join(s.Root, ".git")); err == nil && !fi.IsDir() {
		if err := os.Remove(filepath.Join(s.Root, ".git")); err != nil {
			retire(s)
			return nil, err
		}
	}
	// The copy's private git repo is the one mechanism for its checkpoints: its
	// first commit (Base) is the copy as opened. space.json is written only
	// after the repo and its Base are made, so a copy with a record always has
	// a working shadow, and a crash before then is remade from scratch.
	s.Nested = true
	if err := s.initShadow(ctxOf(r)); err != nil {
		copyBackend{}.Close(s)
		return nil, err
	}
	s.Shadow = true
	if err := s.record(); err != nil {
		copyBackend{}.Close(s)
		return nil, err
	}
	return s, nil
}

func (b copyBackend) Checkpoint(s *Space, name string) (Checkpoint, error) {
	if s.hasShadow() {
		return s.gitCheckpoint(context.Background(), name)
	}
	return checkpoint(s, name, b)
}

// openPlaces makes the copy of a workspace with several folders: each folder
// cloned under the copy's top in a folder of its name, as one folder's copy is
// made.
func openPlaces(r Request) (*Space, error) {
	if err := checkOpen(r); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for i, p := range r.Places {
		if err := checkFolder(Request{ID: r.ID, Workdir: r.Workdir, Folder: p.Folder}); err != nil {
			return nil, err
		}
		if p.Name == "" || p.Name == "." || p.Name == ".." || strings.ContainsAny(p.Name, "/\\\x00") ||
			strings.EqualFold(p.Name, ".git") || names[strings.ToLower(p.Name)] {
			return nil, &Refusal{"each folder of the workspace needs its own name"}
		}
		names[strings.ToLower(p.Name)] = true
		for _, q := range r.Places[:i] {
			if inside(p.Folder, q.Folder) || inside(q.Folder, p.Folder) {
				return nil, &Refusal{refusedNested}
			}
		}
	}
	if s, err := Load(r.Workdir, filepath.Join(r.Workdir, r.ID)); err == nil {
		return s, nil // a retry of the job that made it
	}
	s := &Space{ID: r.ID, Backend: Copy, Root: filepath.Join(r.Workdir, r.ID), Workdir: r.Workdir, stop: ctxOf(r)}
	s.Dir = s.Root
	// What a failed attempt or a failed close left, with no record, is this
	// job's own: it is moved aside, never used (remove.go).
	if err := retire(s); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return nil, err
	}
	s.start = time.Now()
	for _, p := range r.Places {
		dst := filepath.Join(s.Root, p.Name)
		// The folder's real path, never a link into the person's folder.
		src, err := filepath.EvalSymlinks(filepath.Clean(p.Folder))
		if err != nil {
			retire(s)
			return nil, &Refusal{"a folder of the workspace is not an existing directory"}
		}
		method, err := cloneTree(ctxOf(r), src, dst)
		if err != nil {
			retire(s)
			return nil, err
		}
		// The slowest way any folder was copied names the copy's method.
		if s.Method == "" || method == MethodCopy || (method == MethodReflink && s.Method == MethodClone) {
			s.Method = method
		}
		if fi, err := os.Lstat(filepath.Join(dst, ".git")); err == nil && !fi.IsDir() {
			if err := os.Remove(filepath.Join(dst, ".git")); err != nil {
				retire(s)
				return nil, err
			}
		}
		s.Folders = append(s.Folders, SpaceFolder{Name: p.Name, Ref: p.Ref})
	}
	// One private git for the whole copy, its work tree the copy's top: each
	// folder's files, a repo's among them (nested.go), and what a turn puts
	// beside the folders are recorded in one history.
	s.Nested = true
	if err := s.initShadow(ctxOf(r)); err != nil {
		copyBackend{}.Close(s)
		return nil, err
	}
	s.Shadow = true
	if err := s.record(); err != nil {
		copyBackend{}.Close(s)
		return nil, err
	}
	return s, nil
}

// refusedNested: one folder of a workspace is inside another.
const refusedNested = "one folder of the workspace is inside another; name the outer folder alone, or folders side by side"

// inside says whether folder a is b or inside b, by the folders' identity on
// disk (os.SameFile), so a link, a case variant on a disk that ignores case,
// or a Unicode variant of one name is the same folder.
func inside(a, b string) bool {
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	d, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	for {
		if di, err := os.Stat(d); err == nil && os.SameFile(di, bi) {
			return true
		}
		p := filepath.Dir(d)
		if p == d {
			return false
		}
		d = p
	}
}

// checkFolder checks what a copy of r.Folder needs.
func checkFolder(r Request) error {
	if err := checkOpen(r); err != nil {
		return err
	}
	if r.Folder == "" || !filepath.IsAbs(r.Folder) {
		return &Refusal{"the folder must be an absolute path"}
	}
	if info, err := os.Stat(r.Folder); err != nil || !info.IsDir() {
		return &Refusal{"the folder is not an existing directory"}
	}
	if holds(r.Folder, r.Workdir) {
		return &Refusal{refusedHoldsWorkdir}
	}
	return nil
}

func (copyBackend) Checkpoints(s *Space) ([]Checkpoint, error) {
	if s.hasShadow() {
		// The lock, as the other shadow reads take it, so a list never races a
		// checkpoint or a restore moving the refs.
		unlock := lock(s)
		defer unlock()
		return s.shadowCheckpoints(context.Background())
	}
	return checkpoints(s)
}

func (copyBackend) Diff(s *Space, from, to string) (workspace.Changes, error) {
	if s.hasShadow() {
		return s.gitDiff(context.Background(), from, to)
	}
	return diff(s, from, to)
}

// RestoreTo resets the copy's working files to checkpoint name through the
// copy's private git repo (reset --hard, then clean, then the restrictive modes
// git cannot store). Only the copy under s.Dir is touched, never the person's
// real folder. A failed reset leaves the copy as it was; a clean that fails
// after the reset (which the chmod-first guards against) could leave it part
// way, so a failed RestoreTo keeps the dropped turns un-discarded and control
// may retry. A copy without a shadow repo (a snapshot copy, or one made before
// this) cannot be reset, and its Discard is refused.
func (copyBackend) RestoreTo(s *Space, name string) error {
	if s.hasShadow() {
		return s.restoreShadow(context.Background(), name)
	}
	return ErrNoRestore
}

// UpdateFrom brings the person's folder into the copy by a three-way merge in
// the copy's private git repo (gitupdate.go). Only a shadow copy can do it; a
// snapshot-built or legacy copy has no private git and is refused.
func (copyBackend) UpdateFrom(s *Space, base string, folders FolderPaths, merge bool) (UpdateResult, error) {
	if s.hasShadow() {
		return s.updateFromFolder(context.Background(), base, folders, merge)
	}
	return UpdateResult{}, ErrNoUpdate
}

// Close takes the space's record away before any file of the copy goes
// (retire), so a close that fails part way leaves no copy a turn can load.
func (copyBackend) Close(s *Space) error {
	if s.Root != filepath.Join(s.Workdir, s.ID) {
		return ErrOutside
	}
	if err := retire(s); err != nil {
		return err
	}
	// Remove any legacy shared copy this close leaves unreferenced. No new copy
	// makes one; this cleans up an install that used the retired setting.
	gcLegacyShared(s.Workdir)
	return nil
}

// take walks the copy. Its Base records each file by size, modification
// time and mode, and hashes only links and files written just before the
// copy was made (baseHasher): the clone kept every file's time and mode, so
// a file unchanged in the person's folder still has them there. A later
// checkpoint walks it again against prev's files, and writes the trees of
// the folders whose files changed.
func (copyBackend) take(s *Space, t *trees, prev *treeState, save workspace.Hasher) (treeState, int, error) {
	if prev == nil && s.base != nil {
		return t.fromSnapshot(nil, nil, s.base)
	}
	if prev == nil {
		start := s.start
		if start.IsZero() {
			start = time.Now()
		}
		snap, err := workspace.Walk(s.Dir, nil, baseHasher(s.stopCtx(), start, s.objectDir()))
		if err != nil {
			return treeState{}, 0, err
		}
		return t.fromSnapshot(nil, nil, snap)
	}
	return walkAgainst(s, t, prev, save)
}

// walkAgainst walks the space's folder with prev's files, so only a file
// whose size, time or mode changed is passed to save, and records it as a
// tree edited from prev's.
func walkAgainst(s *Space, t *trees, prev *treeState, save workspace.Hasher) (treeState, int, error) {
	old, err := t.flatten(prev.Root)
	if err != nil {
		return treeState{}, 0, err
	}
	old.SetTaken(time.Unix(0, prev.Taken))
	snap, err := workspace.Walk(s.Dir, old, save)
	if err != nil {
		return treeState{}, 0, err
	}
	return t.fromSnapshot(prev, old, snap)
}

// learn reads the Base content of rel from the person's folder, through the
// Lender, where the file still has the size, modification time and mode the
// Base saw. A copy built from a snapshot hashed every file, so it never
// needs to.
func (copyBackend) learn(s *Space, rel string, f workspace.File) (string, bool, bool) {
	return lend(s, rel, f, f.Size <= MaxFileDiff)
}

// openSnapshot makes a copy from r's snapshot: every file written from its
// blobs, and the Base its files as written, each with the hash its blob
// has. The Base's snapshot id must be the snapshot's.
func openSnapshot(r Request) (*Space, error) {
	if err := checkOpen(r); err != nil {
		return nil, err
	}
	if s, err := Load(r.Workdir, filepath.Join(r.Workdir, r.ID)); err == nil {
		return s, nil // a retry of the job that made it
	}
	s := &Space{ID: r.ID, Backend: Copy, Ref: r.Ref, Root: filepath.Join(r.Workdir, r.ID), Workdir: r.Workdir,
		Method: MethodSnapshot, Snapshot: r.Snapshot.ID, stop: ctxOf(r)}
	s.Dir = s.Root
	if err := retire(s); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Workdir, 0o700); err != nil {
		return nil, err
	}
	files, err := materialise(ctxOf(r), r.Snapshot, s.Root, 0o755)
	if err != nil {
		retire(s)
		return nil, err
	}
	// The record last, once the Base is written (remove.go).
	if err := writeBase(s, files); err != nil {
		retire(s)
		return nil, err
	}
	if err := s.record(); err != nil {
		retire(s)
		return nil, err
	}
	// A copy built from a snapshot keeps its checkpoints the Merkle way and has
	// no shadow repo: its runner is remote, where a discard is not offered yet,
	// so RestoreTo refuses and nothing resets it.
	return s, nil
}

// writeBase records files, every one hashed, as s's Base, and checks its
// snapshot id is the one s was built from.
func writeBase(s *Space, files *workspace.Snapshot) error {
	s.base = files
	defer func() { s.base = nil }()
	c, err := checkpoint(s, Base, copyBackend{})
	if err != nil {
		return err
	}
	if c.Root != s.Snapshot {
		return ErrSnapshot
	}
	return nil
}

// copyTree copies the folder src to dst, which must not exist: folders,
// files and symbolic links, with their modes and modification times. A file
// it cannot read, a socket or a device, is left out, as a Snapshot leaves it
// out. Each file is a reflink where the disk has them. It stops with ctx's
// error when ctx ends, within one entry or one chunk of a file, and leaves
// what it copied for its caller to remove (#95).
func copyTree(ctx context.Context, src, dst string) (string, error) {
	files, linked := 0, 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == src {
				return err
			}
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch {
		case d.IsDir():
			// Writable by its owner while it is filled; the mode is set after.
			if err := os.Mkdir(to, info.Mode().Perm()|0o700); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			if err := os.Symlink(target, to); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			ok, err := copyFile(ctx, p, to, info.Mode().Perm(), true)
			if errors.Is(err, errUnreadable) {
				return nil
			}
			if err != nil {
				return err
			}
			files++
			if ok {
				linked++
			}
			_ = os.Chtimes(to, info.ModTime(), info.ModTime())
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// Folder modes, deepest first, now that nothing more is written in them.
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if info, err := d.Info(); err == nil {
				rel, _ := filepath.Rel(src, p)
				_ = os.Chmod(filepath.Join(dst, rel), info.Mode().Perm()|0o700)
				_ = os.Chtimes(filepath.Join(dst, rel), info.ModTime(), info.ModTime())
			}
		}
		return nil
	})
	if files > 0 && linked == files {
		return MethodReflink, nil
	}
	return MethodCopy, nil
}

var errUnreadable = errors.New("isolation: unreadable")

// copyChunk is how much of a file copyFile copies between two looks at its
// context: a stop waits for one chunk at most. io.CopyN keeps the kernel's
// own copy (copy_file_range, sendfile) for each chunk.
const copyChunk = 32 << 20

// copyFile copies the file src to dst, which must not exist, with mode
// perm: a reflink when tryReflink and the disk has them, else byte by byte,
// until ctx ends. It answers whether it was a reflink. A src it cannot open
// is errUnreadable.
func copyFile(ctx context.Context, src, dst string, perm fs.FileMode, tryReflink bool) (bool, error) {
	in, err := os.Open(src)
	if err != nil {
		return false, errUnreadable
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return false, err
	}
	if tryReflink && reflink(out, in) {
		if err := out.Close(); err != nil {
			return false, err
		}
		return true, os.Chmod(dst, perm)
	}
	for err == nil {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int64
		n, err = io.CopyN(out, in, copyChunk)
		if err == io.EOF || (err == nil && n < copyChunk) {
			err = nil
			break
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(dst, perm) // the umask narrowed it
	}
	return false, err
}

// Clone makes dst, which must not exist, a copy of the file src: a
// copy-on-write clone where the disk has one, a plain copy otherwise.
func Clone(src, dst string) error { return cloneFile(src, dst, 0o600) }
