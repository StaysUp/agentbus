package isolation

import (
	"os"
	"path/filepath"
	"strings"
)

// A copy made from another copy (Request.From): a workspace whose turns run
// on a second workspace's files as they were at one of its checkpoints, never
// in that workspace's live copy (plans/DESIGN_one_writer.md section 2). The
// source keeps its one writer; the new copy has its own.
//
// It is made on the runner that holds the source, from the source alone:
//
//   - the source's whole folder is cloned, as a copy is made from the
//     person's folder (clonefile on APFS, reflinks where the disk has them, a
//     plain copy otherwise), so ignored folders (node_modules, build caches)
//     come with it;
//   - the source's private git is cloned under the source's lock, so no
//     checkpoint moves its refs half way through;
//   - the new copy's Base is the checkpoint's commit, and its files are reset
//     to it the way a discard resets a copy: every file the private git
//     records is the checkpoint's, a file added since is removed, and ignored
//     files stay as the clone found them.
//
// A copy that keeps no private git (an overlay on Linux, a git worktree, a
// copy built from a snapshot) keeps no commit to reset to. Its fork holds the
// source's files as they are when the fork is made, ignored folders included,
// and its Base is that state. The checkpoint must still be one the source
// holds. This is never less than a command run in the live copy saw.

// Refusals of a copy made from another copy. Fixed wording: none names a path.
const (
	refusedForkSource     = "the workspace it is made from has no copy on this runner to make it from"
	refusedForkKind       = "the copy of the workspace it is made from cannot be read"
	refusedForkCheckpoint = "the workspace it is made from has no checkpoint of that name: name base, or a turn of it that changed files"
	refusedForkSelf       = "a workspace cannot be made from its own copy"
)

// openFork makes the copy r asks for from r.From at r.FromCheckpoint. A retry
// of the job that made it finds the copy already there.
func openFork(r Request) (*Space, error) {
	if err := checkOpen(r); err != nil {
		return nil, err
	}
	src := r.From
	switch {
	case src == nil || src.Root == "":
		return nil, &Refusal{refusedForkSource}
	case src.ID == r.ID && src.Workdir == r.Workdir:
		return nil, &Refusal{refusedForkSelf}
	case src.hasShadow() && src.Dir != src.Root:
		return nil, &Refusal{refusedForkKind}
	case !checkpointName.MatchString(r.FromCheckpoint) || !gitRefSafe(r.FromCheckpoint):
		return nil, &Refusal{refusedForkCheckpoint}
	}
	if s, err := Load(r.Workdir, filepath.Join(r.Workdir, r.ID)); err == nil {
		return s, nil // a retry of the job that made it
	}
	ctx := ctxOf(r)
	s := &Space{ID: r.ID, Backend: Copy, Ref: src.Ref, Root: filepath.Join(r.Workdir, r.ID), Workdir: r.Workdir,
		Folders: append([]SpaceFolder(nil), src.Folders...), Nested: src.Nested,
		From: src.ID, FromCheckpoint: r.FromCheckpoint, stop: ctx}
	s.Dir = s.Root
	// What a failed attempt or a failed close left, with no record, is this
	// job's own: it is moved aside, never used (remove.go).
	if err := retire(s); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Workdir, 0o700); err != nil {
		return nil, err
	}
	if !src.hasShadow() {
		return forkAsItIs(r, src, s)
	}
	// The source's files first, outside its lock: a clone of a large folder
	// on a disk with no clonefile takes a while, and the source's own turn
	// must not wait for it. Whatever the source's turn writes meanwhile, the
	// reset below puts back every file the checkpoint records.
	method, err := cloneTree(ctx, src.Root, s.Root)
	if err != nil {
		retire(s)
		return nil, err
	}
	s.Method = method
	commit, err := cloneHistory(r, src, s)
	if err != nil {
		retire(s)
		return nil, err
	}
	// The private git now holds only the new copy's Base: the checkpoint's
	// commit. Every other checkpoint ref is the source's and goes, so the new
	// copy's checkpoints start at its Base.
	if err := s.rebase(r, commit); err != nil {
		retire(s)
		return nil, err
	}
	s.Shadow = true
	if err := s.record(); err != nil {
		retire(s)
		return nil, err
	}
	return s, nil
}

