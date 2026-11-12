package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A developer_instructions the person set in codex's own config wins: the
// turn then gets no hint, since a -c value would replace theirs.
func TestCodexOwnInstructions(t *testing.T) {
	home, folder := t.TempDir(), t.TempDir()
	env := []string{"HOME=" + home}
	if codexOwnInstructions(env, folder) {
		t.Fatal("no config, yet own instructions")
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(home, ".codex", "config.toml")
	write(cfg, "model = \"x\"\n[profiles.a]\ndeveloper_instructions = \"in a table\"\n")
	if codexOwnInstructions(env, folder) {
		t.Error("a key inside a table counted as top-level")
	}
	write(cfg, "# mine\ndeveloper_instructions = \"\"\"\nbe brief\n\"\"\"\n[x]\n")
	if !codexOwnInstructions(env, folder) {
		t.Error("a top-level key in ~/.codex/config.toml not seen")
	}
	other := t.TempDir()
	write(filepath.Join(other, "config.toml"), "developer_instructions = 'x'\n")
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir(), "CODEX_HOME=" + other}, folder) {
		t.Error("$CODEX_HOME/config.toml not read")
	}
	// A relative CODEX_HOME is the turn folder's, never the service's.
	rel := t.TempDir()
	write(filepath.Join(rel, "state", "codex", "config.toml"), "developer_instructions = \"x\"\n")
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir(), "CODEX_HOME=state/codex"}, rel) {
		t.Error("a relative CODEX_HOME not read against the turn's folder")
	}
	if codexOwnInstructions([]string{"HOME=" + t.TempDir(), "CODEX_HOME=state/codex"}, t.TempDir()) {
		t.Error("a relative CODEX_HOME read against another folder")
	}
	sub := filepath.Join(folder, "a", "b")
	if codexOwnInstructions([]string{"HOME=" + t.TempDir()}, sub) {
		t.Fatal("own instructions with no project config")
	}
	write(filepath.Join(folder, ".codex", "config.toml"), "developer_instructions = \"x\"\n")
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir()}, folder) {
		t.Error("the folder's .codex/config.toml not read")
	}
	// A project config above the folder counts too: codex reads it.
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir()}, sub) {
		t.Error("a parent folder's .codex/config.toml not read")
	}
	// A folder reached through a symlink: its real parents count.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir()}, link) {
		t.Error("a symlinked folder's real parents not read")
	}
	if a := (codex{}).Args(Turn{Prompt: "hi"}); slices.ContainsFunc(a, func(s string) bool { return strings.HasPrefix(s, "developer_instructions=") }) {
		t.Errorf("no hint, yet %q", a)
	}
}

// A TOML basic string: quoted, with JSON's escapes, which TOML shares.
func TestTOMLString(t *testing.T) {
	if got := tomlString("a\"b\\c\n<d>"); got != `"a\"b\\c\n`+"\\u003cd\\u003e\"" {
		t.Errorf("tomlString %s", got)
	}
}

// Inside the runner's sandbox codex's own sandbox is off, whether the turn
// may write or not, and the network flag goes with it: macOS cannot build
// a Seatbelt sandbox inside another (2026-11-11). Outside it, as before.
func TestCodexWalled(t *testing.T) {
	sandbox := func(a []string) string {
		i := slices.Index(a, "--sandbox")
		if i < 0 || i+1 >= len(a) {
			return ""
		}
		return a[i+1]
	}
	writes := Writes{Edit: []string{"/f"}}
	for _, c := range []struct {
		turn    Turn
		want    string
		network bool
	}{
		{Turn{Writes: writes, Walled: true, Prompt: "p"}, "danger-full-access", false},
		{Turn{Writes: Writes{ReadOnly: true}, Walled: true, Prompt: "p"}, "danger-full-access", false},
		{Turn{Writes: writes, Prompt: "p"}, "workspace-write", true},
		{Turn{Writes: Writes{ReadOnly: true}, Prompt: "p"}, "read-only", false},
	} {
		a := codex{}.Args(c.turn)
		if got := sandbox(a); got != c.want {
			t.Errorf("walled %t, read only %t: --sandbox %s, want %s", c.turn.Walled, c.turn.Writes.ReadOnly, got, c.want)
		}
		if got := slices.Contains(a, "sandbox_workspace_write.network_access=true"); got != c.network {
			t.Errorf("walled %t: network flag %t, want %t", c.turn.Walled, got, c.network)
		}
	}
}
