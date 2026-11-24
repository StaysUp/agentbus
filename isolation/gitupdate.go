package isolation

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// Update from the person's folder, as a three-way merge in the copy's private
// git repo (gitshadow.go). A sandbox session's copy falls behind the folder
// when another session applied, or the person edited: Apply then refuses the
// changed files as conflicts. An update brings the folder's current state into
// the copy so the session's own work sits on top of it, and the next Apply
// writes only that work.
//
// The three sides, all commits in the private repo:
//
//	base    the checkpoint the folder last matched: the last applied one, or
//	        Base. The merge base.
//	ours    the copy's head: the session's work.
//	theirs  a fresh snapshot of the person's folder as it is now, taken the
//	        same way a checkpoint is (tracked files, .gitignore and the built-in
//	        ignores respected), committed with base as its parent.
//
// A clean merge records theirs and the merge as two checkpoints and leaves the
// copy holding the merge; the folder is only read. A real conflict (the same
// lines changed on both sides) changes nothing and names the files, so the
// person can ask the agent to resolve it in a turn. A merge (merge true) goes
// on instead: each conflicting file enters the merge as git left it in the
// trial work directory (both sides between conflict markers), the merge is
// recorded like a clean one, and the files are named as Unresolved, so the
// agent's next turn resolves them and Apply writes only the session's work.
// git 2.37 has no
// `merge-tree --write-tree`, so the merge runs with `merge-recursive` against a
// throwaway index and work directory: the real copy is touched only once the
// merge is known to be clean.

// updateTmpDir holds an update's throwaway index files and merge work
// directory, beside the copy's other state. It is cleared before and after an
// update, so a stopped update leaves nothing behind.
func (s *Space) updateTmpDir() string { return filepath.Join(s.state(), "update-tmp") }

