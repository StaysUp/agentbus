//go:build integration

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
)

// A session starts in a folder the person types, on a server with a
// connector started with no -folder: the connector checks the path and
// registers it, GET /api/v0/folders lists it from then on, and the session
// runs as one named by reference does. The server keeps no path.
func TestTypedFolderOnServer(t *testing.T) {
	bin, bins := built(t), standInDir(t)
	serverHome, addr := t.TempDir(), freeAddr(t)
	sp := startProc(t, bin, serverHome, bins, "server", "-listen", addr, "-replay=false")
	c := waitService(t, sp, addr, serverHome)
	enroll := filepath.Join(serverHome, "enroll_token")
	folder := personFolder(t)
	post := func(path string) (int, string) {
		var e struct{ Error string }
		code, _ := c.call("POST", "/api/v0/turns", map[string]any{"agent": "claude", "folder": path, "prompt": "hi", "isolate": true}, &e)
		return code, e.Error
	}

	// No connector yet: nothing can check the folder.
	if code, msg := post(folder); code != http.StatusServiceUnavailable || !strings.HasPrefix(msg, "no connector is connected") {
		t.Fatalf("with no connector: %d %q", code, msg)
	}

	connHome := t.TempDir()
	startProc(t, bin, connHome, bins, "connector", "-server", c.base, "-enroll-token-file", enroll, "-name", "conn")
	startProc(t, bin, t.TempDir(), bins, "runner", "-server", c.base, "-enroll-token-file", enroll, "-name", "run", "-replay=false")

	// Each refusal says what is wrong in one sentence. The first waits for
	// the connector to be connected.
	missing := filepath.Join(folder, "nope")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		code, msg := post(missing)
		if code == http.StatusBadRequest && msg == "the folder does not exist on the connector's machine" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a missing folder: %d %q", code, msg)
		}
	}
	if code, msg := post(filepath.Join(folder, "a.txt")); code != http.StatusBadRequest || msg != "the folder is a file, not a directory" {
		t.Fatalf("a file: %d %q", code, msg)
	}
	if os.Geteuid() != 0 {
		locked := t.TempDir()
		os.Chmod(locked, 0o000)
		t.Cleanup(func() { os.Chmod(locked, 0o700) })
		if code, msg := post(locked); code != http.StatusBadRequest || msg != "the folder cannot be read by the user the connector runs as" {
			t.Fatalf("an unreadable folder: %d %q", code, msg)
		}
	}
	if code, msg := post(folder + "x"); code != http.StatusBadRequest || !strings.Contains(msg, "does not exist") {
		t.Fatalf("another missing folder: %d %q", code, msg)
	}
	// A connector named that is not connected.
	if code, _ := c.call("POST", "/api/v0/turns", map[string]any{"agent": "claude", "folder": folder, "prompt": "hi",
		"connector": "con_0123456789abcdef"}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("an unknown connector: %d", code)
	}

	// The whole flow, by the typed path: copies, diffs and applies.
	filesFlow(t, c, folder, map[string]any{"folder": folder, "isolate": true})

	// Listed from now on, with the connector that owns it; the missing and
	// refused paths were never registered.
	var fs struct {
		Folders []struct{ Ref, Name, Connector string }
	}
	c.call("GET", "/api/v0/folders", nil, &fs)
	if len(fs.Folders) != 1 || fs.Folders[0].Name != filepath.Base(folder) || !queue.ConnectorShape.MatchString(fs.Folders[0].Connector) {
		t.Fatalf("folders after a typed path: %+v", fs.Folders)
	}
	if s := fileText(t, filepath.Join(connHome, "folders.json")); !strings.Contains(s, folder) || strings.Contains(s, missing) {
		t.Fatalf("the connector's folders.json: %s", s)
	}

	// A retry with the same idempotency key gets the same turn, without the
	// folder being checked again: here it has moved away since.
	retry := map[string]any{"agent": "claude", "folder": folder, "prompt": "again", "isolate": true, "idempotency_key": "typed-1"}
	first := c.turn(retry)
	if err := os.Rename(folder, folder+".moved"); err != nil {
		t.Fatal(err)
	}
	var second struct {
		Turn     string
		Existing bool
	}
	code, out := c.call("POST", "/api/v0/turns", retry, &second)
	if err := os.Rename(folder+".moved", folder); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusAccepted || second.Turn != first.Turn || !second.Existing {
		t.Fatalf("its retry: %d %s, want turn %s again", code, out, first.Turn)
	}
	// The same key with another folder is another request.
	if code, out := c.call("POST", "/api/v0/turns", merge(retry, map[string]any{"folder": missing}), nil); code != http.StatusConflict {
		t.Fatalf("the key with another folder: %d %s", code, out)
	}

	// With a second connector, a typed folder must name one. The missing
	// path asks without starting a turn while the second one enrolls.
	startProc(t, bin, t.TempDir(), bins, "connector", "-server", c.base, "-enroll-token-file", enroll, "-name", "conn2")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		code, msg := post(missing)
		if code == http.StatusBadRequest && msg == "2 connectors are connected; name the folder's connector, from GET /api/v0/folders" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with two connectors: %d %q", code, msg)
		}
	}
	named := c.turn(map[string]any{"agent": "claude", "folder": folder, "prompt": "named", "isolate": true,
		"connector": fs.Folders[0].Connector})
	if named.Turn == "" {
		t.Fatal("a turn naming its connector did not start")
	}

	// The server's queue keeps no path: every job names the folder by its
	// reference, and no file of the queue holds it.
	_ = sp.cmd.Process.Signal(syscall.SIGTERM)
	<-sp.done
	q, err := queue.Open(filepath.Join(serverHome, "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	jobs, err := q.List(queue.DefaultTeam, queue.Filter{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, j := range jobs {
		kinds[j.Kind]++
		if j.Spec.Folder != "" {
			t.Errorf("job %s (%s) keeps the path %q", j.ID, j.Kind, j.Spec.Folder)
		}
	}
	if kinds[queue.KindFolder] < 5 {
		t.Errorf("folder jobs: %v", kinds)
	}
	files, _ := filepath.Glob(filepath.Join(serverHome, "jobs.db*"))
	records, _ := filepath.Glob(filepath.Join(serverHome, "events.db*"))
	if len(files) == 0 || len(records) == 0 {
		t.Fatalf("no queue or record file: %v %v", files, records)
	}
	for _, f := range append(files, records...) {
		if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), folder) {
			t.Errorf("%s holds the folder's path", filepath.Base(f))
		}
	}
}
