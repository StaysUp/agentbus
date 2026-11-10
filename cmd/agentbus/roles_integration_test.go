//go:build integration

package main

// The built binary in its roles, over HTTP only, as a person and their
// machines use it: agentbus up on one machine, and agentbus server, runner
// and connector as three processes. A stand-in claude edits the files, so no
// model is called.
//
// TestOneMachineFilesPinned pins what one machine does with a session's
// files: the copy, two turns, their diffs, an apply of each, an apply
// asked again, and a refusal when the person changed a file. It held before
// the connector was split from the runner (framework 40228fa) and must hold
// after.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pinStandIn is claude for these tests. A turn appends its prompt to a.txt,
// adds new.txt when it is missing and removes it when it is there, and
// prints the result line claude prints.
const pinStandIn = `#!/bin/sh
case "$1" in
--version) echo "0.0.0 (agentbus test stand-in)"; exit 0 ;;
auth) exit 0 ;;
esac
for a; do last=$a; done
if [ "$last" = restore ]; then
	printf 'one\n' > a.txt
else
	echo "$last" >> a.txt
	if [ -e new.txt ]; then rm new.txt; else echo "$last" > new.txt; fi
fi
echo '{"type":"result","subtype":"success","result":"done","session_id":"pin-'$$'"}'
`

// built is this module's agentbus, built once per test.
func built(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agentbus")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// freeAddr is a loopback address with a port no one listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// proc is one agentbus process of a test.
type proc struct {
	cmd  *exec.Cmd
	log  string
	done chan struct{}
}

// startProc starts bin with args, its data folder home, and the stand-in
// claude first on its PATH. It is stopped when the test ends.
func startProc(t *testing.T, bin, home, standInDir string, args ...string) *proc {
	t.Helper()
	p := &proc{log: filepath.Join(t.TempDir(), "out.log"), done: make(chan struct{})}
	logf, err := os.Create(p.log)
	if err != nil {
		t.Fatal(err)
	}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Stdout, p.cmd.Stderr = logf, logf
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "AGENTBUS_HOME", "ANTHROPIC_BASE_URL", "OPENAI_BASE_URL":
			continue
		}
		env = append(env, kv)
	}
	p.cmd.Env = append(env, "PATH="+standInDir+string(os.PathListSeparator)+os.Getenv("PATH"), "AGENTBUS_HOME="+home)
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.cmd.Wait(); logf.Close(); close(p.done) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(20 * time.Second):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.done
		}
		if t.Failed() {
			b, _ := os.ReadFile(p.log)
			t.Logf("%s:\n%s", strings.Join(args, " "), b)
		}
	})
	return p
}

// client is the API of a service, with its main token.
type client struct {
	t     *testing.T
	base  string
	token string
}

// waitService waits for the service at addr with data folder home to answer.
func waitService(t *testing.T, p *proc, addr, home string) *client {
	t.Helper()
	c := &client{t: t, base: "http://" + addr}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		select {
		case <-p.done:
			b, _ := os.ReadFile(p.log)
			t.Fatalf("the service ended at start:\n%s", b)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the service did not answer within 30 seconds")
		}
		b, err := os.ReadFile(filepath.Join(home, "token"))
		if err != nil {
			continue
		}
		c.token = strings.TrimSpace(string(b))
		if code, _ := c.call("GET", "/api/v0/version", nil, nil); code == 200 {
			return c
		}
	}
}

func (c *client) call(method, path string, body, out any) (int, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, string(b)
}

type started struct {
	ID, Turn, Job string
}

// turn starts a turn and waits for its job to end, finished.
func (c *client) turn(body map[string]any) started {
	c.t.Helper()
	var s started
	if code, out := c.call("POST", "/api/v0/turns", body, &s); code/100 != 2 || s.Job == "" {
		c.t.Fatalf("POST /api/v0/turns %v: %d %s", body, code, out)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		var j struct {
			State, Reason, Attempt string
			Ended                  *time.Time
			Result                 json.RawMessage
		}
		c.call("GET", "/api/v0/jobs/"+s.Job, nil, &j)
		if j.Ended != nil {
			if j.State != "finished" {
				c.t.Fatalf("job %s ended %s %s: %s", s.Job, j.State, j.Reason, j.Result)
			}
			s.Turn = j.Attempt
			return s
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("job %s did not end", s.Job)
		}
	}
}

// diffFiles is a turn's diff, path to change and the unified diff.
func (c *client) diffFiles(turn string) map[string]string {
	c.t.Helper()
	var d struct {
		Files *struct {
			Isolated bool
			Changes  []struct {
				Path, Change string
				Diff         *string
			}
		}
	}
	if code, out := c.call("GET", "/api/v0/turns/"+turn+"/diff", nil, &d); code != 200 || d.Files == nil {
		c.t.Fatalf("diff of %s: %d %s", turn, code, out)
	}
	got := map[string]string{}
	if !d.Files.Isolated {
		c.t.Fatalf("turn %s did not run in an isolated copy", turn)
	}
	for _, f := range d.Files.Changes {
		s := f.Change
		if f.Diff != nil {
			s += "\n" + *f.Diff
		}
		got[f.Path] = s
	}
	return got
}

