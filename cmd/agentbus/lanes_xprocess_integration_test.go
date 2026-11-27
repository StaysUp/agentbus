//go:build integration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The person's turns are not counted, through control and a separate runner:
// with one delegated place ([limits] max_delegated_at_once = 1) and a runner
// whose own -capacity is 1, three turns sent to three sessions all run at once.
// GET /api/v0/runners says how the delegated lane stands, and a change of the
// setting shows there at once, with no restart of either process. The order
// inside one workspace (the person's message ahead of queued builders) is
// control's alone, tested in queue/lanes_test.go and, with a runner over HTTP,
// TestPersonsMessageGoesAheadOfAskedBuilders.
func TestXProcPersonsTurnsStartAtOnce(t *testing.T) {
	folder := t.TempDir()
	x := &xproc{t: t, bin: agentbusBinary(t), serverHome: t.TempDir()}
	if err := os.WriteFile(filepath.Join(x.serverHome, "permissions.toml"), []byte("[limits]\nmax_delegated_at_once = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connectFake(t, x.serverHome, "claude")
	env := append(os.Environ(), "AGENTBUS_HOME="+x.serverHome)
	addr := x.spawn(t, "server", []string{"server", "-listen", "127.0.0.1:0", "-replay=false"}, env, reListening)
	x.base, x.tok = "http://"+addr, readToken(t, x.serverHome)
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { os.WriteFile(release, nil, 0o600) })
	x.setAgentEnv("AGENTBUS_FAKE_WAIT="+release, `AGENTBUS_FAKE_STDOUT={"type":"result","session_id":"lanes-test","result":"done"}`)
	runnerHome := t.TempDir()
	signInRunner(t, x.base, x.tok, runnerHome, "far")
	fakeDir := fakeAgentDir(t)
	runnerEnv := append(os.Environ(), "AGENTBUS_HOME="+runnerHome,
		"PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AGENTBUS_FAKE_AGENT=1", "ANTHROPIC_BASE_URL=", "OPENAI_BASE_URL=", "CODEX_HOME="+t.TempDir())
	runnerEnv = append(runnerEnv, x.agentEnv...)
	x.spawn(t, "runner:far", []string{"runner", "-server", x.base, "-name", "far", "-capacity", "1", "-folder", folder, "-replay=false"}, runnerEnv, reRunnerReady)
	x.waitFolders(1)
	f := x.folders()[0]

	var turns []string
	for i := range 3 {
		code, turn := x.startTurnX(map[string]any{"machine": f.Machine, "folder_ref": f.Ref, "agent": "claude", "prompt": "p" + strconv.Itoa(i)})
		if code/100 != 2 || turn.Turn == "" {
			t.Fatalf("turn %d: %d %+v", i, code, turn)
		}
		turns = append(turns, turn.Turn)
	}
	running := func() int {
		n := 0
		for _, id := range turns {
			code, raw := x.get("/api/v0/turns/" + id)
			var s struct {
				Turn struct {
					State string `json:"state"`
				} `json:"turn"`
			}
			if code != 200 || json.Unmarshal(raw, &s) != nil {
				t.Fatalf("turn %s: %d %s", id, code, raw)
			}
			if s.Turn.State == "running" {
				n++
			}
		}
		return n
	}
	waitFor(t, "the person's three turns to run at once", func() bool { return running() == 3 })

	type lane struct {
		AtOnce  int `json:"at_once"`
		Busy    int `json:"busy"`
		Waiting int `json:"waiting"`
	}
	runners := func() (lane, lane, int) {
		code, raw := x.get("/api/v0/runners")
		var l struct {
			Runners []struct {
				Capacity  int  `json:"capacity"`
				Delegated lane `json:"delegated"`
			} `json:"runners"`
			Delegated lane `json:"delegated"`
		}
		if code != 200 || json.Unmarshal(raw, &l) != nil || len(l.Runners) != 1 {
			t.Fatalf("runners: %d %s", code, raw)
		}
		return l.Delegated, l.Runners[0].Delegated, l.Runners[0].Capacity
	}
	all, one, capacity := runners()
	if all != (lane{AtOnce: 1}) || one != (lane{AtOnce: 1}) || capacity != 1 {
		t.Fatalf("runners: %+v, runner %+v, capacity %d; want 1 at once, none busy or waiting", all, one, capacity)
	}
	if code, raw := x.do("PUT", "/api/v0/settings/permissions", map[string]any{"limits": map[string]any{"max_delegated_at_once": 3}}); code/100 != 2 {
		t.Fatalf("settings: %d %s", code, raw)
	}
	// The runner's own -capacity of 1 still bounds it.
	if all, one, _ := runners(); all.AtOnce != 3 || one.AtOnce != 1 {
		t.Fatalf("after the change: %+v, runner %+v; want 3 at once, and 1 on the runner of capacity 1", all, one)
	}
	if code, raw := x.do("PUT", "/api/v0/settings/permissions", map[string]any{"limits": map[string]any{"max_delegated_at_once": 65}}); code != 400 {
		t.Fatalf("65 at once: %d %s, want 400", code, raw)
	}
	os.WriteFile(release, nil, 0o600)
	waitFor(t, "the turns to end", func() bool { return running() == 0 })
}
