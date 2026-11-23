package harness

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// opencode is OpenCode (opencode-ai), a command-line agent that works with
// many model providers.
//
// Everything here was read in OpenCode's source at v1.19.6
// (github.com/anomalyco/opencode, packages/opencode/src) and checked live with
// opencode-ai 1.19.6 on 2026-11-21, unless it says otherwise.
type opencode struct{}

func (opencode) Info() Info {
	return Info{
		Name:    "opencode",
		Label:   "OpenCode",
		Product: "OpenCode (npm install -g opencode-ai)",
		// No offline sign-in status: OpenCode signs in per model provider,
		// and only its OpenRouter access is offered here, which needs none.
		StatusArgs: nil,
		// The run's config is OPENCODE_CONFIG_CONTENT (WireOpenRouter): one
		// already set where the service runs may point OpenCode at an
		// endpoint of the person's, which the run's would replace.
		OwnEndpointEnv: opencodeConfigEnv,
		// Only an OpenRouter key: OpenCode sends it Chat Completions through
		// an @ai-sdk/openai-compatible provider (WireOpenRouter), recorded by
		// adapter/chatcompletions. Its own provider sign-ins (opencode auth)
		// cannot reach the service yet, and it offers no vendor API key.
		Access: []Access{
			{Kind: Subscription, Label: "OpenCode's own sign-in", Disabled: true, Models: []string{}},
			{Kind: OpenRouter, Label: "OpenRouter key", Models: OpenRouterModels, AnyModel: true},
		},
		// Own replay reads Chat Completions (replay.NewChatCompletions): a
		// next turn starts fresh, without --session, and the OpenRouter
		// gateway puts the earlier turn's history in front of each request.
		Replay:             FormatChatCompletions,
		ReplayOnOpenRouter: true,
		// OpenCode keeps its global config in $XDG_CONFIG_HOME/opencode, and
		// its sessions in $XDG_DATA_HOME/opencode (global/index.ts, through
		// xdg-basedir); a session's home unsets the XDG variables, so both
		// fall back under its HOME. No variable of its own moves the config
		// folder (OPENCODE_CONFIG_DIR adds one; it does not replace it).
		ConfigDir: ".config/opencode",
	}
}

// Wire is never used: OpenCode offers no subscription access (Info).
func (opencode) Wire(string) (env, args []string) { return nil, nil }

// WireAPI is never used: OpenCode offers no access "api" (Info.APIKey).
func (opencode) WireAPI(APIWire) (env, args []string) { return nil, nil }

// WireRelay is never used: OpenCode has no Login.
func (opencode) WireRelay(string, string) (env, args []string) { return nil, nil }

// opencodeProvider is the id the turn's provider has in OpenCode's config,
// and so the first half of its model's name (agentbus_openrouter/<model>).
const opencodeProvider = "agentbus_openrouter"

// opencodeAgent is the agent a turn runs as (Args' --agent), defined for the
// run in OPENCODE_CONFIG_CONTENT (TurnEnv).
const opencodeAgent = "agentbus"

// opencodeConfigEnv holds the run's config: OpenCode merges it after the
// person's global and project config files (config/config.ts), so it wins
// over them key by key, and nothing is written to a file.
const opencodeConfigEnv = "OPENCODE_CONFIG_CONTENT"

