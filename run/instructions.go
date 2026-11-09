package run

// The instructions every turn gets come from the settings store
// (settings.Store), read at the start of every turn. {session} in them
// becomes the turn's session id.

import (
	"strings"

	"github.com/staysup/agentbus/settings"
)

// instructions is the text for a turn of session: the store's, or the
// default when there is no store, with {session} filled in and trailing space
// cut. "" when the text holds nothing else.
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
