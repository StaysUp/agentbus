package isolation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// A local copy carries a PRIVATE git repo, invisible to the person, that is
// the ONE mechanism for its checkpoints: it records each checkpoint as a
// commit, computes a diff and a changeset from its commits, and resets the
// copy to a checkpoint on a discard (reset --hard + clean). One mechanism
// decides what changed and what a reset restores, so the two never disagree.
//
// The repo is sealed from the person. Its GIT_DIR is in the copy's state
// folder (never the person's folder or the copy). Every call names --git-dir
// and --work-tree, carries no global or system config, no hooks, a fixed
// identity, and -- load-bearing -- no content transformation at all: the
// repo's info/attributes turns off text, filter and diff attributes, so a
// file's stored bytes are its real bytes and the SHA-256 computed from them
// is the workspace content hash the blob store and apply expect. The person's
// own repo is never touched: no refs, worktrees, reflog or config in it.
//
// A changeset still names each file by the workspace SHA-256 of its content,
// not git's object id, and still publishes the content to the blob store by
// that hash: git replaces change-detection and restore, not the blob or hash
// format. The content a changeset or a diff names is staged into the copy's
// object store (objectDir) from git, so the publish and apply paths read it
// exactly as before.

// gitShadowDir is the copy's private repo (GIT_DIR), beside its other state.
func (s *Space) gitShadowDir() string { return filepath.Join(s.state(), "git") }

// hasShadow says whether the copy carries a private git repo: a local copy
// made by Open since this landed. A snapshot-built or legacy copy has none and
// keeps its checkpoints the Merkle way.
func (s *Space) hasShadow() bool { return s.Backend == Copy && s.Shadow }

// shadowRef is the ref a checkpoint's commit is kept under, so a diff, a
// changeset and a reset find it by the checkpoint name. Names are Base or a
// turn id (safe ref names: letters, digits, _, . and -).
func shadowRef(name string) string { return "refs/checkpoints/" + name }

// zeroOID is git's all-zero object id: an absent side of a raw diff entry.
func zeroOID(sha string) bool { return strings.Trim(sha, "0") == "" }

// runGit runs git against the copy's private repo and work tree, sealed from
// the person's git config, hooks and content filters, and answers its stdout,
// its stderr and its exit code. A caller decides which exit codes it accepts,
// since git add tolerates a partially added tree.
// gitBaseArgs are the --git-dir, --work-tree and sealing flags every git call
// carries: no hooks, no signing, no auto gc, a fixed identity, no content
// transformation (stored bytes are real bytes), and none of the person's own
// config, attributes or ignore files (a global excludes file is read even with
// no global config, so a file a developer ignores globally -- notes.md, .env --
// would otherwise silently drop from every changeset).
func (s *Space) gitBaseArgs() []string { return s.gitBaseArgsWT(s.Dir) }

// gitBaseArgsWT is gitBaseArgs with the work tree named, so a call can read a
// tree other than the copy -- the person's folder, to build the "theirs" side
// of an update's three-way merge (gitupdate.go) -- under the same sealing: no
// hooks, no signing, a fixed identity, no content transformation, and none of
// the person's config, attributes or ignore files. The named work tree is only
// ever read (git add stages into the object store and a named index), never
// written, so the person's folder is untouched and its own .git is never seen.
func (s *Space) gitBaseArgsWT(workTree string) []string {
	return []string{
		"--git-dir=" + s.gitShadowDir(),
		"--work-tree=" + workTree,
		"-c", "core.hooksPath=/dev/null",
		"-c", "commit.gpgSign=false",
		"-c", "gc.auto=0",
		"-c", "user.name=agentbus",
		"-c", "user.email=agentbus@localhost",
		"-c", "core.autocrlf=false",
		"-c", "core.eol=lf",
		"-c", "core.attributesFile=/dev/null",
		"-c", "core.fileMode=true",
		"-c", "core.symlinks=true",
		"-c", "core.excludesFile=/dev/null",
	}
}

func (s *Space) runGit(ctx context.Context, args ...string) (stdout, stderr []byte, code int, err error) {
	return s.runGitFull(ctx, s.Dir, "", args...)
}

