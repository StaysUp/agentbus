package isolation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A copy being made stops when its context ends (#95): the walk looks at it
// before each entry, so a stopped copy writes nothing more.
func TestCopyTreeStopsWithItsContext(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a", "d/b.txt": "b"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(t.TempDir(), "c")
	if _, err := copyTree(ctx, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyTree after a stop: %v", err)
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stopped copy wrote %s: %v", dst, err)
	}
	// One file is copied in chunks, and a stop is seen between two.
	if _, err := copyFile(ctx, filepath.Join(src, "a.txt"), filepath.Join(t.TempDir(), "a"), 0o644, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyFile after a stop: %v", err)
	}
}

// An Open its context stopped answers an error and leaves no copy and no
// record: nothing a turn could load (#95).
func TestOpenStoppedLeavesNoCopy(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a", "d/b.txt": "b"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wd := filepath.Join(t.TempDir(), "w")
	b, _ := Get(Copy)
	if s, err := b.Open(Request{ID: "ws_1", Workdir: wd, Folder: src, Ctx: ctx}); err == nil {
		b.Close(s)
		t.Fatal("a stopped Open made a copy")
	}
	if _, err := LoadID(wd, "ws_1"); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("load after a stopped Open: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(wd, "ws_1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stopped Open left its copy: %v", err)
	}
	if left, _ := os.ReadDir(removingDir(wd)); len(left) != 0 {
		t.Fatalf("a stopped Open left %d entries aside", len(left))
	}
}

// A close that took the record away but could not move the copy leaves a
// folder no Load finds (#96): the session's next turn gets a fresh copy of
// the folder, never what the close left.
func TestCloseLeftoverIsNeverLoaded(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"notes.md": "n", "src/app.js": "js"})
	b, s := open(t, Copy, src)
	// As a close that stopped after its first step: the record is gone, the
	// copy half removed.
	if err := dropRecord(s); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(s.Root, "notes.md"))
	fixture(t, s.Root, map[string]string{"stale.txt": "left"})
	if _, err := Load(s.Workdir, s.Root); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("a half-removed copy loads: %v", err)
	}
	again, err := b.Open(Request{ID: s.ID, Workdir: s.Workdir, Folder: src, Ref: s.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(again.Dir, "notes.md")) != "n" || read(t, filepath.Join(again.Dir, "src/app.js")) != "js" {
		t.Fatal("the fresh copy is not the folder")
	}
	if _, err := os.Lstat(filepath.Join(again.Dir, "stale.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fresh copy holds what the close left: %v", err)
	}
	c, err := b.Checkpoint(again, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := b.Diff(again, Base, c.Name); err != nil || len(ch.Files) != 0 {
		t.Fatalf("the fresh copy's changes: %+v %v", ch, err)
	}
}

// What a close could not remove is removed when the runner starts.
func TestSweepRemoved(t *testing.T) {
	wd := t.TempDir()
	if err := SweepRemoved(wd); err != nil {
		t.Fatalf("nothing to sweep: %v", err)
	}
	fixture(t, removingDir(wd), map[string]string{"ws_1-1/copy/a.txt": "a"})
	// Where 9f37c6c put what it could not remove.
	fixture(t, oldRemovingDir(wd), map[string]string{"ws_2-1/copy/a.txt": "a"})
	if err := SweepRemoved(wd); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{removingDir(wd), oldRemovingDir(wd)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", p, err)
		}
	}
}

