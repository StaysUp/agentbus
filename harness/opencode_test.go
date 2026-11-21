package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runConfig is the run's OpenCode config of env, decoded.
func runConfig(t *testing.T, env []string) map[string]any {
	t.Helper()
	v := envValue(env, opencodeConfigEnv)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(v), &cfg); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", opencodeConfigEnv, err, v)
	}
	return cfg
}

// On OpenRouter, opencode gets a provider of the run's own at the service,
// reading the key from the named variable itself, with the title agent off.
// The key is in no argument and not in the config.
func TestOpencodeWireOpenRouter(t *testing.T) {
	h, ok := Get("opencode")
	if !ok {
		t.Fatal("opencode is not registered")
	}
	env, args := h.WireOpenRouter(OpenRouterWire{Base: "http://127.0.0.1:7/s/s1/t/t_1/openrouter",
		KeyEnv: "OPENROUTER_API_KEY", Key: "sk-or-secret", Model: "z-ai/glm-5.3-flash"})
	if len(args) != 0 {
		t.Errorf("args %q", args)
	}
	if strings.Contains(strings.Join(env, "\n"), "sk-or-secret") {
		t.Error("the key is in the child's config")
	}
	// opencode takes its folder from PWD first: the service's own is emptied.
	if !slices.Contains(env, "PWD=") {
		t.Errorf("env %q does not empty PWD", env)
	}
	cfg := runConfig(t, env)
	p := cfg["provider"].(map[string]any)[opencodeProvider].(map[string]any)
	opts := p["options"].(map[string]any)
	if p["npm"] != "@ai-sdk/openai-compatible" || opts["baseURL"] != "http://127.0.0.1:7/s/s1/t/t_1/openrouter/v1" ||
		opts["apiKey"] != "{env:OPENROUTER_API_KEY}" {
		t.Errorf("provider %v", p)
	}
	if _, ok := p["models"].(map[string]any)["z-ai/glm-5.3-flash"]; !ok || cfg["model"] != "agentbus_openrouter/z-ai/glm-5.3-flash" {
		t.Errorf("model %v, models %v", cfg["model"], p["models"])
	}
	if title := cfg["agent"].(map[string]any)["title"].(map[string]any); title["disable"] != true {
		t.Errorf("title agent %v", title)
	}
	if cfg["share"] != "disabled" || cfg["autoupdate"] != false {
		t.Errorf("share %v autoupdate %v", cfg["share"], cfg["autoupdate"])
	}
}

// The turn's agent is added to the run's config, the wire's provider and
// title setting kept: every command no allow rule names is refused, "*"
// coming first since OpenCode's last matching rule wins.
func TestOpencodeTurnEnv(t *testing.T) {
	h, _ := Get("opencode")
	env, _ := h.WireOpenRouter(OpenRouterWire{Base: "http://b/openrouter", KeyEnv: "K_KEY", Model: "m"})
	turn := Turn{Allow: []string{"tool:*", "api:x", "git status", "go test", "rm *", "a,b", "  "},
		Writes: Writes{Edit: []string{"/work"}}, NoSubagents: true}
	set := TurnEnv(h, turn, env)
	if len(set) != 1 || !strings.HasPrefix(set[0], opencodeConfigEnv+"=") {
		t.Fatalf("set %q", set)
	}
	raw := strings.TrimPrefix(set[0], opencodeConfigEnv+"=")
	cfg := runConfig(t, set)
	if cfg["model"] != "agentbus_openrouter/m" || cfg["agent"].(map[string]any)["title"] == nil {
		t.Errorf("the wire's config was not kept: %s", raw)
	}
	ag := cfg["agent"].(map[string]any)[opencodeAgent].(map[string]any)
	perm := ag["permission"].(map[string]any)
	if ag["mode"] != "primary" || perm["edit"] != "allow" || perm["task"] != "deny" {
		t.Errorf("agent %v", ag)
	}
	bash := perm["bash"].(map[string]any)
	want := map[string]any{"*": "deny", "git status *": "allow", "go test *": "allow"}
	if len(bash) != len(want) {
		t.Errorf("bash %v, want %v", bash, want)
	}
	for k, v := range want {
		if bash[k] != v {
			t.Errorf("bash[%q] = %v, want %v", k, bash[k], v)
		}
	}
	if !strings.Contains(raw, `"bash":{"*":"deny","git status *":"allow","go test *":"allow"}`) {
		t.Errorf("bash rules not in order: %s", raw)
	}

	// No edit rule: edits are refused; subagents on: task is left alone.
	cfg = runConfig(t, TurnEnv(h, Turn{}, env))
	perm = cfg["agent"].(map[string]any)[opencodeAgent].(map[string]any)["permission"].(map[string]any)
	if perm["edit"] != "deny" || perm["task"] != nil {
		t.Errorf("permission %v", perm)
	}
	// A command starting with a character that sorts before "*" still comes
	// after it.
	b, _ := json.Marshal(orderedRules{"*": "deny", "!x *": "allow", "a *": "allow"})
	if string(b) != `{"*":"deny","!x *":"allow","a *":"allow"}` {
		t.Errorf("ordered %s", b)
	}
}

