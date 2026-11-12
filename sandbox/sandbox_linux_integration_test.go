//go:build integration && linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A runner run as root, against the real bwrap: a file in another user's
// closed folder is refused with the plain reason, one open to everyone is
// reached, and so is one in root's own closed folder. The last two show
// closedToRoot refuses nothing bwrap would show. Root only: making a
// folder of another user's needs it.
func TestRootReach(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root only")
	}
	tr := makeTree(t)
	file := func(dir string, mode os.FileMode, uid int) string {
		d := filepath.Join(tr.root, dir)
		if err := os.Mkdir(d, mode); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "prog")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(f, uid, uid); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(d, uid, uid); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, c := range []struct {
		name string
		path string
		ok   bool
	}{
		{"another user's closed folder", file("closed", 0o750, 65534), false},
		{"another user's open folder", file("open", 0o755, 65534), true},
		{"root's closed folder", file("own", 0o700, 0), true},
	} {
		box, err := New(Spec{Copy: tr.copyA, Home: tr.homeA, Read: []string{c.path}})
		if box != nil {
			box.Close()
		}
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case !c.ok && (err == nil || !strings.Contains(err.Error(), "run as root")):
			t.Errorf("%s: %v, want the plain refusal for root", c.name, err)
		}
	}
}
