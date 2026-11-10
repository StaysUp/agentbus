package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// Each harness says which of the folder's files it reads by itself: claude
// its CLAUDE.md files and what such a file is or imports, and AGENTS.md when
// it has no CLAUDE.md; codex AGENTS.md.
func TestNative(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string // name: content, or "->target" for a symlink
		want  bool              // claude reads AGENTS.md by itself
	}{
		{"only AGENTS.md", map[string]string{"AGENTS.md": "a"}, true},
		{"CLAUDE.md of its own", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "c"}, false},
		{".claude/CLAUDE.md of its own", map[string]string{"AGENTS.md": "a", ".claude/CLAUDE.md": "c"}, false},
		{"CLAUDE.md a symlink out of the folder", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "->/nonexistent/CLAUDE.md"}, false},
		{"CLAUDE.md a symlink to AGENTS.md", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "->AGENTS.md"}, true},
		{".claude/CLAUDE.md a symlink to it", map[string]string{"AGENTS.md": "a", ".claude/CLAUDE.md": "->../AGENTS.md"}, true},
		{"CLAUDE.md imports it", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "intro\n  @AGENTS.md  \n"}, true},
		{"CLAUDE.md imports ./AGENTS.md", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "@./AGENTS.md\n"}, true},
		{"CLAUDE.md names it in a sentence", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "see @AGENTS.md for more\n"}, false},
		{"no AGENTS.md", map[string]string{"CLAUDE.md": "@AGENTS.md\n"}, false},
	}
	claude, _ := Get("claude")
	codex, _ := Get("codex")
	for _, c := range cases {
		dir := t.TempDir()
		for name, content := range c.files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			if len(content) > 2 && content[:2] == "->" {
				err = os.Symlink(content[2:], p)
			} else {
				err = os.WriteFile(p, []byte(content), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := claude.Native(root, "AGENTS.md"); got != c.want {
			t.Errorf("%s: claude reads AGENTS.md by itself = %t, want %t", c.name, got, c.want)
		}
		if !codex.Native(root, "AGENTS.md") || codex.Native(root, "CLAUDE.md") {
			t.Errorf("%s: codex reads AGENTS.md by itself, and nothing else", c.name)
		}
		root.Close()
	}
}

// Every harness names its config folder's variable, so a turn in a home of
// its own reads nothing of the runner user's.
func TestConfigDir(t *testing.T) {
	for _, h := range All() {
		if i := h.Info(); i.ConfigEnv == "" || i.ConfigDir == "" || filepath.IsAbs(i.ConfigDir) {
			t.Errorf("%q: config folder %q in %q", i.Name, i.ConfigDir, i.ConfigEnv)
		}
	}
}
