//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A runner's own secrets never reach its agents. The runner runs each
// session in a home of its own, and such a turn keeps only the variables every
// program needs, those its harness's wiring set, and those named in
// -pass-env. The secrets here are dummies in the runner process's
// environment. (2026-11-24: a runner's GH_TOKEN and AWS keys reached every
// agent it ran.)
func TestXProcRunnerKeepsItsSecrets(t *testing.T) {
	folder := t.TempDir()
	secrets := map[string]string{
		"GH_TOKEN":              "dummy-gh-token",
		"GITHUB_TOKEN":          "dummy-github-token",
		"AWS_SECRET_ACCESS_KEY": "dummy-aws-secret",
		"OPENAI_API_KEY":        "dummy-openai-key",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("LANG", "C.UTF-8")
	report := filepath.Join(t.TempDir(), "report.json")
	x := startServer(t, "claude")
	x.setAgentEnv("AGENTBUS_FAKE_OUT="+report, "MY_TOOL_HOME=/opt/tool")
	x.addRunner("far", folder)

	fs := x.folders()
	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref,
		"machine": fs[0].Machine, "prompt": "hello", "isolate": true})
	if code/100 != 2 || st.Session == "" {
		t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
	}
	waitFor(t, "the agent's report", func() bool {
		_, err := os.Stat(report)
		return err == nil
	})
	env := envOf(t, report)
	for k := range secrets {
		if v, ok := env[k]; ok {
			t.Errorf("the agent got the runner's %s=%q", k, v)
		}
	}
	// What every program needs, what -pass-env named, and the turn's own.
	if env["LANG"] != "C.UTF-8" || env["PATH"] == "" {
		t.Errorf("the agent lost the base variables: LANG=%q PATH=%q", env["LANG"], env["PATH"])
	}
	if env["MY_TOOL_HOME"] != "/opt/tool" {
		t.Errorf("-pass-env did not keep MY_TOOL_HOME: %q", env["MY_TOOL_HOME"])
	}
	if env["AGENTBUS_URL"] == "" || env["AGENTBUS_TOKEN"] == "" || env["CLAUDE_CONFIG_DIR"] == "" {
		t.Errorf("the agent lost the turn's own variables: %v", env)
	}
}
