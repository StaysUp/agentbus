//go:build integration

package run

import (
	"runtime"
	"strings"
	"testing"
)

// On macOS the per-user cache and temp folders come from getconf, under
// /var/folders.
func TestDarwinUserDirs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if d := copyCaches([]string{"HOME=/h"}); len(d) != 1 {
			t.Errorf("copyCaches = %v, want the one XDG cache folder off macOS", d)
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
