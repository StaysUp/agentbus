// Package workspace is the folder a session's turns run in, and what a job
// changed there. Package isolation makes a session's own copy, with the git
// worktree functions here; a folder used as it is needs nothing.
//
// Changed files: the runner takes a Snapshot of the whole workspace before a
// job's turn starts and again when the job ends, and records the difference
// in the job's result. The Snapshot walks every file, untracked and
// git-ignored ones included, so it sees what git diff does not: a build
// output, a file a hook wrote. In a git workspace each change also says
// whether git tracks, ignores or does not know the file, and the commit HEAD
// named before and after.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Limits of one Snapshot and of the changes kept in a result.
const (
	MaxSnapshotFiles = 200000
	MaxChanges       = 1000
)

// entry is one file of a Snapshot.
type entry struct {
	size    int64
	mode    fs.FileMode
	modTime int64
	hash    string
}

// Snapshot is a workspace's files, by slash-separated path from its root.
type Snapshot struct {
	files     map[string]entry
	truncated bool // MaxSnapshotFiles was reached
	git       bool // the root is a git work tree
	head      string
	tree      string // in git: the tree of every file but the ignored ones, for a diff
}

// Take walks root. A file whose size and modification time match
// prev keeps prev's hash, so the end of a job hashes only what changed. The
// root's .git, a folder in a repo or a file in a worktree, is skipped. A
// symbolic link is recorded by its target, never followed.
func Take(root string, prev *Snapshot) (*Snapshot, error) {
	s := &Snapshot{files: map[string]entry{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable: left out
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if len(s.files) >= MaxSnapshotFiles {
			s.truncated = true
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		e := entry{size: info.Size(), mode: info.Mode(), modTime: info.ModTime().UnixNano()}
		if old, ok := prevEntry(prev, rel); ok && old.size == e.size && old.modTime == e.modTime && old.mode == e.mode {
			e.hash = old.hash
		} else if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			e.hash = hashBytes([]byte("link:" + target))
		} else if info.Mode().IsRegular() {
			h, err := hashFile(p)
			if err != nil {
				return nil
			}
			e.hash = h
		} else {
			return nil // a socket, a device: not a file of the work
		}
		s.files[rel] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	if head, ok := gitHead(root); ok {
		s.git, s.head = true, head
		s.tree, _ = gitTree(root, head)
	}
	return s, nil
}

// gitTree writes the tree of root's files as they are on disk, untracked
// ones included and ignored ones left out, through an index of its own: the
// repo's index, HEAD and refs are never touched. The file contents go into
// the repo's object store, which git removes in time if nothing names them.
func gitTree(root, head string) (string, error) {
	dir, err := os.MkdirTemp("", "agentbus-runner-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if head != "" {
		if _, err := gitEnv(root, idx, nil, "read-tree", head); err != nil {
			return "", err
		}
	}
	if _, err := gitEnv(root, idx, nil, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	out, err := gitEnv(root, idx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// File is one file of a Snapshot. Hash is the SHA-256 of its content, or of
// "link:" and its target for a symbolic link.
type File struct {
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime int64       `json:"mtime"`
	Hash    string      `json:"sha256"`
}

// File is the file at rel, a slash-separated path from the root.
func (s *Snapshot) File(rel string) (File, bool) {
	e, ok := s.files[rel]
	return File{Size: e.size, Mode: e.mode, ModTime: e.modTime, Hash: e.hash}, ok
}

// Paths are the snapshot's files, sorted.
func (s *Snapshot) Paths() []string {
	out := make([]string, 0, len(s.files))
	for p := range s.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Set records f at rel, such as the hash of a copy taken after the walk.
func (s *Snapshot) Set(rel string, f File) {
	s.files[rel] = entry{size: f.Size, mode: f.Mode, modTime: f.ModTime, hash: f.Hash}
}

// Delete leaves rel out of the snapshot.
func (s *Snapshot) Delete(rel string) { delete(s.files, rel) }

// Len is the number of files.
func (s *Snapshot) Len() int { return len(s.files) }

// Head is the commit HEAD named, in a git work tree; else "".
func (s *Snapshot) Head() string { return s.head }

// Truncated: the walk stopped at MaxSnapshotFiles, so files are missing.
func (s *Snapshot) Truncated() bool { return s.truncated }

// Changed are the paths whose file differs between s and o, by hash or
// mode, or that only one of them has, sorted.
func (s *Snapshot) Changed(o *Snapshot) []string {
	var out []string
	for p, e := range s.files {
		if f, ok := o.files[p]; !ok || f.hash != e.hash || f.mode != e.mode {
			out = append(out, p)
		}
	}
	for p := range o.files {
		if _, ok := s.files[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// snapshotJSON is a Snapshot as it is kept on disk.
type snapshotJSON struct {
	Files     map[string]File `json:"files"`
	Truncated bool            `json:"truncated,omitempty"`
	Git       bool            `json:"git,omitempty"`
	Head      string          `json:"head,omitempty"`
	Tree      string          `json:"tree,omitempty"`
}

func (s *Snapshot) MarshalJSON() ([]byte, error) {
	j := snapshotJSON{Files: make(map[string]File, len(s.files)), Truncated: s.truncated, Git: s.git, Head: s.head, Tree: s.tree}
	for p := range s.files {
		j.Files[p], _ = s.File(p)
	}
	return json.Marshal(j)
}

func (s *Snapshot) UnmarshalJSON(b []byte) error {
	var j snapshotJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = Snapshot{files: make(map[string]entry, len(j.Files)), truncated: j.Truncated, git: j.Git, head: j.Head, tree: j.Tree}
	for p, f := range j.Files {
		s.Set(p, f)
	}
	return nil
}

// HashOf hashes the file at p as Take does, without following a symbolic
// link. A missing file is fs.ErrNotExist; a folder or a device is an error.
func HashOf(p string) (string, fs.FileMode, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", 0, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return "", 0, err
		}
		return hashBytes([]byte("link:" + target)), info.Mode(), nil
	case info.Mode().IsRegular():
		h, err := hashFile(p)
		return h, info.Mode(), err
	}
	return "", info.Mode(), errors.New("not a file")
}

func prevEntry(prev *Snapshot, rel string) (entry, bool) {
	if prev == nil {
		return entry{}, false
	}
	e, ok := prev.files[rel]
	return e, ok
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Change is one file that differs between the start and the end of a job.
type Change struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added, modified or deleted
	// Size and SHA256 are the file at the end; for a deleted file, at the
	// start.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Git, in a git workspace: tracked, untracked or ignored, as git sees
	// the path at the end of the job.
	Git string `json:"git,omitempty"`
	// Content: the whole file at the end, when it is small UTF-8 text and
	// the diff does not show it (untracked, ignored, or a plain folder).
	Content *string `json:"content,omitempty"`
	// Diff, in an isolated session: this file's unified diff from the
	// turn's start to its end, when both sides are small text and its
	// earlier content is kept. DiffLeftOut: the content changed and no diff
	// is here (binary, a link, too big, past the budget, or not kept).
	Diff        *string `json:"diff,omitempty"`
	DiffLeftOut bool    `json:"diff_left_out,omitempty"`
}

// Changes is what a job changed in its workspace.
type Changes struct {
	// Kind is git (a git work tree) or snapshot (a plain folder).
	Kind  string   `json:"kind"`
	Files []Change `json:"files"`
	// Count is every change; Files holds at most MaxChanges of them.
	Count     int  `json:"count"`
	Truncated bool `json:"truncated,omitempty"`
	// HeadBefore and HeadAfter, in a git workspace: the commit HEAD named
	// at the start and at the end. They differ when the job committed.
	HeadBefore string `json:"head_before,omitempty"`
	HeadAfter  string `json:"head_after,omitempty"`
	// Partial: taken while the turn may still run, so it may miss changes
	// made after it.
	Partial bool `json:"partial,omitempty"`
	// Error: the end Snapshot could not be taken.
	Error string `json:"error,omitempty"`
	// Diff, in git: a unified diff of the tracked files against the start
	// of the job, at most MaxDiff bytes. A review job's prompt is built
	// from it.
	Diff          string `json:"diff,omitempty"`
	DiffTruncated bool   `json:"diff_truncated,omitempty"`
	// ContentLeftOut: some small text files have no content here, to keep
	// the result small.
	ContentLeftOut bool `json:"content_left_out,omitempty"`
	// From and To, in an isolated session: the checkpoints the changes are
	// between (package isolation). The same name twice: the turn changed
	// nothing.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Limits of what a result carries for a review.
const (
	MaxDiff    = 150 << 10
	MaxContent = 8 << 10 // a small text file's content, for an untracked, ignored or plain-folder file
	// MaxContents bounds the content of all files together, so a job that
	// writes many small files still has a result that can be sent.
	MaxContents = 256 << 10
)

// Diff lists the changes from before to after, sorted by path.
func Diff(root string, before, after *Snapshot) Changes {
	// Contents are read through the workspace root, which refuses any path
	// that leaves it, through a symlinked file or a symlinked folder alike.
	rt, rootErr := os.OpenRoot(root)
	if rootErr == nil {
		defer rt.Close()
	}
	var read func(string, int) ([]byte, bool)
	if rootErr == nil {
		read = func(rel string, max int) ([]byte, bool) { return readInside(rt, rel, max) }
	}
	return diff(root, before, after, read)
}

// DiffRead is Diff with each changed file's content at the end read by
// read, such as from a checkpoint's own copy of it, rather than from root.
// read answers at most max bytes, or false.
func DiffRead(root string, before, after *Snapshot, read func(rel string, max int) ([]byte, bool)) Changes {
	return diff(root, before, after, read)
}

func diff(root string, before, after *Snapshot, read func(rel string, max int) ([]byte, bool)) Changes {
	c := Changes{Kind: "snapshot", Files: []Change{}, Truncated: before.truncated || after.truncated}
	for p, e := range after.files {
		old, ok := before.files[p]
		switch {
		case !ok:
			c.Files = append(c.Files, Change{Path: p, Change: "added", Size: e.size, SHA256: e.hash})
		case old.hash != e.hash || old.mode != e.mode:
			c.Files = append(c.Files, Change{Path: p, Change: "modified", Size: e.size, SHA256: e.hash})
		}
	}
	for p, e := range before.files {
		if _, ok := after.files[p]; !ok {
			c.Files = append(c.Files, Change{Path: p, Change: "deleted", Size: e.size, SHA256: e.hash})
		}
	}
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	c.Count = len(c.Files)
	if len(c.Files) > MaxChanges {
		c.Files, c.Truncated = c.Files[:MaxChanges], true
	}
	if after.git {
		c.Kind, c.HeadBefore, c.HeadAfter = "git", before.head, after.head
		labelGit(root, c.Files)
		c.Diff, c.DiffTruncated = trackedDiff(root, before.tree, after.tree, c.Files)
	}
	budget := MaxContents
	for i := range c.Files {
		f := &c.Files[i]
		if f.Change == "deleted" || f.Git == "tracked" || f.Size > MaxContent {
			continue
		}
		if f.Size > int64(budget) || read == nil {
			c.ContentLeftOut = true
			continue
		}
		if b, ok := read(f.Path, budget); ok && isText(b) {
			t := string(b)
			f.Content = &t
			budget -= len(b)
		}
	}
	return c
}

// Fit shrinks c until its JSON is at most max bytes: first the files'
// content goes, then each file's own diff, then the diff, then files from the
// end of the list. What was left out is marked: ContentLeftOut, DiffLeftOut,
// DiffTruncated, Truncated. Count stays
// the whole count.
func (c *Changes) Fit(max int) {
	size := func() int { b, _ := json.Marshal(c); return len(b) }
	if size() <= max {
		return
	}
	for i := range c.Files {
		if c.Files[i].Content != nil {
			c.Files[i].Content, c.ContentLeftOut = nil, true
		}
	}
	if size() <= max {
		return
	}
	for i := range c.Files {
		if c.Files[i].Diff != nil {
			c.Files[i].Diff, c.Files[i].DiffLeftOut = nil, true
		}
	}
	if size() <= max {
		return
	}
	if c.Diff != "" {
		c.Diff, c.DiffTruncated = "", true
	}
	for len(c.Files) > 0 && size() > max {
		c.Files, c.Truncated = c.Files[:len(c.Files)/2], true
	}
}

// isText: valid UTF-8 with no NUL byte.
func isText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

// trackedDiff is the unified diff, from the start tree to the end tree, of
// the changed files git tracks, at most MaxDiff bytes. Each path is passed
// literally, never as a pattern.
func trackedDiff(root, from, to string, files []Change) (string, bool) {
	if from == "" || to == "" {
		return "", false
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", from, to, "--"}
	n := 0
	for _, f := range files {
		if f.Git == "tracked" || f.Change == "deleted" {
			args = append(args, ":(literal)"+f.Path)
			n++
		}
	}
	if n == 0 {
		return "", false
	}
	out, err := git(root, nil, args...)
	if err != nil {
		return "", false
	}
	if len(out) > MaxDiff {
		cut := out[:MaxDiff]
		for len(cut) > 0 && !utf8.Valid(cut) {
			cut = cut[:len(cut)-1]
		}
		return string(cut), true
	}
	return string(out), false
}

// git runs git in dir with args, and answers its stdout.
func git(dir string, stdin []byte, args ...string) ([]byte, error) {
	return gitEnv(dir, nil, stdin, args...)
}

// gitEnv is git with extra environment variables.
func gitEnv(dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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

// gitHead says whether dir is the top of a git work tree, and its HEAD.
func gitHead(dir string) (string, bool) {
	top, err := git(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	real, err1 := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	want, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil || real != want {
		return "", false // a folder inside a repo: a plain Snapshot
	}
	head, err := git(dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return "", true // a repo with no commit yet
	}
	return strings.TrimSpace(string(head)), true
}

// labelGit sets each change's Git: ignored when git check-ignore says so,
// tracked when git ls-files lists it, else untracked. A failed git call
// leaves the labels empty.
func labelGit(root string, files []Change) {
	if len(files) == 0 {
		return
	}
	var in bytes.Buffer
	for _, f := range files {
		in.WriteString(f.Path)
		in.WriteByte(0)
	}
	ignored := map[string]bool{}
	// Exit 1 means none is ignored.
	out, err := git(root, in.Bytes(), "check-ignore", "-z", "--stdin")
	if err != nil && len(out) == 0 {
		out = nil
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	tracked := map[string]bool{}
	out, err = git(root, nil, "ls-files", "-z", "--cached")
	if err != nil {
		return
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	for i := range files {
		switch {
		case tracked[files[i].Path]:
			files[i].Git = "tracked"
		case ignored[files[i].Path]:
			files[i].Git = "ignored"
		default:
			files[i].Git = "untracked"
		}
	}
}

// RepoOf says whether folder is in a git work tree whose HEAD names a commit:
// the top of that tree, folder's path inside it ("." at the top) and the
// commit. A folder outside git, or in a repo with no commit yet, answers
// false: it is used as it is.
func RepoOf(folder string) (repo, rel, head string, ok bool) {
	top, err := git(folder, nil, "rev-parse", "--show-toplevel")
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
	out, err := git(folder, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return "", "", "", false
	}
	return realTop, rel, strings.TrimSpace(string(out)), true
}

// MakeWorktree adds a detached git worktree of repo at base, in dir. A dir
// that is already a worktree, from an earlier attempt of the same job, is
// used as it is.
func MakeWorktree(repo, base, dir string) error {
	if _, ok := gitHead(dir); ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return errors.New("the worktree folder could not be made")
	}
	// base never starts with a dash (the control plane checks it), and dir
	// is the runner's own absolute path.
	_, err := git(repo, nil, "worktree", "add", "--detach", dir, base)
	return err
}

// RemoveWorktree removes a worktree the runner made, with whatever is in it.
// dir is the worktree, or a folder inside it: a runner makes each worktree
// one level under workdir, so that level is the worktree. It refuses a
// folder outside workdir: a runner removes only its own.
func RemoveWorktree(workdir, repo, dir string) error {
	rel, err := filepath.Rel(workdir, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return errors.New("the worktree is not under this runner's workdir")
	}
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	dir = filepath.Join(workdir, top)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		_, _ = git(repo, nil, "worktree", "prune")
		return nil
	}
	_, err = git(repo, nil, "worktree", "remove", "--force", dir)
	return err
}

// readInside reads a regular file of the workspace, at most max bytes, through
// rt, so no symlink can take the read outside the workspace. A symlink itself,
// a folder, or anything larger than max is not read.
func readInside(rt *os.Root, rel string, max int) ([]byte, bool) {
	name := filepath.FromSlash(rel)
	if fi, err := rt.Lstat(name); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := rt.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() > int64(max) {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil || len(b) > max {
		return nil, false
	}
	return b, true
}
