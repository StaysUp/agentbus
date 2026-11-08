package run

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/staysup/agentbus/store"
)

func writePerm(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPermissions(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "notes", "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PermissionsFile)

	// A missing file is empty lists: today's behaviour.
	if p, err := loadPermissions(path, home); err != nil || len(p.ReadFolders)+len(p.Allow)+len(p.Forbid) != 0 {
		t.Fatalf("missing file: %+v %v", p, err)
	}
	// The starter parses, with empty lists.
	writePerm(t, path, PermissionsStarter)
	if p, err := loadPermissions(path, home); err != nil || len(p.ReadFolders)+len(p.Allow)+len(p.Forbid) != 0 {
		t.Fatalf("starter: %+v %v", p, err)
	}
	// ~ is the home folder; a folder comes back absolute and clean.
	writePerm(t, path, "read_folders = [\"~\", \"~/notes/a/\", \""+dir+"/.\"]\n"+
		"allow = [\"go test\", 'make test']\n"+
		"forbid = [\n  \"git push\", # a comment\n]\n")
	p, err := loadPermissions(path, home)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{home, filepath.Join(home, "notes", "a"), dir}; !slices.Equal(p.ReadFolders, want) {
		t.Errorf("folders %q, want %q", p.ReadFolders, want)
	}
	// A colon in an argument is fine: the rule ends at the last ":*".
	writePerm(t, path, "allow = [\"curl http://localhost:3000/health\"]\n")
	if q, err := loadPermissions(path, home); err != nil || !slices.Equal(q.Allow, []string{"curl http://localhost:3000/health"}) {
		t.Errorf("a colon in an argument: %+v %v", q, err)
	}
	if !slices.Equal(p.Allow, []string{"go test", "make test"}) || !slices.Equal(p.Forbid, []string{"git push"}) {
		t.Errorf("commands %+v", p)
	}

	for _, tc := range []struct{ name, file, want string }{
		{"syntax", "allow = [\"go test\"]\n\nforbid = git push\n", "line 3"},
		{"wrong type", "allow = \"go test\"\n", "must each be a list of strings"},
		{"not strings", "allow = [1]\n", "must each be a list of strings"},
		{"unknown key", "allowed = [\"go test\"]\n", `unknown key "allowed"`},
		{"a table", "[allow]\nx = 1\n", "list of strings"},
		{"empty folder", "read_folders = [\"\"]\n", `read_folders entry "" is empty`},
		{"relative folder", "read_folders = [\"notes\"]\n", "must be an absolute path"},
		{"a flag as a folder", "read_folders = [\"--help\"]\n", "must be an absolute path"},
		{"another user's home", "read_folders = [\"~root\"]\n", "must be an absolute path"},
		{"missing folder", "read_folders = [\"~/nope\"]\n", "is not an existing folder"},
		{"control character", "read_folders = [\"/tmp\\n\"]\n", "control character"},
		{"empty command", "allow = [\" \"]\n", `allow entry " " is empty`},
		{"a flag as a command", "forbid = [\"--dangerously-skip-permissions\"]\n", "starts with -"},
		{"comma", "allow = [\"go test,Edit\"]\n", "comma or a parenthesis"},
		{"parenthesis", "forbid = [\"x)\"]\n", "comma or a parenthesis"},
		{"padded", "allow = [\"go test \"]\n", "starts or ends with a space"},
		{"rule syntax in a command", "allow = [\"go test:*\"]\n", "has a star"},
		{"two spaces", "allow = [\"go  test\"]\n", "two spaces in a row"},
		{"glob in a folder", "read_folders = [\"/tmp/*\"]\n", "glob character"},
		{"comma in a folder", "read_folders = [\"/tmp,Edit\"]\n", "comma"},
		{"tab", "allow = [\"go\\ttest\"]\n", "control character"},
	} {
		writePerm(t, path, tc.file)
		p, err := loadPermissions(path, home)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %+v %v, want an error with %q", tc.name, p, err, tc.want)
		}
	}
	// A link to a file that is gone is not a missing file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone.toml"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPermissions(path, home); err == nil || !strings.Contains(err.Error(), "link to a file that does not exist") {
		t.Errorf("a dangling link: %v", err)
	}
	// Something there that is not a readable file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPermissions(path, home); err == nil {
		t.Error("a folder in the file's place was read as empty lists")
	}
}

