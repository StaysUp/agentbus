//go:build integration

package main

// A note for a running turn (queue/notes.go) across real processes: control
// keeps it, the runner, a process of its own, hears of it on a heartbeat, its
// gateway's own replay lays it out in the turn's model calls from the record
// (replay/notes.go), and the call's context.added reaches control's record,
// which marks the note delivered. One test per API format: Anthropic
// (claude), OpenAI responses (codex) and Chat Completions (opencode on
// OpenRouter). It holds every finding of the two reviews of 2026-11-28 (Codex,
// turns t_0ea0d0c0e651be05 and t_dc2bc7216610e2e2).
//
// Nothing of the fake agent's own report is read or printed: it holds the
// agent's environment, which carries credentials (first review, P1).

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

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

const (
	noteXFirst  = "your folder changed: a.go"
	noteXSecond = "your folder changed again: b.go"
)

// noteXCase is one API format: its harness, control's upstream variable, the
// fake agent's variables for its first call and for the calls after the
// wait, the bodies, the vendor's reply, and where a request holds its
// conversation.
type noteXCase struct {
	name, harness, upstreamVar string
	firstVar, afterVar         string
	// first is every turn's first call, @PROMPT@ its prompt. step1, step2
	// and compacted are turn 1's calls after the note: its tool loop, one
	// step and then two, and a call after the agent compacted.
	first, step1, step2, compacted string
	reply                          string
	conversation                   string
	note                           func(text string) string
	// afterSummary is where a note goes in compacted's conversation.
	afterSummary int
	// agent makes the saved agent the turn runs on; nil runs on the harness.
	agent func(x *xproc) string
}

func noteXCases() []noteXCase {
	claudeHead := func(p string) string {
		return `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"},{"type":"text","text":"` + p + `"}]},` +
			`{"role":"system","content":[{"type":"text","text":"# Environment\nenv\n\nToday's date is d.","cache_control":{"type":"ephemeral"}}]}`
	}
	claudeTail := `],"tools":[{"name":"Bash","input_schema":{}}],"stream":true}`
	claudeStep := func(id string) string {
		return `,{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"ls"}}]}` +
			`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"a.go"}]}`
	}
	codexHead := func(p string) string {
		return `{"model":"gpt-x","stream":true,"store":false,"prompt_cache_key":"th-` + p + `","input":[` +
			`{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + p + `"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}`
	}
	codexStep := func(id string) string {
		return `,{"type":"function_call","call_id":"` + id + `","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"` + id + `","output":"a.go"}`
	}
	chatHead := func(p string) string {
		return `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"system","content":"You are opencode."},{"role":"user","content":"` + p + `"}`
	}
	chatTail := `],"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],"stream":true,"stream_options":{"include_usage":true}}`
	chatStep := func(id string) string {
		return `,{"role":"assistant","content":null,"tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"bash","arguments":"{}"}}]}` +
			`,{"role":"tool","tool_call_id":"` + id + `","content":"a.go"}`
	}
	return []noteXCase{
		{name: "anthropic", harness: "claude", upstreamVar: "AGENTBUS_TEST_UPSTREAM",
			firstVar: "AGENTBUS_FAKE_BODY", afterVar: "AGENTBUS_FAKE_CALL_AFTER",
			first: claudeHead("@PROMPT@") + claudeTail, step1: claudeHead("one") + claudeStep("t1") + claudeTail,
			step2: claudeHead("one") + claudeStep("t1") + claudeStep("t2") + claudeTail,
			compacted: `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"This session is being continued from a previous conversation. Summary: listed files."}]},` +
				`{"role":"system","content":"# Environment\nenv"}` + claudeStep("t3") + claudeTail,
			afterSummary: 1, reply: streamedReply, conversation: "messages",
			note: func(s string) string {
				return `{"role":"user","content":[{"type":"text","text":` + quoteJSON(event.NotePrefix+s) + `}]}`
			}},
		{name: "openai-responses", harness: "codex", upstreamVar: "AGENTBUS_TEST_CHATGPT_UPSTREAM",
			firstVar: "AGENTBUS_FAKE_CODEX_BODY", afterVar: "AGENTBUS_FAKE_CODEX_AFTER",
			first: codexHead("@PROMPT@") + "]}", step1: codexHead("one") + codexStep("c1") + "]}",
			step2: codexHead("one") + codexStep("c1") + codexStep("c2") + "]}",
			compacted: `{"model":"gpt-x","stream":true,"store":false,"prompt_cache_key":"th-one","input":[` +
				`{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},` +
				`{"type":"compaction","encrypted_content":"U1VN"}` + codexStep("c3") + "]}",
			afterSummary: 2, reply: codexStream, conversation: "input",
			note: func(s string) string {
				return `{"type":"message","role":"user","content":[{"type":"input_text","text":` + quoteJSON(event.NotePrefix+s) + `}]}`
			}},
		{name: "chat-completions", harness: "opencode", upstreamVar: "AGENTBUS_TEST_OPENROUTER_UPSTREAM",
			firstVar: "AGENTBUS_FAKE_OPENCODE_BODY", afterVar: "AGENTBUS_FAKE_OPENCODE_AFTER",
			first: chatHead("@PROMPT@") + chatTail, step1: chatHead("one") + chatStep("k1") + chatTail,
			step2: chatHead("one") + chatStep("k1") + chatStep("k2") + chatTail,
			compacted: `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"system","content":"You are opencode."},{"role":"user","content":"Summary: listed files."}` +
				chatStep("k3") + chatTail,
			afterSummary: 2, reply: opencodeStreamedReply, conversation: "messages",
			note: func(s string) string { return `{"role":"user","content":` + quoteJSON(event.NotePrefix+s) + `}` },
			agent: func(x *xproc) string {
				code, b := x.post("/api/v0/agents", map[string]any{"name": "GLM", "harness": "opencode",
					"access": "openrouter", "model": "z-ai/glm-5.3-flash", "key_env": "AB_TEST_OR_KEY"})
				var ag struct {
					ID string `json:"id"`
				}
				if code != http.StatusCreated || json.Unmarshal(b, &ag) != nil || ag.ID == "" {
					x.t.Fatalf("create the opencode agent: %d", code)
				}
				return ag.ID
			}},
	}
}

