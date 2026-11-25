package isolation

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// changeset is the files a git-backed copy's checkpoint holds that differ from
// its base, by path.
func changeset(t *testing.T, s *Space, name string) map[string]bool {
	t.Helper()
	cs, err := ChangesetOf(s, name)
	if err != nil {
		t.Fatalf("changeset %s: %v", name, err)
	}
	out := map[string]bool{}
	for _, f := range cs.Files {
		out[f.Path] = true
	}
	return out
}

// A copy whose files carry old modification times still records and restores
// them: git compares content, not time, so an aged file is no spurious change,
// and a restore brings its content back.
func TestShadowAgedFileRestores(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "keep.txt": "k"})
	age(t, src)
	b, s := open(t, Copy, src)
	if !s.hasShadow() {
		t.Fatal("the copy made no shadow repo")
	}
	// A turn only touches a.txt's time, not its content: no new checkpoint, so
	// Checkpoint answers the latest (Base), as the contract says of no change.
	age(t, s.Dir)
	c, err := b.Checkpoint(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != Base {
		t.Fatalf("an aged file made checkpoint %q, want the latest (%s)", c.Name, Base)
	}
	// A turn edits a.txt; a restore to base brings the aged content back.
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s.Dir, "a.txt")) != "a1" {
		t.Fatal("the aged file did not restore")
	}
}

