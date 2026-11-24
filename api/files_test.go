package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/settings"
	"golang.org/x/sys/unix"
)

func TestFilesDirectOperations(t *testing.T) {
	e, _ := settingsEnv(t)
	dir := t.TempDir()
	f1 := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f1, []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. read_file
	resp, out := send(t, e, "POST", "/api/v0/files/read", "", fmt.Sprintf(`{"path":%q}`, f1))
	if resp.StatusCode != 200 {
		t.Fatalf("readFile: %d %s", resp.StatusCode, out)
	}
	var rf ReadFileResponse
	if err := json.Unmarshal([]byte(out), &rf); err != nil || rf.Content != "world" || rf.Size != 5 {
		t.Fatalf("readFile body: %v, out: %s", err, out)
	}

	// read missing file -> 404
	resp, _ = send(t, e, "POST", "/api/v0/files/read", "", fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "nope.txt")))
	if resp.StatusCode != 404 {
		t.Fatalf("read missing file: %d", resp.StatusCode)
	}

	// read directory -> 400
	resp, _ = send(t, e, "POST", "/api/v0/files/read", "", fmt.Sprintf(`{"path":%q}`, dir))
	if resp.StatusCode != 400 {
		t.Fatalf("read dir: %d", resp.StatusCode)
	}

	// 2. list_folder
	resp, out = send(t, e, "POST", "/api/v0/files/list", "", fmt.Sprintf(`{"path":%q}`, dir))
	if resp.StatusCode != 200 {
		t.Fatalf("listFolder: %d %s", resp.StatusCode, out)
	}
	var lf ListFolderResponse
	if err := json.Unmarshal([]byte(out), &lf); err != nil || len(lf.Entries) != 1 || lf.Entries[0].Name != "hello.txt" || lf.Entries[0].Kind != "file" {
		t.Fatalf("listFolder body: %v, out: %s", err, out)
	}

	// list file -> 400
	resp, _ = send(t, e, "POST", "/api/v0/files/list", "", fmt.Sprintf(`{"path":%q}`, f1))
	if resp.StatusCode != 400 {
		t.Fatalf("list file: %d", resp.StatusCode)
	}

	// 3. write_file
	f2 := filepath.Join(dir, "sub", "test.txt")
	resp, out = send(t, e, "POST", "/api/v0/files/write", "", fmt.Sprintf(`{"path":%q,"content":"new content"}`, f2))
	if resp.StatusCode != 200 {
		t.Fatalf("writeFile: %d %s", resp.StatusCode, out)
	}
	var wf WriteFileResponse
	if err := json.Unmarshal([]byte(out), &wf); err != nil || wf.Written != 11 {
		t.Fatalf("writeFile body: %v, out: %s", err, out)
	}
	b, err := os.ReadFile(f2)
	if err != nil || string(b) != "new content" {
		t.Fatalf("written file: %s %v", string(b), err)
	}
}