// Review evt_11939830d9ee167b P1, the rule on the failure path: a close that
// cannot move the copy out of its place puts its record back, so the
// workspace keeps a whole copy that loads, and a later close removes it.
// Before, the record was removed and the copy stayed with none.
func TestCloseThatCannotMoveTheCopyKeepsTheRecord(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"notes.md": "n"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"work.txt": "w"})
	refused := errors.New("refused")
	renameCopy = func(string, string) error { return refused }
	err := b.Close(s)
	renameCopy = os.Rename
	if !errors.Is(err, refused) {
		t.Fatalf("close: %v, want the refused move", err)
	}
	got, err := Load(s.Workdir, s.Root)
	if err != nil {
		t.Fatalf("the copy lost its record: %v", err)
	}
	if read(t, filepath.Join(got.Dir, "notes.md")) != "n" || read(t, filepath.Join(got.Dir, "work.txt")) != "w" {
		t.Fatal("the copy is not whole")
	}
	if left, _ := os.ReadDir(removingDir(s.Workdir)); len(left) != 0 {
		t.Fatalf("a failed close left %d entries aside", len(left))
	}
	if err := b.Close(got); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the second close left the copy: %v", err)
	}
}

// A copy in its place with no record (a stopped Open, a close that stopped
// after its record went) is removed by RemoveLeft, which a close calls when
// it finds no record. A space with a record is left to Close.
func TestRemoveLeft(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"notes.md": "n"})
	_, s := open(t, Copy, src)
	if err := RemoveLeft(s.Workdir, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.Workdir, s.Root); err != nil {
		t.Fatalf("RemoveLeft touched a space with a record: %v", err)
	}
	if err := dropRecord(s); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLeft(s.Workdir, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover is still in its place: %v", err)
	}
	if err := RemoveLeft(s.Workdir, "../x"); !errors.Is(err, ErrOutside) {
		t.Fatalf("an id outside: %v", err)
	}
}

// Review evt_11939830d9ee167b P2: every backend's Open takes the stop, in
// every step, its last one included. A stop that comes just before the record
// is written leaves no record and no copy, and Open answers the stop. Before,
// the git backend never read the stop, and the overlay's snapshot scan and
// Base checkpoint ran on and wrote the record.
func TestOpenStoppedBeforeItsRecord(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a", "d/b.txt": "b"})
	gitInit(t, src)
	for _, name := range openBackends(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			beforeRecord = func(*Space) { cancel() }
			defer func() { beforeRecord = nil }()
			wd := filepath.Join(t.TempDir(), "w")
			b, _ := Get(name)
			s, err := b.Open(Request{ID: "ws_1", Workdir: wd, Folder: src, Ctx: ctx})
			if err == nil {
				b.Close(s)
				t.Fatal("an Open stopped before its record made a copy")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("open: %v, want the stop", err)
			}
			assertNoSpace(t, wd, "ws_1")
		})
	}
}

// Review evt_11939830d9ee167b P2: a stop before Open starts is heard by every
// backend. Before, a git worktree was made and its turn started.
func TestOpenStoppedAtOnce(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	gitInit(t, src)
	for _, name := range openBackends(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			wd := filepath.Join(t.TempDir(), "w")
			b, _ := Get(name)
			if s, err := b.Open(Request{ID: "ws_1", Workdir: wd, Folder: src, Ctx: ctx}); err == nil {
				b.Close(s)
				t.Fatal("a stopped Open made a copy")
			}
			assertNoSpace(t, wd, "ws_1")
		})
	}
}

// The Base's hashes stop with the stop: a hash between two pieces of a file
// answers the stop.
func TestBaseHasherStops(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, map[string]string{"a.txt": "a"})
	info, err := os.Lstat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := baseHasher(ctx, time.Now(), filepath.Join(dir, "objects"))
	if _, err := h("a.txt", filepath.Join(dir, "a.txt"), info); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash after a stop: %v", err)
	}
}

// openBackends are the backends that make a copy here: copy and git, and
// overlay where this machine mounts one.
func openBackends(t *testing.T) []string {
	out := []string{Copy, Git}
	if overlayWhy(filepath.Join(t.TempDir(), "probe")) == "" {
		out = append(out, Overlay)
	}
	return out
}