// runGitFull is runGit with the work tree and index file named: it runs from
// workTree (so a "." pathspec resolves there), sets --work-tree to it, and sets
// GIT_INDEX_FILE when indexFile is not empty. runGit uses the copy's own work
// tree and default index; an update (gitupdate.go) names a throwaway index and
// either the person's folder (to read "theirs") or a scratch directory (the
// trial merge), so it never disturbs the copy's own index or work tree.
func (s *Space) runGitFull(ctx context.Context, workTree, indexFile string, args ...string) (stdout, stderr []byte, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", append(s.gitBaseArgsWT(workTree), args...)...)
	// Run from the work tree, so a "." pathspec resolves against it, not the
	// process's own folder. --git-dir is explicit, so git never discovers the
	// work tree's own .git from here.
	cmd.Dir = workTree
	env := sealedGitEnv()
	if indexFile != "" {
		env = append(env, "GIT_INDEX_FILE="+indexFile)
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return out.Bytes(), errb.Bytes(), code, err
}

// shadowGitStdin runs git with stdin fed from in, accepting only a clean exit.
// It is used for `update-ref --stdin`, which applies several ref updates in one
// transaction.
func (s *Space) shadowGitStdin(ctx context.Context, in string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append(s.gitBaseArgs(), args...)...)
	cmd.Dir = s.Dir
	cmd.Env = sealedGitEnv()
	cmd.Stdin = strings.NewReader(in)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errGit
	}
	return nil
}

// shadowGit runs git and accepts only a clean exit. Git's stderr, which can
// name paths, is kept out of the error. A cancelled context is reported as
// such, so a stopped job does not read as a git failure.
func (s *Space) shadowGit(ctx context.Context, args ...string) ([]byte, error) {
	out, _, _, err := s.runGit(ctx, args...)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errGit
	}
	return out, nil
}

// errGit reports that the private git failed. Git's own words are left out of
// it: they can name a path, and this package's errors never do.
var errGit = errors.New("isolation: private git: command failed")

// sealedGitEnv is the environment for the private git: every inherited GIT_*
// variable stripped (an outer GIT_DIR, GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY,
// GIT_AUTHOR_* or GIT_CONFIG_* would otherwise leak in), then the few fixed
// ones this repo needs.
func sealedGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		// A real path, never a pathspec: a filename that begins with pathspec
		// magic (":(top)x") is matched as that path, not interpreted.
		"GIT_LITERAL_PATHSPECS=1",
	)
}

// gitBuiltinIgnore are the paths the private repo always leaves out, beside
// the folder's own .gitignore: the copy's own top .git (a repo copied in
// keeps one), and the usual big build folders. Every one is ANCHORED to the
// copy's top, so "build" or "target" never matches real source at a depth, and
// the list is kept short for the same reason.
var gitBuiltinIgnore = []string{
	"/.git/",
	"/node_modules/",
	"/.venv/",
	"/venv/",
	"/__pycache__/",
	"/.gradle/",
	"/.next/",
	"/target/",
	"/build/",
	"/dist/",
}