// B8 tool gate tests: read_file through MCP with once, always for this folder, and refuse.
func TestFolderToolsHidden(t *testing.T) {
	// B8 folder tools are hidden in 0.6.5: not registered, so tools/list does
	// not name them and tools/call cannot reach them. The HTTP routes stay for
	// 0.9 (see TestDirectTokenFileAccessRequiresGrant), but no agent can call
	// the tools (his call, 2026-11-23).
	e := toolGateEnv(t, 5*time.Second, 0, nil)
	for _, name := range []string{"read_file", "list_folder", "write_file"} {
		if _, ok := e.api.servedByName(name); ok {
			t.Errorf("%s is still served as a tool", name)
		}
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"path":"/tmp/x"}}}`, name)
		_, v := rpc(t, e, mcpWriteTok, call)
		if dig(v, "error") == nil {
			t.Errorf("tools/call for the hidden tool %s did not error: %v", name, v)
		}
		if pending := pendingIDs(t, e); len(pending) != 0 {
			t.Errorf("a hidden tool call was held: %v", pending)
		}
	}
	// tools/list names none of them.
	_, v := rpc(t, e, mcpWriteTok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listed := fmt.Sprintf("%v", v)
	for _, name := range []string{"read_file", "list_folder", "write_file"} {
		if strings.Contains(listed, name) {
			t.Errorf("tools/list names the hidden tool %s", name)
		}
	}
}

func TestDirectTokenFileAccessRequiresGrant(t *testing.T) {
	e := toolGateEnv(t, 5*time.Second, 0, nil)
	dir := t.TempDir()
	f := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(f, []byte("shh"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Direct call without grant -> 403
	req := e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out := do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "folder access not granted") {
		t.Fatalf("direct call without grant: %d %s", resp.StatusCode, out)
	}

	// Add grant to store for s1
	if err := e.store.AddFolderGrant("s1", dir); err != nil {
		t.Fatal(err)
	}

	// Direct call with grant -> 200
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 200 || !strings.Contains(out, "shh") {
		t.Fatalf("direct call with grant: %d %s", resp.StatusCode, out)
	}

	// Finding 1: Symlink escape inside granted folder is blocked
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside_secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "link_to_outside")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, symlinkPath)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "folder access not granted") {
		t.Fatalf("symlink escape was not blocked: %d %s", resp.StatusCode, out)
	}

	// Finding 2: Direct file requests respect forbid rules, including path-style rules
	eForbid := toolGateEnv(t, 5*time.Second, 0, func(p *settings.Policy) {
		p.Forbid = []string{"/usr/bin/read_file", "write_file"}
	})
	if err := eForbid.store.AddFolderGrant("s1", dir); err != nil {
		t.Fatal(err)
	}
	req = eForbid.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "tool forbidden by policy") {
		t.Fatalf("direct call with forbid rule was not rejected: %d %s", resp.StatusCode, out)
	}

	// Finding 1: Cyclic symlink returns 400 instead of crashing or looping
	cycleA := filepath.Join(dir, "cycle_a")
	cycleB := filepath.Join(dir, "cycle_b")
	_ = os.Symlink(cycleB, cycleA)
	_ = os.Symlink(cycleA, cycleB)
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, cycleA)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(out, "symbolic links") {
		t.Fatalf("cyclic symlink was not rejected with 400: %d %s", resp.StatusCode, out)
	}

	// Finding 3: Missing content in writeFile is rejected (400)
	req = e.req(t, "POST", "/api/v0/files/write", strings.NewReader(fmt.Sprintf(`{"path":%q}`, f)))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(out, "path and content") {
		t.Fatalf("missing content in write_file was not rejected: %d %s", resp.StatusCode, out)
	}
	// Verify file content was not truncated
	shhB, _ := os.ReadFile(f)
	if string(shhB) != "shh" {
		t.Fatalf("file content was corrupted: %q", string(shhB))
	}

	// Finding 1: Granting /private does not grant /Users paths
	ePrivate := toolGateEnv(t, 5*time.Second, 0, nil)
	if err := ePrivate.store.AddFolderGrant("s1", "/private"); err != nil {
		t.Fatal(err)
	}
	req = ePrivate.req(t, "POST", "/api/v0/files/read", strings.NewReader(`{"path":"/Users/example/data.txt"}`))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 403 || !strings.Contains(out, "folder access not granted") {
		t.Fatalf("granting /private granted /Users: %d %s", resp.StatusCode, out)
	}

	// Finding 3: Non-regular file (e.g. /dev/null) is rejected
	_ = e.store.AddFolderGrant("s1", "/dev")
	req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(`{"path":"/dev/null"}`))
	req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
	resp, out = do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(out, "not a regular file") {
		t.Fatalf("reading /dev/null was not rejected: %d %s", resp.StatusCode, out)
	}

	// The folder tools are reached only through the HTTP route now (they are
	// hidden as tools), so the route's own checks are what protect it; the
	// cyclic-symlink, grant and forbid checks above cover that route.
	// A directory swapped for a symlink cannot escape the granted folder.
	{
		subDir := filepath.Join(dir, "sub_swap")
		if err := os.Mkdir(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(subDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, subDir); err != nil {
			t.Fatal(err)
		}
		req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, filepath.Join(subDir, "outside_secret.txt"))))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode == 200 {
			t.Fatalf("directory swap read succeeded! %s", out)
		}
	}

	// A FIFO is a non-regular file: read and list refuse it, they do not block.
	{
		fifoPath := filepath.Join(dir, "test_fifo")
		if err := unix.Mkfifo(fifoPath, 0666); err == nil {
			req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, fifoPath)))
			req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
			resp, out = do(t, req)
			if resp.StatusCode != 400 || !strings.Contains(out, "not a regular file") {
				t.Fatalf("FIFO read did not fail with 400: %d %s", resp.StatusCode, out)
			}
			req = e.req(t, "POST", "/api/v0/files/list", strings.NewReader(fmt.Sprintf(`{"path":%q}`, fifoPath)))
			req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
			resp, out = do(t, req)
			if resp.StatusCode != 400 || !strings.Contains(out, "path is a file") {
				t.Fatalf("FIFO list did not fail with 400: %d %s", resp.StatusCode, out)
			}
		}
	}

	// A write creates missing parent directories under the granted folder.
	{
		nestedFile := filepath.Join(dir, "new", "nested", "file.txt")
		req = e.req(t, "POST", "/api/v0/files/write", strings.NewReader(fmt.Sprintf(`{"path":%q,"content":"nested hello"}`, nestedFile)))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode != 200 {
			t.Fatalf("write to nested directory failed: %d %s", resp.StatusCode, out)
		}
		data, err := os.ReadFile(nestedFile)
		if err != nil || string(data) != "nested hello" {
			t.Fatalf("nested file not written: %s %v", string(data), err)
		}
	}

	// Valid file names starting with .. (e.g. ..notes) are not refused.
	{
		dotdotFile := filepath.Join(dir, "..notes")
		if err := os.WriteFile(dotdotFile, []byte("dotdot content"), 0o600); err != nil {
			t.Fatal(err)
		}
		req = e.req(t, "POST", "/api/v0/files/read", strings.NewReader(fmt.Sprintf(`{"path":%q}`, dotdotFile)))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode != 200 || !strings.Contains(out, "dotdot content") {
			t.Fatalf("reading ..notes was refused: %d %s", resp.StatusCode, out)
		}
	}

	// A refused (ungranted) write does not truncate the destination file.
	{
		origFile := filepath.Join(dir, "no_trunc.txt")
		if err := os.WriteFile(origFile, []byte("important content"), 0o600); err != nil {
			t.Fatal(err)
		}
		eNoGrant := toolGateEnv(t, 5*time.Second, 0, nil)
		req = eNoGrant.req(t, "POST", "/api/v0/files/write", strings.NewReader(fmt.Sprintf(`{"path":%q,"content":"bad content"}`, origFile)))
		req.Header.Set("Authorization", "Bearer "+mcpWriteTok)
		resp, out = do(t, req)
		if resp.StatusCode != 403 {
			t.Fatalf("ungranted write status: %d %s", resp.StatusCode, out)
		}
		content, _ := os.ReadFile(origFile)
		if string(content) != "important content" {
			t.Fatalf("refused write truncated file! content: %q", string(content))
		}
	}
}