// assertNoSpace says nothing of space id is left under wd: no record, nothing
// in its place, nothing aside.
func assertNoSpace(t *testing.T, wd, id string) {
	t.Helper()
	if _, err := LoadID(wd, id); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("load: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(wd, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy is in its place: %v", err)
	}
	if left, _ := os.ReadDir(removingDir(wd)); len(left) != 0 {
		t.Fatalf("%d entries left aside", len(left))
	}
}

// gitInit makes dir a repo with its files in one commit, so the git backend
// can make a worktree of it.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "one")
}

// Review evt_2dc8f5f466e43e2e P2: a stopped `git worktree add` leaves a
// worktree git locked ("initializing"), with its folder or without it. The
// next Open of the id removes it and makes a fresh worktree. Before, a single
// --force could not remove it, and prune left the missing one registered, so
// add failed with "missing but locked worktree" and no turn could start.
func TestGitOpenAfterAStoppedAdd(t *testing.T) {
	for _, folder := range []string{"still there", "gone"} {
		t.Run(folder, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "a"})
			gitInit(t, src)
			wd := filepath.Join(t.TempDir(), "w")
			root := filepath.Join(wd, "ws_1")
			gitIn(t, src, "worktree", "add", "-q", "--lock", "--reason", "initializing", root, "HEAD")
			if folder == "gone" {
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			}
			b, _ := Get(Git)
			s, err := b.Open(Request{ID: "ws_1", Workdir: wd, Folder: src})
			if err != nil {
				t.Fatalf("open after a stopped add: %v", err)
			}
			if read(t, filepath.Join(s.Dir, "a.txt")) != "a" {
				t.Fatal("the worktree is not the folder")
			}
			if err := b.Close(s); err != nil {
				t.Fatal(err)
			}
			assertNoSpace(t, wd, "ws_1")
			assertOnlyMainWorktree(t, src)
		})
	}
}

// Review evt_2dc8f5f466e43e2e P2: a git close that cannot move the worktree
// out of its place keeps its record, and the worktree stays whole and
// registered, so the workspace is usable and a later close finishes. Before,
// the record went first and a failed `git worktree remove` left a worktree
// with no record that every later Open failed on.
func TestGitCloseThatCannotMoveKeepsTheRecord(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	gitInit(t, src)
	b, s := open(t, Git, src)
	fixture(t, s.Dir, map[string]string{"work.txt": "w"})
	refused := errors.New("refused")
	renameCopy = func(string, string) error { return refused }
	err := b.Close(s)
	renameCopy = os.Rename
	if !errors.Is(err, refused) {
		t.Fatalf("close: %v, want the refused move", err)
	}
	got, err := Load(s.Workdir, s.Root)
	if err != nil {
		t.Fatalf("the worktree lost its record: %v", err)
	}
	if read(t, filepath.Join(got.Dir, "a.txt")) != "a" || read(t, filepath.Join(got.Dir, "work.txt")) != "w" {
		t.Fatal("the worktree is not whole")
	}
	if err := b.Close(got); err != nil {
		t.Fatal(err)
	}
	assertNoSpace(t, s.Workdir, s.ID)
	assertOnlyMainWorktree(t, src)
}

// A close that removes everything leaves nothing of its own in the workdir:
// no aside folder, and nothing in the copy's place. A git Open refused after
// its worktree was made is the case the refusal tests in cmd/agentbus hold.
func TestCloseLeavesTheWorkdirClean(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	gitInit(t, src)
	for _, name := range []string{Copy, Git} {
		t.Run(name, func(t *testing.T) {
			b, s := open(t, name, src)
			if err := b.Close(s); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{removingDir(s.Workdir), oldRemovingDir(s.Workdir), s.Root} {
				if exists(p) {
					t.Fatalf("%s left after close", p)
				}
			}
		})
	}
}

// assertOnlyMainWorktree says repo lists no worktree but its own.
func assertOnlyMainWorktree(t *testing.T, repo string) {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "worktree "); n != 1 {
		t.Fatalf("the repo lists %d worktrees, want its own only:\n%s", n, out)
	}
}
