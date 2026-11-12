package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// codex is the Codex CLI.
type codex struct{}

func (codex) Info() Info {
	return Info{
		Name:    "codex",
		Label:   "Codex",
		Product: "the Codex CLI",
		// Answers with network access denied (codex-cli 0.164.0,
		// 2026-11-05, run under a sandbox profile that denies the network).
		StatusArgs: []string{"login", "status"},
		OwnSandbox: true,
		// Known limit: an openai_base_url in codex's own config.toml is not
		// visible here. The run's -c replaces it, so that endpoint is
		// bypassed.
		OwnEndpointEnv: "OPENAI_BASE_URL",
		// No model of our own to offer: which ones a ChatGPT sign-in may
		// use is the account's (gpt-5.5 was refused on one, 2026-11-12). A
		// new agent names none, so codex picks its own, and a typed name is
		// tried as it is: a refusal shows the vendor's own words.
		Access: standardAccess("ChatGPT sign-in", []string{}),
		Replay: FormatOpenAI,
		// codex on OpenRouter sends its input items without the content
		// kinds codex puts on them for chatgpt.com, which is how the codex
		// format finds the prompt and the context (codex-cli 0.167.0,
		// 2026-11-09).
		ReplayOnOpenRouter: false,
		// codex keeps its config.toml, sign-in, sessions and memories
		// under CODEX_HOME, which must exist.
		ConfigEnv: "CODEX_HOME", ConfigDir: ".codex",
		ToolServer: true,
		// codex login --device-auth prints a link and a one-time code
		// (XXXX-XXXXX), needs no terminal and reads no input, and keeps the
		// ChatGPT sign-in in $CODEX_HOME/auth.json: tokens.access_token (a
		// JWT that expires), tokens.refresh_token and tokens.account_id
		// (codex-cli 0.164.0, 2026-11-10, run with an empty CODEX_HOME; the
		// file's shape from codex-rs/login/src/auth/storage.rs and
		// token_data.rs). The file store is named: never the Keychain.
		// Renewal and the account header are codex's own
		// (codex-rs/login/src/auth/manager.rs, oauth/client.rs and
		// model-provider/src/bearer_auth_provider.rs, same version). The
		// client id is the public id of codex's own CLI, not a secret.
		Login: &Login{Args: []string{"login", "--device-auth", "-c", `cli_auth_credentials_store="file"`},
			Code: `^[A-Z0-9]{4,8}-[A-Z0-9]{4,8}$`,
			Home: "CODEX_HOME", File: "auth.json",
			TokenKey: "tokens.access_token", RefreshKey: "tokens.refresh_token", AccountKey: "tokens.account_id",
			RenewURL: "https://auth.openai.com/oauth/token", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
			AccountHeader: "ChatGPT-Account-ID"},
	}
}

// Wire: codex reads its base URL from config, not the environment. -c parses
// the value as TOML, so the quotes make it a string. Measured on codex-cli
// 0.164.0, 2026-11-05. The service routes /backend-api/codex to chatgpt.com.
//
// TODO(trade, 2026-11-05): with this wiring codex first tries a WebSocket,
// the service answers 426, and codex falls back to a POST but prints one
// ERROR line per run. No switch turns that attempt off for the built-in
// openai provider (--disable responses_websockets, prefer_websockets and
// supports_websockets change nothing; the provider cannot be overridden). A
// custom provider with supports_websockets=false removes the line, but codex
// files its sessions under that provider: `codex resume` then hides them and
// refuses to resume one ("model provider agentbus ... is no longer
// configured"). Keeping the person's sessions usable is worth more than the
// line. Revisit if codex gains a per-run switch.
func (codex) Wire(baseURL string) (env, args []string) {
	return nil, []string{"-c", `openai_base_url="` + baseURL + `/backend-api/codex"`}
}

