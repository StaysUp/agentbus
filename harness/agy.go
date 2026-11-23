package harness

import (
	"encoding/json"
	"os"
	"strings"
)

// agy is the Antigravity CLI, Google's agent harness for Gemini.
//
// Everything here was measured on agy 1.5.5 on 2026-11-11, signed in with a
// Google account, unless it says otherwise.
type agy struct{}

func (agy) Info() Info {
	return Info{
		Name:    "agy",
		Label:   "Antigravity",
		Product: "the Antigravity CLI (agy)",
		// agy has no offline sign-in status command, so a runner reports
		// it found but says nothing of its sign-in.
		StatusArgs: nil,
		// Where agy sends its Code Assist calls when the person points it
		// elsewhere (Wire).
		OwnEndpointEnv: "CLOUD_CODE_URL",
		// Only its own Google sign-in, on this machine or relayed from
		// control (Login), or a Gemini API key set in Settings (APIKey).
		// It speaks the Gemini format only, so OpenRouter cannot serve it.
		Access: []Access{
			{Kind: Subscription, Label: "Google sign-in", Models: []string{"gemini-3.8-flash-medium", "gemini-4-argon-medium"}, AnyModel: true},
			{Kind: OpenRouter, Label: "OpenRouter key", Disabled: true, Models: []string{}},
			{Kind: API, Label: "Gemini API key", Models: []string{}, AnyModel: true},
		},
		// The Gemini API takes a key in x-goog-api-key
		// (ai.google.dev/api, read 2026-11-13), and agy sends it there
		// (WireAPI).
		APIKey: &APIKey{Name: "gemini", Label: "Gemini API key", URL: "https://generativelanguage.googleapis.com", Header: "X-Goog-Api-Key", Format: FormatGemini},
		// agy keeps its settings, conversations and sign-in under
		// $HOME/.gemini; no variable moves that folder. A home of its own
		// starts signed out (agy then prints a login link and waits).
		ConfigDir: ".gemini",
		// agy acp --login, with HOME set to a folder of control's own and a
		// D-Bus address that reaches no bus, prints a link and reads the pasted code from
		// its terminal. It keeps the sign-in in a plain JSON file in that
		// home (0600): token.access_token, token.refresh_token and
		// token.expiry, an access token of one hour (agy 1.5.5, 2026-11-11,
		// his own sign-in in a scratch VM). Without a bus it skips the
		// keyring (changelog 1.1.3). Control keeps that home as it is, and
		// agy renews its own file there: agy models renews it when less than
		// five minutes are left, and runs no model turn (measured in a
		// scratch VM with a dummy refresh token: at four minutes left it
		// asked oauth2.googleapis.com, at six it did not). A refused renewal
		// leaves the file as it was and prints "Please sign in to view
		// available models".
		//
		// AgentBus never reads or uses agy's client secret: only agy renews.
		Login: &Login{Args: []string{"acp", "--login"},
			Home: "HOME", File: agyTokenFile, Keep: "agy-home",
			TokenKey: "token.access_token", ExpiryKey: "token.expiry",
			RenewArgs: []string{"models"}, RenewWithinMinutes: 4,
			// agy models prints one "id<TAB>label" a line.
			ModelsArgs: []string{"models"}, RenewRefused: `(?i)please sign in`,
			// With no address set, agy still found this user's bus at
			// /run/user/<uid>/bus and asked it for a keyring; "disabled:" is
			// an address agy cannot use, so it keeps the sign-in in its file
			// whatever the machine runs (agy 1.5.5, 2026-11-11, from its log:
			// "invalid bus address ... falling back to file").
			Set: []string{"DBUS_SESSION_BUS_ADDRESS=disabled:"},
			Unset: []string{"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
				"XDG_STATE_HOME", "XDG_CACHE_HOME", "DISPLAY", "WAYLAND_DISPLAY", "CLOUD_CODE_URL"},
			OnlyOn: "linux", OnlyOnWhy: "agy keeps its sign-in in a file only on Linux with no D-Bus bus to reach; " +
				"on macOS its docs say it uses the Keychain, which control never reads. Run control on Linux to connect agy"},
		// With a Google sign-in on a runner every turn ends with this
		// line: agy calls Google's account service itself, outside
		// AgentBus (issue #40, 2026-11-21). The whole line, not its lead:
		// without loadCodeAssist agy stops with "Eligibility check failed"
		// too (adapter/gemini).
		Failures: []Failure{{
			Match: "Eligibility check failed: failed to fetch user info: 401",
			Say:   "Antigravity's Google sign-in can't run sessions yet. Add a Gemini API key in Settings, Subscriptions.",
		}},
	}
}

// agyTokenFile is where agy keeps its Google sign-in, under its HOME.
const agyTokenFile = ".gemini/antigravity-cli/antigravity-oauth-token"

