//go:build integration

package main

// A runner that holds no secret (agentbus runner, -gateway server by
// default): its agent gets the turn's token, minted by the server, as its
// model credential and as its API token; the server's relay swaps the real
// credential in on the way out. The turn's record reaches the server while
// it runs. Every credential here is fake.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A fake vendor: it keeps each request's path and Authorization, and answers
// an Anthropic Messages reply.
type fakeVendor struct {
	*httptest.Server
	mu       sync.Mutex
	auths    []string
	accounts []string // each request's ChatGPT-Account-ID
	keys     []string // each request's path, X-Api-Key and X-Goog-Api-Key
	// stream, when set, is the reply instead: written as server-sent events
	// with no Content-Type, as chatgpt.com answers codex.
	stream []byte
	// ctype, when set, is the reply's Content-Type instead, its body the
	// same: a format the adapter may not read.
	ctype string
}

func newFakeVendor(t *testing.T) *fakeVendor {
	v := &fakeVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		v.mu.Lock()
		v.auths = append(v.auths, r.URL.Path+" "+r.Header.Get("Authorization"))
		v.accounts = append(v.accounts, r.Header.Get("ChatGPT-Account-ID"))
		v.keys = append(v.keys, r.URL.Path+" "+r.Header.Get("X-Api-Key")+"|"+r.Header.Get("X-Goog-Api-Key"))
		stream, ctype := v.stream, v.ctype
		v.mu.Unlock()
		if stream != nil {
			w.Header()["Content-Type"] = nil // no type at all, not a sniffed one
			w.Write(stream)
			return
		}
		if ctype == "" {
			ctype = "application/json"
		}
		w.Header().Set("Content-Type", ctype)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello from the fake"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *fakeVendor) seen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.auths...)
}

type noSecretsSetup struct {
	server, runner *service
	tok            string
	ref, connector string
	serverHome     string
	runnerHome     string
	anth, or, chat *fakeVendor
	cc             *fakeVendor // agy's Code Assist
	// keyed are the vendor APIs of access api, by harness.
	keyed           map[string]*fakeVendor
	report, release string
}

// keysSeen are each request's path, X-Api-Key and X-Goog-Api-Key.
func (v *fakeVendor) keysSeen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.keys...)
}

