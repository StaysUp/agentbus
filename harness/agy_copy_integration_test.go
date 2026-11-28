//go:build integration && (darwin || linux)

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// agyRunCommand serves a Gemini API stream whose first reply calls
// run_command with line, and whose next reply ends the turn.
func agyRunCommand(t *testing.T, line string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		part := map[string]any{"text": "done"}
		if calls.Add(1) == 1 {
			part = map[string]any{"functionCall": map[string]any{"id": "call_1", "name": "run_command",
				"args": map[string]any{"CommandLine": line, "WaitMsBeforeAsync": 10000, "toolSummary": "write"}}}
		}
		b, _ := json.Marshal(map[string]any{
			"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": "STOP"}},
			"usageMetadata": map[string]any{"promptTokenCount": 1, "candidatesTokenCount": 1},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\r\n\r\n", b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An agy turn in Copy mode writes inside its copy, and a command that writes
// outside it is refused; the same turn not in Copy mode writes there. Needs
// agy on PATH; nothing reaches Google, as the model is a local endpoint.
func TestAgyCopyWallsCommands(t *testing.T) {
	bin, err := exec.LookPath("agy")
	if err != nil {
		t.Skip("agy is not installed")
	}
	// Outside the copy, and outside the temp folders agy's sandbox leaves
	// writable: next to this test, as a person's folder is outside them.
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, copyMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("copy=%t", copyMode), func(t *testing.T) {
			outside, err := os.MkdirTemp(here, "agy-outside-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(outside) })
			// By its real path: agy's sandbox lets a command write in its
			// working folder only by that path, and macOS's temp folder is
			// under /var, a link to /private/var (agy 1.5.17, 2026-11-26).
			copyDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			srv := agyRunCommand(t, fmt.Sprintf("echo in > in.txt; echo out > %q", filepath.Join(outside, "x.txt")))
			h := agy{}
			turn := Turn{Writes: Writes{Edit: []string{copyDir}}, Allow: []string{"*"}, Access: API, Prompt: "write"}
			if copyMode {
				turn.Copy = Copy{Dir: copyDir}
			}
			env, args := h.WireAPI(APIWire{Base: srv.URL, Placeholder: "placeholder"})
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, append(args, h.Args(turn)...)...)
			cmd.Dir = copyDir
			cmd.Env = append(os.Environ(), append(env, "HOME="+t.TempDir(), "DBUS_SESSION_BUS_ADDRESS=disabled:")...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("agy: %v\n%s", err, out)
			}
			if b, err := os.ReadFile(filepath.Join(copyDir, "in.txt")); err != nil || string(b) != "in\n" {
				t.Errorf("the write inside the copy: %q %v", b, err)
			}
			_, err = os.Stat(filepath.Join(outside, "x.txt"))
			if copyMode && err == nil {
				t.Error("a Copy turn wrote outside its copy")
			}
			if !copyMode && err != nil {
				t.Errorf("not in Copy mode, the write outside did not happen (%v), so the test shows nothing", err)
			}
		})
	}
}
