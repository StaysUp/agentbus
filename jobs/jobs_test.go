package jobs

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

const (
	mainTok   = "abm_MAIN-TOKEN-NEVER-ECHOED"
	enrollTok = "abe_ENROLL-TOKEN-NEVER-ECHOED"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := newServerQ(t)
	return srv
}

// newServerQ is newServer and its queue, where the tests make jobs: the
// client API that makes them is package api's.
func newServerQ(t *testing.T) (*httptest.Server, *queue.Queue) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fr, err := testFolders(t)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Folders: fr})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close() })
	return srv, q
}

// makeJob makes a turn job of spec, its folder kept by its reference.
func makeJob(t *testing.T, q *queue.Queue, spec queue.Spec) queue.Job {
	t.Helper()
	fr, err := testFolders(t)
	if err != nil {
		t.Fatal(err)
	}
	if msg := queue.ResolveFolders(&spec, fr); msg != "" {
		t.Fatal(msg)
	}
	j, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", spec, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	return j
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
	if code := send(t, srv, enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "r", Capacity: capacity, Protocol: ProtocolVersion,
		Capabilities: &queue.Capabilities{Connector: testConnector}}, &e); code != 201 {
		t.Fatalf("enroll: %d", code)
	}
	if !strings.HasPrefix(e.Token, runnerTokenPrefix) || len(e.Token) != len(runnerTokenPrefix)+64 {
		t.Fatalf("runner token shape: %d", len(e.Token))
	}
	return e
}

// Each token reaches its own routes only, and a runner reaches only its own
// attempts. The main token reaches none of the runner protocol.
func TestTokenScopes(t *testing.T) {
	srv, q := newServerQ(t)
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	j := makeJob(t, q, queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"})
	for _, c := range []struct {
		tok, method, path string
		body              any
		want              int
	}{
		{mainTok, "GET", "/api/v0/runner/settings", nil, 401},
		{enrollTok, "POST", "/api/v0/runner/take", TakeRequest{}, 401},
		{mainTok, "POST", "/api/v0/runner/take", TakeRequest{}, 401},
		{mainTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "x", Capacity: 1, Protocol: ProtocolVersion}, 401},
		{a.Token, "POST", "/api/v0/runner/register", EnrollRequest{Name: "x", Capacity: 1, Protocol: ProtocolVersion}, 401},
		{"abr_wrong", "POST", "/api/v0/runner/take", TakeRequest{}, 401},
		{enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "x", Capacity: 1, Protocol: 99}, 400},
		{enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "x", Capacity: 65, Protocol: ProtocolVersion}, 400},
	} {
		if got := send(t, srv, c.tok, c.method, c.path, c.body, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	var taken TakenWork
	if code := send(t, srv, a.Token, "POST", "/api/v0/runner/take", TakeRequest{}, &taken); code != 200 || taken.Job.ID != j.ID {
		t.Fatalf("take: %d %+v", code, taken)
	}
	att := "/api/v0/runner/attempts/" + taken.Attempt.ID
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
	if _, err := q.Cancel(queue.DefaultTeam, j.ID); err != nil {
		t.Errorf("cancel: %v", err)
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
	srv, q := newServerQ(t)
	a := enroll(t, srv, 1)
	path := "/api/v0/runner/take"
	start := time.Now()
	if got := send(t, srv, a.Token, "POST", path, TakeRequest{WaitMS: 100}, nil); got != 204 || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("empty take: %d after %s", got, time.Since(start))
	}
	// The job is made from another goroutine that never touches t, and the
	// test waits for it, so a slow take cannot leave it running past the end.
	made := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Session: "s1", Prompt: "next"}, queue.Links{})
		made <- err
	}()
	var taken TakenWork
	got := send(t, srv, a.Token, "POST", path, TakeRequest{WaitMS: 5000}, &taken)
	if err := <-made; err != nil {
		t.Fatal(err)
	}
	if got != 200 || taken.Job.Spec.Session != "s1" {
		t.Fatalf("take while a job is made: %d %+v", got, taken)
	}
}

