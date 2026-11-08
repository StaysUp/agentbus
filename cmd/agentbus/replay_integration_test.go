//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// A fresh Claude Code request in 2.1.319's layout (synthetic): the person's
// message with a reminder block, then Claude Code's own system message.
func freshClaudeBody(prompt string) string {
	p, _ := json.Marshal(prompt)
	return `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"},{"type":"text","text":` + string(p) + `}]},` +
		`{"role":"system","content":[{"type":"text","text":"# Environment\nenv\n\nToday's date is d.","cache_control":{"type":"ephemeral"}}]}],` +
		`"tools":[{"name":"Bash","input_schema":{}}],"stream":true}`
}

const streamedReply = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Noted: kiwi.\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// Own replay end to end: claude's second turn starts with no --resume, its
// turn.started names the first turn in replay_from, and the vendor receives
// its request with the first turn's history in front, the first reply
// rebuilt from the stream. A codex session whose turn made no model call (as
// one from before codex replies were kept) still continues by its own
// resume.
func TestReplayedClaudeTurn(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamedReply)
	}))
	t.Cleanup(upstream.Close)
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, upstream.URL, false)
	tok := readToken(t, home)

	t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody("one"))
	id, _ := startRun(t, svc, tok, "claude", claudeResultA)
	var first string
	for _, e := range turnEvents(t, svc.store, id) {
		if e.Kind == "turn.started" {
			first = e.Turn
		}
	}

	report := t.TempDir() + "/report"
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody("which word?"))
	code, out, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "which word?"})
	if code != http.StatusAccepted {
		t.Fatalf("next: %d %s", code, raw)
	}
	waitClosedN(t, svc, id, 2)
	r := readReport(t, report)
	if got := argsOf(t, r.Args, id); got != "-p --output-format json --append-system-prompt HINT -- which word?" {
		t.Errorf("args %q, want a fresh claude with no --resume", got)
	}
	recs, err := svc.store.After(0, store.Filter{Session: id, Only: true}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var from string
	for _, rec := range recs {
		var e struct {
			Kind, Turn string
			ReplayFrom string `json:"replay_from"`
		}
		json.Unmarshal(rec.Data, &e)
		if e.Kind == "turn.started" && e.Turn == out["turn"] {
			from = e.ReplayFrom
		}
	}
	if from == "" || from != first {
		t.Errorf("turn 2's replay_from %q, want turn 1 %q", from, first)
	}
	if c, _ := continuityOf(t, svc, id, out["turn"].(string)); c != "replay" {
		t.Errorf("turn 2's continuity %q, want replay", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream got %d requests, want 2", len(bodies))
	}
	var sent struct{ Messages []json.RawMessage }
	if err := json.Unmarshal([]byte(bodies[1]), &sent); err != nil {
		t.Fatal(err)
	}
	var fresh struct{ Messages []json.RawMessage }
	json.Unmarshal([]byte(freshClaudeBody("one")), &fresh)
	want := []string{
		string(fresh.Messages[0]),
		`{"role":"system","content":"# Environment\nenv\n\nToday's date is d."}`,
		`{"role":"assistant","content":[{"type":"text","text":"Noted: kiwi.","cache_control":{"type":"ephemeral"}}]}`,
		`{"role":"user","content":"which word?"}`,
	}
	var got []string
	for _, m := range sent.Messages {
		got = append(got, string(m))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("turn 2 sent\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// codex with no stored call, in the same service with replay on, falls
	// back to its own resume.
	t.Setenv("AGENTBUS_FAKE_BODY", "")
	t.Setenv("AGENTBUS_FAKE_OUT", "")
	cid, _ := startRun(t, svc, tok, "codex", codexEventsA)
	report2 := t.TempDir() + "/report"
	t.Setenv("AGENTBUS_FAKE_OUT", report2)
	if code, _, raw := postMessage(t, svc, tok, cid, map[string]any{"prompt": "next"}); code != http.StatusAccepted {
		t.Fatalf("codex next: %d %s", code, raw)
	}
	waitClosedN(t, svc, cid, 2)
	if got := strings.Join(readReport(t, report2).Args, " "); !strings.Contains(got, "resume --json -- "+codexIDA+" next") {
		t.Errorf("codex args %q, want its own resume", got)
	}
}

// A claude session from before replies were stored (no stored reply, a
// resume_id, never replayed) still continues, by --resume, and its
// turn.started says so. A session that did use replay is refused: its
// resume_id names only its last turn.
func TestReplayFallsBackToResumeForOldSessions(t *testing.T) {
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, "http://127.0.0.1:1", false)
	tok := readToken(t, home)
	id, _ := startRun(t, svc, tok, "claude", claudeResultA)
	report := t.TempDir() + "/report"
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	code, out, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "next"})
	if code != http.StatusAccepted {
		t.Fatalf("%d %s, want 202 by resume", code, raw)
	}
	waitClosedN(t, svc, id, 2)
	if got := argsOf(t, readReport(t, report).Args, id); got != "-p --output-format json --append-system-prompt HINT --resume "+idA+" -- next" {
		t.Errorf("args %q, want --resume", got)
	}
	if c, from := continuityOf(t, svc, id, out["turn"].(string)); c != "resume" || from != "" {
		t.Errorf("continuity %q replay_from %q, want resume and none", c, from)
	}

	// A session that used replay, now with nothing to replay: 409.
	id2, _ := startRun(t, svc, tok, "claude", claudeResultA)
	if _, _, err := svc.store.Append(event.Event{ID: "evt_r", Kind: event.TurnStarted, Time: time.Now(), Session: id2,
		Turn: "t_replayed", Text: "x", ReplayFrom: "t_old", Continuity: "replay"}); err != nil {
		t.Fatal(err)
	}
	code, _, raw = postMessage(t, svc, tok, id2, map[string]any{"prompt": "next"})
	if code != http.StatusConflict || !strings.Contains(raw, "this session cannot be continued from here: turn t_replayed made no model call and has not ended") {
		t.Errorf("%d %s, want 409", code, raw)
	}
}

