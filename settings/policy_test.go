package settings

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// load reads the policy at path as a turn does: from the file store of its
// folder, then its folders resolved with home.
func load(path, home string) (Policy, error) {
	p, _, err := NewFiles(filepath.Dir(path)).Policy()
	if err != nil {
		return Policy{}, err
	}
	return p.Resolve(home)
}

// samePolicy says p and q have the same edit rule, limits and switches.
func samePolicy(p, q Policy) bool {
	l, m := p.Limits, q.Limits
	a, b := p.AgentAPI, q.AgentAPI
	return p.Edit == q.Edit && *p.Subagents == *q.Subagents && *l.MaxDepth == *m.MaxDepth && *l.MaxJobsPerTurn == *m.MaxJobsPerTurn &&
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

func TestReadPolicy(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "notes", "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PolicyFile)

	// A missing file is empty lists and every default.
	def := DefaultPolicy()
	if p, err := load(path, home); err != nil || len(p.ReadFolders)+len(p.Allow)+len(p.Forbid) != 0 || !samePolicy(p, def) {
		t.Fatalf("missing file: %+v %v", p, err)
	}
	if def.Edit != EditSession || *def.Limits.MaxDepth != 2 || *def.Limits.MaxJobsPerTurn != 10 || *def.Limits.MaxJobsPerTree != 30 ||
		*def.Limits.MaxCostUSDPerTree != 0 || def.AgentAPI.Scope != ScopeTree || !*def.AgentAPI.Start || !*def.AgentAPI.Cancel || !*def.AgentAPI.Rename {
		t.Fatalf("defaults: %+v %+v", def.Limits, def.AgentAPI)
	}
	if !*def.Subagents {
		t.Fatal("subagents are off by default")
	}
	// The starter parses, with empty lists, and writes out every default.
	writePerm(t, path, PolicyStarter)
	if p, err := load(path, home); err != nil || len(p.ReadFolders)+len(p.Allow)+len(p.Forbid) != 0 || !samePolicy(p, def) {
		t.Fatalf("starter: %+v %v", p, err)
	}
	// An older file, with the lists only, keeps its lists and gets the
	// defaults for the rest; a file may set any one value alone.
	writePerm(t, path, "allow = [\"go test\"]\n")
	if p, err := load(path, home); err != nil || !slices.Equal(p.Allow, []string{"go test"}) || !samePolicy(p, def) {
		t.Fatalf("older file: %+v %v", p, err)
	}
	writePerm(t, path, "edit = \"none\"\n[limits]\nmax_depth = 0\nmax_cost_usd_per_tree = 0.25\n[agent_api]\nscope = \"session\"\nstart = false\nrename = false\n")
	if p, err := load(path, home); err != nil || p.Edit != EditNone || *p.Limits.MaxDepth != 0 || *p.Limits.MaxJobsPerTurn != 10 ||
		*p.Limits.MaxCostUSDPerTree != 0.25 || p.AgentAPI.Scope != ScopeSession || *p.AgentAPI.Start || !*p.AgentAPI.Continue || *p.AgentAPI.Rename {
		t.Fatalf("set values: %+v %v", p, err)
	}
	writePerm(t, path, "subagents = false\n")
	if p, err := load(path, home); err != nil || *p.Subagents || p.Edit != EditSession {
		t.Fatalf("subagents = false: %+v %v", p, err)
	}
	// ~ is the home folder; a folder comes back absolute and clean.
	writePerm(t, path, "read_folders = [\"~\", \"~/notes/a/\", \""+dir+"/.\"]\n"+
		"allow = [\"go test\", 'make test']\n"+
		"forbid = [\n  \"git push\", # a comment\n]\n")
	p, err := load(path, home)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{home, filepath.Join(home, "notes", "a"), dir}; !slices.Equal(p.ReadFolders, want) {
		t.Errorf("folders %q, want %q", p.ReadFolders, want)
	}
	// A colon in an argument is fine: the rule ends at the last ":*".
	writePerm(t, path, "allow = [\"curl http://localhost:3000/health\"]\n")
	if q, err := load(path, home); err != nil || !slices.Equal(q.Allow, []string{"curl http://localhost:3000/health"}) {
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
		{"subagents not a bool", "subagents = \"no\"\n", "subagents is true or false"},
		{"switch not a bool", "[agent_api]\nstart = \"no\"\n", "wrong type"},
		{"unknown switch", "[agent_api]\nwrite_permissions = true\n", `unknown key "agent_api.write_permissions"`},
	} {
		writePerm(t, path, tc.file)
		p, err := load(path, home)
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
	if _, err := load(path, home); err == nil || !strings.Contains(err.Error(), "link to a file that does not exist") {
		t.Errorf("a dangling link: %v", err)
	}
	// Something there that is not a readable file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := load(path, home); err == nil {
		t.Error("a folder in the file's place was read as empty lists")
	}
}

// [network]: contain and allow, read from TOML and JSON, refused when out of
// shape, and written back by FormatPolicy.
func TestNetworkPolicy(t *testing.T) {
	p, err := ParsePolicy([]byte("[network]\ncontain = \"on\"\nallow = [\"github.com\", \"*.npmjs.org\", \"10.0.0.5\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Network.Contain != ContainOn || !slices.Equal(p.Network.Allow, []string{"github.com", "*.npmjs.org", "10.0.0.5"}) {
		t.Fatalf("network: %+v", p.Network)
	}
	if d := DefaultPolicy(); d.Network.Contain != ContainAuto || d.Network.Allow == nil || len(d.Network.Allow) != 0 {
		t.Fatalf("default network: %+v", d.Network)
	}
	again, err := ParsePolicy(FormatPolicy(p))
	if err != nil || again.Network.Contain != ContainOn || !slices.Equal(again.Network.Allow, p.Network.Allow) {
		t.Fatalf("format and read back: %+v %v", again.Network, err)
	}
	if j, err := DecodePolicy([]byte(`{"network":{"allow":["github.com"]}}`)); err != nil || j.Network.Contain != ContainAuto {
		t.Fatalf("JSON: %+v %v", j.Network, err)
	}
	for _, bad := range []string{
		"[network]\ncontain = \"yes\"\n",
		"[network]\nallow = [\"https://github.com\"]\n",
		"[network]\nallow = [\"github.com:443\"]\n",
		"[network]\nallow = [\"github.com/x\"]\n",
		"[network]\nallow = [\"\"]\n",
		"[network]\nallow = [\"*\"]\n",
		"[network]\nallow = [\"-a.com\"]\n",
		"[network]\nhosts = []\n",
	} {
		if _, err := ParsePolicy([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