// A nested git repo in the copy never breaks a checkpoint: an unborn one (a
// turn ran `git init` and did not commit) has its files recorded like any
// folder's, and a restore removes the files one a dropped turn added, never
// its .git.
func TestShadowNestedRepos(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	initRepo := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(git, "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v %s", dir, err, out)
		}
	}
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)

	// An unborn nested repo (no commit) must not break the checkpoint.
	initRepo(filepath.Join(s.Dir, "unborn"))
	fixture(t, s.Dir, map[string]string{"unborn/g.txt": "g", "a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatalf("a checkpoint over an unborn nested repo failed: %v", err)
	}
	// The outer change is still recorded.
	if cs := changeset(t, s, "t1"); !cs["a.txt"] {
		t.Fatalf("the outer change was lost: %v", cs)
	}

	// A nested repo a dropped turn added is removed by a restore to t1.
	initRepo(filepath.Join(s.Dir, "dropped"))
	fixture(t, s.Dir, map[string]string{"dropped/h.txt": "h", "a.txt": "a3"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// The dropped turn's files in it go; the repo's own .git stays, as a
	// restore of a copy made since 0.6.6 never removes a .git (nested.go).
	if _, err := os.Lstat(filepath.Join(s.Dir, "dropped", "h.txt")); !os.IsNotExist(err) {
		t.Fatal("a restore left a file of a nested repo a dropped turn added")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "dropped", ".git")); err != nil {
		t.Fatal("a restore removed a nested repo's .git")
	}
	if read(t, filepath.Join(s.Dir, "a.txt")) != "a2" {
		t.Fatal("a.txt did not restore to t1")
	}
}

// A turn can make a folder read-only (a module cache, say); a restore still
// completes, because it makes the tree writable before cleaning. Reverting
// makeTreeWritable makes the clean stop after the reset, half restored.
func TestShadowReadOnlyDirRestores(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	// t1 is the applied point: a real change, so it is its own checkpoint.
	fixture(t, s.Dir, map[string]string{"a.txt": "a1b"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// A turn writes a file in a new folder and makes the folder read-only.
	fixture(t, s.Dir, map[string]string{"cache/x": "y", "a.txt": "a2"})
	if err := os.Chmod(filepath.Join(s.Dir, "cache"), 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatalf("a restore past a read-only folder failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "cache")); !os.IsNotExist(err) {
		t.Fatal("the read-only folder was not cleaned")
	}
	if read(t, filepath.Join(s.Dir, "a.txt")) != "a1b" {
		t.Fatal("a.txt did not restore to t1")
	}
}

// A copy whose .gitattributes would normalise content (CRLF, filters) is never
// transformed: the stored bytes are the real bytes, so the changeset hash is
// the content's own SHA-256 and a restore returns the exact bytes. Reverting
// the info/attributes seal corrupts both.
func TestShadowGitattributesByteExact(t *testing.T) {
	src := t.TempDir()
	content := "line1\nline2\n" // LF, which `eol=crlf` would turn to CRLF
	fixture(t, src, map[string]string{
		".gitattributes": "* text=auto eol=crlf\n",
		"f.txt":          content,
	})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"f.txt": content + "line3\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	want := content + "line3\n"
	sum := sha256.Sum256([]byte(want))
	wantHash := hex.EncodeToString(sum[:])
	var got string
	for _, f := range cs.Files {
		if f.Path == "f.txt" && f.Now != nil {
			got = f.Now.Hash
		}
	}
	if got != wantHash {
		t.Fatalf("changeset hash %q, want the real LF content's %q", got, wantHash)
	}
	// A restore to base returns the exact original bytes, not CRLF.
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(s.Dir, "f.txt")) != content {
		t.Fatal("the restored file was not byte-exact")
	}
}

// The built-in ignores are anchored to the copy's top, so a root build folder
// is left out but real source under a nested build folder is tracked. Reverting
// the leading slash (unanchoring) would drop the nested source too.
func TestShadowAnchoredIgnore(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{
		"a.txt":          "a2",          // a sure change, so t1 is its own checkpoint
		"build/out.o":    "object",      // root build: ignored
		"src/build/s.go": "package src", // nested build: real source, tracked
	})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	cs := changeset(t, s, "t1")
	if cs["build/out.o"] {
		t.Fatal("a root build folder was tracked")
	}
	if !cs["src/build/s.go"] {
		t.Fatal("nested source under a build folder was ignored")
	}
}

// After a restore, the dropped turns' checkpoint refs are pruned, so they no
// longer resolve and a later checkpoint of the same name records its edits
// instead of silently finding the stale ref and dropping them. Reverting
// pruneUnreachableRefs makes the re-checkpoint return the latest and lose a3.
func TestShadowDiscardPrunesStaleRefs(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	// The discarded checkpoint no longer resolves anywhere.
	if _, err := ChangesetOf(s, "t1"); err != ErrNoCheckpoint {
		t.Fatalf("changeset of a discarded checkpoint: %v, want ErrNoCheckpoint", err)
	}
	if err := b.RestoreTo(s, "t1"); err != ErrNoCheckpoint {
		t.Fatalf("restore to a discarded checkpoint: %v, want ErrNoCheckpoint", err)
	}
	// A later turn reuses the name: its edits are recorded, not dropped.
	fixture(t, s.Dir, map[string]string{"a.txt": "a3"})
	c, err := b.Checkpoint(s, "t1")
	if err != nil || c.Name != "t1" {
		t.Fatalf("re-checkpoint: %+v %v", c, err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Files) != 1 || cs.Files[0].Path != "a.txt" || cs.Files[0].Now == nil {
		t.Fatalf("the re-checkpoint dropped the edit: %+v", cs.Files)
	}
}

// A file a turn leaves 0600 (a key, a .env) keeps its exact permission bits in
// the changeset, so apply never widens it to 0644. Reverting the workingMode
// lookup in shadowFileSide reports git's normalised 0644 and widens it.
func TestShadowPreservesRestrictiveMode(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"secret.env": "token"})
	if err := os.Chmod(filepath.Join(s.Dir, "secret.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var mode os.FileMode = 0
	for _, f := range cs.Files {
		if f.Path == "secret.env" && f.Now != nil {
			mode = f.Now.Mode.Perm()
		}
	}
	if mode != 0o600 {
		t.Fatalf("secret.env mode %o in the changeset, want 0600 (not widened)", mode)
	}
}

// A copy whose .gitattributes would expand $Id$ keywords (ident) is never
// transformed: a restore returns the exact bytes, not an expanded $Id: <oid> $.
// Reverting the -ident seal corrupts the restored file.
func TestShadowIdentAttrByteExact(t *testing.T) {
	src := t.TempDir()
	body := "head\n$Id$\ntail\n"
	fixture(t, src, map[string]string{".gitattributes": "* ident\n", "f.txt": body})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"f.txt": body + "edit\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(s.Dir, "f.txt")); got != body {
		t.Fatalf("the restored file was keyword-expanded: %q", got)
	}
}

// A workspace with a top-level file named HEAD (or head on a case-insensitive
// disk, or a refs/checkpoints path) does not confuse git: every op names its
// revisions with a trailing -- so git never reads the name as a filename.
// Reverting the -- separators makes git exit "ambiguous argument 'HEAD'".
func TestShadowHeadNamedPath(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"HEAD": "not a ref", "a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatalf("checkpoint with a top-level HEAD file: %v", err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Files) != 1 || cs.Files[0].Path != "a.txt" {
		t.Fatalf("changeset with a HEAD file: %+v", cs.Files)
	}
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatalf("restore with a top-level HEAD file: %v", err)
	}
	if read(t, filepath.Join(s.Dir, "a.txt")) != "a1" {
		t.Fatal("restore did not revert a.txt")
	}
}

