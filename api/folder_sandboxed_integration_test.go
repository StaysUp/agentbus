//go:build integration && (darwin || linux)

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/netjail"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/sandbox"
)

// canSandbox skips where this machine cannot build the wall.
func canSandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	box, err := sandbox.New(sandbox.Spec{Copy: dir, Home: dir, Net: netjail.Spec{Open: true}})
	if err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	box.Close()
}

// B8 done-when: A sandboxed turn reads a granted outside file mid-turn,
// and direct shell reads to outside files fail (sandbox stays tight).
func TestSandboxedTurnReadsGrantedOutsideFileMidTurn(t *testing.T) {
	canSandbox(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("SANDBOX_OUTSIDE_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	permDir := t.TempDir()
	permFile := filepath.Join(permDir, "permissions.toml")
	if err := os.WriteFile(permFile, []byte("[sandbox]\nmode = \"on\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	homesDir := filepath.Join(t.TempDir(), "homes")

	script := fmt.Sprintf(`cat %q > "$HOME/direct.txt" 2>/dev/null || true
curl -s -X POST "$AGENTBUS_URL/api/v0/mcp" \
  -H "Authorization: Bearer $AGENTBUS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}' \
  > "$HOME/mcp.txt"
echo '{"type":"result","session_id":"s1"}'
exit 0`, outsideFile, outsideFile)

	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script}}, nil
	}

	e := stackEnv(t, stack{
		wire:        wire,
		permissions: permFile,
		homes:       homesDir,
		turnWait:    5 * time.Second,
	})

	folder := t.TempDir()
	started := e.firstTurn(t, folder, "claude", "hi", true)

	// Mid-turn hold: the tool call is held at the gate waiting for approval.
	id := waitForApproval(t, e)
	if id == "" {
		t.Fatal("tool call did not hold for approval")
	}

	// Person grants the folder access mid-turn with "always for this folder".
	answerApproval(t, e, id, "always for this folder")

	// Wait for turn job to finish.
	waitEnded(t, e, started.Turn)

	home := filepath.Join(homesDir, started.Session)

	// Direct shell access failed (sandbox stayed tight).
	directB, _ := os.ReadFile(filepath.Join(home, "direct.txt"))
	if string(directB) != "" {
		t.Fatalf("sandbox allowed direct shell read of outside file: %q", directB)
	}

	// Tool call succeeded via mid-turn grant.
	mcpB, err := os.ReadFile(filepath.Join(home, "mcp.txt"))
	if err != nil {
		t.Fatalf("could not read mcp.txt: %v", err)
	}
	if !strings.Contains(string(mcpB), "SANDBOX_OUTSIDE_SECRET") {
		t.Fatalf("mcp tool call did not receive file content: %s", string(mcpB))
	}

	// Grant was recorded in folder_grants table on control.
	grants, err := e.store.ListFolderGrants(started.Session)
	if err != nil || len(grants) != 1 || (grants[0].Folder != outsideDir && strings.TrimPrefix(grants[0].Folder, "/private") != strings.TrimPrefix(outsideDir, "/private")) {
		t.Fatalf("folder grant not recorded in store: %v (err: %v)", grants, err)
	}
}

// B8 done-when: A sandboxed turn refused folder access cannot read the outside file.
func TestSandboxedTurnRefusedCannotReadOutsideFile(t *testing.T) {
	canSandbox(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("REFUSED_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	permDir := t.TempDir()
	permFile := filepath.Join(permDir, "permissions.toml")
	if err := os.WriteFile(permFile, []byte("[sandbox]\nmode = \"on\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	homesDir := filepath.Join(t.TempDir(), "homes")

	script := fmt.Sprintf(`cat %q > "$HOME/direct.txt" 2>/dev/null || true
curl -s -X POST "$AGENTBUS_URL/api/v0/mcp" \
  -H "Authorization: Bearer $AGENTBUS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":%q}}}' \
  > "$HOME/mcp.txt"
echo '{"type":"result","session_id":"s1"}'
exit 0`, outsideFile, outsideFile)

	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script}}, nil
	}

	e := stackEnv(t, stack{
		wire:        wire,
		permissions: permFile,
		homes:       homesDir,
		turnWait:    5 * time.Second,
	})

	folder := t.TempDir()
	started := e.firstTurn(t, folder, "claude", "hi", true)

	// Mid-turn hold: tool call is held.
	id := waitForApproval(t, e)
	if id == "" {
		t.Fatal("tool call did not hold for approval")
	}

	// Person refuses the call mid-turn.
	answerApproval(t, e, id, "refuse")

	// Wait for turn job to finish.
	waitEnded(t, e, started.Turn)

	home := filepath.Join(homesDir, started.Session)

	// Direct shell access failed.
	directB, _ := os.ReadFile(filepath.Join(home, "direct.txt"))
	if string(directB) != "" {
		t.Fatalf("sandbox allowed direct shell read of outside file: %q", directB)
	}

	// Tool call received refusal, NOT secret content.
	mcpB, err := os.ReadFile(filepath.Join(home, "mcp.txt"))
	if err != nil {
		t.Fatalf("could not read mcp.txt: %v", err)
	}
	if strings.Contains(string(mcpB), "REFUSED_SECRET") {
		t.Fatalf("refused tool call received file content: %s", string(mcpB))
	}
	if !strings.Contains(string(mcpB), "refused this call") {
		t.Fatalf("mcp output did not indicate refusal: %s", string(mcpB))
	}
}
