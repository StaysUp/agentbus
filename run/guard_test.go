package run

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/staysup/agentbus/gateway"
)

func TestTurnRules(t *testing.T) {
	e := &Engine{}
	// t_1: forbid git push, allow ls, allow git fetch once.
	e.turnRules.set("s1", "t_1", []string{"ls"}, nil, []string{"git push"}, []string{"git fetch origin"})
	// t_2: every list empty, the ask default for a command on no list.
	e.turnRules.set("s1", "t_2", nil, nil, nil, nil)
	if got := e.Forbid("s1", "t_1"); !slices.Equal(got, []string{"git push"}) {
		t.Errorf("Forbid = %q", got)
	}
	if got := e.Allow("s1", "t_1"); !slices.Equal(got, []string{"ls"}) {
		t.Errorf("Allow = %q", got)
	}
	if got := e.AllowOnce("s1", "t_1"); !slices.Equal(got, []string{"git fetch origin"}) {
		t.Errorf("AllowOnce = %q", got)
	}
	// A live turn holds a command on no list for a person.
	if got := e.Unlisted("s1", "t_1"); got != gateway.Ask {
		t.Errorf("Unlisted t_1 = %v, want Ask", got)
	}
	if got := e.Unlisted("s1", "t_2"); got != gateway.Ask {
		t.Errorf("Unlisted t_2 = %v, want Ask", got)
	}
	if got := e.Forbid("s1", "t_2"); got != nil {
		t.Errorf("a turn without forbid got %q", got)
	}
	// A turn that is not live is not guarded: a command on no list runs.
	if got := e.Unlisted("s1", "gone"); got != gateway.Allow {
		t.Errorf("Unlisted of an unknown turn = %v, want Allow", got)
	}
	e.turnRules.clear("s1", "t_1")
	if got := e.Forbid("s1", "t_1"); got != nil {
		t.Errorf("after clear: %q", got)
	}
	if got := e.Unlisted("s1", "t_1"); got != gateway.Allow {
		t.Errorf("after clear, Unlisted = %v, want Allow", got)
	}
}

// CopyWrite is the writable set of a turn that runs in a copy, for the gateway
// to refuse a file-edit outside it. setCopyWrite updates the entry set() made;
// a turn with no copy, or one not live, gets nil.
func TestCopyWrite(t *testing.T) {
	e := &Engine{}
	e.turnRules.set("s1", "t_1", nil, nil, nil, nil)
	// A Copy turn's write-set is the copy dir and any extra writable paths.
	e.turnRules.setCopyWrite("s1", "t_1", []string{"/copy", "/tmp/cache"})
	if got := e.CopyWrite("s1", "t_1"); !slices.Equal(got, []string{"/copy", "/tmp/cache"}) {
		t.Errorf("CopyWrite = %q", got)
	}
	// A turn with no setCopyWrite (a Direct turn) has no boundary.
	e.turnRules.set("s1", "t_2", nil, nil, nil, nil)
	if got := e.CopyWrite("s1", "t_2"); got != nil {
		t.Errorf("a Direct turn got a write-set %q", got)
	}
	// setCopyWrite on a turn that is not live records nothing.
	e.turnRules.setCopyWrite("s1", "gone", []string{"/copy"})
	if got := e.CopyWrite("s1", "gone"); got != nil {
		t.Errorf("an unknown turn got a write-set %q", got)
	}
	// The stored set is a copy: mutating the caller's slice does not change it.
	in := []string{"/copy"}
	e.turnRules.setCopyWrite("s1", "t_1", in)
	in[0] = "/elsewhere"
	if got := e.CopyWrite("s1", "t_1"); !slices.Equal(got, []string{"/copy"}) {
		t.Errorf("the stored write-set aliased the caller's slice: %q", got)
	}
}

// The copy write-set is built with appendReal, so a copy reached through a
// symlink (/tmp -> /private/tmp on macOS) is in the set under both names.
// Claude Code reports the resolved path to the model, so without the realpath
// the gateway would refuse every edit inside the copy. This mirrors run.go.
func TestCopyWriteResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("eval real: %v", err)
	}
	link := filepath.Join(t.TempDir(), "copylink")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Build the write-set the way run.go does for spec.Copy.Dir.
	got := appendReal(nil, link)
	if got[0] != link {
		t.Errorf("the copy dir is not first (the cwd): %q", got)
	}
	if !slices.Contains(got, realDir) {
		t.Errorf("the realpath %q is missing from the write-set %q", realDir, got)
	}
}
