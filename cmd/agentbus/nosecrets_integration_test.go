//go:build integration

package main

// A runner that holds no secret (agentbus runner, -gateway server by
// default): its agent gets the turn's token, minted by the server, as its
// model credential and as its API token; the server's relay swaps the real
// credential in on the way out. The turn's record reaches the server while
// it runs. Every credential here is fake.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A fake vendor: it keeps each request's path and Authorization, and answers
// an Anthropic Messages reply.
type fakeVendor struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newFakeVendor(t *testing.T) *fakeVendor {
	v := &fakeVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		v.mu.Lock()
		v.auths = append(v.auths, r.URL.Path+" "+r.Header.Get("Authorization"))
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
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
	server, runner  *service
	tok             string
	ref, connector  string
	serverHome      string
	anth, or        *fakeVendor
	report, release string
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
	ns := &noSecretsSetup{anth: newFakeVendor(t), or: newFakeVendor(t), serverHome: t.TempDir()}
	look := func(string) (string, error) { return self, nil }
	// The server's own environment holds the API key.
	serverEnv := func(k string) string {
		if k == "AB_TEST_OR_KEY" {
			return "fake-openrouter-key-for-tests"
		}
		return os.Getenv(k)
	}
	ns.server, err = start(config{mode: modeServer, listen: "127.0.0.1:0", home: ns.serverHome, upstream: mustURL(ns.anth.URL),
		chatgpt: mustURL("http://127.0.0.1:1"), openrouter: mustURL(ns.or.URL), getenv: serverEnv, logf: t.Logf,
		heartbeat: 100 * time.Millisecond, turnWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ns.server.stop)
	ns.tok = readToken(t, ns.serverHome)
	folder, runnerHome := t.TempDir(), t.TempDir()
	fr, err := folders.Open(filepath.Join(runnerHome, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ns.ref, ns.connector = folders.Ref(folder), fr.Connector()
	// The runner's upstreams name nothing: with no secrets it sends every
	// call to the server's relay.
	ns.runner, err = start(config{mode: modeRunner, listen: "127.0.0.1:0", home: runnerHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), openrouter: mustURL("http://127.0.0.1:1"), logf: t.Logf, lookPath: look, noReplay: true,
		getenv: func(k string) string { return os.Getenv(k) },
		server: "http://" + ns.server.addr, enrollFile: filepath.Join(ns.serverHome, "enroll_token"), runnerName: "far", folders: []string{folder}})
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
	var j queue.Job
	if code := callAPI(t, ns.server, ns.tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"agent": ag["id"], "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"}}, &j); code != 201 {
		t.Fatalf("POST /api/v0/jobs: %d", code)
	}
	// While the turn runs, the server's record holds it and its model call.
	var turn, exchange string
	waitFor(t, "the running turn in the server's record", func() bool {
		jj := waitState(t, ns.server, ns.tok, j.ID)
		if len(jj.Attempts) == 0 {
			return false
		}
		turn = jj.Attempts[0].ID
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
		code, raw := getAPI(t, ns.server, ns.tok, "/api/v0/calls/"+exchange+"/context")
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
	// A next turn that names no agent runs on the session's agent, and gets
	// that saved agent's key.
	t.Setenv("AGENTBUS_FAKE_WAIT", "")
	code, out, raw := postRun(t, ns.server, ns.tok, map[string]any{"session": j.Session, "prompt": "again"})
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

func TestRunnerUsesConnectedSubscription(t *testing.T) {
	ns := startNoSecrets(t)
	run := func() queue.Job {
		var j queue.Job
		if code := callAPI(t, ns.server, ns.tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
			"spec": map[string]any{"agent": "claude", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"}}, &j); code != 201 {
			t.Fatalf("POST /api/v0/jobs: %d", code)
		}
		return waitJob(t, ns.server, ns.tok, j.ID)
	}
	// Off by default: the relay refuses, and the vendor sees nothing.
	run()
	if got := ns.anth.seen(); len(got) != 0 {
		t.Fatalf("with subscriptions off the vendor saw %v", got)
	}
	// On, but nothing connected: refused too.
	if err := os.WriteFile(filepath.Join(ns.serverHome, "permissions.toml"), []byte("subscriptions_on_runners = \"own\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	if got := ns.anth.seen(); len(got) != 0 {
		t.Fatalf("with nothing connected the vendor saw %v", got)
	}
	var list struct {
		On            bool
		Subscriptions []struct {
			Harness   string
			Connected bool
		}
	}
	if code := callAPI(t, ns.server, ns.tok, "GET", "/api/v0/subscriptions", nil, &list); code != 200 || !list.On ||
		len(list.Subscriptions) != 1 || list.Subscriptions[0].Harness != "claude" || list.Subscriptions[0].Connected {
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
	if code := callAPI(t, ns.server, ns.tok, "GET", "/api/v0/subscriptions", nil, &list); code != 200 || !list.Subscriptions[0].Connected {
		t.Fatalf("subscriptions: %d %+v", code, list)
	}
}

// An agent on a runner asks another agent through the server's API, with
// the token the server minted; the asked turn runs, and its result comes
// back to the asking session as its next turn, all in the server's record.
func TestRunnerAgentAsksAnother(t *testing.T) {
	ns := startNoSecrets(t)
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	asked := filepath.Join(t.TempDir(), "asked")
	t.Setenv("AGENTBUS_FAKE_ASK", `{"agent":"claude","prompt":"answer me"}`)
	t.Setenv("AGENTBUS_FAKE_ASKED", asked)
	// A resume id, so the asking session can take its next turn.
	t.Setenv("AGENTBUS_FAKE_STDOUT", `{"type":"result","subtype":"success","result":"done","session_id":"fake-resume-1"}`+"\n")
	var j queue.Job
	if code := callAPI(t, ns.server, ns.tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"agent": "claude", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "ask someone"}}, &j); code != 201 {
		t.Fatalf("POST /api/v0/jobs: %d", code)
	}
	waitJob(t, ns.server, ns.tok, j.ID)
	b, err := os.ReadFile(asked)
	if err != nil || !strings.HasPrefix(string(b), "202 ") {
		t.Fatalf("the ask: %v %s", err, b)
	}
	// The result comes back as the asking session's next turn.
	defer func() {
		if t.Failed() {
			var all struct{ Jobs []queue.Job }
			callAPI(t, ns.server, ns.tok, "GET", "/api/v0/jobs", nil, &all)
			for _, x := range all.Jobs {
				t.Logf("job %s %s %s session %s links %+v result %s", x.ID, x.Kind, x.State, x.Session, x.Links, x.Result)
			}
		}
	}()
	waitFor(t, "the result delivered to the asking session", func() bool {
		var jobs struct{ Jobs []queue.Job }
		callAPI(t, ns.server, ns.tok, "GET", "/api/v0/jobs?session="+j.Session, nil, &jobs)
		for _, x := range jobs.Jobs {
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
}

// A turn that has not called its model yet is in the server's record
// already: what its start wrote is sent at once.
func TestRunnerTurnShownBeforeItsFirstCall(t *testing.T) {
	ns := startNoSecrets(t)
	t.Setenv("AGENTBUS_FAKE_CALL", "")
	t.Setenv("AGENTBUS_FAKE_WAIT", ns.release)
	var j queue.Job
	if code := callAPI(t, ns.server, ns.tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"agent": "claude", "folder_ref": ns.ref, "connector": ns.connector, "prompt": "hello"}}, &j); code != 201 {
		t.Fatalf("POST /api/v0/jobs: %d", code)
	}
	waitFor(t, "the waiting turn in the server's record", func() bool {
		jj := waitState(t, ns.server, ns.tok, j.ID)
		if len(jj.Attempts) == 0 {
			return false
		}
		list, _ := ns.server.store.Turns(store.TurnQuery{Turn: jj.Attempts[0].ID, Limit: 1})
		return len(list.Turns) == 1 && list.Turns[0].State == "running"
	})
	os.WriteFile(ns.release, nil, 0o600)
	waitJob(t, ns.server, ns.tok, j.ID)
}