func continuityOf(t *testing.T, svc *service, session, turn string) (continuity, from string) {
	t.Helper()
	recs, err := svc.store.After(0, store.Filter{Session: session, Only: true}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		var e struct {
			Kind, Turn, Continuity string
			ReplayFrom             string `json:"replay_from"`
		}
		json.Unmarshal(rec.Data, &e)
		if e.Kind == "turn.started" && e.Turn == turn {
			return e.Continuity, e.ReplayFrom
		}
	}
	return "", ""
}

// A turn's session.closed is stored only after Settle returns, so a reader
// that sees the turn closed sees all of its calls.
func TestSessionClosedWaitsForSettle(t *testing.T) {
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, "http://127.0.0.1:1", false)
	tok := readToken(t, home)
	settled := make(chan bool, 1)
	svc.eng.SetSettle(func(session, turn string) bool {
		closedYet := len(closedEvents(runEvents(t, svc.store, session))) > 0
		time.Sleep(50 * time.Millisecond)
		settled <- closedYet
		return true
	})
	startRun(t, svc, tok, "claude", claudeResultA)
	if closedYet := <-settled; closedYet {
		t.Error("session.closed was stored before Settle returned")
	}
}

// A session continued by own replay is refused on a service with replay
// off, never put on the vendor's resume, whose transcript holds only its
// last turn. A session that never used replay still resumes there.
func TestReplayedSessionRefusedWithReplayOff(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0) // replay off
	tok := readToken(t, home)
	id, _ := startRun(t, svc, tok, "claude", claudeResultA)
	if _, _, err := svc.store.Append(event.Event{ID: "evt_r", Kind: event.TurnStarted, Time: time.Now(), Session: id,
		Turn: "t_replayed", Text: "x", ReplayFrom: "t_old", Continuity: "replay"}); err != nil {
		t.Fatal(err)
	}
	code, _, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "next"})
	if code != http.StatusConflict || !strings.Contains(raw, "continued by own replay") {
		t.Errorf("%d %s, want 409 naming own replay", code, raw)
	}
	plain, _ := startRun(t, svc, tok, "claude", claudeResultA)
	if code, _, raw := postMessage(t, svc, tok, plain, map[string]any{"prompt": "next"}); code != http.StatusAccepted {
		t.Errorf("a never-replayed session: %d %s, want 202", code, raw)
	}
	waitClosedN(t, svc, plain, 2)
}

