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

// This file is the one place AgentBus runs git: the git backend, used only
// when the isolation setting is git, and the removal of a worktree made
// before the backends kept a record. Every other backend, and every diff,
// reads the folder's files only.

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
		repo, in, head, ok := repoOf(r.Folder)
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
	if err := makeWorktree(s.Repo, s.Base, root); err != nil {
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
	if _, err := checkpoint(s, Base, gitBackend{}); err != nil {
		return undo(err)
	}
	return s, nil
}

func (b gitBackend) Checkpoint(s *Space, name string) (Checkpoint, error) {
	return checkpoint(s, name, b)
}

// take walks the worktree. Git wrote every file of it just now, so no time
// in it says the file is as in the person's folder: the Base hashes all. It
// also saves each file small enough for a diff, since no other copy of its
// content is kept that a diff could read without git.
func (gitBackend) take(s *Space, t *trees, prev *treeState, save workspace.Hasher) (treeState, int, error) {
	if prev == nil {
		keep := func(rel, p string, info fs.FileInfo) (string, error) {
			if info.Mode().IsRegular() && info.Size() <= MaxFileDiff {
				return save(rel, p, info)
			}
			return workspace.Hash(rel, p, info)
		}
		snap, err := workspace.Walk(s.Dir, nil, keep)
		if err != nil {
			return treeState{}, 0, err
		}
		return t.fromSnapshot(nil, nil, snap)
	}
	return walkAgainst(s, t, prev, save)
}

// learn is never needed: the Base hashed every file.
func (gitBackend) learn(*Space, string, workspace.File) (string, bool, bool) { return "", false, false }
func (gitBackend) Checkpoints(s *Space) ([]Checkpoint, error)                { return checkpoints(s) }
func (gitBackend) Diff(s *Space, from, to string) (workspace.Changes, error) {
	return diff(s, from, to)
}
func (gitBackend) Apply(s *Space, from, to string) (Applied, error) { return apply(s, from, to) }

func (gitBackend) Close(s *Space) error {
	if s.Root != filepath.Join(s.Workdir, s.ID) {
		return ErrOutside
	}
	if err := RemoveWorktree(s.Workdir, s.Repo, s.Root); err != nil {
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
	out, err := git(dir, args...)
	if err != nil {
		return nil, errors.New("isolation: git status failed")
	}
	return out, nil
}

// git runs git in dir with args, and answers its stdout, or an error with
// the start of git's own words.
func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if msg == "" {
			msg = "git failed"
		}
		return out.Bytes(), errors.New(msg)
	}
	return out.Bytes(), nil
}

// gitTop says whether dir is the top of a git work tree.
func gitTop(dir string) bool {
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	real, err1 := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	want, err2 := filepath.EvalSymlinks(dir)
	return err1 == nil && err2 == nil && real == want
}

// repoOf says whether folder is in a git work tree whose HEAD names a commit:
// the top of that tree, folder's path inside it ("." at the top) and the
// commit. A folder outside git, or in a repo with no commit yet, answers
// false.
func repoOf(folder string) (repo, rel, head string, ok bool) {
	top, err := git(folder, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", false
	}
	repo = strings.TrimSpace(string(top))
	realTop, err1 := filepath.EvalSymlinks(repo)
	realDir, err2 := filepath.EvalSymlinks(folder)
	if err1 != nil || err2 != nil {
		return "", "", "", false
	}
	rel, err = filepath.Rel(realTop, realDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", "", false
	}
	out, err := git(folder, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return "", "", "", false
	}
	return realTop, rel, strings.TrimSpace(string(out)), true
}

// makeWorktree adds a detached git worktree of repo at base, in dir. A dir
// that is already a worktree, from an earlier attempt of the same job, is
// used as it is.
func makeWorktree(repo, base, dir string) error {
	if gitTop(dir) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return errors.New("the worktree folder could not be made")
	}
	// base never starts with a dash (the control plane checks it), and dir
	// is the runner's own absolute path.
	_, err := git(repo, "worktree", "add", "--detach", dir, base)
	return err
}

// RemoveWorktree removes a worktree the runner made, with whatever is in it:
// the git backend's, or one made before the backends kept a record. dir is
// the worktree, or a folder inside it: a runner makes each worktree one
// level under workdir, so that level is the worktree. It refuses a folder
// outside workdir: a runner removes only its own.
func RemoveWorktree(workdir, repo, dir string) error {
	rel, err := filepath.Rel(workdir, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return errors.New("the worktree is not under this runner's workdir")
	}
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	dir = filepath.Join(workdir, top)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		_, _ = git(repo, "worktree", "prune")
		return nil
	}
	_, err = git(repo, "worktree", "remove", "--force", dir)
	return err
}
