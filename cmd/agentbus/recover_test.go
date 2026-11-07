package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/staysup/agentbus/store"
)

// A turn that does not finish never ends the session. These run the whole
// service with the fake agent: the turn is stopped inside its tool loop (the
// child killed, or the service restarted), or its only model call fails; the
// next turn is accepted, its turn.started says what it recovered from, and
// the vendor receives the earlier work with a note. One more turn follows.

const toolStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_slow\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"go test\\\"}\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// vendor answers each request as its mode says: "ok" a finished reply,
// "tool" a reply asking for a tool, or an HTTP status as an error.
type vendor struct {
	mu     sync.Mutex
	mode   string
	bodies []string
}

func (v *vendor) set(mode string) { v.mu.Lock(); v.mode = mode; v.mu.Unlock() }

func (v *vendor) last(t *testing.T) []string {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.bodies) == 0 {
		t.Fatal("the vendor got no request")
	}
	var sent struct{ Messages []json.RawMessage }
	if err := json.Unmarshal([]byte(v.bodies[len(v.bodies)-1]), &sent); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(sent.Messages))
	for i, m := range sent.Messages {
		out[i] = string(m)
	}
	return out
}

func newVendor(t *testing.T) (*vendor, string) {
	v := &vendor{mode: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.bodies = append(v.bodies, string(b))
		mode := v.mode
		v.mu.Unlock()
		switch mode {
		case "cut-end", "cut-tool":
			// The stream breaks after message_delta, before message_stop.
			w.Header().Set("Content-Type", "text/event-stream")
			full := streamedReply
			if mode == "cut-tool" {
				full = toolStream
			}
			io.WriteString(w, full[:strings.Index(full, "event: message_stop")])
		case "ok", "tool":
			w.Header().Set("Content-Type", "text/event-stream")
			if mode == "ok" {
				io.WriteString(w, streamedReply)
			} else {
				io.WriteString(w, toolStream)
			}
		default:
			code, _ := strconv.Atoi(mode)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return v, srv.URL
}

type startedTurn struct {
	Kind, Turn      string
	ReplayFrom      string `json:"replay_from"`
	RecoveredFrom   string `json:"recovered_from"`
	RecoveredReason string `json:"recovered_reason"`
	Continuity      string
}

func turnStarted(t *testing.T, st *store.Store, session, turn string) startedTurn {
	t.Helper()
	recs, err := st.After(0, store.Filter{Session: session, Only: true}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		var e startedTurn
		json.Unmarshal(rec.Data, &e)
		if e.Kind == "turn.started" && e.Turn == turn {
			return e
		}
	}
	t.Fatalf("no turn.started for %s", turn)
	return startedTurn{}
}

// nextTurn posts the session's next turn with the fake agent sending one
// fresh request for prompt, and returns the turn id.
func nextTurn(t *testing.T, svc *service, tok, session, prompt string) string {
	t.Helper()
	t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody(prompt))
	code, out, raw := postMessage(t, svc, tok, session, map[string]any{"prompt": prompt})
	if code != http.StatusAccepted {
		t.Fatalf("%q: %d %s", prompt, code, raw)
	}
	return out["turn"].(string)
}

func countAll(msgs []string, s string) int {
	return strings.Count(strings.Join(msgs, "\n"), s)
}

// respondedIn waits until a model.responded of turn is stored.
func respondedIn(t *testing.T, st *store.Store, session, turn string) {
	t.Helper()
	waitFor(t, "a model.responded of "+turn, func() bool {
		recs, err := st.After(0, store.Filter{Session: session, Only: true}, 10000)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			var e struct{ Kind, Turn string }
			json.Unmarshal(rec.Data, &e)
			if e.Kind == "model.responded" && e.Turn == turn {
				return true
			}
		}
		return false
	})
}

var pidOf = regexp.MustCompile(`message\.start session=\S+ agent=claude turn=(\S+) hold=\S+ pid=(\d+)`)

func childPID(t *testing.T, home, turn string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "logs", "service.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pidOf.FindAllStringSubmatch(string(b), -1) {
		if m[1] == turn {
			pid, _ := strconv.Atoi(m[2])
			return pid
		}
	}
	t.Fatalf("no pid for %s in the service log", turn)
	return 0
}

