package isolation

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/staysup/agentbus/workspace"
)

// How a copy was made, in Space.Method.
const (
	MethodClone   = "clone"   // one copy-on-write clone of the whole folder (APFS)
	MethodReflink = "reflink" // every file a copy-on-write reflink (btrfs, xfs)
	MethodCopy    = "copy"    // a plain copy of some or all files (ext4, or across disks)
)

// copyBackend copies the person's folder, whatever it is: in a git repo or
// not, a repo's top or a folder inside one. A .git folder at the top is
// copied with it, so git works in the copy and a commit there stays there.
// A .git file at the top (a linked worktree's, or a submodule's) is left
// out: it names the person's own repo, which git in the copy would change.
type copyBackend struct{}

func (copyBackend) Name() string { return Copy }

func (copyBackend) Open(r Request) (*Space, error) {
	if err := checkOpen(r); err != nil {
		return nil, err
	}
	if r.Folder == "" || !filepath.IsAbs(r.Folder) {
		return nil, &Refusal{"the folder must be an absolute path"}
	}
	if info, err := os.Stat(r.Folder); err != nil || !info.IsDir() {
		return nil, &Refusal{"the folder is not an existing directory"}
	}
	if holds(r.Folder, r.Workdir) {
		return nil, &Refusal{refusedHoldsWorkdir}
	}
	if s, err := Load(r.Workdir, filepath.Join(r.Workdir, r.ID)); err == nil {
		return s, nil // a retry of the job that made it
	}
	s := &Space{ID: r.ID, Backend: Copy, Source: filepath.Clean(r.Folder), Root: filepath.Join(r.Workdir, r.ID), Workdir: r.Workdir}
	s.Dir = s.Root
	// What a failed attempt left, with no record, is this job's own.
	if err := removeAll(s.Root); err != nil {
		return nil, err
	}
	if err := removeState(s); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Workdir, 0o700); err != nil {
		return nil, err
	}
	method, err := cloneTree(s.Source, s.Root)
	if err != nil {
		removeAll(s.Root)
		return nil, err
	}
	s.Method = method
	if fi, err := os.Lstat(filepath.Join(s.Root, ".git")); err == nil && !fi.IsDir() {
		if err := os.Remove(filepath.Join(s.Root, ".git")); err != nil {
			removeAll(s.Root)
			return nil, err
		}
	}
	if err := s.save(); err != nil {
		removeAll(s.Root)
		removeState(s)
		return nil, err
	}
	if _, err := checkpoint(s, Base); err != nil {
		copyBackend{}.Close(s)
		return nil, err
	}
	return s, nil
}

func (copyBackend) Checkpoint(s *Space, name string) (Checkpoint, error) { return checkpoint(s, name) }
func (copyBackend) Checkpoints(s *Space) ([]Checkpoint, error)           { return checkpoints(s) }
func (copyBackend) Apply(s *Space, from, to string) (Applied, error)     { return apply(s, from, to) }

func (copyBackend) Diff(s *Space, from, to string) (workspace.Changes, error) {
	return diff(s, from, to)
}

func (copyBackend) Close(s *Space) error {
	if s.Root != filepath.Join(s.Workdir, s.ID) {
		return ErrOutside
	}
	if err := removeAll(s.Root); err != nil {
		return err
	}
	return removeState(s)
}

// copyTree copies the folder src to dst, which must not exist: folders,
// files and symbolic links, with their modes and modification times. A file
// it cannot read, a socket or a device, is left out, as a Snapshot leaves it
// out. Each file is a reflink where the disk has them.
func copyTree(src, dst string) (string, error) {
	files, linked := 0, 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
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
			ok, err := copyFile(p, to, info.Mode().Perm(), true)
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

// copyFile copies the file src to dst, which must not exist, with mode
// perm: a reflink when tryReflink and the disk has them, else byte by byte.
// It answers whether it was a reflink. A src it cannot open is
// errUnreadable.
func copyFile(src, dst string, perm fs.FileMode, tryReflink bool) (bool, error) {
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
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(dst, perm) // the umask narrowed it
	}
	return false, err
}
