package event

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrorLineMax bounds ErrorLine, in bytes.
const ErrorLineMax = 300

// errorLead is a line that says it is an error, as every harness starts
// one: "Error: ..." (claude, codex), "error: ..." (agy 1.5.9, for a --model
// it does not know, followed by its list of models), "ERROR ...".
var errorLead = regexp.MustCompile(`(?i)^(error|fatal)\b`)

// listItem is a line of a list: indented, or a bullet or a number first,
// such as agy's "  Gemini 3.1 Pro (Low)" under "Available models:".
var listItem = regexp.MustCompile(`^(\s|[-*•]\s|\d+[.)]\s)`)

// ErrorLine is the line of an agent's error output (its stderr, already
// cleaned of secrets) that says why it failed, the same way for every
// harness: the first line that starts with "error" or "fatal" (any case),
// else the first non-empty line that is not a list item. Never a list item:
// a harness that ends with a list (agy's models) would otherwise name one
// as the error. "" when no line fits. At most ErrorLineMax bytes, cut at a
// character.
func ErrorLine(text string) string {
	var first string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if errorLead.MatchString(strings.TrimSpace(l)) {
			return bound(strings.TrimSpace(l))
		}
		if first == "" && !listItem.MatchString(l) {
			first = strings.TrimSpace(l)
		}
	}
	return bound(first)
}

// ExitError is the error of a failed run's run.ended when its agent
// wrote no line ErrorLine would take: its exit code alone.
func ExitError(code int) string {
	return fmt.Sprintf("the agent exited with code %d and wrote no reason to its error output", code)
}

func bound(s string) string {
	if len(s) <= ErrorLineMax {
		return s
	}
	cut := ErrorLineMax - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