// A synthetic fresh codex request in codex-cli 0.164.0's layout: context
// items, then the prompt, under thread.
func freshCodexBody(thread, prompt string) string {
	p, _ := json.Marshal(prompt)
	kinds := func(k string) string {
		return `"internal_chat_message_metadata_passthrough":{"content_item_kinds":["` + k + `"]}`
	}
	return `{"model":"gpt-x","stream":true,"store":false,"prompt_cache_key":"` + thread + `","client_metadata":{"thread_id":"` + thread + `"},"input":[` +
		`{"type":"additional_tools","id":"t-` + thread + `","role":"developer","tools":[{"type":"custom","name":"exec"}]},` +
		`{"type":"message","id":"d-` + thread + `","role":"developer","content":[{"type":"input_text","text":"base"}],` + kinds("model.base_instructions") + `},` +
		`{"type":"message","id":"p-` + thread + `","role":"user","content":[{"type":"input_text","text":` + string(p) + `}],` + kinds("user.text") + `}]}`
}

const codexStream = "data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-x\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-x\",\"status\":\"completed\",\"output\":[" +
	"{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[],\"encrypted_content\":\"RU5D\"}," +
	"{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]," +
	"\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"

// Own replay for codex end to end: the second turn is a fresh `codex exec`
// with no resume, and chatgpt.com gets the first turn's input and reply
// items in front of its prompt, under the first turn's thread id in the body
// and in the headers.
func TestReplayedCodexTurn(t *testing.T) {
	const th1, th2 = "019a0000-0000-7000-8000-0000000000c1", "019a0000-0000-7000-8000-0000000000c2"
	var mu sync.Mutex
	var bodies, threads []string
	chatgpt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, threads = append(bodies, string(b)), append(threads, r.Header.Get("Thread-Id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, codexStream)
	}))
	t.Cleanup(chatgpt.Close)
	home := t.TempDir()
	svc := runServiceVendors(t, home, nil, 0, "http://127.0.0.1:1", chatgpt.URL, false)
	tok := readToken(t, home)

	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", freshCodexBody(th1, "one"))
	t.Setenv("AGENTBUS_FAKE_THREAD", th1)
	id, _ := startRun(t, svc, tok, "codex", codexEventsA)

	t.Setenv("AGENTBUS_FAKE_CODEX_BODY", freshCodexBody(th2, "next"))
	t.Setenv("AGENTBUS_FAKE_THREAD", th2)
	report := t.TempDir() + "/report"
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	code, out, raw := postMessage(t, svc, tok, id, map[string]any{"prompt": "next"})
	if code != http.StatusAccepted {
		t.Fatalf("next: %d %s", code, raw)
	}
	waitClosedN(t, svc, id, 2)
	if got := strings.Join(readReport(t, report).Args, " "); strings.Contains(got, "resume") || !strings.HasSuffix(got, "-- next") {
		t.Errorf("args %q, want a fresh exec", got)
	}
	if c, from := continuityOf(t, svc, id, out["turn"].(string)); c != "replay" || from == "" {
		t.Errorf("continuity %q replay_from %q", c, from)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d calls reached chatgpt.com, want 2", len(bodies))
	}
	var sent struct {
		Key   string            `json:"prompt_cache_key"`
		Meta  json.RawMessage   `json:"client_metadata"`
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(bodies[1]), &sent); err != nil {
		t.Fatal(err)
	}
	var first struct {
		Input []json.RawMessage `json:"input"`
	}
	json.Unmarshal([]byte(bodies[0]), &first)
	if sent.Key != th1 || threads[1] != th1 || string(sent.Meta) != `{"thread_id":"`+th1+`"}` {
		t.Errorf("thread not rewritten: key %q header %q", sent.Key, threads[1])
	}
	// turn 1's three items, its two reply items, then the new prompt.
	if len(sent.Input) != 6 || string(sent.Input[0]) != string(first.Input[0]) ||
		!strings.Contains(string(sent.Input[3]), `"encrypted_content":"RU5D"`) || !strings.Contains(string(sent.Input[5]), `"next"`) {
		t.Errorf("turn 2 sent %d items: %s", len(sent.Input), bodies[1])
	}
}
