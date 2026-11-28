package run

import (
	"slices"
	"testing"
)

// A turn in a home of its own gets none of the service's secrets: only the
// allow list, the locale, the turn's own variables, what the wiring set and
// what the person named. The values are dummies.
func TestCleanEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/home/runner", "LANG=C.UTF-8", "LC_ALL=C", "HTTPS_PROXY=http://proxy:3128",
		"AGENTBUS_URL=http://127.0.0.1:1", "AGENTBUS_TOKEN=turn-token",
		"GH_TOKEN=dummy-gh", "GITHUB_TOKEN=dummy-github", "AWS_SECRET_ACCESS_KEY=dummy-aws", "AWS_ACCESS_KEY_ID=dummy-id",
		"OPENAI_API_KEY=dummy-oai", "AGENTBUS_CODEX_TOKEN=dummy-codex", "SSH_AUTH_SOCK=/tmp/agent.sock",
		"XDG_CONFIG_HOME=/home/runner/.config",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1/t", "OPENROUTER_API_KEY=placeholder", "MY_TOOL_HOME=/opt/tool",
	}
	got := cleanEnv(env, []string{"ANTHROPIC_BASE_URL", "OPENROUTER_API_KEY"}, []string{"MY_TOOL_HOME"})
	want := []string{
		"PATH=/usr/bin", "HOME=/home/runner", "LANG=C.UTF-8", "LC_ALL=C", "HTTPS_PROXY=http://proxy:3128",
		"AGENTBUS_URL=http://127.0.0.1:1", "AGENTBUS_TOKEN=turn-token",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1/t", "OPENROUTER_API_KEY=placeholder", "MY_TOOL_HOME=/opt/tool",
	}
	if !slices.Equal(got, want) {
		t.Errorf("env\n got %q\nwant %q", got, want)
	}
}