// A runner's capabilities are checked at enroll and on its heartbeat.
func TestEnrollCapabilities(t *testing.T) {
	srv := newServer(t)
	bad := &queue.Capabilities{Connector: "con_short"}
	if code := send(t, srv, enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "r", Capacity: 1, Protocol: ProtocolVersion, Capabilities: bad}, nil); code != 400 {
		t.Fatalf("bad capabilities at enroll: %d", code)
	}
	if code := send(t, srv, enrollTok, "POST", "/api/v0/runner/register", EnrollRequest{Name: "r", Capacity: 1, Protocol: ProtocolVersion, Labels: []string{"a b"}}, nil); code != 400 {
		t.Fatalf("bad label at enroll: %d", code)
	}
	e := enroll(t, srv, 1)
	if code := send(t, srv, e.Token, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 1, Capabilities: bad}, nil); code != 400 {
		t.Fatalf("bad capabilities on a heartbeat: %d", code)
	}
	var rn queue.Runner
	good := &queue.Capabilities{OS: "linux", Arch: "arm64", Connector: testConnector}
	if code := send(t, srv, e.Token, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 1, Capabilities: good}, &rn); code != 200 ||
		rn.Caps == nil || rn.Caps.Arch != "arm64" {
		t.Fatalf("heartbeat: %d %+v", code, rn.Caps)
	}
}

// Sessions: removed for the runner's own sessions only, and the idle
// heartbeat, for the runner the token names.
func TestSessionRoutes(t *testing.T) {
	srv := newServer(t)
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	if got := send(t, srv, a.Token, "POST", "/api/v0/runner/removed", RemovedReport{Session: "s_none"}, nil); got != 404 {
		t.Fatalf("removed unknown: %d", got)
	}
	if got := send(t, srv, b.Token, "POST", "/api/v0/runner/removed", RemovedReport{Session: "s"}, nil); got != 404 {
		t.Fatalf("removed as another runner: %d", got)
	}
	var rn queue.Runner
	if got := send(t, srv, a.Token, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 5}, &rn); got != 200 || rn.Capacity != 5 {
		t.Fatalf("idle heartbeat: %d %+v", got, rn)
	}
	if got := send(t, srv, b.Token, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 1}, &rn); got != 200 || rn.ID != b.Runner.ID {
		t.Fatalf("b's heartbeat: %d %+v", got, rn)
	}
	if got := send(t, srv, a.Token, "POST", "/api/v0/runner/heartbeat", RunnerBeat{Capacity: 0}, nil); got != 400 {
		t.Fatalf("capacity 0: %d", got)
	}
}

// A runner reads the server's settings with its own token, fresh on each
// call: a hand edit on the server is in the next read. A token that is not
// a runner's is refused, and a broken file is 503 with why.
func TestRunnerSettings(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Settings: settings.NewFiles(dir)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close() })
	a, b := enroll(t, srv, 1), enroll(t, srv, 1)
	path := "/api/v0/runner/settings"
	var got settings.RunnerSettings
	if code := send(t, srv, a.Token, "GET", path, nil, &got); code != 200 || got.Instructions != settings.DefaultInstructions || got.Permissions.Edit != settings.EditSession {
		t.Fatalf("no files: %d %+v", code, got)
	}
	if err := os.WriteFile(filepath.Join(dir, settings.PolicyFile), []byte("forbid = [\"git push\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := send(t, srv, a.Token, "GET", path, nil, &got); code != 200 || !slices.Equal(got.Permissions.Forbid, []string{"git push"}) {
		t.Fatalf("after a hand edit: %d %+v", code, got)
	}
	if code := send(t, srv, b.Token, "GET", path, nil, nil); code != 200 {
		t.Errorf("another runner's own read: %d", code)
	}
	for _, tok := range []string{mainTok, enrollTok} {
		if code := send(t, srv, tok, "GET", path, nil, nil); code != 401 {
			t.Errorf("another token: %d", code)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, settings.PolicyFile), []byte("forbid = git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := send(t, srv, a.Token, "GET", path, nil, nil); code != 503 {
		t.Fatalf("a broken file: %d", code)
	}
}

// A runner gets the settings version with the settings, from one read, so a
// turn on it records the version it ran under.
func TestRunnerSettingsVersion(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := settings.OpenDB(filepath.Join(t.TempDir(), settings.DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetInstructions("mine", settings.Any); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Settings: db})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close(); db.Close() })
	a := enroll(t, srv, 1)
	var got settings.RunnerSettings
	if code := send(t, srv, a.Token, "GET", "/api/v0/runner/settings", nil, &got); code != 200 || got.SettingsVersion != 1 || got.Instructions != "mine" {
		t.Fatalf("%d %+v", code, got)
	}
}

// testConnector is the connector id of testFolders: the runners the tests
// enroll report it, so they take the jobs that name its folders.
const testConnector = "con_0123456789abcdef"

func testFolders(t *testing.T) (*folders.Registry, error) {
	p := filepath.Join(t.TempDir(), "folders.json")
	if err := os.WriteFile(p, []byte(`{"connector":"`+testConnector+`","folders":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return folders.Open(p)
}
