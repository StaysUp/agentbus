//go:build integration

package main

// Each vendor's API key set in Settings: pasted once over the API, kept on
// control beside the subscriptions, and added by the relay, in that vendor's
// own header, to the calls of agents with access api of that vendor's
// harness only. The agent holds a placeholder. The key is never in a reply,
// the service log, the record, a file under a home or the agent's
// environment. Every key here is fake.

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

func TestRunnerUsesAPIKeysSetInSettings(t *testing.T) {
	keys := map[string]string{
		"anthropic": "sk-ant-api03-fake-settings-key-0000111122223333",
		"openai":    "sk-proj-fake-settings-key-4444555566667777",
		"gemini":    "AIzaFakeSettingsKey000011112222333344",
	}
	ns := startNoSecrets(t)
	var replies []string
	keep := func(s string) { replies = append(replies, s) }

	code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/subscriptions")
	keep(raw)
	if code != 200 || !strings.Contains(raw, `"api_keys":{"anthropic":{"set":false},"gemini":{"set":false},"openai":{"set":false}}`) {
		t.Fatalf("before: %d %s", code, raw)
	}
	// One agent per harness on its vendor's API key.
	ids := map[string]string{}
	for _, h := range []string{"claude", "codex", "agy"} {
		code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": h + " on its key", "harness": h, "access": "api"})
		if code != http.StatusCreated || ag["key_set"] != false {
			t.Fatalf("create %s: %d %v", h, code, ag)
		}
		ids[h] = ag["id"].(string)
	}
	code, raw = getAPI(t, ns.server, ns.tok, "/api/v0/harnesses")
	keep(raw)
	for _, name := range []string{"anthropic", "openai", "gemini"} {
		if code != 200 || !strings.Contains(raw, `"kind":"api"`) || !strings.Contains(raw, `"key_name":"`+name+`"`) {
			t.Fatalf("harnesses: %d %s", code, raw)
		}
	}
	runOne := func(h string) queue.Job {
		t.Helper()
		j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": ids[h], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
		return waitJob(t, ns.server, ns.tok, j.ID)
	}

	// No key set: refused in words that say where to set it, before any call.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-a"}`+"\n")
	if jj := runOne("claude"); jj.State != queue.Failed || !strings.Contains(string(jj.Result), `set your Anthropic API key in Settings`) {
		t.Fatalf("with no key: %s %s", jj.State, jj.Result)
	}
	if got := ns.keyed["claude"].seen(); len(got) != 0 {
		t.Fatalf("a call went out with no key: %v", got)
	}

	// A key that cannot be one is refused, and its words quote nothing; a
	// wrong token cannot set one.
	code, out := sendAPI(t, ns.server, ns.tok, "PUT", "/api/v0/subscriptions/anthropic/key", map[string]any{"key": "has space " + keys["anthropic"]})
	if code != http.StatusBadRequest || strings.Contains(out["error"].(string), keys["anthropic"]) {
		t.Fatalf("a bad key: %d %v", code, out)
	}
	if code, _ = sendAPI(t, ns.server, "abt_not-a-token", "PUT", "/api/v0/subscriptions/anthropic/key", map[string]any{"key": keys["anthropic"]}); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token set the key: %d", code)
	}
	if code, _ = sendAPI(t, ns.server, ns.tok, "PUT", "/api/v0/subscriptions/claude/key", map[string]any{"key": keys["anthropic"]}); code == http.StatusNoContent {
		t.Fatal("a key was set under a name that is no key")
	}
	for name, k := range keys {
		if code, out := sendAPI(t, ns.server, ns.tok, "PUT", "/api/v0/subscriptions/"+name+"/key", map[string]any{"key": " " + k + "\n"}); code != http.StatusNoContent || out != nil {
			t.Fatalf("set %s: %d %v", name, code, out)
		}
	}
	code, raw = getAPI(t, ns.server, ns.tok, "/api/v0/subscriptions")
	keep(raw)
	for _, name := range []string{"anthropic", "openai", "gemini"} {
		if !strings.Contains(raw, `"`+name+`":{"set":true,"set_at":`) {
			t.Fatalf("after: %s", raw)
		}
	}
	if !strings.Contains(raw, `"openrouter_key":{"set":false}`) {
		t.Fatalf("a vendor key reads as the OpenRouter key: %s", raw)
	}
	for _, p := range []string{"/api/v0/agents", "/api/v0/harnesses"} {
		code, raw = getAPI(t, ns.server, ns.tok, p)
		keep(raw)
		if code != 200 || !strings.Contains(raw, `"key_set":true`) {
			t.Fatalf("%s: %d %s", p, code, raw)
		}
	}

	// claude: the key in x-api-key, no Authorization, at Anthropic's API.
	// Its try of another endpoint with the key reaches nothing.
	t.Setenv("AGENTBUS_FAKE_OTHER", "DELETE /v1/files/file_1")
	if jj := runOne("claude"); jj.State != queue.Finished {
		t.Fatalf("claude: %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	// One call each: the try of another endpoint never reached the vendor.
	if got := ns.keyed["claude"].seen(); len(got) != 1 || got[0] != "/v1/messages " {
		t.Fatalf("Anthropic's API saw %v", got)
	}
	if got := ns.keyed["claude"].keysSeen(); got[0] != "/v1/messages "+keys["anthropic"]+"|" {
		t.Fatalf("Anthropic's API saw keys %v", got)
	}
	env := envOf(t, ns.report)
	if tok := env["ANTHROPIC_AUTH_TOKEN"]; !strings.HasPrefix(tok, "abr_") || env["ANTHROPIC_API_KEY"] != "" ||
		!strings.HasSuffix(env["ANTHROPIC_BASE_URL"], "/key/claude") {
		t.Fatalf("claude's environment: %q %q %q", tok, env["ANTHROPIC_API_KEY"], env["ANTHROPIC_BASE_URL"])
	}
	os.Remove(ns.report)

	// codex: the key as the bearer at OpenAI's API.
	t.Setenv("AGENTBUS_FAKE_OTHER", "POST /v1/responses/compact")
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"thread.started","thread_id":"fake-thread-k"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	if jj := runOne("codex"); jj.State != queue.Finished {
		t.Fatalf("codex: %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	if got := ns.keyed["codex"].seen(); len(got) != 1 || got[0] != "/v1/responses Bearer "+keys["openai"] {
		t.Fatalf("OpenAI's API saw %v", got)
	}
	if tok := envOf(t, ns.report)["AGENTBUS_OPENAI_TOKEN"]; !strings.HasPrefix(tok, "abr_") {
		t.Fatalf("codex's placeholder: %q", tok)
	}
	os.Remove(ns.report)
	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", "")

	// agy: the key in x-goog-api-key, no Authorization, at the Gemini API,
	// with the gemini provider named for the run only.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"conversation_id":"fake-conv-k","status":"SUCCESS","response":"pong"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_AGY_BODY", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	t.Setenv("AGENTBUS_FAKE_OTHER", "POST /v1beta/cachedContents")
	if jj := runOne("agy"); jj.State != queue.Finished {
		t.Fatalf("agy: %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	const geminiPath = "/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent"
	if got := ns.keyed["agy"].seen(); len(got) != 1 || got[0] != geminiPath+" " {
		t.Fatalf("the Gemini API saw %v", got)
	}
	if got := ns.keyed["agy"].keysSeen(); got[0] != geminiPath+" |"+keys["gemini"] {
		t.Fatalf("the Gemini API saw keys %v", got)
	}
	env = envOf(t, ns.report)
	if tok := env["GEMINI_API_KEY"]; !strings.HasPrefix(tok, "abr_") || !strings.HasSuffix(env["GOOGLE_GEMINI_BASE_URL"], "/key/agy") {
		t.Fatalf("agy's environment: %q %q", tok, env["GOOGLE_GEMINI_BASE_URL"])
	}
	t.Setenv("AGENTBUS_FAKE_AGY_BODY", "")
	t.Setenv("AGENTBUS_FAKE_OTHER", "")

	// Each try of another endpoint was refused at control's relay and
	// recorded there as call.refused.
	refused := map[string]bool{}
	for _, e := range storedEvents(t, ns.server.store) {
		if e.Kind == "call.refused" && e.Status == http.StatusForbidden {
			refused[e.Call] = true
		}
	}
	for _, call := range []string{"DELETE /v1/files/file_1", "POST /v1/responses/compact", "POST /v1beta/cachedContents"} {
		if !refused[call] {
			t.Errorf("control recorded no call.refused for %s: %v", call, refused)
		}
	}

	// A reply in a format the adapter does not read is not passed on.
	ns.keyed["agy"].mu.Lock()
	ns.keyed["agy"].ctype = "application/x-protobuf"
	ns.keyed["agy"].mu.Unlock()
	t.Setenv("AGENTBUS_FAKE_AGY_BODY", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	runOne("agy")
	t.Setenv("AGENTBUS_FAKE_AGY_BODY", "")
	bad := 0
	for _, e := range storedEvents(t, ns.server.store) {
		if e.Kind == "call.refused" && e.Status == http.StatusBadGateway {
			bad++
		}
	}
	if bad != 1 {
		t.Errorf("unreadable replies recorded: %d, want 1", bad)
	}

	// No subscription's vendor, nor OpenRouter, saw any of these calls.
	for name, v := range map[string]*fakeVendor{"anthropic": ns.anth, "chatgpt": ns.chat, "code assist": ns.cc, "openrouter": ns.or} {
		if got := v.seen(); len(got) != 0 {
			t.Fatalf("%s saw %v", name, got)
		}
	}

	// Removed: the next turn is refused again.
	for name := range keys {
		if code, _ := sendAPI(t, ns.server, ns.tok, "DELETE", "/api/v0/subscriptions/"+name+"/key", nil); code != http.StatusNoContent {
			t.Fatalf("remove %s: %d", name, code)
		}
	}
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-b"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	if jj := runOne("claude"); jj.State != queue.Failed || !strings.Contains(string(jj.Result), `set your Anthropic API key in Settings`) {
		t.Fatalf("after remove: %s %s", jj.State, jj.Result)
	}

	// No key is in a reply, the record, a file under either home, or an
	// agent's environment.
	holds := func(b string) string {
		for name, k := range keys {
			if strings.Contains(b, k) {
				return name
			}
		}
		return ""
	}
	for _, r := range replies {
		if n := holds(r); n != "" {
			t.Fatalf("a reply holds the %s key: %s", n, r)
		}
	}
	for _, svc := range []*service{ns.server, ns.runner} {
		recs, _ := svc.store.After(0, store.Filter{}, 100000)
		for _, r := range recs {
			if n := holds(string(r.Data)); n != "" {
				t.Fatalf("the record holds the %s key: %s", n, r.Data)
			}
		}
	}
	time.Sleep(100 * time.Millisecond) // the logs' last lines
	for _, home := range []string{ns.runnerHome, ns.serverHome, filepath.Dir(ns.report)} {
		filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, _ := os.ReadFile(p); holds(string(b)) != "" {
					t.Fatalf("%s holds the %s key", p, holds(string(b)))
				}
			}
			return nil
		})
	}
}
