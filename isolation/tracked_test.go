package isolation

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// builtRepo makes dir a repo that commits its build output, as a JavaScript
// GitHub Action commits dist/ (#87): src/main.js, dist/index.js, and
// config.local, added with -f past the .gitignore's *.local. Beside them, an
// untracked build/out.o, under a built-in rule.
func builtRepo(t *testing.T, dir string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	fixture(t, dir, map[string]string{".gitignore": "*.local\n", "src/main.js": "v1\n", "dist/index.js": "v1\n",
		"config.local": "mode=1\n", "build/out.o": "o1\n"})
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "src", "dist", ".gitignore"},
		{"add", "-f", "config.local"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-q", "-m", "c"},
	} {
		if out, err := exec.Command(git, append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// A turn's edits to files the folder's repo tracks reach the changeset and
// the folder, under a built-in rule (dist/) or the folder's own .gitignore
// (a force-added file) alike, and so does a new file in a folder the repo
// tracks files in. Untracked build output stays out. Before 0.6.7 the
// changeset held src/main.js alone, and apply said applied.
func TestTrackedFilesUnderIgnoreRules(t *testing.T) {
	src := t.TempDir()
	builtRepo(t, src)
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"src/main.js": "v2\n", "dist/index.js": "v2\n", "dist/chunk.js": "c\n",
		"config.local": "mode=2\n", "build/out.o": "o2\n", "other.local": "x\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"config.local", "dist/chunk.js", "dist/index.js", "src/main.js"}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
	if got := diffNames(t, b, s, Base, "t1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff: %v, want %v", got, want)
	}
	applied, err := applyIn(t, s, src, Base, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied.Files, want) {
		t.Fatalf("applied %v, want %v", applied.Files, want)
	}
	for p, c := range map[string]string{"src/main.js": "v2\n", "dist/index.js": "v2\n", "dist/chunk.js": "c\n",
		"config.local": "mode=2\n", "build/out.o": "o1\n", "other.local": "<missing>"} {
		if got := read(t, filepath.Join(src, filepath.FromSlash(p))); got != c {
			t.Errorf("folder %s: %q, want %q", p, got, c)
		}
	}
}

// A folder with no repo keeps the built-in list as it was: its dist/ is
// build output, left out.
func TestUntrackedBuildOutputStaysOut(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "dist/index.js": "v1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2", "dist/index.js": "v2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"a.txt"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// In a copy of several folders, each folder's own repo decides for that
// folder: a's committed dist/ is the work, b's untracked dist/ is not.
func TestTrackedFilesInSeveralFolders(t *testing.T) {
	a, c := t.TempDir(), t.TempDir()
	builtRepo(t, a)
	fixture(t, c, map[string]string{"x.txt": "x1", "dist/out.js": "o1"})
	b, s := openPlacesT(t, []Place{{Name: "a", Folder: a, Ref: "ra"}, {Name: "c", Folder: c, Ref: "rc"}})
	fixture(t, s.Dir, map[string]string{"a/dist/index.js": "v2\n", "a/config.local": "mode=2\n", "a/build/out.o": "o2\n",
		"c/x.txt": "x2", "c/dist/out.js": "o2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"a/config.local", "a/dist/index.js", "c/x.txt"}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
}

// An update brings the person's edit of a tracked file under a built-in rule
// into the copy: the person's side holds by the same rule as the copy's.
// Before, the edit never came in.
func TestUpdateBringsTrackedBuildOutput(t *testing.T) {
	src := t.TempDir()
	builtRepo(t, src)
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"src/main.js": "v2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, src, map[string]string{"dist/index.js": "theirs\n", "dist/new.js": "n\n"})
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(s.Dir, "dist", "index.js")) + read(t, filepath.Join(s.Dir, "dist", "new.js")); got != "theirs\nn\n" {
		t.Fatalf("the folder's dist/ in the copy: %q", got)
	}
	if got := read(t, filepath.Join(s.Dir, "src", "main.js")); got != "v2\n" {
		t.Fatalf("the session's work: %q", got)
	}
}

// The same in a copy of several folders, whose person's side reads each
// folder's built-in list from the copy's own.
func TestUpdateBringsTrackedBuildOutputInSeveralFolders(t *testing.T) {
	a, c := t.TempDir(), t.TempDir()
	builtRepo(t, a)
	fixture(t, c, map[string]string{"x.txt": "x1", "dist/out.js": "o1"})
	b, s := openPlacesT(t, []Place{{Name: "a", Folder: a, Ref: "ra"}, {Name: "c", Folder: c, Ref: "rc"}})
	fixture(t, s.Dir, map[string]string{"c/x.txt": "x2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, a, map[string]string{"dist/index.js": "theirs\n", "dist/new.js": "n\n"})
	fixture(t, c, map[string]string{"dist/out.js": "o-theirs"})
	paths := map[string]string{"ra": a, "rc": c}
	res, err := b.UpdateFrom(s, "", func(ref string) (string, bool) { p, ok := paths[ref]; return p, ok }, false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(s.Dir, "a", "dist", "index.js")) + read(t, filepath.Join(s.Dir, "a", "dist", "new.js")); got != "theirs\nn\n" {
		t.Fatalf("a's dist/ in the copy: %q", got)
	}
	if got := read(t, filepath.Join(s.Dir, "c", "dist", "out.js")); got != "o1" {
		t.Fatalf("c's untracked dist/ was merged: %q", got)
	}
}

// A folder that is a git worktree has its index in the main repo's git
// folder, which its .git file names: what it tracks holds there too.
func TestTrackedFilesInAWorktree(t *testing.T) {
	main := t.TempDir()
	builtRepo(t, main)
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, out)
	}
	b, s := open(t, Copy, wt)
	fixture(t, s.Dir, map[string]string{"dist/index.js": "v2\n", "config.local": "mode=2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, want := changesetNames(t, s, "t1"), []string{"config.local", "dist/index.js"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
}

// forceAdded is a repo at dir with a.txt committed and name committed with
// -f past the .gitignore's *.local.
func forceAdded(t *testing.T, dir, name string) {
	t.Helper()
	fixture(t, dir, map[string]string{".gitignore": "*.local\n", "a.txt": "a\n", name: "mode=1\n"})
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", ".gitignore", "a.txt"},
		{"add", "-f", "--", name},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-q", "-m", "c"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// A force-added file renamed by case alone in the copy is a rename, not a
// removal: the changeset holds both names, and the apply leaves the file
// under its new name. Before, the ignored file was looked up by the index's
// spelling, not found in the listing, and recorded as removed; the apply
// removed it from the folder.
func TestForceAddedCaseRenameApplies(t *testing.T) {
	src := t.TempDir()
	caseBlind(t, src)
	forceAdded(t, src, "config.local")
	b, s := open(t, Copy, src)
	os.Rename(filepath.Join(s.Dir, "config.local"), filepath.Join(s.Dir, "tmp-rename"))
	os.Rename(filepath.Join(s.Dir, "tmp-rename"), filepath.Join(s.Dir, "Config.local"))
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(onDisk(t, src), " "); got != ".gitignore Config.local a.txt" {
		t.Fatalf("the folder after the apply: %s", got)
	}
	if got := read(t, filepath.Join(src, "Config.local")); got != "mode=1\n" {
		t.Fatalf("Config.local: %q", got)
	}
}

// The same rename made by the person comes in with an update, and the copy
// keeps the file. Before, the update was clean and the copy lost it.
func TestUpdateKeepsAForceAddedCaseRename(t *testing.T) {
	src := t.TempDir()
	caseBlind(t, src)
	forceAdded(t, src, "config.local")
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(src, "config.local"), filepath.Join(src, "tmp-rename"))
	os.Rename(filepath.Join(src, "tmp-rename"), filepath.Join(src, "Config.local"))
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := strings.Join(onDisk(t, s.Dir), " "); got != ".gitignore Config.local a.txt" {
		t.Fatalf("the copy after the update: %s", got)
	}
}

// A force-added file whose name the disk keeps decomposed (NFD) while git's
// index holds it composed (NFC, core.precomposeunicode) is still the work:
// an edit of it reaches the folder. Before, it was never recorded.
func TestForceAddedDecomposedNameApplies(t *testing.T) {
	src := t.TempDir()
	nfd := "café.local"
	forceAdded(t, src, nfd)
	if des, _ := os.ReadDir(src); !hasName(des, nfd) {
		t.Skip("this disk does not keep a decomposed name as written")
	}
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{nfd: "mode=2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(src, nfd)); got != "mode=2\n" {
		t.Fatalf("the folder's %s: %q", nfd, got)
	}
}

func hasName(des []os.DirEntry, name string) bool {
	for _, de := range des {
		if de.Name() == name {
			return true
		}
	}
	return false
}

// A committed dist/ that the folder's own .gitignore ignores, its built files
// added with -f (the usual layout): an edit of a tracked file there is the
// work, and a new file there stays out, as git status leaves it out.
func TestIgnoredCommittedFolderKeepsTrackedFilesOnly(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{".gitignore": "dist/\n", "a.txt": "a\n", "dist/index.js": "v1\n"})
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", ".gitignore", "a.txt"},
		{"add", "-f", "dist/index.js"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-q", "-m", "c"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"dist/index.js": "v2\n", "dist/chunk.js": "c\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"dist/index.js"}) {
		t.Fatalf("changeset: %v, want [dist/index.js]", got)
	}
}

// commitIn makes dir a repo with files committed, the first of them by -f.
func commitIn(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	fixture(t, dir, files)
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-q", "-m", "c"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// A repo that commits its build folder under a capitalised name (Dist/,
// Build/, Target/), which the built-in list names in lower case: a new file
// beside the tracked one is the work, on Apply, and a turn that only adds it
// is a change. Before (review evt_a8b60d193c994a2c), the built-in rule was
// dropped only by a case-sensitive test of ours, while git's ignore folds case
// on this disk, so the new file never entered a checkpoint and Apply said
// applied without it.
func TestCapitalisedBuildFolderTakesNewFiles(t *testing.T) {
	for _, dir := range []string{"Dist", "Build", "Target"} {
		t.Run(dir, func(t *testing.T) {
			src := t.TempDir()
			caseBlind(t, src)
			commitIn(t, src, map[string]string{dir + "/index.js": "v1\n", "a.txt": "a\n"})
			b, s := open(t, Copy, src)
			fixture(t, s.Dir, map[string]string{dir + "/chunk.js": "c\n"})
			cp, err := b.Checkpoint(s, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if cp.Name != "t1" {
				t.Fatalf("a turn that only added %s/chunk.js recorded no change", dir)
			}
			fixture(t, s.Dir, map[string]string{dir + "/index.js": "v2\n"})
			if _, err := b.Checkpoint(s, "t2"); err != nil {
				t.Fatal(err)
			}
			if _, err := applyIn(t, s, src, Base, "t2"); err != nil {
				t.Fatal(err)
			}
			if got := read(t, filepath.Join(src, dir, "index.js")) + read(t, filepath.Join(src, dir, "chunk.js")); got != "v2\nc\n" {
				t.Fatalf("the folder's %s/ after the apply: %q", dir, got)
			}
		})
	}
}

// The same folders on an Update: a new file the person puts beside the
// tracked one comes into the copy. Before, the update was clean without it.
func TestUpdateBringsANewFileInACapitalisedBuildFolder(t *testing.T) {
	for _, dir := range []string{"Dist", "Build", "Target"} {
		t.Run(dir, func(t *testing.T) {
			src := t.TempDir()
			caseBlind(t, src)
			commitIn(t, src, map[string]string{dir + "/index.js": "v1\n", "a.txt": "a\n"})
			b, s := open(t, Copy, src)
			fixture(t, s.Dir, map[string]string{"a.txt": "a2\n"})
			if _, err := b.Checkpoint(s, "t1"); err != nil {
				t.Fatal(err)
			}
			fixture(t, src, map[string]string{dir + "/chunk.js": "theirs\n"})
			res, err := b.UpdateFrom(s, "", at(src), false)
			if err != nil || !res.Clean {
				t.Fatalf("update: %+v %v", res, err)
			}
			if got := read(t, filepath.Join(s.Dir, dir, "chunk.js")); got != "theirs\n" {
				t.Fatalf("the copy's %s/chunk.js after the update: %q", dir, got)
			}
		})
	}
}

// The same in a copy of several folders: the repo folder's Dist/ takes a new
// file on Apply and on Update, while a folder that is no repo keeps the
// built-in list.
func TestCapitalisedBuildFolderInSeveralFolders(t *testing.T) {
	a, c := t.TempDir(), t.TempDir()
	caseBlind(t, a)
	commitIn(t, a, map[string]string{"Dist/index.js": "v1\n"})
	fixture(t, c, map[string]string{"x.txt": "x1", "dist/out.js": "o1"})
	b, s := openPlacesT(t, []Place{{Name: "a", Folder: a, Ref: "ra"}, {Name: "c", Folder: c, Ref: "rc"}})
	fixture(t, s.Dir, map[string]string{"a/Dist/chunk.js": "c\n", "c/dist/out.js": "o2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, want := changesetNames(t, s, "t1"), []string{"a/Dist/chunk.js"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
	fixture(t, a, map[string]string{"Dist/theirs.js": "t\n"})
	paths := map[string]string{"ra": a, "rc": c}
	res, err := b.UpdateFrom(s, "", func(ref string) (string, bool) { p, ok := paths[ref]; return p, ok }, false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(s.Dir, "a", "Dist", "theirs.js")); got != "t\n" {
		t.Fatalf("a/Dist/theirs.js in the copy: %q", got)
	}
}
