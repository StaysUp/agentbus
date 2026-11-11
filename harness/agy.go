package harness

import (
	"encoding/json"
	"os"
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
		// Only its own Google sign-in. It speaks the Gemini format only, so
		// OpenRouter cannot serve it, and a Gemini API key is not wired yet.
		Access: []Access{
			{Kind: Subscription, Label: "Google sign-in", Models: []string{"gemini-3.8-flash-medium", "gemini-4-argon-medium"}},
			{Kind: OpenRouter, Label: "OpenRouter key", Disabled: true, Models: []string{}},
			{Kind: API, Label: "API key", Disabled: true, Models: []string{}},
		},
		// agy keeps its settings, conversations and sign-in under
		// $HOME/.gemini; no variable moves that folder. A home of its own
		// starts signed out (agy then prints a login link and waits).
		ConfigDir: ".gemini",
	}
}

// Wire: agy sends every Code Assist call, model calls included, to
// CLOUD_CODE_URL when it is set, else to https://daily-cloudcode-pa.googleapis.com.
// GOOGLE_GEMINI_BASE_URL is for a Gemini API key only (agy 1.5.0,
// 2026-11-04). The service routes /v1internal: to that host (cmd/agentbus).
func (agy) Wire(baseURL string) (env, args []string) {
	return []string{"CLOUD_CODE_URL=" + baseURL}, nil
}

// WireOpenRouter is never used: agy offers no OpenRouter access (Info).
func (agy) WireOpenRouter(OpenRouterWire) (env, args []string) { return nil, nil }

// WireRelay is never used: agy has no Login.
func (agy) WireRelay(string, string) (env, args []string) { return nil, nil }

// Edit: agy's permission rules hold paths, as Claude Code's do; a name that
// cannot go in one gets no edit rule.
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

// Refuse: agy has no switch that turns its subagents off (a deny rule on
// invoke_subagent did not stop one), and the gateway's guard does not read
// the Gemini format, so forbid could only be agy's own prefix rule, which a
// command in another shape passes.
func (agy) Refuse(t Turn) string {
	if t.NoSubagents {
		return "agy cannot turn its subagents off; set subagents = true in permissions.toml, or use " + "claude or codex"
	}
	if len(t.Forbid) > 0 {
		return "forbid in permissions.toml is not enforced for agy yet; remove it, or use " + "claude or codex"
	}
	return ""
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
	for _, f := range t.ReadFolders {
		if ruleSafe(f) {
			allow = append(allow, "read_file("+f+")")
		}
	}
	write := !t.Writes.ReadOnly
	if write {
		for _, f := range t.Writes.Edit {
			allow = append(allow, "write_file("+f+")", "edit_file("+f+")")
		}
		for _, c := range t.Allow {
			if ruleSafe(c) {
				allow = append(allow, "command("+c+")")
			}
		}
		for _, p := range t.Writes.Protect {
			deny = append(deny, "write_file("+p+")", "edit_file("+p+")")
		}
	}
	if !write || len(t.Writes.Edit) == 0 {
		deny = append(deny, "write_file(*)", "edit_file(*)")
	}
	if !write {
		// A read-only turn runs no command, as a read-only claude turn
		// has no shell.
		deny = append(deny, "command(*)")
	}
	perm := map[string][]string{"allow": allow}
	if len(deny) > 0 {
		perm["deny"] = deny
	}
	b, _ := json.Marshal(map[string]any{"allowNonWorkspaceAccess": false, "permissions": perm})
	return string(b)
}
