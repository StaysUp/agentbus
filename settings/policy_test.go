package settings

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// samePolicy says p and q have the same limits and isolation.
func samePolicy(p, q Policy) bool {
	l, m := p.Limits, q.Limits
	return p.Isolation == q.Isolation && *l.MaxDepth == *m.MaxDepth && *l.MaxJobsPerTurn == *m.MaxJobsPerTurn &&
		*l.MaxJobsPerTree == *m.MaxJobsPerTree && *l.MaxCostUSDPerTree == *m.MaxCostUSDPerTree
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
	if p, err := load(path, home); err != nil || !slices.Equal(p.Allow, def.Allow) || !slices.Equal(p.Forbid, def.Forbid) || !samePolicy(p, def) {
		t.Fatalf("missing file: %+v %v", p, err)
	}
	if *def.Limits.MaxDepth != 2 || *def.Limits.MaxJobsPerTurn != 10 || *def.Limits.MaxJobsPerTree != 30 ||
		*def.Limits.MaxCostUSDPerTree != 0 {
		t.Fatalf("defaults: %+v", def.Limits)
	}
	if !slices.Equal(def.Allow, []string{"*", "tool:*"}) || !slices.Equal(def.Forbid, []string{"tool:subagent"}) {
		t.Fatalf("default rules: allow %v forbid %v", def.Allow, def.Forbid)
	}
	// The starter parses and writes out every default.
	writePerm(t, path, PolicyStarter)
	if p, err := load(path, home); err != nil || !slices.Equal(p.Allow, def.Allow) || !slices.Equal(p.Forbid, def.Forbid) || !samePolicy(p, def) {
		t.Fatalf("starter: %+v %v", p, err)
	}
	// An older file, with the lists only, keeps its lists and gets the
	// defaults for the rest; a file may set any one value alone.
	writePerm(t, path, "allow = [\"go test\"]\n")
	if p, err := load(path, home); err != nil || !slices.Equal(p.Allow, []string{"go test"}) || !samePolicy(p, def) {
		t.Fatalf("older file: %+v %v", p, err)
	}
	// Retired switches still read, and are left out.
	writePerm(t, path, "edit = \"none\"\n[limits]\nmax_depth = 0\nmax_cost_usd_per_tree = 0.25\n[agent_api]\nscope = \"session\"\nstart = false\nrename = false\napply = true\n")
	if p, err := load(path, home); err != nil || *p.Limits.MaxDepth != 0 || *p.Limits.MaxJobsPerTurn != 10 ||
		*p.Limits.MaxCostUSDPerTree != 0.25 {
		t.Fatalf("set values: %+v %v", p, err)
	}
	writePerm(t, path, "subagents = false\n")
	if p, err := load(path, home); err != nil {
		t.Fatalf("subagents = false: %+v %v", p, err)
	}
	// Commands and comments.
	writePerm(t, path, "allow = [\"go test\", 'make test']\n"+
		"forbid = [\n  \"git push\", # a comment\n]\n")
	p, err := load(path, home)
	if err != nil {
		t.Fatal(err)
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
		{"empty command", "allow = [\" \"]\n", `allow entry " " is empty`},
		{"a flag as a command", "forbid = [\"--dangerously-skip-permissions\"]\n", "starts with -"},
		{"comma", "allow = [\"go test,Edit\"]\n", "comma or a parenthesis"},
		{"parenthesis", "forbid = [\"x)\"]\n", "comma or a parenthesis"},
		{"padded", "allow = [\"go test \"]\n", "starts or ends with a space"},
		{"rule syntax in a command", "allow = [\"go test:*\"]\n", "has a star"},
		{"two spaces", "allow = [\"go  test\"]\n", "two spaces in a row"},
		{"tab", "allow = [\"go\\ttest\"]\n", "control character"},
		{"isolation unknown", "isolation = \"everywhere\"\n", `isolation must be`},
		{"isolation not a string", "isolation = true\n", "wrong type"},
		{"limit negative", "[limits]\nmax_depth = -1\n", "max_depth must be 0 or more"},
		{"limit not a number", "[limits]\nmax_jobs_per_turn = \"10\"\n", "wrong type"},
		{"cost negative", "[limits]\nmax_cost_usd_per_tree = -0.5\n", "max_cost_usd_per_tree must be 0"},
		{"unknown limit", "[limits]\nmax_rounds = 2\n", `unknown key "limits.max_rounds"`},
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

// home_files: empty by default, entries only under ~, written back by
// FormatPolicy.
func TestHomeFilesPolicy(t *testing.T) {
	if p := DefaultPolicy(); p.HomeFiles == nil || len(p.HomeFiles) != 0 {
		t.Fatalf("default home_files: %v", p.HomeFiles)
	}
	p, err := ParsePolicy([]byte("home_files = [\"~/.aws/config\"]\n"))
	if err != nil || len(p.HomeFiles) != 1 {
		t.Fatalf("home_files: %v %v", p.HomeFiles, err)
	}
	if back, err := ParsePolicy(FormatPolicy(p)); err != nil || len(back.HomeFiles) != 1 || back.HomeFiles[0] != "~/.aws/config" {
		t.Errorf("FormatPolicy lost home_files: %v %v", back.HomeFiles, err)
	}
	for _, bad := range []string{`"/etc/passwd"`, `"~/../x"`, `"~/"`, `"~"`} {
		if _, err := ParsePolicy([]byte("home_files = [" + bad + "]\n")); err == nil || !strings.Contains(err.Error(), "home_files") {
			t.Errorf("%s: %v, want a refusal naming home_files", bad, err)
		}
		if _, err := DecodePolicy([]byte(`{"home_files":[` + bad + `]}`)); err == nil || !strings.Contains(err.Error(), "home_files") {
			t.Errorf("JSON %s: %v, want a refusal naming home_files", bad, err)
		}
		// The old [sandbox] read name reads as home_files, and is refused as it.
		if _, err := ParsePolicy([]byte("[sandbox]\nread = [" + bad + "]\n")); err == nil || !strings.Contains(err.Error(), "home_files") {
			t.Errorf("old %s: %v, want a refusal naming home_files", bad, err)
		}
		if _, err := DecodePolicy([]byte(`{"sandbox":{"read":[` + bad + `]}}`)); err == nil || !strings.Contains(err.Error(), "home_files") {
			t.Errorf("old JSON %s: %v, want a refusal naming home_files", bad, err)
		}
	}
}

// sandbox.read moved to home_files in 0.5.2: a permissions.toml or a PUT
// body that still holds it reads with its value as home_files, unless it
// sets home_files too, which wins. A wrong type is still refused. The old
// [sandbox] mode and [network], retired at the Copy level, are dropped
// without error.
func TestSandboxReadMoved(t *testing.T) {
	p, err := ParsePolicy([]byte("[sandbox]\nmode = \"on\"\nread = [\"~/.aws/config\"]\n"))
	if err != nil || len(p.HomeFiles) != 1 || p.HomeFiles[0] != "~/.aws/config" {
		t.Fatalf("file: %v %v", p.HomeFiles, err)
	}
	p, err = DecodePolicy([]byte(`{"sandbox":{"mode":"on","read":["~/.aws/config"]}}`))
	if err != nil || len(p.HomeFiles) != 1 || p.HomeFiles[0] != "~/.aws/config" {
		t.Fatalf("body: %v %v", p.HomeFiles, err)
	}
	p, err = ParsePolicy([]byte("home_files = [\"~/.gitconfig\"]\n[sandbox]\nread = [\"~/.aws/config\"]\n"))
	if err != nil || len(p.HomeFiles) != 1 || p.HomeFiles[0] != "~/.gitconfig" {
		t.Fatalf("file with both: %v %v", p.HomeFiles, err)
	}
	p, err = DecodePolicy([]byte(`{"home_files":["~/.gitconfig"],"sandbox":{"read":["~/.aws/config"]}}`))
	if err != nil || len(p.HomeFiles) != 1 || p.HomeFiles[0] != "~/.gitconfig" {
		t.Fatalf("body with both: %v %v", p.HomeFiles, err)
	}
	if _, err := ParsePolicy([]byte("[sandbox]\nread = \"~/.aws/config\"\n")); err == nil || !strings.Contains(err.Error(), "wrong type") {
		t.Errorf("file with a string: %v", err)
	}
	if _, err := DecodePolicy([]byte(`{"sandbox":{"read":"~/.aws/config"}}`)); err == nil || !strings.Contains(err.Error(), "wrong type") {
		t.Errorf("body with a string: %v", err)
	}
}

// commands, the switch of the person's "!" commands, is retired in 0.5: a
// permissions.toml or a PUT body from before that still holds it reads as
// if it did not, whatever its value, and the policy written back has no
// such key. Another unknown key is still refused.
func TestCommandsKeyRetired(t *testing.T) {
	for _, src := range []string{"commands = true\n", "commands = false\nsubagents = false\n", "commands = \"no\"\n"} {
		p, err := ParsePolicy([]byte(src))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if strings.Contains(string(FormatPolicy(p)), "\ncommands =") {
			t.Errorf("%q: the policy written back holds commands", src)
		}
	}
	p, err := DecodePolicy([]byte(`{"commands":false,"subagents":false}`))
	if err != nil {
		t.Fatalf("JSON with commands: %+v %v", p, err)
	}
	for _, src := range []string{"subscriptions_on_runners = \"own\"\n", "subscriptions_on_runners = \"off\"\n"} {
		p, err := ParsePolicy([]byte(src))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if strings.Contains(string(FormatPolicy(p)), "subscriptions_on_runners") {
			t.Errorf("%q: the policy written back holds subscriptions_on_runners", src)
		}
	}
	if _, err := DecodePolicy([]byte(`{"subscriptions_on_runners":"own"}`)); err != nil {
		t.Errorf("JSON with subscriptions_on_runners: %v", err)
	}
	if _, err := ParsePolicy([]byte("command = true\n")); err == nil || !strings.Contains(err.Error(), `unknown key "command"`) {
		t.Errorf("an unknown key: %v", err)
	}
	if _, err := DecodePolicy([]byte(`{"command":true}`)); err == nil {
		t.Error("JSON accepted an unknown key")
	}
}

// unlisted is retired in 0.6.5: a permissions.toml or a PUT body that still
// holds it reads as if it did not, whatever its value, and the policy written
// back has no such key. A command or tool on no list is always held now.
func TestUnlistedKeyRetired(t *testing.T) {
	for _, src := range []string{"unlisted = \"ask\"\n", "unlisted = \"refuse\"\n", "unlisted = true\n"} {
		p, err := ParsePolicy([]byte(src))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if strings.Contains(string(FormatPolicy(p)), "\nunlisted =") {
			t.Errorf("%q: the policy written back holds unlisted", src)
		}
	}
	if _, err := DecodePolicy([]byte(`{"unlisted":"refuse"}`)); err != nil {
		t.Errorf("JSON with unlisted: %v", err)
	}
}

// A vendor sign-in file is refused in home_files, in words that say why,
// however it is written; the usual config list holds none.
func TestHomeFilesSignInRefused(t *testing.T) {
	for _, f := range []string{"~/.claude/.credentials.json", "~/.codex/auth.json", "~/.codex//auth.json", "~/.codex/./auth.json",
		"~/.gemini/antigravity-cli/antigravity-oauth-token", "~/.gemini/oauth_creds.json", "~/.claude/.claude.json"} {
		_, err := ParsePolicy([]byte("home_files = [" + strconv.Quote(f) + "]\n"))
		if err == nil || !strings.Contains(err.Error(), "sign-ins stay with control") {
			t.Errorf("%s: %v, want a refusal", f, err)
		}
	}
	for _, f := range UsualHomeFiles {
		if SignInFile(f) != "" {
			t.Errorf("the usual list holds the sign-in %s", f)
		}
		if _, err := ParsePolicy([]byte("home_files = [" + strconv.Quote(f) + "]\n")); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// B16: JSON PATCH / PUT containing legacy switches (edit, subagents, agent_api.tools)
// converts them into rules rather than silently discarding them.
func TestDecodePolicyLegacySwitchesConverted(t *testing.T) {
	// 1. PUT with subagents: true enables subagents without fill restoring it into forbid
	pPut, err := DecodePolicy([]byte(`{"subagents":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pPut.Allow, "tool:subagent") {
		t.Fatalf("PUT subagents true: want tool:subagent in Allow, got %v", pPut.Allow)
	}
	if slices.Contains(pPut.Forbid, "tool:subagent") {
		t.Fatalf("PUT subagents true: tool:subagent should not be in Forbid, got %v", pPut.Forbid)
	}

	// 2. PATCH preserving existing rules
	dir := home(t)
	d, err := openDB(filepath.Join(dir, DBFile), fixedNow, len(upgradeSteps))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Initial policy with custom forbid and allow commands
	initPolicy := Policy{
		Allow:  []string{"git status", "tool:*", "tool:edit"},
		Forbid: []string{"rm -rf /", "tool:subagent"},
		Ask:    []string{"deploy"},
	}
	if _, _, err := d.SetPolicy(initPolicy, Any); err != nil {
		t.Fatal(err)
	}

	// PATCH tools: false
	pToolsOff, err := DecodePolicyChange([]byte(`{"agent_api":{"tools":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err := d.ChangePolicy(pToolsOff, Any)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.Forbid, "rm -rf /") || !slices.Contains(pol.Forbid, "tool:*") {
		t.Fatalf("tools false: want rm -rf / and tool:* in Forbid, got %v", pol.Forbid)
	}
	if slices.Contains(pol.Allow, "tool:*") {
		t.Fatalf("tools false: tool:* should be removed from Allow, got %v", pol.Allow)
	}
	// Editing is not taken away with tools off (the old default allowed it, and
	// no explicit edit was set), so it stays allowed and out of forbid.
	if !slices.Contains(pol.Allow, "tool:edit") || slices.Contains(pol.Forbid, "tool:edit") {
		t.Fatalf("tools false should keep editing allowed: allow %v, forbid %v", pol.Allow, pol.Forbid)
	}

	// PATCH edit: none
	pEditNone, err := DecodePolicyChange([]byte(`{"edit":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err = d.ChangePolicy(pEditNone, Any)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.Ask, "deploy") || !slices.Contains(pol.Ask, "tool:edit") {
		t.Fatalf("edit none: want deploy and tool:edit in Ask, got %v", pol.Ask)
	}
	if slices.Contains(pol.Allow, "tool:edit") {
		t.Fatalf("edit none: tool:edit should be removed from Allow, got %v", pol.Allow)
	}

	// PATCH subagents: true
	pSubagentsOn, err := DecodePolicyChange([]byte(`{"subagents":true}`))
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err = d.ChangePolicy(pSubagentsOn, Any)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.Allow, "git status") || !slices.Contains(pol.Allow, "tool:subagent") {
		t.Fatalf("subagents true: want git status and tool:subagent in Allow, got %v", pol.Allow)
	}
	if slices.Contains(pol.Forbid, "tool:subagent") {
		t.Fatalf("subagents true: tool:subagent should be removed from Forbid, got %v", pol.Forbid)
	}

	// 3. PATCH with null tools produces no change
	pNullTools, err := DecodePolicyChange([]byte(`{"agent_api":{"tools":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if pNullTools.legacyTools != nil {
		t.Fatalf("null tools should be left unset, got %v", *pNullTools.legacyTools)
	}

	// 4. Mixed PATCH with rules and legacy switch preserves all submitted lists
	pMixed, err := DecodePolicyChange([]byte(`{"subagents":true,"allow":["go test"],"forbid":["git push"],"ask":["deploy"]}`))
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err = d.ChangePolicy(pMixed, Any)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.Allow, "go test") || !slices.Contains(pol.Allow, "tool:subagent") {
		t.Fatalf("mixed patch: want go test and tool:subagent in Allow, got %v", pol.Allow)
	}
	if !slices.Contains(pol.Forbid, "git push") || slices.Contains(pol.Forbid, "tool:subagent") {
		t.Fatalf("mixed patch: want git push in Forbid (no tool:subagent), got %v", pol.Forbid)
	}
	if !slices.Contains(pol.Ask, "deploy") {
		t.Fatalf("mixed patch: want deploy in Ask, got %v", pol.Ask)
	}
}

// A legacy tools-off PATCH preserves the existing edit permission: it keeps
// editing allowed where it was (under tool:* or an explicit tool:edit), and
// does not grant it where the allow list did not have it.
func TestToolsOffPreservesEditPermission(t *testing.T) {
	off := func(allow []string) Policy {
		t.Helper()
		base := DefaultPolicy()
		base.Allow = allow
		patch, err := DecodePolicyChange([]byte(`{"agent_api":{"tools":false}}`))
		if err != nil {
			t.Fatal(err)
		}
		res, err := patch.over(base)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Default allow has tool:*, so editing was allowed and stays allowed.
	if p := off([]string{"*", "tool:*"}); !slices.Contains(p.Allow, "tool:edit") || slices.Contains(p.Allow, "tool:*") {
		t.Errorf("tools off under tool:*: editing should stay allowed, got allow %v", p.Allow)
	}
	// An empty allow did not allow editing, so tools off must not grant it.
	if p := off([]string{}); slices.Contains(p.Allow, "tool:edit") {
		t.Errorf("tools off with empty allow: editing must not be granted, got allow %v", p.Allow)
	}
}

func TestB16TOMLFalseSubagents(t *testing.T) {
	p, err := ParsePolicy([]byte("allow = [\"tool:subagent\"]\nsubagents = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Forbid, "tool:subagent") {
		t.Errorf("subagents=false did not produce Forbid tool:subagent; forbid=%v", p.Forbid)
	}
	if slices.Contains(p.Allow, "tool:subagent") {
		t.Errorf("subagents=false did not remove tool:subagent from Allow; allow=%v", p.Allow)
	}
}

func TestB16InvalidLegacyEdit(t *testing.T) {
	for _, decode := range []func([]byte) (Policy, error){DecodePolicy, DecodePolicyChange} {
		p, err := decode([]byte(`{"edit":"no-edits"}`))
		if err == nil {
			t.Errorf("accepted out-of-range legacy edit; allow=%q ask=%q forbid=%q", p.Allow, p.Ask, p.Forbid)
		}
	}
}

// carry_files: empty by default, patterns inside the folder only, written
// back by FormatPolicy, refused by name in TOML and JSON alike.
func TestCarryFilesPolicy(t *testing.T) {
	if p := DefaultPolicy(); p.CarryFiles == nil || len(p.CarryFiles) != 0 {
		t.Fatalf("default carry_files: %v", p.CarryFiles)
	}
	p, err := ParsePolicy([]byte("carry_files = [\".env\", \".env.*\", \"config/local.yml\"]\n"))
	if err != nil || len(p.CarryFiles) != 3 {
		t.Fatalf("carry_files: %v %v", p.CarryFiles, err)
	}
	if back, err := ParsePolicy(FormatPolicy(p)); err != nil || len(back.CarryFiles) != 3 || back.CarryFiles[1] != ".env.*" {
		t.Errorf("FormatPolicy lost carry_files: %v %v", back.CarryFiles, err)
	}
	for _, bad := range []string{`"/etc/passwd"`, `"~/.ssh/id"`, `"../x"`, `"a/../b"`, `".git/config"`, `"sub/.git"`, `""`, `"a//b"`, `"[x"`, `"a\\b"`} {
		if _, err := ParsePolicy([]byte("carry_files = [" + bad + "]\n")); err == nil || !strings.Contains(err.Error(), "carry_files") {
			t.Errorf("%s: %v, want a refusal naming carry_files", bad, err)
		}
		if _, err := DecodePolicy([]byte(`{"carry_files":[` + bad + `]}`)); err == nil || !strings.Contains(err.Error(), "carry_files") {
			t.Errorf("JSON %s: %v, want a refusal naming carry_files", bad, err)
		}
	}
}
