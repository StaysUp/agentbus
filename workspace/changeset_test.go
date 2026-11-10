package workspace

import (
	"io/fs"
	"testing"
)

// A link is recorded with one mode whatever its machine gives links, so
// the same tree has the same id on Linux and macOS.
func TestLinkModeIsOneOnEveryMachine(t *testing.T) {
	h := TreeHash(LinkContent("a.txt"))
	linux := EncodeTree([]TreeEntry{{Name: "l", Hash: h, Mode: fs.ModeSymlink | 0o777, Size: 5}})
	mac := EncodeTree([]TreeEntry{{Name: "l", Hash: h, Mode: fs.ModeSymlink | 0o755, Size: 5}})
	if string(linux) != string(mac) {
		t.Fatalf("%q and %q", linux, mac)
	}
	if file := EncodeTree([]TreeEntry{{Name: "l", Hash: h, Mode: 0o755, Size: 5}}); string(file) == string(mac) {
		t.Fatal("a file and a link are one entry")
	}
}

// Between: a file only the earlier changeset changed goes back to the base;
// one changed again goes to its latest state; one put back is no step.
func TestBetween(t *testing.T) {
	f := func(h string) *File { return &File{Hash: h, Mode: 0o644} }
	from := &Changeset{Space: "s", Files: []FileChange{{Path: "a", Base: f("a0"), Now: f("a1")}, {Path: "b", Base: f("b0"), Now: f("b1")}}}
	to := &Changeset{Space: "s", Files: []FileChange{{Path: "b", Base: f("b0"), Now: f("b2")}, {Path: "c", Now: f("c1")}}}
	steps, err := Between(from, to)
	if err != nil || len(steps) != 3 || steps[0].To.Hash != "a0" || steps[1].From.Hash != "b1" || steps[1].To.Hash != "b2" || steps[2].From != nil {
		t.Fatalf("%+v %v", steps, err)
	}
	if _, err := Between(&Changeset{Space: "x"}, to); err != ErrNotComparable {
		t.Fatal("changesets of two copies compared")
	}
}
