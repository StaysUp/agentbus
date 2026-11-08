//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// callAPI sends a JSON body (or none) with the token and decodes a 2xx reply.
func callAPI(t *testing.T, svc *service, tok, method, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+svc.addr+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func waitJob(t *testing.T, svc *service, tok, id string) queue.Job {
	t.Helper()
	var j queue.Job
	for deadline := time.Now().Add(20 * time.Second); ; {
		if code := callAPI(t, svc, tok, "GET", "/api/v0/jobs/"+id, nil, &j); code != 200 {
			t.Fatalf("GET job: %d", code)
		}
		if j.Ended != nil {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not end: %s", id, j.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// One process, one address, one token: a job made through the API runs on
// the built-in runner, and the turn it ran is in the record.
func TestUpRunsAJobOnItsOwnRunner(t *testing.T) {
	home := t.TempDir()
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	folder := t.TempDir()
	t.Setenv("AGENTBUS_FAKE_MARK", filepath.Join(folder, "made.txt"))

	var j queue.Job
	code := callAPI(t, svc, tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn", "idempotency_key": "one",
		"spec": map[string]any{"agent": "claude", "folder": folder, "prompt": "hello", "model": "haiku"}}, &j)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/v0/jobs: %d", code)
	}
	j = waitJob(t, svc, tok, j.ID)
	if j.State != queue.Finished {
		t.Fatalf("job %s %s: %s", j.State, j.Reason, j.Result)
	}
	var res struct {
		Session string `json:"session"`
		Changes struct {
			Count int `json:"count"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(j.Result, &res); err != nil || res.Session == "" || res.Changes.Count != 1 {
		t.Fatalf("result %s", j.Result)
	}
	if evs := runEvents(t, svc.store, res.Session); len(evs) < 3 || evs[len(evs)-1].Kind != "session.closed" {
		t.Fatalf("record of the job's session: %+v", evs)
	}
	// The data folder holds the queue beside the record, and the runner's
	// identity.
	for _, f := range []string{"jobs.db", "enroll_token", "runner.json"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// Two processes' worth of agentbus: a server with the queue and no runner,
// and a runner with its own gateway and record that takes jobs from it. A
// job made on the server runs on the runner and ends there; its result comes
// back to the server. The turn's events stay in the runner's record.
func TestServerAndRunner(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	look := func(string) (string, error) { return self, nil }
	serverHome, runnerHome := t.TempDir(), t.TempDir()
	server, err := start(config{mode: modeServer, listen: "127.0.0.1:0", home: serverHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, heartbeat: 100 * time.Millisecond, turnWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.stop)
	if server.eng != nil {
		t.Fatal("a server has an engine: it runs no turns")
	}
	tok := readToken(t, serverHome)
	folder := t.TempDir()
	t.Setenv("AGENTBUS_FAKE_MARK", filepath.Join(folder, "made.txt"))

	// With no runner yet, a turn waits in the server's queue.
	var queued queue.Job
	if code := callAPI(t, server, tok, "POST", "/api/v0/jobs", map[string]any{"kind": "turn",
		"spec": map[string]any{"agent": "claude", "folder": folder, "prompt": "hello", "model": "haiku"}}, &queued); code != 201 {
		t.Fatalf("POST /api/v0/jobs: %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if j := waitState(t, server, tok, queued.ID); j.State != queue.Queued {
		t.Fatalf("with no runner the job is %s", j.State)
	}

	r, err := start(config{mode: modeRunner, listen: "127.0.0.1:0", home: runnerHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, lookPath: look, noReplay: true,
		server: "http://" + server.addr, enrollFile: filepath.Join(serverHome, "enroll_token"), runnerName: "far"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.stop)
	if r.queue != nil {
		t.Fatal("a runner keeps a queue")
	}
	j := waitJob(t, server, tok, queued.ID)
	var res struct {
		Session string `json:"session"`
		State   string `json:"state"`
		Changes struct {
			Count int `json:"count"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(j.Result, &res); err != nil || j.State != queue.Finished || res.State != "finished" ||
		res.Session != j.Session || res.Changes.Count != 1 || len(j.Attempts) != 1 || !j.Attempts[0].TurnStarted {
		t.Fatalf("job %s %s: %s", j.State, j.Reason, j.Result)
	}
	turn := j.Attempts[0].ID
	if list, _ := r.store.Turns(store.TurnQuery{Turn: turn, Limit: 1}); len(list.Turns) != 1 || list.Turns[0].Session != j.Session {
		t.Fatalf("the runner's record lacks turn %s", turn)
	}
	if list, _ := server.store.Turns(store.TurnQuery{Turn: turn, Limit: 1}); len(list.Turns) != 0 {
		t.Fatal("the server's record holds the runner's turn")
	}
	// POST /api/v0/turns on the server runs on the runner too, and answers
	// with the turn.
	code, out, raw := postRun(t, server, tok, map[string]any{"agent": "claude", "folder": folder, "prompt": "again"})
	if code != 202 || out["turn"] == "" || out["queued"] == true {
		t.Fatalf("POST /api/v0/turns on the server: %d %s", code, raw)
	}
	// The runner's own service keeps no queue: it takes no turns itself.
	rtok := readToken(t, runnerHome)
	if code, _, raw := postRun(t, r, rtok, map[string]any{"agent": "claude", "folder": folder, "prompt": "x"}); code != 404 {
		t.Fatalf("POST /api/v0/turns on the runner: %d %s, want 404", code, raw)
	}
}

func waitState(t *testing.T, svc *service, tok, id string) queue.Job {
	t.Helper()
	var j queue.Job
	if code := callAPI(t, svc, tok, "GET", "/api/v0/jobs/"+id, nil, &j); code != 200 {
		t.Fatalf("GET job: %d", code)
	}
	return j
}
