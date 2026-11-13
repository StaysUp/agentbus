//go:build integration

package main

// The models a codex agent's sign-in may use, as unmodified codex keeps them
// in a session's home: after a turn the runner reads only their names and
// reports them to control, and the agent form's list offers them. Nothing
// else of the file reaches control.

import (
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/queue"
)

func TestRunnerReportsCodexModels(t *testing.T) {
	ns := startNoSecrets(t)
	code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/harnesses/codex/models?access=subscription")
	if code != 200 || !strings.Contains(raw, `"models":[]`) || !strings.Contains(raw, `"source":"runner"`) {
		t.Fatalf("before a turn: %d %s", code, raw)
	}
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"thread.started","thread_id":"fake-thread-m"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_MODELS_CACHE", `{"fetched_at":"2026-11-13T10:00:00Z","identity":{"email":"someone@example.com"},`+
		`"models":[{"slug":"gpt-6.1-sol","visibility":"list","display_name":"GPT-6.1 Sol"},{"slug":"gpt-reserve","visibility":"hide"},{"slug":"gpt-6-luna","visibility":"list"}]}`)
	// A fake ChatGPT sign-in connected on control, so the turn starts.
	if err := subscriptionStore(ns.serverHome).Keep("codex", credentials.Signin{Token: "fake-chatgpt-access-for-tests",
		Refresh: "fake-chatgpt-refresh-for-tests", Account: "fake-account-1", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": "codex", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	if jj := waitJob(t, ns.server, ns.tok, j.ID); jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	waitFor(t, "the runner's report of codex's models", func() bool {
		_, raw = getAPI(t, ns.server, ns.tok, "/api/v0/harnesses/codex/models?access=subscription")
		return strings.Contains(raw, `"models":[{"id":"gpt-6.1-sol"},{"id":"gpt-6-luna"}]`)
	})
	if !strings.Contains(raw, `"fetched_at":`) || strings.Contains(raw, "example.com") || strings.Contains(raw, "gpt-reserve") {
		t.Fatalf("%s", raw)
	}
	// Nothing of the file but the names reached control.
	_, raw = getAPI(t, ns.server, ns.tok, "/api/v0/runners")
	if strings.Contains(raw, "example.com") || strings.Contains(raw, "fetched_at") || strings.Contains(raw, "GPT-6.1 Sol") {
		t.Fatalf("runners: %s", raw)
	}
}
