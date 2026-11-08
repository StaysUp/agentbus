//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// heldEnv starts an API with a whole stack whose turns run a shell that marks
// <dir>/child-<pid> and waits for <dir>/release, with at most maxRuns at
// once. A turn no runner starts within 2 seconds answers queued.
func heldEnv(t *testing.T, maxRuns int) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	script := `: > "$D/child-$$"; i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
	}
	e := stackEnv(t, stack{wire: wire, maxRuns: maxRuns, turnsToken: true, turnWait: 2 * time.Second})
	t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) })
	return e, dir
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
	e, dir := heldEnv(t, 4)
	body := `{"agent":"claude","folder":"` + t.TempDir() + `","prompt":"hello","idempotency_key":"job-1"}`
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
		if codes[i] != 202 || got[i].Turn != got[0].Turn || got[i].ID != got[0].ID {
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
	waitSessionClosed(t, e.store, got[0].ID)
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

// An idempotency key is the turn job's key. A retry with the same key and
// fields answers the same turn, existing true, and starts nothing; other
// fields under it get 409; a bad key gets 400 and is never queued.
func TestIdempotencyKey(t *testing.T) {
	var wired atomic.Int32
	e := turnsEnv(t, func(agent, baseURL string) (run.Launch, error) {
		wired.Add(1)
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", "exit 0", "sh"}}, nil
	})
	dir := t.TempDir()
	body := `{"agent":"claude","folder":"` + dir + `","prompt":"hello","idempotency_key":"job-7:a1"}`
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var first Started
	_ = json.Unmarshal([]byte(out), &first)
	if resp.StatusCode != 202 || first.Turn == "" || first.Existing || first.Job == "" ||
		first.BaseURL != e.api.cfg.BaseURL+"/s/"+first.ID+"/t/"+first.Turn {
		t.Fatalf("first: %d %s", resp.StatusCode, out)
	}
	waitSessionClosed(t, e.store, first.ID)
	resp, out = do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	var s Started
	_ = json.Unmarshal([]byte(out), &s)
	if resp.StatusCode != 202 || s.ID != first.ID || s.Turn != first.Turn || !s.Existing || s.Job != first.Job {
		t.Fatalf("retry: %d %s, first %+v", resp.StatusCode, out, first)
	}
	// The turns token gets the same answer.
	if got, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", body); got != 202 || !strings.Contains(out, `"existing":true`) {
		t.Fatalf("retry with the turns token: %d %s", got, out)
	}
	if n := wired.Load(); n != 1 {
		t.Fatalf("the agent was wired %d times, want 1", n)
	}
	for name, b := range map[string]string{
		"another prompt": `{"agent":"claude","folder":"` + dir + `","prompt":"other","idempotency_key":"job-7:a1"}`,
		"another model":  `{"agent":"claude","folder":"` + dir + `","prompt":"hello","model":"haiku","idempotency_key":"job-7:a1"}`,
		"read_only":      `{"agent":"claude","folder":"` + dir + `","prompt":"hello","read_only":true,"idempotency_key":"job-7:a1"}`,
		"with a session": `{"session":"` + first.ID + `","prompt":"hello","idempotency_key":"job-7:a1"}`,
	} {
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode != 409 {
			t.Errorf("%s: %d %s, want 409", name, resp.StatusCode, out)
		}
	}
	for _, k := range []string{`"a b"`, `"` + strings.Repeat("k", 129) + `"`, `"é"`} {
		b := `{"agent":"claude","folder":"` + dir + `","prompt":"hello","idempotency_key":` + k + `}`
		if resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(b))); resp.StatusCode != 400 {
			t.Errorf("key %s: %d %s, want 400", k, resp.StatusCode, out)
		}
	}
	if jobs, _ := e.q.List(queue.DefaultTeam, queue.Filter{}, 10); len(jobs) != 1 {
		t.Errorf("%d jobs, want the one", len(jobs))
	}
}

// With MaxRuns 1 a second turn is queued while the first runs: the answer
// says queued, with its job and session and no turn. The turns token may stop
// a turn. Once the first has ended the second runs, and the same request
// with its key then names its turn.
func TestMaxRunsQueuesAndStopWithTurnsToken(t *testing.T) {
	e, dir := heldEnv(t, 1)
	folder := t.TempDir()
	code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", `{"agent":"claude","folder":"`+folder+`","prompt":"one"}`)
	if code != 202 {
		t.Fatalf("first: %d %s", code, out)
	}
	var first Started
	_ = json.Unmarshal([]byte(out), &first)
	second := `{"agent":"claude","folder":"` + folder + `","prompt":"two","idempotency_key":"k2"}`
	code, out = asTok(t, e, turnsTok, "POST", "/api/v0/turns", second)
	var queued Started
	_ = json.Unmarshal([]byte(out), &queued)
	if code != 202 || !queued.Queued || queued.Turn != "" || queued.Job == "" || queued.ID == "" {
		t.Fatalf("second: %d %s, want 202 queued", code, out)
	}
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns/"+first.Turn+"/stop", ""); code != 202 {
		t.Fatalf("stop: %d %s", code, out)
	}
	waitSessionClosed(t, e.store, first.ID)
	code, out = asTok(t, e, turnsTok, "POST", "/api/v0/turns", second)
	var s Started
	_ = json.Unmarshal([]byte(out), &s)
	if code != 202 || s.Queued || s.Turn == "" || !s.Existing || s.ID != queued.ID || s.Job != queued.Job {
		t.Fatalf("second, after the first ended: %d %s", code, out)
	}
	if _, one := asTok(t, e, turnsTok, "GET", "/api/v0/turns/"+first.Turn, ""); !strings.Contains(one, `"stopped":true`) {
		t.Fatalf("the stopped turn: %s", one)
	}
	if j, _ := e.q.Get(queue.DefaultTeam, first.Job); j.State != queue.Cancelled {
		t.Fatalf("the stopped turn's job: %s", j.State)
	}
	// A stop of a turn that has ended gets 409; of no turn, 404.
	if code, _ := asTok(t, e, turnsTok, "POST", "/api/v0/turns/"+first.Turn+"/stop", ""); code != 409 {
		t.Fatalf("a second stop: %d, want 409", code)
	}
	if code, _ := asTok(t, e, turnsTok, "POST", "/api/v0/turns/t_none/stop", ""); code != 404 {
		t.Fatalf("a stop of no turn: %d, want 404", code)
	}
	_ = dir
}
