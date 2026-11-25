//go:build integration

package main

// An OpenCode agent on a provider (harness.Providers) under agentbus up: its
// Chat Completions call goes through the service's /p/<name> path and
// control's relay, which puts the provider's key set in Settings on it. The
// call is recorded with the provider's own
// charge as its cost. The child holds only a placeholder, and the key is in
// no reply, no record and no file but its own. A provider with a key that is
// not set refuses the turn before it starts. Every key here is fake.

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/store"
)

// chatVendor is a fake Chat Completions API: it keeps each call's path and
// Authorization, and answers with usage carrying xAI's cost_in_usd_ticks.
type chatVendor struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newChatVendor(t *testing.T) *chatVendor {
	v := &chatVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		v.mu.Lock()
		v.auths = append(v.auths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"grok-4.7","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"hello from the fake"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":200,"total_tokens":300,"cost_in_usd_ticks":37756000}}`)
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *chatVendor) seen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.auths...)
}

func TestUpProviderKeyOnRecordedCalls(t *testing.T) {
	const xaiKey = "xai-fake-up-key-0000111122223333444455556666"
	cases := []struct {
		provider, model string
		key             string // set in Settings; "" sets none
		token           string // what the provider gets in Authorization
		refused         string // the turn is refused with this, before it starts
	}{
		{provider: "xai", model: "grok-4.7", key: xaiKey, token: "Bearer " + xaiKey},
		{provider: "xai", model: "grok-4.7", refused: "set your xAI API key in Settings"},
	}
	for _, tc := range cases {
		name := tc.provider
		if tc.refused != "" {
			name += "/no-key"
		}
		t.Run(name, func(t *testing.T) {
			vendor := newChatVendor(t)
			home := t.TempDir()
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTBUS_FAKE_AGENT", "1")
			t.Setenv("OPENCODE_CONFIG_CONTENT", "")
			svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
				chatgpt: mustURL("http://127.0.0.1:1"), providers: map[string]*url.URL{tc.provider: mustURL(vendor.URL)}, logf: t.Logf,
				lookPath: func(string) (string, error) { return self, nil }, noReplay: true,
				heartbeat: 100 * time.Millisecond, turnWait: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(svc.stop)
			tok := readToken(t, home)
			if tc.key != "" {
				if code, _ := sendAPI(t, svc, tok, "PUT", "/api/v0/subscriptions/"+tc.provider+"/key", map[string]any{"key": tc.key}); code != http.StatusNoContent {
					t.Fatalf("set the key: %d", code)
				}
			}
			code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "OpenCode on " + tc.provider,
				"harness": "opencode", "access": tc.provider, "model": tc.model})
			if code != http.StatusCreated {
				t.Fatalf("agent: %d %v", code, ag)
			}
			report, release := filepath.Join(t.TempDir(), "report"), filepath.Join(t.TempDir(), "release")
			t.Setenv("AGENTBUS_FAKE_OUT", report)
			t.Setenv("AGENTBUS_FAKE_WAIT", release)
			t.Setenv("AGENTBUS_FAKE_EXIT", "0")
			code, out, raw := postRun(t, svc, tok, map[string]any{"agent": ag["id"], "folder": t.TempDir(), "prompt": "hi"})
			if tc.refused != "" {
				if code != http.StatusBadRequest || !strings.Contains(raw, tc.refused) {
					t.Fatalf("a turn with no key: %d %s", code, raw)
				}
				if got := vendor.seen(); len(got) != 0 {
					t.Fatalf("the provider was called: %v", got)
				}
				return
			}
			if code != http.StatusAccepted {
				t.Fatalf("turn: %d %s", code, raw)
			}
			session, turn := out["session"].(string), out["turn"].(string)
			waitFor(t, "the running turn's report", func() bool { _, err := os.Stat(report); return err == nil })
			env := envOf(t, report)
			placeholder := env[providerKeyEnv]
			if !strings.HasPrefix(placeholder, jobs.StandInPrefix) {
				t.Fatalf("the agent's placeholder: %q", placeholder)
			}
			// The run's OpenCode config: a provider of its own at the
			// service's path for the provider, reading the placeholder's
			// variable, on the agent's model.
			var cfg struct {
				Provider map[string]struct {
					Options struct{ BaseURL, APIKey string }
				}
				Model string
			}
			if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
				t.Fatalf("the run's config: %v", err)
			}
			p := cfg.Provider["agentbus_"+tc.provider]
			if cfg.Model != "agentbus_"+tc.provider+"/"+tc.model || p.Options.APIKey != "{env:"+providerKeyEnv+"}" ||
				!strings.HasSuffix(p.Options.BaseURL, "/s/"+session+"/t/"+turn+"/p/"+tc.provider+"/v1") {
				t.Fatalf("the run's config: %s", env["OPENCODE_CONFIG_CONTENT"])
			}
			req, _ := http.NewRequest("POST", p.Options.BaseURL+"/chat/completions",
				strings.NewReader(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+placeholder)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			reply, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("the model call: %d %s", resp.StatusCode, reply)
			}
			os.WriteFile(release, nil, 0o600)
			waitClosed(t, svc.store, session)

			if got := vendor.seen(); len(got) != 1 || got[0] != "POST /v1/chat/completions "+tc.token {
				t.Fatalf("the provider saw %v", got)
			}
			// Recorded, with the provider's own charge: 37756000 ticks.
			recs, _ := svc.store.After(0, store.Filter{}, 100000)
			var priced int
			for _, r := range recs {
				var e struct {
					Kind, Session string
					CostUSD       *float64 `json:"cost_usd"`
					CostComplete  *bool    `json:"cost_complete"`
				}
				json.Unmarshal(r.Data, &e)
				if e.Kind == "model.responded" && e.Session == session {
					if e.CostUSD == nil || *e.CostUSD != 0.0037756 || e.CostComplete == nil || !*e.CostComplete {
						t.Errorf("model.responded: %s", r.Data)
					}
					priced++
				}
			}
			if priced != 1 {
				t.Fatalf("%d model.responded, want 1", priced)
			}
			// No key in the reply, the agent's environment, the record or a
			// file under the home but its own.
			if strings.Contains(string(reply), tc.key) {
				t.Fatalf("the reply holds the key: %s", reply)
			}
			for name, v := range env {
				if strings.Contains(v, tc.key) {
					t.Fatalf("the agent's %s holds the key", name)
				}
			}
			for _, r := range recs {
				if strings.Contains(string(r.Data), tc.key) {
					t.Fatalf("the record holds the key: %s", r.Data)
				}
			}
			time.Sleep(100 * time.Millisecond) // the log's last lines
			filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || strings.HasSuffix(p, tc.provider+".key.json") {
					return nil
				}
				if b, _ := os.ReadFile(p); strings.Contains(string(b), tc.key) {
					t.Fatalf("%s holds the key", p)
				}
				return nil
			})
		})
	}
}