// openRouterProvider is the id codex files the turn's provider under. The ids
// openai, ollama and lmstudio are reserved (codex config reference, read
// 2026-11-08).
const openRouterProvider = "agentbus_openrouter"

// WireOpenRouter: a provider of its own, set by -c flags for this run only,
// reading the key from the named variable itself. supports_websockets=false:
// codex then posts at once, with no WebSocket try the service would refuse.
func (codex) WireOpenRouter(w OpenRouterWire) (env, args []string) {
	p := "model_providers." + openRouterProvider + "."
	return nil, []string{
		"-c", "model_provider=" + openRouterProvider,
		"-c", p + `name="OpenRouter"`,
		"-c", p + `base_url="` + w.Base + `/v1"`,
		"-c", p + `env_key="` + w.KeyEnv + `"`,
		"-c", p + `wire_api="responses"`,
		"-c", p + "supports_websockets=false",
	}
}

// relayProvider is the id codex files a relayed turn's provider under.
const relayProvider = "agentbus_chatgpt"

// WireRelay: a provider of its own for this run, at the service's
// /backend-api/codex, reading the placeholder from AGENTBUS_CODEX_TOKEN as an
// API key: codex sends it as the bearer, and control's relay swaps in the
// ChatGPT sign-in and its account. Named "OpenAI", as codex's own provider
// is: codex then sends the request it sends chatgpt.com, with the content
// kinds own replay reads, which it leaves out for a provider of another name
// (codex-cli 0.164.0, 2026-11-10, measured against a local listener: the
// same body keys and item keys as the ChatGPT capture in
// adapter/openai/testdata, but access_programs, and no ChatGPT-Account-ID
// header, which the relay adds). supports_websockets=false: codex posts at
// once. A runner's sessions are its own, so codex filing them under this
// provider hides nothing from the person.
func (codex) WireRelay(baseURL, placeholder string) (env, args []string) {
	p := "model_providers." + relayProvider + "."
	return []string{codexRelayEnv + "=" + placeholder}, []string{
		"-c", "model_provider=" + relayProvider,
		"-c", p + `name="OpenAI"`,
		"-c", p + `base_url="` + baseURL + `/backend-api/codex"`,
		"-c", p + `env_key="` + codexRelayEnv + `"`,
		"-c", p + `wire_api="responses"`,
		"-c", p + "supports_websockets=false",
	}
}

// Refuse: every setting of permissions.toml has a flag on codex, or the
// gateway's guard.
func (codex) Refuse(Turn) string { return "" }

// codexRelayEnv holds a relayed turn's placeholder.
const codexRelayEnv = "AGENTBUS_CODEX_TOKEN"

// Edit: codex's sandbox takes its folder as it is, so any name works; what it
// cannot do is keep a protected file inside it read-only. A folder holding a
// protected file runs codex read-only.
func (codex) Edit(folders, protect []string) ([]string, string) {
	for _, p := range protect {
		for _, f := range folders {
			if p == f || strings.HasPrefix(p, strings.TrimSuffix(f, "/")+"/") {
				return nil, "data_folder_inside"
			}
		}
	}
	return folders, ""
}

// Native: codex reads the AGENTS.md of its folder by itself, into the
// context of every turn.
func (codex) Native(_ *os.Root, name string) bool { return name == "AGENTS.md" }

// ResumeID is the thread_id of the first "type":"thread.started" line that
// --json prints.
func (codex) ResumeID(line []byte, current string) string {
	if current != "" {
		return current
	}
	if v, ok := decodeLine(line); ok && v.Type == "thread.started" && resumeIDShape.MatchString(v.ThreadID) {
		return v.ThreadID
	}
	return current
}