type applyAnswer struct {
	State     string
	Already   bool
	Files     []string
	Conflicts []struct{ Path, Why string }
	Error     string
}

func (c *client) apply(turn string) (int, applyAnswer) {
	var a applyAnswer
	code, _ := c.call("POST", "/api/v0/turns/"+turn+"/apply", nil, &a)
	return code, a
}

func fileText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return "<missing>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jobFiles is a turn job's changes as its result holds them, in the shape
// diffFiles answers: what a runner reports, where the turn's record stays
// on the runner (agentbus server, until the record is central).
func (c *client) jobFiles(job string) map[string]string {
	c.t.Helper()
	var j struct {
		Result struct {
			Changes struct {
				From, To string
				Files    []struct {
					Path, Change string
					Diff         *string
				}
			}
		}
	}
	if code, out := c.call("GET", "/api/v0/jobs/"+job, nil, &j); code != 200 || j.Result.Changes.To == "" {
		c.t.Fatalf("job %s: %d %s", job, code, out)
	}
	got := map[string]string{}
	for _, f := range j.Result.Changes.Files {
		s := f.Change
		if f.Diff != nil {
			s += "\n" + *f.Diff
		}
		got[f.Path] = s
	}
	return got
}

// filesFlow runs the pinned flow against c, with folder as the person's
// folder, named by body (a path in agentbus up, a reference otherwise).
// record: the service keeps the turns' record, so their diffs are read
// from GET /api/v0/turns/{id}/diff; else from their jobs.
func filesFlow(t *testing.T, c *client, folder string, body map[string]any, record bool) {
	t.Helper()
	t1 := c.turn(merge(body, map[string]any{"agent": "claude", "prompt": "p1"}))
	t2 := c.turn(map[string]any{"session": t1.ID, "prompt": "p2"})
	// The person's folder is untouched until an apply.
	if got := fileText(t, filepath.Join(folder, "a.txt")); got != "one\n" {
		t.Fatalf("a.txt before any apply: %q", got)
	}
	diff := func(s started) map[string]string {
		if record {
			return c.diffFiles(s.Turn)
		}
		return c.jobFiles(s.Job)
	}
	if got, want := diff(t1), map[string]string{
		"a.txt":   "modified\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1,2 @@\n one\n+p1\n",
		"new.txt": "added\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+p1\n",
	}; !equalMaps(got, want) {
		t.Fatalf("first turn's diff %q, want %q", got, want)
	}
	if got, want := diff(t2), map[string]string{
		"a.txt":   "modified\n--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,3 @@\n one\n p1\n+p2\n",
		"new.txt": "deleted\n--- a/new.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-p1\n",
	}; !equalMaps(got, want) {
		t.Fatalf("second turn's diff %q, want %q", got, want)
	}
	if code, a := c.apply(t1.Turn); code != 200 || a.State != "applied" || strings.Join(a.Files, ",") != "a.txt,new.txt" {
		t.Fatalf("apply of the first turn: %d %+v", code, a)
	}
	if got := fileText(t, filepath.Join(folder, "a.txt")) + fileText(t, filepath.Join(folder, "new.txt")); got != "one\np1\np1\n" {
		t.Fatalf("after the first apply: %q", got)
	}
	if code, a := c.apply(t2.Turn); code != 200 || a.State != "applied" || strings.Join(a.Files, ",") != "a.txt,new.txt" {
		t.Fatalf("apply of the second turn: %d %+v", code, a)
	}
	if got := fileText(t, filepath.Join(folder, "a.txt")) + fileText(t, filepath.Join(folder, "new.txt")); got != "one\np1\np2\n<missing>" {
		t.Fatalf("after the second apply: %q", got)
	}
	// Asked again: the same apply, and nothing written.
	if code, a := c.apply(t2.Turn); code != 200 || !a.Already {
		t.Fatalf("apply asked again: %d %+v", code, a)
	}
	// A new session; the person changes a.txt before applying it: refused,
	// with the file and why, and nothing written.
	t3 := c.turn(merge(body, map[string]any{"agent": "claude", "prompt": "p3"}))
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, a := c.apply(t3.Turn)
	if code != http.StatusConflict || a.State != "refused" || len(a.Conflicts) != 1 || a.Conflicts[0].Path != "a.txt" ||
		a.Conflicts[0].Why != "changed in your folder since the checkpoint" {
		t.Fatalf("apply over the person's change: %d %+v", code, a)
	}
	if got := fileText(t, filepath.Join(folder, "a.txt")) + fileText(t, filepath.Join(folder, "new.txt")); got != "mine\n<missing>" {
		t.Fatalf("after the refused apply: %q", got)
	}
}

