//go:build integration

package main

// An agent on its vendor's API key under agentbus up: the key set in
// Settings goes only on the calls that vendor's adapter records (the same
// rule as a sign-in, gateway/recorded.go). Each tool's model call, as it was
// measured (claude 2.1.321, codex 0.167.0, agy 1.5.8, 2026-11-13), gets the
// key in the header the vendor reads and is recorded; any other path,
// method or query gets 403, reaches no vendor and is recorded as
// call.refused; a reply in a format the adapter does not read is not passed
// on (502). No key is in a reply, the record or a file under the home.
// Every key here is fake.

import (
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/store"
)

func TestUpAPIKeyOnlyOnRecordedCalls(t *testing.T) {
	keys := map[string]string{
		"anthropic": "sk-ant-api03-fake-up-key-0000111122223333",
		"openai":    "sk-proj-fake-up-key-4444555566667777",
		"gemini":    "AIzaFakeUpKey00001111222233334444555",
	}
	const geminiPath = "/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent"
	tools := []struct {
		harness, key string
		// placeholder is the variable that holds the agent's placeholder,
		// and header where the agent sends it.
		placeholder, header string
		// call is the model call the tool makes; seen how the vendor gets
		// it: path, then Authorization, X-Api-Key and X-Goog-Api-Key.
		call, seen string
		// refused are other calls with the placeholder: method, path and
		// query, and the call each is recorded as.
		refused map[string]string
		// bare is a call that goes to the vendor with no credential at
		// all, sent with the placeholder, and is recorded as vendor.called.
		bare string
	}{
		{"claude", "anthropic", "ANTHROPIC_AUTH_TOKEN", "Authorization", "POST /v1/messages?beta=true",
			"/v1/messages |" + keys["anthropic"] + "|",
			map[string]string{
				"POST /v1/messages/count_tokens?beta=true": "POST /v1/messages/count_tokens",
				"GET /v1/models":                  "GET /v1/models",
				"POST /v1/messages?beta=true&x=1": "POST /v1/messages",
				"DELETE /v1/files/file_1":         "DELETE /v1/files/file_1",
				"POST /v1/messages/batches":       "POST /v1/messages/batches",
			}, "HEAD /api/hello"},
		{"codex", "openai", "AGENTBUS_OPENAI_TOKEN", "Authorization", "POST /v1/responses",
			"/v1/responses Bearer " + keys["openai"] + "||",
			map[string]string{
				"POST /v1/responses/compact": "POST /v1/responses/compact",
				"GET /v1/models":             "GET /v1/models",
				"POST /v1/files":             "POST /v1/files",
				"GET /v1/responses/resp_1":   "GET /v1/responses/resp_1",
				"POST /v1/responses?x=1":     "POST /v1/responses",
			}, ""},
		{"agy", "gemini", "GEMINI_API_KEY", "X-Goog-Api-Key", "POST " + geminiPath + "?alt=sse",
			geminiPath + " ||" + keys["gemini"],
			map[string]string{
				"POST " + geminiPath + "?alt=proto":                         "POST " + geminiPath,
				"POST " + geminiPath + "?alt=sse&key=x":                     "POST " + geminiPath,
				"POST /v1beta/models/gemini-3.1-pro-preview:countTokens":    "POST /v1beta/models/gemini-3.1-pro-preview:countTokens",
				"GET /v1beta/models":                                        "GET /v1beta/models",
				"POST /v1beta/cachedContents":                               "POST /v1beta/cachedContents",
				"POST /v1beta/files":                                        "POST /v1beta/files",
				"GET /v1beta/models/gemini-3.1-pro-preview:generateContent": "GET /v1beta/models/gemini-3.1-pro-preview:generateContent",
			}, ""},
	}
	for _, tc := range tools {
		t.Run(tc.harness, func(t *testing.T) {
			vendor := newFakeVendor(t)
			home := t.TempDir()
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTBUS_FAKE_AGENT", "1")
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", "")
			t.Setenv("OPENAI_BASE_URL", "")
			connectFake(t, home)
			svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
				chatgpt: mustURL("http://127.0.0.1:1"), apiKeys: map[string]*url.URL{tc.harness: mustURL(vendor.URL)}, logf: t.Logf,
				lookPath: func(string) (string, error) { return self, nil }, noReplay: true,
				heartbeat: 100 * time.Millisecond, turnWait: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(svc.stop)
			tok := readToken(t, home)
			var replies []string
			if code, _ := sendAPI(t, svc, tok, "PUT", "/api/v0/subscriptions/"+tc.key+"/key", map[string]any{"key": keys[tc.key]}); code != http.StatusNoContent {
				t.Fatalf("set the key: %d", code)
			}
			code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": tc.harness + " on its key", "harness": tc.harness, "access": "api"})
			if code != http.StatusCreated || ag["key_set"] != true {
				t.Fatalf("agent: %d %v", code, ag)
			}
			report, release := filepath.Join(t.TempDir(), "report"), filepath.Join(t.TempDir(), "release")
			t.Setenv("AGENTBUS_FAKE_OUT", report)
			t.Setenv("AGENTBUS_FAKE_WAIT", release)
			t.Setenv("AGENTBUS_FAKE_EXIT", "0")
			code, out, raw := postRun(t, svc, tok, map[string]any{"agent": ag["id"], "folder": t.TempDir(), "prompt": "hi"})
			if code != http.StatusAccepted {
				t.Fatalf("turn: %d %s", code, raw)
			}
			session, turn := out["session"].(string), out["turn"].(string)
			waitFor(t, "the running turn's report", func() bool { _, err := os.Stat(report); return err == nil })
			env := envOf(t, report)
			placeholder := env[tc.placeholder]
			if !strings.HasPrefix(placeholder, "abr_") {
				t.Fatalf("the agent's placeholder: %q", placeholder)
			}
			base := "http://" + svc.addr + "/s/" + session + "/t/" + turn + "/key/" + tc.harness
			send := func(call string) (int, string) {
				method, target, _ := strings.Cut(call, " ")
				req, _ := http.NewRequest(method, base+target, strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}],"contents":[{"role":"user","parts":[{"text":"x"}]}]}`))
				if tc.header == "Authorization" {
					req.Header.Set("Authorization", "Bearer "+placeholder)
				} else {
					req.Header.Set(tc.header, placeholder)
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				replies = append(replies, string(b))
				return resp.StatusCode, string(b)
			}
			if tc.bare != "" {
				if code, b := send(tc.bare); code != http.StatusOK {
					t.Errorf("%s: %d %s", tc.bare, code, b)
				}
			}
			for call := range tc.refused {
				if code, b := send(call); code != http.StatusForbidden || !strings.Contains(b, "control adds a credential only to the calls") {
					t.Errorf("%s: %d %s", call, code, b)
				}
			}
			// The tool's own model call: the key, in its vendor's header.
			if code, b := send(tc.call); code != http.StatusOK {
				t.Fatalf("the model call: %d %s", code, b)
			}
			// The same call answered in a format the adapter does not read:
			// not passed on.
			vendor.mu.Lock()
			vendor.ctype = "application/x-protobuf"
			vendor.mu.Unlock()
			if code, b := send(tc.call); code != http.StatusBadGateway || !strings.Contains(b, "format the adapter does not read") {
				t.Errorf("an unreadable reply: %d %s", code, b)
			}
			os.WriteFile(release, nil, 0o600)
			waitClosed(t, svc.store, session)

			// The vendor saw the bare call first, with no credential, then
			// the model call twice, with the key, and nothing else.
			got := vendor.keysSeen()
			auths := vendor.seen()
			if tc.bare != "" {
				_, path, _ := strings.Cut(tc.bare, " ")
				if len(got) == 0 || got[0] != path+" |" || auths[0] != path+" " {
					t.Fatalf("the bare call: the vendor saw %v %v", got, auths)
				}
				got, auths = got[1:], auths[1:]
			}
			if len(got) != 2 {
				t.Fatalf("the vendor saw %v", got)
			}
			for i := range got {
				path, auth, _ := strings.Cut(auths[i], " ")
				_, xkeys, _ := strings.Cut(got[i], " ")
				if s := path + " " + auth + "|" + xkeys; s != tc.seen {
					t.Errorf("the vendor saw %q, want %q", s, tc.seen)
				}
			}
			// Recorded: the model call, each refusal, and the reply not
			// passed on.
			recorded := map[string]int{}
			var requested, bad502, bare int
			for _, e := range storedEvents(t, svc.store) {
				switch {
				case e.Kind == "vendor.called" && e.Call == tc.bare && e.Session == session && e.Status == http.StatusOK:
					bare++
				case e.Kind == "call.refused" && e.Status == http.StatusBadGateway:
					bad502++
				case e.Kind == "call.refused":
					if e.Session != session || e.Error == "" {
						t.Errorf("call.refused %+v", e)
					}
					recorded[e.Call]++
				case e.Kind == "model.requested" && e.Session == session:
					requested++
				}
			}
			want := map[string]int{}
			for _, call := range tc.refused {
				want[call]++
			}
			for call, n := range want {
				if recorded[call] != n {
					t.Errorf("%s: %d call.refused, want %d (all: %v)", call, recorded[call], n, recorded)
				}
			}
			if requested != 2 || bad502 != 1 {
				t.Errorf("model.requested %d, want 2; refused replies %d, want 1", requested, bad502)
			}
			if tc.bare != "" && bare != 1 {
				t.Errorf("%s: %d vendor.called, want 1", tc.bare, bare)
			}

			// No key in a reply, the agent's environment, the record or a
			// file under the home.
			k := keys[tc.key]
			for _, r := range replies {
				if strings.Contains(r, k) {
					t.Fatalf("a reply holds the key: %s", r)
				}
			}
			for name, v := range env {
				if strings.Contains(v, k) {
					t.Fatalf("the agent's %s holds the key", name)
				}
			}
			recs, _ := svc.store.After(0, store.Filter{}, 100000)
			for _, r := range recs {
				if strings.Contains(string(r.Data), k) {
					t.Fatalf("the record holds the key: %s", r.Data)
				}
			}
			time.Sleep(100 * time.Millisecond) // the log's last lines
			filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || strings.HasSuffix(p, tc.key+".key.json") {
					return nil
				}
				if b, _ := os.ReadFile(p); strings.Contains(string(b), k) {
					t.Fatalf("%s holds the key", p)
				}
				return nil
			})
		})
	}
}
