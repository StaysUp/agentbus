package isolation

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Codex review t_56de25744b974a9b and t_ed8451e103f35c21: which files git
// tracks was read once, at Open, of the folder's own repo only, by AgentBus
// itself. Each test below failed before git was asked again, at every
// checkpoint, of the repo that owns each path (gitwork.go). The first four
// are the reviewer's own reproductions.

// P1: a tracked file a rule matches, gone at one checkpoint and made again
// before the next, is the work again: the changeset holds it, never its
// removal, so Apply does not remove the person's file.
func TestRecreatedTrackedFileIsTheWork(t *testing.T) {
	src := t.TempDir()
	builtRepo(t, src)
	b, s := open(t, Copy, src)
	if err := os.Rename(filepath.Join(s.Dir, "config.local"), filepath.Join(t.TempDir(), "config.local")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"config.local": "mode=2\n"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyIn(t, s, src, Base, "t2"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(src, "config.local")); got != "mode=2\n" {
		t.Fatalf("the folder's config.local after the apply: %q", got)
	}
}

// A relative core.excludesFile is read from the repo's work tree, as git
// reads it, not from AgentBus's own folder.
func TestRelativeExcludesFileIsTheRepos(t *testing.T) {
	src := t.TempDir()
	commitIn(t, src, map[string]string{"a.txt": "a1\n"})
	fixture(t, src, map[string]string{"local-ignore": "local-only.txt\n", "local-only.txt": "one\n"})
	gitDo(t, src, "config", "core.excludesFile", "local-ignore")
	gitDo(t, src, "check-ignore", "local-only.txt")
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"local-only.txt": "two\n", "a.txt": "a2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"a.txt"}) {
		t.Fatalf("changeset: %v, want [a.txt]: git ignores local-only.txt", got)
	}
}

// subRepo makes src a repo with lib (builtRepo) as its submodule sub, and
// answers src/sub.
func subRepo(t *testing.T) (string, string) {
	t.Helper()
	lib, src := t.TempDir(), t.TempDir()
	builtRepo(t, lib)
	commitIn(t, src, map[string]string{"a.txt": "a1\n"})
	gitDo(t, src, "submodule", "add", "-q", lib, "sub")
	gitDo(t, src, "commit", "-q", "-m", "sub")
	sub := filepath.Join(src, "sub")
	gitDo(t, sub, "ls-files", "--error-unmatch", "config.local", "dist/index.js")
	return src, sub
}