// initShadow makes the copy's private repo and takes its first commit (Base):
// the copy as opened, the folder's uncommitted edits included. It writes the
// repo's info/exclude (the built-in ignores; the folder's own .gitignore git
// respects on its own) and info/attributes (every content transformation off),
// then commits Base. A repo already there, from a retried Open, is used as it
// is. The caller sets s.Shadow and writes space.json only after this returns.
func (s *Space) initShadow(ctx context.Context) error {
	dir := s.gitShadowDir()
	// The base ref, not HEAD, says the repo is fully made: a crash between
	// init and the first commit leaves HEAD but no base, and is remade.
	if _, _, code, _ := s.runGit(ctx, "rev-parse", "--verify", "-q", shadowRef(Base)); code == 0 {
		return nil // a retry of the job that made it
	}
	if err := os.MkdirAll(filepath.Join(dir, "info"), 0o700); err != nil {
		return err
	}
	if _, err := s.shadowGit(ctx, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "info", "exclude"),
		[]byte(strings.Join(gitBuiltinIgnore, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	// info/attributes overrides any .gitattributes in the copy, so no path is
	// ever normalised, filtered, re-encoded or keyword-expanded: stored bytes
	// are real bytes. -text/-eol stop EOL conversion, -filter the clean/smudge
	// filters, -ident the $Id$ expansion, -working-tree-encoding a re-encoding
	// (UTF-16 and the like), -diff the textconv a cat-file could otherwise run.
	if err := os.WriteFile(filepath.Join(dir, "info", "attributes"),
		[]byte("* -text -eol -filter -diff -ident -working-tree-encoding\n"), 0o600); err != nil {
		return err
	}
	_, err := s.commitShadow(ctx, Base, "")
	return err
}

// shadowAdd stages the whole work tree. git add aborts the lot on an unborn
// nested repo (a turn ran `git init sub` and did not commit), so --ignore-
// errors lets it add the rest; exit 1 is that partial add and is expected,
// any other non-zero is a real failure. A committed nested repo is added as a
// gitlink, its own files left to it; a reset leaves such a repo in place and a
// clean removes one a dropped turn added (clean -ffd, below).
func (s *Space) shadowAdd(ctx context.Context) error {
	_, _, code, err := s.runGit(ctx, "add", "-A", "--ignore-errors")
	if err == nil || code == 1 {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errGit
}

// commitShadow records the copy's current files as the checkpoint name's
// commit and points its ref at it, and says whether it made a new checkpoint.
// When nothing changed since the last commit it makes none and answers false,
// as a Merkle checkpoint records nothing then; the caller names the latest
// checkpoint instead, so the history grows no commit per no-op turn. The first
// commit (Base) is allowed to be empty, so an empty folder still has a Base.
// The caller holds lock(s): a checkpoint and a diff must not run at once, and
// name is a valid git ref (gitCheckpoint checks it), so the one transaction
// below never fails after a commit object is made.
func (s *Space) commitShadow(ctx context.Context, name, parentCheckpoint string) (bool, error) {
	if err := s.shadowAdd(ctx); err != nil {
		return false, err
	}
	out, err := s.shadowGit(ctx, "write-tree")
	if err != nil {
		return false, err
	}
	tree := strings.TrimSpace(string(out))
	modes, err := s.currentModes(ctx)
	if err != nil {
		return false, err
	}
	// HEAD^{commit} is absent only before the very first commit (Base). For any
	// later checkpoint a missing HEAD is a transient failure, not a reason to
	// start a new root commit that would orphan the history.
	parent := ""
	if p, _, code, _ := s.runGit(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}"); code == 0 {
		parent = strings.TrimSpace(string(p))
		if ht, _, hc, _ := s.runGit(ctx, "rev-parse", "--verify", "-q", "HEAD^{tree}"); hc == 0 &&
			strings.TrimSpace(string(ht)) == tree {
			// git's tree is unchanged, but a permission bit it cannot store
			// (0644 <-> 0600) may have. Only when the mode map also matches the
			// latest checkpoint is this truly a no-op; otherwise record the
			// change as its own checkpoint on the same tree.
			if sameModeMap(modes, s.loadModesRaw(parentCheckpoint)) {
				return false, nil
			}
		}
	} else if name != Base {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, errGit
	}
	// The commit object is built without moving any ref (commit-tree), then
	// HEAD's branch and the checkpoint ref move together in ONE transaction. A
	// plain commit + a separate update-ref could leave HEAD advanced with no
	// checkpoint ref -- a crash or a git-rejected name between them -- and the
	// next turn would see no change and drop its edits. update-ref overwrites a
	// stale unreachable ref of the same name, so no earlier discard can block it.
	args := []string{"commit-tree", tree, "-m", "checkpoint " + name}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	cout, err := s.shadowGit(ctx, args...)
	if err != nil {
		return false, err
	}
	commit := strings.TrimSpace(string(cout))
	// The exact modes git cannot store, written BEFORE the refs move, so a
	// failed record never leaves a checkpoint without its modes.
	if err := s.writeModes(name, modes); err != nil {
		return false, err
	}
	stdin := "update refs/heads/main " + commit + "\n" +
		"update " + shadowRef(name) + " " + commit + "\n"
	if err := s.shadowGitStdin(ctx, stdin, "update-ref", "--stdin"); err != nil {
		return false, err
	}
	return true, nil
}

// shadowCheckpoints are the copy's active checkpoints, oldest first, with the
// same Checkpoint shape the Merkle checkpoints() give (Root empty: git keeps no
// snapshot id, the local fast path's state). Order is by the commit history,
// NOT by commit time -- two checkpoints a fast turn commits in the same second
// would tie on time and sort by name, which the Seq-based order check reads as
// out of order. A reset moves HEAD back, so a dropped turn's ref is left out of
// bounds of HEAD's history and drops from the active list, which is right.
func (s *Space) shadowCheckpoints(ctx context.Context) ([]Checkpoint, error) {
	refOut, err := s.shadowGit(ctx, "for-each-ref",
		"--format=%(objectname) %(refname) %(creatordate:unix)", "refs/checkpoints/")
	if err != nil {
		return nil, err
	}
	type refInfo struct {
		name string
		unix string
	}
	refOf := map[string]refInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(refOut)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		info := refInfo{name: strings.TrimPrefix(f[1], "refs/checkpoints/")}
		if len(f) >= 3 {
			info.unix = f[2]
		}
		refOf[f[0]] = info
	}
	if len(refOf) == 0 {
		return nil, nil
	}
	// The checkpoint commits reachable from HEAD, oldest first: a linear
	// history, one commit per checkpoint (a no-op turn commits nothing).
	logOut, err := s.shadowGit(ctx, "rev-list", "--reverse", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	var list []Checkpoint
	for _, oid := range strings.Split(strings.TrimSpace(string(logOut)), "\n") {
		info, ok := refOf[oid]
		if !ok {
			continue
		}
		c := Checkpoint{Name: info.name, Seq: len(list)}
		if sec, err := strconv.ParseInt(info.unix, 10, 64); err == nil && sec != 0 {
			c.Time = time.Unix(sec, 0)
		}
		list = append(list, c)
	}
	return list, nil
}

// gitCheckpoint records the checkpoint name in the private git repo and
// answers it, with the Backend.Checkpoint contract: a bad name is ErrBadName,
// a name already taken is that checkpoint, and a name under which nothing
// changed is the latest checkpoint (no new one is recorded).
func (s *Space) gitCheckpoint(ctx context.Context, name string) (Checkpoint, error) {
	if !checkpointName.MatchString(name) || !gitRefSafe(name) {
		return Checkpoint{}, ErrBadName
	}
	// The read of the checkpoints and the commit are one critical section, so
	// two concurrent checkpoints cannot both find a name absent and race.
	unlock := lock(s)
	defer unlock()
	list, err := s.shadowCheckpoints(ctx)
	if err != nil {
		return Checkpoint{}, err
	}
	if c, ok := findCheckpoint(list, name); ok {
		return c, nil
	}
	parent := ""
	if len(list) > 0 {
		parent = list[len(list)-1].Name
	}
	changed, err := s.commitShadow(ctx, name, parent)
	if err != nil {
		return Checkpoint{Name: name}, err
	}
	if !changed {
		if len(list) > 0 {
			return list[len(list)-1], nil
		}
		return Checkpoint{Name: name}, nil
	}
	return Checkpoint{Name: name, Seq: len(list)}, nil
}

// gitRefSafe rejects the checkpoint names that pass checkpointName but git's
// ref rules refuse, so a checkpoint never fails after it has committed. The
// regex already bars spaces, control bytes, /, ~, ^, :, ?, *, [, \ and @{;
// what is left is a .. run, a trailing dot, or a .lock suffix.
func gitRefSafe(name string) bool {
	return !strings.Contains(name, "..") &&
		!strings.HasSuffix(name, ".") &&
		!strings.HasSuffix(name, ".lock")
}

// findCheckpoint is the checkpoint of that name in list.
func findCheckpoint(list []Checkpoint, name string) (Checkpoint, bool) {
	for _, c := range list {
		if c.Name == name {
			return c, true
		}
	}
	return Checkpoint{}, false
}

// shadowChange is one file git reports changed between two checkpoints: its
// path, and each side's git mode and object id (a zero id means that side has
// no such file).
type shadowChange struct {
	path             string
	srcMode, dstMode string
	srcSha, dstSha   string
}

// shadowDiffRaw parses `git diff --raw -z` between two checkpoints: the git
// modes and object ids of each changed path, with no rename detection and no
// content transformation.
func (s *Space) shadowDiffRaw(ctx context.Context, from, to string) ([]shadowChange, error) {
	out, err := s.shadowGit(ctx, "diff", "--raw", "-z", "--no-renames", "--no-textconv", "--no-abbrev",
		shadowRef(from), shadowRef(to), "--")
	if err != nil {
		return nil, err
	}
	// Records are NUL-separated and alternate meta, path, meta, path, ...
	// Meta is ":<srcmode> <dstmode> <srcsha> <dstsha> <status>".
	toks := strings.Split(string(out), "\x00")
	var changes []shadowChange
	for i := 0; i+1 < len(toks); i += 2 {
		meta, path := toks[i], toks[i+1]
		if meta == "" || path == "" {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(fields) < 5 {
			continue
		}
		changes = append(changes, shadowChange{
			path:    path,
			srcMode: fields[0], dstMode: fields[1],
			srcSha: fields[2], dstSha: fields[3],
		})
	}
	return changes, nil
}

// errGitlink: a nested repo's commit pointer (git mode 160000), which is not a
// file to apply. Its own files belong to it, not the changeset.
var errGitlink = errors.New("isolation: a nested repo is tracked by its commit only")

// shadowFileSide builds the workspace file for one side of a changed path, by
// its git mode and object id, and stages its content into the copy's object
// store under the workspace SHA-256, so the publish and apply paths read it as
// they read a Merkle copy's. A symbolic link is hashed and stored as the
// workspace does, over "link:" and its target. modes is the side's recorded
// exact-mode map (the modes git cannot store, 0600 and the like): git gives the
// 0644/0755 default and the modes record overrides it where a file is
// restrictive, so no apply ever widens one and a historical side is as exact as
// the current one.
func (s *Space) shadowFileSide(ctx context.Context, path, mode, sha string, modes map[string]fs.FileMode) (*workspace.File, error) {
	switch mode {
	case "160000":
		return nil, errGitlink
	case "120000":
		target, err := s.shadowGit(ctx, "cat-file", "blob", sha)
		if err != nil {
			return nil, err
		}
		h := linkHash(string(target))
		if err := s.stageLink(string(target), h); err != nil {
			return nil, err
		}
		return &workspace.File{Size: int64(len(target)), Mode: fs.ModeSymlink | 0o777, Hash: h}, nil
	default:
		content, err := s.shadowGit(ctx, "cat-file", "blob", sha)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		h := hex.EncodeToString(sum[:])
		if err := s.stageBlob(content, h); err != nil {
			return nil, err
		}
		m := fs.FileMode(0o644)
		if mode == "100755" {
			m = 0o755
		}
		if rec, ok := modes[path]; ok {
			m = rec
		}
		return &workspace.File{Size: int64(len(content)), Mode: m, Hash: h}, nil
	}
}

// modesDir holds the exact-mode records, one JSON file per checkpoint.
func (s *Space) modesDir() string { return filepath.Join(s.state(), "modes") }

// recordModes writes the checkpoint name's exact-mode record: every tracked
// regular file whose permission bits are not git's 0644 or 0755 (0600, 0700,
// 0400 and the like), read from the working tree, which at the moment a
// checkpoint is taken holds that checkpoint's state. git decides WHAT changed;
// this only annotates the bits git cannot store, so the two never disagree.
// currentModes is the exact-mode map of the working tree now: every tracked
// regular file whose permission bits are not git's 0644 or 0755.
func (s *Space) currentModes(ctx context.Context) (map[string]string, error) {
	out, err := s.shadowGit(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	modes := map[string]string{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		fi, err := os.Lstat(filepath.Join(s.Dir, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o644 && perm != 0o755 {
			modes[p] = "0" + strconv.FormatUint(uint64(perm), 8)
		}
	}
	return modes, nil
}

// sameModeMap says whether two exact-mode maps are equal.
func sameModeMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for p, v := range a {
		if b[p] != v {
			return false
		}
	}
	return true
}

// writeModes stores the checkpoint name's mode record, or removes it when there
// is nothing restrictive to record.
func (s *Space) writeModes(name string, modes map[string]string) error {
	dst := filepath.Join(s.modesDir(), name+".json")
	if len(modes) == 0 {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(s.modesDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(modes)
	return writeFileAtomic(dst, b)
}

// loadModes is the checkpoint name's exact-mode record, as perms. A missing or
// unreadable record is no overrides: every file keeps git's mode.
func (s *Space) loadModes(name string) map[string]fs.FileMode {
	out := map[string]fs.FileMode{}
	b, err := os.ReadFile(filepath.Join(s.modesDir(), name+".json"))
	if err != nil {
		return out
	}
	var raw map[string]string
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for p, m := range raw {
		if perm, err := strconv.ParseUint(m, 8, 32); err == nil {
			out[p] = fs.FileMode(perm).Perm()
		}
	}
	return out
}

// loadModesRaw is the checkpoint name's mode record as stored (octal strings),
// for comparing one checkpoint's modes against another's.
func (s *Space) loadModesRaw(name string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.modesDir(), name+".json"))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// dropModes removes a checkpoint's mode record, as a discarded ref is pruned.
func (s *Space) dropModes(name string) {
	_ = os.Remove(filepath.Join(s.modesDir(), name+".json"))
}

// gitDefault is the perm git stores a regular file as: 0755 for an executable,
// 0644 otherwise. A file's effective mode is its mode record's entry, or this.
func gitDefault(gitmode string) fs.FileMode {
	if gitmode == "100755" {
		return 0o755
	}
	return 0o644
}

// reconcileModes sets every tracked regular file's permission bits to the
// checkpoint name's mode -- the record's entry, else git's default. git reset
// writes git's 0644/0755 (masked by the process umask), which neither restores
// a 0600 a discarded turn set nor a 0644 the umask narrowed; this makes the
// copy's modes exactly name's, so a later turn's changeset reads the right ones.
func (s *Space) reconcileModes(ctx context.Context, name string) error {
	out, err := s.shadowGit(ctx, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}
	over := s.loadModes(name)
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		gitmode, path, ok := parseLsEntry(entry)
		if !ok || (gitmode != "100644" && gitmode != "100755") {
			continue
		}
		want := gitDefault(gitmode)
		if m, ok := over[path]; ok {
			want = m
		}
		p := filepath.Join(s.Dir, filepath.FromSlash(path))
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() != want {
			_ = os.Chmod(p, want)
		}
	}
	return nil
}

// parseLsEntry splits one NUL-terminated `git ls-files -s` entry
// ("<mode> <sha> <stage>\t<path>") into its git mode and path.
func parseLsEntry(entry string) (gitmode, path string, ok bool) {
	tab := strings.IndexByte(entry, '\t')
	if tab < 0 {
		return "", "", false
	}
	fields := strings.Fields(entry[:tab])
	if len(fields) < 1 {
		return "", "", false
	}
	return fields[0], entry[tab+1:], true
}

// treeEntry is the git mode and object id of path at ref, and whether ref holds
// it as a regular file. ls-tree prints "<mode> <type> <sha>\t<path>".
func (s *Space) treeEntry(ctx context.Context, ref, path string) (gitmode, sha string, ok bool, err error) {
	out, err := s.shadowGit(ctx, "ls-tree", shadowRef(ref), "--", path)
	if err != nil {
		return "", "", false, err
	}
	line := strings.TrimRight(string(out), "\n")
	tab := strings.IndexByte(line, '\t')
	if tab < 0 {
		return "", "", false, nil
	}
	fields := strings.Fields(line[:tab])
	if len(fields) < 3 {
		return "", "", false, nil
	}
	gitmode = fields[0]
	return gitmode, fields[2], gitmode == "100644" || gitmode == "100755", nil
}

// modeOnlyChanges are the paths whose only change between from and to is a
// permission bit git cannot see (0600 <-> 0644, 0700 <-> 0755), which git's
// tree diff never reports. seen holds the paths git already reported. Each is
// returned with its git mode and object id, the same on both sides (the content
// did not change), so the shared loop builds a changed file whose mode differs.
func (s *Space) modeOnlyChanges(ctx context.Context, from, to string, modesFrom, modesTo map[string]fs.FileMode, seen map[string]bool) ([]shadowChange, error) {
	cand := map[string]bool{}
	for p := range modesFrom {
		cand[p] = true
	}
	for p := range modesTo {
		cand[p] = true
	}
	var out []shadowChange
	for p := range cand {
		if seen[p] {
			continue
		}
		// A file recorded identically on both sides (or on neither) has the same
		// effective mode -- git's exec bit is the same, since a change to it
		// would be in seen -- so skip it before forking a git ls-tree. This
		// keeps a diff O(files whose mode changed), not O(every restrictive file)
		// under a umask that makes every file non-default.
		mf, okF := modesFrom[p]
		mt, okT := modesTo[p]
		if okF == okT && mf == mt {
			continue
		}
		gm, sha, ok, err := s.treeEntry(ctx, to, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // not a regular file at to (a git-visible change, in seen)
		}
		def := gitDefault(gm)
		ef, et := def, def
		if okF {
			ef = mf
		}
		if okT {
			et = mt
		}
		if ef != et {
			out = append(out, shadowChange{path: p, srcMode: gm, dstMode: gm, srcSha: sha, dstSha: sha})
		}
	}
	return out, nil
}

// stageBlob writes content into the object store under h, when it is not there.
func (s *Space) stageBlob(content []byte, h string) error {
	dir := s.objectDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, h)
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// stageLink writes a symbolic link into the object store under h (a link's
// hash over "link:" and its target), when it is not there.
func (s *Space) stageLink(target, h string) error {
	dir := s.objectDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, h)
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	// A unique temp name, so two stagings of the same link never race on it.
	f, err := os.CreateTemp(dir, ".tmp-link-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close()
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// gitChangeset is what the checkpoint name holds that differs from Base: each
// changed file as Base held it and as name holds it, by its workspace content
// hash, with the content staged into the object store. It is the git form of
// ChangesetOf, for a copy with a shadow repo.
func (s *Space) gitChangeset(ctx context.Context, name string) (workspace.Changeset, error) {
	unlock := lock(s)
	defer unlock()
	list, err := s.shadowCheckpoints(ctx)
	if err != nil {
		return workspace.Changeset{}, err
	}
	if _, ok := findCheckpoint(list, Base); !ok {
		return workspace.Changeset{}, ErrNoCheckpoint
	}
	if _, ok := findCheckpoint(list, name); !ok {
		// A discarded checkpoint's ref may linger; only an active one counts.
		return workspace.Changeset{}, ErrNoCheckpoint
	}
	changes, err := s.shadowDiffRaw(ctx, Base, name)
	if err != nil {
		return workspace.Changeset{}, err
	}
	modesBase, modesNow := s.loadModes(Base), s.loadModes(name)
	// Git's tree diff cannot see a 0600 <-> 0644 change, so fold in the paths
	// whose only change is such a bit, or an apply would miss or mis-set it.
	seen := map[string]bool{}
	for _, ch := range changes {
		seen[ch.path] = true
	}
	extra, err := s.modeOnlyChanges(ctx, Base, name, modesBase, modesNow, seen)
	if err != nil {
		return workspace.Changeset{}, err
	}
	changes = append(changes, extra...)
	cs := workspace.Changeset{Space: s.ID, Checkpoint: name, Files: []workspace.FileChange{}}
	for _, ch := range changes {
		fc := workspace.FileChange{Path: ch.path}
		if !zeroOID(ch.srcSha) {
			f, err := s.shadowFileSide(ctx, ch.path, ch.srcMode, ch.srcSha, modesBase)
			if err != nil && !errors.Is(err, errGitlink) {
				return workspace.Changeset{}, err
			}
			fc.Base = f
		}
		if !zeroOID(ch.dstSha) {
			f, err := s.shadowFileSide(ctx, ch.path, ch.dstMode, ch.dstSha, modesNow)
			if err != nil && !errors.Is(err, errGitlink) {
				return workspace.Changeset{}, err
			}
			fc.Now = f
		}
		if fc.Base == nil && fc.Now == nil {
			continue // a gitlink changed: nothing to apply
		}
		cs.Files = append(cs.Files, fc)
	}
	sort.Slice(cs.Files, func(i, j int) bool { return cs.Files[i].Path < cs.Files[j].Path })
	return cs, nil
}

// gitDiff is what changed between two checkpoints, as workspace.Changes, built
// from the two commits and the content staged for the later side. It reuses
// workspace.Diff, so the change list, the content of small text files and the
// size limits are exactly as a Merkle diff's.
func (s *Space) gitDiff(ctx context.Context, from, to string) (workspace.Changes, error) {
	unlock := lock(s)
	defer unlock()
	list, err := s.shadowCheckpoints(ctx)
	if err != nil {
		return workspace.Changes{}, err
	}
	fa, aok := findCheckpoint(list, from)
	fb, bok := findCheckpoint(list, to)
	if !aok || !bok {
		return workspace.Changes{}, ErrNoCheckpoint
	}
	if fa.Seq > fb.Seq {
		return workspace.Changes{}, ErrOrder
	}
	changes, err := s.shadowDiffRaw(ctx, from, to)
	if err != nil {
		return workspace.Changes{}, err
	}
	modesFrom, modesTo := s.loadModes(from), s.loadModes(to)
	// Fold in the paths whose only change is a permission bit git cannot see.
	seen := map[string]bool{}
	for _, ch := range changes {
		seen[ch.path] = true
	}
	extra, err := s.modeOnlyChanges(ctx, from, to, modesFrom, modesTo, seen)
	if err != nil {
		return workspace.Changes{}, err
	}
	changes = append(changes, extra...)
	before, after := workspace.Empty(), workspace.Empty()
	for _, ch := range changes {
		if !zeroOID(ch.srcSha) {
			f, err := s.shadowFileSide(ctx, ch.path, ch.srcMode, ch.srcSha, modesFrom)
			if err != nil && !errors.Is(err, errGitlink) {
				return workspace.Changes{}, err
			}
			if f != nil {
				before.Set(ch.path, *f)
			}
		}
		if !zeroOID(ch.dstSha) {
			f, err := s.shadowFileSide(ctx, ch.path, ch.dstMode, ch.dstSha, modesTo)
			if err != nil && !errors.Is(err, errGitlink) {
				return workspace.Changes{}, err
			}
			if f != nil {
				after.Set(ch.path, *f)
			}
		}
	}
	// Content for both sides is staged in the object store, so a diff reads it
	// there by hash, exactly as a Merkle diff reads its saved files.
	objects, err := os.OpenRoot(s.objectDir())
	if err != nil {
		objects = nil
	} else {
		defer objects.Close()
	}
	end := func(rel string, f workspace.File, max int) ([]byte, bool) {
		return readEnd(objects, nil, rel, f, max)
	}
	read := func(rel string, max int) ([]byte, bool) {
		f, ok := after.File(rel)
		if !ok {
			return nil, false
		}
		return end(rel, f, max)
	}
	c := workspace.Diff(before, after, read)
	addFileDiffs(s, end, before, after, &c)
	c.From, c.To = from, to
	return c, nil
}

// restoreShadow resets the copy's work tree to the checkpoint name: dirs made
// read-only by a turn (a module cache, say) are made writable first, then a
// hard reset, then a clean of files and nested repos a dropped turn added.
// reset and clean each either complete or leave the copy as it was; the
// chmod-first makes the clean, which a read-only dir would otherwise stop
// after the reset, able to finish.
func (s *Space) restoreShadow(ctx context.Context, name string) error {
	unlock := lock(s)
	defer unlock()
	list, err := s.shadowCheckpoints(ctx)
	if err != nil {
		return err
	}
	if _, ok := findCheckpoint(list, name); !ok {
		// Only an active checkpoint (reachable from HEAD) can be a target; a
		// discarded one whose ref lingered must not resurrect its commit.
		return ErrNoCheckpoint
	}
	return s.resetTo(ctx, name)
}

// resetTo resets the copy's work tree to the checkpoint name and prunes the
// checkpoints no longer reachable from it. The caller holds lock(s) and has
// checked name is a valid target. Dirs made read-only by a turn are made
// writable first, then a hard reset, then a clean; each either completes or
// leaves the copy as it was. It is the shared body of restoreShadow (a discard)
// and an update's rollback, so an update that fails after advancing the copy
// resets it back to its prior head the same way a discard resets it.
func (s *Space) resetTo(ctx context.Context, name string) error {
	if err := makeTreeWritable(s.Dir); err != nil {
		return err
	}
	if _, err := s.shadowGit(ctx, "reset", "-q", "--hard", shadowRef(name), "--"); err != nil {
		return err
	}
	// -ffd: a second -f removes a nested repo a dropped turn added, which a
	// single -f would skip; -d removes now-empty folders too. Ignored files
	// (node_modules and the like) stay, so a discard does not rebuild them.
	if _, err := s.shadowGit(ctx, "clean", "-q", "-ffd"); err != nil {
		return err
	}
	// git reset writes git's 0644/0755, masked by the umask; reconcile every
	// tracked file to name's exact mode, so the copy is exactly name's state and
	// a later turn's changeset does not read a drifted mode and change the
	// person's file.
	if err := s.reconcileModes(ctx, name); err != nil {
		return err
	}
	// The checkpoints after name are dropped: their commits are no longer
	// reachable from HEAD, so their refs and mode records are removed, or a
	// later Checkpoint of the same name would find a stale ref and record nothing.
	return s.pruneUnreachableRefs(ctx)
}

// pruneUnreachableRefs removes every refs/checkpoints/* ref whose commit is no
// longer reachable from HEAD, as after a reset dropped the turns beyond the
// restore point. The caller holds lock(s).
func (s *Space) pruneUnreachableRefs(ctx context.Context) error {
	refOut, err := s.shadowGit(ctx, "for-each-ref", "--format=%(objectname) %(refname)", "refs/checkpoints/")
	if err != nil {
		return err
	}
	logOut, err := s.shadowGit(ctx, "rev-list", "HEAD", "--")
	if err != nil {
		return err
	}
	reachable := map[string]bool{}
	for _, oid := range strings.Fields(string(logOut)) {
		reachable[oid] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(refOut)), "\n") {
		if line == "" {
			continue
		}
		oid, ref, ok := strings.Cut(line, " ")
		if !ok || reachable[oid] {
			continue
		}
		if _, err := s.shadowGit(ctx, "update-ref", "-d", ref); err != nil {
			return err
		}
		s.dropModes(strings.TrimPrefix(ref, "refs/checkpoints/"))
	}
	return nil
}

// makeTreeWritable gives the owner write and search on every folder under
// root, so git can remove or rewrite files inside a folder a turn made
// read-only. It never widens a file's own mode, which git restores itself. It
// walks the whole copy: which folders git reset and clean touch depends on the
// folder's own .gitignore (which can un-ignore a build folder), so it cannot be
// guessed from a folder's name without asking git and disagreeing with it.
func makeTreeWritable(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if info, e := d.Info(); e == nil && info.Mode().Perm()&0o700 != 0o700 {
				_ = os.Chmod(p, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
}
