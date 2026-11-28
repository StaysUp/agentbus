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

	"github.com/staysup/agentbus/workspace"
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
// (file or folder, in any case: isDotGit) at any depth and what git says is
// not the work, and a file
// the record already holds stays held while it is there, as git never ignores
// a tracked file. Since 0.6.7 git is asked at every checkpoint, of the repo
// that owns each path (gitwork.go): what it tracks is the work, and its own
// rules alone say what else is left out. A held or tracked file is found by
// the disk, under the name the folder lists it by (spelled), so neither case
// nor Unicode form can hide it. The list is made by our own walk
// (walkTree), which never follows a symbolic link and asks git only which
// paths its rules ignore (check-ignore --no-index); git then hashes and records
// what changed (diff-files, update-index), and never records a path beyond a
// link. A restore resets the record (reset --hard writes every file, a nested
// repo's too) and then removes by the same walk what the record does not hold
// (cleanTree), never a .git: a repo's own history is never touched.

// stageTree records the files of the folder workTree into the index indexFile
// (the copy's own when empty) by the one rule above. rules is git's answer,
// asked as the walk goes, of which files there are the work (gitwork.go).
func (s *Space) stageTree(ctx context.Context, workTree, indexFile string, rules treeRules, held map[string]bool) error {
	// Every change goes into a copy of the index, put in place only once all
	// are made: a stop or a failure part way leaves the index as it was, never
	// with removals made and additions not.
	real := indexFile
	if real == "" {
		real = filepath.Join(s.gitShadowDir(), "index")
	}
	// When the index was last written (copyIndex keeps it): git's stat cache
	// is trusted only for a file changed in no way since well before then.
	var written int64
	if fi, err := os.Stat(real); err == nil {
		written = fi.ModTime().UnixNano()
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
	// look new, and so ignored. Each is found where the disk has it and named
	// as the folder lists it (spelled), so the rules never leave one out: a
	// file leaves the record only when the disk no longer has it. What each
	// repo in the tree tracks now is added as the walk finds the repo.
	want := map[string]bool{}
	for p := range held {
		want[p] = true
	}
	for p := range tracked {
		want[p] = true
	}
	keep, err := spelled(workTree, want)
	if err != nil {
		return err
	}
	listed, _, _, err := walkTree(ctx, workTree, rules, keep)
	if err != nil {
		return err
	}
	var add, remove []string
	for p := range listed {
		if !tracked[p] {
			add = append(add, p)
		}
	}
	// What the index holds that the folder does not list by that name: gone,
	// or now spelt another way (added above under the listing's name).
	for p := range tracked {
		if !listed[p] {
			remove = append(remove, p)
		}
	}
	// What changed of what is held, by git's own stat cache: only those hash.
	out, _, _, err = s.runGitFull(ctx, workTree, indexFile, "diff-files", "--name-only", "-z")
	if err != nil {
		return gitErr(ctx)
	}
	changed := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" && tracked[p] && listed[p] {
			changed[p] = true
		}
	}
	// git's cache compares a change time to the second, so a file edited
	// with its time put back in the second the index was written reads as
	// unchanged. One rule decides instead, the one every checkpoint keeps a
	// hash by (workspace.Keeps): a file whose change or modification time
	// is not well before the index was written is taken out and hashed
	// again.
	for p := range tracked {
		if !listed[p] || changed[p] {
			continue
		}
		fi, err := os.Lstat(filepath.Join(workTree, filepath.FromSlash(p)))
		if err != nil {
			return errNotRead
		}
		self := workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}
		if written == 0 || !workspace.Keeps(self, fi, written) {
			remove = append(remove, p)
			changed[p] = true
		}
	}
	for p := range changed {
		add = append(add, p)
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
	w, err := s.copyWork(ctx)
	if err != nil {
		return err
	}
	defer w.close()
	listed, dirs, _, err := walkTree(ctx, s.Dir, w, nil)
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
// .git (nested repos), by slash path. As it lists each folder it tells rules
// whether a .git is there, and keeps whatever the repo there tracks, the
// folders on the way to it entered. The rules are asked once per depth, for
// every entry of that depth at once. A folder that cannot be read fails the
// walk: its files are not gone, so they must not be recorded as removed. Only
// a folder that is gone by the time it is read (a turn removed it) is skipped.
func walkTree(ctx context.Context, root string, rules treeRules, held map[string]bool) (map[string]bool, []string, []string, error) {
	// A folder on the way to a kept file is entered, whatever the rules say.
	keep, keepDirs := map[string]bool{}, map[string]bool{}
	hold := func(m map[string]bool) {
		for p := range m {
			keep[p] = true
			for d := path.Dir(p); d != "."; d = path.Dir(d) {
				keepDirs[d] = true
			}
		}
	}
	hold(held)
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
			// A repo is here when the disk finds a .git, as git looks it up:
			// on a disk that ignores case, a .GIT is that .git.
			hasGit := false
			for _, de := range des {
				hasGit = hasGit || isDotGit(de.Name())
			}
			hasGit = hasGit && holdsGit(filepath.Join(root, filepath.FromSlash(d)))
			if hasGit && d != "" {
				repos = append(repos, d)
			}
			tracked, err := rules.repo(ctx, d, hasGit)
			if err != nil {
				return nil, nil, nil, err
			}
			hold(tracked)
			for _, de := range des {
				if isDotGit(de.Name()) {
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
		ignored, err := rules.ignored(ctx, ask)
		if err != nil {
			return nil, nil, nil, err
		}
		level = level[:0:0]
		for _, e := range entries {
			if e.dir {
				// A folder holding its own .git is a repo: it, not a rule of
				// the repo above, says what in it is the work (gitwork.go),
				// so it is entered before any rule is asked about it.
				if !ignored[e.rel+"/"] || keepDirs[e.rel] || holdsGit(filepath.Join(root, filepath.FromSlash(e.rel))) {
					dirs = append(dirs, e.rel)
					level = append(level, e.rel)
				}
			} else if !ignored[e.rel] || keep[e.rel] {
				files[e.rel] = true
			}
		}
	}
	sort.Strings(repos)
	return files, dirs, repos, nil
}

// holdsGit says whether the folder dir holds a .git (folder or file): a repo
// of its own, a nested one or a submodule. The disk is asked, as git asks it,
// so a .GIT counts where the disk takes it as .git.
func holdsGit(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// isDotGit says whether name is .git in any case. git never records a path
// through such a name, whatever its case and on every disk (its verify_path),
// so none is ever the work, and a walk never lists or removes what one holds.
func isDotGit(name string) bool {
	return strings.EqualFold(name, ".git")
}

// errSpelling: the disk has a file by a name its folder does not list, and no
// one listed name is another spelling of it, so which file it is is not
// known; nothing is recorded rather than the file taken as gone.
var errSpelling = errors.New("isolation: a file's name could not be matched to how its folder lists it, so nothing was recorded")

// errNotRead: a folder of the copy or of the person's folder could not be
// read, so what it holds is not known; nothing is recorded as removed.
var errNotRead = errors.New("isolation: a folder could not be read, so its files were not recorded")

// spelled finds each of paths under root as the folder lists it: the name
// its folder's listing gives the file, and that name only. A name is looked
// up as written first; where the listing has no such name, the disk is asked
// which listed name, another spelling of it (workspace.SameName), is the same
// file (os.SameFile), so a name the disk takes
// as another spelling of it (case on a disk that does not tell case apart,
// composed or decomposed Unicode on one that does not tell them apart) finds
// the file under its listed name, and on a disk that tells them apart finds
// nothing. That is how git, with core.ignorecase and core.precomposeunicode,
// matches its index to a folder, but with the disk's own rule, not a fold of
// ours. Only a regular file or a link counts, reached through real folders,
// never a .git. A path not found is left out; a folder on the way that cannot
// be read is an error, never an absent file.
func spelled(root string, paths map[string]bool) (map[string]bool, error) {
	out := map[string]bool{}
	// Each folder's listing, read once: its names in order, and each name's
	// kind as the listing gives it (no look-up of its own).
	type listing struct {
		names []string
		kind  map[string]fs.FileMode
	}
	lists := map[string]*listing{}
	list := func(dir string) (*listing, error) {
		if l, ok := lists[dir]; ok {
			return l, nil
		}
		des, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, errNotRead
		}
		l := &listing{kind: map[string]fs.FileMode{}}
		for _, de := range des {
			l.names = append(l.names, de.Name())
			l.kind[de.Name()] = de.Type()
		}
		lists[dir] = l
		return l, nil
	}
	for p := range paths {
		dir, ok := "", true
		parts := strings.Split(p, "/")
		for i, c := range parts {
			l, err := list(dir)
			if err != nil {
				return nil, err
			}
			name := c
			if _, listed := l.kind[c]; !listed {
				// The disk has a name it takes as c, or none. Only a listed
				// name that is another spelling of c may stand in for it
				// (workspace.SameName), and only the one the look-up of c lands
				// on: a hard link under another name is the same file, never
				// the same name. Where the disk has c but no one listed name
				// is that spelling, nothing is recorded rather than the file
				// taken as gone.
				fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path.Join(dir, c))))
				if err != nil {
					ok = false
					break
				}
				name = ""
				for _, n := range l.names {
					if !workspace.SameName(n, c) {
						continue
					}
					fn, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path.Join(dir, n))))
					if err != nil || !os.SameFile(fi, fn) {
						continue
					}
					if name != "" {
						return nil, errSpelling
					}
					name = n
				}
				if name == "" {
					return nil, errSpelling
				}
			}
			if name == "" || isDotGit(name) {
				ok = false
				break
			}
			kind := l.kind[name]
			last := i == len(parts)-1
			if !last && !kind.IsDir() || last && !(kind.IsRegular() || kind&fs.ModeSymlink != 0) {
				ok = false
				break
			}
			dir = path.Join(dir, name)
		}
		if ok {
			out[dir] = true
		}
	}
	return out, nil
}