// Wire: agy sends every Code Assist call, model calls included, to
// CLOUD_CODE_URL when it is set, else to https://daily-cloudcode-pa.googleapis.com.
// GOOGLE_GEMINI_BASE_URL is for a Gemini API key only (agy 1.5.0,
// 2026-11-04). The service routes /v1internal: to that host (cmd/agentbus).
func (agy) Wire(baseURL string) (env, args []string) {
	return []string{"CLOUD_CODE_URL=" + baseURL}, nil
}

// WireAPI: the Gemini API at the service, with the placeholder as
// GEMINI_API_KEY; the turn's --settings names the gemini provider
// (agySettings). agy 1.5.8 so set, with a scratch HOME and no sign-in, sent
// POST $GOOGLE_GEMINI_BASE_URL/v1beta/models/<model>:streamGenerateContent?alt=sse
// with the key in X-Goog-Api-Key and nothing in Authorization, and asked no
// sign-in (2026-11-13, against a local listener, with a fake key). Control's
// relay swaps the key set in Settings in for the placeholder, so the child
// never holds it, and no Keychain or login is involved.
func (agy) WireAPI(w APIWire) (env, args []string) {
	return []string{"GOOGLE_GEMINI_BASE_URL=" + w.Base, "GEMINI_API_KEY=" + w.Placeholder}, nil
}

// WireKeyModels: agy on a Gemini API key takes as --model only the models
// it knows, not the Gemini API's catalogue: "gemini-2.5-flash-preview-tts",
// from that catalogue, was refused with "invalid model selection ... is not
// recognized as a known model or custom model in settings" and its own list,
// before any call (agy 1.5.9, 2026-11-14, his own run). agy models, with
// the gemini provider in --settings (before the subcommand: "models
// --settings" is refused) and any GEMINI_API_KEY, prints that list as
// "slug<TAB>label" lines, such as gemini-3.8-flash-low<TAB>Gemini 3.8 Flash
// (Low), with no sign-in and no call: a local listener as
// GOOGLE_GEMINI_BASE_URL got nothing, and a turn so set took both the slug
// and the label as --model (agy 1.5.9, 2026-11-14, a scratch HOME and a
// placeholder key). With no GEMINI_API_KEY it lists nothing. The base URL
// is a loopback port nothing listens on, so a later agy that did ask the
// API could send the placeholder nowhere.
func (agy) WireKeyModels(placeholder string) (env, args []string) {
	return []string{"GEMINI_API_KEY=" + placeholder, "GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:9", "DBUS_SESSION_BUS_ADDRESS=disabled:"},
		[]string{"--settings", `{"modelProvider":"gemini"}`, "models"}
}

// WireOpenRouter is never used: agy offers no OpenRouter access (Info).
func (agy) WireOpenRouter(OpenRouterWire) (env, args []string) { return nil, nil }

// WireRelay: as Wire. The placeholder is in the home's sign-in file
// (RelayHome), and agy sends it as the bearer of every Code Assist call,
// which control's relay replaces with the person's own sign-in.
func (a agy) WireRelay(baseURL, _ string) (env, args []string) { return a.Wire(baseURL) }

// RelayHome: a session's home starts signed out, and agy then waits for a
// login. This file makes it start signed in on the placeholder: an access
// token, no refresh token, so agy never tries to renew it, and an expiry far
// off. auth_method "consumer" is agy's own Google sign-in; any other value
// was refused ("Unknown auth method"), and without it agy asked for a login.
// id_token_email spares a lookup of the account (agy 1.5.5, 2026-11-11, in a
// scratch VM against a local listener: every Code Assist call then carried
// the placeholder as its bearer).
func (agy) RelayHome(placeholder string) map[string][]byte {
	b, _ := json.Marshal(map[string]any{
		"token":          map[string]string{"access_token": placeholder, "token_type": "Bearer", "expiry": "2099-01-01T00:00:00Z"},
		"auth_method":    "consumer",
		"id_token_email": "agentbus@runner.invalid",
	})
	return map[string][]byte{agyTokenFile: b}
}

// Edit: agy's permission rules hold paths, as Claude Code's do; a name that
// cannot go in one gets no edit rule.
// Temporary glue: mapping tool:edit to agy's edit rules.
func (agy) Edit(folders, protect []string) ([]string, string) {
	for _, f := range folders {
		if !ruleSafe(f) {
			return nil, "folder_shape"
		}
	}
	for _, p := range protect {
		if !ruleSafe(p) {
			return nil, "protected_file_shape"
		}
	}
	return folders, ""
}

// OwnInstructions: agy has no flag or setting that adds to its system prompt
// for one run. What it reads as instructions are rule files: the AGENTS.md
// and GEMINI.md of its folder (Native) and of ~/.gemini. Writing either would
// change the person's own files, so a turn's hint is left out.
func (agy) OwnInstructions([]string, string) string { return "agy_no_instruction_flag" }

