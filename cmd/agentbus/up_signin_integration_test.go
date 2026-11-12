//go:build integration

package main

// agentbus up uses the subscription connected on control, as a runner of a
// server does (0.5.1): its agent gets the turn's token as a placeholder, its
// gateway swaps in the connected sign-in in up's own process (the relay's
// answer, with no hop over HTTP), and each session runs in a home of its
// own. Every credential here is fake.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
)

func TestUpUsesTheSubscriptionConnectedOnControl(t *testing.T) {
	vendor := newFakeVendor(t)
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, vendor.URL, true)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_CALL", "1")

	// Not connected: refused in plain words before the agent starts, and
	// the vendor sees nothing.
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/subscriptions/claude", nil); code != http.StatusNoContent {
		t.Fatalf("disconnect: %d", code)
	}
	code, _, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusBadRequest || !strings.Contains(raw, "connect your claude subscription in Settings") {
		t.Fatalf("with nothing connected: %d %s", code, raw)
	}
	if got := vendor.seen(); len(got) != 0 {
		t.Fatalf("with nothing connected the vendor saw %v", got)
	}

	// Connected: the vendor gets the person's token, the agent a
	// placeholder and a home of its own.
	connectFake(t, home, "claude")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	session := out["session"].(string)
	waitClosed(t, svc.store, session)
	if got := vendor.seen(); len(got) != 1 || got[0] != "/v1/messages Bearer "+fakeSubscription {
		t.Fatalf("the vendor saw %v", got)
	}
	env := envOf(t, report)
	if !strings.HasPrefix(env["CLAUDE_CODE_OAUTH_TOKEN"], jobs.TokenPrefix) {
		t.Fatal("the agent's sign-in is not the placeholder")
	}
	if want := filepath.Join(home, homesDir, session); env["HOME"] != want {
		t.Fatalf("the agent's HOME %q, want %q", env["HOME"], want)
	}
	b, _ := os.ReadFile(report)
	if strings.Contains(string(b), fakeSubscription) {
		t.Fatal("the agent's environment holds the token")
	}

	// One record: the call is stored once, by up's own gateway, not again
	// by the relay it went through.
	var requested, responded int
	for _, e := range storedEvents(t, svc.store) {
		if e.Session != session {
			continue
		}
		switch e.Kind {
		case "model.requested":
			requested++
		case "model.responded":
			responded++
		}
	}
	if requested != 1 || responded != 1 {
		t.Fatalf("stored %d model.requested and %d model.responded, want one each", requested, responded)
	}
}

