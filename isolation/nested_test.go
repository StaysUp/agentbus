package isolation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// commitRepo makes dir a git repo holding files, committed: a repo a person
// keeps inside a folder, or a folder of a workspace.
func commitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	fixture(t, dir, files)
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c"},
	} {
		if out, err := exec.Command(git, append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v %s", args, dir, err, out)
		}
	}
}

// openPlacesT opens a copy of several folders.
func openPlacesT(t *testing.T, places []Place) (Backend, *Space) {
	t.Helper()
	b, _ := Get(Copy)
	s, err := b.Open(Request{ID: "ws_multi", Workdir: t.TempDir(), Places: places})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { b.Close(s) })
	return b, s
}

// nestedSrc is a folder with top.txt and a committed repo inner/ holding code.
func nestedSrc(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	fixture(t, src, map[string]string{"top.txt": "t1"})
	commitRepo(t, filepath.Join(src, "inner"), map[string]string{"code": "c1"})
	return src
}

// The bug, as measured on 2026-11-23: a folder with top.txt and a committed
// inner repo. A turn edits top.txt, edits inner/code and adds inner/new.
// Before, the changeset listed only top.txt. Now all three are there, a diff
// shows them, a restore reverts them, and the inner repo's own git still works.
func TestNestedRepoTracked(t *testing.T) {
	b, s := open(t, Copy, nestedSrc(t))
	if !s.hasShadow() || !s.Nested {
		t.Skip("the copy made no shadow repo on this disk")
	}
	fixture(t, s.Dir, map[string]string{"top.txt": "t2", "inner/code": "c2", "inner/new": "n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"inner/code", "inner/new", "top.txt"}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
	if got := diffNames(t, b, s, Base, "t1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff: %v, want %v", got, want)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(s.Dir, "inner"), "status", "--porcelain").CombinedOutput(); err != nil || len(out) == 0 {
		t.Fatalf("the inner repo's git in the copy: %v %q", err, out)
	}
	// A file the agent made in the inner repo and no checkpoint saw is removed
	// by a restore too; the repo's own .git stays.
	fixture(t, s.Dir, map[string]string{"inner/unseen": "u"})
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s.Dir, "inner", "code")) != "c1" || read(t, filepath.Join(s.Dir, "top.txt")) != "t1" {
		t.Fatal("a restore left a change in place")
	}
	for _, p := range []string{"new", "unseen"} {
		if _, err := os.Lstat(filepath.Join(s.Dir, "inner", p)); !os.IsNotExist(err) {
			t.Fatalf("a restore left inner/%s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "inner", ".git", "HEAD")); err != nil {
		t.Fatal("a restore removed the inner repo's own .git")
	}
}

// A repo inside a repo is followed too.
func TestNestedInNested(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"top": "t"})
	commitRepo(t, filepath.Join(src, "a", "b"), map[string]string{"deep": "d1"})
	commitRepo(t, filepath.Join(src, "a"), map[string]string{"mid": "m1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a/mid": "m2", "a/b/deep": "d2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"a/b/deep", "a/mid"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// A repo the folder's own .gitignore ignores (clones kept for reference, say)
// stays out, as before: its files were never the folder's.
func TestIgnoredNestedRepoNotTracked(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{".gitignore": "_refs/\n", "a.txt": "a"})
	commitRepo(t, filepath.Join(src, "_refs", "other"), map[string]string{"x.go": "x"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"_refs/other/x.go": "y", "a.txt": "b"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"a.txt"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// A copy made before 0.6.6 (no nested in its record) records a nested repo by
// its commit only, exactly as it did.
func TestNestedLegacyCopyUnchanged(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"top.txt": "t1"})
	b, s := open(t, Copy, src)
	s.Nested = false // as space.json reads for a copy made before
	commitRepo(t, filepath.Join(s.Dir, "inner"), map[string]string{"code": "c1"})
	fixture(t, s.Dir, map[string]string{"top.txt": "t2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"top.txt"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// Review rounds 1 and 2 of 0.6.6 step 3 (2026-11-24): each case they found,
// on the one-history mechanism that replaced the per-repo one.

// A turn moves a nested repo and edits it: its files are recorded at the new
// path (it is found again at the checkpoint, not only when the copy opened),
// and a restore puts it back where it was.
func TestNestedRepoMoved(t *testing.T) {
	b, s := open(t, Copy, nestedSrc(t))
	if err := os.Rename(filepath.Join(s.Dir, "inner"), filepath.Join(s.Dir, "moved")); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"moved/code": "c2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"inner/code", "moved/code"}) {
		t.Fatalf("changeset: %v", got)
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s.Dir, "inner", "code")) != "c1" {
		t.Fatal("a restore did not bring the moved repo's files back")
	}
}

// A repo a turn clones in is recorded by its files too.
func TestNestedRepoClonedByATurn(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a": "a"})
	b, s := open(t, Copy, src)
	commitRepo(t, filepath.Join(s.Dir, "lib"), map[string]string{"lib.go": "l"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"lib/lib.go"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// A turn replaces a nested repo's folder with a file: the checkpoint records
// the file and the repo's files as removed, and a restore to that checkpoint
// keeps the file.
func TestNestedRepoReplacedByFile(t *testing.T) {
	b, s := open(t, Copy, nestedSrc(t))
	if err := os.RemoveAll(filepath.Join(s.Dir, "inner")); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"inner": "now a file"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"top.txt": "t3"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s.Dir, "inner")) != "now a file" {
		t.Fatal("a restore lost the file the turn put in place of the repo")
	}
}

// A turn replaces a folder of the copy with a link to a folder outside it.
// The checkpoint records the link, not what it points at, and a restore makes
// the folder again inside the copy: nothing is read or written through it.
func TestFolderReplacedByLink(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	fixture(t, one, map[string]string{"code": "base"})
	fixture(t, two, map[string]string{"b": "b"})
	b, s := openPlacesT(t, []Place{{Name: "one", Folder: one, Ref: "fld_1"}, {Name: "two", Folder: two, Ref: "fld_2"}})
	outside := t.TempDir()
	fixture(t, outside, map[string]string{"code": "outside"})
	if err := os.RemoveAll(filepath.Join(s.Dir, "one")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Dir, "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Files) != 2 || cs.Files[0].Path != "one" || cs.Files[1].Path != "one/code" || cs.Files[1].Now != nil {
		t.Fatalf("changeset, want the link added and one/code removed: %+v", cs.Files)
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(outside, "code")) != "outside" {
		t.Fatal("a restore wrote through a link out of the copy")
	}
	if fi, err := os.Lstat(filepath.Join(s.Dir, "one")); err != nil || !fi.IsDir() || read(t, filepath.Join(s.Dir, "one", "code")) != "base" {
		t.Fatalf("the folder was not made again in the copy: %v", err)
	}
}

// A .gitignore that un-ignores a folder never makes one file appear twice.
func TestGitignoreNegationOnce(t *testing.T) {
	b, s := open(t, Copy, nestedSrc(t))
	fixture(t, s.Dir, map[string]string{".gitignore": "!inner/\n!/inner/\n", "inner/code": "c2"})
	if err := os.Rename(filepath.Join(s.Dir, "inner", ".git"), filepath.Join(s.Dir, "dotgit")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range cs.Files {
		if f.Path == "inner/code" {
			n++
			if f.Base == nil {
				t.Error("inner/code is recorded as new")
			}
		}
	}
	if n != 1 {
		t.Fatalf("inner/code is in the changeset %d times: %+v", n, cs.Files)
	}
}

// An update from a folder that is not where it was refuses and leaves the
// copy as it was. One whose folder put a file in place of a nested repo is
// refused too: the merge would remove the copy's repo and its history (which
// can hold the agent's own commits), so nothing changes until it is moved.
func TestUpdateMovedOrReplaced(t *testing.T) {
	src := nestedSrc(t)
	b, s := open(t, Copy, src)
	if _, err := b.UpdateFrom(s, "", at(filepath.Join(t.TempDir(), "moved")), false); !errors.Is(err, ErrNoFolder) {
		t.Fatalf("update from a folder that moved: %v", err)
	}
	if read(t, filepath.Join(s.Dir, "inner", "code")) != "c1" {
		t.Fatal("a refused update changed the copy")
	}
	if err := os.RemoveAll(filepath.Join(src, "inner")); err != nil {
		t.Fatal(err)
	}
	fixture(t, src, map[string]string{"inner": "a file now"})
	if _, err := b.UpdateFrom(s, "", at(src), false); !errors.Is(err, ErrRepoInTheWay) {
		t.Fatalf("update over the copy's repo: %v, want ErrRepoInTheWay", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "inner", ".git", "HEAD")); err != nil || read(t, filepath.Join(s.Dir, "inner", "code")) != "c1" {
		t.Fatalf("a refused update changed the copy's repo: %v", err)
	}
}

// An update never reads through a link in the person's folder: a folder the
// person replaced by a link to elsewhere comes in as the link.
func TestUpdateDoesNotFollowLinks(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"top": "t", "docs/readme": "r"})
	b, s := open(t, Copy, src)
	outside := t.TempDir()
	fixture(t, outside, map[string]string{"readme": "SECRET"})
	if err := os.RemoveAll(filepath.Join(src, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "docs")); err != nil {
		t.Fatal(err)
	}
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if fi, err := os.Lstat(filepath.Join(s.Dir, "docs")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link did not come in as a link: %v", err)
	}
	cs, err := ChangesetOf(s, res.Theirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cs.Files {
		if f.Path == "docs/readme" && f.Now != nil {
			t.Fatal("the update read a file through a link")
		}
	}
}

// A copy of several folders holds each under its name, in one history. A
// changeset's paths start with the folder's name, nested repos included; a
// no-op checkpoint records nothing; a file beside the folders is recorded under
// its own path; and a restore resets everything.
func TestPlacesCopy(t *testing.T) {
	fw, ck := t.TempDir(), t.TempDir()
	commitRepo(t, fw, map[string]string{"main.go": "m1"})
	commitRepo(t, filepath.Join(fw, "vendor", "lib"), map[string]string{"lib.go": "l1"})
	fixture(t, ck, map[string]string{"app.tsx": "a1"})
	b, s := openPlacesT(t, []Place{{Name: "framework", Folder: fw, Ref: "fld_1"}, {Name: "cockpit", Folder: ck, Ref: "fld_2"}})
	if !s.hasShadow() || !s.Nested || s.Dir != s.Root || len(s.Folders) != 2 || s.Folders[1] != (SpaceFolder{Name: "cockpit", Ref: "fld_2"}) {
		t.Fatalf("space: %+v", s)
	}
	fixture(t, s.Dir, map[string]string{"cockpit/app.tsx": "a2"})
	if c, err := b.Checkpoint(s, "t1"); err != nil || c.Name != "t1" {
		t.Fatalf("checkpoint: %+v %v", c, err)
	}
	if c, err := b.Checkpoint(s, "t2"); err != nil || c.Name != "t1" {
		t.Fatalf("a no-op checkpoint: %+v %v", c, err)
	}
	fixture(t, s.Dir, map[string]string{"framework/main.go": "m2", "framework/vendor/lib/lib.go": "l2", "notes.md": "n"})
	if _, err := b.Checkpoint(s, "t3"); err != nil {
		t.Fatal(err)
	}
	want := []string{"cockpit/app.tsx", "framework/main.go", "framework/vendor/lib/lib.go", "notes.md"}
	if got := changesetNames(t, s, "t3"); !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s.Dir, "framework", "main.go")) != "m1" || read(t, filepath.Join(s.Dir, "framework", "vendor", "lib", "lib.go")) != "l1" ||
		read(t, filepath.Join(s.Dir, "cockpit", "app.tsx")) != "a2" {
		t.Fatal("a restore to t1 did not reset every folder to t1")
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "notes.md")); !os.IsNotExist(err) {
		t.Fatal("a restore left a file beside the folders")
	}
	again, err := LoadID(s.Workdir, s.ID)
	if err != nil || !reflect.DeepEqual(again.Folders, s.Folders) || !again.Nested {
		t.Fatalf("load: %+v %v", again, err)
	}
}

// The built-in ignores hold at each folder's top in a copy of several, as in
// a copy of one: a folder's node_modules stays out.
func TestPlacesBuiltinIgnores(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	fixture(t, one, map[string]string{"a": "a"})
	fixture(t, two, map[string]string{"b": "b"})
	b, s := openPlacesT(t, []Place{{Name: "one", Folder: one, Ref: "fld_1"}, {Name: "two", Folder: two, Ref: "fld_2"}})
	fixture(t, s.Dir, map[string]string{"one/node_modules/x/i.js": "x", "one/a": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"one/a"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// An update of a copy of several folders merges each folder from its own
// source in one merge: a conflict in one changes nothing in any, and a folder
// not on this machine refuses it.
func TestPlacesUpdateAllOrNothing(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	fixture(t, one, map[string]string{"a.txt": "a1\n"})
	fixture(t, two, map[string]string{"b.txt": "b1\n"})
	b, s := openPlacesT(t, []Place{{Name: "one", Folder: one, Ref: "fld_1"}, {Name: "two", Folder: two, Ref: "fld_2"}})
	paths := func(ref string) (string, bool) {
		switch ref {
		case "fld_1":
			return one, true
		case "fld_2":
			return two, true
		}
		return "", false
	}
	fixture(t, s.Dir, map[string]string{"two/b.txt": "OURS\n", "beside.md": "mine\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, one, map[string]string{"a.txt": "THEIRS\n"})
	fixture(t, two, map[string]string{"b.txt": "THEIRS\n"})
	res, err := b.UpdateFrom(s, "", paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean || !reflect.DeepEqual(res.Conflicts, []string{"two/b.txt"}) {
		t.Fatalf("update: %+v, want a conflict on two/b.txt", res)
	}
	if read(t, filepath.Join(s.Dir, "one", "a.txt")) != "a1\n" {
		t.Fatal("a conflict in one folder still merged another")
	}
	fixture(t, two, map[string]string{"b.txt": "b1\n"})
	if res, err = b.UpdateFrom(s, "", paths, false); err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if read(t, filepath.Join(s.Dir, "one", "a.txt")) != "THEIRS\n" || read(t, filepath.Join(s.Dir, "two", "b.txt")) != "OURS\n" ||
		read(t, filepath.Join(s.Dir, "beside.md")) != "mine\n" {
		t.Fatal("the clean update did not bring each folder in and keep the session's own work")
	}
	if got := diffNames(t, b, s, res.Theirs, res.Merged); !reflect.DeepEqual(got, []string{"beside.md", "two/b.txt"}) {
		t.Fatalf("theirs..merged: %v", got)
	}
	if _, err := b.UpdateFrom(s, res.Theirs, func(string) (string, bool) { return "", false }, false); !errors.Is(err, ErrNoFolder) {
		t.Fatalf("update with no folder: %v", err)
	}
}

// A copy of several folders refuses one inside another (the same folder twice
// included, by another path through a symlink too), and two folders of one
// name. Control never reads a folder, so the runner is where two references to
// one real folder are caught, before anything is copied.
func TestPlacesRefused(t *testing.T) {
	a := t.TempDir()
	inner := filepath.Join(a, "sub")
	fixture(t, inner, map[string]string{"x": "x"})
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(a, alias); err != nil {
		t.Fatal(err)
	}
	b, _ := Get(Copy)
	for _, places := range [][]Place{
		{{Name: "a", Folder: a}, {Name: "sub", Folder: inner}},
		{{Name: "a", Folder: a}, {Name: "alias", Folder: alias}},
		{{Name: "a", Folder: a}, {Name: "b", Folder: a}},
		{{Name: "a", Folder: a}, {Name: "A", Folder: t.TempDir()}},
		{{Name: "a", Folder: a}, {Name: ".git", Folder: t.TempDir()}},
	} {
		_, err := b.Open(Request{ID: "ws_x", Workdir: t.TempDir(), Places: places})
		var rf *Refusal
		if !errors.As(err, &rf) {
			t.Errorf("open %+v: %v, want a refusal", places, err)
		}
	}
}

// A merge (Update with merge) of a copy of several folders: a conflict in one
// folder leaves both sides between conflict markers there, names the file by
// its copy path, and still merges the other folder's change.
func TestPlacesMergeLeavesMarkers(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	fixture(t, one, map[string]string{"a.txt": "a1\n"})
	fixture(t, two, map[string]string{"b.txt": "b1\n"})
	b, s := openPlacesT(t, []Place{{Name: "one", Folder: one, Ref: "fld_1"}, {Name: "two", Folder: two, Ref: "fld_2"}})
	paths := func(ref string) (string, bool) {
		switch ref {
		case "fld_1":
			return one, true
		case "fld_2":
			return two, true
		}
		return "", false
	}
	fixture(t, s.Dir, map[string]string{"two/b.txt": "OURS\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, one, map[string]string{"a.txt": "THEIRS\n"})
	fixture(t, two, map[string]string{"b.txt": "THEIRS\n"})
	res, err := b.UpdateFrom(s, "", paths, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clean || !reflect.DeepEqual(res.Unresolved, []string{"two/b.txt"}) {
		t.Fatalf("merge: %+v, want two/b.txt unresolved", res)
	}
	if got := read(t, filepath.Join(s.Dir, "two", "b.txt")); !strings.Contains(got, "OURS\n") || !strings.Contains(got, "THEIRS\n") || !strings.Contains(got, "<<<<<<< ") {
		t.Fatalf("two/b.txt holds no markers: %q", got)
	}
	if read(t, filepath.Join(s.Dir, "one", "a.txt")) != "THEIRS\n" {
		t.Fatal("the other folder's change did not merge")
	}
}

// Final review of 0.6.6 step 3 (Codex, 2026-11-24): one test per finding.

// A turn removes every file of a nested repo. A discard, a restore to that
// turn, and an update all keep the repo's own .git: the clean leaves a repo
// the checkpoint saw alone and cleans inside it on its own.
func TestNestedRepoGitKept(t *testing.T) {
	src := nestedSrc(t)
	b, s := open(t, Copy, src)
	gitHead := filepath.Join(s.Dir, "inner", ".git", "HEAD")
	if err := os.Remove(filepath.Join(s.Dir, "inner", "code")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"inner/dropped": "d", "top.txt": "t2"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gitHead); err != nil {
		t.Fatal("a restore to a checkpoint holding none of the repo's files removed its .git")
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "inner", "dropped")); !os.IsNotExist(err) {
		t.Fatal("a restore left a file a dropped turn made in the repo")
	}
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if _, err := os.Stat(gitHead); err != nil {
		t.Fatal("an update removed the repo's .git")
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gitHead); err != nil || read(t, filepath.Join(s.Dir, "inner", "code")) != "c1" {
		t.Fatalf("a discard did not bring the repo back whole: %v", err)
	}
}

// A nested repo is staged by the same rules at the first checkpoint and every
// later one: an unrelated turn shows nothing of it appearing or vanishing.
func TestNestedIgnoreRulesSteady(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{".gitignore": "/inner/excluded.txt\n", "top.txt": "t1"})
	commitRepo(t, filepath.Join(src, "inner"), map[string]string{"code": "c", "excluded.txt": "x", "build/source.go": "s"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"top.txt": "t2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"top.txt"}) {
		t.Fatalf("an unrelated turn changed the nested repo: %v", got)
	}
	fixture(t, s.Dir, map[string]string{"top.txt": "t3"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if got := diffNames(t, b, s, "t1", "t2"); !reflect.DeepEqual(got, []string{"top.txt"}) {
		t.Fatalf("a second unrelated turn changed the nested repo: %v", got)
	}
}

// A folder of a copy of several named like a built-in ignore (build) is a
// folder of the person's: its edits are recorded.
func TestPlacesFolderNamedBuild(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	fixture(t, one, map[string]string{"a": "a"})
	fixture(t, two, map[string]string{"b": "b"})
	b, s := openPlacesT(t, []Place{{Name: "build", Folder: one, Ref: "fld_1"}, {Name: "two", Folder: two, Ref: "fld_2"}})
	fixture(t, s.Dir, map[string]string{"build/a": "a2", "build/node_modules/x": "x"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"build/a"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// Two names for one folder that differ only in case, on a disk that ignores
// case, are one folder: refused.
func TestPlacesCaseAliasRefused(t *testing.T) {
	a := filepath.Join(t.TempDir(), "proj")
	fixture(t, a, map[string]string{"x": "x"})
	upper := filepath.Join(filepath.Dir(a), "PROJ")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("this disk tells case apart")
	}
	b, _ := Get(Copy)
	_, err := b.Open(Request{ID: "ws_x", Workdir: t.TempDir(), Places: []Place{{Name: "a", Folder: a}, {Name: "b", Folder: upper}}})
	var rf *Refusal
	if !errors.As(err, &rf) {
		t.Fatalf("open: %v, want a refusal", err)
	}
}

// Final review round 2 of 0.6.6 step 3 (Codex, 2026-11-24), on the rule that
// replaced per-repo staging.

// A file the record holds stays held while it is there, though a .gitignore
// now leaves it out: an update never removes it from the copy.
func TestUpdateKeepsTrackedFileNowIgnored(t *testing.T) {
	src := nestedSrc(t)
	b, s := open(t, Copy, src)
	fixture(t, src, map[string]string{"inner/.gitignore": "code\n"})
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if read(t, filepath.Join(s.Dir, "inner", "code")) != "c1" {
		t.Fatal("an update removed a held file a .gitignore now leaves out")
	}
	if got := changesetNames(t, s, res.Theirs); !reflect.DeepEqual(got, []string{"inner/.gitignore"}) {
		t.Fatalf("theirs holds, beside base: %v", got)
	}
}

// The built-in list holds at each folder's top only: in a folder named one, a
// path one/one/build/... is the person's, and the folder's node_modules is
// left out by a checkpoint and by an update alike.
func TestPlacesBuiltinsAtFolderTop(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	commitRepo(t, one, map[string]string{"one/build/source.txt": "s1", "a": "a"})
	fixture(t, two, map[string]string{"b": "b"})
	b, s := openPlacesT(t, []Place{{Name: "one", Folder: one, Ref: "fld_1"}, {Name: "two", Folder: two, Ref: "fld_2"}})
	paths := func(ref string) (string, bool) {
		if ref == "fld_1" {
			return one, true
		}
		return two, ref == "fld_2"
	}
	fixture(t, s.Dir, map[string]string{"one/one/build/source.txt": "s2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"one/one/build/source.txt"}) {
		t.Fatalf("changeset: %v", got)
	}
	fixture(t, one, map[string]string{"node_modules/x.js": "x"})
	res, err := b.UpdateFrom(s, "", paths, false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := changesetNames(t, s, res.Theirs); len(got) != 0 {
		t.Fatalf("the update brought in what a checkpoint leaves out: %v", got)
	}
}

// Final review round 3 of 0.6.6 step 3 (Codex, 2026-11-24).

// A folder that cannot be read is not a folder whose files were removed: a
// checkpoint fails rather than record them as gone.
func TestUnreadableFolderNotRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	src := t.TempDir()
	fixture(t, src, map[string]string{"sub/code": "c", "a": "a"})
	b, s := open(t, Copy, src)
	sub := filepath.Join(s.Dir, "sub")
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o755)
	if c, err := b.Checkpoint(s, "t1"); err == nil {
		os.Chmod(sub, 0o755)
		if c.Name == "t1" && changeset(t, s, "t1")["sub/code"] {
			t.Fatal("an unreadable folder's file was recorded as removed")
		}
		t.Fatalf("a checkpoint over an unreadable folder: %+v, want an error", c)
	}
}

// A restore to a state with a file where the copy now holds a nested repo is
// refused, and the repo and its history stay: git's reset would remove them.
func TestRestoreRefusesToEraseARepo(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"inner": "a file", "a": "a"})
	b, s := open(t, Copy, src)
	if err := os.Remove(filepath.Join(s.Dir, "inner")); err != nil {
		t.Fatal(err)
	}
	commitRepo(t, filepath.Join(s.Dir, "inner"), map[string]string{"code": "c"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, Base); !errors.Is(err, ErrRepoInTheWay) {
		t.Fatalf("restore over a repo: %v, want ErrRepoInTheWay", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "inner", ".git", "HEAD")); err != nil {
		t.Fatal("a refused restore removed the repo's history")
	}
}

// Final review round 4 of 0.6.6 step 3 (Codex, 2026-11-24).

// A repo the rules ignore is still a repo: a restore that would write a file
// over it is refused, and its history stays.
func TestRestoreRefusesToEraseAnIgnoredRepo(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"inner": "a file", "a": "a"})
	b, s := open(t, Copy, src)
	if err := os.Remove(filepath.Join(s.Dir, "inner")); err != nil {
		t.Fatal(err)
	}
	commitRepo(t, filepath.Join(s.Dir, "inner"), map[string]string{"code": "c"})
	fixture(t, s.Dir, map[string]string{".gitignore": "inner/\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, Base); !errors.Is(err, ErrRepoInTheWay) {
		t.Fatalf("restore over an ignored repo: %v, want ErrRepoInTheWay", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "inner", ".git", "HEAD")); err != nil {
		t.Fatal("a refused restore removed an ignored repo's history")
	}
}

// A checkpoint that fails part way leaves the record as it was: a file it was
// about to drop is still held, and the next checkpoint records it as changed,
// never as removed.
func TestFailedCheckpointLeavesRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	src := t.TempDir()
	fixture(t, src, map[string]string{"keep.txt": "k1", "a": "a"})
	b, s := open(t, Copy, src)
	if err := os.Remove(filepath.Join(s.Dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{".gitignore": "keep.txt\n", "locked": "l"})
	locked := filepath.Join(s.Dir, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err == nil {
		t.Fatal("a checkpoint over an unreadable file succeeded")
	}
	os.Chmod(locked, 0o644)
	fixture(t, s.Dir, map[string]string{"keep.txt": "k2"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t2")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cs.Files {
		if f.Path == "keep.txt" && f.Now == nil {
			t.Fatal("a file that is there was recorded as removed after a failed checkpoint")
		}
	}
}

// A discard repairs a folder a turn made unreadable, as before.
func TestRestoreRepairsUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	src := t.TempDir()
	fixture(t, src, map[string]string{"sub/code": "c", "a": "a"})
	b, s := open(t, Copy, src)
	sub := filepath.Join(s.Dir, "sub")
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o755)
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatalf("restore over an unreadable folder: %v", err)
	}
	if read(t, filepath.Join(sub, "code")) != "c" {
		t.Fatal("the folder was not repaired")
	}
}

// An empty folder is copied, with an empty Base.
func TestEmptyFolderCopied(t *testing.T) {
	b, s := open(t, Copy, t.TempDir())
	if c, err := b.Checkpoint(s, "t1"); err != nil || c.Name != Base {
		t.Fatalf("checkpoint of an empty copy: %+v %v", c, err)
	}
}

// Final review round 5 of 0.6.6 step 3 (Codex, 2026-11-24).

// A folder named through a link is copied as the folder, never as the link:
// a write in the copy never reaches the person's folder.
func TestLinkedFolderCopied(t *testing.T) {
	real := t.TempDir()
	fixture(t, real, map[string]string{"code": "c"})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, one := open(t, Copy, link)
	fixture(t, one.Dir, map[string]string{"code": "copy"})
	other := t.TempDir()
	fixture(t, other, map[string]string{"b": "b"})
	_, s := openPlacesT(t, []Place{{Name: "one", Folder: link, Ref: "fld_1"}, {Name: "two", Folder: other, Ref: "fld_2"}})
	if fi, err := os.Lstat(filepath.Join(s.Dir, "one")); err != nil || !fi.IsDir() {
		t.Fatalf("the folder was copied as a link: %v", err)
	}
	fixture(t, s.Dir, map[string]string{"one/code": "copy"})
	if read(t, filepath.Join(real, "code")) != "c" {
		t.Fatal("a write in the copy reached the person's folder")
	}
}

// What the latest checkpoint holds stays held, whatever an index a stop left
// behind says: a stop after staging (before the checkpoint's commit) cannot
// make a held file look removed later.
func TestHeldIsTheCheckpoint(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"keep.txt": "k1", "a": "a"})
	b, s := open(t, Copy, src)
	if err := os.Remove(filepath.Join(s.Dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{".gitignore": "keep.txt\n"})
	// Staged, as a checkpoint does, and then stopped before its commit.
	if err := s.shadowAdd(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"keep.txt": "k2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cs.Files {
		if f.Path == "keep.txt" && f.Now == nil {
			t.Fatal("a held file that is there was recorded as removed")
		}
	}
}
