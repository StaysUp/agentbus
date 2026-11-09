package isolation

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/staysup/agentbus/workspace"
)

// gitBackend makes a detached git worktree of the folder's repo at its HEAD,
// and runs turns in the same folder inside it. The folder's uncommitted
// edits are carried in, tracked and untracked files alike, so the session
// starts from the folder as it is; files git ignores are not, which keeps a
// big build folder out of the copy. Checkpoints are kept as the copy
// backend keeps them. A Request with Repo and Base, and no Folder, is a
// worktree of Repo at Base, with nothing carried and nowhere to apply.
type gitBackend struct{}

func (gitBackend) Name() string { return Git }

// refusedNotInCommit: the folder is not in the repo's last commit, or is a
// symbolic link there, so the worktree has no such folder to run in.
const refusedNotInCommit = "the folder is not in its repo's last commit, so it cannot get its own worktree; " +
	"commit it, or start the session with isolate false to use the folder as it is"

func (gitBackend) Open(r Request) (*Space, error) {
	if err := checkOpen(r); err != nil {
		return nil, err
	}
	root := filepath.Join(r.Workdir, r.ID)
	if s, err := Load(r.Workdir, root); err == nil {
		return s, nil // a retry of the job that made it
	}
	s := &Space{ID: r.ID, Backend: Git, Root: root, Dir: root, Workdir: r.Workdir}
	rel := "."
	if r.Folder != "" {
		repo, in, head, ok := workspace.RepoOf(r.Folder)
		if !ok {
			return nil, &Refusal{"the folder is not in a git repo with a commit"}
		}
		// The repo's untracked files are carried in: the copies must not be
		// among them.
		if holds(repo, r.Workdir) {
			return nil, &Refusal{refusedHoldsWorkdir}
		}
		s.Source, s.Repo, s.Base, rel = filepath.Clean(r.Folder), repo, head, in
	} else {
		s.Repo, s.Base = r.Repo, r.Base
	}
	if err := removeState(s); err != nil {
		return nil, err
	}
	if err := workspace.MakeWorktree(s.Repo, s.Base, root); err != nil {
		return nil, err
	}
	undo := func(err error) (*Space, error) {
		gitBackend{}.Close(s)
		return nil, err
	}
	// The folder must be a real folder inside the new worktree: one the
	// commit records as a symlink could lead anywhere.
	s.Dir = filepath.Join(root, rel)
	if !insideDir(root, s.Dir) {
		return undo(&Refusal{refusedNotInCommit})
	}
	if s.Source != "" {
		if err := carry(s.Repo, s.Source, root); err != nil {
			return undo(err)
		}
	}
	if err := s.save(); err != nil {
		return undo(err)
	}
	if _, err := checkpoint(s, Base); err != nil {
		return undo(err)
	}
	return s, nil
}

func (gitBackend) Checkpoint(s *Space, name string) (Checkpoint, error) { return checkpoint(s, name) }
func (gitBackend) Checkpoints(s *Space) ([]Checkpoint, error)           { return checkpoints(s) }
func (gitBackend) Diff(s *Space, from, to string) (workspace.Changes, error) {
	return diff(s, from, to)
}
func (gitBackend) Apply(s *Space, from, to string) (Applied, error) { return apply(s, from, to) }

func (gitBackend) Close(s *Space) error {
	if s.Root != filepath.Join(s.Workdir, s.ID) {
		return ErrOutside
	}
	if err := workspace.RemoveWorktree(s.Workdir, s.Repo, s.Root); err != nil {
		return err
	}
	return removeState(s)
}

// carry copies into the worktree at root what git status lists in the
// person's folder: each tracked file changed or removed, and each untracked
// file, not the ignored ones. Paths are from the repo's top, as git status
// gives them. Every write goes through root as a root, so no symbolic link
// in the worktree can take one outside it.
func carry(repo, folder, root string) error {
	out, err := gitOut(folder, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames",
		"--ignore-submodules=all", "--", ".")
	if err != nil {
		return err
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	for _, e := range strings.Split(string(out), "\x00") {
		if len(e) < 4 {
			continue
		}
		x, y, rel := e[0], e[1], filepath.FromSlash(e[3:])
		if y == 'D' || (x == 'D' && y == ' ') {
			if err := rt.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if err := carryFile(rt, filepath.Join(repo, rel), rel); err != nil {
			return err
		}
	}
	return nil
}

// carryFile writes the person's file src at rel of rt: a regular file or a
// symbolic link. Anything else, and a file gone since git status, is left.
func carryFile(rt *os.Root, src, rel string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return nil
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := rt.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := rt.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return nil
		}
		return rt.Symlink(target, rel)
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return nil
		}
		defer in.Close()
		out, err := rt.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = rt.Chmod(rel, info.Mode().Perm())
		}
		if err == nil {
			err = rt.Chtimes(rel, info.ModTime(), info.ModTime())
		}
		return err
	}
	return nil
}

// insideDir says whether folder is a folder at or below dir once every
// symlink in both is resolved.
func insideDir(dir, folder string) bool {
	d, err1 := filepath.EvalSymlinks(dir)
	f, err2 := filepath.EvalSymlinks(folder)
	if err1 != nil || err2 != nil {
		return false
	}
	if info, err := os.Stat(f); err != nil || !info.IsDir() {
		return false
	}
	rel, err := filepath.Rel(d, f)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// gitOut runs git in dir, without taking the repo's optional locks, and
// answers its stdout. Its own words are not passed on: they can name paths.
func gitOut(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("isolation: git status failed")
	}
	return out.Bytes(), nil
}
