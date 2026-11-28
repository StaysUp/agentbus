package harness

import (
	"strings"
	"testing"
)

// A command AgentBus runs for a turn never gets more than the turn may do
// (CommandLimit), for every harness and both workspace kinds. Each review P1 of
// 2026-11-27 is a case: codex in a copy, and codex read-only (edits forbidden)
// in a direct folder.
func TestCommandLimit(t *testing.T) {
	edit := Writes{Edit: []string{"/w"}}
	copyMode := Copy{Dir: "/w"}
	cases := []struct {
		name string
		h    string
		t    Turn
		want string // "" may run, else a word of the refusal
	}{
		{"claude direct, may edit", "claude", Turn{Writes: edit}, ""},
		{"claude direct, edits forbidden", "claude", Turn{}, "may not edit"},
		{"claude copy", "claude", Turn{Writes: edit, Copy: copyMode}, "own sandbox"},
		{"codex direct, may edit", "codex", Turn{Writes: edit}, "own sandbox"},
		{"codex direct, read-only", "codex", Turn{}, "may not edit"},
		{"codex copy", "codex", Turn{Writes: edit, Copy: copyMode}, "own sandbox"},
		{"opencode direct, may edit", "opencode", Turn{Writes: edit}, ""},
		{"opencode direct, edits forbidden", "opencode", Turn{}, "may not edit"},
		{"agy direct, may edit", "agy", Turn{Writes: edit}, ""},
		{"walled", "opencode", Turn{Writes: edit, Walled: true}, "wall"},
	}
	for _, c := range cases {
		h, ok := Get(c.h)
		if !ok {
			t.Fatalf("no harness %s", c.h)
		}
		got := CommandLimit(h, c.t)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Every registered harness answers for itself (ShellWall is a method).
	for _, h := range All() {
		_ = CommandLimit(h, Turn{Writes: edit})
	}
}
