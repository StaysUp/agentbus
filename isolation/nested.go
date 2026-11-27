package isolation

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A committed repo inside a copy's folder is a repo the person keeps there: a
// vendored library, a submodule, or one of several projects under one folder.
// `git add` records a folder that holds its own .git as a commit pointer only
// (a gitlink), so a turn's edits inside it never reached a changeset, Review
// or Apply, and nothing said so (measured 2026-11-23).
//
// A copy made since 0.6.6 (Space.Nested) does not use `git add`. One rule
// decides what it records, the same at every checkpoint and on both sides of
// an update: every file under its top, a nested repo's included, except a .git
// (file or folder) at any depth and what the copy's ignore rules leave out (the
// .gitignore files along the path, the nested repo's own among them, and the
// built-in list). As git never ignores a tracked file, the rules never leave
// out a file the folder's own repo tracks (the index of the .git folder at the
// folder's top: the copy's, or the person's folder's on the other side of an
// update; each folder's own in a copy of several), nor a folder that holds one,
// and a file the record already holds stays held while it is there. Untracked
// build output stays out (#87). That part holds for a copy made since 0.6.7
// (Space.Tracked); a copy made before keeps the rules it was made with, so its
// Base and what comes after agree. A nested repo is a plain folder:
// nothing about it is kept or decided apart. The list is made by our own walk
// (walkTree), which never follows a symbolic link and asks git only which
// paths its rules ignore (check-ignore --no-index); git then hashes and records
// what changed (diff-files, update-index), and never records a path beyond a
// link. A restore resets the record (reset --hard writes every file, a nested
// repo's too) and then removes by the same walk what the record does not hold
// (cleanTree), never a .git: a repo's own history is never touched.

