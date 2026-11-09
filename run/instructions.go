package run

// instructions.md, in the data folder, is the text every turn gets as an
// instruction of its agent's own: Claude Code's --append-system-prompt and
// codex's developer_instructions, never inside the prompt. It is read at the
// start of every turn. {session} in it becomes the turn's session id. Keep
// the text the same across a session's turns: own replay and the vendor's
// prompt cache rely on it.

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"
)

// InstructionsFile is the file's name in the data folder.
const InstructionsFile = "instructions.md"

// MaxInstructions bounds the file, in bytes: it goes on the agent's command
// line.
const MaxInstructions = 64 << 10

// DefaultInstructions is the text a missing file gives, and what `agentbus
// up` writes when the file is missing. An empty file gives no instructions.
const DefaultInstructions = `Your session's earlier turns are stored by AgentBus. To look something up, run:
agentbus-api GET "/api/v0/sessions/{session}/search?q=<text>"
with <text> URL-encoded. Each hit's "read" is a path you can GET the same way, for the whole of what matched.
You can also ask other agents with the same command: start a turn on any saved agent, continue one, or ask for a review. Do not wait for them: each result comes back to you as a later turn. How: agentbus-api GET /docs/llms.txt, section "Ask other agents".
`

// instructions is the text for a turn of session: the file's, or the
// default when there is none, with {session} filled in and trailing space
// cut. "" when the file holds nothing else.
func (e *Engine) instructions(session string) (string, error) {
	text, err := loadInstructions(e.cfg.Instructions)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(strings.ReplaceAll(text, "{session}", session), " \t\r\n"), nil
}

// loadInstructions reads the file at path. "" or a missing file is the
// default. The error never names the path.
func loadInstructions(path string) (string, error) {
	if path == "" {
		return DefaultInstructions, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := os.Lstat(path); lerr == nil {
			return "", errors.New("it is a link to a file that does not exist")
		}
		return DefaultInstructions, nil
	}
	if err != nil {
		return "", errors.New("it could not be read")
	}
	switch {
	case len(b) > MaxInstructions:
		return "", errors.New("it is over 64 KiB")
	case !utf8.Valid(b):
		return "", errors.New("it is not UTF-8 text")
	case strings.ContainsRune(string(b), 0):
		return "", errors.New("it holds a NUL byte")
	}
	return string(b), nil
}
