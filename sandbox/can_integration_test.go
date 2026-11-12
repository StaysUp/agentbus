//go:build integration && (darwin || linux)

package sandbox

import (
	"runtime"
	"testing"
)

// On this machine the check builds a real wall. A Mac always can; a Linux
// machine can with bubblewrap and user namespaces, and where it cannot, the
// reason is a sentence.
func TestCanHere(t *testing.T) {
	ok, why := Can()
	t.Logf("can sandbox: %v %q", ok, why)
	switch {
	case !ok && why == "":
		t.Fatal("no reason given")
	case ok && why != "":
		t.Fatalf("a reason given when it can: %q", why)
	case !ok && runtime.GOOS == "darwin":
		t.Fatalf("a Mac cannot sandbox: %s", why)
	}
}
