package connector

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/workspace"
)

// stopFixture is a folder of a.txt, b.txt and c.txt, and the changes that
// write each anew, their blobs in a store of their own.
func stopFixture(t *testing.T) (folder string, c *Connector, cs *workspace.Changeset) {
	t.Helper()
	folder = t.TempDir()
	write(t, folder, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})
	st, err := blobs.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"A\n", "B\n", "C\n"} {
		if _, err := st.PutBytes([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	cs = &workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "a.txt", Base: fileOf("a\n", 0o644), Now: fileOf("A\n", 0o644)},
		{Path: "b.txt", Base: fileOf("b\n", 0o644), Now: fileOf("B\n", 0o644)},
		{Path: "c.txt", Base: fileOf("c\n", 0o644), Now: fileOf("C\n", 0o644)},
	}}
	return folder, &Connector{Store: st}, cs
}

// A stop while the look again reads the files (it can read large ones, after
// the last stop check before it) writes nothing: the apply ends stopped, the
// folder as it was (review of #107: the writes went ahead after a stop
// during the look again).
func TestApplyStoppedDuringTheLookAgainWritesNothing(t *testing.T) {
	folder, c, cs := stopFixture(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	lookAgain = func(rel string) {
		if rel == "c.txt" { // the last file looked at again
			stop()
		}
	}
	t.Cleanup(func() { lookAgain = nil })
	done, err := c.ApplyChangesIn(ctx, folder, nil, cs)
	if !errors.Is(err, context.Canceled) || len(done.Files) != 0 {
		t.Fatalf("apply: %+v, %v; want stopped, nothing written", done, err)
	}
	for p, want := range map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"} {
		if got := text(t, filepath.Join(folder, p)); got != want {
			t.Errorf("%s holds %q after a stopped apply", p, got)
		}
	}
}

// A stop once some files are in place ends the apply there: ErrPartial, with
// the files written, as any other failure then, so a caller knows what to put
// back; the rest are left as they were.
func TestApplyStoppedPartWayIsPartial(t *testing.T) {
	folder, c, cs := stopFixture(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	placed = func(rel string) {
		if rel == "a.txt" {
			stop()
		}
	}
	t.Cleanup(func() { placed = nil })
	done, err := c.ApplyChangesIn(ctx, folder, nil, cs)
	if !errors.Is(err, ErrPartial) || len(done.Files) != 1 || done.Files[0] != "a.txt" {
		t.Fatalf("apply: %+v, %v; want ErrPartial with a.txt", done, err)
	}
	for p, want := range map[string]string{"a.txt": "A\n", "b.txt": "b\n", "c.txt": "c\n"} {
		if got := text(t, filepath.Join(folder, p)); got != want {
			t.Errorf("%s holds %q", p, got)
		}
	}
}

// An edit to a file already looked at again, saved while the look again reads
// the files after it, is not written over: each file is looked at once more
// just before its own write (review of the take-over of #107, Codex's
// reproduction: a.txt edited while c.txt was looked at again; the apply
// answered success and replaced the edit). Found before anything is written,
// it refuses the whole apply, nothing written.
func TestApplyKeepsAnEditSavedDuringTheLookAgain(t *testing.T) {
	folder, c, cs := stopFixture(t)
	lookAgain = func(rel string) {
		if rel == "c.txt" {
			write(t, folder, map[string]string{"a.txt": "mine\n"})
		}
	}
	t.Cleanup(func() { lookAgain = nil })
	done, err := c.ApplyChangesIn(context.Background(), folder, nil, cs)
	var cf *Conflict
	if !errors.As(err, &cf) || len(cf.Files) != 1 || cf.Files[0] != (ConflictFile{"a.txt", WhyChanged}) || len(done.Files) != 0 {
		t.Fatalf("apply: %+v, %v; want a conflict at a.txt, nothing written", done, err)
	}
	for p, want := range map[string]string{"a.txt": "mine\n", "b.txt": "b\n", "c.txt": "c\n"} {
		if got := text(t, filepath.Join(folder, p)); got != want {
			t.Errorf("%s holds %q", p, got)
		}
	}
}

// An edit found just before a file's own write, after others were written,
// ends the apply part way: ErrPartial, the files written before it listed,
// the edited file named in Refused and kept, the rest left as they were.
func TestApplyEditFoundPartWayIsPartial(t *testing.T) {
	folder, c, cs := stopFixture(t)
	beforeWrite = func(rel string) {
		if rel == "b.txt" {
			write(t, folder, map[string]string{"b.txt": "mine\n"})
		}
	}
	t.Cleanup(func() { beforeWrite = nil })
	done, err := c.ApplyChangesIn(context.Background(), folder, nil, cs)
	if !errors.Is(err, ErrPartial) || len(done.Files) != 1 || done.Files[0] != "a.txt" ||
		done.Refused == nil || *done.Refused != (ConflictFile{"b.txt", WhyChanged}) {
		t.Fatalf("apply: %+v %+v, %v; want ErrPartial, a.txt written, b.txt refused", done, done.Refused, err)
	}
	for p, want := range map[string]string{"a.txt": "A\n", "b.txt": "mine\n", "c.txt": "c\n"} {
		if got := text(t, filepath.Join(folder, p)); got != want {
			t.Errorf("%s holds %q", p, got)
		}
	}
}

// A folder above a file, made a link after the look again, is refused just
// before the file's write, as the look again would refuse it: the write
// does not go through the link (review of the take-over of #107, Codex's
// reproduction: d became a link to other after the check, and the apply
// wrote other/x.txt).
func TestApplyRefusesAFolderMadeALinkBeforeTheWrite(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"d/x.txt": "x\n", "other/x.txt": "x\n"})
	st, err := blobs.Open(filepath.Join(t.TempDir(), "store"))
	must(t, err)
	_, err = st.PutBytes([]byte("X\n"))
	must(t, err)
	cs := &workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "d/x.txt", Base: fileOf("x\n", 0o644), Now: fileOf("X\n", 0o644)},
	}}
	beforeWrite = func(rel string) {
		if rel == "d/x.txt" {
			must(t, os.RemoveAll(filepath.Join(folder, "d")))
			must(t, os.Symlink("other", filepath.Join(folder, "d")))
		}
	}
	t.Cleanup(func() { beforeWrite = nil })
	done, err := (&Connector{Store: st}).ApplyChangesIn(context.Background(), folder, nil, cs)
	var cf *Conflict
	if !errors.As(err, &cf) || len(cf.Files) != 1 || cf.Files[0] != (ConflictFile{"d/x.txt", WhyNoFolder}) || len(done.Files) != 0 {
		t.Fatalf("apply: %+v, %v; want a conflict at d/x.txt, nothing written", done, err)
	}
	if got := text(t, filepath.Join(folder, "other", "x.txt")); got != "x\n" {
		t.Errorf("other/x.txt holds %q", got)
	}
}