func TestOpencodeArgs(t *testing.T) {
	h, _ := Get("opencode")
	if got, want := h.Args(Turn{Prompt: "hi", Model: "m"}), []string{"run", "--format", "json", "--agent", "agentbus", "--", "hi"}; !slices.Equal(got, want) {
		t.Errorf("args %q, want %q", got, want)
	}
	id := "ses_19a7c2e4f5b6Ab3dE4fG5hI6jK"
	if got, want := h.Args(Turn{Prompt: "-p", ResumeID: id}), []string{"run", "--format", "json", "--agent", "agentbus", "--session", id, "--", "-p"}; !slices.Equal(got, want) {
		t.Errorf("args %q, want %q", got, want)
	}
}

// The resume id is the sessionID every --format json line carries.
func TestOpencodeResumeID(t *testing.T) {
	h, _ := Get("opencode")
	id := "ses_19a7c2e4f5b6Ab3dE4fG5hI6jK"
	for _, tc := range []struct {
		line, current, want string
	}{
		{`{"type":"step_start","timestamp":1,"sessionID":"` + id + `","part":{}}`, "", id},
		{`{"type":"text","sessionID":"ses_other"}`, id, id},
		{`{"type":"text","sessionID":"ses_short"}`, "", ""},
		{`{"type":"text","sessionID":"--help"}`, "", ""},
		{`{"sessionID":"` + id + `"}`, "", ""},
		{`not json ` + id, "", ""},
	} {
		if got := h.ResumeID([]byte(tc.line), tc.current); got != tc.want {
			t.Errorf("%s (current %q): %q, want %q", tc.line, tc.current, got, tc.want)
		}
	}
}

// forbid and ask on shell commands refuse the turn: neither can be kept.
// Tool and API rules do not.
func TestOpencodeRefuse(t *testing.T) {
	h, _ := Get("opencode")
	if h.Refuse(Turn{Forbid: []string{"tool:subagent"}, Ask: []string{"api:x"}, NoSubagents: true}) != "" {
		t.Error("a turn with tool and API rules only was refused")
	}
	if !strings.Contains(h.Refuse(Turn{Forbid: []string{"git push"}}), "forbid") {
		t.Error("forbid on a command was not refused")
	}
	if !strings.Contains(h.Refuse(Turn{Ask: []string{"curl"}}), "ask") {
		t.Error("ask on a command was not refused")
	}
}

// A folder is edited only where OpenCode's project is the folder itself: a
// git repository whose root is above it, or a protected file inside it, runs
// the turn without edits.
func TestOpencodeEdit(t *testing.T) {
	h, _ := Get("opencode")
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "sub")
	for _, d := range []string{plain, filepath.Join(repo, ".git"), sub} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		folder, protect, why string
	}{
		{plain, "/elsewhere/policy.toml", ""},
		{repo, "/elsewhere/policy.toml", ""},
		{sub, "/elsewhere/policy.toml", "opencode_repository_above"},
		{plain, filepath.Join(plain, "policy.toml"), "data_folder_inside"},
	} {
		edit, why := h.Edit([]string{tc.folder}, []string{tc.protect})
		if why != tc.why || (why == "") != (len(edit) == 1) {
			t.Errorf("%s: edit %q why %q, want why %q", tc.folder, edit, why, tc.why)
		}
	}
}

// opencode reads its folder's AGENTS.md by itself; a turn's hint has no
// place in it.
func TestOpencodeContext(t *testing.T) {
	h, _ := Get("opencode")
	if !h.Native(nil, "AGENTS.md") || h.Native(nil, "CLAUDE.md") {
		t.Error("Native")
	}
	if h.OwnInstructions(nil, "/f") == "" {
		t.Error("a hint would be given")
	}
	if h.Info().ToolServer || h.Info().Replay != "" || harnessOffers(h, Subscription) || harnessOffers(h, API) || !harnessOffers(h, OpenRouter) {
		t.Errorf("info %+v", h.Info())
	}
}

func harnessOffers(h Harness, kind string) bool { return Offers(h.Info(), kind) }
