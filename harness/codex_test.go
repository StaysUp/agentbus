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
		// edit = "none" (no Edit rule): read-only sandbox. Read-only turns retired (B12).
		{Turn{Walled: true, Prompt: "p"}, "danger-full-access", false},
		{Turn{Writes: writes, Prompt: "p"}, "workspace-write", true},
		{Turn{Prompt: "p"}, "read-only", false},
	} {
		a := codex{}.Args(c.turn)
		if got := sandbox(a); got != c.want {
			t.Errorf("walled %t, edit %v: --sandbox %s, want %s", c.turn.Walled, c.turn.Writes.Edit, got, c.want)
		}
		if got := slices.Contains(a, "sandbox_workspace_write.network_access=true"); got != c.network {
			t.Errorf("walled %t: network flag %t, want %t", c.turn.Walled, got, c.network)
		}
	}
}

// Only the slugs of the models codex's own cache lists are read: a hidden one
// is left out, and nothing else of the file is kept.
func TestCodexModelsCache(t *testing.T) {
	i := codex{}.Info()
	if i.ModelsCache != "models_cache.json" || i.ReadModelsCache == nil {
		t.Fatalf("%+v", i.ModelsCache)
	}
	b := []byte(`{"fetched_at":"2026-11-13T10:00:00Z","etag":"e","identity":{"account":"someone@example.com"},` +
		`"models":[{"slug":"gpt-6.1-sol","visibility":"list","display_name":"x"},{"slug":"gpt-reserve","visibility":"hide"},{"slug":"gpt-6-luna"},{"visibility":"list"}]}`)
	if got := i.ReadModelsCache(b); !slices.Equal(got, []string{"gpt-6.1-sol", "gpt-6-luna"}) {
		t.Fatalf("%v", got)
	}
	if got := i.ReadModelsCache([]byte("not json")); got != nil {
		t.Fatalf("%v", got)
	}
}

// A relayed turn's provider names the relay's list of models as its
// model_catalog_url, once: without it a turn that names no model ran on
// codex's built-in default, gpt-6.2-sol, which the ChatGPT account refused;
// with it, on gpt-6-luna, the account's own (codex-cli 0.171.0, 2026-11-27,
// #112).
func TestCodexWireRelayNamesTheModelCatalog(t *testing.T) {
	env, args := codex{}.WireRelay("http://127.0.0.1:8787/s/s1/t/t_1", "abs_x")
	want := `model_providers.agentbus_chatgpt.model_catalog_url="http://127.0.0.1:8787/s/s1/t/t_1/backend-api/codex/models"`
	n := 0
	for i, a := range args {
		if strings.Contains(a, "model_catalog_url") {
			n++
			if a != want || i == 0 || args[i-1] != "-c" {
				t.Fatalf("%q", args)
			}
		}
	}
	if n != 1 || len(env) != 1 || env[0] != "AGENTBUS_CODEX_TOKEN=abs_x" {
		t.Fatalf("%q\n%q", env, args)
	}
}

// codex's model server: wired as a relayed turn, whose provider names the
// relay's list of models as its model_catalog_url, with the file sign-in
// store, and app-server last (codex-cli 0.164.0, 2026-11-14).
func TestCodexWireModels(t *testing.T) {
	env, args := codex{}.WireModels("http://127.0.0.1:9/relay/chatgpt", "abm_x")
	want := []string{"-c", "model_provider=agentbus_chatgpt",
		"-c", `model_providers.agentbus_chatgpt.name="OpenAI"`,
		"-c", `model_providers.agentbus_chatgpt.base_url="http://127.0.0.1:9/relay/chatgpt/backend-api/codex"`,
		"-c", `model_providers.agentbus_chatgpt.env_key="AGENTBUS_CODEX_TOKEN"`,
		"-c", `model_providers.agentbus_chatgpt.wire_api="responses"`,
		"-c", "model_providers.agentbus_chatgpt.supports_websockets=false",
		"-c", `model_providers.agentbus_chatgpt.model_catalog_url="http://127.0.0.1:9/relay/chatgpt/backend-api/codex/models"`,
		"-c", `cli_auth_credentials_store="file"`,
		"app-server"}
	if len(env) != 1 || env[0] != "AGENTBUS_CODEX_TOKEN=abm_x" || strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q\n%q", env, args)
	}
	var _ ModelServer = codex{}
}