// A file already as the apply leaves it, just before its own write or
// removal, is neither written again nor listed as written (review of the
// take-over of #107: such a file was written and listed).
func TestApplyLeavesAFileAlreadyAsItLeavesIt(t *testing.T) {
	folder, c, cs := stopFixture(t)
	cs.Files = append(cs.Files, workspace.FileChange{Path: "d.txt", Base: fileOf("d\n", 0o644)})
	write(t, folder, map[string]string{"d.txt": "d\n"})
	var before fs.FileInfo
	beforeWrite = func(rel string) {
		switch rel {
		case "b.txt":
			write(t, folder, map[string]string{"b.txt": "B\n"})
			var err error
			before, err = os.Lstat(filepath.Join(folder, "b.txt"))
			must(t, err)
		case "d.txt":
			must(t, os.Remove(filepath.Join(folder, "d.txt")))
		}
	}
	t.Cleanup(func() { beforeWrite = nil })
	done, err := c.ApplyChangesIn(context.Background(), folder, nil, cs)
	if err != nil || !reflect.DeepEqual(done.Files, []string{"a.txt", "c.txt"}) {
		t.Fatalf("apply: %+v, %v; want a.txt and c.txt written, nothing else", done, err)
	}
	after, err := os.Lstat(filepath.Join(folder, "b.txt"))
	must(t, err)
	if !os.SameFile(before, after) {
		t.Error("b.txt was written again")
	}
	for p, want := range map[string]string{"a.txt": "A\n", "b.txt": "B\n", "c.txt": "C\n"} {
		if got := text(t, filepath.Join(folder, p)); got != want {
			t.Errorf("%s holds %q", p, got)
		}
	}
}

// A file the changes remove makes way for a folder of its name only until
// it is removed: a link made at that name after the removal, before the
// write under it, is refused, never written through (review of the
// take-over of #107, Codex's reproduction: file d replaced by folder d/x,
// a link d -> other made before d/x was written; the apply replaced
// other/x).
func TestApplyRefusesALinkMadeWhereAFileWasRemoved(t *testing.T) {
	folder := t.TempDir()
	write(t, folder, map[string]string{"d": "d\n", "other/x": "x\n"})
	st, err := blobs.Open(filepath.Join(t.TempDir(), "store"))
	must(t, err)
	_, err = st.PutBytes([]byte("X\n"))
	must(t, err)
	cs := &workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "d", Base: fileOf("d\n", 0o644)},
		{Path: "d/x", Now: fileOf("X\n", 0o644)},
	}}
	beforeWrite = func(rel string) {
		if rel == "d/x" {
			must(t, os.Symlink("other", filepath.Join(folder, "d")))
		}
	}
	t.Cleanup(func() { beforeWrite = nil })
	done, err := (&Connector{Store: st}).ApplyChangesIn(context.Background(), folder, nil, cs)
	if !errors.Is(err, ErrPartial) || done.Refused == nil || *done.Refused != (ConflictFile{"d/x", WhyNoFolder}) {
		t.Fatalf("apply: %+v %+v, %v; want ErrPartial, d/x refused", done, done.Refused, err)
	}
	if got := text(t, filepath.Join(folder, "other", "x")); got != "x\n" {
		t.Errorf("other/x holds %q", got)
	}
}