// WireOpenRouter: a provider of the run's own, at the service's OpenRouter
// path, through OpenCode's bundled @ai-sdk/openai-compatible package, which
// sends POST <baseURL>/chat/completions with stream_options.include_usage on
// (provider/provider.ts sets includeUsage for that package). Its key is
// "{env:<KeyEnv>}": OpenCode replaces that with the variable's value when it
// reads the config (config/variable.ts), so the key is in no argument and no
// file. The title agent is off: OpenCode otherwise asks the same model for a
// session title before the turn (session/prompt.ts, ensureTitle, which
// returns when there is no "title" agent), and that call would read as a
// turn of its own. Sharing and its own updates are off for the run.
//
// PWD is emptied: opencode run takes its project folder from PWD before its
// working folder (cli/cmd/run.ts: process.env.PWD ?? process.cwd()), and the
// PWD a child inherits is the service's own, not the turn's folder it starts
// in. Checked live: with the service's PWD, a turn wrote a file into the
// folder agentbus was started from. Empty, it resolves to the working folder.
func (opencode) WireOpenRouter(w OpenRouterWire) (env, args []string) {
	cfg := map[string]any{
		"provider": map[string]any{opencodeProvider: map[string]any{
			"npm":     "@ai-sdk/openai-compatible",
			"name":    "OpenRouter",
			"options": map[string]any{"baseURL": w.Base + "/v1", "apiKey": "{env:" + w.KeyEnv + "}"},
			"models":  map[string]any{w.Model: map[string]any{}},
		}},
		"model":      opencodeProvider + "/" + w.Model,
		"agent":      map[string]any{"title": map[string]any{"disable": true}},
		"share":      "disabled",
		"autoupdate": false,
	}
	b, _ := json.Marshal(cfg)
	return []string{opencodeConfigEnv + "=" + string(b), "PWD="}, nil
}

// TurnEnv adds the turn's agent to the run's config (WireOpenRouter's
// OPENCODE_CONFIG_CONTENT, read back from env): its permission rules, which
// OpenCode applies after the person's own (agent/agent.ts merges an agent's
// permission last, and the last rule that matches wins), so the person's
// config cannot widen them. "*" deny under bash refuses every command no
// allow rule names; the default ask would be refused too, since opencode run
// refuses whatever asks (cli/cmd/run.ts, permission.asked).
func (opencode) TurnEnv(t Turn, env []string) []string {
	cfg := map[string]any{}
	if v := envValue(env, opencodeConfigEnv); v != "" {
		if json.Unmarshal([]byte(v), &cfg) != nil {
			cfg = map[string]any{}
		}
	}
	agents, _ := cfg["agent"].(map[string]any)
	if agents == nil {
		agents = map[string]any{}
	}
	agents[opencodeAgent] = map[string]any{"mode": "primary", "permission": opencodePermission(t)}
	cfg["agent"] = agents
	b, _ := json.Marshal(cfg)
	return []string{opencodeConfigEnv + "=" + string(b)}
}

// opencodePermission is the permission of a turn's agent, in OpenCode's
// config shape.
//
//   - bash: "*" deny, then "<command> *" allow for each command of allow. A
//     pattern ending " *" also matches the command with no arguments
//     (util/wildcard.ts). OpenCode splits a command line into its simple
//     commands and asks for each (tool/shell.ts), so every one of them must
//     be allowed. A rule with a wildcard or a config substitution in it is
//     left out (ruleSafe).
//   - edit: allow with an edit rule (Writes.Edit), else deny. It covers
//     OpenCode's write, edit and apply_patch tools. Outside its folder
//     OpenCode asks (external_directory), which opencode run refuses.
//   - task: deny when subagents are off.
func opencodePermission(t Turn) map[string]any {
	bash := map[string]any{"*": "deny"}
	for _, c := range t.Allow {
		if c = strings.TrimSpace(c); c == "" || strings.HasPrefix(c, "tool:") || strings.HasPrefix(c, "api:") || !ruleSafe(c) {
			continue
		}
		bash[c+" *"] = "allow"
	}
	edit := "deny"
	if len(t.Writes.Edit) > 0 {
		edit = "allow"
	}
	p := map[string]any{"bash": orderedRules(bash), "edit": edit}
	if t.NoSubagents {
		p["task"] = "deny"
	}
	return p
}

// orderedRules keeps "*" first when the rules are encoded: OpenCode reads a
// permission's patterns in the order its JSON names them, and the last that
// matches wins, so the catch-all must come before the allows. encoding/json
// sorts a map's keys, and "*" sorts before any letter, but not before every
// character a command may start with, so the order is made explicit.
type orderedRules map[string]any

