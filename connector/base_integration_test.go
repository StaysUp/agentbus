//go:build integration

package connector

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remoteAndClone is a bare repo with branch main and branch feature, and a
// clone of it whose feature is then left one commit behind the remote's.
// It answers the clone and the remote's newer feature commit.
func remoteAndClone(t *testing.T) (clone, ahead string) {
	t.Helper()
	root := t.TempDir()
	bare, work := filepath.Join(root, "remote.git"), filepath.Join(root, "work")
	gitT(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitT(t, root, "clone", "-q", bare, work)
	write(t, work, map[string]string{"a.txt": "1"})
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "one")
	gitT(t, work, "push", "-q", "origin", "HEAD:main")
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/feature")
	clone = filepath.Join(root, "clone")
	gitT(t, root, "clone", "-q", bare, clone)
	// The remote's feature moves on; the clone has not fetched it.
	write(t, work, map[string]string{"a.txt": "2"})
	gitT(t, work, "commit", "-q", "-am", "two")
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/feature")
	return clone, gitT(t, work, "rev-parse", "HEAD")
}

// A local base resolves to its commit as the repo has it now, and changes
// nothing of the repo; with a remote, the branch is fetched first, so the
// base is the remote's commit even when the local copy is behind. The fetch
// moves only that branch's remote-tracking ref.
func TestBaseLocalAndRemote(t *testing.T) {
	clone, ahead := remoteAndClone(t)
	c, ref := newConnector(t, clone)
	ctx := context.Background()
	stale := gitT(t, clone, "rev-parse", "origin/feature")
	head, branch := gitT(t, clone, "rev-parse", "HEAD"), gitT(t, clone, "branch", "--show-current")

	got, err := c.Base(ctx, ref, "origin/feature", "")
	if err != nil || got != stale {
		t.Fatalf("local origin/feature: %q %v, want %q", got, err, stale)
	}
	got, err = c.Base(ctx, ref, "main", "")
	if err != nil || got != head {
		t.Fatalf("local main: %q %v", got, err)
	}
	got, err = c.Base(ctx, ref, "feature", "origin")
	if err != nil || got != ahead || got == stale {
		t.Fatalf("remote feature: %q %v, want the remote's %q", got, err, ahead)
	}
	if gitT(t, clone, "rev-parse", "origin/feature") != ahead {
		t.Fatal("the remote-tracking ref did not move")
	}
	if gitT(t, clone, "rev-parse", "HEAD") != head || gitT(t, clone, "branch", "--show-current") != branch ||
		gitT(t, clone, "status", "--porcelain") != "" || strings.Contains(gitT(t, clone, "branch", "--list"), "feature") {
		t.Fatal("the fetch changed the person's branch, HEAD, files or local branches")
	}
}

// Each refusal is its fixed sentence: a folder outside git, a name the repo
// does not have, a remote it does not have, a branch the remote does not
// have, and an option-shaped name.
func TestBaseRefusals(t *testing.T) {
	clone, _ := remoteAndClone(t)
	c, ref := newConnector(t, clone)
	plain, plainRef := newConnector(t, t.TempDir())
	ctx := context.Background()
	for _, tc := range []struct {
		c            *Connector
		ref          string
		base, remote string
		want         error
	}{
		{plain, plainRef, "main", "", ErrNotRepo},
		{c, ref, "nope", "", ErrNoBase},
		{c, ref, "feature", "upstream", ErrNoRemote},
		{c, ref, "nope", "origin", ErrFetch},
		{c, ref, "--output=x", "", ErrBadBase},
		{c, ref, "main", "--upload-pack=x", ErrBadBase},
	} {
		if _, err := tc.c.Base(ctx, tc.ref, tc.base, tc.remote); !errors.Is(err, tc.want) {
			t.Errorf("%s %s: %v, want %v", tc.base, tc.remote, err, tc.want)
		}
	}
}
