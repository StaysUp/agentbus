//go:build integration

package main

// The OpenRouter key set in Settings: pasted once over the API, kept on
// control beside the subscriptions, added by the relay to an OpenRouter
// agent's calls in place of the key in its variable, and never in a reply,
// the service log, the record or the agent's environment. Every key here
// is fake.

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

func TestRunnerUsesKeySetInSettings(t *testing.T) {
	const settingsKey = "sk-or-v1-fake-settings-key-0000111122223333"
	ns := startNoSecrets(t)
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-k"}`+"\n")
	var replies []string
	keep := func(s string) { replies = append(replies, s) }

	// Nothing set yet: the list says so.
	code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/subscriptions")
	keep(raw)
	if code != 200 || !strings.Contains(raw, `"openrouter_key":{"set":false}`) {
		t.Fatalf("before: %d %s", code, raw)
	}
	// A key that cannot be one is refused, and its words quote nothing.
	code, out := sendAPI(t, ns.server, ns.tok, "PUT", "/api/v0/subscriptions/openrouter/key", map[string]any{"key": "has space " + settingsKey})
	if code != http.StatusBadRequest || strings.Contains(out["error"].(string), settingsKey) {
		t.Fatalf("a bad key: %d %v", code, out)
	}
	// A turn token cannot set it.
	code, _ = sendAPI(t, ns.server, "abt_not-a-token", "PUT", "/api/v0/subscriptions/openrouter/key", map[string]any{"key": settingsKey})
	if code != http.StatusUnauthorized {
		t.Fatalf("a wrong token set the key: %d", code)
	}
	code, out = sendAPI(t, ns.server, ns.tok, "PUT", "/api/v0/subscriptions/openrouter/key", map[string]any{"key": " " + settingsKey + "\n"})
	if code != http.StatusNoContent || out != nil {
		t.Fatalf("set: %d %v", code, out)
	}
	code, raw = getAPI(t, ns.server, ns.tok, "/api/v0/subscriptions")
	keep(raw)
	if code != 200 || !strings.Contains(raw, `"openrouter_key":{"set":true,"set_at":`) {
		t.Fatalf("after: %d %s", code, raw)
	}
	// An agent whose variable is not set anywhere has a key now.
	code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_UNSET_KEY"})
	if code != http.StatusCreated || ag["key_set"] != true {
		t.Fatalf("create: %d %v", code, ag)
	}
	for _, p := range []string{"/api/v0/agents", "/api/v0/harnesses"} {
		code, raw = getAPI(t, ns.server, ns.tok, p)
		keep(raw)
		// The OpenRouter agent's key; access api's key_set is the vendor's.
		if code != 200 || !strings.Contains(raw, `"key_set":true`) || strings.Contains(raw, `"key_env":"AB_TEST_UNSET_KEY","key_set":false`) ||
			strings.Contains(raw, `"key_env":"OPENROUTER_API_KEY","key_set":false`) {
			t.Fatalf("%s: %d %s", p, code, raw)
		}
	}
	j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	if jj := waitJob(t, ns.server, ns.tok, j.ID); jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	if got := ns.or.seen(); len(got) != 1 || got[0] != "/v1/messages Bearer "+settingsKey {
		t.Fatalf("OpenRouter saw %v", got)
	}

	// A key set in Settings wins over the variable the agent names.
	code, envAg := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek env", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, envAg)
	}
	j2 := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": envAg["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	if jj := waitJob(t, ns.server, ns.tok, j2.ID); jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	if got := ns.or.seen(); len(got) != 2 || got[1] != "/v1/messages Bearer "+settingsKey {
		t.Fatalf("OpenRouter saw %v", got)
	}

	// Removed: the variable is read again, as before; the agent whose
	// variable is not set is refused, in words that say where to set it.
	if code, _ := sendAPI(t, ns.server, ns.tok, "DELETE", "/api/v0/subscriptions/openrouter/key", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	code, raw = getAPI(t, ns.server, ns.tok, "/api/v0/subscriptions")
	keep(raw)
	if !strings.Contains(raw, `"openrouter_key":{"set":false}`) {
		t.Fatalf("after remove: %s", raw)
	}
	j3 := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": envAg["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	if jj := waitJob(t, ns.server, ns.tok, j3.ID); jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	if got := ns.or.seen(); len(got) != 3 || got[2] != "/v1/messages Bearer fake-openrouter-key-for-tests" {
		t.Fatalf("OpenRouter saw %v", got)
	}
	j4 := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	if jj := waitJob(t, ns.server, ns.tok, j4.ID); jj.State != queue.Failed || !strings.Contains(string(jj.Result),
		`"refused":"set your OpenRouter key in Settings, or set AB_TEST_UNSET_KEY where control runs and restart agentbus"`) {
		t.Fatalf("with no key: job %s %s", jj.State, jj.Result)
	}
	if len(ns.or.seen()) != 3 {
		t.Fatalf("a call went out with no key: %v", ns.or.seen())
	}

	// The key is in no reply, no log, no record and no agent's environment.
	for _, r := range replies {
		if strings.Contains(r, settingsKey) {
			t.Fatalf("a reply holds the key: %s", r)
		}
	}
	for _, svc := range []*service{ns.server, ns.runner} {
		recs, _ := svc.store.After(0, store.Filter{}, 100000)
		for _, r := range recs {
			if strings.Contains(string(r.Data), settingsKey) {
				t.Fatalf("the record holds the key: %s", r.Data)
			}
		}
	}
	time.Sleep(100 * time.Millisecond) // the logs' last lines
	for _, home := range []string{ns.runnerHome, filepath.Dir(ns.report)} {
		filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), settingsKey) {
					t.Fatalf("%s holds the key", p)
				}
			}
			return nil
		})
	}
	// Removed, no file on control holds it: not the service log, not a
	// database, not the subscriptions' folder.
	filepath.WalkDir(ns.serverHome, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), settingsKey) {
			t.Fatalf("%s holds the key", p)
		}
		return nil
	})
}