// stageTree records the files of the folder workTree into the index indexFile
// (the copy's own when empty) by the one rule above. ign answers which of a
// list of paths (relative, folders ending in /) the ignore rules leave out.
func (s *Space) stageTree(ctx context.Context, workTree, indexFile string, ign ignorer, held map[string]bool) error {
	listed, _, _, err := walkTree(ctx, workTree, ign)
	if err != nil {
		return err
	}
	// Every change goes into a copy of the index, put in place only once all
	// are made: a stop or a failure part way leaves the index as it was, never
	// with removals made and additions not.
	real := indexFile
	if real == "" {
		real = filepath.Join(s.gitShadowDir(), "index")
	}
	work := real + ".agentbus-stage"
	if err := copyIndex(real, work); err != nil {
		return err
	}
	defer os.Remove(work)
	indexFile = work
	out, _, _, err := s.runGitFull(ctx, workTree, indexFile, "ls-files", "-z")
	if err != nil {
		return gitErr(ctx)
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	// What the record holds is what the latest checkpoint holds (held) and
	// what the index holds: an index a stop left behind cannot make a held file
	// look new, and so ignored.
	keep := func(p string) bool { return held[p] || tracked[p] }
	var add, remove []string
	for p := range listed {
		if !tracked[p] {
			add = append(add, p)
		}
	}
	// Held but left out by the rules: still held while it is there (git never
	// ignores a tracked file); gone, it is removed.
	for p := range held {
		if listed[p] || tracked[p] {
			continue
		}
		here, err := present(workTree, p)
		if err != nil {
			return err
		}
		if here {
			add = append(add, p)
		}
	}
	for p := range tracked {
		if listed[p] {
			continue
		}
		here, err := present(workTree, p)
		if err != nil {
			return err
		}
		if !here || !keep(p) {
			remove = append(remove, p)
		}
	}
	// What changed of what is held, by git's own stat cache: only those hash.
	out, _, _, err = s.runGitFull(ctx, workTree, indexFile, "diff-files", "--name-only", "-z")
	if err != nil {
		return gitErr(ctx)
	}
	gone := map[string]bool{}
	for _, p := range remove {
		gone[p] = true
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" && tracked[p] && !gone[p] {
			add = append(add, p)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	if len(remove) > 0 {
		if err := s.gitStdinIn(ctx, workTree, indexFile, strings.Join(remove, "\x00")+"\x00", "update-index", "--force-remove", "-z", "--stdin"); err != nil {
			return err
		}
	}
	if len(add) > 0 {
		if err := s.gitStdinIn(ctx, workTree, indexFile, strings.Join(add, "\x00")+"\x00", "update-index", "--add", "--remove", "-z", "--stdin"); err != nil {
			return err
		}
	}
	if _, err := os.Stat(work); errors.Is(err, fs.ErrNotExist) {
		return nil // nothing held and nothing to hold: there is no index yet
	}
	return os.Rename(work, real)
}

// copyIndex copies the index file src to dst, or leaves dst absent when src
// is (a repo with no index yet). The copy keeps src's modification time: git
// tells a file changed in the same second as the index was written by that
// time (racy-git), and a newer time would let such a change go unseen.
func copyIndex(src, dst string) error {
	os.Remove(dst)
	fi, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// headFiles are the files the copy's latest checkpoint holds (HEAD's tree),
// by slash path; none before the first.
func (s *Space) headFiles(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	b, _, code, _ := s.runGit(ctx, "ls-tree", "-r", "-z", "--name-only", "HEAD")
	if code != 0 {
		return out
	}
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	return out
}

// cleanTree removes from the copy what its index (just reset to a checkpoint)
// does not hold, by the same walk stageTree lists from: each file, then each
// folder left empty, deepest first. What the rules leave out stays (a discard
// does not rebuild node_modules), and so does every .git: a nested repo keeps
// its own history, whichever turn made it.
func (s *Space) cleanTree(ctx context.Context) error {
	ign, err := s.ignorer(ctx, s.Dir)
	if err != nil {
		return err
	}
	listed, dirs, _, err := walkTree(ctx, s.Dir, ign)
	if err != nil {
		return err
	}
	out, err := s.shadowGit(ctx, "ls-files", "-z")
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	for p := range listed {
		if !tracked[p] {
			if err := os.Remove(filepath.Join(s.Dir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		// Only an empty folder goes; one holding a .git, an ignored file or a
		// held file is not empty and stays.
		_ = os.Remove(filepath.Join(s.Dir, filepath.FromSlash(d)))
	}
	return nil
}

// ignorer answers which of paths (relative to a work tree, folders ending in
// /) the ignore rules leave out.
type ignorer func(ctx context.Context, paths []string) (map[string]bool, error)

// ignorer is the copy's rules for the folder workTree: its private repo's
// info/exclude (the built-in list) and the .gitignore files under workTree,
// less what the folder's own repo tracks (repoFiles) in a copy made since
// 0.6.7 (Space.Tracked). For the top of a copy of several folders, that is
// what each folder's repo tracks, under its name.
func (s *Space) ignorer(ctx context.Context, workTree string) (ignorer, error) {
	inRepo := map[string]bool{}
	switch {
	case !s.Tracked:
		// A copy made before keeps the rules it was made with.
	case workTree == s.Dir && len(s.Folders) > 0:
		for _, f := range s.Folders {
			if err := s.repoFiles(ctx, filepath.Join(workTree, f.Name), f.Name+"/", inRepo); err != nil {
				return nil, err
			}
		}
	default:
		if err := s.repoFiles(ctx, workTree, "", inRepo); err != nil {
			return nil, err
		}
	}
	base := s.gitBaseArgsWT(workTree)
	return keepTracked(func(ctx context.Context, paths []string) (map[string]bool, error) {
		return checkIgnore(ctx, base, workTree, paths)
	}, inRepo), nil
}

// keepTracked is ign, except that it never leaves out a path inRepo names: a
// file the folder's own repo tracks, or a folder (ending in /) that holds one,
// so the walk enters it. What else such a folder holds the rules still leave
// out, as git does a file under an ignored folder.
func keepTracked(ign ignorer, inRepo map[string]bool) ignorer {
	if len(inRepo) == 0 {
		return ign
	}
	return func(ctx context.Context, paths []string) (map[string]bool, error) {
		out, err := ign(ctx, paths)
		if err != nil {
			return nil, err
		}
		for p := range out {
			if inRepo[p] {
				delete(out, p)
			}
		}
		return out, nil
	}
}

// repoFiles adds to inRepo, each behind prefix, the files the git repo at the
// top of folder tracks, and every folder above one (ending in /): what its own
// index holds, as git never ignores a tracked file. Only a .git folder at the
// top counts. A .git file (a linked worktree's, a submodule's) points outside
// the folder, and the copy leaves it out, so the other side of an update must
// too, or the two would disagree. A folder inside a repo, not at its top, has
// no .git and nothing tracked, and nor has a repo whose index cannot be read
// on its own (a split or sparse index). The index is read by the private repo,
// never by the folder's: its config, which in a copy the agent can write
// (core.fsmonitor runs a program on a read), is never seen.
func (s *Space) repoFiles(ctx context.Context, folder, prefix string, inRepo map[string]bool) error {
	if fi, err := os.Lstat(filepath.Join(folder, ".git")); err != nil || !fi.IsDir() {
		return nil
	}
	index := filepath.Join(folder, ".git", "index")
	if fi, err := os.Lstat(index); err != nil || !fi.Mode().IsRegular() {
		return nil // nothing added yet, or no plain file to read
	}
	out, _, _, err := s.runGitFull(ctx, folder, index, "ls-files", "-z")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		inRepo[prefix+p] = true
		for d := path.Dir(prefix + p); d != "." && !inRepo[d+"/"]; d = path.Dir(d) {
			inRepo[d+"/"] = true
		}
	}
	return nil
}

// checkIgnore runs git check-ignore --no-index over paths with the given git
// arguments and answers the ignored ones. Exit 1 is "none ignored".
func checkIgnore(ctx context.Context, base []string, workTree string, paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out, nil
	}
	cmd := exec.CommandContext(ctx, "git", append(base, "check-ignore", "--no-index", "-z", "--stdin")...)
	cmd.Dir = workTree
	// check-ignore takes no literal pathspecs, so it gets plain ones, each
	// behind ./ so a name that begins with pathspec magic (":(top)x") is read
	// as a name; it answers each path as it was given.
	env := sealedGitEnv()
	for i, kv := range env {
		if kv == "GIT_LITERAL_PATHSPECS=1" {
			env[i] = "GIT_LITERAL_PATHSPECS=0"
		}
	}
	cmd.Env = env
	var in strings.Builder
	for _, p := range paths {
		in.WriteString("./" + p + "\x00")
	}
	cmd.Stdin = strings.NewReader(in.String())
	var b bytes.Buffer
	cmd.Stdout = &b
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return nil, gitErr(ctx)
		}
	}
	for _, p := range strings.Split(b.String(), "\x00") {
		if p != "" {
			out[strings.TrimPrefix(p, "./")] = true
		}
	}
	return out, nil
}

// walkTree lists the files under root by the one rule: regular files and
// symbolic links (never followed), at any depth, nested repos' included, but
// no .git and nothing the rules leave out, a folder they leave out not entered.
// It answers the files, the folders it entered, and the folders that hold a
// .git (nested repos), by slash path. The rules are asked once per depth, for
// every entry of that depth at once. A folder that cannot be read fails the
// walk: its files are not gone, so they must not be recorded as removed. Only
// a folder that is gone by the time it is read (a turn removed it) is skipped.
func walkTree(ctx context.Context, root string, ign ignorer) (map[string]bool, []string, []string, error) {
	files := map[string]bool{}
	var dirs, repos []string
	level := []string{""}
	for len(level) > 0 {
		var ask []string
		type entry struct {
			rel string
			dir bool
		}
		var entries []entry
		for _, d := range level {
			des, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(d)))
			if err != nil {
				if d != "" && errors.Is(err, fs.ErrNotExist) {
					continue // gone while walked: nothing to list
				}
				return nil, nil, nil, errNotRead
			}
			for _, de := range des {
				if de.Name() == ".git" {
					if d != "" {
						repos = append(repos, d)
					}
					continue
				}
				rel := path.Join(d, de.Name())
				switch t := de.Type(); {
				case t.IsDir():
					entries = append(entries, entry{rel, true})
					ask = append(ask, rel+"/")
				case t.IsRegular(), t&fs.ModeSymlink != 0:
					entries = append(entries, entry{rel, false})
					ask = append(ask, rel)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		ignored, err := ign(ctx, ask)
		if err != nil {
			return nil, nil, nil, err
		}
		level = level[:0:0]
		for _, e := range entries {
			if e.dir {
				if !ignored[e.rel+"/"] {
					dirs = append(dirs, e.rel)
					level = append(level, e.rel)
				}
			} else if !ignored[e.rel] {
				files[e.rel] = true
			}
		}
	}
	sort.Strings(repos)
	return files, dirs, repos, nil
}

// errNotRead: a folder of the copy or of the person's folder could not be
// read, so what it holds is not known; nothing is recorded as removed.
var errNotRead = errors.New("isolation: a folder could not be read, so its files were not recorded")

// present says whether rel is a file or a link under root that is reached
// without passing a link: every folder on the way a real folder. A path that
// cannot be looked at (a folder on the way not readable) is an error, never an
// absent file.
func present(root, rel string) (bool, error) {
	cur := root
	parts := strings.Split(rel, "/")
	for i, c := range parts {
		if c == ".git" {
			return false, nil
		}
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, errNotRead
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return false, nil
		}
		if i == len(parts)-1 && !(fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0) {
			return false, nil
		}
	}
	return true, nil
}

// reposUnder are the folders under root (not root itself) that hold a .git,
// by slash path, whatever the ignore rules say; links are not followed.
func reposUnder(root string) ([]string, error) {
	var repos []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errNotRead
		}
		if d.Name() == ".git" && p != filepath.Join(root, ".git") {
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			repos = append(repos, filepath.ToSlash(rel))
			if d.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	return repos, err
}

// ErrRepoInTheWay: resetting the copy to a checkpoint would write a file where
// the copy holds a nested repo, which would remove the repo and its history.
// Nothing is reset; the person or the agent moves the repo first.
var ErrRepoInTheWay = errors.New("the copy holds a git repository where that state has a file, so going back would remove the repository and its history; move the repository, then try again")

// guardRepos refuses (ErrRepoInTheWay) a reset of the copy to checkpoint name
// when name has a file at the folder of a nested repo the copy holds, or at a
// folder above one: git's reset would remove that folder, its .git with it.
// Every repo counts, an ignored folder's too: the history is the repo's, not
// the record's. The caller made the copy's folders readable first.
func (s *Space) guardRepos(ctx context.Context, name string) error {
	if !s.Nested {
		return nil
	}
	repos, err := reposUnder(s.Dir)
	if err != nil || len(repos) == 0 {
		return err
	}
	out, err := s.shadowGit(ctx, "ls-tree", "-r", "-z", "--name-only", shadowRef(name))
	if err != nil {
		return err
	}
	files := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files[p] = true
		}
	}
	for _, r := range repos {
		for p := r; p != "." && p != ""; p = path.Dir(p) {
			if files[p] {
				return ErrRepoInTheWay
			}
		}
	}
	return nil
}

