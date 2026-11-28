package workspace

import (
	"encoding/json"
	"io/fs"
	"strings"
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

// A changeset goes to the folder side as JSON. A name that is not valid UTF-8
// (Latin-1 "café.txt") comes back as the same bytes, a name that is valid is
// written with no second field, and a changeset written before path_raw reads
// as it did.
func TestChangesetKeepsANameThatIsNotUTF8(t *testing.T) {
	const latin1, plain = "d\xff/caf\xe9.txt", "src/caf\u00e9 \"q\" \\.txt"
	in := Changeset{Space: "s", Checkpoint: "t1", Files: []FileChange{
		{Path: latin1, Now: &File{Hash: "h1", Size: 4, Mode: 0o644}},
		{Path: plain, Base: &File{Hash: "h2", Size: 2, Mode: 0o644}},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "path_raw"); n != 1 {
		t.Fatalf("path_raw is written %d times, want once, for the name that is not UTF-8: %s", n, b)
	}
	var out Changeset
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 2 || out.Files[0].Path != latin1 || out.Files[1].Path != plain {
		t.Fatalf("the paths read back are %+v", out.Files)
	}
	if out.Files[0].Now == nil || out.Files[0].Now.Hash != "h1" || out.Files[1].Base == nil || out.Files[1].Base.Hash != "h2" {
		t.Fatalf("the files read back are %+v", out.Files)
	}

	var old Changeset
	before := `{"space":"s","checkpoint":"t1","files":[{"path":"a.txt","now":{"size":4,"mode":420,"mtime":0,"sha256":"h1"}}]}`
	if err := json.Unmarshal([]byte(before), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Files) != 1 || old.Files[0].Path != "a.txt" || old.Files[0].Now == nil || old.Files[0].Now.Hash != "h1" {
		t.Fatalf("a changeset written before path_raw reads as %+v", old.Files)
	}
}