// forkAsItIs makes s from the files src holds now, for a source with no
// private git, and records them as s's Base in a private git of s's own.
func forkAsItIs(r Request, src, s *Space) (*Space, error) {
	b, ok := Get(src.Backend)
	if !ok {
		return nil, &Refusal{refusedForkKind}
	}
	list, err := b.Checkpoints(src)
	if err != nil {
		return nil, err
	}
	if _, ok := findCheckpoint(list, r.FromCheckpoint); !ok {
		return nil, &Refusal{refusedForkCheckpoint}
	}
	method, err := cloneTree(ctxOf(r), src.Dir, s.Root)
	if err != nil {
		retire(s)
		return nil, err
	}
	s.Method = method
	// A worktree's .git file names the person's own repo: git in the fork must
	// never reach it, as a copy of the person's folder leaves it out.
	if fi, err := os.Lstat(filepath.Join(s.Root, ".git")); err == nil && !fi.IsDir() {
		if err := os.Remove(filepath.Join(s.Root, ".git")); err != nil {
			retire(s)
			return nil, err
		}
	}
	s.Nested = true
	if err := s.initShadow(ctxOf(r)); err != nil {
		retire(s)
		return nil, err
	}
	s.Shadow = true
	if err := s.record(); err != nil {
		retire(s)
		return nil, err
	}
	return s, nil
}

// cloneHistory clones src's private git, its saved folder git and the
// checkpoint's mode record into s's state, under src's lock, and answers the
// checkpoint's commit. A checkpoint src does not hold as an active one is a
// Refusal.
func cloneHistory(r Request, src, s *Space) (string, error) {
	ctx := ctxOf(r)
	unlock := lock(src)
	defer unlock()
	list, err := src.shadowCheckpoints(ctx)
	if err != nil {
		return "", err
	}
	if _, ok := findCheckpoint(list, r.FromCheckpoint); !ok {
		return "", &Refusal{refusedForkCheckpoint}
	}
	out, err := src.shadowGit(ctx, "rev-parse", "--verify", "-q", shadowRef(r.FromCheckpoint)+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if err := os.MkdirAll(s.state(), 0o700); err != nil {
		return "", err
	}
	if _, err := cloneTree(ctx, src.gitShadowDir(), s.gitShadowDir()); err != nil {
		return "", err
	}
	if from := filepath.Join(src.state(), "folder-git"); exists(from) {
		if _, err := cloneTree(ctx, from, filepath.Join(s.state(), "folder-git")); err != nil {
			return "", err
		}
	}
	if err := s.writeModes(Base, src.loadModesRaw(r.FromCheckpoint)); err != nil {
		return "", err
	}
	return commit, nil
}

// rebase makes commit s's Base and only checkpoint, and resets s's files to
// it, as a discard to Base would.
func (s *Space) rebase(r Request, commit string) error {
	ctx := ctxOf(r)
	unlock := lock(s)
	defer unlock()
	refs, err := s.shadowGit(ctx, "for-each-ref", "--format=%(refname)", "refs/checkpoints/")
	if err != nil {
		return err
	}
	var stdin strings.Builder
	for _, ref := range strings.Fields(string(refs)) {
		if ref != shadowRef(Base) {
			stdin.WriteString("delete " + ref + "\n")
		}
	}
	stdin.WriteString("update " + shadowRef(Base) + " " + commit + "\n")
	if err := s.shadowGitStdin(ctx, stdin.String(), "update-ref", "--stdin"); err != nil {
		return err
	}
	return s.resetTo(ctx, Base)
}