// checkRecovered checks turn 3 of a recovered session: its turn.started, and
// what the vendor received.
func checkRecovered(t *testing.T, v *vendor, svc *service, session, failed, turn3, reason, wantInHistory string) []string {
	t.Helper()
	ts := turnStarted(t, svc.store, session, turn3)
	if ts.ReplayFrom != failed || ts.RecoveredFrom != failed || !strings.Contains(ts.RecoveredReason, reason) || ts.Continuity != "replay" {
		t.Errorf("turn 3's turn.started %+v, want replay_from and recovered_from %s, reason with %q", ts, failed, reason)
	}
	msgs := v.last(t)
	note := "[Note from AgentBus, not written by the model] The turn above did not finish: " + reason
	if countAll(msgs, note) != 1 || countAll(msgs, wantInHistory) == 0 {
		t.Errorf("turn 3 sent\n%s\nwant the note %q once and %s", strings.Join(msgs, "\n"), note, wantInHistory)
	}
	if !strings.Contains(msgs[len(msgs)-1], "what happened?") && !strings.Contains(msgs[len(msgs)-2], "what happened?") {
		t.Errorf("turn 3's own prompt is not last: %s", msgs[len(msgs)-1])
	}
	return msgs
}

// checkAfter checks a turn after the recovered one: no recovered_from, and
// the recovered history replayed whole, with the note once.
func checkAfter(t *testing.T, v *vendor, svc *service, session, prev, turn string, before []string) {
	t.Helper()
	ts := turnStarted(t, svc.store, session, turn)
	if ts.ReplayFrom != prev || ts.RecoveredFrom != "" || ts.RecoveredReason != "" {
		t.Errorf("turn.started %+v, want replay_from %s and no recovered_from", ts, prev)
	}
	msgs := v.last(t)
	if countAll(msgs, "The turn above did not finish") != 1 || len(msgs) <= len(before) {
		t.Errorf("next turn sent\n%s", strings.Join(msgs, "\n"))
	}
	// The history before turn 3's own prompt is the same messages: a stable
	// prefix (the breakpoint on its last message aside).
	for i := 0; i < len(before)-2; i++ {
		a := strings.ReplaceAll(before[i], `,"cache_control":{"type":"ephemeral"}`, "")
		b := strings.ReplaceAll(msgs[i], `,"cache_control":{"type":"ephemeral"}`, "")
		if a != b {
			t.Errorf("message %d changed\n%s\n%s", i, a, b)
		}
	}
}

func killedInToolLoop(t *testing.T, restart bool) {
	v, url := newVendor(t)
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, url, false)
	tok := readToken(t, home)

	t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody("one"))
	id, _ := startRun(t, svc, tok, "claude", claudeResultA)

	// Turn 2 asks for a tool, then its agent waits, as while the tool runs.
	v.set("tool")
	t.Setenv("AGENTBUS_FAKE_STDOUT", "")
	t.Setenv("AGENTBUS_FAKE_WAIT", filepath.Join(t.TempDir(), "never"))
	t2 := nextTurn(t, svc, tok, id, "run the tests")
	respondedIn(t, svc.store, id, t2)
	if restart {
		// Stopping the service stops the child (SIGTERM), as a restart does.
		svc.stop()
		svc = runServiceWith(t, home, nil, 0, url, false)
	} else {
		if err := syscall.Kill(-childPID(t, home, t2), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	waitClosedN(t, svc, id, 2)
	t.Setenv("AGENTBUS_FAKE_WAIT", "")

	v.set("ok")
	t3 := nextTurn(t, svc, tok, id, "what happened?")
	waitClosedN(t, svc, id, 3)
	m3 := checkRecovered(t, v, svc, id, t2, t3, "it stopped while a tool was running; its agent process was stopped (signal 15, exit 143)",
		`{"tool_use_id":"toolu_slow","type":"tool_result","content":"Not finished: the turn was interrupted before this tool's result came back. It may have run in part.","is_error":true}`)

	t4 := nextTurn(t, svc, tok, id, "go on")
	waitClosedN(t, svc, id, 4)
	checkAfter(t, v, svc, id, t3, t4, m3)
}

func TestRecoverAfterChildKilledInToolLoop(t *testing.T) { killedInToolLoop(t, false) }

func TestRecoverAfterServiceRestartMidTurn(t *testing.T) { killedInToolLoop(t, true) }

// A turn whose only model call got 400 or 529 from the vendor.
func TestRecoverAfterFailedLastCall(t *testing.T) {
	for _, status := range []string{"400", "529"} {
		t.Run(status, func(t *testing.T) {
			v, url := newVendor(t)
			home := t.TempDir()
			svc := runServiceWith(t, home, nil, 0, url, false)
			tok := readToken(t, home)
			t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody("one"))
			id, _ := startRun(t, svc, tok, "claude", claudeResultA)

			v.set(status)
			t.Setenv("AGENTBUS_FAKE_STDOUT", "")
			t.Setenv("AGENTBUS_FAKE_EXIT", "1")
			t2 := nextTurn(t, svc, tok, id, "do it")
			waitClosedN(t, svc, id, 2)

			v.set("ok")
			t.Setenv("AGENTBUS_FAKE_EXIT", "0")
			t3 := nextTurn(t, svc, tok, id, "what happened?")
			waitClosedN(t, svc, id, 3)
			m3 := checkRecovered(t, v, svc, id, t2, t3,
				"its last model request failed with status "+status+"; its agent process exited with code 1", `{"role":"user","content":"do it"}`)
			t4 := nextTurn(t, svc, tok, id, "go on")
			waitClosedN(t, svc, id, 4)
			checkAfter(t, v, svc, id, t3, t4, m3)
		})
	}
}

