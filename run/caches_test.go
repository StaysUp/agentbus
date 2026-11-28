package run

import (
	"runtime"
	"slices"
	"testing"
)

// A Copy turn with no cache folder of its own writes no cache at all: never
// the person's.
func TestCopyCacheNone(t *testing.T) {
	write, env := copyCache("", []string{"/sys"})
	if !slices.Equal(write, []string{"/sys"}) || env != nil {
		t.Errorf("copyCache(\"\") = %q %q, want only the system folders and no variables", write, env)
	}
}

// On macOS only the Keychain's MDS folder of the per-user cache folder is
// writable, with the per-user temp folder; elsewhere no system folder is.
func TestSystemDirs(t *testing.T) {
	was := userDirs
	userDirs = func() []string { return []string{"/var/folders/xx/yy/C/", "/var/folders/xx/yy/T/"} }
	t.Cleanup(func() { userDirs = was })
	want := []string(nil)
	if runtime.GOOS == "darwin" {
		want = []string{"/var/folders/xx/yy/C/mds", "/var/folders/xx/yy/T"}
	}
	if got := systemDirs(); !slices.Equal(got, want) {
		t.Errorf("systemDirs = %q, want %q", got, want)
	}
}
