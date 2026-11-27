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
	"strconv"
	"strings"
)

// Which files of a tree are the work is git's answer, asked again at every
// checkpoint and on both sides of an update, of the repo that owns each path,
// as git itself answers it. AgentBus keeps no list of its own between two
// checkpoints: only the record, from which a file leaves when the disk no
// longer has it (stageTree), as git never ignores a file its index holds.
//
//   - A path belongs to the deepest repo above it: a folder holding a .git
//     (the folder's own, a nested repo, a submodule), or a folder's top. Its
//     git folder is found as git finds it: a .git folder, or a .git file
//     read relative to where it sits (a submodule's "gitdir: ../.git/modules/x").
//     A folder whose .git is a file (a worktree, a submodule opened as the
//     folder) has it read at Open into the copy's state (saveFolderGit), since
//     the copy holds no .git there for an agent's git to write through.
//   - That repo's index says what it tracks: each such file is the work,
//     whatever any rule says, under the name the disk lists it by (spelled).
//   - Its rules say what else is left out, and only its: its .gitignore files,
//     its info/exclude, its core.excludesFile (relative to its work tree's top,
//     as git reads it), else the user's; a parent repo's rules never reach
//     into a nested one, as git status there does not read them. A folder's
//     top also gets the built-in list (gitBuiltinIgnore), less each rule git
//     matches to a file the repo tracks.
//
// Every question goes to git through a scratch git folder of the repo's object
// format, which reads the repo's index and rule files only: none of the
// person's config, hooks or programs runs, and names compare as the private
// repo's probe of the disk found (core.ignorecase, core.precomposeunicode).

// treeRules is what a walk asks of a tree (walkTree): for each folder it
// lists, the files the repo there tracks, if one is; and which of a batch of
// paths the owning repos' rules leave out.
type treeRules interface {
	repo(ctx context.Context, dir string, hasGit bool) (map[string]bool, error)
	ignored(ctx context.Context, paths []string) (map[string]bool, error)
}

// gitWork answers treeRules for the tree root, by git.
type gitWork struct {
	s     *Space
	root  string
	tops  map[string]bool         // folders that are a folder's top ("" for root)
	saved func(top string) string // the git folder saveFolderGit kept for a top, or ""
	tmp   string
	fold  []string
	glob  string // the user's excludes file, or ""
	n     int
	regs  map[string][]string // a repo's folder: its check-ignore git arguments
}

