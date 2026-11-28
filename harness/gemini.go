package harness

import (
	"os"
	"regexp"
	"strings"
)

// gemini is the Gemini CLI (@google/gemini-cli), Google's open-source agent
// harness for Gemini.
//
// Everything here was read in the Gemini CLI's source at v0.70.0 (the npm
// package's bundle, packages/cli and packages/core) and checked live with
// @google/gemini-cli 0.70.0 against a local listener, unless it says
// otherwise.
type gemini struct{}

func (gemini) Info() Info {
	return Info{
		Name:    "gemini",
		Label:   "Gemini CLI",
		Product: "the Gemini CLI (npm install -g @google/gemini-cli)",
		// The Gemini CLI has no offline sign-in status command.
		StatusArgs: nil,
		// Where the Gemini CLI sends its Gemini API calls when the person
		// points it elsewhere (WireAPI).
		OwnEndpointEnv: geminiBaseURLEnv,
		// A Gemini API key set in Settings (APIKey), recorded by
		// adapter/gemini as agy's is. Its own Google sign-in reaches the
		// service by CODE_ASSIST_ENDPOINT (Wire), but control cannot connect
		// that sign-in yet (no Login), and every turn runs relayed, so it is
		// listed, disabled. It speaks the Gemini format only, so OpenRouter
		// cannot serve it.
		Access: []Access{
			{Kind: Subscription, Label: "Google sign-in", Disabled: true, Models: []string{}},
			{Kind: OpenRouter, Label: "OpenRouter key", Disabled: true, Models: []string{}},
			{Kind: API, Label: "Gemini API key", Models: []string{}, AnyModel: true},
		},
		// The same key as agy's: the Gemini API takes it in x-goog-api-key,
		// and the Gemini CLI sends it there (WireAPI).
		APIKey: &APIKey{Name: "gemini", Label: "Gemini API key", URL: "https://generativelanguage.googleapis.com", Header: "X-Goog-Api-Key", Format: FormatGemini},
		// No own replay reads the Gemini format: a next turn continues the
		// session by the Gemini CLI's own --resume.
		Replay: "",
		// The Gemini CLI keeps its settings, sessions and sign-in under
		// $HOME/.gemini (GEMINI_DIR). GEMINI_CLI_HOME moves the home that
		// folder is in, not the folder, so it is not a ConfigEnv.
		ConfigDir: ".gemini",
	}
}

// geminiBaseURLEnv is where the Gemini CLI sends its Gemini API calls, and
// geminiSettingsFile its user settings, under its HOME.
const (
	geminiBaseURLEnv   = "GOOGLE_GEMINI_BASE_URL"
	geminiSettingsFile = ".gemini/settings.json"
)

// Wire: on its own Google sign-in the Gemini CLI sends every Code Assist
// call, model calls included, to CODE_ASSIST_ENDPOINT when it is set, else to
// https://cloudcode-pa.googleapis.com (code_assist/server.ts), each under
// /v1internal: (loadCodeAssist, retrieveUserQuota, listExperiments, then
// streamGenerateContent?alt=sse). GOOGLE_GENAI_USE_GCA=true selects that
// sign-in when no setting selects one (contentGenerator.ts,
// getAuthTypeFromEnv). Never used yet: the access is disabled (Info).
func (gemini) Wire(baseURL string) (env, args []string) {
	return []string{"CODE_ASSIST_ENDPOINT=" + baseURL, "GOOGLE_GENAI_USE_GCA=true"}, nil
}

// WireRelay: as Wire, with the placeholder as GOOGLE_CLOUD_ACCESS_TOKEN: with
// GOOGLE_GENAI_USE_GCA set, the Gemini CLI takes that token as its sign-in
// and asks for no login and no sign-in file (code_assist/oauth2.ts). Checked
// live: every Code Assist call carried it as its bearer. Never used yet: the
// Gemini CLI has no Login, so control has no sign-in to swap in.
func (g gemini) WireRelay(baseURL, placeholder string) (env, args []string) {
	env, args = g.Wire(baseURL)
	return append(env, "GOOGLE_CLOUD_ACCESS_TOKEN="+placeholder), args
}

// WireAPI: the Gemini API at the service, with the placeholder as
// GEMINI_API_KEY. So set, the Gemini CLI sent POST
// $GOOGLE_GEMINI_BASE_URL/v1beta/models/<model>:streamGenerateContent?alt=sse
// with the key in x-goog-api-key, the call agy makes on a key, and nothing
// else. GOOGLE_GEMINI_BASE_URL alone selects its "gateway" sign-in, which a
// headless run refuses ("Invalid auth method selected.", config/auth.ts), so
// the turn's home pins the key's sign-in (APIHome). Control's relay swaps the
// key set in Settings in for the placeholder, so the child never holds it.
func (gemini) WireAPI(w APIWire) (env, args []string) {
	return []string{geminiBaseURLEnv + "=" + w.Base, "GEMINI_API_KEY=" + w.Placeholder}, nil
}