func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

// conversationX is a request body's conversation, each message as sent.
func conversationX(t *testing.T, data, key string) []string {
	t.Helper()
	fields, err := body.Fields([]byte(data))
	if err != nil {
		t.Fatalf("a request is not JSON: %v", err)
	}
	for _, f := range fields {
		if f.Key == key {
			els, _ := body.Elements(f.Value)
			out := make([]string, len(els))
			for i, e := range els {
				out[i] = string(e)
			}
			return out
		}
	}
	t.Fatalf("a request has no %s", key)
	return nil
}

func withNoteAtX(msgs []string, i int, note string) []string {
	return append(append(append([]string{}, msgs[:i]...), note), msgs[i:]...)
}

// noteEvents are the context.added events of a session that give a note,
// from control's record.
func (x *xproc) noteEvents(session string) []event.Event {
	x.t.Helper()
	code, b := x.get("/api/v0/sessions/" + session + "/events")
	if code != 200 {
		x.t.Fatalf("GET events: %d", code)
	}
	var page struct {
		Events []event.Event `json:"events"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		x.t.Fatalf("events: %v", err)
	}
	var out []event.Event
	for _, e := range page.Events {
		if len(event.NotesGiven(e)) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// callStatus is the status control's record holds for a call of turn.
func (x *xproc) callStatus(turn, exchange string) int {
	x.t.Helper()
	code, b := x.get("/api/v0/turns/" + turn + "/calls?limit=500")
	var page struct {
		Calls []struct {
			Exchange string `json:"exchange"`
			Status   int    `json:"status"`
		} `json:"calls"`
	}
	if code != 200 || json.Unmarshal(b, &page) != nil {
		x.t.Fatalf("GET the turn's calls: %d", code)
	}
	for _, c := range page.Calls {
		if c.Exchange == exchange {
			return c.Status
		}
	}
	return 0
}

// xCall is one model call the fake vendor got, and its answer.
type xCall struct {
	body   string
	status int
}

// A note's life across processes, for each API format:
//   - the first call that carries it is refused (429): it is not delivered,
//     and the next call carries it again (second review, P2 1);
//   - the call the vendor accepts records it once, on control, with that
//     call's exchange and its place, and control marks it delivered;
//   - every later call of the turn carries it at that place, as the tool loop
//     grows (second review, P2 2), and after a compaction right after the
//     summary, never between a tool call and its result (first review, P2 4);
//   - the next turn's history, built from the turn's last call before the
//     compaction, keeps it, and the next turn is not refused (first review,
//     P1 2; second review, P2 2);
//   - a note attached once the turn has ended reaches the next turn's first
//     call, whatever its harness's instructions (first review, P2 3).
func TestXProcNoteLife(t *testing.T) {
	for _, c := range noteXCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AB_TEST_OR_KEY", "test-key-value-notes")
			dir := t.TempDir()
			gate, stop := filepath.Join(dir, "go"), filepath.Join(dir, "stop")
			var mu sync.Mutex
			var calls []xCall
			refused := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || len(b) == 0 {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				carries := strings.Contains(string(b), noteXFirst)
				if carries && !refused {
					refused = true
					calls = append(calls, xCall{string(b), 429})
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
					return
				}
				calls = append(calls, xCall{string(b), 200})
				if carries {
					os.WriteFile(stop, []byte("seen"), 0o600)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, c.reply)
			}))
			t.Cleanup(upstream.Close)
			seen := func() []xCall {
				mu.Lock()
				defer mu.Unlock()
				return append([]xCall{}, calls...)
			}

			after := "[" + c.step1 + "," + c.step2 + "," + c.compacted + "]"
			x := startServerEnv(t, []string{c.upstreamVar + "=" + upstream.URL}, c.harness)
			x.setAgentEnv(c.firstVar+"="+c.first, "AGENTBUS_FAKE_WAIT="+gate, "AGENTBUS_FAKE_WAIT_S=30",
				c.afterVar+"="+after, "AGENTBUS_FAKE_AFTER_UNTIL="+stop, "AGENTBUS_FAKE_AFTER_PROMPT=one")
			x.addRunner("far", t.TempDir())
			fs := x.folders()
			agent := c.harness
			if c.agent != nil {
				agent = c.agent(x)
			}
			code, st := x.startTurnX(map[string]any{"agent": agent, "folder_ref": fs[0].Ref,
				"machine": fs[0].Machine, "prompt": "one", "isolate": false})
			if code/100 != 2 || st.Session == "" || st.Turn == "" {
				t.Fatalf("POST /api/v0/turns: %d %+v", code, st)
			}
			waitForN(t, "the turn's first call", 20*time.Second, func() bool { return len(seen()) >= 1 })

			// Control attaches the note: internal plumbing, no route.
			q, err := queue.Open(filepath.Join(x.serverHome, "jobs.db"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			n1, err := q.AddNote(queue.DefaultTeam, st.Turn, noteXFirst)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitForN(t, "turn 1 ended", 40*time.Second, func() bool { return x.turnState(st.Turn) == "finished" })

			// The calls before the runner heard of the note do not carry it;
			// every call from the refused one on carries it at its place.
			note := c.note(noteXFirst)
			one := conversationX(t, c.step1, c.conversation)
			want := map[string][]string{
				c.step1:     append(append([]string{}, one...), note),
				c.step2:     withNoteAtX(conversationX(t, c.step2, c.conversation), len(one), note),
				c.compacted: withNoteAtX(conversationX(t, c.compacted, c.conversation), c.afterSummary, note),
			}
			all := seen()
			if strings.Contains(all[0].body, noteXFirst) {
				t.Fatal("the call before the note carried it")
			}
			from := 1
			for from < len(all) && !strings.Contains(all[from].body, noteXFirst) {
				if all[from].body != c.step1 {
					t.Fatalf("call %d is not the tool loop's first step", from)
				}
				from++
			}
			all = all[from-1:]
			stages := map[string]int{}
			for i, cl := range all[1:] {
				got := conversationX(t, cl.body, c.conversation)
				matched := false
				for agentBody, w := range want {
					if strings.Join(got, "\n") == strings.Join(w, "\n") {
						stages[agentBody]++
						matched = true
					}
				}
				if !matched {
					t.Fatalf("call %d does not carry the note at its place:\n%s", i+from, strings.Join(got, "\n"))
				}
			}
			if all[1].status != 429 || stages[c.step1] < 2 || stages[c.step2] != 1 || stages[c.compacted] != 1 {
				t.Fatalf("calls by stage %v, the first after the note answered %d: want a refused call, then the accepted one, then one of each later",
					stages, all[1].status)
			}

			// Control's record: one context.added, of the accepted call, with
			// its place; the call's kept request holds the note; delivered.
			var evs []event.Event
			waitForN(t, "the note's context.added on control", 10*time.Second, func() bool {
				evs = x.noteEvents(st.Session)
				return len(evs) > 0
			})
			if len(evs) != 1 || evs[0].Exchange == "" || evs[0].Turn != st.Turn {
				t.Fatalf("%d note events, want one of a call of turn 1", len(evs))
			}
			if a := evs[0].Added; len(a) != 1 || a[0].Note != n1.ID || a[0].At != len(one) || a[0].Compacted || a[0].Text != event.NotePrefix+noteXFirst {
				t.Fatalf("added %+v, want note %d at %d", a, n1.ID, len(one))
			}
			if s := x.callStatus(st.Turn, evs[0].Exchange); s != 200 {
				t.Fatalf("the note was recorded on a call answered %d, not the accepted one", s)
			}
			code, call := x.get("/api/v0/turns/" + st.Turn + "/calls/" + evs[0].Exchange)
			if code != 200 || !strings.Contains(string(call), noteXFirst) {
				t.Fatalf("GET the accepted call: %d, holds the note %v", code, strings.Contains(string(call), noteXFirst))
			}
			waitForN(t, "the note marked delivered", 10*time.Second, func() bool {
				p, err := q.PendingNotes(queue.DefaultTeam, st.Session)
				return err == nil && len(p) == 0
			})

			// A note attached after the turn ended waits for the next turn.
			n2, err := q.AddNote(queue.DefaultTeam, st.Turn, noteXSecond)
			if err != nil {
				t.Fatal(err)
			}
			before := len(seen())
			code, st2 := x.startTurnX(map[string]any{"session": st.Session, "agent": agent, "prompt": "two"})
			if code/100 != 2 || st2.Turn == "" {
				t.Fatalf("turn 2: %d %+v", code, st2)
			}
			waitForN(t, "turn 2 ended", 30*time.Second, func() bool { return x.turnState(st2.Turn) == "finished" })
			two := seen()[before:]
			if len(two) != 1 || two[0].status != 200 {
				t.Fatalf("turn 2 made %d calls, want one accepted", len(two))
			}
			conv := conversationX(t, two[0].body, c.conversation)
			hist := strings.Join(conv, "\n")
			if strings.Count(hist, noteXFirst) != 1 || !strings.Contains(hist, "a.go") || conv[len(conv)-1] != c.note(noteXSecond) {
				t.Fatalf("turn 2's call should hold turn 1's history with its note once, then the new note last:\n%s", hist)
			}
			waitForN(t, "the second note's context.added on control", 10*time.Second, func() bool {
				evs = x.noteEvents(st.Session)
				return len(evs) == 2
			})
			if evs[1].Turn != st2.Turn || evs[1].Exchange == "" || event.NotesGiven(evs[1])[0] != n2.ID {
				t.Fatalf("the second note event %+v, want one of turn 2's call", evs[1].Added)
			}
			waitForN(t, "the second note marked delivered", 10*time.Second, func() bool {
				p, err := q.PendingNotes(queue.DefaultTeam, st.Session)
				return err == nil && len(p) == 0
			})
		})
	}
}