func (codex) Args(t Turn) []string {
	a := []string{"exec", "--skip-git-repo-check"}
	if t.ResumeID == "" {
		// --json prints events as lines; the first is thread.started
		// with the thread_id (codex-cli 0.164.0, 2026-11-05).
		a = append(a, "--json")
	}
	// The sandbox is always named, so a turn gets what read_only says
	// and not codex's own default, which for `codex exec` is read-only
	// (acceptance soak, 2026-11-06: every turn with read_only false
	// could not write). `codex exec resume` takes no --sandbox of its
	// own. Given to exec before resume it reaches the resumed turn: the
	// turn's recorded sandbox policy follows it (codex-cli 0.164.0,
	// 2026-11-05).
	// workspace-write only with an edit rule: edit = "none", or a
	// folder codex could not be kept out of the data folder's files
	// in, runs read-only (Edit).
	write := !t.Writes.ReadOnly && len(t.Writes.Edit) > 0
	switch {
	case t.Walled:
		// Inside the runner's sandbox codex's own cannot be built: every
		// command would exit 71 (codex-cli 0.164.0, 2026-11-11). The wall
		// is the boundary, and keeps the folder read only when !write.
		a = append(a, "--sandbox", "danger-full-access")
	default:
		a = append(a, "--sandbox", codexSandbox(!write))
	}
	// The hint as codex's developer_instructions, which codex puts in a
	// developer message of the context before the prompt; own replay
	// keeps it from the session's first turn.
	if t.Hint != "" {
		a = append(a, "-c", "developer_instructions="+tomlString(t.Hint))
	}
	// The tool server, for this run only: a Streamable HTTP server whose
	// bearer codex reads from AGENTBUS_TOKEN itself, so the token is in no
	// argument (codex-cli 0.164.0, 2026-11-11, checked live).
	if t.Tools != "" {
		p := "mcp_servers." + ToolServer + "."
		a = append(a, "-c", p+"url="+tomlString(t.Tools), "-c", p+`bearer_token_env_var="AGENTBUS_TOKEN"`)
	}
	// A turn that may write may also reach the network, as Claude Code
	// can under typical settings: the person's decision, 2026-11-06. Per
	// turn, on the command line; ~/.codex/config.toml is never edited.
	if write && !t.Walled {
		a = append(a, codexNetwork()...)
	}
	// permissions.toml gives codex no flags. read_folders: codex's
	// sandbox reads every folder already, in read-only and in
	// workspace-write, and its --add-dir makes a folder writable, which
	// read_folders never means (codex-cli 0.164.0, 2026-11-07, checked
	// with codex sandbox). allow: codex has no per-command flag. forbid
	// reaches codex through the gateway's guard (gateway/guard.go).
	// subagents = false: both of codex's multi-agent features off for this
	// run. multi_agent is on by default, multi_agent_v2 off, and either
	// lets a turn spawn agents (codex-cli 0.167.0, 2026-11-09, `codex
	// features list`). Before resume, it reaches the resumed turn as
	// --sandbox does.
	if t.NoSubagents {
		a = append(a, "--disable", "multi_agent", "--disable", "multi_agent_v2")
	}
	if t.ResumeID != "" {
		a = append(a, "resume", "--json")
	}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	a = append(a, "--")
	if t.ResumeID != "" {
		// After "--" too, so an id can never be read as a flag.
		a = append(a, t.ResumeID)
	}
	return append(a, t.Prompt)
}

// OwnInstructions says the person set codex's developer_instructions
// themselves, in $CODEX_HOME/config.toml (~/.codex by default, read from env,
// the child's environment) or in a .codex/config.toml of the folder or any
// folder above it: codex reads a project's config from its root down to the
// folder it runs in (codex-cli 0.164.0, 2026-11-07, found in review). Every
// folder above is checked, so a file codex would skip still leaves the hint
// out, which keeps the person's setting either way. A -c value
// would replace theirs, so a turn then gets no hint: their own setting wins.
// A top-level key is a line naming it before the first table header; nothing
// else of the file is read, and the file is never written.
func (codex) OwnInstructions(env []string, folder string) string {
	if codexOwnInstructions(env, folder) {
		return "codex_developer_instructions_set"
	}
	return ""
}

