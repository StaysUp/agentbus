package runner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/queue"
)

// Review evt_11939830d9ee167b P1: a close that finds a copy with no record
// in its place (an Open stopped part way, a close that stopped after the
// record went) removes it before the workspace reads closed. Before, a copy
// with no record was "nothing of it is left": the close finished and the
// files stayed, and every later turn's copy failed on them.
func TestCloseRemovesACopyWithNoRecord(t *testing.T) {
	data := t.TempDir()
	r := &Runner{cfg: Config{Workdir: filepath.Join(data, "sessions")}}
	left := filepath.Join(r.cfg.Workdir, "ws_0123456789abcdef")
	write(t, filepath.Join(left, "notes.md"), "n\n")
	if err := r.remove(queue.Workspace{Kind: queue.WorkspaceCopy, Space: "ws_0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(left); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the copy with no record is still there: %v", err)
	}
	if _, err := isolation.LoadID(r.cfg.Workdir, "ws_0123456789abcdef"); !errors.Is(err, isolation.ErrNoSpace) {
		t.Fatalf("load: %v", err)
	}
}
