package run

// The instructions every turn gets come from the settings store
// (settings.Store), read at the start of every turn. {session} in them
// becomes the turn's session id, and {tools} how the turn calls AgentBus.

import (
	"strings"

	"github.com/staysup/agentbus/settings"
)

// instructions is the text for a turn of session: the store's, or the
// default when there is no store, with {session} filled in and trailing space
// cut. {tools} is left for withTools. "" when the text holds nothing else.
func (e *Engine) instructions(session string) (string, error) {
	text := settings.DefaultInstructions
	if e.cfg.Settings != nil {
		var err error
		if text, _, err = e.cfg.Settings.Instructions(); err != nil {
			return "", err
		}
	}
	return strings.TrimRight(strings.ReplaceAll(text, "{session}", session), " \t\r\n"), nil
}

// withTools fills {tools} in text, a turn's instructions of session: how
// the turn calls AgentBus, by the agentbus_* tools when tools, else by
// agentbus-api, with the session named in it.
func withTools(text, session string, tools bool) string {
	how := settings.APIText
	if tools {
		how = settings.ToolsText
	}
	return strings.ReplaceAll(text, "{tools}", strings.ReplaceAll(how, "{session}", session))
}