// A checkpoint name that passes the name rule but git's refs reject (a .lock
// suffix, a .. run, a trailing dot) is refused up front with ErrBadName, so no
// commit is ever made that a ref cannot then name; a later valid checkpoint
// records its edits. Reverting gitRefSafe returns a generic error instead.
func TestShadowRejectsGitInvalidName(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	for _, bad := range []string{"t1.lock", "a..b", "t1."} {
		if _, err := b.Checkpoint(s, bad); err != ErrBadName {
			t.Fatalf("checkpoint %q: %v, want ErrBadName", bad, err)
		}
	}
	c, err := b.Checkpoint(s, "t2")
	if err != nil || c.Name != "t2" {
		t.Fatalf("checkpoint after rejected names: %+v %v", c, err)
	}
	cs, err := ChangesetOf(s, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Files) != 1 || cs.Files[0].Path != "a.txt" {
		t.Fatalf("a rejected name dropped the edit: %+v", cs.Files)
	}
}

// A historical checkpoint keeps its own executable bit even after a later turn
// changed it: the exec bit comes from git, which stores it, so a historical checkpoint keeps
// its own exec bit after a later turn changed it.
func TestShadowExecBitAcrossCheckpoints(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"script.sh": "#!/bin/sh\necho hi\n"})
	b, s := open(t, Copy, src)
	// t1 makes it executable (same bytes); t2 makes it non-executable again.
	if err := os.Chmod(filepath.Join(s.Dir, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(s.Dir, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var mode os.FileMode
	for _, f := range cs.Files {
		if f.Path == "script.sh" && f.Now != nil {
			mode = f.Now.Mode
		}
	}
	if mode.Perm()&0o100 == 0 {
		t.Fatalf("t1's executable bit was erased from its changeset: %o", mode.Perm())
	}
}

// A nested build/ folder (tracked, not a top-level ignore) that a dropped turn
// made read-only is still reset: makeTreeWritable only spares a top-level
// ignore, so it makes the nested folder writable and the clean completes.
// Reverting the top-level anchor makes the clean stop on the read-only folder.
func TestShadowNestedReadOnlyBuildDirRestores(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a1b"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"src/build/out": "o", "a.txt": "a2"})
	if err := os.Chmod(filepath.Join(s.Dir, "src", "build"), 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatalf("restore past a nested read-only build dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "src")); !os.IsNotExist(err) {
		t.Fatal("the nested build folder was not cleaned")
	}
	if read(t, filepath.Join(s.Dir, "a.txt")) != "a1b" {
		t.Fatal("a.txt did not restore")
	}
}

// A restrictive mode survives a discard and an unrelated later turn: the mode
// record is re-applied on the copy after a reset, and a changeset reads the
// recorded mode, so an unrelated turn's apply never widens a 0600 secret.
// Reverting the re-apply in restoreShadow leaves 0644 on disk and in t3.
func TestShadowModeSurvivesDiscard(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"secret.env": "token"})
	if err := os.Chmod(filepath.Join(s.Dir, "secret.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"secret.env": "token2"})
	if err := os.Chmod(filepath.Join(s.Dir, "secret.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(s.Dir, "secret.env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("restore left secret.env mode %v, want 0600", fi.Mode().Perm())
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "a3"})
	if _, err := b.Checkpoint(s, "t3"); err != nil {
		t.Fatal(err)
	}
	cs, err := ChangesetOf(s, "t3")
	if err != nil {
		t.Fatal(err)
	}
	var mode os.FileMode
	for _, f := range cs.Files {
		if f.Path == "secret.env" && f.Now != nil {
			mode = f.Now.Mode.Perm()
		}
	}
	if mode != 0o600 {
		t.Fatalf("secret.env mode %o after a discard and an unrelated turn, want 0600", mode)
	}
}

// A permission-only change git cannot see (0600 -> 0644) appears in BOTH the
// diff and the changeset, so apply and preview agree on it. Reverting the
// modeOnlyChanges fold in gitDiff hides it from the diff.
func TestShadowModeOnlyChangeIsConsistent(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "secret.env": "v1"})
	if err := os.Chmod(filepath.Join(src, "secret.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"secret.env": "v2"})
	if err := os.Chmod(filepath.Join(s.Dir, "secret.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// t2 widens secret.env (no byte change) and edits a.txt.
	if err := os.Chmod(filepath.Join(s.Dir, "secret.env"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	c, err := b.Diff(s, "t1", "t2")
	if err != nil {
		t.Fatal(err)
	}
	inDiff := false
	for _, f := range c.Files {
		if f.Path == "secret.env" {
			inDiff = true
		}
	}
	if !inDiff {
		t.Fatal("Diff(t1,t2) hid the permission-only change to secret.env")
	}
	cs, err := ChangesetOf(s, "t2")
	if err != nil {
		t.Fatal(err)
	}
	var mode os.FileMode
	for _, f := range cs.Files {
		if f.Path == "secret.env" && f.Now != nil {
			mode = f.Now.Mode.Perm()
		}
	}
	if mode != 0o644 {
		t.Fatalf("ChangesetOf(t2) secret.env mode %o, want 0644", mode)
	}
}

// A discard returns a file to its target mode even when git reset cannot: a
// 0400 a dropped turn set with no byte change (git sees no change) is put back
// to 0644. Reverting reconcileModes leaves a.txt read-only.
func TestShadowReconcileRestoresMode(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "b.txt": "b1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// t2 makes a.txt read-only (no byte change) and edits b.txt.
	if err := os.Chmod(filepath.Join(s.Dir, "a.txt"), 0o400); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"b.txt": "b2"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(s.Dir, "a.txt")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("reconcile left a.txt %v, want 0644", fi.Mode().Perm())
	}
}

// A restrictive process umask does not corrupt a discard: git reset recreates a
// file 0600 under umask 0077, and the reconcile puts it back to 0644. Reverting
// reconcileModes leaves it 0600.
func TestShadowReconcileUnderUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "b.txt": "b1"})
	if err := os.Chmod(filepath.Join(src, "a.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	_ = os.Chmod(filepath.Join(s.Dir, "a.txt"), 0o644)
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "a3"})
	_ = os.Chmod(filepath.Join(s.Dir, "a.txt"), 0o644)
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(s.Dir, "a.txt")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("a discard under umask 0077 left a.txt %v, want 0644", fi.Mode().Perm())
	}
}

// A turn whose ONLY change is a permission bit git cannot see (0644 -> 0400) is
// still its own checkpoint and still shows up in the diff and changeset, so the
// chmod is applied. Reverting the mode-map comparison in commitShadow drops the
// turn (git's tree is unchanged) and loses the chmod.
func TestShadowPureChmodTurn(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// t2 changes nothing but a.txt's permission bits.
	if err := os.Chmod(filepath.Join(s.Dir, "a.txt"), 0o400); err != nil {
		t.Fatal(err)
	}
	c2, err := b.Checkpoint(s, "t2")
	if err != nil || c2.Name != "t2" {
		t.Fatalf("a permission-only turn was dropped: %+v %v", c2, err)
	}
	cd, err := b.Diff(s, "t1", "t2")
	if err != nil {
		t.Fatal(err)
	}
	inDiff := false
	for _, f := range cd.Files {
		if f.Path == "a.txt" {
			inDiff = true
		}
	}
	if !inDiff {
		t.Fatal("Diff(t1,t2) missed the permission-only change")
	}
	cs, err := ChangesetOf(s, "t2")
	if err != nil {
		t.Fatal(err)
	}
	var mode os.FileMode
	for _, f := range cs.Files {
		if f.Path == "a.txt" && f.Now != nil {
			mode = f.Now.Mode.Perm()
		}
	}
	if mode != 0o400 {
		t.Fatalf("ChangesetOf(t2) a.txt mode %o, want 0400", mode)
	}
}

// A top-level build/ that the folder's .gitignore un-ignores is tracked by the
// shadow, so a reset must clean it; makeTreeWritable walks the whole copy (it
// cannot tell tracked from ignored by a folder's name) and the read-only one is
// made writable. Reverting makeTreeWritable to a no-op leaves it half reset.
func TestShadowUnignoredBuildDirRestores(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", ".gitignore": "!build/\n"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, s.Dir, map[string]string{"build/out": "o", "a.txt": "a3"})
	if err := os.Chmod(filepath.Join(s.Dir, "build"), 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	// The un-ignored build/ was tracked, or there is nothing to test.
	if cs := changeset(t, s, "t2"); !cs["build/out"] {
		t.Skip("build/ was not un-ignored on this git; the walk is still correct")
	}
	if err := b.RestoreTo(s, "t1"); err != nil {
		t.Fatalf("restore past an un-ignored read-only build dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "build")); !os.IsNotExist(err) {
		t.Fatal("the un-ignored build dir was not cleaned")
	}
}

// A copy with no shadow repo (built from a snapshot, or made before this)
// refuses a restore rather than resetting it a way that could half-wipe it.
func TestNoShadowRefusesRestore(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1"})
	b, s := open(t, Copy, src)
	// Stand in for a legacy or snapshot copy: no shadow repo.
	s.Shadow = false
	if err := b.RestoreTo(s, Base); err != ErrNoRestore {
		t.Fatalf("restore of a copy with no shadow: %v, want ErrNoRestore", err)
	}
}
