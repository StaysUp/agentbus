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
)

// Own replay's history budget and a mid-turn compaction, across processes:
// control (agentbus server) and a runner (agentbus runner) are separate
// processes, and the runner's own replay builds each turn's history (review
// of cbb539f: no test ran either across control and a runner).

const xEnv = `{"role":"system","content":[{"type":"text","text":"# Environment\nenv\n\nToday's date is d."}]}`

func xFresh(extra ...string) string {
	msgs := append([]string{`{"role":"user","content":[{"type":"text","text":"@PROMPT@"}]}`, xEnv}, extra...)
	return `{"model":"claude-x","messages":[` + strings.Join(msgs, ",") + `],"tools":[{"name":"Bash","input_schema":{}}],"stream":true}`
}

// xVendor is a fake Anthropic that answers every call with streamedReply and
// keeps every body it got.
type xVendor struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newXVendor(t *testing.T) *xVendor {
	v := &xVendor{}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.bodies = append(v.bodies, string(b))
		v.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamedReply)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// with is the bodies the vendor got that hold s.
func (v *xVendor) with(s string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for _, b := range v.bodies {
		if strings.Contains(b, s) {
			out = append(out, b)
		}
	}
	return out
}

// runTurns runs prompts as one session's turns on the runner, one after the
// other, each to its end, and returns the session.
func (x *xproc) runTurns(folderRef, machine string, prompts ...string) string {
	t := x.t
	t.Helper()
	session := ""
	for i, p := range prompts {
		body := map[string]any{"agent": "claude", "prompt": p}
		if session == "" {
			body["folder_ref"], body["machine"], body["isolate"] = folderRef, machine, false
		} else {
			body["session"] = session
		}
		code, st := x.startTurnX(body)
		if code/100 != 2 || st.Turn == "" {
			t.Fatalf("turn %d: %d %+v", i+1, code, st)
		}
		session = st.Session
		waitFor(t, "turn "+p+" ended", func() bool {
			code, b := x.get("/api/v0/turns/" + st.Turn)
			var tr struct {
				Turn struct {
					State string `json:"state"`
				} `json:"turn"`
			}
			return code == 200 && json.Unmarshal(b, &tr) == nil && tr.Turn.State != "" && tr.Turn.State != "queued" && tr.Turn.State != "running"
		})
		if _, b := x.get("/api/v0/turns/" + st.Turn); !strings.Contains(string(b), `"state":"finished"`) {
			t.Fatalf("turn %q did not finish: %s", p, b)
		}
	}
	return session
}

func TestXProcHistoryBudget(t *testing.T) {
	folder := t.TempDir()
	v := newXVendor(t)
	x := startServerUpstream(t, v.srv.URL, "claude")
	// The budget is set on control only: the runner reads it from control.
	// 0.004 of claude-x's 1M window is 4,000 tokens; the default (0.4) would
	// never shorten these turns.
	req, _ := http.NewRequest("PUT", x.base+"/api/v0/settings/compaction", strings.NewReader(`{"budget":0.004}`))
	req.Header.Set("Authorization", "Bearer "+x.tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("set the budget: %v %v", err, resp)
	}
	resp.Body.Close()
	// Each turn runs one tool whose result is ~6 KB (~2,300 tokens).
	big, _ := json.Marshal(strings.Repeat("output line of kiwi\n", 300))
	loop := xFresh(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cat log"}}]}`,
		`{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":`+string(big)+`,"is_error":false}]}`)
	bodies, _ := json.Marshal(map[string][]string{"*": {xFresh(), loop}})
	x.setAgentEnv("AGENTBUS_FAKE_BODIES=" + string(bodies))
	x.addRunner("far", folder)
	fs := x.folders()
	session := x.runTurns(fs[0].Ref, fs[0].Machine, "turn one", "turn two", "turn three", "turn four")

	// The vendor got a shortened history, built by the runner's replay.
	if got := v.with("Earlier turns of this session, in short."); len(got) == 0 {
		t.Fatal("no request to the vendor carried a shortened history")
	}
	// The step reached control's record from the runner.
	waitFor(t, "history.shortened in control's record", func() bool {
		return hasKind(x.recordKinds(session), "history.shortened")
	})
	// Every request stays under the budget plus the turn's own work.
	for _, b := range v.with("turn four") {
		if len(b) > 40000 {
			t.Errorf("turn four sent %d bytes: the history was not kept within the budget", len(b))
		}
	}
}

func TestXProcCompactionMidTurn(t *testing.T) {
	folder := t.TempDir()
	v := newXVendor(t)
	x := startServerUpstream(t, v.srv.URL, "claude")
	compacted := `{"model":"claude-x","messages":[{"role":"user","content":"HARNESS SUMMARY of the turn"},` + xEnv + `],"tools":[{"name":"Bash","input_schema":{}}],"stream":true}`
	bodies, _ := json.Marshal(map[string][]string{"turn two": {xFresh(), compacted}, "*": {xFresh()}})
	x.setAgentEnv("AGENTBUS_FAKE_BODIES=" + string(bodies))
	x.addRunner("far", folder)
	fs := x.folders()
	session := x.runTurns(fs[0].Ref, fs[0].Machine, "turn one", "turn two", "turn three")

	// The compacted request went to the vendor as the harness sent it.
	if len(v.with("HARNESS SUMMARY")) != 1 {
		t.Fatalf("the compacted request did not reach the vendor once: %d", len(v.with("HARNESS SUMMARY")))
	}
	waitFor(t, "history.compacted in control's record", func() bool {
		return hasKind(x.recordKinds(session), "history.compacted")
	})
	// Turn three is built from the record, not from the harness's summary.
	three := v.with("turn three")
	if len(three) == 0 {
		t.Fatal("turn three reached no vendor")
	}
	if strings.Contains(three[0], "HARNESS SUMMARY") || !strings.Contains(three[0], "compacted its context") {
		t.Errorf("turn three's history carries the harness's summary or no note:\n%s", three[0])
	}
}
