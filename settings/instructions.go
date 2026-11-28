package settings

// The instructions are the text every turn gets as an instruction of its
// agent's own: Claude Code's --append-system-prompt and codex's
// developer_instructions, never inside the prompt. The file store keeps them
// in instructions.md. {session} in them becomes the turn's session id, and
// {tools} how the turn calls AgentBus: ToolsText when it has the agentbus
// MCP tools, else APIText (package run), each saying whether the turn may
// apply as [agent_api] apply does. Keep the text the same across a
// session's turns: own replay and the vendor's prompt cache rely on it.

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxInstructions bounds the text, in bytes: it goes on the agent's command
// line.
const MaxInstructions = 64 << 10

// DefaultInstructions is the text of a missing file, and what the file store
// writes when the file is missing. An empty file gives no instructions.
const DefaultInstructions = `You work through AgentBus, in session {session}. A turn is one message to you, your reply and your changes.
Your folder is usually your session's own copy of the person's folder. Your edits stay in the copy. They reach the person's folder only when a turn is applied.
{tools}
`

// ToolsText is what {tools} becomes for a turn with the agentbus MCP tools.
// B15: review is the review_turn app, not a route — the tools cover asking
// another agent (ask_agent), asking one to review a turn (review_turn) and
// handing AgentBus a long command (run_command, H1);
// reading turns and diffs, searching, apply and stop go through agentbus-api.
// apply is [agent_api] apply: whether the turn may apply turns.
func ToolsText(apply bool) string {
	write := `To read turns and their diffs, search this session, or stop a turn, use agentbus-api (GET /api/v0/turns, GET /api/v0/turns/T/diff, POST /api/v0/turns/T/stop). A short guide to every route is at agentbus-api GET /docs/llms.txt.
The person applies your changes to their folder; you cannot.`
	if apply {
		write = `To read turns and their diffs, search this session, stop a turn, or apply your workspace's changes to the person's folder, use agentbus-api (GET /api/v0/turns, GET /api/v0/turns/T/diff, POST /api/v0/turns/T/stop, POST /api/v0/workspaces/{workspace}/apply). A short guide to every route is at agentbus-api GET /docs/llms.txt.`
	}
	return `Use the agentbus MCP tools to ask other saved agents (any vendor) for work (ask_agent) or to have one review a turn (review_turn). AgentBus agents are saved setups across vendors, not your harness's own agents or subagents.
To run a long command, such as a gate of several minutes, hand it to AgentBus with run_command and end your turn: its exit code and output come back as a later turn. Do not run it in the background: that keeps your turn running.
` + write + `
Work you ask for comes back to you as a later turn: do not wait for it.`
}

// APIText is what {tools} becomes for a turn without them. apply is as for
// ToolsText. B15: ask and review are apps (tools), so an agent without the tool
// server asks by starting a turn directly (POST /api/v0/turns).
func APIText(apply bool) string {
	line := "- The person applies your changes to their folder; you cannot.\n"
	if apply {
		line = "- Apply your workspace's changes to the person's folder (every turn not applied yet): agentbus-api POST /api/v0/workspaces/{workspace}/apply\n"
	}
	return `Call AgentBus with agentbus-api METHOD PATH [JSON]. Replace T with a turn id.
- List this session's turns, with each id, number, state and cost: agentbus-api GET /api/v0/turns
` + line + `- Ask another agent to do work and get its reply back here: the ask_agent tool, or three calls. agentbus-api POST /api/v0/sessions '{}' names a new session S2. agentbus-api POST /api/v0/ons '{"event":"turn.completed","filter":{"session":"S2"}}' brings the reply of every turn of S2 back here as a later turn. agentbus-api POST /api/v0/turns '{"session":"S2","agent":"<agent id>","prompt":"<what to do>"}' starts the work and names the new turn T2. To have one review a turn, ask it to read that turn and its diff (GET /api/v0/turns/T, GET /api/v0/turns/T/diff). Pick an agent other than the one that did the turn. List the agents: agentbus-api GET /api/v0/agents. These are AgentBus agents, not your harness's own agents or subagents.
- Run a long command (such as a gate of several minutes) as AgentBus work and end your turn: agentbus-api POST /api/v0/commands '{"command":"<command line>"}'. Its exit code and the end of its output come back here as a later turn. Do not run it in the background: that keeps your turn running.
- Read a turn, its state and cost: agentbus-api GET /api/v0/turns/T
- Stop a turn: agentbus-api POST /api/v0/turns/T/stop
- Search this session's history: agentbus-api GET "/api/v0/sessions/{session}/search?q=<text>", with <text> URL-encoded.
A reply you asked to be delivered comes back as a later turn: do not wait for it. More: agentbus-api GET /docs/llms.txt`
}

// ToolServerInstructions is the tool server's own instructions (POST
// /api/v0/mcp, initialize), which a harness may show its model. apply says
// whether the caller may apply turns ([agent_api] apply). B15: the tools are the
// apps (ask_agent, review_turn); reading the record, applying and stopping go
// through agentbus-api.
func ToolServerInstructions(apply bool) string {
	write := "To read turns, search history or stop a turn, use agentbus-api. The person applies your changes to their folder."
	if apply {
		write = "To read turns, search history, stop a turn, or apply your workspace's changes, use agentbus-api."
	}
	return "AgentBus tools: ask other saved agents (any vendor) for work (ask_agent), have one review a turn (review_turn), or hand AgentBus a long command and end your turn (run_command). " +
		"Your folder is your session's own copy. " + write + " Work you ask for comes back to your session as a later turn."
}

// CheckInstructions refuses text a turn cannot carry.
func CheckInstructions(b []byte) error {
	switch {
	case len(b) > MaxInstructions:
		return errors.New("it is over 64 KiB")
	case !utf8.Valid(b):
		return errors.New("it is not UTF-8 text")
	case strings.ContainsRune(string(b), 0):
		return errors.New("it holds a NUL byte")
	}
	return nil
}
