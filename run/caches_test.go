package run

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// A Copy turn may write the person's cache folders, from the host HOME (not a
// session home), and on macOS the per-user cache and temp folders under
// /var/folders: the Keychain keeps its MDS files in the first (gh read no
// token without it), and mktemp failed in the second.
func TestCopyCaches(t *testing.T) {
	was := userDirs
	userDirs = func() []string { return []string{"/var/folders/xx/yy/C/", "/var/folders/xx/yy/T/"} }
	t.Cleanup(func() { userDirs = was })
	got := copyCaches([]string{"HOME=/old", "HOME=/h"})
	switch runtime.GOOS {
	case "darwin":
		if !slices.Contains(got, "/h/Library/Caches") {
			t.Errorf("copyCaches = %v, want /h/Library/Caches (the last HOME)", got)
		}
		for _, want := range []string{"/var/folders/xx/yy/C", "/var/folders/xx/yy/T"} {
			if !slices.Contains(got, want) {
				t.Errorf("copyCaches = %v, want the per-user cache and temp folder %s", got, want)
			}
		}
	default:
		if !slices.Equal(got, []string{"/h/.cache"}) {
			t.Errorf("copyCaches = %v, want [/h/.cache]", got)
		}
		if got := copyCaches([]string{"HOME=/h", "XDG_CACHE_HOME=/x/c"}); !slices.Equal(got, []string{"/x/c"}) {
			t.Errorf("copyCaches with XDG_CACHE_HOME = %v, want [/x/c]", got)
		}
	}
	for _, d := range got {
		if !filepath.IsAbs(d) {
			t.Errorf("copyCaches has %q, not an absolute path", d)
		}
	}
	// No HOME and a relative XDG_CACHE_HOME add nothing a sandbox would read
	// against the working folder.
	if got := copyCaches([]string{"XDG_CACHE_HOME=rel"}); slices.ContainsFunc(got, func(d string) bool { return !filepath.IsAbs(d) }) {
		t.Errorf("copyCaches = %v, want no relative path", got)
	}
}
