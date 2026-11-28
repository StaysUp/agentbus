//go:build integration

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// copyRunner is a runner process of a test that needs its process: to stop
// it, and to read its copies on disk.
type copyRunner struct {
	cmd  *exec.Cmd
	home string
	done chan struct{} // closed when its stderr ends, as it exits
}

// startCopyRunner signs a runner in to x's control and starts it with folder,
// as addRunner does, and answers its process and home.
func (x *xproc) startCopyRunner(t *testing.T, name, folder string) *copyRunner {
	t.Helper()
	home := t.TempDir()
	signInRunner(t, x.base, x.tok, home, name)
	env := append(os.Environ(), "AGENTBUS_HOME="+home,
		"PATH="+fakeAgentDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AGENTBUS_FAKE_AGENT=1", "ANTHROPIC_BASE_URL=", "OPENAI_BASE_URL=", "CODEX_HOME="+t.TempDir())
	env = append(env, x.agentEnv...)
	cmd := exec.Command(x.bin, "runner", "-server", x.base, "-name", name, "-folder", folder, "-replay=false")
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rn := &copyRunner{cmd: cmd, home: home, done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(rn.done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		once := false
		for sc.Scan() {
			t.Logf("[runner:%s] %s", name, sc.Text())
			if !once && reRunnerReady.MatchString(sc.Text()) {
				once = true
				close(ready)
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-rn.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	select {
	case <-ready:
	case <-rn.done:
		t.Fatalf("runner %s exited before it was ready", name)
	case <-time.After(30 * time.Second):
		t.Fatalf("runner %s was not ready within 30s", name)
	}
	x.waitFolders(1)
	return rn
}

// copies is where the runner makes its copies (copiesDir).
func (rn *copyRunner) copies() string { return filepath.Join(rn.home, copiesDir) }

// turnState is a turn's state on control.
func (x *xproc) turnState(turn string) string {
	x.t.Helper()
	code, raw := x.get("/api/v0/turns/" + turn)
	var st struct {
		Turn struct {
			State string `json:"state"`
		} `json:"turn"`
	}
	if code != 200 || json.Unmarshal(raw, &st) != nil {
		return ""
	}
	return st.Turn.State
}

// waitLong polls cond every 100 ms until it holds, for at most d.
func waitLong(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// #95: a Copy workspace's first turn stops making its copy when the turn is
// stopped, and when its runner stops (Ctrl-C to agentbus up is the same
// runner stopping). Before, the copy went on to the end: the stop answered
// 202 and the turn stayed running, and the runner did not exit until the copy
// was made. What the copy left has no record: no turn runs in it.
//
// The runner hears a stop at its next heartbeat, up to 2 s after it is sent,
// and a fast machine could finish the copy first (Linux CI, 2026-11-27). So
// the runner holds the copy after its first file until the stop reaches it
// (AGENTBUS_TEST_HOLD_COPY), and the test always stops a copy that is not
// whole.
func TestXProcStopWhileTheCopyIsMade(t *testing.T) {
	// Many files, so the copy is far from whole when it is held. On macOS
	// one file nobody can read makes clonefile refuse the folder, so it is
	// copied file by file there too (as on another volume).
	folder := t.TempDir()
	const files = 30000
	for i := range files {
		d := filepath.Join(folder, fmt.Sprintf("d%03d", i/250))
		if i%250 == 0 {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%05d.txt", i)), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(folder, "unreadable"), []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	for _, how := range []string{"stop", "runner stops"} {
		t.Run(how, func(t *testing.T) {
			x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
			connectFake(t, x.serverHome, "claude")
			addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"},
				append(os.Environ(), "AGENTBUS_HOME="+x.serverHome), reListening)
			x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
			x.setAgentEnv(`AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"copy-stop","result":"done"}`,
				"AGENTBUS_TEST_HOLD_COPY=1")
			rn := x.startCopyRunner(t, "far", folder)
			f := x.folders()[0]
			code, turn := x.startTurnX(map[string]any{"folder_ref": f.Ref, "machine": f.Machine, "agent": "claude", "prompt": "edit"})
			if code/100 != 2 {
				t.Fatalf("turn: %d %+v", code, turn)
			}
			// The copy is under way, and held, once it holds its first file:
			// the copy's top, its first folder and that file.
			var copyDir string
			if !waitLong(30*time.Second, func() bool {
				entries, _ := os.ReadDir(rn.copies())
				for _, e := range entries {
					if !strings.HasPrefix(e.Name(), ".") {
						copyDir = filepath.Join(rn.copies(), e.Name())
						return entriesIn(copyDir) >= 3
					}
				}
				return false
			}) {
				t.Fatal("the copy never started")
			}
			// The folder's every entry but the unreadable file, which no copy
			// holds. A whole copy means the hold did not hold, and the test
			// did not stop a copy being made.
			if n, all := entriesIn(copyDir), entriesIn(folder)-1; n >= all {
				t.Fatalf("the copy was made whole (%d of %d entries) before the stop", n, all)
			}
			stopped := time.Now()
			switch how {
			case "stop":
				if code, raw := x.post("/api/v0/turns/"+turn.Turn+"/stop", nil); code != 202 {
					t.Fatalf("stop: %d %s", code, raw)
				}
				// The runner hears the stop at its next heartbeat (2 s).
				if !waitLong(10*time.Second, func() bool { return x.turnState(turn.Turn) == "stopped" }) {
					t.Fatalf("the turn still reads %q 10s after the stop; the copy holds %d of %d entries",
						x.turnState(turn.Turn), entriesIn(copyDir), entriesIn(folder)-1)
				}
			case "runner stops":
				if err := rn.cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				select {
				case <-rn.done:
				case <-time.After(10 * time.Second):
					t.Fatalf("the runner still runs 10s after Ctrl-C; the copy holds %d of %d entries", entriesIn(copyDir), entriesIn(folder)-1)
				}
			}
			t.Logf("%s: ended %v after it was asked", how, time.Since(stopped).Round(time.Millisecond))
			// What the copy left is never a copy a turn can load: no record,
			// and nothing in the copy's place.
			ws := filepath.Base(copyDir)
			if _, err := os.Stat(filepath.Join(rn.copies(), ".isolation", ws, "space.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a stopped copy has a record: %v", err)
			}
			if _, err := os.Lstat(copyDir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a stopped copy is still in its place: %v", err)
			}
		})
	}
}

// entriesIn is how many entries the folder p holds, at any depth.
func entriesIn(p string) int {
	n := 0
	_ = filepath.WalkDir(p, func(string, fs.DirEntry, error) error { n++; return nil })
	return n
}

// closeWorkspace sends Close, which answers 202 queued at once, and waits for
// its workspace.closed, then for the workspace to read closed later than
// before.
func (x *xproc) closeWorkspace(id string) {
	x.t.Helper()
	closed := func() time.Time {
		code, raw := x.get("/api/v0/workspaces/" + id)
		var w struct {
			Closed *time.Time `json:"closed"`
		}
		if code != 200 || json.Unmarshal(raw, &w) != nil {
			x.t.Fatalf("workspace: %d %s", code, raw)
		}
		if w.Closed == nil {
			return time.Time{}
		}
		return *w.Closed
	}
	before := closed()
	if e := x.act(id, "close", nil); e.Kind != event.WorkspaceClosed {
		x.t.Fatalf("close: %+v", e)
	}
	if !waitLong(30*time.Second, func() bool { return closed().After(before) }) {
		x.t.Fatal("the workspace did not read closed within 30s")
	}
}

// #96 through control and a separate runner: a lock does not stop a close,
// and the turn after it runs in a whole copy. Before, the close removed every
// file of the copy but the locked one, kept the workspace open, and the next
// turn's changes listed the person's files as deleted, so Apply deleted them
// from the folder. "copy closed and locked" is review evt_11939830d9ee167b
// P1: a command in the copy made its top folder chmod 0 and uchg. Before, the
// close took the record away, could not move the copy, and every later turn
// failed on what was left.
func TestXProcCloseWithALockedFile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Finder's Locked is macOS's")
	}
	for _, lock := range []string{"person's file", "copy closed and locked"} {
		t.Run(lock, func(t *testing.T) { closeLocked(t, lock) })
	}
}

func closeLocked(t *testing.T, lock string) {
	folder := t.TempDir()
	for name, body := range map[string]string{"notes.md": "notes\n", "src/app.js": "app\n", "contract.pdf": "pdf\n"} {
		p := filepath.Join(folder, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(folder, "contract.pdf")
	if lock == "person's file" {
		if out, err := exec.Command("chflags", "uchg", locked).CombinedOutput(); err != nil {
			t.Fatalf("chflags: %v %s", err, out)
		}
		t.Cleanup(func() { exec.Command("chflags", "nouchg", locked).Run() })
	}

	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	connectFake(t, x.serverHome, "claude")
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"},
		append(os.Environ(), "AGENTBUS_HOME="+x.serverHome), reListening)
	x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
	x.setAgentEnv("AGENTBUS_FAKE_WRITE=hello.txt=hello\n",
		`AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"close-locked","result":"done"}`)
	rn := x.startCopyRunner(t, "far", folder)
	f := x.folders()[0]
	code, raw := x.post("/api/v0/workspaces", map[string]any{"machine": f.Machine, "folder_ref": f.Ref})
	var ws struct {
		Workspace string `json:"workspace"`
	}
	if code != 201 || json.Unmarshal(raw, &ws) != nil {
		t.Fatalf("workspace: %d %s", code, raw)
	}
	code, raw = x.post("/api/v0/sessions", map[string]any{"workspace": ws.Workspace})
	var session struct {
		Session string `json:"session"`
	}
	if code != 201 || json.Unmarshal(raw, &session) != nil {
		t.Fatalf("session: %d %s", code, raw)
	}
	run := func() {
		t.Helper()
		code, turn := x.startTurnX(map[string]any{"session": session.Session, "agent": "claude", "prompt": "edit"})
		if code/100 != 2 {
			t.Fatalf("turn: %d %+v", code, turn)
		}
		if !waitLong(30*time.Second, func() bool { return x.turnState(turn.Turn) == "finished" }) {
			t.Fatalf("the turn reads %q", x.turnState(turn.Turn))
		}
	}
	base := "/api/v0/workspaces/" + ws.Workspace
	run()
	if e := x.act(ws.Workspace, "apply", map[string]any{}); e.Applied == nil || e.Applied.Result != "applied" {
		t.Fatalf("apply: %+v", e)
	}
	if lock == "copy closed and locked" {
		// As a command in the copy can: its top folder and a file in it
		// closed to everyone and locked.
		root := filepath.Join(rn.copies(), ws.Workspace)
		t.Cleanup(func() {
			filepath.WalkDir(rn.copies(), func(p string, d fs.DirEntry, err error) error {
				exec.Command("chflags", "nouchg", p).Run()
				os.Chmod(p, 0o700)
				return nil
			})
		})
		// The mode first: a locked file's mode cannot change.
		for _, p := range []string{filepath.Join(root, "hello.txt"), root} {
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("chflags", "uchg", p).CombinedOutput(); err != nil {
				t.Fatalf("chflags: %v %s", err, out)
			}
		}
	}
	x.closeWorkspace(ws.Workspace)
	run()
	code, raw = x.get(base + "/changes?fresh=true")
	if code != 200 {
		t.Fatalf("changes: %d %s", code, raw)
	}
	for _, name := range []string{"notes.md", "app.js", "contract.pdf"} {
		if strings.Contains(string(raw), name) {
			t.Fatalf("the turn after the close changed %s: %s", name, raw)
		}
	}
	if e := x.act(ws.Workspace, "apply", map[string]any{}); e.Applied == nil || e.Applied.Result != "applied" {
		t.Fatalf("apply after the close: %+v", e)
	}
	for name, want := range map[string]string{"notes.md": "notes\n", "src/app.js": "app\n", "contract.pdf": "pdf\n", "hello.txt": "hello\n"} {
		if got, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(name))); err != nil || string(got) != want {
			t.Fatalf("the folder's %s: %q %v", name, got, err)
		}
	}
	// The fresh copy goes as the first did.
	x.closeWorkspace(ws.Workspace)
}
