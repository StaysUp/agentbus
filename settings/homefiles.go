package settings

// The usual config files a person may give each session (home_files), and
// the vendor sign-in files that never go. Vendor sign-ins live in control
// (his rule, 2026-11-14): a session never gets one through home_files, by
// name or inside a folder it lists.

import (
	"path/filepath"
	"strings"
)

// UsualHomeFiles are the config paths "Use my config" looks for on the
// folder's machine (GET /api/v0/folders/{ref}/home-files), in this order. A
// path ending in / is a folder. ~/.claude.json gives its MCP servers only
// (homefiles.ClaudeJSON). agy keeps its MCP servers in
// ~/.gemini/config/mcp_config.json (agy 1.5.5, 2026-11-14, read on this
// machine; harness/agy.go keeps the rest of agy in ~/.gemini).
var UsualHomeFiles = []string{
	"~/.claude/settings.json",
	"~/.claude/CLAUDE.md",
	"~/.claude/skills/",
	"~/.claude/commands/",
	"~/.claude/agents/",
	"~/.claude.json",
	"~/.codex/config.toml",
	"~/.codex/AGENTS.md",
	"~/.gemini/config/mcp_config.json",
	"~/.aws/config",
	"~/.aws/credentials",
	"~/.config/gh/",
	"~/.gitconfig",
	"~/.ssh/config",
}

// signInFiles are the vendors' own sign-in files, under the home, and what
// each is, for a refusal. claude keeps its sign-in in .credentials.json on
// Linux (the Keychain on macOS), and .claude/.claude.json is its account
// state under CLAUDE_CONFIG_DIR; codex in auth.json (harness/codex.go); agy
// in its token file (harness/agy.go, agyTokenFile); the Gemini CLI in
// oauth_creds.json.
var signInFiles = map[string]string{
	".claude/.credentials.json":                       "Claude Code's sign-in",
	".claude/.claude.json":                            "Claude Code's account file",
	".codex/auth.json":                                "codex's sign-in",
	".gemini/antigravity-cli/antigravity-oauth-token": "agy's sign-in",
	".gemini/oauth_creds.json":                        "the Gemini CLI's sign-in",
}

// SignInFile says what vendor sign-in path is, when it is one: a path under
// the home written ~/x, or x alone. "" when it is not one. A folder that
// holds one is not one: its sign-in is left out when it is copied.
func SignInFile(path string) string {
	rel := strings.TrimPrefix(path, "~/")
	return signInFiles[filepath.Clean(rel)]
}
