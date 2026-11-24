package connector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/staysup/agentbus/queue"
)

// A workspace may start from a base: a branch, tag or commit of the git repo
// that holds the person's folder, in place of the folder as it is. The base
// is resolved here, on the folder's machine, when the workspace is made, so
// control records a commit id and never runs git itself; the runner makes a
// worktree at that commit (isolation, the git backend).
//
// With a remote, the branch is fetched from it first, with this machine's own
// git credentials, so the workspace starts from what the remote has now. The
// fetch updates that one branch's remote-tracking ref
// (refs/remotes/<remote>/<branch>), as a git fetch in a terminal does, and
// nothing else of the person's repo: no branch, no index, no working file.

// Base refusals, in fixed wording: git's own words can name paths.
var (
	ErrNotRepo  = errors.New("the folder is not in a git repo with a commit, so it cannot start from a base")
	ErrNoBase   = errors.New("base is not a branch, tag or commit in the folder's repo")
	ErrNoRemote = errors.New("remote is not a remote of the folder's repo")
	ErrFetch    = errors.New("the branch could not be fetched from that remote; check the branch name and that git can reach the remote from this machine")
	ErrBadBase  = errors.New("base must be a branch, tag or commit id")
)

// Base resolves base in the repo that holds the folder ref to its commit id,
// fetching the branch base from remote first when remote is set.
func (c *Connector) Base(ctx context.Context, ref, base, remote string) (string, error) {
	dir, err := c.path(ref)
	if err != nil {
		return "", err
	}
	// The API checks the shape; a dash would read as an option.
	if base == "" || strings.HasPrefix(base, "-") || strings.HasPrefix(remote, "-") {
		return "", ErrBadBase
	}
	if _, err := gitIn(ctx, dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return "", ErrNotRepo
	}
	rev := base
	if remote != "" {
		out, err := gitIn(ctx, dir, "remote")
		if err != nil || !hasLine(out, remote) {
			return "", ErrNoRemote
		}
		fctx, cancel := context.WithTimeout(ctx, queue.FetchWait)
		defer cancel()
		track := "refs/remotes/" + remote + "/" + base
		if _, err := gitIn(fctx, dir, "fetch", "--no-tags", "--quiet", "--", remote, "+refs/heads/"+base+":"+track); err != nil {
			return "", ErrFetch
		}
		rev = track
	}
	out, err := gitIn(ctx, dir, "rev-parse", "--verify", "-q", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", ErrNoBase
	}
	return strings.TrimSpace(string(out)), nil
}

// hasLine says whether out has a line that is exactly want.
func hasLine(out []byte, want string) bool {
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// gitIn runs git in dir and answers its stdout. Its own words are dropped:
// they can name paths. It never prompts for a password.
func gitIn(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("git failed")
	}
	return out.Bytes(), nil
}

// BaseJob is Base, for a folder job that names a base.
func (j Jobs) BaseJob(ctx context.Context, ref, base, remote string) (string, error) {
	return j.Base(ctx, ref, base, remote)
}

// ErrListRemote: the remote's branches could not be listed, and the repo has
// none fetched from it to show instead.
var ErrListRemote = errors.New("the remote's branches could not be listed; check that git can reach the remote from this machine")

// maxBranches bounds a listing.
const maxBranches = 1000

// Branches lists the branches of the repo that holds the folder ref, most
// recently changed first, or with remote those the remote has now, by name.
// A remote that cannot be reached answers the branches the repo last fetched
// from it, marked stale. Listing changes nothing of the repo.
func (c *Connector) Branches(ctx context.Context, ref, remote string) (queue.BranchList, error) {
	var out queue.BranchList
	dir, err := c.path(ref)
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(remote, "-") {
		return out, ErrNoRemote
	}
	if _, err := gitIn(ctx, dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return out, ErrNotRepo
	}
	rs, err := gitIn(ctx, dir, "remote")
	if err != nil {
		return out, ErrNotRepo
	}
	out.Remotes = []string{}
	for _, l := range strings.Split(string(rs), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out.Remotes = append(out.Remotes, l)
		}
	}
	sort.Strings(out.Remotes)
	out.Branches = []queue.Branch{}
	if remote == "" {
		if cur, err := gitIn(ctx, dir, "symbolic-ref", "-q", "HEAD"); err == nil {
			out.Current = strings.TrimPrefix(strings.TrimSpace(string(cur)), "refs/heads/")
		}
		refs, err := gitIn(ctx, dir, "for-each-ref", "--sort=-committerdate", "--format=%(objectname)%09%(refname)", "refs/heads/")
		if err != nil {
			return out, ErrNotRepo
		}
		addBranches(&out, refs, "refs/heads/")
		return out, nil
	}
	if !hasLine(rs, remote) {
		return out, ErrNoRemote
	}
	out.Remote = remote
	lctx, cancel := context.WithTimeout(ctx, queue.FetchWait)
	defer cancel()
	if heads, err := gitIn(lctx, dir, "ls-remote", "--heads", "--", remote); err == nil {
		addBranches(&out, heads, "refs/heads/")
	} else {
		// Offline: what the repo last fetched from it.
		refs, err := gitIn(ctx, dir, "for-each-ref", "--format=%(objectname)%09%(refname)", "refs/remotes/"+remote+"/")
		if err != nil {
			return out, ErrListRemote
		}
		addBranches(&out, refs, "refs/remotes/"+remote+"/")
		if len(out.Branches) == 0 {
			return out, ErrListRemote
		}
		out.Stale = true
	}
	sort.Slice(out.Branches, func(i, j int) bool { return out.Branches[i].Name < out.Branches[j].Name })
	return out, nil
}

// addBranches appends to l each "<commit>\t<prefix><name>" line of refs, but
// a remote's HEAD, up to maxBranches.
func addBranches(l *queue.BranchList, refs []byte, prefix string) {
	for _, line := range strings.Split(string(refs), "\n") {
		commit, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		name = strings.TrimPrefix(name, prefix)
		if name == "" || name == "HEAD" {
			continue
		}
		if len(l.Branches) == maxBranches {
			l.Truncated = true
			return
		}
		l.Branches = append(l.Branches, queue.Branch{Name: name, Commit: commit})
	}
}

// BranchesJob is Branches, for a folder job that lists branches.
func (j Jobs) BranchesJob(ctx context.Context, ref, remote string) (queue.BranchList, error) {
	return j.Branches(ctx, ref, remote)
}
