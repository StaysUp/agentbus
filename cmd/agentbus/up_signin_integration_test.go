//go:build integration

package main

// agentbus up uses the subscription connected on control, as a runner of a
// server does (0.5.1): its agent gets the turn's token as a placeholder, its
// gateway sends each call to up's own relay, which adds the token, and each
// session runs in a home of its own. Every credential here is fake.

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
// own runner, whose gateway records them before the relay.
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

// A session whose saved agent was removed goes on, on its harness's
// subscription as the record holds it: up's relay adds the sign-in to its
// next turn's calls as before the removal.
func TestUpSessionGoesOnAfterItsAgentIsRemoved(t *testing.T) {
	vendor := newFakeVendor(t)
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, vendor.URL, true)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_OUT", filepath.Join(t.TempDir(), "report"))
	t.Setenv("AGENTBUS_FAKE_CALL", "1")
	code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "Mine", "harness": "claude", "access": "subscription"})
	if code != http.StatusCreated {
		t.Fatalf("agent: %d %v", code, ag)
	}
	session, _ := startRun(t, svc, tok, ag["id"].(string), `{"type":"result","subtype":"success","result":"one","session_id":"fake-resume-1"}`+"\n")
	if code, _ := sendAPI(t, svc, tok, "DELETE", "/api/v0/agents/"+ag["id"].(string), nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	code, _, raw := postMessage(t, svc, tok, session, map[string]any{"prompt": "two"})
	if code != http.StatusAccepted {
		t.Fatalf("next turn: %d %s", code, raw)
	}
	closed := waitClosedN(t, svc, session, 2)
	if c := closed[len(closed)-1]; c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("the next turn closed %+v", c)
	}
	want := "/v1/messages Bearer " + fakeSubscription
	if got := vendor.seen(); len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("the vendor saw %v", got)
	}
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
