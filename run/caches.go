package run

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// copyCaches are the cache and temp folders a Copy turn's commands may write
// besides the copy (harness.Copy.Write), from the host environment env: the
// person's own cache folder, where Go's build cache, pip and Homebrew keep
// theirs, and on macOS the per-user cache and temp folders under /var/folders.
// The Keychain writes its Module Directory Service files in the cache one
// (…/C/mds): with it read-only, "security list-keychains" fails with "A
// Module Directory Service error has occurred" and gh reads no token. mktemp
// writes in the temp one (…/T) when TMPDIR names it. Both seen in a Copy
// session, 2026-11-27, Claude Code 2.1.332.
func copyCaches(env []string) []string {
	var dirs []string
	add := func(d string) {
		if d != "" && filepath.IsAbs(d) {
			d = filepath.Clean(d)
			for _, have := range dirs {
				if have == d {
					return
				}
			}
			dirs = append(dirs, d)
		}
	}
	home := lastEnv(env, "HOME")
	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			add(filepath.Join(home, "Library", "Caches"))
		}
		for _, d := range userDirs() {
			add(d)
		}
	default:
		if x := lastEnv(env, "XDG_CACHE_HOME"); x != "" {
			add(x)
		} else if home != "" {
			add(filepath.Join(home, ".cache"))
		}
	}
	return dirs
}

// userDirs are macOS's per-user cache and temp folders; a unit test swaps
// it, since the real one starts getconf.
var userDirs = darwinUserDirs

var (
	darwinDirsOnce sync.Once
	darwinDirs     []string
)

// darwinUserDirs are macOS's per-user cache and temp folders (confstr
// _CS_DARWIN_USER_CACHE_DIR and _CS_DARWIN_USER_TEMP_DIR), read once from
// getconf: Go has no confstr.
func darwinUserDirs() []string {
	darwinDirsOnce.Do(func() {
		for _, name := range []string{"DARWIN_USER_CACHE_DIR", "DARWIN_USER_TEMP_DIR"} {
			if out, err := exec.Command("/usr/bin/getconf", name).Output(); err == nil {
				if d := strings.TrimSpace(string(out)); d != "" {
					darwinDirs = append(darwinDirs, d)
				}
			}
		}
	})
	return darwinDirs
}

// lastEnv is the value of key in env (the last one wins, as for exec), or "".
func lastEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
