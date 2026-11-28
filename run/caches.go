package run

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The cache rule of a Copy turn, in one place (copyCache), so the one wall of
// 0.6.9 can take it over whole.
//
// A Copy turn's commands write the copy and the session's own cache folder,
// never the person's own cache folders. A write to ~/.cache, ~/Library/Caches
// or XDG_CACHE_HOME lands outside the copy, where review and Discard never see
// it, and those folders hold programs that run later with no sandbox (uv's
// tool binaries, Go's build cache). So until 0.6.8 a Copy turn could plant
// code that ran outside its sandbox (reported privately 2026-11-28; the grant
// came with 0.6.7). The session's own cache is in its workspace's state
// (isolation.CacheDir) and goes when the workspace closes.

// cacheEnv are the variables that point a build or a package tool at its
// cache, each set to a folder of the session's own cache. The XDG one covers
// every tool that follows it (uv, pip, pre-commit, and Go's GOCACHE off
// macOS); the rest name the tools whose macOS default is ~/Library/Caches,
// or that read no XDG variable.
var cacheEnv = [][2]string{
	{"XDG_CACHE_HOME", ""},
	{"GOCACHE", "go-build"},
	{"UV_CACHE_DIR", "uv"},
	{"PIP_CACHE_DIR", "pip"},
	{"HOMEBREW_CACHE", "Homebrew"},
	{"npm_config_cache", "npm"},
}

// copyCache is what a Copy turn may write besides its copy, and the
// variables that send its tools' caches there: own, the session's own cache
// folder ("" when the workspace has none, and then no cache is writable), and
// the system folders a command cannot work without (system).
func copyCache(own string, system []string) (write, env []string) {
	if own != "" {
		write = append(write, own)
		for _, v := range cacheEnv {
			env = append(env, v[0]+"="+filepath.Join(own, v[1]))
		}
	}
	return append(write, system...), env
}

// systemDirs are the folders a Copy turn's commands must write and that no
// variable can move: on macOS the
// Keychain's Module Directory Service files under the per-user cache folder
// (…/C/mds; with them read-only, "security list-keychains" fails with "A
// Module Directory Service error has occurred" and gh reads no token) and the
// per-user temp folder (…/T), which mktemp writes when TMPDIR names it. Both
// seen in a Copy session, 2026-11-27, Claude Code 2.1.332. Elsewhere none.
func systemDirs() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	var dirs []string
	for i, d := range userDirs() {
		if d = filepath.Clean(d); !filepath.IsAbs(d) {
			continue
		}
		if i == 0 {
			d = filepath.Join(d, "mds")
		}
		dirs = append(dirs, d)
	}
	return dirs
}

// userDirs are macOS's per-user cache and temp folders, in that order; a
// unit test swaps it, since the real one starts getconf.
var userDirs = darwinUserDirs

var (
	darwinDirsOnce sync.Once
	darwinDirs     []string
)

// darwinUserDirs are macOS's per-user cache and temp folders (confstr
// _CS_DARWIN_USER_CACHE_DIR and _CS_DARWIN_USER_TEMP_DIR), read once from
// getconf: Go has no confstr. Both or none, so the order holds.
func darwinUserDirs() []string {
	darwinDirsOnce.Do(func() {
		var ds []string
		for _, name := range []string{"DARWIN_USER_CACHE_DIR", "DARWIN_USER_TEMP_DIR"} {
			out, err := exec.Command("/usr/bin/getconf", name).Output()
			d := strings.TrimSpace(string(out))
			if err != nil || d == "" {
				return
			}
			ds = append(ds, d)
		}
		darwinDirs = ds
	})
	return darwinDirs
}
