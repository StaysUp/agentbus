package connector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