// APIHome: the user settings of the turn's home select the Gemini API key as
// the sign-in. A selected sign-in wins over the variables
// (validateNonInteractiveAuth), and with it the Gemini CLI still takes its
// base URL from GOOGLE_GEMINI_BASE_URL (contentGenerator.ts). Without a home
// of its own the turn is refused, so the person's own settings are never
// written.
func (gemini) APIHome() map[string][]byte {
	return map[string][]byte{geminiSettingsFile: []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}` + "\n")}
}

// WireOpenRouter is never used: the Gemini CLI offers no OpenRouter access
// (Info).
func (gemini) WireOpenRouter(OpenRouterWire) (env, args []string) { return nil, nil }

// Edit: the Gemini CLI's edit tools change any file of its workspace, which
// is its working folder (config.ts, targetDir), and its rules hold no path,
// so a protected file inside the folder cannot be kept read only: such a
// folder runs without edits.
func (gemini) Edit(folders, protect []string) ([]string, string) {
	for _, f := range folders {
		for _, p := range protect {
			if p == f || strings.HasPrefix(p, strings.TrimSuffix(f, "/")+"/") {
				return nil, "data_folder_inside"
			}
		}
	}
	return folders, ""
}

// OwnInstructions: the Gemini CLI has no flag or setting that adds to its
// system prompt for one run: GEMINI_SYSTEM_MD replaces it. What it reads as
// instructions is the GEMINI.md of its folder (Native) and of ~/.gemini. A
// turn's hint is left out.
func (gemini) OwnInstructions([]string, string) string { return "gemini_no_instruction_flag" }

// OwnTools is never asked: the Gemini CLI takes no tool server yet
// (Info.ToolServer).
func (gemini) OwnTools([]string, string) string { return "" }

// Native: the Gemini CLI reads the GEMINI.md of its folder
// (DEFAULT_CONTEXT_FILENAME); AGENTS.md only when the person's context.fileName
// names it.
func (gemini) Native(_ *os.Root, name string) bool { return name == "GEMINI.md" }

// geminiSessionShape is a Gemini CLI session id, a random UUID
// (createSessionId). --resume reads a number as a place in its list, so only
// this shape is taken.
var geminiSessionShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ResumeID is the session_id of the init line --output-format stream-json
// prints first, with the model (nonInteractiveCli.ts). --output-format json
// prints its object over many lines, which a line cannot be read from.
func (gemini) ResumeID(line []byte, current string) string {
	if v, ok := decodeLine(line); ok && v.Type == "init" && geminiSessionShape.MatchString(v.SessionID) {
		return v.SessionID
	}
	return current
}

// Refuse: the gateway's guard does not read the Gemini format, so forbid and
// ask could only be the Gemini CLI's own rules, which a command in another
// shape passes, and a headless run has no person to ask. Tool and API rules
// are checked elsewhere (control and tool gate).
func (gemini) Refuse(t Turn) string {
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
		return "forbid in permissions.toml is not enforced for gemini yet; remove it, or use claude or codex"
	}
	if hasShellRules(t.Ask) {
		return "ask in permissions.toml is not enforced for gemini yet; remove it, or use claude or codex"
	}
	return ""
}

// GeminiSubagentsUnkept is what a turn with subagents off records on the
// Gemini CLI.
const GeminiSubagentsUnkept = "the Gemini CLI cannot turn its own subagents off from its command line, so subagents = false does not apply to this turn"

// ShellWall: the Gemini CLI is started without its own sandbox (no --sandbox,
// and Info.CopySandbox is false), and the runner's whole-process wall is
// retired (0.6.4), so its shell runs unwalled in a copy and in a direct folder
// alike, held by the gateway's command rules only.
func (gemini) ShellWall(Turn) string { return "" }

// Unkept: the Gemini CLI's default policy allows its invoke_agent tool
// (policies/agents.toml), and no flag denies a tool. As on agy, the turn runs
// and records that the setting was not kept.
func (gemini) Unkept(t Turn) []string {
	if t.NoSubagents {
		return []string{GeminiSubagentsUnkept}
	}
	return nil
}

// Args: a headless run (--prompt), its events as JSON lines. A headless run
// refuses a folder it does not trust, and --skip-trust trusts the turn's
// folder for the run only (it sets GEMINI_CLI_TRUST_WORKSPACE), so nothing is
// saved.
//
// Its default policy in a headless run refuses the tools that write or run a
// command (policies/write.toml). --approval-mode auto_edit lets its edit
// tools change files of its workspace without asking, and only with an edit
// rule; never yolo. Each command of allow is --allowed-tools
// run_shell_command(<cmd>): the command and any arguments after it, every
// other command still refused (policy-engine's narrowing deny). A rule with a
// comma, which the flag splits on, or a parenthesis is left out (ruleSafe).
//
// The prompt is --prompt's value, joined with "=": -p takes the next flag as
// its value no more than agy's does, and the prompt is never read as a flag.
func (gemini) Args(t Turn) []string {
	a := []string{"--output-format", "stream-json", "--skip-trust"}
	mode := "default"
	if len(t.Writes.Edit) > 0 {
		mode = "auto_edit"
	}
	a = append(a, "--approval-mode", mode)
	for _, c := range t.Allow {
		if c = strings.TrimSpace(c); c == "" || strings.HasPrefix(c, "tool:") || strings.HasPrefix(c, "api:") || !ruleSafe(c) {
			continue
		}
		a = append(a, "--allowed-tools", "run_shell_command("+c+")")
	}
	if t.ResumeID != "" {
		a = append(a, "--resume", t.ResumeID)
	}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	return append(a, "--prompt="+t.Prompt)
}