// updateFromFolder runs the three-way merge. The caller resolved folder to the
// person's folder on this machine (the runner, never this package). base is the
// merge-base checkpoint ("" means Base).
func (s *Space) updateFromFolder(ctx context.Context, base, folder string, merge bool) (UpdateResult, error) {
	unlock := lock(s)
	defer unlock()

	baseName := base
	if baseName == "" {
		baseName = Base
	}
	list, err := s.shadowCheckpoints(ctx)
	if err != nil {
		return UpdateResult{}, err
	}
	if _, ok := findCheckpoint(list, baseName); !ok {
		// The baseline must be an active checkpoint reachable from HEAD; a
		// discarded one must not be a merge base.
		return UpdateResult{}, ErrNoCheckpoint
	}
	if len(list) == 0 {
		return UpdateResult{}, ErrNoCheckpoint
	}
	headName := list[len(list)-1].Name

	baseCommit, err := s.revCommit(ctx, shadowRef(baseName))
	if err != nil {
		return UpdateResult{}, err
	}
	oursCommit, err := s.revCommit(ctx, "HEAD")
	if err != nil {
		return UpdateResult{}, err
	}

	tmp := s.updateTmpDir()
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return UpdateResult{}, err
	}
	defer os.RemoveAll(tmp)

	// theirs: a snapshot of the folder as it is now, parented on base.
	theirsCommit, theirsModes, err := s.commitFolder(ctx, folder, baseCommit, tmp)
	if err != nil {
		return UpdateResult{}, err
	}

	// A trial merge into a throwaway index and work directory, so the copy is
	// untouched until the merge is known to be clean.
	trialIdx := filepath.Join(tmp, "merge.idx")
	trialWT := filepath.Join(tmp, "merge-wt")
	if err := os.MkdirAll(trialWT, 0o700); err != nil {
		return UpdateResult{}, err
	}
	if _, err := s.shadowGitIn(ctx, trialWT, trialIdx, "read-tree", oursCommit); err != nil {
		return UpdateResult{}, err
	}
	_, _, code, rerr := s.runGitIn(ctx, trialWT, trialIdx, "merge-recursive", baseCommit, "--", oursCommit, theirsCommit)
	var unresolved []string
	switch {
	case code == 0:
		// clean
	case code == 1:
		// A real conflict: the same paths changed on both sides.
		conflicts, cerr := s.unmergedPaths(ctx, trialWT, trialIdx)
		if cerr != nil {
			return UpdateResult{}, cerr
		}
		if !merge {
			// Name them and change nothing.
			return UpdateResult{Clean: false, Conflicts: conflicts}, nil
		}
		// A merge: stage each conflicting file with both sides between conflict
		// markers, so the merge below writes it into the copy for the agent to
		// resolve. A conflict markers cannot show (a binary file, a link, a file
		// one side removed) would let one side win silently, so then the merge
		// changes nothing, as an update does, and names those files.
		unmergeable, err := s.stageResolved(ctx, trialIdx, tmp, conflicts)
		if err != nil {
			return UpdateResult{}, err
		}
		if len(unmergeable) > 0 {
			return UpdateResult{Clean: false, Conflicts: conflicts, Unmergeable: unmergeable}, nil
		}
		unresolved = conflicts
	default:
		if ctx.Err() != nil {
			return UpdateResult{}, ctx.Err()
		}
		_ = rerr
		return UpdateResult{}, errGit
	}

	mergedTree, err := s.shadowGitIn(ctx, trialWT, trialIdx, "write-tree")
	if err != nil {
		return UpdateResult{}, err
	}
	mergedTreeID := strings.TrimSpace(string(mergedTree))

	// Two checkpoints: theirs (the new baseline the folder matches) and the
	// merge (the copy's new head). A stamp keyed by the clock, unique under the
	// per-workspace lane that serialises updates, names them.
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	theirsName := "update." + stamp + ".theirs"
	mergedName := "update." + stamp

	// The merge commit has both sides as parents, so theirs stays reachable
	// from HEAD and its changeset can be read for the apply baseline.
	mergedCommit, err := s.commitTree(ctx, mergedTreeID, "checkpoint "+mergedName, oursCommit, theirsCommit)
	if err != nil {
		return UpdateResult{}, err
	}

	// Merged modes, a three-way merge of the restrictive permission bits git
	// cannot store (0600 and the like): for each file, the side that changed the
	// mode relative to the merge base wins, so the session's own mode changes are
	// kept and a mode the folder changed (and the session did not) is taken from
	// the folder. Overlaying every one of ours would revert a folder-only mode
	// change (a 0600 base the person relaxed to 0644). reconcileModes later
	// touches only files present in the merged tree, so stale entries are
	// harmless. "" is git's default (0644/0755): absence from a record.
	//
	// Known limit (TODO, 0.6.x): modes are matched by path, not by git's rename
	// detection, so a mode the folder changed on a path the session renamed is
	// not carried to the new path -- the renamed file keeps git's default rather
	// than the folder's restrictive bit. The content still merges (rename
	// followed); only the restrictive bit on that one renamed file is lost, never
	// widening another file, so it loosens at worst and never corrupts content.
	baseModes := s.loadModesRaw(baseName)
	oursModes := s.loadModesRaw(headName)
	seenMode := map[string]bool{}
	mergedModes := map[string]string{}
	chooseMode := func(p string) {
		if seenMode[p] {
			return
		}
		seenMode[p] = true
		chosen := theirsModes[p]
		if oursModes[p] != baseModes[p] {
			chosen = oursModes[p] // ours changed the mode from the base: keep ours
		}
		if chosen != "" {
			mergedModes[p] = chosen
		}
	}
	for p := range baseModes {
		chooseMode(p)
	}
	for p := range oursModes {
		chooseMode(p)
	}
	for p := range theirsModes {
		chooseMode(p)
	}

	// Record theirs' modes, then the merged modes, before the refs move: a
	// failed record never leaves a checkpoint without its modes.
	if err := s.writeModes(theirsName, theirsModes); err != nil {
		return UpdateResult{}, err
	}
	if err := s.writeModes(mergedName, mergedModes); err != nil {
		return UpdateResult{}, err
	}

	// Move HEAD and both checkpoint refs in one transaction, so a crash never
	// leaves HEAD advanced with no checkpoint, or theirs without its ref.
	stdin := "update refs/heads/main " + mergedCommit + "\n" +
		"update " + shadowRef(mergedName) + " " + mergedCommit + "\n" +
		"update " + shadowRef(theirsName) + " " + theirsCommit + "\n"
	if err := s.shadowGitStdin(ctx, stdin, "update-ref", "--stdin"); err != nil {
		return UpdateResult{}, err
	}

	// From here the copy is being advanced to the merge. Make it all-or-nothing
	// for every error this returns: any failure resetting the work tree rolls the
	// copy back to its prior head (ours stays reachable as the merge's first
	// parent), so a reachable failure never leaves the copy ahead of the merge
	// control will record. reset, clean and reconcile each either complete or
	// leave the copy as it was, so the reset back lands on a consistent state.
	// (A process stopped mid-advance is the one residual window, the same class
	// as a discard interrupted mid-reset; a durable pending-update record would
	// close it, tracked as a follow-up.)
	advance := func() error {
		if err := makeTreeWritable(s.Dir); err != nil {
			return err
		}
		if _, err := s.shadowGit(ctx, "reset", "-q", "--hard", shadowRef(mergedName), "--"); err != nil {
			return err
		}
		if _, err := s.shadowGit(ctx, "clean", "-q", "-ffd"); err != nil {
			return err
		}
		return s.reconcileModes(ctx, mergedName)
	}
	if err := advance(); err != nil {
		// Roll the copy back to its prior head and drop the merge's refs, so the
		// copy matches control's unchanged view. A rollback that itself fails is
		// reported, so the caller never treats the copy as either state.
		if rb := s.resetTo(ctx, headName); rb != nil {
			return UpdateResult{}, errGit
		}
		return UpdateResult{}, err
	}
	return UpdateResult{Clean: true, Theirs: theirsName, Merged: mergedName, PriorHead: headName, Unresolved: unresolved}, nil
}

