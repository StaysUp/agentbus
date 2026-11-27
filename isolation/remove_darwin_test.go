package isolation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// lockFile sets Finder's Locked (uchg) on p, and clears it when the test
// ends so its temp folder can go.
func lockFile(t *testing.T, p string) {
	t.Helper()
	if err := unix.Chflags(p, unix.UF_IMMUTABLE); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Chflags(p, 0) })
}

func locked(p string) bool {
	var st syscall.Stat_t
	return syscall.Lstat(p, &st) == nil && st.Flags&unix.UF_IMMUTABLE != 0
}

// #96: a file the person locked in Finder is locked in the clone too. Close
// removes the whole copy, never part of it, and the person's folder keeps
// every file and its lock. Before, Close removed every file but the locked
// one, failed, and kept the record, so the next turn read the person's files
// as deleted and an apply deleted them.
func TestCloseCopyWithLockedFiles(t *testing.T) {
	for _, where := range []string{"person's file", "person's folder", "agent's file"} {
		t.Run(where, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"notes.md": "n", "src/app.js": "js", "contract.pdf": "pdf"})
			switch where {
			case "person's file":
				lockFile(t, filepath.Join(src, "contract.pdf"))
			case "person's folder":
				lockFile(t, filepath.Join(src, "src"))
			}
			b, s := open(t, Copy, src)
			if where == "agent's file" {
				fixture(t, s.Dir, map[string]string{"out/report.txt": "r"})
				lockFile(t, filepath.Join(s.Dir, "out/report.txt"))
			}
			if err := b.Close(s); err != nil {
				t.Fatalf("close: %v", err)
			}
			if _, err := Load(s.Workdir, s.Root); !errors.Is(err, ErrNoSpace) {
				t.Fatalf("load after close: %v", err)
			}
			for _, p := range []string{s.Root, s.state(), removingDir(s.Workdir)} {
				if left, _ := os.ReadDir(p); len(left) != 0 {
					t.Fatalf("%s holds %d entries after close", p, len(left))
				}
			}
			for name, want := range map[string]string{"notes.md": "n", "src/app.js": "js", "contract.pdf": "pdf"} {
				if got := read(t, filepath.Join(src, name)); got != want {
					t.Fatalf("the folder's %s after close: %q", name, got)
				}
			}
			switch where {
			case "person's file":
				if !locked(filepath.Join(src, "contract.pdf")) {
					t.Fatal("close cleared the lock on the person's file")
				}
			case "person's folder":
				if !locked(filepath.Join(src, "src")) {
					t.Fatal("close cleared the lock on the person's folder")
				}
			}
		})
	}
}

// Review evt_11939830d9ee167b P1: a copy whose top folder and a file in it
// are closed (chmod 0) and locked (uchg), as one command in the copy can make
// them, closes whole. Before, unlock opened the folder to clear its lock, the
// open failed, the copy could not be moved, and the record was already gone:
// a copy with no record that no close or Open could remove.
func TestCloseCopyClosedAndLocked(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"notes.md": "n", "src/app.js": "js"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"out/stuck.bin": "x"})
	// Clear every lock left under the workdir, wherever it went, so the temp
	// folder can go.
	t.Cleanup(func() {
		unix.Chflags(s.Root, 0)
		os.Chmod(s.Root, 0o700)
		filepath.WalkDir(s.Workdir, func(p string, d os.DirEntry, err error) error {
			unix.Chflags(p, 0)
			os.Chmod(p, 0o700)
			return nil
		})
	})
	for _, p := range []string{filepath.Join(s.Dir, "out", "stuck.bin"), filepath.Join(s.Dir, "out"), s.Root} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		if err := unix.Chflags(p, unix.UF_IMMUTABLE); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Close(s); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Load(s.Workdir, s.Root); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("load after close: %v", err)
	}
	if _, err := os.Lstat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy is in its place after close: %v", err)
	}
	if left, _ := os.ReadDir(removingDir(s.Workdir)); len(left) != 0 {
		t.Fatalf("close left %d entries aside", len(left))
	}
	again, err := b.Open(Request{ID: s.ID, Workdir: s.Workdir, Folder: src, Ref: s.Ref})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(again)
	if read(t, filepath.Join(again.Dir, "notes.md")) != "n" || read(t, filepath.Join(again.Dir, "src/app.js")) != "js" {
		t.Fatal("the next copy is not the folder")
	}
}

// Review evt_2dc8f5f466e43e2e P2: a git worktree holding a locked file closes
// whole, and git lists it no more. Before, `git worktree remove --force` exited
// 255 on the lock, after the record was gone.
func TestGitCloseWithALockedFile(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a"})
	gitInit(t, src)
	b, s := open(t, Git, src)
	fixture(t, s.Dir, map[string]string{"out/stuck.bin": "x"})
	lockFile(t, filepath.Join(s.Dir, "out/stuck.bin"))
	if err := b.Close(s); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertNoSpace(t, s.Workdir, s.ID)
	assertOnlyMainWorktree(t, src)
}

// lockLink sets Finder's Locked on the link p itself (chflags -h uchg), and
// clears it when the test ends.
func lockLink(t *testing.T, p string) {
	t.Helper()
	if out, err := exec.Command("chflags", "-h", "uchg", p).CombinedOutput(); err != nil {
		t.Fatalf("chflags -h: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("chflags", "-h", "nouchg", p).Run() })
}

// Review evt_2dc8f5f466e43e2e P2: a locked link is unlocked as a locked file
// is, so a close removes the whole copy and nothing waits aside. Before,
// removeAll skipped links, the copy's move succeeded, and the aside copy was
// never removed, by the close or by any runner start's sweep.
func TestCloseCopyWithALockedLink(t *testing.T) {
	for _, whose := range []string{"person's link", "agent's link"} {
		t.Run(whose, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "a"})
			if whose == "person's link" {
				if err := os.Symlink("a.txt", filepath.Join(src, "to-a")); err != nil {
					t.Fatal(err)
				}
				lockLink(t, filepath.Join(src, "to-a"))
			}
			b, s := open(t, Copy, src)
			if whose == "agent's link" {
				if err := os.Symlink("a.txt", filepath.Join(s.Dir, "to-a")); err != nil {
					t.Fatal(err)
				}
				lockLink(t, filepath.Join(s.Dir, "to-a"))
			}
			if err := b.Close(s); err != nil {
				t.Fatalf("close: %v", err)
			}
			assertNoSpace(t, s.Workdir, s.ID)
			if whose == "person's link" {
				var st syscall.Stat_t
				if err := syscall.Lstat(filepath.Join(src, "to-a"), &st); err != nil || st.Flags&unix.UF_IMMUTABLE == 0 {
					t.Fatalf("close cleared the lock on the person's link: %v", err)
				}
			}
		})
	}
}

// What a close left aside with a locked link in it goes at the next sweep.
func TestSweepRemovesALockedLink(t *testing.T) {
	wd := t.TempDir()
	dir := filepath.Join(removingDir(wd), "ws_1-1", "copy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "to-a")
	if err := os.Symlink("a.txt", link); err != nil {
		t.Fatal(err)
	}
	lockLink(t, link)
	if err := SweepRemoved(wd); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Lstat(removingDir(wd)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("left: %v", err)
	}
}
