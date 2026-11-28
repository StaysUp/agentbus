//go:build integration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/wall"
)

// wallingSrt is a folder holding an srt stand-in: it answers --version with
// the pinned version and runs the -c command after making the probe's out
// folder read-only, so the probe sees a write outside its wall refused.
func wallingSrt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		`if [ "$1" = --version ]; then echo ` + wall.Version + "; exit 0; fi\n" +
		`while [ "$1" != -c ]; do shift; done` + "\n" +
		`chmod 0500 out; trap 'chmod 0700 out' EXIT; /bin/sh -c "$2"` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "srt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// spawnWallRunner starts agentbus runner in its own process, signed in to
// control, owning folder, with a PATH of the fake agent, extra (when set)
// and the system's own folders, so srt is there only when extra holds it.
func spawnWallRunner(t *testing.T, x *xproc, name, folder, extra string) string {
	t.Helper()
	home := t.TempDir()
	signInRunner(t, x.base, x.tok, home, name)
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(home, "logs", "service.log"))
			t.Logf("runner %s service.log:\n%s", name, b)
		}
	})
	path := fakeAgentDir(t) + string(os.PathListSeparator)
	if extra != "" {
		path += extra + string(os.PathListSeparator)
	}
	path += "/usr/bin:/bin"
	env := append(os.Environ(), "AGENTBUS_HOME="+home, "PATH="+path, "AGENTBUS_FAKE_AGENT=1",
		"ANTHROPIC_BASE_URL=", "OPENAI_BASE_URL=", "CODEX_HOME="+t.TempDir(), "AGENTBUS_FAKE_MARK=made.txt")
	x.spawn(t, "runner:"+name, []string{"runner", "-server", x.base, "-name", name, "-folder", folder, "-replay=false"}, env, reRunnerReady)
	fr, err := folders.Open(filepath.Join(home, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	return fr.Machine()
}

// wallOf is the wall report control lists for the runner named name, or nil.
func wallOf(t *testing.T, x *xproc, name string) map[string]any {
	t.Helper()
	code, raw := x.get("/api/v0/runners")
	var l struct {
		Runners []struct {
			Name string         `json:"name"`
			Wall map[string]any `json:"wall"`
		} `json:"runners"`
	}
	if code != 200 || json.Unmarshal(raw, &l) != nil {
		t.Fatalf("GET /api/v0/runners: %d %s", code, raw)
	}
	for _, r := range l.Runners {
		if r.Name == name {
			return r.Wall
		}
	}
	return nil
}

// Step 1 of the one wall, with control and separate runner processes: each
// runner probes srt and control lists what it found. A turn that must run in
// a wall is refused by a runner that cannot build it, before any copy or
// agent, and the reason is recorded on the turn: srt missing on one machine;
// on the other srt holds the probe's wall, but this build puts no turn in it
// yet. A turn with no wall runs as before.
func TestXProcWallReportAndRefusal(t *testing.T) {
	serverHome := t.TempDir()
	connectFake(t, serverHome)
	server, err := start(config{mode: modeServer, listen: "127.0.0.1:0", home: serverHome, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, heartbeat: 100 * time.Millisecond, turnWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.stop)
	tok := readToken(t, serverHome)
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: serverHome, base: "http://" + server.addr, tok: tok}

	bare, walled := t.TempDir(), t.TempDir()
	bareMachine := spawnWallRunner(t, x, "bare", bare, "")
	walledMachine := spawnWallRunner(t, x, "walled", walled, wallingSrt(t))
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	waitFor("both wall reports", func() bool { return wallOf(t, x, "bare") != nil && wallOf(t, x, "walled") != nil })
	if w := wallOf(t, x, "bare"); w["engine"] != "srt" || w["problem"] != wall.NotInstalled || w["version"] != nil {
		t.Fatalf("the runner with no srt reports %v", w)
	}
	if w := wallOf(t, x, "walled"); w["engine"] != "srt" || w["version"] != wall.Version || w["problem"] != nil ||
		len(w["levels"].([]any)) != 0 {
		t.Fatalf("the runner whose srt holds the wall reports %v", w)
	}

	refusedOn := func(folder, machine, want string) {
		t.Helper()
		j := queueTurn(t, server, tok, map[string]any{"folder_ref": folders.Ref(folder), "connector": machine,
			"isolate": true, "agent": "claude", "prompt": "hi", "wall": "copy"})
		j = waitJob(t, server, tok, j.ID)
		var res queue.Result
		json.Unmarshal(j.Result, &res)
		if j.State != queue.Failed || !strings.Contains(res.Refused, want) || len(j.Attempts) != 1 || j.Attempts[0].TurnStarted {
			t.Fatalf("a walled turn on %s: %s %s, want failed before it started, with %q", machine, j.State, j.Result, want)
		}
		code, raw := x.get("/api/v0/turns/" + j.Turn)
		if code != 200 || !strings.Contains(string(raw), `"failed"`) || !strings.Contains(string(raw), want) {
			t.Fatalf("GET the refused turn: %d %s", code, raw)
		}
		if _, err := os.Stat(filepath.Join(folder, "made.txt")); !os.IsNotExist(err) {
			t.Fatalf("the agent ran in %s", folder)
		}
	}
	refusedOn(bare, bareMachine, "cannot build one: "+wall.NotInstalled)
	refusedOn(walled, walledMachine, "this runner's build cannot put one around a turn")

	// A turn with no wall runs on the runner with no srt, as before.
	j := queueTurn(t, server, tok, map[string]any{"folder_ref": folders.Ref(bare), "connector": bareMachine,
		"agent": "claude", "prompt": "hi"})
	if j = waitJob(t, server, tok, j.ID); j.State != queue.Finished {
		t.Fatalf("a turn with no wall: %s %s", j.State, j.Result)
	}
}
