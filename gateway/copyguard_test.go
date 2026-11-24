package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// copyDecider builds a guardBody with a copy write-set, so decide can be driven
// directly for the copy-boundary check (like decider in onegate_test.go).
func copyDecider(t *testing.T, a adapter.Adapter, copyWrite []string) *guardBody {
	t.Helper()
	g := New(&url.URL{}, a, &event.Memory{})
	return g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"},
		Rules{Unlisted: Allow}, nil, copyWrite,
		io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
}

func editItem(name, pathKey, path string) map[string]any {
	return map[string]any{"name": name, "input": map[string]any{pathKey: path,
		"old_string": "x", "new_string": "y"}}
}

// A file-edit tool call whose path is outside the turn's copy is refused, with a
// reason naming the path. Every Claude Code write tool and its path field.
func TestCopyEditOutsideRefused(t *testing.T) {
	for _, tc := range []struct{ tool, key string }{
		{"Edit", "file_path"},
		{"Write", "file_path"},
		{"MultiEdit", "file_path"},
		{"NotebookEdit", "notebook_path"},
	} {
		b := copyDecider(t, anthropic.Adapter{}, []string{"/copy"})
		item := editItem(tc.tool, tc.key, "/etc/passwd")
		b.decide("0", "c0", tc.tool, json.RawMessage(`{}`), item)
		text, ok := b.refused["0"]
		if !ok {
			t.Fatalf("%s outside the copy was not refused", tc.tool)
		}
		if !strings.Contains(text, "outside this turn's copy") || !strings.Contains(text, "/etc/passwd") {
			t.Errorf("%s refusal text = %q", tc.tool, text)
		}
	}
}

// A file-edit inside the copy runs: it is not refused.
func TestCopyEditInsideRuns(t *testing.T) {
	for _, p := range []string{"/copy/a.go", "/copy/sub/dir/b.txt", "/copy"} {
		b := copyDecider(t, anthropic.Adapter{}, []string{"/copy"})
		b.decide("0", "c0", "Edit", json.RawMessage(`{}`), editItem("Edit", "file_path", p))
		if _, ok := b.refused["0"]; ok {
			t.Errorf("an edit of %q inside the copy was refused", p)
		}
	}
}

// A relative path resolves against the copy (the turn's working folder), so a
// relative edit inside the copy runs and one that climbs out is refused.
func TestCopyRelativePath(t *testing.T) {
	b := copyDecider(t, anthropic.Adapter{}, []string{"/copy"})
	b.decide("0", "c0", "Edit", json.RawMessage(`{}`), editItem("Edit", "file_path", "sub/a.go"))
	if _, ok := b.refused["0"]; ok {
		t.Errorf("a relative edit inside the copy was refused")
	}
	b2 := copyDecider(t, anthropic.Adapter{}, []string{"/copy"})
	b2.decide("1", "c1", "Edit", json.RawMessage(`{}`), editItem("Edit", "file_path", "../outside/a.go"))
	if _, ok := b2.refused["1"]; !ok {
		t.Errorf("a relative edit climbing out of the copy was not refused")
	}
}

// Read is a read — Copy leaves reads open across the disk — so reading outside
// the copy is not refused on a path.
func TestCopyReadNotRefused(t *testing.T) {
	b := copyDecider(t, anthropic.Adapter{}, []string{"/copy"})
	item := map[string]any{"name": "Read", "input": map[string]any{"file_path": "/etc/passwd"}}
	b.decide("0", "c0", "Read", json.RawMessage(`{}`), item)
	if _, ok := b.refused["0"]; ok {
		t.Errorf("a Read outside the copy was refused")
	}
}

// An edit under an extra writable path (a sandbox allowWrite entry beyond the
// copy, e.g. a tool cache) runs: the gateway mirrors the sandbox's whole set.
func TestCopyExtraWritablePath(t *testing.T) {
	b := copyDecider(t, anthropic.Adapter{}, []string{"/copy", "/tmp/cache"})
	b.decide("0", "c0", "Write", json.RawMessage(`{}`), editItem("Write", "file_path", "/tmp/cache/x"))
	if _, ok := b.refused["0"]; ok {
		t.Errorf("an edit under an extra writable path was refused")
	}
}

