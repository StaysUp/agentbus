//go:build integration

package api

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/store"
)

// heldEnv starts an API whose turns run a shell that marks <dir>/child-<pid>
// and waits for <dir>/release, with at most maxRuns at once.
func heldEnv(t *testing.T, maxRuns int) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	script := `: > "$D/child-$$"; i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string) (Launch, error) {
		return Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
	}
	a, err := New(Config{Store: st, Token: token, TurnsToken: turnsTok, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Wire: wire, MaxRuns: maxRuns})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() {
		os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
		srv.Close()
		a.StopRuns()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port}, dir
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

// With MaxRuns 1 a second turn gets 429 while the first runs, and its key is
// not used, so the same request starts a turn once the first has ended. The
// turns token may stop a turn.
func TestMaxRunsAndStopWithTurnsToken(t *testing.T) {
	e, dir := heldEnv(t, 1)
	folder := t.TempDir()
	code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", `{"agent":"claude","folder":"`+folder+`","prompt":"one"}`)
	if code != 202 {
		t.Fatalf("first: %d %s", code, out)
	}
	var first Started
	_ = json.Unmarshal([]byte(out), &first)
	second := `{"agent":"claude","folder":"` + folder + `","prompt":"two","idempotency_key":"k2"}`
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", second); code != 429 || !strings.Contains(out, "1 runs") {
		t.Fatalf("second: %d %s, want 429", code, out)
	}
	if code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns/"+first.Turn+"/stop", ""); code != 202 {
		t.Fatalf("stop: %d %s", code, out)
	}
	waitSessionClosed(t, e.store, first.ID)
	// The slot is freed just after session.closed is stored.
	var got int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if got, out = asTok(t, e, turnsTok, "POST", "/api/v0/turns", second); got != 429 {
			break
		}
	}
	if got != 202 || strings.Contains(out, `"existing"`) {
		t.Fatalf("second, after the first ended: %d %s", got, out)
	}
	if _, one := asTok(t, e, turnsTok, "GET", "/api/v0/turns/"+first.Turn, ""); !strings.Contains(one, `"stopped":true`) {
		t.Fatalf("the stopped turn: %s", one)
	}
	_ = dir
}
