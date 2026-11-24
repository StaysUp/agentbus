package gateway

// Copy containment at the gateway. A turn that runs in a session's copy
// (DESIGN, 2026-11-22) may write only inside that copy. The per-command OS
// sandbox fences writes at the operating-system level (harness.Copy); this is
// its companion in the reply stream — it refuses a structured file-edit tool
// call whose target path lies outside the copy, so the model reads a clean,
// named refusal in place of an opaque "operation not permitted", and so a
// harness whose OS sandbox is not wired is still held to the boundary.
//
// It reads the path from the tool call's arguments, the way the command guard
// reads a shell command (commandsOf). Like that guard it reads text only, and
// the check is LEXICAL: filepath.Clean and filepath.Rel, no filesystem access,
// because the gateway is a control package and control never touches a folder
// (cmd/agentbus/layers_test.go). The copy's symlinked names are resolved once
// on the runner side (run.go, appendReal) and both are in the write-set, so the
// lexical check accepts either.
//
// This matters most because Claude Code's --settings sandbox (srt) walls only
// its Bash tool; Write, Edit and NotebookEdit run in-process, outside srt
// (measured, 2.1.330). What keeps those inside the copy is Claude Code's own
// Edit(//<copy>/**) permission from --settings — until a user's or a cloned
// repo's settings.json widens it, and then this gateway check is the wall. A
// symlink inside the copy that points out is still Claude Code's own permission
// to catch (a realpath it does not have here); the gateway draws the same
// text-only boundary the command guard draws for a command built at run time.
//
// Only Claude Code (Anthropic Messages) is covered. Its file writes are
// structured tool calls with a path field (Edit/Write/MultiEdit/NotebookEdit).
// codex writes through the shell, so it has no path field and relies on the
// sandbox (its exec is covered by the command guard). opencode speaks Chat
// Completions, which the guard does not read at all (harness/opencode.go), so
// its copy containment is the OS sandbox and its own edit permission. The write
// here is keyed by vendor so a harness whose format the guard learns to read
// slots in without a second rule.

import (
	"path/filepath"
	"strconv"
	"strings"
)

// copyRefusalText is what the model reads in place of a file-edit call refused
// for writing outside the turn's copy.
func copyRefusalText(path string) string {
	return "Refused by the AgentBus gateway: " + strconv.Quote(path) +
		" is outside this turn's copy, the only place it may write. The edit did not run."
}

// fileEditPaths returns the target paths of one structured file-edit tool call,
// by vendor and tool. A call that writes no file through a path argument (a
// shell command, a read) gives none, so it is never refused on a path.
func fileEditPaths(vendor string, item map[string]any) []string {
	// Only the Anthropic Messages format carries Claude Code's file-write tools
	// as structured calls with a path field. codex (OpenAI Responses) edits
	// through the shell; opencode (Chat Completions) is not read by the guard.
	if vendor != "anthropic" {
		return nil
	}
	in, _ := item["input"].(map[string]any)
	if in == nil {
		return nil
	}
	name, _ := item["name"].(string)
	// Claude Code 2.1.x write tools and their path field (VERIFY_copy_level,
	// 2026-11-22): Edit, Write and MultiEdit carry file_path, NotebookEdit
	// carries notebook_path. Read is a read — Copy leaves reads open across the
	// disk — so it is not listed here.
	var key string
	switch name {
	case "Edit", "Write", "MultiEdit":
		key = "file_path"
	case "NotebookEdit":
		key = "notebook_path"
	default:
		return nil
	}
	if p, ok := in[key].(string); ok && p != "" {
		return []string{p}
	}
	return nil
}

// outsideCopy says whether p, a file-edit target, lies outside the write-set:
// the copy directory and any extra writable paths, exactly the paths the
// per-command sandbox allows (harness.Copy Dir plus Write). cwd, the turn's
// folder, resolves a relative path. The check is lexical; a symlink that leads
// out of the copy is the OS sandbox's to catch. An empty write-set means the
// turn has no copy boundary (a Direct turn, or a vendor the guard does not
// read), so nothing is outside it.
func outsideCopy(writeSet []string, cwd, p string) bool {
	if len(writeSet) == 0 {
		return false
	}
	// Claude Code trims leading whitespace and expands a leading ~ to $HOME
	// before it resolves a path (measured, 2.1.330), and the gateway does not
	// know the turn's home. So a path it cannot resolve the same way is treated
	// as outside — fail closed: whitespace is trimmed first, then a tilde path
	// (~, ~/…, ~user) or an empty path is refused. Such a path is never inside
	// the copy by a name the gateway can check, and the model addresses an
	// in-copy file by the absolute path Claude Code gives it, not through ~.
	// Checked before Clean, which would collapse "~/.." to "." and hide the ~.
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "~") {
		return true
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		p = filepath.Clean(filepath.Join(cwd, p))
	}
	for _, root := range writeSet {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if p == root {
			return false
		}
		// Rel gives "..", or a path starting "../", only when p is not under
		// root; a lexical check, no filesystem access.
		rel, err := filepath.Rel(root, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}