func codexOwnInstructions(env []string, folder string) bool {
	return slices.ContainsFunc(codexConfigs(env, folder), func(f string) bool { return topLevelKey(f, "developer_instructions") })
}

// OwnTools says the person defined a codex MCP server named as the tool
// server (harness.ToolServer) in a config.toml codex reads, so the turn gets
// no tool server and its instructions name agentbus-api: their entry wins.
// A -c override cannot take theirs away. codex merges -c into their table
// field by field, even given a whole table: a stdio entry with a url then
// stops codex from loading its config ("url is not supported for stdio"),
// and enabled = false keeps the server off (codex-cli 0.164.0, 2026-11-11,
// codex mcp list with a scratch CODEX_HOME; found in review). The file is
// only read.
func (codex) OwnTools(env []string, folder string) string {
	for _, f := range codexConfigs(env, folder) {
		if codexDefinesServer(f, ToolServer) {
			return "codex_mcp_server_set"
		}
	}
	return ""
}

// codexDefinesServer says the TOML file defines mcp_servers.<name>. A
// missing file defines nothing; one that does not parse is taken to define
// it, so the person's file is never overridden on a guess.
func codexDefinesServer(path, name string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var v struct {
		MCPServers map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(string(b), &v); err != nil {
		return true
	}
	_, ok := v.MCPServers[name]
	return ok
}

// codexConfigs are the config.toml files codex reads for a turn in folder,
// with env its environment: $CODEX_HOME/config.toml (~/.codex by default),
// then the .codex/config.toml of the folder and of every folder above it.
func codexConfigs(env []string, folder string) []string {
	var out []string
	var home, codexHome string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME":
			home = v
		case "CODEX_HOME":
			codexHome = v
		}
	}
	if codexHome == "" && home != "" {
		codexHome = filepath.Join(home, ".codex")
	}
	// A relative CODEX_HOME is read by codex from its own working folder,
	// the turn's folder, not from the service's (found in review).
	if codexHome != "" && !filepath.IsAbs(codexHome) {
		codexHome = filepath.Join(folder, codexHome)
	}
	if codexHome != "" {
		out = append(out, filepath.Join(codexHome, "config.toml"))
	}
	// Both the folder as given and its real path: codex walks the physical
	// parents of a folder reached through a symlink (found in review).
	starts := []string{filepath.Clean(folder)}
	if real, err := filepath.EvalSymlinks(folder); err == nil && real != starts[0] {
		starts = append(starts, real)
	}
	for _, start := range starts {
		for dir := start; ; dir = filepath.Dir(dir) {
			out = append(out, filepath.Join(dir, ".codex", "config.toml"))
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return out
}

// topLevelKey says a TOML file sets key before its first table header. A
// missing or unreadable file sets nothing.
func topLevelKey(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return false
		}
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.Trim(strings.TrimSpace(k), `"'`) == key {
			return true
		}
	}
	return false
}

// tomlString quotes s as a TOML basic string, for a codex -c value, which
// codex reads as TOML. A JSON string is one: TOML takes the same escapes,
// \uXXXX among them.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// codexSandbox is the value of codex's --sandbox for a turn: read-only when
// read_only is true, else workspace-write, which lets codex write in the
// turn's folder (and its temporary folders) and nowhere else (codex-cli
// 0.164.0, `codex exec --help`). danger-full-access is used only inside the
// runner's sandbox (Turn.Walled).
func codexSandbox(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "workspace-write"
}

// codexNetwork turns on network access in codex's workspace-write sandbox,
// which has it off by default. Without it a codex turn could not reach the
// service with its turn token: curl exited 7 (acceptance soak, 2026-11-06,
// item 2). The key is the [sandbox_workspace_write] table's network_access
// (codex-cli 0.164.0). A read-only turn never gets it.
func codexNetwork() []string {
	return []string{"-c", "sandbox_workspace_write.network_access=true"}
}