// agentbus up is control for other runners too: their calls go through up's
// relay and are recorded in up's record once, and so are the calls of up's
// own runner, which its gateway records and sends to the vendor itself.
func TestUpRecordsEveryRunnersCallsOnce(t *testing.T) {
	vendor := newFakeVendor(t)
	home := t.TempDir()
	up := runServiceWith(t, home, nil, 0, vendor.URL, true)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	t.Setenv("AGENTBUS_FAKE_OUT", filepath.Join(t.TempDir(), "report"))
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	folder, runnerHome := t.TempDir(), t.TempDir()
	fr, err := folders.Open(filepath.Join(runnerHome, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref, machine := folders.Ref(folder), fr.Connector()
	far, err := start(config{mode: modeRunner, listen: "127.0.0.1:0", home: runnerHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), openrouter: mustURL("http://127.0.0.1:1"), logf: t.Logf, noReplay: true,
		lookPath: func(string) (string, error) { return self, nil }, getenv: os.Getenv,
		server: "http://" + up.addr, enrollFile: filepath.Join(home, "enroll_token"), runnerName: "far", folders: []string{folder}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(far.stop)

	calls := func(session string) (requested int) {
		for _, e := range storedEvents(t, up.store) {
			if e.Session == session && e.Kind == "model.requested" {
				requested++
			}
		}
		return requested
	}
	// The other runner's turn.
	j := waitJob(t, up, tok, queueTurn(t, up, tok, map[string]any{"agent": "claude", "folder_ref": ref, "connector": machine, "prompt": "hello"}).ID)
	if j.State != queue.Finished {
		t.Fatalf("the far runner's job %s %s", j.State, j.Result)
	}
	waitFor(t, "the far runner's call in up's record", func() bool { return calls(j.Session) >= 1 })
	// up's own runner's turn.
	code, out, raw := postRun(t, up, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	own := out["session"].(string)
	waitClosed(t, up.store, own)
	if got := vendor.seen(); len(got) != 2 || got[0] != "/v1/messages Bearer "+fakeSubscription || got[1] != got[0] {
		t.Fatalf("the vendor saw %v", got)
	}
	if far, mine := calls(j.Session), calls(own); far != 1 || mine != 1 {
		t.Fatalf("up's record holds %d of the far runner's calls and %d of its own runner's, want one each", far, mine)
	}
}

// Every turn names its agent, read as it is when the turn is asked for,
// and the tool and its credential both come from that one answer (his rule,
// 2026-11-12):
//   - a next turn that names a removed agent is refused, and says so, for a
//     subscription agent and an OpenRouter one alike (rounds 2 and 3);
//   - a next turn that names an agent edited since runs on it as it is now,
//     its tool and its credential agreeing (round 3: an edit from claude to
//     codex left the old claude session's call refused, 403).
func TestUpEveryTurnNamesItsAgent(t *testing.T) {
	vendor := newFakeVendor(t)
	chat := newFakeVendor(t)
	home := t.TempDir()
	// A fake OpenRouter key in control's environment; no turn here calls
	// OpenRouter.
	getenv := func(k string) string {
		if k == "OPENROUTER_API_KEY" {
			return "fake-openrouter-key-for-tests"
		}
		return os.Getenv(k)
	}
	svc := runServiceVendors(t, home, getenv, 0, vendor.URL, chat.URL, true)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_OUT", filepath.Join(t.TempDir(), "report"))
	save := func(fields map[string]any) string {
		code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", fields)
		if code != http.StatusCreated {
			t.Fatalf("agent: %d %v", code, ag)
		}
		return ag["id"].(string)
	}
	claudeResult := `{"type":"result","subtype":"success","result":"one","session_id":"fake-resume-1"}` + "\n"

	for _, fields := range []map[string]any{
		{"name": "Mine", "harness": "claude", "access": "subscription"},
		{"name": "Routed", "harness": "claude", "access": "openrouter", "model": "deepseek/deepseek-v4.1-flash", "key_env": "OPENROUTER_API_KEY"},
	} {
		id := save(fields)
		session, _ := startRun(t, svc, tok, id, claudeResult)
		if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/"+id, nil); code != http.StatusNoContent {
			t.Fatalf("remove: %d", code)
		}
		code, _, raw := postMessage(t, svc, tok, session, map[string]any{"agent": id, "prompt": "two"})
		if code != http.StatusBadRequest || !strings.Contains(raw, `agent \"`+id+`\" was removed; pick another`) {
			t.Fatalf("%s, removed: %d %s", fields["access"], code, raw)
		}
		// A next turn that names no agent at all is refused too.
		code, _, raw = postAPI(t, svc, tok, "/api/v0/turns", []byte(`{"session":"`+session+`","prompt":"three"}`))
		if code != http.StatusBadRequest || !strings.Contains(raw, "agent is required") {
			t.Fatalf("%s, no agent: %d %s", fields["access"], code, raw)
		}
	}

	// Edited from claude to codex: the session's next turn runs codex, and
	// control adds codex's sign-in to its call.
	id := save(map[string]any{"name": "Edited", "harness": "claude", "access": "subscription"})
	session, _ := startRun(t, svc, tok, id, claudeResult)
	if code, ag := sendAPI(t, svc, tok, "PUT", "/api/v0/agents/"+id, map[string]any{"name": "Edited", "harness": "codex", "access": "subscription"}); code != http.StatusOK {
		t.Fatalf("edit: %d %v", code, ag)
	}
	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", `{"model":"x","input":[]}`)
	code, out, raw := postMessage(t, svc, tok, session, map[string]any{"agent": id, "prompt": "two"})
	if code != http.StatusAccepted {
		t.Fatalf("edited, next turn: %d %s", code, raw)
	}
	closed := waitClosedN(t, svc, session, 2)
	if c := closed[len(closed)-1]; c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("the edited agent's turn closed %+v", c)
	}
	if got := chat.seen(); len(got) != 1 || got[0] != "/backend-api/codex/responses Bearer "+fakeSubscription {
		t.Fatalf("chatgpt saw %v", got)
	}
	_ = out
}

// A model the vendor refuses fails the turn with the vendor's own words,
// not the agent's last line of stderr. Seen 2026-11-12 on a 0.5.0 record:
// chatgpt.com answered 400, and model.responded's error held "The
// 'gpt-5.5' model is not supported when using Codex with a ChatGPT
// account." while codex's stderr ended on a WebSocket line.
func TestUpTurnShowsTheVendorsRefusal(t *testing.T) {
	const refusal = "The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"`+refusal+`"}}`)
	}))
	t.Cleanup(chat.Close)
	home := t.TempDir()
	svc := runServiceVendors(t, home, nil, 0, "http://127.0.0.1:1", chat.URL, true)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_OUT", filepath.Join(t.TempDir(), "report"))
	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", `{"model":"gpt-5.5","input":[]}`)
	t.Setenv("AGENTBUS_FAKE_EXIT", "1")
	t.Setenv("AGENTBUS_FAKE_STDERR", "ERROR codex_api::endpoint: something else went wrong")
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "Codex 5.5", "harness": "codex",
		"access": "subscription", "model": "gpt-5.5"})
	if code != http.StatusCreated {
		t.Fatalf("agent: %d %v", code, ag)
	}
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": ag["id"], "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	waitClosed(t, svc.store, out["session"].(string))
	var got struct {
		Turn struct {
			State string
			Error string
		}
	}
	waitFor(t, "the failed turn", func() bool {
		callAPI(t, svc, tok, "GET", "/api/v0/turns/"+out["turn"].(string), nil, &got)
		return got.Turn.State == "failed"
	})
	turn := got.Turn
	if turn.Error != refusal {
		t.Fatalf("the turn's error is %q, want the vendor's %q", turn.Error, refusal)
	}
}

