package jobs

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
)

const (
	mainTok   = "abm_MAIN-TOKEN-NEVER-ECHOED"
	enrollTok = "abe_ENROLL-TOKEN-NEVER-ECHOED"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close() })
	return srv
}

func send(t *testing.T, srv *httptest.Server, tok, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, secret := range []string{mainTok, enrollTok} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s %s: a token is in the reply", method, path)
		}
	}
	if out != nil && resp.StatusCode/100 == 2 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func enroll(t *testing.T, srv *httptest.Server, capacity int) Enrolled {
	t.Helper()
	var e Enrolled
	if code := send(t, srv, enrollTok, "POST", "/api/v0/runners", EnrollRequest{Name: "r", Capacity: capacity, Protocol: 1}, &e); code != 201 {
		t.Fatalf("enroll: %d", code)
	}
	if !strings.HasPrefix(e.Token, runnerTokenPrefix) || len(e.Token) != len(runnerTokenPrefix)+64 {
		t.Fatalf("runner token shape: %d", len(e.Token))
	}
	return e
}

// Each token reaches its own routes only, and a runner reaches only its own
// runner id and its own attempts.
func TestTokenScopes(t *testing.T) {
	srv := newServer(t)
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	job := JobRequest{Kind: "turn", Spec: queue.Spec{Agent: "claude", Folder: "/x", Prompt: "hi"}}
	var j queue.Job
	if code := send(t, srv, mainTok, "POST", "/api/v0/jobs", job, &j); code != 201 {
		t.Fatalf("create: %d", code)
	}
	for _, c := range []struct {
		tok, method, path string
		body              any
		want              int
	}{
		{a.Token, "POST", "/api/v0/jobs", job, 401},
		{a.Token, "GET", "/api/v0/jobs", nil, 401},
		{enrollTok, "GET", "/api/v0/jobs", nil, 401},
		{mainTok, "POST", "/api/v0/runners/" + a.Runner.ID + "/take", TakeRequest{}, 401},
		{mainTok, "POST", "/api/v0/runners", EnrollRequest{Name: "x", Capacity: 1, Protocol: 1}, 401},
		{a.Token, "POST", "/api/v0/runners", EnrollRequest{Name: "x", Capacity: 1, Protocol: 1}, 401},
		{"abr_wrong", "POST", "/api/v0/runners/" + a.Runner.ID + "/take", TakeRequest{}, 401},
		// b may not take as a.
		{b.Token, "POST", "/api/v0/runners/" + a.Runner.ID + "/take", TakeRequest{}, 404},
		{enrollTok, "POST", "/api/v0/runners", EnrollRequest{Name: "x", Capacity: 1, Protocol: 2}, 400},
		{enrollTok, "POST", "/api/v0/runners", EnrollRequest{Name: "x", Capacity: 65, Protocol: 1}, 400},
	} {
		if got := send(t, srv, c.tok, c.method, c.path, c.body, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runners/"+a.Runner.ID+"/take", TakeRequest{}, &taken); code != 200 || taken.Job.ID != j.ID {
		t.Fatalf("take: %d %+v", code, taken)
	}
	att := "/api/v0/attempts/" + taken.Attempt.ID
	// b cannot touch a's attempt: not found, not refused, so it learns nothing.
	for _, op := range []string{"start", "heartbeat"} {
		if got := send(t, srv, b.Token, "POST", att+"/"+op, AttemptCall{Number: 1}, nil); got != 404 {
			t.Errorf("b %s on a's attempt: %d, want 404", op, got)
		}
	}
	if got := send(t, srv, b.Token, "POST", att+"/finish", Finishing(1, queue.Finish{Status: "finished"}), nil); got != 404 {
		t.Errorf("b finish on a's attempt: %d, want 404", got)
	}
	// The wrong number is stale.
	if got := send(t, srv, a.Token, "POST", att+"/start", AttemptCall{Number: 2}, nil); got != 409 {
		t.Errorf("wrong number: %d, want 409", got)
	}
	if got := send(t, srv, a.Token, "POST", att+"/start", AttemptCall{Number: 1}, nil); got != 200 {
		t.Errorf("start: %d", got)
	}
	// An unknown field is refused.
	if got := send(t, srv, a.Token, "POST", att+"/heartbeat", map[string]any{"number": 1, "x": 1}, nil); got != 400 {
		t.Errorf("unknown field: %d, want 400", got)
	}
	var beat AttemptBeat
	if got := send(t, srv, a.Token, "POST", att+"/heartbeat", AttemptCall{Number: 1}, &beat); got != 200 || beat.Cancel {
		t.Errorf("heartbeat: %d %+v", got, beat)
	}
	if got := send(t, srv, mainTok, "POST", "/api/v0/jobs/"+j.ID+"/cancel", nil, nil); got != 202 {
		t.Errorf("cancel: %d", got)
	}
	if got := send(t, srv, a.Token, "POST", att+"/heartbeat", AttemptCall{Number: 1}, &beat); got != 200 || !beat.Cancel {
		t.Errorf("heartbeat after cancel: %d %+v", got, beat)
	}
	if got := send(t, srv, a.Token, "POST", att+"/finish", Finishing(1, queue.Finish{Status: "cancelled"}), &j); got != 200 || j.State != "cancelled" {
		t.Errorf("finish: %d %+v", got, j)
	}
}

// A take with nothing queued waits, then answers 204; a job created while it
// waits is answered at once.
func TestTakeLongPoll(t *testing.T) {
	srv := newServer(t)
	a := enroll(t, srv, 1)
	path := "/api/v0/runners/" + a.Runner.ID + "/take"
	start := time.Now()
	if got := send(t, srv, a.Token, "POST", path, TakeRequest{WaitMS: 100}, nil); got != 204 || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("empty take: %d after %s", got, time.Since(start))
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		send(t, srv, mainTok, "POST", "/api/v0/jobs", JobRequest{Kind: "turn", Spec: queue.Spec{Session: "s1", Prompt: "next"}}, nil)
	}()
	var taken TakenWork
	if got := send(t, srv, a.Token, "POST", path, TakeRequest{WaitMS: 5000}, &taken); got != 200 || taken.Job.Spec.Session != "s1" {
		t.Fatalf("take while a job is made: %d %+v", got, taken)
	}
}

