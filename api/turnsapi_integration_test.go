//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// heldEnv starts an API with a whole stack whose turns run a shell that marks
// <dir>/child-<pid> and waits for <dir>/release. A turn no runner starts
// within 2 seconds answers queued.
func heldEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	// It names its session first, as claude does, so a session whose turn was
	// stopped can still be continued.
	script := `echo '{"type":"result","session_id":"sid-'$$'"}'; : > "$D/child-$$"; i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
	}
	e := stackEnv(t, stack{wire: wire, turnsToken: true, turnWait: 2 * time.Second})
	t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) })
	return e, dir
}

// The person's turns are not counted (queue/lanes.go): with one delegated
// place and a runner whose own capacity is 1, three turns sent to three
// sessions all run at once. Before 0.6.7 the third waited for a place, and on
// 2026-11-26 his messages waited 10+ minutes behind builders his coordinator
// had started.
func TestPersonsTurnsStartAtOnce(t *testing.T) {
	dir := t.TempDir()
	script := `: > "$D/child-$$"; i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
	}
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("[limits]\nmax_delegated_at_once = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := stackEnv(t, stack{wire: wire, turnsToken: true, capacity: 1, permissions: perm})
	t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) })
	folder := t.TempDir()
	for i := range 3 {
		body := `{"session":"` + e.newSession(t, folder, true) + `","agent":"claude","prompt":"p` + strconv.Itoa(i) + `"}`
		if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", body); code != 202 {
			t.Fatalf("turn %d: %d %s", i, code, out)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); children(t, dir) < 3; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of the person's 3 turns run at once, want 3", children(t, dir))
		}
	}
}

func children(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "child-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// Requests sent at once under one key start one turn and one child, and all
// of them name that turn. GET /api/v0/turns/{id} follows it to its end.
func TestIdempotentStartStartsOneChild(t *testing.T) {
	e, dir := heldEnv(t)
	sess := e.newSession(t, t.TempDir(), true)
	body := `{"session":"` + sess + `","agent":"claude","prompt":"hello","idempotency_key":"job-1"}`
	const n = 6
	var wg sync.WaitGroup
	got := make([]Started, n)
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", body)
			codes[i] = code
			_ = json.Unmarshal([]byte(out), &got[i])
		}()
	}
	wg.Wait()
	fresh := 0
	for i := range n {
		if codes[i] != 202 || got[i].Turn != got[0].Turn || got[i].Session != got[0].Session {
			t.Fatalf("request %d: %d %+v, first %+v", i, codes[i], got[i], got[0])
		}
		if !got[i].Existing {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d requests started a turn, want 1", fresh)
	}
	for deadline := time.Now().Add(5 * time.Second); children(t, dir) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	time.Sleep(100 * time.Millisecond)
	if c := children(t, dir); c != 1 {
		t.Fatalf("%d children, want 1", c)
	}
	list, err := e.store.Turns(store.TurnQuery{Limit: 10})
	if err != nil || len(list.Turns) != 1 {
		t.Fatalf("%d turns stored, want 1 (%v)", len(list.Turns), err)
	}

	path := "/api/v0/turns/" + got[0].Turn
	if code, out := asTok(t, e, turnsTok, "GET", path, ""); code != 200 || !strings.Contains(out, `"state":"running"`) {
		t.Fatalf("while running: %d %s", code, out)
	}
	os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
	waitRunEnded(t, e.store, got[0].Session)
	// It reads running until its job has ended too.
	waitEnded(t, e, got[0].Turn)
	code, out := asTok(t, e, turnsTok, "GET", path, "")
	var one OneTurn
	_ = json.Unmarshal([]byte(out), &one)
	if code != 200 || one.Turn.State != store.TurnFinished || one.Turn.ExitCode == nil || *one.Turn.ExitCode != 0 ||
		one.Turn.Ended == nil || one.Turn.IdempotencyKey != "job-1" {
		t.Fatalf("after it ended: %d %s", code, out)
	}
	// After the turn ended, a retry still names it and starts nothing.
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", body); code != 202 || !strings.Contains(out, got[0].Turn) {
		t.Fatalf("a retry after the end: %d %s", code, out)
	}
	if c := children(t, dir); c != 1 {
		t.Fatalf("%d children after a retry, want 1", c)
	}
}