// newWork is the rules of the tree root, whose folders' tops are tops; saved,
// when not nil, finds a top's git folder kept at Open. close removes its
// scratch folders.
func (s *Space) newWork(ctx context.Context, root string, tops []string, saved func(string) string) (*gitWork, error) {
	if err := os.MkdirAll(s.state(), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(s.state(), ".work-")
	if err != nil {
		return nil, err
	}
	w := &gitWork{s: s, root: root, tops: map[string]bool{}, saved: saved, tmp: tmp, regs: map[string][]string{}}
	for _, t := range tops {
		w.tops[t] = true
	}
	for _, k := range []string{"core.ignorecase", "core.precomposeunicode"} {
		if out, _, code, _ := s.runGit(ctx, "config", "--get", k); code == 0 {
			w.fold = append(w.fold, "-c", k+"="+strings.TrimSpace(string(out)))
		}
	}
	w.glob = s.globalExcludes(ctx)
	return w, nil
}

func (w *gitWork) close() { os.RemoveAll(w.tmp) }

// copyWork is the rules of the copy itself.
func (s *Space) copyWork(ctx context.Context) (*gitWork, error) {
	tops := []string{""}
	if len(s.Folders) > 0 {
		tops = tops[:0]
		for _, f := range s.Folders {
			tops = append(tops, f.Name)
		}
	}
	return s.newWork(ctx, s.Dir, tops, func(top string) string {
		d := s.folderGitDir(top)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		return ""
	})
}

// errIndex: a repo's index is there but git could not read it, so what the
// repo tracks is not known; nothing is recorded rather than its files left out.
var errIndex = errors.New("isolation: a git repository's index could not be read, so its files were not recorded")

func (w *gitWork) repo(ctx context.Context, dir string, hasGit bool) (map[string]bool, error) {
	top := w.tops[dir]
	if !top && !hasGit {
		return nil, nil
	}
	if _, ok := w.regs[dir]; ok {
		return nil, nil
	}
	work := filepath.Join(w.root, filepath.FromSlash(dir))
	// A .git in the tree wins: one an agent made in the copy is that repo
	// now. The git folder kept at Open stands in only where the copy holds
	// no .git (a worktree's or a submodule's, whose .git file went).
	gitDir := ""
	if hasGit {
		gitDir = gitDirOf(work)
	}
	if gitDir == "" && top && w.saved != nil {
		gitDir = w.saved(dir)
	}
	w.n++
	sd := filepath.Join(w.tmp, strconv.Itoa(w.n))
	for _, sub := range []string{"info", "objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(sd, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(sd, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		return nil, err
	}
	var tracked []string
	exfile, common := "", ""
	if gitDir != "" {
		common = commonDir(gitDir)
		format := ""
		format, exfile = w.repoConfig(ctx, filepath.Join(common, "config"))
		if format != "" && format != "sha1" {
			cfg := "[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = " + format + "\n"
			if err := os.WriteFile(filepath.Join(sd, "config"), []byte(cfg), 0o600); err != nil {
				return nil, err
			}
		}
		index := filepath.Join(gitDir, "index")
		if fi, err := os.Stat(index); err == nil {
			if !fi.Mode().IsRegular() {
				return nil, errIndex
			}
			out, err := w.git(ctx, sd, work, index, "ls-files", "-z")
			if err != nil {
				return nil, gitOr(ctx, errIndex)
			}
			for _, p := range strings.Split(string(out), "\x00") {
				if p != "" {
					tracked = append(tracked, p)
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, errIndex
		}
	}
	// The excludes file: the built-in list at a folder's top, less what git
	// matches to a tracked file, then the repo's own excludes file, whose
	// rules (a "!" among them) win over the list as later lines do.
	var lines []string
	if top {
		var builtin []string
		for _, r := range gitBuiltinIgnore {
			if r != "/.git/" {
				builtin = append(builtin, r)
			}
		}
		// Every built-in rule is a folder at the top ("/name/"), so git needs
		// one tracked path under each top-level name to say which it takes.
		one := map[string]bool{}
		var samples []string
		for _, p := range tracked {
			if first, _, deep := strings.Cut(p, "/"); deep && !one[first] {
				one[first] = true
				samples = append(samples, p)
			}
		}
		used, err := w.matching(ctx, sd, builtin, samples)
		if err != nil {
			return nil, err
		}
		for i, r := range builtin {
			if !used[i] {
				lines = append(lines, r)
			}
		}
	}
	if gitDir != "" {
		if exfile == "" {
			exfile = w.glob
		}
		if exfile != "" {
			if !filepath.IsAbs(exfile) {
				exfile = filepath.Join(work, exfile) // as git reads it: from the work tree's top
			}
			if b, err := os.ReadFile(exfile); err == nil {
				lines = append(lines, strings.TrimRight(string(b), "\n"))
			}
		}
		// The repo's info/exclude, read only after the list was matched, so
		// none of its rules hides a built-in one git matched.
		if b, err := os.ReadFile(filepath.Join(common, "info", "exclude")); err == nil {
			if err := os.WriteFile(filepath.Join(sd, "info", "exclude"), b, 0o600); err != nil {
				return nil, err
			}
		}
	}
	excludes := filepath.Join(sd, "excludes")
	if err := os.WriteFile(excludes, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	w.regs[dir] = w.args(sd, work, excludes)
	want := map[string]bool{}
	for _, p := range tracked {
		want[path.Join(dir, p)] = true
	}
	return spelled(w.root, want)
}

// args are the sealed git arguments of the scratch git folder sd over the work
// tree work, its excludes file excludes.
func (w *gitWork) args(sd, work, excludes string) []string {
	args := w.s.gitBaseArgsWT(work)
	args[0] = "--git-dir=" + sd
	for i, a := range args {
		if a == "core.excludesFile=/dev/null" {
			args[i] = "core.excludesFile=" + excludes
		}
	}
	return append(append(args, w.fold...), "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false")
}

// git runs git through the scratch folder sd over work, reading index.
func (w *gitWork) git(ctx context.Context, sd, work, index string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append(w.args(sd, work, "/dev/null"), args...)...)
	cmd.Dir = work
	cmd.Env = append(sealedGitEnv(), "GIT_INDEX_FILE="+index)
	return cmd.Output()
}

// repoConfig is the object format and core.excludesFile a repo's config file
// sets, read by git from that file only (nothing run, no includes).
func (w *gitWork) repoConfig(ctx context.Context, config string) (format, excludes string) {
	if _, err := os.Stat(config); err != nil {
		return "", ""
	}
	out, _, code, _ := w.s.runGit(ctx, "config", "--file", config, "--path", "--get-regexp", `^(extensions\.objectformat|core\.excludesfile)$`)
	if code != 0 {
		return "", ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(l, " ")
		switch strings.ToLower(k) {
		case "extensions.objectformat":
			format = strings.ToLower(strings.TrimSpace(v))
		case "core.excludesfile":
			excludes = strings.TrimSpace(v)
		}
	}
	return format, excludes
}

// matching says which of rules (by index) git, as names compare here, takes
// to leave out one of paths. git runs against an empty work tree, so only the
// rules are read.
func (w *gitWork) matching(ctx context.Context, sd string, rules, paths []string) (map[int]bool, error) {
	used := map[int]bool{}
	if len(paths) == 0 {
		return used, nil
	}
	file := filepath.Join(sd, "builtin")
	if err := os.WriteFile(file, []byte(strings.Join(rules, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	empty := filepath.Join(sd, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", append(w.args(sd, empty, file), "check-ignore", "--no-index", "-v", "-z", "--stdin")...)
	cmd.Dir = empty
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
	// Records of four fields: source, line, pattern, path.
	f := strings.Split(b.String(), "\x00")
	for k := 0; k+3 < len(f); k += 4 {
		if f[k] != file {
			continue
		}
		if n, err := strconv.Atoi(f[k+1]); err == nil && n >= 1 && n <= len(rules) {
			used[n-1] = true
		}
	}
	return used, nil
}

// ignored asks each path's own repo, all of one repo's paths at once.
func (w *gitWork) ignored(ctx context.Context, paths []string) (map[string]bool, error) {
	groups := map[string][]string{}
	for _, p := range paths {
		d, ok := w.owner(p)
		if !ok {
			continue // the top of a copy of several: the folders, never left out
		}
		rel := p
		if d != "" {
			rel = p[len(d)+1:]
		}
		groups[d] = append(groups[d], rel)
	}
	out := map[string]bool{}
	for d, rels := range groups {
		got, err := checkIgnore(ctx, w.regs[d], filepath.Join(w.root, filepath.FromSlash(d)), rels)
		if err != nil {
			return nil, err
		}
		for r := range got {
			if d != "" {
				r = d + "/" + r
			}
			out[r] = true
		}
	}
	return out, nil
}

// owner is the deepest repo folder strictly above p (a folder ends in /).
func (w *gitWork) owner(p string) (string, bool) {
	q := strings.TrimSuffix(p, "/")
	for d := path.Dir(q); ; d = path.Dir(d) {
		if d == "." {
			d = ""
		}
		if _, ok := w.regs[d]; ok {
			return d, true
		}
		if d == "" {
			return "", false
		}
	}
}

// gitDirOf is the git folder of the repo whose work tree is work, as git finds
// it: work/.git when a folder, else the folder a .git file names, relative to
// work; "" when there is none.
func gitDirOf(work string) string {
	p := filepath.Join(work, ".git")
	fi, err := os.Lstat(p)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return p
	}
	if !fi.Mode().IsRegular() {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	g, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	g = strings.TrimSpace(g)
	if !filepath.IsAbs(g) {
		g = filepath.Join(work, g)
	}
	return filepath.Clean(g)
}

// folderGitDir is where the copy keeps the git folder of the folder at top
// (saveFolderGit).
func (s *Space) folderGitDir(top string) string {
	if top == "" {
		return filepath.Join(s.state(), "folder-git", "one")
	}
	return filepath.Join(s.state(), "folder-git", "of", top)
}

// saveFolderGit keeps, for a folder whose .git is a file (a worktree, or a
// submodule opened as the folder), what git reads of its repo to answer which
// files are the work: the index (and a split index's shared part), the config
// and info/exclude. The copy holds no .git there, so an agent's git never
// writes through it into the person's repo; this is the copy's view of that
// repo, as a copied .git folder is for any other. The .git file is read
// relative to the folder in the person's folder (src), as git reads it.
func saveFolderGit(src, dst string) error {
	fi, err := os.Lstat(filepath.Join(src, ".git"))
	if err != nil || fi.IsDir() {
		return nil
	}
	gitDir := gitDirOf(src)
	if gitDir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(dst, "info"), 0o700); err != nil {
		return err
	}
	if err := copyIndex(filepath.Join(gitDir, "index"), filepath.Join(dst, "index")); err != nil {
		return err
	}
	shared, _ := filepath.Glob(filepath.Join(gitDir, "sharedindex.*"))
	for _, f := range shared {
		if err := copyIndex(f, filepath.Join(dst, filepath.Base(f))); err != nil {
			return err
		}
	}
	common := commonDir(gitDir)
	for _, f := range []string{"config", filepath.Join("info", "exclude")} {
		if b, err := os.ReadFile(filepath.Join(common, f)); err == nil {
			if err := os.WriteFile(filepath.Join(dst, f), b, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// gitOr is the context's error when it was stopped, else err.
func gitOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