// restoreFlow: a turn puts a.txt back as the folder had it, after an
// earlier turn's change was applied; applying it puts the old content back.
// That content is the copy's base, which an apply reads from the store.
func restoreFlow(t *testing.T, c *client, folder string, body map[string]any) {
	t.Helper()
	t1 := c.turn(merge(body, map[string]any{"agent": "claude", "prompt": "p1"}))
	if code, a := c.apply(t1.Turn); code != 200 || a.State != "applied" {
		t.Fatalf("apply of the first turn: %d %+v", code, a)
	}
	t2 := c.turn(map[string]any{"session": t1.ID, "prompt": "restore"})
	if code, a := c.apply(t2.Turn); code != 200 || a.State != "applied" {
		t.Fatalf("apply of the turn that puts a.txt back: %d %+v", code, a)
	}
	if got := fileText(t, filepath.Join(folder, "a.txt")); got != "one\n" {
		t.Fatalf("a.txt after the restore was applied: %q", got)
	}
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// personFolder is a folder of the person's, with the files the flow edits.
func personFolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range map[string]string{"a.txt": "one\n", "b.txt": "keep\n", "sub/c.txt": "c\n"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An hour old, as a person's files are: the copy's base then records
	// them by size, time and mode, as it does on a real folder.
	old := time.Now().Add(-time.Hour)
	filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })
	return dir
}

func standInDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(pinStandIn), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOneMachineFilesPinned(t *testing.T) {
	bin, bins := built(t), standInDir(t)
	home, addr := t.TempDir(), freeAddr(t)
	p := startProc(t, bin, home, bins, "up", "-listen", addr, "-replay=false")
	c := waitService(t, p, addr, home)
	folder := personFolder(t)
	filesFlow(t, c, folder, map[string]any{"folder": folder}, true)
	other := personFolder(t)
	restoreFlow(t, c, other, map[string]any{"folder": other})
}

// The three roles as three processes on one machine: the server holds the
// queue, the record and the blob store; the connector owns the folder; the
// runner owns no folder and runs the agent. Every file goes through the
// server's blob store: the runner builds its copy from the folder's
// snapshot, and the connector applies the changes it reads from there.
func TestThreeProcesses(t *testing.T) {
	bin, bins := built(t), standInDir(t)
	serverHome, addr := t.TempDir(), freeAddr(t)
	sp := startProc(t, bin, serverHome, bins, "server", "-listen", addr, "-replay=false")
	c := waitService(t, sp, addr, serverHome)
	enroll := filepath.Join(serverHome, "enroll_token")
	folder := personFolder(t)
	connHome, runHome := t.TempDir(), t.TempDir()
	startProc(t, bin, connHome, bins, "connector", "-server", c.base, "-folder", folder, "-enroll-token-file", enroll, "-name", "conn")
	startProc(t, bin, runHome, bins, "runner", "-server", c.base, "-enroll-token-file", enroll, "-name", "run", "-replay=false")
	// The folder, by its reference, once the connector reports it.
	var ref, connector string
	waitFor(t, "the connector's folder", func() bool {
		var fs struct {
			Folders []struct{ Ref, Name, Connector string }
		}
		c.call("GET", "/api/v0/folders", nil, &fs)
		for _, f := range fs.Folders {
			if f.Name == filepath.Base(folder) {
				ref, connector = f.Ref, f.Connector
			}
		}
		return ref != ""
	})
	filesFlow(t, c, folder, map[string]any{"folder_ref": ref, "connector": connector, "isolate": true}, true)
	// The same folder again, now as filesFlow left it.
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restoreFlow(t, c, folder, map[string]any{"folder_ref": ref, "connector": connector, "isolate": true})
	// The runner's copy came from the snapshot, not from the folder: its
	// data folder holds the blobs it fetched, and the connector's the ones
	// it sent.
	for home, dir := range map[string]string{runHome: cacheDir, connHome: cacheDir, serverHome: "blobs"} {
		if n := countFiles(t, filepath.Join(home, dir)); n == 0 {
			t.Errorf("no blob in %s", filepath.Join(home, dir))
		}
	}
	if s := fileText(t, filepath.Join(runHome, "folders.json")); strings.Contains(s, folder) {
		t.Errorf("the runner knows the folder's path: %s", s)
	}
	// The server's record holds the runner's turns, as the cockpit reads
	// them, and no path of the runner's.
	var ss struct {
		Store    string
		Sessions []struct{ ID string }
	}
	if code, out := c.call("GET", "/api/v0/sessions", nil, &ss); code != 200 || len(ss.Sessions) < 3 {
		t.Fatalf("the server's sessions: %d %s", code, out)
	}
	for _, se := range ss.Sessions {
		code, out := c.call("GET", "/api/v0/sessions/"+se.ID+"/events?after=0&limit=1000&store="+ss.Store, nil, nil)
		if code != 200 || strings.Contains(out, runHome) || !strings.Contains(out, "turn.started") || !strings.Contains(out, "session.closed") {
			t.Fatalf("the server's record of %s: %d %s", se.ID, code, out)
		}
	}
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}