// Job creation: refusals, the idempotency key, and a review is read-only.
func TestCreateJob(t *testing.T) {
	srv := newServer(t)
	for _, b := range []any{
		JobRequest{Kind: "build", Spec: queue.Spec{Agent: "claude", Folder: "/x", Prompt: "hi"}},
		JobRequest{Kind: "turn", Spec: queue.Spec{Agent: "claude", Prompt: "hi"}},
		JobRequest{Kind: "turn", Spec: queue.Spec{Agent: "claude", Folder: "/x"}},
		JobRequest{Kind: "turn", IdempotencyKey: "a b", Spec: queue.Spec{Agent: "claude", Folder: "/x", Prompt: "hi"}},
		map[string]any{"kind": "turn", "spec": map[string]any{"prompt": "x", "session": "s"}, "extra": 1},
	} {
		if got := send(t, srv, mainTok, "POST", "/api/v0/jobs", b, nil); got != 400 {
			t.Errorf("%+v: %d, want 400", b, got)
		}
	}
	rev := JobRequest{Kind: "review", IdempotencyKey: "rev-1", Spec: queue.Spec{Agent: "codex", Folder: "/x", Prompt: "review"},
		Links: queue.Links{ReviewsJob: "job_1"}}
	var one, two queue.Job
	if got := send(t, srv, mainTok, "POST", "/api/v0/jobs", rev, &one); got != 201 || !one.Spec.ReadOnly || one.Links.ReviewsJob != "job_1" {
		t.Fatalf("review: %d %+v", got, one)
	}
	if got := send(t, srv, mainTok, "POST", "/api/v0/jobs", rev, &two); got != 200 || two.ID != one.ID {
		t.Fatalf("retry: %d %+v", got, two)
	}
	rev.Spec.Prompt = "other"
	if got := send(t, srv, mainTok, "POST", "/api/v0/jobs", rev, nil); got != 409 {
		t.Fatalf("other fields: %d", got)
	}
	var list JobList
	if got := send(t, srv, mainTok, "GET", "/api/v0/jobs?state=queued", nil, &list); got != 200 || len(list.Jobs) != 1 {
		t.Fatalf("list: %d %d", got, len(list.Jobs))
	}
	if got := send(t, srv, mainTok, "GET", "/api/v0/jobs?state=nope", nil, nil); got != 400 {
		t.Fatalf("bad state: %d", got)
	}
	if got := send(t, srv, mainTok, "GET", "/api/v0/jobs/job_none", nil, nil); got != 404 {
		t.Fatalf("unknown job: %d", got)
	}
}

// A first job names agent with folder, or with repo and base; a later job
// names session, and may name agent. A base can never be read as a flag.
func TestSpecShapes(t *testing.T) {
	srv := newServer(t)
	for _, c := range []struct {
		spec queue.Spec
		want int
	}{
		{queue.Spec{Agent: "claude", Repo: "/r", Base: "main", Prompt: "x"}, 201},
		{queue.Spec{Agent: "claude", Repo: "/r", Base: "feature/a-1", Prompt: "x"}, 201},
		{queue.Spec{Agent: "claude", Folder: "/f", Prompt: "x"}, 201},
		{queue.Spec{Session: "s1", Prompt: "x"}, 201},
		{queue.Spec{Agent: "claude", Repo: "/r", Base: "-c", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Repo: "/r", Base: "a b", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Repo: "/r", Base: "a..b", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Repo: "/r", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Repo: "rel", Base: "main", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Folder: "rel", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Folder: "/f", Repo: "/r", Base: "main", Prompt: "x"}, 400},
		{queue.Spec{Agent: "claude", Base: "main", Prompt: "x"}, 400},
		{queue.Spec{Session: "s1", Folder: "/f", Prompt: "x"}, 400},
		{queue.Spec{Session: "s1", Agent: "claude", Prompt: "x"}, 201},
		{queue.Spec{Session: "s1", Isolate: true, Prompt: "x"}, 400},
		{queue.Spec{Session: "s1", Repo: "/r", Base: "main", Prompt: "x"}, 400},
	} {
		if got := send(t, srv, mainTok, "POST", "/api/v0/jobs", JobRequest{Kind: "turn", Spec: c.spec}, nil); got != c.want {
			t.Errorf("%+v: %d, want %d", c.spec, got, c.want)
		}
	}
}

