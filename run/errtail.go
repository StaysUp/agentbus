package run

// An agent's last error lines, for its turn's session.closed when it exits
// with an error: what it wrote to stderr, bounded, with anything shaped
// like a secret hidden. Without it the record says only "failed", and the
// reason sits in the runner's own log (logs/runs/<session>.log).

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
)

// Bounds of an error tail.
const (
	// ErrorTailKept is how much of a child's stderr is held while it runs:
	// its last bytes.
	ErrorTailKept = 8 << 10
	// ErrorTailLines and ErrorTailMax bound what goes on session.closed.
	ErrorTailLines = 20
	ErrorTailMax   = 2 << 10
)

// hidden replaces what looks like a secret.
const hidden = "[hidden]"

var (
	// A value after a name that says it is a secret: key=..., "token": "...".
	secretNamed = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?key|secret|token|password|passwd|authorization|cookie|credential)s?["']?\s*[:=]\s*["']?)[^\s"',;&]{4,}`)
	// The word after Bearer or Basic.
	secretScheme = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// A long run of key-shaped characters: API keys, tokens, JWT parts.
	secretShaped = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)
	// The name of an environment variable that holds a secret.
	secretEnv = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|auth|credential|cookie)`)
)

// errorTail is the last lines of b, a child's stderr, as session.closed
// holds them: at most ErrorTailLines lines and ErrorTailMax bytes, the end
// kept, as UTF-8 with no control characters but newlines and tabs, with each
// of hide and anything shaped like a secret replaced. Empty when b holds no
// text. cut says b lost its start (the buffer kept only its last bytes):
// then its first line is dropped, as it may start inside a secret that
// neither hide nor the shapes would know.
func errorTail(b []byte, cut bool, hide ...string) string {
	if cut {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return "" // one cut line: all of it may be part of a secret
		}
		b = b[i+1:]
	}
	s := strings.ToValidUTF8(string(b), "�")
	for _, h := range hide {
		if len(h) >= 6 {
			s = strings.ReplaceAll(s, h, hidden)
		}
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, " \t"); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > ErrorTailLines {
		lines = lines[len(lines)-ErrorTailLines:]
	}
	for i, l := range lines {
		l = secretScheme.ReplaceAllString(l, "$1 "+hidden)
		l = secretNamed.ReplaceAllString(l, "${1}"+hidden)
		lines[i] = secretShaped.ReplaceAllStringFunc(l, func(m string) string {
			if m == hidden || !strings.ContainsAny(m, "0123456789") {
				return m // a long word, not a key
			}
			return hidden
		})
	}
	out := strings.Join(lines, "\n")
	if len(out) > ErrorTailMax {
		out = out[len(out)-ErrorTailMax:]
		// Start at a whole line, or at least a whole character.
		if i := strings.IndexByte(out, '\n'); i >= 0 && i < len(out)-1 {
			out = out[i+1:]
		}
		out = strings.ToValidUTF8(out, "")
	}
	return out
}

// secretValues are the values of env's variables whose names say they hold
// a secret, such as an API key handed to the agent.
func secretValues(env []string) []string {
	var out []string
	for _, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if ok && len(val) >= 6 && secretEnv.MatchString(name) {
			out = append(out, val)
		}
	}
	return out
}