func (r orderedRules) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString("{")
	write := func(k string, v any) {
		if b.Len() > 1 {
			b.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(v)
		b.Write(kb)
		b.WriteString(":")
		b.Write(vb)
	}
	if v, ok := r["*"]; ok {
		write("*", v)
	}
	keys := make([]string, 0, len(r))
	for k := range r {
		if k != "*" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		write(k, r[k])
	}
	b.WriteString("}")
	return []byte(b.String()), nil
}

// Refuse: the gateway's guard does not read the Chat Completions format, so
// forbid and ask could only be OpenCode's own rules, which a command in
// another shape passes, and opencode run has no person to ask. Tool and API
// rules are checked elsewhere (control and tool gate).
func (opencode) Refuse(t Turn) string {
	hasShellRules := func(list []string) bool {
		for _, r := range list {
			f := strings.Fields(r)
			if len(f) > 0 && !strings.HasPrefix(f[0], "tool:") && !strings.HasPrefix(f[0], "api:") {
				return true
			}
		}
		return false
	}
	if hasShellRules(t.Forbid) {
		return "forbid in permissions.toml is not enforced for opencode yet; remove it, or use claude or codex"
	}
	if hasShellRules(t.Ask) {
		return "ask in permissions.toml is not enforced for opencode yet; remove it, or use claude or codex"
	}
	return ""
}

// Edit: OpenCode lets its edit tools change any file of its project, which is
// its folder or, in a git repository, the repository's root (project/
// instance-context.ts, containsPath): a folder below a repository's root
// would let a turn edit the rest of the repository, so it runs without edits.
// Its permission rules hold no absolute path, so a protected file inside the
// folder cannot be kept read only either.
func (opencode) Edit(folders, protect []string) ([]string, string) {
	for _, f := range folders {
		for _, p := range protect {
			if p == f || strings.HasPrefix(p, strings.TrimSuffix(f, "/")+"/") {
				return nil, "data_folder_inside"
			}
		}
		if gitAbove(f) {
			return nil, "opencode_repository_above"
		}
	}
	return folders, ""
}

// gitAbove says a folder above dir, and not dir itself, holds a .git: dir is
// inside a git repository whose root is higher up.
func gitAbove(dir string) bool {
	dir = filepath.Clean(dir)
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// OwnInstructions: OpenCode has no flag or setting that adds to its system
// prompt for one run: an agent's prompt replaces OpenCode's own, and its
// instructions setting takes files. What it reads by itself is the AGENTS.md
// of its folder (Native). A turn's hint is left out.
func (opencode) OwnInstructions([]string, string) string { return "opencode_no_instruction_flag" }

// OwnTools is never asked: OpenCode takes no tool server yet
// (Info.ToolServer).
func (opencode) OwnTools([]string, string) string { return "" }

// Native: OpenCode reads the first of AGENTS.md, CLAUDE.md and CONTEXT.md it
// finds from its folder up (session/instruction.ts); AGENTS.md comes first.
func (opencode) Native(_ *os.Root, name string) bool { return name == "AGENTS.md" }

// opencodeSessionShape is an OpenCode session id: "ses_", 12 hex digits of
// time and 14 base62 characters (id/id.ts).
var opencodeSessionShape = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// ResumeID is the sessionID of the lines --format json prints: every one
// carries it, with its type (cli/cmd/run.ts, emit).
func (opencode) ResumeID(line []byte, current string) string {
	if current != "" {
		return current
	}
	var v struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
	}
	if json.Unmarshal(line, &v) == nil && v.Type != "" && opencodeSessionShape.MatchString(v.SessionID) {
		return v.SessionID
	}
	return current
}

// Args: opencode run, its events as JSON lines, as the run's own agent
// (TurnEnv). The model is the run's config's (WireOpenRouter). The prompt
// comes after "--", so it is never read as a flag.
func (opencode) Args(t Turn) []string {
	a := []string{"run", "--format", "json", "--agent", opencodeAgent}
	if t.ResumeID != "" {
		a = append(a, "--session", t.ResumeID)
	}
	return append(a, "--", t.Prompt)
}
