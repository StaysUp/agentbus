//go:build integration

package run

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// On macOS the per-user cache and temp folders come from getconf, under
// /var/folders.
func TestDarwinUserDirs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if d := systemDirs(); d != nil {
			t.Errorf("systemDirs = %v, want none off macOS", d)
		}
		return
	}
	ds := darwinUserDirs()
	if len(ds) != 2 {
		t.Fatalf("darwinUserDirs = %v, want the cache and the temp folder", ds)
	}
	for _, d := range ds {
		if !strings.HasPrefix(d, "/var/folders/") && !strings.HasPrefix(d, "/private/var/folders/") {
			t.Errorf("darwinUserDirs has %q, want a folder under /var/folders", d)
		}
	}
}

// copyTurn starts a Copy turn of claude whose program is script (sh), with
// HOME at a folder of the person's own, and waits for it to end. It returns
// the person's home, the turn's folder and its own cache folder.
func copyTurn(t *testing.T, script string) (user, folder, cache string) {
	t.Helper()
	user = realDir(t)
	t.Setenv("HOME", user)
	// The person's own cache variables, which a Copy turn must not follow.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(user, "xdg"))
	t.Setenv("GOCACHE", "")
	folder = realDir(t)
	cache = filepath.Join(realDir(t), "state", "cache")
	e, _ := sandboxEngine(t, "", false, script)
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Copy: true, Cache: cache, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	return user, folder, cache
}

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A Copy turn's commands write the copy, the session's own cache and the
// system folders they need, never the person's own cache folders: a write
// there lands outside the copy, where review and Discard miss it, and runs
// later with no sandbox. Every tool's cache variable points into the
// session's cache, which the turn makes.
func TestCopyTurnOwnCache(t *testing.T) {
	was := userDirs
	userDirs = func() []string { return []string{"/var/folders/xx/yy/C/", "/var/folders/xx/yy/T/"} }
	t.Cleanup(func() { userDirs = was })
	user, folder, cache := copyTurn(t, `printf '%s\n' "$@" > args; env > env; exit 0`)
	b, err := os.ReadFile(filepath.Join(folder, "args"))
	if err != nil {
		t.Fatal(err)
	}
	var write []string
	for _, a := range strings.Split(string(b), "\n") {
		var s struct {
			Sandbox struct {
				Filesystem struct {
					AllowWrite []string `json:"allowWrite"`
				} `json:"filesystem"`
			} `json:"sandbox"`
		}
		if json.Unmarshal([]byte(a), &s) == nil && s.Sandbox.Filesystem.AllowWrite != nil {
			write = s.Sandbox.Filesystem.AllowWrite
		}
	}
	want := []string{folder, cache}
	if runtime.GOOS == "darwin" {
		want = append(want, "/var/folders/xx/yy/C/mds", "/var/folders/xx/yy/T")
	}
	if !slices.Equal(write, want) {
		t.Errorf("allowWrite = %q, want %q", write, want)
	}
	for _, mine := range []string{filepath.Join(user, ".cache"), filepath.Join(user, "Library", "Caches"),
		filepath.Join(user, "xdg"), "/var/folders/xx/yy/C"} {
		if slices.Contains(write, mine) {
			t.Errorf("allowWrite has the person's cache folder %s: %q", mine, write)
		}
	}
	env, err := os.ReadFile(filepath.Join(folder, "env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{"XDG_CACHE_HOME=" + cache, "GOCACHE=" + filepath.Join(cache, "go-build"),
		"UV_CACHE_DIR=" + filepath.Join(cache, "uv"), "PIP_CACHE_DIR=" + filepath.Join(cache, "pip"),
		"HOMEBREW_CACHE=" + filepath.Join(cache, "Homebrew"), "npm_config_cache=" + filepath.Join(cache, "npm")} {
		if !slices.Contains(strings.Split(string(env), "\n"), kv) {
			t.Errorf("the turn's environment has no %s", kv)
		}
	}
	if fi, err := os.Stat(cache); err != nil || !fi.IsDir() {
		t.Errorf("the session's cache folder was not made: %v", err)
	}
}

// go build and go install work in a Copy turn through the session's own
// cache while the person's cache folders cannot be written (as the sandbox
// holds them), and what they keep lands in the session's cache.
func TestCopyTurnBuildsInOwnCache(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go on PATH: %v", err)
	}
	script := `set -e
printf 'module m\n\ngo 1.21\n' > go.mod
printf 'package main\n\nfunc main() {}\n' > main.go
GOTOOLCHAIN=local GOFLAGS= GOBIN="$PWD/bin" ` + gobin + ` build -o out . 2> build.err
GOTOOLCHAIN=local GOFLAGS= GOBIN="$PWD/bin" ` + gobin + ` install . 2>> build.err
echo ok > done`
	user := realDir(t)
	// The person's cache folders exist and cannot be written.
	for _, d := range []string{filepath.Join(user, ".cache"), filepath.Join(user, "Library", "Caches"), filepath.Join(user, "xdg")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(d, 0o700) })
	}
	t.Setenv("HOME", user)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(user, "xdg"))
	t.Setenv("GOCACHE", "")
	folder := realDir(t)
	cache := filepath.Join(realDir(t), "state", "cache")
	e, _ := sandboxEngine(t, "", false, script)
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Copy: true, Cache: cache, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	if _, err := os.Stat(filepath.Join(folder, "done")); err != nil {
		b, _ := os.ReadFile(filepath.Join(folder, "build.err"))
		t.Fatalf("the build in the Copy turn failed: %s", b)
	}
	if _, err := os.Stat(filepath.Join(folder, "bin", "m")); err != nil {
		t.Errorf("go install left no program: %v", err)
	}
	if ents, err := os.ReadDir(filepath.Join(cache, "go-build")); err != nil || len(ents) == 0 {
		t.Errorf("the session's cache holds no Go build cache: %v", err)
	}
}

// The gateway refuses a file-edit tool call outside the same set the sandbox
// writes (gateway/copyguard.go): while a Copy turn runs, that set holds the
// session's own cache and none of the person's cache folders.
func TestCopyTurnEditSetOwnCache(t *testing.T) {
	user := realDir(t)
	t.Setenv("HOME", user)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(user, "xdg"))
	folder := realDir(t)
	cache := filepath.Join(realDir(t), "state", "cache")
	e, _ := sandboxEngine(t, "", false, `while [ ! -f go ]; do sleep 0.05; done; exit 0`)
	turn, err := e.Start(Request{Harness: "claude", Folder: folder, Copy: true, Cache: cache, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	set := e.CopyWrite(turn.Session, turn.ID)
	if err := os.WriteFile(filepath.Join(folder, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-turn.Done()
	if !slices.Contains(set, cache) {
		t.Errorf("the edit set %q has no session cache %s", set, cache)
	}
	for _, mine := range []string{filepath.Join(user, ".cache"), filepath.Join(user, "Library", "Caches"), filepath.Join(user, "xdg")} {
		if slices.Contains(set, mine) {
			t.Errorf("the edit set %q has the person's cache folder %s", set, mine)
		}
	}
}