// reposUnder are the folders under root (not root itself) that hold a .git,
// by slash path, whatever the ignore rules say; links are not followed.
func reposUnder(root string) ([]string, error) {
	var repos []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errNotRead
		}
		if isDotGit(d.Name()) && filepath.Dir(p) != root {
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

// commonDir is the git folder that holds a repo's config and info: gitdir
// itself, or for a worktree the main repo's, which its commondir file names.
func commonDir(gitdir string) string {
	b, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return gitdir
	}
	p := strings.TrimSpace(string(b))
	if !filepath.IsAbs(p) {
		p = filepath.Join(gitdir, p)
	}
	return filepath.Clean(p)
}

// globalExcludes is the excludes file git status reads for this user when no
// repo names one: the user's core.excludesFile when set ("" is none), else
// git's default, $XDG_CONFIG_HOME/git/ignore or ~/.config/git/ignore. Read by
// git from the user's own config, the only part of it the private repo reads.
func (s *Space) globalExcludes(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "git", append(s.gitBaseArgsWT(s.Dir), "config", "--global", "--get", "--path", "core.excludesFile")...)
	cmd.Dir = s.Dir
	var env []string
	for _, kv := range sealedGitEnv() {
		if kv != "GIT_CONFIG_GLOBAL=/dev/null" {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out)) // set: "" says none, as git reads it
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "git", "ignore")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "git", "ignore")
	}
	return ""
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
