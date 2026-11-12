package run

import (
	"strings"
	"testing"
)

// An agent's error tail keeps its last lines, bounded, as text, with what
// looks like a secret hidden; the reason a turn failed stays readable.
func TestErrorTail(t *testing.T) {
	var b strings.Builder
	for i := range 40 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("\n")
	}
	b.WriteString("\x1b[31mError: Eligibility check failed: request failed with status 401\x1b[0m\n\n")
	b.WriteString("auth: Bearer abcdefghijklmnop.qrstuvwx and api_key=sk-live-0123 and token: \"tok_99\"\n")
	b.WriteString("key sk-ant-api03-AbCdEf0123456789AbCdEf0123456789xyz and the turn's abtok_0123456789\n")
	got := errorTail([]byte(b.String()), false, "abtok_0123456789")
	lines := strings.Split(got, "\n")
	if len(lines) > ErrorTailLines || len(got) > ErrorTailMax {
		t.Fatalf("%d lines, %d bytes", len(lines), len(got))
	}
	if !strings.Contains(got, "Error: Eligibility check failed: request failed with status 401") || strings.Contains(got, "\x1b") {
		t.Fatalf("the reason is not kept as text:\n%s", got)
	}
	for _, s := range []string{"abcdefghijklmnop", "sk-live-0123", "tok_99", "AbCdEf0123456789", "abtok_0123456789"} {
		if strings.Contains(got, s) {
			t.Errorf("%q is in the tail:\n%s", s, got)
		}
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "key [hidden]") {
		t.Errorf("last line %q", last)
	}
	// A long line is cut from its start, at a character.
	long := strings.Repeat("é", ErrorTailMax)
	if got := errorTail([]byte(long), false); len(got) > ErrorTailMax || !strings.HasSuffix(got, "é") || strings.ContainsRune(got, '�') {
		t.Fatalf("a long line: %d bytes", len(got))
	}
	if errorTail(nil, false) != "" || errorTail([]byte("\n \n"), false) != "" {
		t.Fatal("nothing written is no tail")
	}
	if v := secretValues([]string{"OPENROUTER_API_KEY=sk-or-123456", "PATH=/bin", "AGENTBUS_TOKEN=abc"}); len(v) != 1 || v[0] != "sk-or-123456" {
		t.Fatalf("secret values %q", v)
	}
}

// A tail whose start was cut (the buffer kept its last bytes) drops its
// first line: it may start inside a secret, past the part that names it,
// so neither hide nor the shapes would catch it (0.5.1).
func TestErrorTailCutDropsFirstLine(t *testing.T) {
	// The cut fell inside "api_key=sk-or-v1-a1b2...": only "b2c3d4" is left.
	b := []byte("b2c3d4\nError: request failed with status 401\n")
	got := errorTail(b, true, "sk-or-v1-a1b2c3d4")
	if strings.Contains(got, "b2c3d4") || got != "Error: request failed with status 401" {
		t.Fatalf("cut tail %q", got)
	}
	if got := errorTail(b, false); !strings.Contains(got, "b2c3d4") {
		t.Fatalf("uncut tail lost its first line: %q", got)
	}
	if got := errorTail([]byte("one long cut line, all of it maybe a secret"), true); got != "" {
		t.Fatalf("one cut line kept: %q", got)
	}
}