// Sessions: list, close an unknown one, removed and the idle heartbeat for
// the runner's own id only.
func TestSessionRoutes(t *testing.T) {
	srv := newServer(t)
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	var list WorkspaceList
	if got := send(t, srv, mainTok, "GET", "/api/v0/workspaces", nil, &list); got != 200 || list.Sessions == nil {
		t.Fatalf("list: %d %+v", got, list)
	}
	if got := send(t, srv, mainTok, "POST", "/api/v0/workspaces/s_none/close", nil, nil); got != 404 {
		t.Fatalf("close unknown: %d", got)
	}
	if got := send(t, srv, a.Token, "GET", "/api/v0/workspaces", nil, nil); got != 401 {
		t.Fatalf("a runner lists workspaces: %d", got)
	}
	if got := send(t, srv, a.Token, "POST", "/api/v0/runners/"+a.Runner.ID+"/removed", RemovedReport{Session: "s_none"}, nil); got != 404 {
		t.Fatalf("removed unknown: %d", got)
	}
	if got := send(t, srv, b.Token, "POST", "/api/v0/runners/"+a.Runner.ID+"/removed", RemovedReport{Session: "s"}, nil); got != 404 {
		t.Fatalf("removed as another runner: %d", got)
	}
	var rn queue.Runner
	if got := send(t, srv, a.Token, "POST", "/api/v0/runners/"+a.Runner.ID+"/heartbeat", RunnerBeat{Capacity: 5}, &rn); got != 200 || rn.Capacity != 5 {
		t.Fatalf("idle heartbeat: %d %+v", got, rn)
	}
	if got := send(t, srv, b.Token, "POST", "/api/v0/runners/"+a.Runner.ID+"/heartbeat", RunnerBeat{Capacity: 1}, nil); got != 404 {
		t.Fatalf("heartbeat as another runner: %d", got)
	}
	if got := send(t, srv, a.Token, "POST", "/api/v0/runners/"+a.Runner.ID+"/heartbeat", RunnerBeat{Capacity: 0}, nil); got != 400 {
		t.Fatalf("capacity 0: %d", got)
	}
}

// The review route's refusals, and the list filters' checks.
func TestReviewRoute(t *testing.T) {
	srv := newServer(t)
	var j queue.Job
	send(t, srv, mainTok, "POST", "/api/v0/jobs", JobRequest{Kind: "turn", Spec: queue.Spec{Agent: "claude", Folder: "/f", Prompt: "x"}}, &j)
	for _, c := range []struct {
		path string
		body any
		want int
	}{
		{"/api/v0/jobs/job_none/review", ReviewRequest{Agent: "claude"}, 404},
		{"/api/v0/jobs/" + j.ID + "/review", ReviewRequest{Agent: "claude"}, 409},
		{"/api/v0/jobs/" + j.ID + "/review", ReviewRequest{Agent: "-x"}, 400},
		{"/api/v0/jobs/" + j.ID + "/review", ReviewRequest{Agent: "claude", Model: "a b"}, 400},
		{"/api/v0/jobs/" + j.ID + "/review", map[string]any{"agent": "claude", "extra": 1}, 400},
	} {
		if got := send(t, srv, mainTok, "POST", c.path, c.body, nil); got != c.want {
			t.Errorf("%s %+v: %d, want %d", c.path, c.body, got, c.want)
		}
	}
	if got := send(t, srv, enrollTok, "POST", "/api/v0/jobs/"+j.ID+"/review", ReviewRequest{Agent: "claude"}, nil); got != 401 {
		t.Errorf("review with the enrollment token: %d", got)
	}
	var list JobList
	if got := send(t, srv, mainTok, "GET", "/api/v0/jobs?kind=turn&session=s&reviews=job_x", nil, &list); got != 200 || len(list.Jobs) != 0 {
		t.Errorf("filters: %d %d", got, len(list.Jobs))
	}
	if got := send(t, srv, mainTok, "GET", "/api/v0/jobs?kind=build", nil, nil); got != 400 {
		t.Errorf("bad kind: %d", got)
	}
}