// A Direct turn (no copy) has an empty write-set, so no edit is refused on a
// path — even one far outside any folder.
func TestDirectTurnNoPathRefusal(t *testing.T) {
	b := copyDecider(t, anthropic.Adapter{}, nil)
	b.decide("0", "c0", "Edit", json.RawMessage(`{}`), editItem("Edit", "file_path", "/etc/passwd"))
	if _, ok := b.refused["0"]; ok {
		t.Errorf("a Direct turn refused an edit on a path")
	}
}

// codex (OpenAI Responses) writes through the shell, with no path field, so the
// copy-path check refuses nothing of its — its containment is the sandbox.
func TestCopyNonAnthropicNoPathRefusal(t *testing.T) {
	b := copyDecider(t, openai.Adapter{}, []string{"/copy"})
	item := map[string]any{"type": "function_call", "name": "edit",
		"arguments": `{"file_path":"/etc/passwd"}`}
	b.decide("c1", "c1", "edit", json.RawMessage(`{"file_path":"/etc/passwd"}`), item)
	if _, ok := b.refused["c1"]; ok {
		t.Errorf("a non-anthropic call was refused on a path")
	}
}

// A refused edit's call id is remembered with the copy refusal, so the next
// request's fixHistory puts that text in place of the call's output (the same
// path a forbidden command takes).
func TestCopyRefusalRememberedForFixHistory(t *testing.T) {
	b := copyDecider(t, anthropic.Adapter{}, []string{"/copy"})
	b.decide("0", "c0", "Edit", json.RawMessage(`{}`), editItem("Edit", "file_path", "/etc/passwd"))
	text := b.g.refused.text("c0")
	if text == refusalText("") {
		t.Fatalf("the call id was not remembered for fixHistory (got the generic refusal)")
	}
	if !strings.Contains(text, "outside this turn's copy") {
		t.Errorf("remembered text = %q", text)
	}
}

// outsideCopy is lexical: it resolves a relative path against the copy and
// decides containment with no filesystem access (the gateway never touches a
// folder). A symlink is the OS sandbox's to catch, so it is not a case here.
func TestOutsideCopy(t *testing.T) {
	set := []string{"/copy"}
	for _, tc := range []struct {
		path string
		out  bool
	}{
		{"/copy/a.go", false},
		{"/copy", false},
		{"/copy/sub/b", false},
		{"/copyother/a", true}, // a sibling sharing a prefix is outside
		{"/etc/passwd", true},
		{"a.go", false},             // relative, resolves under the copy
		{"sub/a.go", false},         // relative
		{"../outside/a", true},      // climbs out
		{"/copy/../etc/x", true},    // cleans to /etc/x
		{"/copy/./sub/../a", false}, // cleans to /copy/a
		// Live-measured bypass vectors (Claude Code 2.1.330): a tilde path is
		// expanded to $HOME, leading whitespace is trimmed, and ~/.. collapses —
		// each must be refused, never turned into a path under the copy.
		{"~/pwned.txt", true},
		{"~/.claude/projects/x/memory/test.md", true},
		{"~", true},
		{"~/../escaped.txt", true},
		{"~root/x", true},
		{" /etc/passwd", true},
		{"\t/etc/passwd", true},
		{" ~/pwned.txt", true},
		{"  sub/a.go  ", false}, // trimmed, still a relative in-copy path
		{"", true},              // empty: refused, fail closed
		{"   ", true},           // whitespace only: refused
	} {
		if got := outsideCopy(set, "/copy", tc.path); got != tc.out {
			t.Errorf("outsideCopy(%q) = %v, want %v", tc.path, got, tc.out)
		}
	}
	// An empty write-set is no boundary: nothing is outside it.
	if outsideCopy(nil, "/copy", "/etc/passwd") {
		t.Errorf("an empty write-set treated a path as outside")
	}
	// Several writable roots: a path under any one is inside.
	multi := []string{"/copy", "/tmp/cache"}
	if outsideCopy(multi, "/copy", "/tmp/cache/x") {
		t.Errorf("a path under a second writable root was outside")
	}
	if !outsideCopy(multi, "/copy", "/tmp/other/x") {
		t.Errorf("a path under no writable root was inside")
	}
}
