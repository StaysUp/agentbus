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
