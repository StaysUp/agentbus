//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/jobs"
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

// startTurn sends POST /api/v0/turns and answers its status and the job
// that runs the turn, read from svc's own queue: the job is internal, and the
// tests read its result and attempts there.
func startTurn(t *testing.T, svc *service, tok string, body map[string]any) (int, queue.Job) {
	t.Helper()
	var st struct {
		Session, Turn, State string
	}
	code := callAPI(t, svc, tok, "POST", "/api/v0/turns", body, &st)
	if code/100 != 2 {
		return code, queue.Job{}
	}
	if st.Turn == "" {
		t.Fatalf("POST /api/v0/turns: %d with no turn", code)
	}
	return code, jobOfTurn(t, svc, st.Turn)
}

// queueTurn makes a turn job in svc's own queue, as the runner protocol
// carries it: spec is a queue.Spec as JSON, whose agent may name a saved
// agent, read through the API. It is for a test of what a runner does with a
// job, whatever its turn's start answers; POST /api/v0/turns waits for it.
func queueTurn(t *testing.T, svc *service, tok string, spec map[string]any) queue.Job {
	t.Helper()
	b, _ := json.Marshal(spec)
	var sp queue.Spec
	if err := json.Unmarshal(b, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Harness != "" {
		if code, raw := getAPI(t, svc, tok, "/api/v0/agents/"+sp.Harness); code == 200 {
			var ag struct {
				ID      string `json:"id"`
				Harness string `json:"harness"`
				Access  string `json:"access"`
				Model   string `json:"model"`
				KeyEnv  string `json:"key_env"`
			}
			json.Unmarshal([]byte(raw), &ag)
			sp.Harness, sp.AgentID, sp.Access, sp.KeyEnv = ag.Harness, ag.ID, ag.Access, ag.KeyEnv
			if sp.Model == "" {
				sp.Model = ag.Model
			}
		}
	}
	if svc.folders != nil {
		if msg := queue.ResolveFolders(&sp, svc.folders); msg != "" {
			t.Fatal(msg)
		}
	}
	j, _, err := svc.queue.Create(queue.DefaultTeam, queue.KindTurn, "", sp, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// reviewTurn sends POST /api/v0/turns/{turn}/review and answers its status
// and the review's job, from svc's own queue.
func reviewTurn(t *testing.T, svc *service, tok, turn string, body map[string]any) (int, queue.Job) {
	t.Helper()
	var st struct{ Turn string }
	code := callAPI(t, svc, tok, "POST", "/api/v0/turns/"+turn+"/review", body, &st)
	if code/100 != 2 {
		return code, queue.Job{}
	}
	return code, jobOfTurn(t, svc, st.Turn)
}

// jobOfTurn is the job of a turn, from svc's own queue.
func jobOfTurn(t *testing.T, svc *service, turn string) queue.Job {
	t.Helper()
	j, err := svc.queue.JobByTurn(queue.DefaultTeam, turn)
	if err != nil {
		t.Fatalf("the job of turn %s: %v", turn, err)
	}
	return j
}

func waitJob(t *testing.T, svc *service, tok, id string) queue.Job {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		j, err := svc.queue.Get(queue.DefaultTeam, id)
		if err != nil {
			t.Fatalf("GET job: %v", err)
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

	code, j := startTurn(t, svc, tok, map[string]any{"idempotency_key": "one", "isolate": false,
		"agent": "claude", "folder": folder, "prompt": "hello", "model": "haiku"})
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/v0/turns: %d", code)
	}
	j = waitJob(t, svc, tok, j.ID)
	if j.State != queue.Finished {
		t.Fatalf("job %s %s: %s", j.State, j.Reason, j.Result)
	}
	var res struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(j.Result, &res); err != nil || res.Session == "" {
		t.Fatalf("result %s", j.Result)
	}
	// What the turn changed is in the record (turn.changed); the job names it.
	if c, err := jobs.Changes(svc.store, j); err != nil || c == nil || c.Count != 1 {
		t.Fatalf("changes %+v %v, result %s", c, err, j.Result)
	}
	// The job ended, so its turn.changed is in, after the turn's own end.
	var kinds []string
	for _, e := range runEvents(t, svc.store, res.Session) {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) < 4 || kinds[len(kinds)-2] != "session.closed" || kinds[len(kinds)-1] != "turn.changed" {
		t.Fatalf("record of the job's session: %v", kinds)
	}
	// The data folder holds the queue beside the record, and the runner's
	// identity. No enrollment token is written: no other machine can join
	// with one.
	for _, f := range []string{"jobs.db", "runner.json"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "enroll_token")); !os.IsNotExist(err) {
		t.Errorf("enroll_token written: %v", err)
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
	connectFake(t, serverHome)
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

	// The server has no folders of its own, and no connector is connected
	// yet to check a path: it is refused. The runner owns the folder
	// (-folder), and the server knows it by its reference.
	if code, _ := startTurn(t, server, tok, map[string]any{"agent": "claude", "folder": folder, "prompt": "hello"}); code != 503 {
		t.Fatalf("POST /api/v0/turns with a path on the server and no connector: %d, want 503", code)
	}
	fr, err := folders.Open(filepath.Join(runnerHome, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref, connector := folders.Ref(folder), fr.Connector()

	// With no runner yet, a turn waits in the server's queue.
	code, queued := startTurn(t, server, tok, map[string]any{"agent": "claude", "folder_ref": ref, "connector": connector,
		"prompt": "hello", "model": "haiku", "isolate": false})
	if code != 202 {
		t.Fatalf("POST /api/v0/turns: %d", code)
	}
	if strings.Contains(fmt.Sprint(queued.Spec), folder) {
		t.Fatalf("the server keeps the folder's path: %+v", queued.Spec)
	}
	time.Sleep(300 * time.Millisecond)
	if j := waitState(t, server, tok, queued.ID); j.State != queue.Queued {
		t.Fatalf("with no runner the job is %s", j.State)
	}

	signInRunner(t, "http://"+server.addr, tok, runnerHome, "far")
	r, err := start(config{mode: modeRunner, listen: "127.0.0.1:0", home: runnerHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, lookPath: look, noReplay: true,
		server: "http://" + server.addr, runnerName: "far", folders: []string{folder}})
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
	}
	if err := json.Unmarshal(j.Result, &res); err != nil || j.State != queue.Finished || res.State != "finished" ||
		res.Session != j.Session || len(j.Attempts) != 1 || !j.Attempts[0].TurnStarted {
		t.Fatalf("job %s %s: %s", j.State, j.Reason, j.Result)
	}
	// What the turn changed is in the server's record (turn.changed) before
	// the job ends, and the job keeps only its id.
	if c, err := jobs.Changes(server.store, j); err != nil || c == nil || c.Count != 1 || !strings.Contains(string(j.Result), `"changes_event"`) ||
		strings.Contains(string(j.Result), `"changes":`) {
		t.Fatalf("changes %+v %v, result %s", c, err, j.Result)
	}
	turn := j.Turn
	if list, _ := r.store.Turns(store.TurnQuery{Turn: turn, Limit: 1}); len(list.Turns) != 1 || list.Turns[0].Session != j.Session {
		t.Fatalf("the runner's record lacks turn %s", turn)
	}
	// The runner sent the turn to the server's record at its end, without
	// the path of its copy.
	if list, _ := server.store.Turns(store.TurnQuery{Turn: turn, Limit: 1}); len(list.Turns) != 1 || list.Turns[0].Session != j.Session ||
		list.Turns[0].Folder != "" {
		t.Fatalf("the server's record of the runner's turn: %+v", list.Turns)
	}
	// POST /api/v0/turns on the server runs on the runner too, and answers
	// with the turn.
	code, out, raw := postRun(t, server, tok, map[string]any{"agent": "claude", "folder_ref": ref, "connector": connector, "prompt": "again"})
	if code != 202 || out["turn"] == "" || out["state"] != "running" {
		t.Fatalf("POST /api/v0/turns on the server: %d %s", code, raw)
	}
	// The runner reported its folder by reference and its name, never its path.
	var fl struct {
		Folders []queue.RunnerFolder `json:"folders"`
	}
	if code, raw := getAPI(t, server, tok, "/api/v0/folders"); code != 200 || json.Unmarshal([]byte(raw), &fl) != nil ||
		len(fl.Folders) != 1 || fl.Folders[0].Ref != ref || fl.Folders[0].Connector != connector ||
		fl.Folders[0].Name != filepath.Base(folder) || strings.Contains(raw, folder) {
		t.Fatalf("GET /api/v0/folders: %d %s", code, raw)
	}
	// The runner's own service keeps no queue: it takes no turns itself.
	rtok := readToken(t, runnerHome)
	if code, _, raw := postRun(t, r, rtok, map[string]any{"agent": "claude", "folder": folder, "prompt": "x"}); code != 404 {
		t.Fatalf("POST /api/v0/turns on the runner: %d %s, want 404", code, raw)
	}
}

func waitState(t *testing.T, svc *service, tok, id string) queue.Job {
	t.Helper()
	j, err := svc.queue.Get(queue.DefaultTeam, id)
	if err != nil {
		t.Fatalf("GET job: %v", err)
	}
	return j
}
