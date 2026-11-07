package api

import (
	"net"
	"net/http"
	"net/http/httptest"
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
		{"tab", "allow = [\"go\\ttest\"]\n", "control character"},
	} {
		writePerm(t, path, tc.file)
		p, err := loadPermissions(path, home)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %+v %v, want an error with %q", tc.name, p, err, tc.want)
		}
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
	dirs := []string{"--add-dir", "/home/me/.agents", "--add-dir", "/home/me/notes"}
	allow := []string{"--allowedTools", "Bash(go test:*),Bash(make test:*)"}
	forbid := []string{"--disallowedTools", "Bash(git push:*),Bash(git checkout:*)"}
	readOnly := []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob", "--strict-mcp-config"}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	head := []string{"-p", "--output-format", "json"}
	for _, tc := range []struct {
		name, agent, resume, model string
		readOnly                   bool
		perm                       Permissions
		want                       []string
	}{
		{"claude, no policy", "claude", "", "", false, Permissions{}, cat(head, []string{"--", "hi"})},
		{"claude", "claude", "", "", false, perm, cat(head, dirs, allow, forbid, []string{"--", "hi"})},
		{"claude, allow only", "claude", "", "", false, Permissions{Allow: []string{"go test"}},
			cat(head, []string{"--allowedTools", "Bash(go test:*)", "--", "hi"})},
		// allow never widens a read-only turn; folders and forbid still apply.
		{"claude read-only", "claude", "", "", true, perm, cat(head, dirs, readOnly, forbid, []string{"--", "hi"})},
		{"claude resume and model", "claude", "r-1", "opus", false, perm,
			cat(head, []string{"--resume", "r-1"}, dirs, allow, forbid, []string{"--model", "opus", "--", "hi"})},
		{"claude read-only, resume and model", "claude", "r-1", "opus", true, perm,
			cat(head, []string{"--resume", "r-1"}, dirs, readOnly, forbid, []string{"--model", "opus", "--", "hi"})},
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

// permEnv is an API that reads permissions.toml at path, whose Wire counts
// its calls and answers ErrNotFound, so a turn that gets past the policy
// answers 400 with the missing-agent reply.
func permEnv(t *testing.T, path string) (*env, *atomic.Int32) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	var wired atomic.Int32
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Permissions: path,
		Wire: func(agent, baseURL string) (Launch, error) { wired.Add(1); return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); a.StopRuns(); st.Close() })
	return &env{srv: srv, api: a, store: st, port: port}, &wired
}

// A broken permissions.toml stops the turn before anything is stored, with
// the file and the line in the reply. The file is read for every turn, so a
// fix applies to the next one.
func TestBrokenPermissionsRefusesTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), PermissionsFile)
	e, wired := permEnv(t, path)
	post := func() (int, string) {
		resp, body := do(t, e.req(t, "POST", "/api/v0/turns",
			strings.NewReader(`{"agent":"claude","folder":`+jsonString(t.TempDir())+`,"prompt":"hi"}`)))
		return resp.StatusCode, body
	}
	// Missing: empty lists, and the turn goes on to start its agent.
	if code, body := post(); code != http.StatusBadRequest || !strings.Contains(body, "is not on the service's PATH") || wired.Load() != 1 {
		t.Fatalf("missing file: %d %s, wire %d", code, body, wired.Load())
	}
	writePerm(t, path, "# mine\nallow = go test\n")
	code, body := post()
	if code != http.StatusBadRequest || !strings.Contains(body, "permissions.toml") || !strings.Contains(body, "line 2") {
		t.Fatalf("broken file: %d %s, want 400 naming the file and line 2", code, body)
	}
	if wired.Load() != 1 {
		t.Errorf("broken file: the agent was wired")
	}
	if s := snapshot(t, e); len(s.Sessions) != 0 {
		t.Errorf("broken file: %d sessions stored, want none", len(s.Sessions))
	}
	writePerm(t, path, "allow = [\"go test\"]\n")
	if code, body := post(); code != http.StatusBadRequest || !strings.Contains(body, "is not on the service's PATH") || wired.Load() != 2 {
		t.Fatalf("fixed file: %d %s, wire %d", code, body, wired.Load())
	}
}

func jsonString(s string) string { return tomlString(s) }
