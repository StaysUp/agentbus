package settings

// The instructions are the text every turn gets as an instruction of its
// agent's own: Claude Code's --append-system-prompt and codex's
// developer_instructions, never inside the prompt. The file store keeps them
// in instructions.md. {session} in them becomes the turn's session id, and
// {tools} how the turn calls AgentBus: ToolsText when it has the agentbus
// MCP tools, else APIText (package run). Keep the text the same across a
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
const ToolsText = `Use the tools of the agentbus MCP server for AgentBus: list and ask other agents, review, read, apply and stop turns, search this session. AgentBus agents are saved setups across vendors, not your harness's own agents or subagents.
Work you ask for comes back to you as a later turn: do not wait for it.`

// APIText is what {tools} becomes for a turn without them.
const APIText = `Call AgentBus with agentbus-api METHOD PATH [JSON]. Replace T with a turn id.
- List this session's turns, with each id, number, state and cost: agentbus-api GET /api/v0/turns
- Apply a turn to the person's folder (its changes and the earlier turns' that are not applied yet): agentbus-api POST /api/v0/turns/T/apply
- Ask another agent to review a turn: agentbus-api POST /api/v0/turns/T/review '{"agent":"<agent id>"}'. Pick an agent other than the one that did the turn. List the agents: agentbus-api GET /api/v0/agents. These are AgentBus agents, not your harness's own agents or subagents.
- Read a turn, its state, cost and a review's verdict: agentbus-api GET /api/v0/turns/T
- Stop a turn: agentbus-api POST /api/v0/turns/T/stop
- Search this session's history: agentbus-api GET "/api/v0/sessions/{session}/search?q=<text>", with <text> URL-encoded.
Work you ask for comes back to you as a later turn: do not wait for it. More: agentbus-api GET /docs/llms.txt`

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
