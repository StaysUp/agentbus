// Package workspace is the folder a session's turns run in. A session's first job
// names a git repo and a base, and the runner makes a git worktree for it
// under Config.Workdir; or it names a folder, used as it is. A worktree is
// removed only when its session is closed, never when a job ends.
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
		if f.Size > int64(budget) {
			c.ContentLeftOut = true
			continue
		}
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path))); err == nil && isText(b) && len(b) <= budget {
			t := string(b)
			f.Content = &t
			budget -= len(b)
		}
	}
	return c
}

// Fit shrinks c until its JSON is at most max bytes: first the files'
// content goes, then the diff, then files from the end of the list. What was
// left out is marked: ContentLeftOut, DiffTruncated, Truncated. Count stays
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
// It refuses a folder outside workdir: a runner removes only its own.
func RemoveWorktree(workdir, repo, dir string) error {
	rel, err := filepath.Rel(workdir, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return errors.New("the worktree is not under this runner's workdir")
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		_, _ = git(repo, nil, "worktree", "prune")
		return nil
	}
	_, err = git(repo, nil, "worktree", "remove", "--force", dir)
	return err
}