// gitStdinIn runs git with stdin fed from in, against workTree and indexFile
// (the copy's own index when empty), accepting only a clean exit.
func (s *Space) gitStdinIn(ctx context.Context, workTree, indexFile, in string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append(s.gitBaseArgsWT(workTree), args...)...)
	cmd.Dir = workTree
	env := sealedGitEnv()
	if indexFile != "" {
		env = append(env, "GIT_INDEX_FILE="+indexFile)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(in)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errGit
	}
	return nil
}

// folderIgnores are a copy's built-in ignores: the list every copy leaves out
// at its top, or, for a copy of several folders, at each folder's top, so a
// folder sitting under the copy's top is left the same as a copy of it alone.
func folderIgnores(folders []SpaceFolder) []string {
	if len(folders) == 0 {
		return append([]string(nil), gitBuiltinIgnore...)
	}
	// The top of a copy of several folders holds the folders by name, so the
	// list is anchored at each folder's top only: a folder named build is a
	// folder of the person's, never a build output to leave out.
	var out []string
	for _, f := range folders {
		for _, p := range gitBuiltinIgnore {
			out = append(out, "/"+escapeIgnore(f.Name)+p)
		}
	}
	return out
}

// escapeIgnore escapes a path for a gitignore pattern, so it matches that path
// and nothing else: each glob character, backslash and space is taken as
// itself.
func escapeIgnore(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '\\', '*', '?', '[', ']', ' ', '!', '#':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cleanCopy removes what the copy holds that its index, just reset to a
// checkpoint, does not: by the one rule for a copy made since 0.6.6
// (cleanTree), else git's clean as always. -ffd: a second -f removes a nested
// repo a dropped turn added, -d empty folders; ignored files stay.
func (s *Space) cleanCopy(ctx context.Context) error {
	if s.Nested {
		return s.cleanTree(ctx)
	}
	_, err := s.shadowGit(ctx, "clean", "-q", "-ffd")
	return err
}