// stageResolved records each conflicting path in the trial index idx at stage
// 0, so the index writes a tree. The trial work tree holds no file to take
// (merge-recursive writes there only what it can), so each file is built from
// the index's own stages: git merge-file joins both sides with conflict
// markers labelled for the agent. A path markers cannot show -- one side
// removed it, either side is a link or a nested repository, or merge-file
// cannot join it (binary) --
// is answered in unmergeable, sorted, and the index is then not to be used.
// Any path still unmerged after is an error, never a half-merged tree.
func (s *Space) stageResolved(ctx context.Context, idx, tmp string, paths []string) (unmergeable []string, err error) {
	out, _, _, err := s.runGitIn(ctx, tmp, idx, "ls-files", "-u", "-z")
	if err != nil {
		return nil, gitErr(ctx)
	}
	type entry struct{ mode, sha string }
	stages := map[string]map[string]entry{}
	for _, e := range strings.Split(string(out), "\x00") {
		// "<mode> <sha> <stage>\t<path>"
		tab := strings.IndexByte(e, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(e[:tab])
		if len(f) != 3 {
			continue
		}
		p := e[tab+1:]
		if stages[p] == nil {
			stages[p] = map[string]entry{}
		}
		stages[p][f[2]] = entry{f[0], f[1]}
	}
	blob := func(sha, name string) (string, error) {
		path := filepath.Join(tmp, name)
		b := []byte{}
		if sha != "" {
			var err error
			if b, err = s.shadowGit(ctx, "cat-file", "blob", sha); err != nil {
				return "", err
			}
		}
		return path, os.WriteFile(path, b, 0o600)
	}
	for _, p := range paths {
		st := stages[p]
		base, ours, theirs := st["1"], st["2"], st["3"]
		if !regularMode(ours.mode) || !regularMode(theirs.mode) {
			unmergeable = append(unmergeable, p)
			continue
		}
		o, err := blob(ours.sha, "merge-ours")
		if err != nil {
			return nil, err
		}
		b, err := blob(base.sha, "merge-base")
		if err != nil {
			return nil, err
		}
		t, err := blob(theirs.sha, "merge-theirs")
		if err != nil {
			return nil, err
		}
		// merge-file writes the joined file over its first argument and exits
		// with the number of conflicts; a negative exit is binary or an error.
		_, _, code, _ := s.runGitIn(ctx, tmp, idx, "merge-file", "-L", workspace.MergeOurs, "-L", "base", "-L", workspace.MergeTheirs, o, b, t)
		if code < 0 || code >= 128 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			unmergeable = append(unmergeable, p)
			continue
		}
		sha, err := s.shadowGitIn(ctx, tmp, idx, "hash-object", "-w", "--", o)
		if err != nil {
			return nil, err
		}
		// The mode is a three-way choice as well: the side that changed it from
		// the base wins, so the person's chmod +x survives a text conflict.
		mode := theirs.mode
		if base.mode == "" || ours.mode != base.mode {
			mode = ours.mode
		}
		// A stage-0 entry replaces every unmerged stage of the path.
		if _, err := s.shadowGitIn(ctx, tmp, idx, "update-index", "--add", "--cacheinfo", mode+","+strings.TrimSpace(string(sha))+","+p); err != nil {
			return nil, err
		}
	}
	if len(unmergeable) > 0 {
		sort.Strings(unmergeable)
		return unmergeable, nil
	}
	left, err := s.unmergedPaths(ctx, tmp, idx)
	if err != nil {
		return nil, err
	}
	if len(left) > 0 {
		return nil, errGit
	}
	return nil, nil
}

// regularMode: a git index mode of a regular file (plain or executable), the
// only kind conflict markers can show. Absent (one side removed it), a link
// (120000) or a nested repository (160000) is not.
func regularMode(m string) bool { return m == "100644" || m == "100755" }