// The exact arguments each agent gets for a policy, with and without
// read_only, resume and model.
func TestTurnArgsPermissions(t *testing.T) {
	perm := Permissions{ReadFolders: []string{"/home/me/.agents", "/home/me/notes"},
		Allow: []string{"go test", "make test"}, Forbid: []string{"git push", "git checkout"}}
	const reads = "Read(//home/me/.agents/**),Grep(//home/me/.agents/**),Glob(//home/me/.agents/**)," +
		"Read(//home/me/notes/**),Grep(//home/me/notes/**),Glob(//home/me/notes/**)"
	allow := []string{"--allowedTools", reads + ",Bash(go test:*),Bash(make test:*)"}
	forbid := []string{"--disallowedTools", "Bash(git push:*),Bash(git checkout:*)"}
	readOnly := []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob," + reads, "--strict-mcp-config"}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	head := []string{"-p", "--output-format", "json"}
	for _, tc := range []struct {
		name, agent, resume, model string
		readOnly                   bool
		perm                       Permissions
		want                       []string
	}{
		{"claude, no policy", "claude", "", "", false, Permissions{}, cat(head, []string{"--", "hi"})},
		{"claude", "claude", "", "", false, perm, cat(head, allow, forbid, []string{"--", "hi"})},
		// read_folders never becomes --add-dir: that would let a turn write
		// there under the person's acceptEdits mode.
		{"claude, folders only", "claude", "", "", false, Permissions{ReadFolders: []string{"/x"}},
			cat(head, []string{"--allowedTools", "Read(//x/**),Grep(//x/**),Glob(//x/**)", "--", "hi"})},
		{"claude read-only, no policy", "claude", "", "", true, Permissions{},
			cat(head, []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob", "--strict-mcp-config", "--", "hi"})},
		{"claude, allow only", "claude", "", "", false, Permissions{Allow: []string{"go test"}},
			cat(head, []string{"--allowedTools", "Bash(go test:*)", "--", "hi"})},
		// allow never widens a read-only turn; folders and forbid still apply.
		{"claude read-only", "claude", "", "", true, perm, cat(head, readOnly, forbid, []string{"--", "hi"})},
		{"claude resume and model", "claude", "r-1", "opus", false, perm,
			cat(head, []string{"--resume", "r-1"}, allow, forbid, []string{"--model", "opus", "--", "hi"})},
		{"claude read-only, resume and model", "claude", "r-1", "opus", true, perm,
			cat(head, []string{"--resume", "r-1"}, readOnly, forbid, []string{"--model", "opus", "--", "hi"})},
		// codex gets nothing from the policy (see turnArgs).
		{"codex", "codex", "", "", false, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true", "--", "hi"}},
		{"codex read-only", "codex", "", "", true, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "read-only", "--", "hi"}},
		{"codex resume and model", "codex", "r-1", "gpt-5", false, perm, []string{"exec", "--skip-git-repo-check", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true", "resume", "--json", "--model", "gpt-5", "--", "r-1", "hi"}},
	} {
		got, ok := turnArgs(tc.agent, "", tc.resume, tc.model, tc.readOnly, tc.perm, "hi")
		if !ok || !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func jsonString(s string) string { return tomlString(s) }

// A broken permissions.toml stops the turn before anything is stored, with
// the file and the line in the refusal. The file is read for every turn, so a
// fix applies to the next one.
func TestBrokenPermissionsRefusesTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), PermissionsFile)
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var wired atomic.Int32
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Permissions: path,
		Wire: func(agent, baseURL string) (Launch, error) { wired.Add(1); return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	start := func() *Refusal {
		_, err := e.Start(Request{Agent: "claude", Folder: t.TempDir(), Prompt: "hi"})
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("Start: %v, want a refusal", err)
		}
		return r
	}
	// Missing: empty lists, and the turn goes on to start its agent.
	if r := start(); r.Status != 400 || !strings.Contains(r.Msg, "is not on the service's PATH") || wired.Load() != 1 {
		t.Fatalf("missing file: %+v, wire %d", r, wired.Load())
	}
	writePerm(t, path, "# mine\nallow = go test\n")
	if r := start(); r.Status != 400 || !strings.Contains(r.Msg, "permissions.toml") || !strings.Contains(r.Msg, "line 2") {
		t.Fatalf("broken file: %+v, want 400 naming the file and line 2", r)
	}
	if wired.Load() != 1 {
		t.Errorf("broken file: the agent was wired")
	}
	if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
		t.Errorf("broken file: %d sessions stored, want none", len(snap.Sessions))
	}
	writePerm(t, path, "allow = [\"go test\"]\n")
	if r := start(); r.Status != 400 || !strings.Contains(r.Msg, "is not on the service's PATH") || wired.Load() != 2 {
		t.Fatalf("fixed file: %+v, wire %d", r, wired.Load())
	}
}