// untilStarted sends post again, a retry under one idempotency key, until its
// answer is no longer queued: POST /api/v0/turns answers at once, and a retry
// of a turn a runner has started answers running.
func untilStarted(t *testing.T, post func() (int, string)) (int, Started, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, out := post()
		var s Started
		_ = json.Unmarshal([]byte(out), &s)
		if code != 202 || s.State != "queued" || time.Now().After(deadline) {
			return code, s, out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An idempotency key is the turn job's key. A retry with the same key and
// fields answers the same turn, existing true, and starts nothing; other
// fields under it get 409; a bad key gets 400 and is never queued.
func TestIdempotencyKey(t *testing.T) {
	var wired atomic.Int32
	e := turnsEnv(t, func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		wired.Add(1)
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0", "sh"}}, nil
	})
	sess := e.newSession(t, t.TempDir(), true)
	body := `{"session":"` + sess + `","agent":"claude","prompt":"hello","idempotency_key":"job-7:a1"}`
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var first Started
	_ = json.Unmarshal([]byte(out), &first)
	if resp.StatusCode != 202 || first.Turn == "" || first.Existing || first.State != "queued" {
		t.Fatalf("first: %d %s, want 202 queued at once", resp.StatusCode, out)
	}
	// A retry once a runner has started it says running, with its gateway.
	code, started, out := untilStarted(t, func() (int, string) {
		resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
		return resp.StatusCode, out
	})
	if code != 202 || started.Turn != first.Turn || !started.Existing || started.State != "running" ||
		started.BaseURL != e.api.cfg.BaseURL+"/s/"+first.Session+"/t/"+first.Turn {
		t.Fatalf("retry once started: %d %s", code, out)
	}
	waitRunEnded(t, e.store, first.Session)
	resp, out = do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var s Started
	_ = json.Unmarshal([]byte(out), &s)
	if resp.StatusCode != 202 || s.Session != first.Session || s.Turn != first.Turn || !s.Existing {
		t.Fatalf("retry: %d %s, first %+v", resp.StatusCode, out, first)
	}
	// The turns token gets the same answer.
	if got, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", body); got != 202 || !strings.Contains(out, `"existing":true`) {
		t.Fatalf("retry with the turns token: %d %s", got, out)
	}
	if n := wired.Load(); n != 1 {
		t.Fatalf("the agent was wired %d times, want 1", n)
	}
	// The same key with other fields gets 409: another prompt, another model, or
	// another session. Each is a different request under a key the first took.
	sess2 := e.newSession(t, t.TempDir(), true)
	for name, b := range map[string]string{
		"another prompt":  `{"session":"` + sess + `","agent":"claude","prompt":"other","idempotency_key":"job-7:a1"}`,
		"another model":   `{"session":"` + sess + `","agent":"claude","prompt":"hello","model":"haiku","idempotency_key":"job-7:a1"}`,
		"another session": `{"session":"` + sess2 + `","agent":"claude","prompt":"hello","idempotency_key":"job-7:a1"}`,
	} {
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode != 409 {
			t.Errorf("%s: %d %s, want 409", name, resp.StatusCode, out)
		}
	}
	for _, k := range []string{`"a b"`, `"` + strings.Repeat("k", 129) + `"`, `"é"`} {
		b := `{"session":"` + sess + `","agent":"claude","prompt":"hello","idempotency_key":` + k + `}`
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode != 400 {
			t.Errorf("key %s: %d %s, want 400", k, resp.StatusCode, out)
		}
	}
	// One turn job: the idempotent request is answered from the first.
	if jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindTurn}, 10); len(jobs) != 1 {
		t.Errorf("%d turn jobs, want the one", len(jobs))
	}
}

// A second turn of a session is queued while its first runs: the answer
// says queued, with its turn and session. A turn of another session starts at
// once beside them (the person's turns are not counted), and the turns token
// may stop it. Once the first has ended the second runs, and the same request
// with its key then names its turn.
func TestQueuedBehindItsSessionAndStopWithTurnsToken(t *testing.T) {
	e, dir := heldEnv(t)
	folder := t.TempDir()
	sess1 := e.newSession(t, folder, true)
	code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", `{"session":"`+sess1+`","agent":"claude","prompt":"one"}`)
	if code != 202 {
		t.Fatalf("first: %d %s", code, out)
	}
	for deadline := time.Now().Add(10 * time.Second); children(t, dir) < 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first turn did not start")
		}
	}
	second := `{"session":"` + sess1 + `","agent":"claude","prompt":"two","idempotency_key":"k2"}`
	code, out = asTok(t, e, turnsTok, "POST", "/api/v0/turns", second)
	var queued Started
	_ = json.Unmarshal([]byte(out), &queued)
	if code != 202 || queued.State != "queued" || queued.Turn == "" || queued.Session != sess1 || queued.Ahead != 1 {
		t.Fatalf("second: %d %s, want 202 queued behind the first", code, out)
	}
	code, out = asTok(t, e, turnsTok, "POST", "/api/v0/turns", `{"session":"`+e.newSession(t, folder, true)+`","agent":"claude","prompt":"other"}`)
	var other Started
	_ = json.Unmarshal([]byte(out), &other)
	if code != 202 {
		t.Fatalf("other: %d %s", code, out)
	}
	// The stop is sent once the agent runs, so it stops a running turn and
	// the run ends. (A stop while the copy is still being made ends the turn
	// before any run starts, so no run.ended would come: #95.)
	for deadline := time.Now().Add(10 * time.Second); children(t, dir) < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a turn of another session did not start beside the first")
		}
	}
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns/"+other.Turn+"/stop", ""); code != 202 {
		t.Fatalf("stop: %d %s", code, out)
	}
	waitRunEnded(t, e.store, other.Session)
	// A stopped turn reads stopped: one word, whichever side is asked.
	if _, one := asTok(t, e, turnsTok, "GET", "/api/v0/turns/"+other.Turn, ""); !strings.Contains(one, `"stopped":true`) || !strings.Contains(one, `"state":"stopped"`) {
		t.Fatalf("the stopped turn: %s", one)
	}
	if j := waitEnded(t, e, other.Turn); j.State != queue.Cancelled || queue.TurnState(j.State) != "stopped" {
		t.Fatalf("the stopped turn's job: %s", j.State)
	}
	// A stop of a turn that has ended gets 409; of no turn, 404.
	if code, _ := asTok(t, e, turnsTok, "POST", "/api/v0/turns/"+other.Turn+"/stop", ""); code != 409 {
		t.Fatalf("a second stop: %d, want 409", code)
	}
	if code, _ := asTok(t, e, turnsTok, "POST", "/api/v0/turns/t_none/stop", ""); code != 404 {
		t.Fatalf("a stop of no turn: %d, want 404", code)
	}
	os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
	code, st, out := untilStarted(t, func() (int, string) { return asTok(t, e, turnsTok, "POST", "/api/v0/turns", second) })
	if code != 202 || st.State == "queued" || st.Turn != queued.Turn || !st.Existing || st.Session != sess1 {
		t.Fatalf("second, after the first ended: %d %s", code, out)
	}
}
