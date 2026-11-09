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

// samePolicy says p and q have the same edit rule, limits and switches.
func samePolicy(p, q Permissions) bool {
	l, m := p.Limits, q.Limits
	a, b := p.AgentAPI, q.AgentAPI
	return p.Edit == q.Edit && *l.MaxDepth == *m.MaxDepth && *l.MaxJobsPerTurn == *m.MaxJobsPerTurn &&
		*l.MaxJobsPerTree == *m.MaxJobsPerTree && *l.MaxCostUSDPerTree == *m.MaxCostUSDPerTree && a.Scope == b.Scope &&
		*a.Read == *b.Read && *a.ListAgents == *b.ListAgents && *a.Start == *b.Start && *a.Continue == *b.Continue &&
		*a.Review == *b.Review && *a.Cancel == *b.Cancel && *a.Rename == *b.Rename
}

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

	// A missing file is empty lists and every default.
	def := DefaultPermissions()
	if p, err := loadPermissions(path, home); err != nil || len(p.ReadFolders)+len(p.Allow)+len(p.Forbid) != 0 || !samePolicy(p, def) {
		t.Fatalf("missing file: %+v %v", p, err)
	}
	if def.Edit != EditSession || *def.Limits.MaxDepth != 2 || *def.Limits.MaxJobsPerTurn != 10 || *def.Limits.MaxJobsPerTree != 30 ||
		*def.Limits.MaxCostUSDPerTree != 0 || def.AgentAPI.Scope != ScopeTree || !*def.AgentAPI.Start || !*def.AgentAPI.Cancel || !*def.AgentAPI.Rename {
		t.Fatalf("defaults: %+v %+v", def.Limits, def.AgentAPI)
	}
	// The starter parses, with empty lists, and writes out every default.
	writePerm(t, path, PermissionsStarter)
	if p, err := loadPermissions(path, home); err != nil || len(p.ReadFolders)+len(p.Allow)+len(p.Forbid) != 0 || !samePolicy(p, def) {
		t.Fatalf("starter: %+v %v", p, err)
	}
	// An older file, with the lists only, keeps its lists and gets the
	// defaults for the rest; a file may set any one value alone.
	writePerm(t, path, "allow = [\"go test\"]\n")
	if p, err := loadPermissions(path, home); err != nil || !slices.Equal(p.Allow, []string{"go test"}) || !samePolicy(p, def) {
		t.Fatalf("older file: %+v %v", p, err)
	}
	writePerm(t, path, "edit = \"none\"\n[limits]\nmax_depth = 0\nmax_cost_usd_per_tree = 0.25\n[agent_api]\nscope = \"session\"\nstart = false\nrename = false\n")
	if p, err := loadPermissions(path, home); err != nil || p.Edit != EditNone || *p.Limits.MaxDepth != 0 || *p.Limits.MaxJobsPerTurn != 10 ||
		*p.Limits.MaxCostUSDPerTree != 0.25 || p.AgentAPI.Scope != ScopeSession || *p.AgentAPI.Start || !*p.AgentAPI.Continue || *p.AgentAPI.Rename {
		t.Fatalf("set values: %+v %v", p, err)
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
		{"wrong type", "allow = \"go test\"\n", "a value has the wrong type"},
		{"not strings", "allow = [1]\n", "a value has the wrong type"},
		{"unknown key", "allowed = [\"go test\"]\n", `unknown key "allowed"`},
		{"a table", "[allow]\nx = 1\n", "lists of strings"},
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
		{"edit unknown", "edit = \"everywhere\"\n", `edit must be "session" or "none"`},
		{"edit not a string", "edit = true\n", "wrong type"},
		{"limit negative", "[limits]\nmax_depth = -1\n", "max_depth must be 0 or more"},
		{"limit not a number", "[limits]\nmax_jobs_per_turn = \"10\"\n", "wrong type"},
		{"cost negative", "[limits]\nmax_cost_usd_per_tree = -0.5\n", "max_cost_usd_per_tree must be 0"},
		{"unknown limit", "[limits]\nmax_rounds = 2\n", `unknown key "limits.max_rounds"`},
		{"scope unknown", "[agent_api]\nscope = \"all\"\n", `scope must be "tree" or "session"`},
		{"switch not a bool", "[agent_api]\nstart = \"no\"\n", "wrong type"},
		{"unknown switch", "[agent_api]\nwrite_permissions = true\n", `unknown key "agent_api.write_permissions"`},
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
// read_only, an edit rule, resume and model.
func TestTurnArgsPermissions(t *testing.T) {
	perm := Permissions{ReadFolders: []string{"/home/me/.agents", "/home/me/notes"},
		Allow: []string{"go test", "make test"}, Forbid: []string{"git push", "git checkout"}}
	const reads = "Read(//home/me/.agents/**),Grep(//home/me/.agents/**),Glob(//home/me/.agents/**)," +
		"Read(//home/me/notes/**),Grep(//home/me/notes/**),Glob(//home/me/notes/**)"
	allow := []string{"--allowedTools", reads + ",Bash(go test:*),Bash(make test:*)"}
	forbid := []string{"--disallowedTools", "Bash(git push:*),Bash(git checkout:*)"}
	// No edit rule: every tool that edits files is denied, whatever the
	// person's own settings allow.
	noEdit := []string{"--disallowedTools", "Bash(git push:*),Bash(git checkout:*),Edit,Write,NotebookEdit"}
	noEditOnly := []string{"--disallowedTools", "Edit,Write,NotebookEdit"}
	readOnly := []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob," + reads, "--strict-mcp-config"}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	head := []string{"-p", "--output-format", "json"}
	edit := Writes{Edit: []string{"/w/repo"}, Protect: []string{"/data/permissions.toml", "/data/token"}}
	ro := Writes{ReadOnly: true, Protect: edit.Protect}
	for _, tc := range []struct {
		name, agent, resume, model string
		w                          Writes
		perm                       Permissions
		want                       []string
	}{
		{"claude, no policy, no edits", "claude", "", "", Writes{}, Permissions{}, cat(head, noEditOnly, []string{"--", "hi"})},
		{"claude", "claude", "", "", Writes{}, perm, cat(head, allow, noEdit, []string{"--", "hi"})},
		// The edit rule: Edit and Write in the session's folder, and a deny
		// on each protected file, which wins over it and over forbid's
		// neighbours in the same list.
		{"claude edits its folder", "claude", "", "", edit, perm, cat(head,
			[]string{"--allowedTools", reads + ",Edit(//w/repo/**),Bash(go test:*),Bash(make test:*)",
				"--disallowedTools", "Bash(git push:*),Bash(git checkout:*),Edit(//data/permissions.toml),Edit(//data/token)", "--", "hi"})},
		{"claude edits, no other policy", "claude", "", "", edit, Permissions{}, cat(head,
			[]string{"--allowedTools", "Edit(//w/repo/**)", "--disallowedTools", "Edit(//data/permissions.toml),Edit(//data/token)", "--", "hi"})},
		// read_folders never becomes --add-dir: that would let a turn write
		// there under the person's acceptEdits mode.
		{"claude, folders only", "claude", "", "", Writes{}, Permissions{ReadFolders: []string{"/x"}},
			cat(head, []string{"--allowedTools", "Read(//x/**),Grep(//x/**),Glob(//x/**)"}, noEditOnly, []string{"--", "hi"})},
		{"claude read-only, no policy", "claude", "", "", ro, Permissions{},
			cat(head, []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob", "--strict-mcp-config", "--", "hi"})},
		{"claude, allow only", "claude", "", "", Writes{}, Permissions{Allow: []string{"go test"}},
			cat(head, []string{"--allowedTools", "Bash(go test:*)"}, noEditOnly, []string{"--", "hi"})},
		// allow never widens a read-only turn; folders and forbid still apply.
		{"claude read-only", "claude", "", "", ro, perm, cat(head, readOnly, forbid, []string{"--", "hi"})},
		{"claude resume and model", "claude", "r-1", "opus", Writes{}, perm,
			cat(head, []string{"--resume", "r-1"}, allow, noEdit, []string{"--model", "opus", "--", "hi"})},
		{"claude read-only, resume and model", "claude", "r-1", "opus", ro, perm,
			cat(head, []string{"--resume", "r-1"}, readOnly, forbid, []string{"--model", "opus", "--", "hi"})},
		// codex writes, with network, only with the edit rule.
		{"codex edits", "codex", "", "", edit, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true", "--", "hi"}},
		{"codex, no edit rule", "codex", "", "", Writes{}, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "read-only", "--", "hi"}},
		{"codex read-only", "codex", "", "", ro, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "read-only", "--", "hi"}},
		{"codex resume and model", "codex", "r-1", "gpt-5", edit, perm, []string{"exec", "--skip-git-repo-check", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true", "resume", "--json", "--model", "gpt-5", "--", "r-1", "hi"}},
	} {
		got, ok := turnArgs(tc.agent, "", tc.resume, tc.model, tc.w, tc.perm, "hi")
		if !ok || !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// writesFor gives the edit rule from edit = "session", never to a read-only
// turn; a folder that cannot go in a rule gets none; codex gets none for a
// folder that holds a protected file, which its sandbox could not keep out.
func TestWritesFor(t *testing.T) {
	folder := t.TempDir()
	real, _ := filepath.EvalSymlinks(folder)
	data := filepath.Join(t.TempDir(), "home")
	protect := []string{filepath.Join(data, PermissionsFile)}
	def := DefaultPermissions()
	w, why := writesFor("claude", folder, false, def, protect)
	if why != "" || len(w.Edit) == 0 || w.Edit[0] != folder || !slices.Contains(w.Edit, real) || len(w.Protect) == 0 || w.ReadOnly {
		t.Fatalf("claude: %+v %q", w, why)
	}
	if w, _ := writesFor("codex", folder, false, def, protect); len(w.Edit) == 0 {
		t.Fatalf("codex: %+v", w)
	}
	if w, _ := writesFor("claude", folder, true, def, protect); !w.ReadOnly || len(w.Edit)+len(w.Protect) != 0 {
		t.Fatalf("read-only: %+v", w)
	}
	none := def
	none.Edit = EditNone
	if w, _ := writesFor("claude", folder, false, none, protect); len(w.Edit) != 0 || len(w.Protect) == 0 {
		t.Fatalf("edit none: %+v", w)
	}
	odd := filepath.Join(t.TempDir(), "a,b")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if w, why := writesFor("claude", odd, false, def, protect); len(w.Edit) != 0 || why != "folder_shape" {
		t.Fatalf("a comma in the folder: %+v %q", w, why)
	}
	// codex takes any folder name: its sandbox needs no rule.
	if w, why := writesFor("codex", odd, false, def, protect); len(w.Edit) == 0 || why != "" {
		t.Fatalf("codex, a comma in the folder: %+v %q", w, why)
	}
	// A data folder named relative to the service's own folder is still
	// found inside the session's folder.
	wd, _ := os.Getwd()
	t.Chdir(filepath.Dir(data))
	if w, why := writesFor("codex", filepath.Dir(data), false, def, []string{filepath.Join("home", PermissionsFile)}); len(w.Edit) != 0 || why != "data_folder_inside" {
		t.Fatalf("codex, a relative data folder inside: %+v %q", w, why)
	}
	t.Chdir(wd)
	// The data folder inside the session's folder.
	parent := filepath.Dir(data)
	if w, why := writesFor("codex", parent, false, def, protect); len(w.Edit) != 0 || why != "data_folder_inside" {
		t.Fatalf("codex over the data folder: %+v %q", w, why)
	}
	if w, why := writesFor("claude", parent, false, def, protect); len(w.Edit) == 0 || why != "" || len(w.Protect) == 0 {
		t.Fatalf("claude over the data folder keeps the deny rules: %+v %q", w, why)
	}
	// A protected file that cannot go in a deny rule leaves edits out.
	if w, why := writesFor("claude", folder, false, def, []string{"/data/a(b"}); len(w.Edit) != 0 || why != "protected_file_shape" {
		t.Fatalf("an odd protected file: %+v %q", w, why)
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
		Wire: func(agent, baseURL string, _ Access) (Launch, error) { wired.Add(1); return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	start := func() *Refusal {
		_, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
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

// An access the engine does not know is refused before a slot is taken, so
// the refusal holds none: with one slot, the next turn still gets it.
func TestUnknownAccessHoldsNoSlot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", MaxRuns: 1,
		Wire: func(agent, baseURL string, _ Access) (Launch, error) { return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	for i, access := range []string{"bogus", "bogus", ""} {
		_, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi", Access: access})
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("%d: %v", i, err)
		}
		want := "access must be"
		if access == "" {
			want = "is not on the service's PATH" // past the slot: it was free
		}
		if r.Status != 400 || !strings.Contains(r.Msg, want) {
			t.Fatalf("%d (%q): %+v, want 400 %q", i, access, r, want)
		}
	}
}