// A submodule opened as the folder: its .git file names its repo relative to
// where it sits in the person's folder, and that repo says what is tracked.
func TestSubmoduleOpenedAsTheFolder(t *testing.T) {
	_, sub := subRepo(t)
	b, s := open(t, Copy, sub)
	fixture(t, s.Dir, map[string]string{"config.local": "mode=2\n", "dist/index.js": "v2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"config.local", "dist/index.js"}) {
		t.Fatalf("changeset: %v", got)
	}
	// The person's side of an update reads the same repo, from their folder.
	fixture(t, sub, map[string]string{"dist/theirs.js": "t\n"})
	res, err := b.UpdateFrom(s, "", at(sub), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(s.Dir, "dist", "theirs.js")); got != "t\n" {
		t.Fatalf("the copy's dist/theirs.js after the update: %q", got)
	}
}

// A parent repo's info/exclude does not reach into a nested repo, as git
// status there does not read it: the nested repo's tracked file is the work,
// and so is a new file there that only the parent's rule would leave out.
func TestParentRulesStopAtANestedRepo(t *testing.T) {
	src := t.TempDir()
	commitIn(t, src, map[string]string{"a.txt": "a1\n"})
	commitIn(t, filepath.Join(src, "inner"), map[string]string{"config.local": "mode=1\n"})
	fixture(t, src, map[string]string{".git/info/exclude": "*.local\n"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"inner/config.local": "mode=2\n", "inner/new.local": "n\n", "top.local": "x\n", "a.txt": "a2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, want := changesetNames(t, s, "t1"), []string{"a.txt", "inner/config.local", "inner/new.local"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
	fixture(t, src, map[string]string{"inner/theirs.local": "t\n"})
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(s.Dir, "inner", "theirs.local")); got != "t\n" {
		t.Fatalf("the copy's inner/theirs.local after the update: %q", got)
	}
}

// A file the nested repo itself ignores but tracks (added with -f) is the
// work: the nested repo's index is read too.
func TestNestedForceAddedFileIsTheWork(t *testing.T) {
	src := t.TempDir()
	commitIn(t, src, map[string]string{"a.txt": "a1\n"})
	forceAdded(t, filepath.Join(src, "inner"), "config.local")
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"inner/config.local": "mode=2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"inner/config.local"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// A repo of SHA-256 objects: its index is read by a git of its own format,
// so a tracked file a rule matches is still the work. Before, the SHA-1
// private repo failed to read it and nothing said so.
func TestSHA256RepoIsRead(t *testing.T) {
	src := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--object-format=sha256", src).CombinedOutput(); err != nil {
		t.Skipf("no SHA-256 repos here: %s", out)
	}
	fixture(t, src, map[string]string{".gitignore": "*.local\n", "a.txt": "a1\n", "config.local": "mode=1\n"})
	gitDo(t, src, "add", "a.txt", ".gitignore")
	gitDo(t, src, "add", "-f", "config.local")
	gitDo(t, src, "commit", "-q", "-m", "c")
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"config.local": "mode=2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"config.local"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// A split index (git update-index --split-index) is read whole.
func TestSplitIndexIsRead(t *testing.T) {
	src := t.TempDir()
	builtRepo(t, src)
	gitDo(t, src, "update-index", "--split-index")
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"config.local": "mode=2\n", "dist/index.js": "v2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"config.local", "dist/index.js"}) {
		t.Fatalf("changeset: %v", got)
	}
}

// An index git cannot read fails the checkpoint: what the repo tracks is not
// known, and its files must not drop out as if ignored.
func TestUnreadableIndexFailsTheCheckpoint(t *testing.T) {
	src := t.TempDir()
	builtRepo(t, src)
	b, s := open(t, Copy, src)
	if err := os.WriteFile(filepath.Join(s.Dir, ".git", "index"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"config.local": "mode=2\n"})
	if _, err := b.Checkpoint(s, "t1"); err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("checkpoint over an unreadable index: %v", err)
	}
}

// Every built-in rule is a folder at the top ("/name/"): gitWork asks git to
// match the list against one tracked path per top-level name, which holds
// only for rules of that shape.
func TestBuiltinRulesAreTopFolders(t *testing.T) {
	for _, r := range gitBuiltinIgnore {
		name := strings.TrimSuffix(strings.TrimPrefix(r, "/"), "/")
		if !strings.HasPrefix(r, "/") || !strings.HasSuffix(r, "/") || name == "" || strings.ContainsAny(name, "/*?[!") {
			t.Errorf("built-in rule %q is not a top-level folder", r)
		}
	}
}

// Codex review t_154ec0738d6b2d0c, P1: a submodule whose folder the parent
// repo's rules ignore is still a repo of its own, and it decides what in it is
// the work. The walk used to leave the folder out before finding its .git.
func TestSubmoduleUnderAParentRule(t *testing.T) {
	src, _ := subRepo(t)
	fixture(t, src, map[string]string{".git/info/exclude": "sub/\n"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"sub/config.local": "mode=2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"sub/config.local"}) {
		t.Fatalf("changeset: %v", got)
	}
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(src, "sub", "config.local")); got != "mode=2\n" {
		t.Fatalf("the folder's sub/config.local after the apply: %q", got)
	}
	// The person's side of an update is walked the same way.
	fixture(t, src, map[string]string{"sub/dist/theirs.js": "t\n"})
	res, err := b.UpdateFrom(s, "t1", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(s.Dir, "sub", "dist", "theirs.js")); got != "t\n" {
		t.Fatalf("the copy's sub/dist/theirs.js after the update: %q", got)
	}
}

// The same for a nested repo that is no submodule, under a parent's
// .gitignore; a plain folder that rule takes stays out, and so does a repo
// inside a folder the rules leave out (node_modules/x/.git).
func TestNestedRepoUnderAParentRule(t *testing.T) {
	src := t.TempDir()
	commitIn(t, src, map[string]string{".gitignore": "vendor/\nscratch/\n", "a.txt": "a1\n"})
	commitIn(t, filepath.Join(src, "vendor"), map[string]string{"lib.go": "v1\n"})
	commitIn(t, filepath.Join(src, "node_modules", "x"), map[string]string{"index.js": "v1\n"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"vendor/lib.go": "v2\n", "scratch/n.txt": "n\n", "node_modules/x/index.js": "v2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"vendor/lib.go"}) {
		t.Fatalf("changeset: %v, want [vendor/lib.go]", got)
	}
}

// Codex review t_154ec0738d6b2d0c, P2: a .git the copy holds wins over the
// git folder kept at Open. A worktree opened as the folder, then a repo made
// in the copy with a file added past its rules: that file is the work.
func TestARepoMadeInTheCopyWins(t *testing.T) {
	main := t.TempDir()
	builtRepo(t, main)
	wt := filepath.Join(t.TempDir(), "wt")
	gitDo(t, main, "worktree", "add", "-q", wt)
	b, s := open(t, Copy, wt)
	gitDo(t, s.Dir, "init", "-q")
	fixture(t, s.Dir, map[string]string{"added.local": "a\n"})
	gitDo(t, s.Dir, "add", "-f", "added.local")
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	got := changesetNames(t, s, "t1")
	if !reflect.DeepEqual(got, []string{"added.local"}) {
		t.Fatalf("changeset: %v, want [added.local]", got)
	}
}
