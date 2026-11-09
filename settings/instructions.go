package settings

// The instructions are the text every turn gets as an instruction of its
// agent's own: Claude Code's --append-system-prompt and codex's
// developer_instructions, never inside the prompt. The file store keeps them
// in instructions.md. {session} in them becomes the turn's session id (package
// run). Keep the text the same across a session's turns: own replay and the
// vendor's prompt cache rely on it.

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
const DefaultInstructions = `Your session's earlier turns are stored by AgentBus. To look something up, run:
agentbus-api GET "/api/v0/sessions/{session}/search?q=<text>"
with <text> URL-encoded. Each hit's "read" is a path you can GET the same way, for the whole of what matched.
You can also ask other agents with the same command: start a turn on any saved agent, continue one, or ask for a review. Do not wait for them: each result comes back to you as a later turn. How: agentbus-api GET /docs/llms.txt, section "Ask other agents".
`

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
