package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// A codex exec tool call can run several commands in one script. Forbid wins
// over ask across every command in the call, so a forbidden command is refused
// even when an ask command comes before it — the ordering does not let it
// through. (Review round 3: an ask command before a forbidden one in the same
// exec held the whole call for ask and ran the forbidden one on an allow.)
func TestDecideForbidWinsInMultiCommandExec(t *testing.T) {
	for _, script := range []string{
		`await tools.exec_command({cmd:"git status"}); await tools.exec_command({cmd:"git push origin main"});`,
		`await tools.exec_command({cmd:"git push origin main"}); await tools.exec_command({cmd:"git status"});`,
	} {
		g := New(&url.URL{}, openai.Adapter{}, &event.Memory{})
		b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"},
			Rules{Forbid: []string{"git push"}, Ask: []string{"git status"}}, nil, nil,
			io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
		item := map[string]any{"type": "custom_tool_call", "name": "exec", "input": script}
		b.decide("c1", "c1", "exec", json.RawMessage(`"exec"`), item)
		if len(b.asked) != 0 {
			t.Fatalf("a forbidden command was recorded as ask instead of refused (script %q): %+v", script, b.asked)
		}
		if txt := b.refused["c1"]; !strings.Contains(txt, "Forbid rule") || !strings.Contains(txt, "git push") {
			t.Fatalf("forbid did not win over ask in a multi-command exec (script %q): %q", script, txt)
		}
		g.Close()
	}
}