// In agentbus up the agent's gateway gets each call's credential in
// control's own process, so no call of up's own runner goes over /relay.
// A turn's token sent straight to /relay while the turn runs is not lost
// from the record: the relay records every call it carries, in up as on a
// server, under the turn the token names (chosen over refusing it: /relay
// serves the runners of other machines in up too).
func TestUpRelayRecordsADirectCall(t *testing.T) {
	vendor := newFakeVendor(t)
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, vendor.URL, true)
	tok := readToken(t, home)
	report, release := filepath.Join(t.TempDir(), "report"), filepath.Join(t.TempDir(), "release")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_WAIT", release)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("turn: %d %s", code, raw)
	}
	session := out["session"].(string)
	waitFor(t, "the running turn's report", func() bool { _, err := os.Stat(report); return err == nil })
	placeholder := envOf(t, report)["CLAUDE_CODE_OAUTH_TOKEN"]
	if !strings.HasPrefix(placeholder, jobs.TokenPrefix) {
		t.Fatal("no placeholder")
	}
	req, _ := http.NewRequest("POST", "http://"+svc.addr+jobs.RelayPrefix+"anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"straight to the relay"}]}`))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the direct call: %d", resp.StatusCode)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, svc.store, session)
	if got := vendor.seen(); len(got) != 1 || got[0] != "/v1/messages Bearer "+fakeSubscription {
		t.Fatalf("the vendor saw %v", got)
	}
	var requested int
	for _, e := range storedEvents(t, svc.store) {
		if e.Session == session && e.Kind == "model.requested" {
			requested++
		}
	}
	if requested != 1 {
		t.Fatalf("up's record holds %d model.requested of the direct call, want 1", requested)
	}
}