// OwnTools is never asked: agy takes no tool server yet (Info.ToolServer).
func (agy) OwnTools([]string, string) string { return "" }

// Native: agy reads the AGENTS.md and the GEMINI.md of its working folder,
// git repository or not, into its system prompt as user rules (checked live
// with a marker in each, read back from the request).
func (agy) Native(_ *os.Root, name string) bool { return name == "AGENTS.md" || name == "GEMINI.md" }

// ResumeID is the conversation_id of the one JSON object --output-format json
// prints when the turn ends, with its status.
func (agy) ResumeID(line []byte, current string) string {
	if v, ok := decodeLine(line); ok && v.Status != "" && resumeIDShape.MatchString(v.ConversationID) {
		return v.ConversationID
	}
	return current
}

// Refuse: the gateway's guard does not read the Gemini format, so forbid
// could only be agy's own prefix rule, which a command in another shape
// passes. Tool and API rules are checked elsewhere (control and tool gate).
func (agy) Refuse(t Turn) string {
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
		return "forbid in permissions.toml is not enforced for agy yet; remove it, or use " + "claude or codex"
	}
	if hasShellRules(t.Ask) {
		return "ask in permissions.toml is not enforced for agy yet; remove it, or use " + "claude or codex"
	}
	return ""
}

// AgySubagentsUnkept is what a turn with subagents off records on agy.
// Temporary glue: mapping tool:subagent to agy subagents recording.
const AgySubagentsUnkept = "agy cannot turn its own subagents off, so subagents = false does not apply to this turn"

// Unkept: agy has no switch that turns its subagents off (a deny rule on
// invoke_subagent did not stop one). With subagents off by default (0.5.2),
// refusing would refuse every agy turn, so the turn runs and records that
// the setting was not kept (his call, 2026-11-13).
func (agy) Unkept(t Turn) []string {
	if t.NoSubagents {
		return []string{AgySubagentsUnkept}
	}
	return nil
}

func (agy) Args(t Turn) []string {
	a := []string{"--output-format", "json", "--settings", agySettings(t)}
	if t.ResumeID != "" {
		a = append(a, "--conversation", t.ResumeID)
	}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	// agy takes no argument after its flags (a stray one is an error), so
	// the prompt is --print's value, joined with "=": it can never be read
	// as a flag, and -p alone would take the next flag as its prompt.
	return append(a, "--print="+t.Prompt)
}

// agySettings is the --settings value of a turn: settings.json keys for this
// run only, never saved. Its "permissions" replaces the person's own for the
// run (checked live: with an empty allow list, a command their settings
// allow was refused), so the turn gets exactly these rules. Headless agy
// refuses any tool call no rule allows, and a deny rule wins over an allow.
//
//   - read_file(<folder>), write_file(<folder>), edit_file(<folder>): that
//     folder and everything under it, and not a folder whose name only
//     starts the same (checked: <folder>2 was refused).
//   - command(<cmd>): the command and any arguments after it.
//   - write_file(*), edit_file(*), command(*): every one.
//
// allowNonWorkspaceAccess false: outside its folder, agy reads only what a
// read_file rule allows.
func agySettings(t Turn) string {
	allow := []string{}
	var deny []string
	// Every turn may call the MCP servers the person copied into the
	// session's home (Home files). Read-only turns retired in 0.6 (B12): a
	// review runs like any turn, so it gets the mcp rule too.
	allow = append(allow, "mcp(*)")
	for _, f := range t.Writes.Edit {
		allow = append(allow, "write_file("+f+")", "edit_file("+f+")")
	}
	for _, c := range t.Allow {
		if c == "*" {
			allow = append(allow, "command(*)") // every command; the gateway still gates forbid and ask
			continue
		}
		if !strings.HasPrefix(c, "tool:") && !strings.HasPrefix(c, "api:") && ruleSafe(c) {
			allow = append(allow, "command("+c+")")
		}
	}
	for _, p := range t.Writes.Protect {
		deny = append(deny, "write_file("+p+")", "edit_file("+p+")")
	}
	// A turn with no edit rule (edit = "none") may still not write its folder.
	if len(t.Writes.Edit) == 0 {
		deny = append(deny, "write_file(*)", "edit_file(*)")
	}
	perm := map[string][]string{"allow": allow}
	if len(deny) > 0 {
		perm["deny"] = deny
	}
	v := map[string]any{"allowNonWorkspaceAccess": false, "permissions": perm}
	if t.Access == API {
		// The Gemini API, with the key in GEMINI_API_KEY (WireAPI):
		// "Set modelProvider: \"gemini\" in settings.json, export
		// GEMINI_API_KEY" (agy's changelog). Set for this run only, so the
		// person's own settings never change.
		v["modelProvider"] = "gemini"
	}
	b, _ := json.Marshal(v)
	return string(b)
}