func startNoSecrets(t *testing.T) *noSecretsSetup {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("AB_TEST_OR_KEY", "") // in this process only the server's getenv has it
	ns := &noSecretsSetup{anth: newFakeVendor(t), or: newFakeVendor(t), chat: newFakeVendor(t), cc: newFakeVendor(t), serverHome: t.TempDir(),
		keyed: map[string]*fakeVendor{"claude": newFakeVendor(t), "codex": newFakeVendor(t), "agy": newFakeVendor(t)}}
	apiKeys := map[string]*url.URL{}
	for h, v := range ns.keyed {
		apiKeys[h] = mustURL(v.URL)
	}
	look := func(string) (string, error) { return self, nil }
	// The server's own environment holds the API key.
	serverEnv := func(k string) string {
		if k == "AB_TEST_OR_KEY" {
			return "fake-openrouter-key-for-tests"
		}
		return os.Getenv(k)
	}
	ns.server, err = start(config{mode: modeServer, listen: "127.0.0.1:0", home: ns.serverHome, upstream: mustURL(ns.anth.URL),
		chatgpt: mustURL(ns.chat.URL), openrouter: mustURL(ns.or.URL), cloudCode: mustURL(ns.cc.URL), apiKeys: apiKeys, getenv: serverEnv, logf: t.Logf,
		heartbeat: 100 * time.Millisecond, turnWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ns.server.stop)
	ns.tok = readToken(t, ns.serverHome)
	folder, runnerHome := t.TempDir(), t.TempDir()
	ns.runnerHome = runnerHome
	fr, err := folders.Open(filepath.Join(runnerHome, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ns.ref, ns.connector = folders.Ref(folder), fr.Connector()
	// The runner's upstreams name nothing: with no secrets it sends every
	// call to the server's relay.
	signInRunner(t, "http://"+ns.server.addr, ns.tok, runnerHome, "far")
	ns.runner, err = start(config{mode: modeRunner, listen: "127.0.0.1:0", home: runnerHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), openrouter: mustURL("http://127.0.0.1:1"), logf: t.Logf, lookPath: look, noReplay: true,
		getenv: func(k string) string { return os.Getenv(k) },
		server: "http://" + ns.server.addr, runnerName: "far", folders: []string{folder}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ns.runner.stop)
	dir := t.TempDir()
	ns.report, ns.release = filepath.Join(dir, "report.json"), filepath.Join(dir, "release")
	t.Setenv("AGENTBUS_FAKE_OUT", ns.report)
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	return ns
}

// Under contain = "on" a runner's agent runs closed off: its model call still reaches the
// vendor through the runner's gateway and the server's relay, a host not in
// [network] allow is refused, and the refusal is in the server's record.
func TestRunnerAgentClosedOff(t *testing.T) {
	if !netjail.Supported {
		t.Skip("network control is Linux-only")
	}
	if err := netjail.Check(); err != nil {
		t.Skipf("this machine cannot close an agent off: %v", err)
	}
	ns := startNoSecrets(t)
	putPolicy(t, ns.server, ns.tok, "[network]\ncontain = \"on\"\n")
	t.Setenv("AGENTBUS_FAKE_NET", "http://blocked.example/")
	code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	jj := waitJob(t, ns.server, ns.tok, j.ID)
	if jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	if env := envOf(t, ns.report); !strings.HasPrefix(env["HTTPS_PROXY"], "http://127.0.0.1:") || env["NO_PROXY"] == "" {
		t.Fatalf("the agent's proxy: %q %q", env["HTTPS_PROXY"], env["NO_PROXY"])
	}
	if got := ns.or.seen(); len(got) != 1 {
		t.Fatalf("OpenRouter saw %v", got)
	}
	turn := jj.Turn
	waitFor(t, "network.refused in the server's record", func() bool {
		recs, _ := ns.server.store.After(0, store.Filter{Turn: turn}, 1000)
		for _, r := range recs {
			var e struct{ Host, Error string }
			if r.Kind == "network.refused" && json.Unmarshal(r.Data, &e) == nil {
				if e.Host != "blocked.example:80" || !strings.Contains(e.Error, "not in [network] allow") {
					t.Fatalf("network.refused: %+v", e)
				}
				return true
			}
		}
		return false
	})
}

func envOf(t *testing.T, report string) map[string]string {
	t.Helper()
	var rep fakeReport
	b, err := os.ReadFile(report)
	if err != nil || json.Unmarshal(b, &rep) != nil {
		t.Fatalf("no report: %v", err)
	}
	env := map[string]string{}
	for _, kv := range rep.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

func TestRunnerHoldsNoKey(t *testing.T) {
	ns := startNoSecrets(t)
	// A resume id, so the session can take a next turn.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-2"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_WAIT", ns.release)
	// Spaces and <, > and &, which JSON's own encoding would change on the
	// way to control's record.
	t.Setenv("AGENTBUS_FAKE_BODY", `{"model": "claude-x", "messages": [{"role": "user", "content": "fake turn <system-reminder> & more"}]}`)
	code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	// While the turn runs, the server's record holds it and its model call.
	var turn, exchange string
	waitFor(t, "the running turn in the server's record", func() bool {
		jj := waitState(t, ns.server, ns.tok, j.ID)
		if len(jj.Attempts) == 0 {
			return false
		}
		turn = jj.Turn
		recs, _ := ns.server.store.After(0, store.Filter{Turn: turn}, 1000)
		for _, r := range recs {
			var e struct{ Exchange string }
			if r.Kind == "model.responded" && json.Unmarshal(r.Data, &e) == nil && e.Exchange != "" {
				exchange = e.Exchange
			}
		}
		return exchange != ""
	})
	waitFor(t, "the call's context on the server", func() bool {
		code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/turns/"+turn+"/calls/"+exchange)
		return code == 200 && strings.Contains(raw, "fake turn") && strings.Contains(raw, "hello from the fake")
	})
	if st := waitState(t, ns.server, ns.tok, j.ID); st.State != queue.Running {
		t.Fatalf("the job is %s while its turn waits", st.State)
	}
	// The running turn's token is a turn token: it never reads the
	// subscriptions or the settings.
	for _, path := range []string{"/api/v0/subscriptions", "/api/v0/settings/permissions"} {
		if code, _ := getAPI(t, ns.server, envOf(t, ns.report)["AGENTBUS_TOKEN"], path); code != http.StatusForbidden {
			t.Fatalf("a turn token on %s: %d", path, code)
		}
	}
	// The vendor got the server's key; the agent never had it.
	env := envOf(t, ns.report)
	placeholder := env["ANTHROPIC_AUTH_TOKEN"]
	if !strings.HasPrefix(placeholder, "abr_") || env["AB_TEST_OR_KEY"] != placeholder || env["AGENTBUS_TOKEN"] != placeholder ||
		env["AGENTBUS_URL"] != "http://"+ns.server.addr {
		t.Fatalf("the agent's credential and API: %q %q %q %q", placeholder, env["AB_TEST_OR_KEY"], env["AGENTBUS_TOKEN"], env["AGENTBUS_URL"])
	}
	b, _ := os.ReadFile(ns.report)
	if strings.Contains(string(b), "fake-openrouter-key-for-tests") {
		t.Fatal("the agent's environment holds the key")
	}
	if got := ns.or.seen(); len(got) != 1 || got[0] != "/v1/messages Bearer fake-openrouter-key-for-tests" {
		t.Fatalf("OpenRouter saw %v", got)
	}
	os.WriteFile(ns.release, nil, 0o600)
	if jj := waitJob(t, ns.server, ns.tok, j.ID); jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	// The call is in the server's record once, from the relay's own copy:
	// the runner sent none of its call's events. Both name the call by the
	// runner's exchange.
	runnerIDs := map[string]bool{}
	recs, _ := ns.runner.store.After(0, store.Filter{Turn: turn}, 1000)
	for _, r := range recs {
		var e event.Event
		if json.Unmarshal(r.Data, &e) == nil && e.Exchange == exchange {
			runnerIDs[e.ID] = true
		}
	}
	if len(runnerIDs) == 0 {
		t.Fatalf("the runner's record has no call %s", exchange)
	}
	counts := map[string]int{}
	recs, _ = ns.server.store.After(0, store.Filter{Turn: turn}, 1000)
	for _, r := range recs {
		var e event.Event
		if json.Unmarshal(r.Data, &e) != nil || e.Exchange == "" {
			continue
		}
		if runnerIDs[e.ID] {
			t.Fatalf("the runner sent its copy of a call's event: %s", r.Data)
		}
		counts[string(e.Kind)]++
	}
	if counts["model.requested"] != 1 || counts["model.responded"] != 1 {
		t.Fatalf("the server's record of the call: %v", counts)
	}
	// Once the attempt ended, its token adds no credential and calls no API.
	req, _ := http.NewRequest("POST", "http://"+ns.server.addr+"/relay/openrouter/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(ns.or.seen()) != 1 {
		t.Fatalf("an ended turn's token: %d, %d calls", resp.StatusCode, len(ns.or.seen()))
	}
	if code, _ := getAPI(t, ns.server, placeholder, "/api/v0/sessions"); code != http.StatusUnauthorized {
		t.Fatalf("an ended turn's token on the API: %d", code)
	}
	// A forged token is refused.
	req, _ = http.NewRequest("POST", "http://"+ns.server.addr+"/relay/openrouter/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSuffix(placeholder, placeholder[len(placeholder)-4:])+"0000")
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a forged token: %v %v", err, resp)
	}
	resp.Body.Close()
	// A next turn names its agent, and gets that saved agent's key.
	t.Setenv("AGENTBUS_FAKE_WAIT", "")
	code, out, raw := postRun(t, ns.server, ns.tok, map[string]any{"session": j.Session, "agent": j.Spec.AgentID, "prompt": "again"})
	if code != 202 {
		t.Fatalf("next turn: %d %s", code, raw)
	}
	_ = out
	waitFor(t, "the next turn's call", func() bool { return len(ns.or.seen()) == 2 })
	if got := ns.or.seen(); got[1] != "/v1/messages Bearer fake-openrouter-key-for-tests" {
		t.Fatalf("OpenRouter saw %v", got)
	}
	// The whole turn is in the server's record, without the runner's path.
	if list, _ := ns.server.store.Turns(store.TurnQuery{Turn: turn, Limit: 1}); len(list.Turns) != 1 || list.Turns[0].Folder != "" ||
		list.Turns[0].ModelCalls != 1 || list.Turns[0].State != "finished" {
		t.Fatalf("the server's record of the turn: %+v", list.Turns)
	}
}

// A subscription connected on control is used for a turn on an approved
// runner, with no setting: every runner is the person's own. With nothing
// connected the relay refuses, and the vendor sees nothing.
func TestRunnerUsesConnectedSubscription(t *testing.T) {
	ns := startNoSecrets(t)
	run := func() queue.Job {
		j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": "claude", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
		return waitJob(t, ns.server, ns.tok, j.ID)
	}
	// Nothing connected: refused before the agent starts, in plain words.
	if j := run(); j.State != queue.Failed || !strings.Contains(string(j.Result), `"refused":"connect your claude subscription in Settings"`) {
		t.Fatalf("with nothing connected: job %s %s", j.State, j.Result)
	}
	if got := ns.anth.seen(); len(got) != 0 {
		t.Fatalf("with nothing connected the vendor saw %v", got)
	}
	var list struct {
		Subscriptions []struct {
			Harness   string
			Connected bool
		}
	}
	if code := callAPI(t, ns.server, ns.tok, "GET", "/api/v0/subscriptions", nil, &list); code != 200 ||
		len(list.Subscriptions) != 3 || list.Subscriptions[0].Harness != "agy" || list.Subscriptions[0].Connected ||
		list.Subscriptions[1].Harness != "claude" || list.Subscriptions[1].Connected ||
		list.Subscriptions[2].Harness != "codex" || list.Subscriptions[2].Connected {
		t.Fatalf("subscriptions: %d %+v", code, list)
	}
	// Connected: the vendor gets the person's token, the agent a placeholder.
	subs := &credentials.Subscriptions{Dir: filepath.Join(ns.serverHome, subscriptionsDir)}
	if err := subs.Put("claude", "fake-subscription-token-for-tests", time.Hour); err != nil {
		t.Fatal(err)
	}
	if j := run(); j.State != queue.Finished {
		t.Fatalf("job %s %s", j.State, j.Result)
	}
	if got := ns.anth.seen(); len(got) != 1 || got[0] != "/v1/messages Bearer fake-subscription-token-for-tests" {
		t.Fatalf("the vendor saw %v", got)
	}
	env := envOf(t, ns.report)
	if !strings.HasPrefix(env["CLAUDE_CODE_OAUTH_TOKEN"], "abr_") {
		t.Fatalf("the agent's sign-in: %q", env["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	b, _ := os.ReadFile(ns.report)
	if strings.Contains(string(b), "fake-subscription-token-for-tests") {
		t.Fatal("the agent's environment holds the token")
	}
	if code := callAPI(t, ns.server, ns.tok, "GET", "/api/v0/subscriptions", nil, &list); code != 200 || list.Subscriptions[1].Harness != "claude" || !list.Subscriptions[1].Connected {
		t.Fatalf("subscriptions: %d %+v", code, list)
	}
}

// codex on its ChatGPT sign-in, connected on the server: the runner's codex
// gets a placeholder as its key and the runner's gateway as its provider; the
// relay sends chatgpt.com the sign-in's token and account. The sign-in goes
// to its own vendor only: the same turn's token on the Anthropic path gets
// nothing. chatgpt.com streams its reply with no Content-Type, and the
// relay passes it on so: the runner's gateway records the reply's text.
func TestRunnerUsesConnectedCodexSubscription(t *testing.T) {
	ns := startNoSecrets(t)
	sample, err := os.ReadFile(filepath.Join("..", "..", "adapter", "openai", "testdata", "plain_response.sse"))
	if err != nil {
		t.Fatal(err)
	}
	ns.chat.mu.Lock()
	ns.chat.stream = sample
	ns.chat.mu.Unlock()
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	t.Setenv("AGENTBUS_FAKE_THREAD", "fake-thread-1")
	t.Setenv("AGENTBUS_FAKE_WAIT", ns.release)
	run := func() queue.Job {
		j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": "codex", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
		return j
	}
	// Not connected: refused before codex starts, in control's words, and
	// the vendor sees nothing.
	os.WriteFile(ns.release, nil, 0o600)
	t.Setenv("AGENTBUS_FAKE_EXIT", "1")
	refused := waitJob(t, ns.server, ns.tok, run().ID)
	if got := ns.chat.seen(); len(got) != 0 {
		t.Fatalf("with nothing connected the vendor saw %v", got)
	}
	waitFor(t, "the refused turn's error", func() bool {
		code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/turns/"+refused.Turn)
		return code == 200 && strings.Contains(raw, `"state":"failed"`) && strings.Contains(raw, `connect your codex subscription in Settings`)
	})
	t.Setenv("AGENTBUS_FAKE_EXIT", "")
	os.Remove(ns.release)
	os.Remove(ns.report)
	subs := subscriptionStore(ns.serverHome)
	if err := subs.Keep("codex", credentials.Signin{Token: "fake-chatgpt-access-for-tests", Refresh: "fake-chatgpt-refresh-for-tests",
		Account: "fake-account-1", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	j := run()
	waitFor(t, "the codex call at the vendor", func() bool { return len(ns.chat.seen()) == 1 })
	if got := ns.chat.seen(); got[0] != "/backend-api/codex/responses Bearer fake-chatgpt-access-for-tests" || ns.chat.accounts[0] != "fake-account-1" {
		t.Fatalf("the vendor saw %v %v", got, ns.chat.accounts)
	}
	// The agent writes its report after its call, before it waits.
	waitFor(t, "the running turn's report", func() bool { _, err := os.Stat(ns.report); return err == nil })
	env := envOf(t, ns.report)
	placeholder := env["AGENTBUS_CODEX_TOKEN"]
	if !strings.HasPrefix(placeholder, "abr_") {
		t.Fatalf("the agent's key: %q", placeholder)
	}
	// codex runs in the session's own home, and no sign-in file is put
	// there: its key is the placeholder in its environment.
	if want := filepath.Join(ns.runnerHome, homesDir, j.Session, ".codex"); env["CODEX_HOME"] != want {
		t.Fatalf("CODEX_HOME %q, want %q", env["CODEX_HOME"], want)
	}
	if _, err := os.Stat(filepath.Join(env["CODEX_HOME"], "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("a sign-in file in the session's codex home: %v", err)
	}
	b, _ := os.ReadFile(ns.report)
	if strings.Contains(string(b), "fake-chatgpt") || strings.Contains(string(b), "fake-account-1") {
		t.Fatal("the agent's environment or arguments hold the sign-in")
	}
	// The streamed reply is in the server's record: its text, its usage,
	// and the turn's reply.
	waitFor(t, "the codex reply in the server's record", func() bool {
		recs, _ := ns.server.store.After(0, store.Filter{Turn: j.Turn}, 1000)
		for _, r := range recs {
			var e event.Event
			if r.Kind == "model.responded" && json.Unmarshal(r.Data, &e) == nil {
				if e.Error != "" || e.Text != "pong" || e.Usage == nil {
					t.Fatalf("the codex reply recorded as %s", r.Data)
				}
				return true
			}
		}
		return false
	})
	waitFor(t, "the turn's reply", func() bool {
		code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/turns/"+j.Turn)
		return code == 200 && strings.Contains(raw, `"reply":"pong"`)
	})
	// The running turn's token on the Anthropic path: refused, nothing sent.
	req, _ := http.NewRequest("POST", "http://"+ns.server.addr+"/relay/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(ns.anth.seen()) != 0 {
		t.Fatalf("a codex turn's token on the Anthropic path: %d, %v", resp.StatusCode, ns.anth.seen())
	}
	// The refusal is in the server's record, as a call of the turn: the
	// relay records every call a running turn's token makes.
	waitFor(t, "the refused call in the server's record", func() bool {
		recs, _ := ns.server.store.After(0, store.Filter{Turn: j.Turn}, 1000)
		for _, r := range recs {
			var e event.Event
			if r.Kind == "model.responded" && json.Unmarshal(r.Data, &e) == nil && e.Vendor == "anthropic" {
				return e.Status == http.StatusForbidden && strings.Contains(e.Error, "control adds its subscription to no call on this path")
			}
		}
		return false
	})
	os.WriteFile(ns.release, nil, 0o600)
	jj := waitJob(t, ns.server, ns.tok, j.ID)
	if jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	var list struct {
		Subscriptions []struct {
			Harness           string
			Connected, Renews bool
		}
	}
	if code := callAPI(t, ns.server, ns.tok, "GET", "/api/v0/subscriptions", nil, &list); code != 200 || len(list.Subscriptions) != 3 ||
		list.Subscriptions[2].Harness != "codex" || !list.Subscriptions[2].Connected || !list.Subscriptions[2].Renews {
		t.Fatalf("subscriptions: %d %+v", code, list)
	}
}

// agy on its Google sign-in, connected on the server: agy's own home there
// holds it, and the relay adds its access token to the runner's Code Assist
// calls. The runner's agy starts signed in on a placeholder file in the
// session's own home; its call carries the placeholder, and the vendor sees
// the person's token. Only Code Assist's methods are relayed, and the
// sign-in goes to its own vendor only.
func TestRunnerUsesConnectedAgySubscription(t *testing.T) {
	ns := startNoSecrets(t)
	ns.cc.mu.Lock()
	ns.cc.stream = []byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4},"modelVersion":"gemini-3.8-flash","responseId":"r1"}}` + "\r\n\r\n")
	ns.cc.mu.Unlock()
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	t.Setenv("AGENTBUS_FAKE_AGY_BODY", `{"model":"gemini-3.8-flash","project":"p","requestId":"q","request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"conversation_id":"fake-conv-1","status":"SUCCESS","response":"pong"}`+"\n")
	t.Setenv("AGENTBUS_FAKE_WAIT", ns.release)
	run := func() queue.Job {
		return queueTurn(t, ns.server, ns.tok, map[string]any{"agent": "agy", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	}
	// Not connected: refused, and the vendor sees nothing.
	os.WriteFile(ns.release, nil, 0o600)
	t.Setenv("AGENTBUS_FAKE_EXIT", "1")
	refused := waitJob(t, ns.server, ns.tok, run().ID)
	if got := ns.cc.seen(); len(got) != 0 {
		t.Fatalf("with nothing connected the vendor saw %v", got)
	}
	waitFor(t, "the refused turn's error", func() bool {
		code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/turns/"+refused.Turn)
		return code == 200 && strings.Contains(raw, `"state":"failed"`) && strings.Contains(raw, `connect your agy subscription in Settings`)
	})
	t.Setenv("AGENTBUS_FAKE_EXIT", "")
	os.Remove(ns.release)
	os.Remove(ns.report)
	// Connected, as agy acp --login leaves it: a folder of control's own
	// that holds agy's file.
	subs := subscriptionStore(ns.serverHome)
	os.MkdirAll(subs.Dir, 0o700)
	login, err := os.MkdirTemp(subs.Dir, ".login-*")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(login, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, []byte(`{"token":{"access_token":"fake-agy-access-for-tests","refresh_token":"fake-agy-refresh-for-tests",`+
		`"expiry":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"},"auth_method":"consumer"}`), 0o600)
	if err := subs.Keep("agy", credentials.Signin{Home: login}); err != nil {
		t.Fatal(err)
	}
	j := run()
	waitFor(t, "the agy call at the vendor", func() bool { return len(ns.cc.seen()) == 1 })
	if got := ns.cc.seen(); got[0] != "/v1internal:streamGenerateContent Bearer fake-agy-access-for-tests" {
		t.Fatalf("the vendor saw %v", got)
	}
	waitFor(t, "the running turn's report", func() bool { _, err := os.Stat(ns.report); return err == nil })
	env := envOf(t, ns.report)
	home := filepath.Join(ns.runnerHome, homesDir, j.Session)
	if env["HOME"] != home || !strings.HasSuffix(env["CLOUD_CODE_URL"], "/s/"+j.Session+"/t/"+j.Turn) {
		t.Fatalf("HOME %q, CLOUD_CODE_URL %q", env["HOME"], env["CLOUD_CODE_URL"])
	}
	// The session's home holds the placeholder sign-in, readable by its
	// owner only: the turn's token, no refresh token.
	fi, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the placeholder file: %v %v", err, fi)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	var ph struct {
		Token  map[string]string `json:"token"`
		Method string            `json:"auth_method"`
	}
	if json.Unmarshal(b, &ph) != nil || !strings.HasPrefix(ph.Token["access_token"], "abr_") || ph.Token["refresh_token"] != "" || ph.Method != "consumer" {
		t.Fatalf("the placeholder file: %s", b)
	}
	rb, _ := os.ReadFile(ns.report)
	if strings.Contains(string(rb), "fake-agy") || strings.Contains(string(b), "fake-agy") {
		t.Fatal("the agent's environment, arguments or home hold the sign-in")
	}
	placeholder := ph.Token["access_token"]
	for _, tc := range []struct{ path, why string }{
		{"/relay/cloudcode/v1/projects", "a path outside Code Assist's methods"},
		{"/relay/anthropic/v1/messages", "the Anthropic path"},
	} {
		req, _ := http.NewRequest("POST", "http://"+ns.server.addr+tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+placeholder)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 4 || len(ns.cc.seen()) != 1 || len(ns.anth.seen()) != 0 {
			t.Fatalf("%s: %d, %v %v", tc.why, resp.StatusCode, ns.cc.seen(), ns.anth.seen())
		}
	}
	os.WriteFile(ns.release, nil, 0o600)
	jj := waitJob(t, ns.server, ns.tok, j.ID)
	if jj.State != queue.Finished {
		t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
	}
	waitFor(t, "the agy reply in the server's record", func() bool {
		recs, _ := ns.server.store.After(0, store.Filter{Turn: jj.Turn}, 1000)
		for _, r := range recs {
			var e event.Event
			if r.Kind == "model.responded" && json.Unmarshal(r.Data, &e) == nil {
				if e.Error != "" || e.Text != "pong" {
					t.Fatalf("the agy reply recorded as %s", r.Data)
				}
				return true
			}
		}
		return false
	})
	var list struct {
		Subscriptions []struct {
			Harness           string
			Connected, Renews bool
		}
	}
	if code := callAPI(t, ns.server, ns.tok, "GET", "/api/v0/subscriptions", nil, &list); code != 200 || len(list.Subscriptions) != 3 ||
		list.Subscriptions[0].Harness != "agy" || !list.Subscriptions[0].Connected || !list.Subscriptions[0].Renews {
		t.Fatalf("subscriptions: %d %+v", code, list)
	}
}

// An agent on a runner asks another agent through the server's API, with
// the token the server minted; the asked turn runs, and its result comes
// back to the asking session as its next turn, all in the server's record.
func TestRunnerAgentAsksAnother(t *testing.T) {
	ns := startNoSecrets(t)
	connectFake(t, ns.serverHome, "claude")
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	asked := filepath.Join(t.TempDir(), "asked")
	t.Setenv("AGENTBUS_FAKE_ASK", `{"agent":"claude","prompt":"answer me"}`)
	t.Setenv("AGENTBUS_FAKE_ASKED", asked)
	// A resume id, so the asking session can take its next turn.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-1"}`+"\n")
	j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": "claude", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "ask someone"})
	j = waitJob(t, ns.server, ns.tok, j.ID)
	b, err := os.ReadFile(asked)
	if err != nil || !strings.HasPrefix(string(b), "202 ") {
		t.Fatalf("the ask: %v %s", err, b)
	}
	// The result comes back as the asking session's next turn.
	defer func() {
		if t.Failed() {
			all, _ := ns.server.queue.List(queue.DefaultTeam, queue.Filter{}, 100)
			for _, x := range all {
				t.Logf("job %s %s %s session %s links %+v result %s", x.ID, x.Kind, x.State, x.Session, x.Links, x.Result)
			}
		}
	}()
	waitFor(t, "the result delivered to the asking session", func() bool {
		jobs, _ := ns.server.queue.List(queue.DefaultTeam, queue.Filter{Session: j.Session}, 100)
		for _, x := range jobs {
			if x.ID != j.ID && x.Links.ResultOf != nil && x.State == queue.Finished {
				return true
			}
		}
		return false
	})
	snap, err := ns.server.store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("the server's record holds %d sessions, want the asking one and the asked one", len(snap.Sessions))
	}
	// The asked turn says who asked it, as control's record holds it: the
	// asking job's session and turn, from the asking turn's token.
	for _, s := range snap.Sessions {
		if s.ID == j.Session {
			continue
		}
		list, err := ns.server.store.Turns(store.TurnQuery{Session: s.ID, Limit: 10})
		if err != nil || len(list.Turns) != 1 {
			t.Fatalf("the asked session's turns: %v %+v", err, list.Turns)
		}
		o := list.Turns[0].Origin
		if o == nil || o.Kind != event.OriginAsked || o.Session != j.Session || o.Turn != j.Turn {
			t.Errorf("the asked turn's origin is %+v, want asked by %s turn %s", o, j.Session, j.Turn)
		}
	}
}

// A turn that has not called its model yet is in the server's record
// already: what its start wrote is sent at once.
func TestRunnerTurnShownBeforeItsFirstCall(t *testing.T) {
	ns := startNoSecrets(t)
	connectFake(t, ns.serverHome, "claude")
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	t.Setenv("AGENTBUS_FAKE_WAIT", ns.release)
	j := queueTurn(t, ns.server, ns.tok, map[string]any{"agent": "claude", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"})
	waitFor(t, "the waiting turn in the server's record", func() bool {
		jj := waitState(t, ns.server, ns.tok, j.ID)
		if len(jj.Attempts) == 0 {
			return false
		}
		list, _ := ns.server.store.Turns(store.TurnQuery{Turn: jj.Turn, Limit: 1})
		return len(list.Turns) == 1 && list.Turns[0].State == "running"
	})
	os.WriteFile(ns.release, nil, 0o600)
	waitJob(t, ns.server, ns.tok, j.ID)
}

// A runner that holds no secrets runs each session in a home of its own:
// a session's next turn finds what its first left there, another session
// finds nothing of it, the runner user's own home gets nothing, and the
// home goes when the session is closed.
func TestRunnerHomePerSession(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(realHome, ".claude"))
	ns := startNoSecrets(t)
	connectFake(t, ns.serverHome, "claude")
	t.Setenv("AGENTBUS_FAKE_HOME", "1")
	// A resume id, so a session can take a next turn: the runner continues
	// by the vendor's resume, which reads the transcript in the home.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-1"}`+"\n")
	code, ag := sendAPI(t, ns.server, ns.tok, "POST", "/api/v0/agents", map[string]any{"name": "DeepSeek", "harness": "claude",
		"access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "AB_TEST_OR_KEY"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, ag)
	}
	turn := func(spec map[string]any) (queue.Job, fakeReport) {
		t.Helper()
		j := queueTurn(t, ns.server, ns.tok, spec)
		if jj := waitJob(t, ns.server, ns.tok, j.ID); jj.State != queue.Finished {
			t.Fatalf("job %s %s %s", jj.State, jj.Reason, jj.Result)
		}
		var rep fakeReport
		b, err := os.ReadFile(ns.report)
		if err != nil || json.Unmarshal(b, &rep) != nil {
			t.Fatalf("no report: %v", err)
		}
		return j, rep
	}
	homeOf := func(rep fakeReport) string {
		for _, kv := range rep.Env {
			if v, ok := strings.CutPrefix(kv, "HOME="); ok {
				return v
			}
		}
		return ""
	}
	first := map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "isolate": true}
	first["prompt"] = "first of A"
	a, repA := turn(first)
	homeA := homeOf(repA)
	if homeA != filepath.Join(ns.runnerHome, homesDir, a.Session) || len(repA.Seen) != 0 {
		t.Fatalf("session A's home %q, seen %q", homeA, repA.Seen)
	}
	if env := envOf(t, ns.report); env["CLAUDE_CONFIG_DIR"] != filepath.Join(homeA, ".claude") {
		t.Fatalf("claude's config folder: %q", env["CLAUDE_CONFIG_DIR"])
	}
	_, repA2 := turn(map[string]any{"session": a.Session, "agent": "claude", "prompt": "second of A"})
	if homeOf(repA2) != homeA || strings.Join(repA2.Seen, ",") != "first of A" {
		t.Fatalf("A's next turn: home %q, seen %q", homeOf(repA2), repA2.Seen)
	}
	first["prompt"] = "first of B"
	_, repB := turn(first)
	if homeOf(repB) == homeA || len(repB.Seen) != 0 {
		t.Fatalf("session B: home %q, seen %q", homeOf(repB), repB.Seen)
	}
	// A review runs in a home of its own too, removed once it ends.
	code, rev := reviewTurn(t, ns.server, ns.tok, a.Turn, map[string]any{"agent": ag["id"]})
	if code != http.StatusAccepted {
		t.Fatalf("review: %d", code)
	}
	if rev = waitJob(t, ns.server, ns.tok, rev.ID); rev.State != queue.Finished {
		t.Fatalf("review %s %s %s", rev.State, rev.Reason, rev.Result)
	}
	b, _ := os.ReadFile(ns.report)
	var repR fakeReport
	json.Unmarshal(b, &repR)
	homeR := homeOf(repR)
	if homeR == homeA || filepath.Dir(homeR) != filepath.Dir(homeA) || len(repR.Seen) != 0 {
		t.Fatalf("the review's home %q, seen %q", homeR, repR.Seen)
	}
	waitFor(t, "the review's home removed", func() bool { _, err := os.Stat(homeR); return os.IsNotExist(err) })
	if left, _ := os.ReadDir(realHome); len(left) != 0 {
		t.Fatalf("the runner user's home holds %v", left)
	}
	if code := callAPI(t, ns.server, ns.tok, "PATCH", "/api/v0/sessions/"+a.Session, map[string]any{"closed": true}, nil); code != 200 {
		t.Fatalf("close A: %d", code)
	}
	waitFor(t, "session A's home removed", func() bool { _, err := os.Stat(homeA); return os.IsNotExist(err) })
	if _, err := os.Stat(homeOf(repB)); err != nil {
		t.Fatalf("session B's home: %v", err)
	}
}

// putPolicy replaces the server's policy, given as TOML, through the API:
// control reads no settings file once it has started.
func putPolicy(t *testing.T, svc *service, tok, policy string) {
	t.Helper()
	p, err := settings.ParsePolicy([]byte(policy))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	req, _ := http.NewRequest("PUT", "http://"+svc.addr+"/api/v0/settings/permissions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("If-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT the policy: %d", resp.StatusCode)
	}
}