// A first turn whose agent failed before any model call (as when its sign-in
// expired): the second turn is accepted, and the model is told the first
// message was not acted on, with that message. A third turn follows.
func TestRecoverAfterFirstTurnWithNoCall(t *testing.T) {
	v, url := newVendor(t)
	home := t.TempDir()
	svc := runServiceWith(t, home, nil, 0, url, false)
	tok := readToken(t, home)
	t.Setenv("AGENTBUS_FAKE_BODY", "")
	t.Setenv("AGENTBUS_FAKE_STDOUT", "")
	t.Setenv("AGENTBUS_FAKE_EXIT", "1")
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "fix the parser"})
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %s", code, raw)
	}
	id, t1 := out["id"].(string), out["turn"].(string)
	waitClosedN(t, svc, id, 1)

	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	t2 := nextTurn(t, svc, tok, id, "what happened?")
	waitClosedN(t, svc, id, 2)
	ts := turnStarted(t, svc.store, id, t2)
	want := "it was not acted on: it ended before the model was called (its agent process exited with code 1)"
	if ts.ReplayFrom != t1 || ts.RecoveredFrom != t1 || ts.RecoveredReason != want {
		t.Errorf("turn 2's turn.started %+v", ts)
	}
	m2 := v.last(t)
	if len(m2) < 3 || m2[0] != `{"role":"user","content":"fix the parser"}` ||
		!strings.Contains(m2[1], "The message above was not acted on: it ended before the model was called (its agent process exited with code 1).") {
		t.Errorf("turn 2 sent\n%s", strings.Join(m2, "\n"))
	}
	t3 := nextTurn(t, svc, tok, id, "go on")
	waitClosedN(t, svc, id, 3)
	ts3 := turnStarted(t, svc.store, id, t3)
	m3 := v.last(t)
	if ts3.ReplayFrom != t2 || ts3.RecoveredFrom != "" || countAll(m3, "not acted on") != 1 || m3[0] != m2[0] {
		t.Errorf("turn 3: %+v\n%s", ts3, strings.Join(m3, "\n"))
	}
}

// A turn whose last stream ended before message_stop (after its stop
// reason arrived) is recovered, not refused.
func TestRecoverAfterIncompleteStream(t *testing.T) {
	for _, mode := range []string{"cut-end", "cut-tool"} {
		t.Run(mode, func(t *testing.T) {
			v, url := newVendor(t)
			home := t.TempDir()
			svc := runServiceWith(t, home, nil, 0, url, false)
			tok := readToken(t, home)
			t.Setenv("AGENTBUS_FAKE_BODY", freshClaudeBody("one"))
			id, _ := startRun(t, svc, tok, "claude", claudeResultA)

			v.set(mode)
			t.Setenv("AGENTBUS_FAKE_STDOUT", "")
			t.Setenv("AGENTBUS_FAKE_EXIT", "1")
			t2 := nextTurn(t, svc, tok, id, "do it")
			waitClosedN(t, svc, id, 2)

			v.set("ok")
			t.Setenv("AGENTBUS_FAKE_EXIT", "0")
			t3 := nextTurn(t, svc, tok, id, "what happened?")
			waitClosedN(t, svc, id, 3)
			m3 := checkRecovered(t, v, svc, id, t2, t3,
				"its last reply did not arrive whole (the stream ended before message_stop), so that reply is left out; its agent process exited with code 1",
				`{"role":"user","content":"do it"}`)
			if countAll(m3, "toolu_slow") != 0 {
				t.Error("the incomplete reply was replayed")
			}
			t4 := nextTurn(t, svc, tok, id, "go on")
			waitClosedN(t, svc, id, 4)
			checkAfter(t, v, svc, id, t3, t4, m3)
		})
	}
}
