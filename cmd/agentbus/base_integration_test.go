//go:build integration

package main

// A workspace that starts from a git base, across processes: control
// (agentbus server) and the runner of the machine that holds the repo are
// two processes. The workspace is made by the API with base (and remote);
// the runner resolves it, fetching first for a remote; the turn runs in a
// git worktree at that commit with the carry_files copied in; the person's
// folder, branch and HEAD are not changed. A stand-in claude records what it
// saw, so no model is called.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// baseStandIn writes what the turn saw: a.txt as it was, and .env or none.
// A later turn of the session, which finds the first one's seen.txt, also
// writes second.txt.
const baseStandIn = `#!/bin/sh
case "$1" in
--version) echo "0.0.0 (agentbus test stand-in)"; exit 0 ;;
auth) exit 0 ;;
esac
if [ -e seen.txt ]; then echo again > second.txt; fi
cp a.txt seen.txt
if [ -e .env ]; then cp .env env-seen.txt; else echo none > env-seen.txt; fi
if [ -e build.log ]; then echo leaked > log-seen.txt; fi
echo '{"type":"result","subtype":"success","result":"done","session_id":"base-'$$'"}'
`

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// baseRepos is a remote (bare) with main and feature, and the person's clone
// of it, on main with an uncommitted edit, an ignored .env and an ignored
// build.log. The remote's feature is one commit ahead of the clone's
// origin/feature. It answers the clone.
func baseRepos(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare, work, clone := filepath.Join(root, "remote.git"), filepath.Join(root, "work"), filepath.Join(root, "clone")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitIn(t, root, "clone", "-q", bare, work)
	writeFiles(t, work, map[string]string{"a.txt": "main\n", ".gitignore": ".env\n*.log\n"})
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "-q", "-m", "main")
	gitIn(t, work, "push", "-q", "origin", "HEAD:main")
	writeFiles(t, work, map[string]string{"a.txt": "feature\n"})
	gitIn(t, work, "commit", "-q", "-am", "feature")
	gitIn(t, work, "push", "-q", "origin", "HEAD:refs/heads/feature")
	gitIn(t, root, "clone", "-q", bare, clone)
	writeFiles(t, work, map[string]string{"a.txt": "feature online\n"})
	gitIn(t, work, "commit", "-q", "-am", "feature online")
	gitIn(t, work, "push", "-q", "origin", "HEAD:refs/heads/feature")
	writeFiles(t, clone, map[string]string{"a.txt": "main, edited\n", ".env": "SECRET=1\n", "build.log": "x\n"})
	return clone
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceFromABaseAcrossProcesses(t *testing.T) {
	bin := built(t)
	bins := t.TempDir()
	if err := os.WriteFile(filepath.Join(bins, "claude"), []byte(baseStandIn), 0o755); err != nil {
		t.Fatal(err)
	}
	folder := baseRepos(t)
	head := gitIn(t, folder, "rev-parse", "HEAD")
	serverHome, addr := t.TempDir(), freeAddr(t)
	sp := startProc(t, bin, serverHome, bins, "server", "-listen", addr, "-replay=false")
	c := waitService(t, sp, addr, serverHome)
	putCarry(t, c, ".env")
	homeA := t.TempDir()
	signInRunner(t, c.base, c.token, homeA, "a")
	startProc(t, bin, homeA, bins, "runner", "-server", c.base, "-folder", folder, "-name", "a", "-replay=false")
	var ref, machine string
	waitFor(t, "the runner's folder", func() bool {
		var fs struct {
			Folders []struct{ Ref, Name, Machine string }
		}
		c.call("GET", "/api/v0/folders", nil, &fs)
		for _, f := range fs.Folders {
			if f.Name == filepath.Base(folder) {
				ref, machine = f.Ref, f.Machine
			}
		}
		return ref != ""
	})

	// The branches the form offers, listed by the folder's machine: the
	// repo's own, and the remote's as it has them now.
	var local, online struct {
		Current  string
		Remotes  []string
		Branches []struct{ Name, Commit string }
	}
	if code, out := c.call("POST", "/api/v0/folders/branches", map[string]any{"folder_ref": ref, "machine": machine}, &local); code != 200 ||
		local.Current != "main" || len(local.Remotes) != 1 || local.Remotes[0] != "origin" {
		t.Fatalf("local branches: %d %s", code, out)
	}
	if code, out := c.call("POST", "/api/v0/folders/branches", map[string]any{"folder_ref": ref, "machine": machine, "remote": "origin"}, &online); code != 200 ||
		len(online.Branches) != 2 || online.Branches[0].Name != "feature" || online.Branches[0].Commit != gitIn(t, filepath.Join(filepath.Dir(folder), "work"), "rev-parse", "HEAD") {
		t.Fatalf("origin's branches: %d %s", code, out)
	}

	// A bad base is refused when the workspace is made, before any turn.
	if code, out := c.call("POST", "/api/v0/workspaces", map[string]any{"folder_ref": ref, "machine": machine, "base": "nope"}, nil); code != http.StatusBadRequest ||
		!strings.Contains(out, "base is not a branch, tag or commit") {
		t.Fatalf("an unknown base: %d %s", code, out)
	}

	for _, tc := range []struct {
		name, base, remote, want string
	}{
		// A local branch, as the clone has it: behind the remote.
		{"local", "feature", "", "feature\n"},
		// The same branch from the remote, fetched first: what it has now.
		{"remote", "feature", "origin", "feature online\n"},
	} {
		if tc.name == "local" {
			// A local branch the person made from what they fetched before.
			gitIn(t, folder, "branch", "feature", "origin/feature")
		}
		body := map[string]any{"folder_ref": ref, "machine": machine, "base": tc.base}
		if tc.remote != "" {
			body["remote"] = tc.remote
		}
		var ws struct {
			Workspace  string `json:"workspace"`
			Base       string `json:"base"`
			Remote     string `json:"remote"`
			BaseCommit string `json:"base_commit"`
		}
		code, out := c.call("POST", "/api/v0/workspaces", body, &ws)
		if code != http.StatusCreated || ws.Base != tc.base || ws.Remote != tc.remote || len(ws.BaseCommit) != 40 {
			t.Fatalf("%s: create: %d %s", tc.name, code, out)
		}
		var sc struct{ Session string }
		c.call("POST", "/api/v0/sessions", map[string]any{"workspace": ws.Workspace}, &sc)
		turn := c.turn(map[string]any{"session": sc.Session, "agent": "claude", "prompt": "look"})
		got := c.diffFiles(turn.Turn)
		if !strings.Contains(got["seen.txt"], "+"+strings.TrimSuffix(tc.want, "\n")) {
			t.Errorf("%s: the turn saw a.txt as %q, want %q", tc.name, got["seen.txt"], tc.want)
		}
		if !strings.Contains(got["env-seen.txt"], "+SECRET=1") {
			t.Errorf("%s: .env was not carried: %q", tc.name, got["env-seen.txt"])
		}
		if _, ok := got["log-seen.txt"]; ok {
			t.Errorf("%s: an ignored file carry_files does not name reached the worktree", tc.name)
		}
		if _, ok := got[".env"]; ok {
			t.Errorf("%s: the carried .env shows as the turn's change", tc.name)
		}
		if tc.name != "remote" {
			continue
		}
		// A second turn continues in the same worktree, and an apply writes
		// the session's work into the person's folder, which stays on its
		// own branch and HEAD.
		t2 := c.turn(map[string]any{"session": sc.Session, "agent": "claude", "prompt": "again"})
		if got := c.diffFiles(t2.Turn); !strings.Contains(got["second.txt"], "+again") {
			t.Errorf("the second turn did not see the first one's files: %v", got)
		}
		if c.ws == nil {
			c.ws = map[string]string{}
		}
		c.ws[sc.Session] = ws.Workspace
		if code, a := c.apply(t2); code != 200 || a.State != "applied" {
			t.Fatalf("apply: %d %+v", code, a)
		}
		for p, want := range map[string]string{"seen.txt": "feature online\n", "second.txt": "again\n", "env-seen.txt": "SECRET=1\n"} {
			if got := fileText(t, filepath.Join(folder, p)); got != want {
				t.Errorf("%s in the folder after the apply: %q, want %q", p, got, want)
			}
		}
	}
	// The person's folder: same branch, same HEAD, the edit still there.
	if gitIn(t, folder, "rev-parse", "HEAD") != head || gitIn(t, folder, "branch", "--show-current") != "main" ||
		fileText(t, filepath.Join(folder, "a.txt")) != "main, edited\n" {
		t.Fatal("the person's folder changed")
	}
}

// putCarry sets the policy's carry_files through the API, keeping the rest.
func putCarry(t *testing.T, c *client, carry ...string) {
	t.Helper()
	var p map[string]any
	if code, out := c.call("GET", "/api/v0/settings/permissions", nil, &p); code != 200 {
		t.Fatalf("GET the policy: %d %s", code, out)
	}
	p["carry_files"] = carry
	b, _ := json.Marshal(p)
	req, _ := http.NewRequest("PUT", c.base+"/api/v0/settings/permissions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("If-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT the policy: %d", resp.StatusCode)
	}
}