// gitErr is errGit, or the context's error when it was stopped.
func gitErr(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errGit
}

// commitFolder snapshots the folder as a commit parented on parent, and answers
// the commit id and the folder's restrictive-mode map. It stages the folder
// through a throwaway index with the folder as the work tree, so nothing in the
// folder is written and the folder's own .git is never seen (GIT_DIR is the
// private repo). The built-in ignores (info/exclude) and the folder's own
// .gitignore are respected, the same as a checkpoint.
func (s *Space) commitFolder(ctx context.Context, folder, parent, tmp string) (commit string, modes map[string]string, err error) {
	idx := filepath.Join(tmp, "theirs.idx")
	// Seed the index from the merge base's tree first, so a file the base tracked
	// that now matches a .gitignore rule but is still present in the folder stays
	// tracked (git never ignores a file already in the index): add -A updates it,
	// rather than an empty index dropping it as if the folder had removed it. A
	// genuinely new ignored file is still left out, as git normally would.
	if _, err := s.shadowGitIn(ctx, folder, idx, "read-tree", parent); err != nil {
		return "", nil, err
	}
	// git add aborts the lot on an unborn nested repo; --ignore-errors adds the
	// rest and exits 1, the same partial-add shadowAdd tolerates.
	if _, _, code, aerr := s.runGitIn(ctx, folder, idx, "add", "-A", "--ignore-errors"); aerr != nil && code != 1 {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, errGit
	}
	treeOut, err := s.shadowGitIn(ctx, folder, idx, "write-tree")
	if err != nil {
		return "", nil, err
	}
	tree := strings.TrimSpace(string(treeOut))
	modes, err = s.modesIn(ctx, folder, idx)
	if err != nil {
		return "", nil, err
	}
	commit, err = s.commitTree(ctx, tree, "checkpoint theirs", parent)
	if err != nil {
		return "", nil, err
	}
	return commit, modes, nil
}

// commitTree makes a commit object for tree with the given message and parents,
// moving no ref, and answers its id.
func (s *Space) commitTree(ctx context.Context, tree, message string, parents ...string) (string, error) {
	args := []string{"commit-tree", tree, "-m", message}
	for _, p := range parents {
		if p != "" {
			args = append(args, "-p", p)
		}
	}
	out, err := s.shadowGit(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// revCommit resolves ref to a commit id.
func (s *Space) revCommit(ctx context.Context, ref string) (string, error) {
	out, err := s.shadowGit(ctx, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", ErrNoCheckpoint
	}
	return id, nil
}

// unmergedPaths are the folder-relative paths the trial merge left unmerged in
// index idx (any stage above 0), sorted and unique: the files that conflicted.
func (s *Space) unmergedPaths(ctx context.Context, workTree, idx string) ([]string, error) {
	out, _, _, err := s.runGitIn(ctx, workTree, idx, "ls-files", "-u", "-z")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errGit
	}
	seen := map[string]bool{}
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		// "<mode> <sha> <stage>\t<path>"
		tab := strings.IndexByte(entry, '\t')
		if tab < 0 {
			continue
		}
		seen[entry[tab+1:]] = true
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// modesIn is the restrictive-mode map of the files tracked in index idx, read
// from work tree wt: every regular file whose permission bits are not git's
// 0644 or 0755. It mirrors currentModes for a named index and work tree, so a
// theirs snapshot carries the folder's exact perms.
func (s *Space) modesIn(ctx context.Context, wt, idx string) (map[string]string, error) {
	out, _, _, err := s.runGitIn(ctx, wt, idx, "ls-files", "-z")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errGit
	}
	modes := map[string]string{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		fi, err := os.Lstat(filepath.Join(wt, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o644 && perm != 0o755 {
			modes[p] = "0" + strconv.FormatUint(uint64(perm), 8)
		}
	}
	return modes, nil
}

// runGitIn runs git against the private repo with a named work tree and index,
// so a call can stage or merge without disturbing the copy's own index or work
// tree. Like runGit, the caller decides which exit codes it accepts.
func (s *Space) runGitIn(ctx context.Context, workTree, indexFile string, args ...string) (stdout, stderr []byte, code int, err error) {
	return s.runGitFull(ctx, workTree, indexFile, args...)
}

// shadowGitIn runs git with a named work tree and index and accepts only a
// clean exit, as shadowGit does for the default work tree.
func (s *Space) shadowGitIn(ctx context.Context, workTree, indexFile string, args ...string) ([]byte, error) {
	out, _, _, err := s.runGitFull(ctx, workTree, indexFile, args...)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errGit
	}
	return out, nil
}
