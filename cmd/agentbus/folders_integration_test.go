//go:build integration

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
)

// GET /api/v0/folders never lists a session copy: not one in this data
// folder's sessions/ or worktrees/, and not a 0.2 copy that 0.3.0's
// migration registered from a data folder since moved (his folders.json,
// 2026-11-10: job_2f2e608012a9ca19). The entries stay in folders.json.
func TestFoldersLeaveOutCopies(t *testing.T) {
	home, mine := t.TempDir(), t.TempDir()
	fr, err := folders.Open(filepath.Join(home, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	copies := []string{"/old/home/worktrees/job_2f2e608012a9ca19", filepath.Join(home, "sessions", "ses_0123456789abcdef")}
	for _, f := range copies {
		if _, err := fr.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, noReplay: true, folders: []string{mine},
		heartbeat: 100 * time.Millisecond, turnWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)
	var raw string
	waitFor(t, "the folder list", func() bool {
		var fl struct {
			Folders []queue.RunnerFolder `json:"folders"`
		}
		var code int
		code, raw = getAPI(t, svc, tok, "/api/v0/folders")
		return code == 200 && json.Unmarshal([]byte(raw), &fl) == nil && len(fl.Folders) > 0
	})
	if !strings.Contains(raw, folders.Ref(mine)) {
		t.Fatalf("the person's folder is not listed: %s", raw)
	}
	for _, f := range copies {
		if strings.Contains(raw, folders.Ref(f)) || strings.Contains(raw, filepath.Base(f)) {
			t.Errorf("the copy %s is listed: %s", f, raw)
		}
		if got, ok := svc.folders.Path(folders.Ref(f)); !ok || got != f {
			t.Errorf("the copy %s is no longer registered", f)
		}
	}
}
