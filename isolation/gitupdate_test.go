package isolation

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// diffNames are the paths that changed between two checkpoints of a git-backed
// copy, sorted.
func diffNames(t *testing.T, b Backend, s *Space, from, to string) []string {
	t.Helper()
	c, err := b.Diff(s, from, to)
	if err != nil {
		t.Fatalf("diff %s..%s: %v", from, to, err)
	}
	var out []string
	for _, ch := range c.Files {
		out = append(out, ch.Path)
	}
	sort.Strings(out)
	return out
}

func changesetNames(t *testing.T, s *Space, name string) []string {
	t.Helper()
	m := changeset(t, s, name)
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// A clean update brings the folder's changes into the copy next to the
// session's own, records theirs and the merge as checkpoints, and -- the point
// of the feature -- leaves theirs..merged holding only the session's work, so a
// later apply writes just that against the folder as it is now.
func TestUpdateCleanMerge(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "line1\nline2\nline3\n", "keep.txt": "k\n"})
	b, s := open(t, Copy, src)
	if !s.hasShadow() {
		t.Skip("the copy made no shadow repo on this disk")
	}

	// The session's work in the copy: change line1 of a.txt, add ours.txt.
	fixture(t, s.Dir, map[string]string{"a.txt": "OURS\nline2\nline3\n", "ours.txt": "mine\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}

	// The folder moved on (another session applied, or the person edited):
	// change line3 of a.txt, add theirs.txt, and an ignored dependency folder.
	fixture(t, src, map[string]string{"a.txt": "line1\nline2\nTHEIRS\n", "theirs.txt": "yours\n", "node_modules/pkg.txt": "junk\n"})

	res, err := b.UpdateFrom(s, "", src)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Clean {
		t.Fatalf("update was not clean: conflicts %v", res.Conflicts)
	}
	if res.Theirs == "" || res.Merged == "" {
		t.Fatalf("update recorded no checkpoints: %+v", res)
	}

	// The copy now holds both sides' work and the ignored folder is NOT merged.
	if got := read(t, filepath.Join(s.Dir, "a.txt")); got != "OURS\nline2\nTHEIRS\n" {
		t.Fatalf("a.txt did not three-way merge: %q", got)
	}
	if read(t, filepath.Join(s.Dir, "ours.txt")) != "mine\n" {
		t.Fatal("the session's own new file was lost by the update")
	}
	if read(t, filepath.Join(s.Dir, "theirs.txt")) != "yours\n" {
		t.Fatal("the folder's new file did not reach the copy")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "node_modules", "pkg.txt")); !os.IsNotExist(err) {
		t.Fatal("an ignored dependency folder was merged into the copy")
	}

	// The baseline the folder matches (theirs) carries the folder's changes;
	// theirs..merged is exactly the session's own work.
	if got, want := changesetNames(t, s, res.Theirs), []string{"a.txt", "theirs.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("theirs changeset = %v, want %v", got, want)
	}
	if got, want := diffNames(t, b, s, res.Theirs, res.Merged), []string{"a.txt", "ours.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("theirs..merged = %v, want the session's work %v", got, want)
	}
}

// A real conflict (the same lines changed on both sides) changes nothing in the
// copy and names the files, so the person can ask the agent to resolve it.
func TestUpdateConflictChangesNothing(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "line1\nline2\n", "keep.txt": "k\n"})
	b, s := open(t, Copy, src)
	if !s.hasShadow() {
		t.Skip("the copy made no shadow repo on this disk")
	}

	fixture(t, s.Dir, map[string]string{"a.txt": "OURS\nline2\n", "ours.txt": "mine\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// The folder changed the same line a different way.
	fixture(t, src, map[string]string{"a.txt": "THEIRS\nline2\n"})

	res, err := b.UpdateFrom(s, "", src)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Clean {
		t.Fatal("a conflicting update reported clean")
	}
	if !reflect.DeepEqual(res.Conflicts, []string{"a.txt"}) {
		t.Fatalf("conflicts = %v, want [a.txt]", res.Conflicts)
	}
	if res.Merged != "" || res.Theirs != "" {
		t.Fatalf("a conflict recorded checkpoints: %+v", res)
	}
	// The copy is exactly as it was: the session's work intact, no theirs file.
	if got := read(t, filepath.Join(s.Dir, "a.txt")); got != "OURS\nline2\n" {
		t.Fatalf("the copy changed on a conflict: a.txt = %q", got)
	}
	if read(t, filepath.Join(s.Dir, "ours.txt")) != "mine\n" {
		t.Fatal("the session's work was disturbed by a conflicting update")
	}
	// Only Base and t1 remain: no update checkpoints were recorded.
	for _, n := range names(t, b, s) {
		if n != Base && n != "t1" {
			t.Fatalf("a conflict left checkpoint %q", n)
		}
	}
}

// Regression (Codex P2.5): an update must not revert a permission bit the folder
// changed and the session did not. Base keep.txt is 0600; the person relaxes the
// folder's copy to 0644; a clean update keeps 0644, not the session side's 0600.
func TestUpdateKeepsFolderOnlyModeChange(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "one\n", "keep.txt": "k\n"})
	if err := os.Chmod(filepath.Join(src, "keep.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, s := open(t, Copy, src)
	if !s.hasShadow() {
		t.Skip("the copy made no shadow repo on this disk")
	}
	// The session edits a.txt (its own work), not keep.txt's mode.
	fixture(t, s.Dir, map[string]string{"a.txt": "one\nwork\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// The person relaxes keep.txt in the folder to 0644.
	if err := os.Chmod(filepath.Join(src, "keep.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := b.UpdateFrom(s, "", src)
	if err != nil || !res.Clean {
		t.Fatalf("update: clean=%v conflicts=%v err=%v", res.Clean, res.Conflicts, err)
	}
	fi, err := os.Lstat(filepath.Join(s.Dir, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("keep.txt mode after update = %o, want 0644 (the folder's change kept, not reverted to 0600)", fi.Mode().Perm())
	}
}

// Regression (Codex P1.3): a clean update reports the pre-update head, and
// restoring to it undoes the merge, so a caller whose publish fails can leave the
// copy matching control's unchanged view.
func TestUpdatePriorHeadUndoesTheMerge(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "one\n"})
	b, s := open(t, Copy, src)
	if !s.hasShadow() {
		t.Skip("the copy made no shadow repo on this disk")
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "one\nwork\n", "ours.txt": "mine\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, src, map[string]string{"theirs.txt": "yours\n"})
	res, err := b.UpdateFrom(s, "", src)
	if err != nil || !res.Clean {
		t.Fatalf("update: clean=%v err=%v", res.Clean, err)
	}
	if res.PriorHead != "t1" {
		t.Fatalf("prior head = %q, want t1", res.PriorHead)
	}
	// The merge brought theirs.txt into the copy.
	if _, err := os.Stat(filepath.Join(s.Dir, "theirs.txt")); err != nil {
		t.Fatal("the merge did not bring theirs.txt into the copy")
	}
	// Undo: restore to the pre-update head drops the merge, keeps the session's work.
	if err := b.RestoreTo(s, res.PriorHead); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "theirs.txt")); !os.IsNotExist(err) {
		t.Fatal("restoring the prior head did not undo the merge (theirs.txt remains)")
	}
	if read(t, filepath.Join(s.Dir, "ours.txt")) != "mine\n" || read(t, filepath.Join(s.Dir, "a.txt")) != "one\nwork\n" {
		t.Fatal("restoring the prior head disturbed the session's own work")
	}
}

// Regression (Codex r2 finding 3): a file the base tracked that the folder later
// adds to .gitignore -- while keeping the file -- must not vanish from the copy.
// theirs is seeded from the merge-base tree, so git keeps the still-present
// tracked file rather than treating a new ignore rule as a deletion.
func TestUpdateKeepsNewlyIgnoredTrackedFile(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "one\n", "keep.txt": "k\n"})
	b, s := open(t, Copy, src)
	if !s.hasShadow() {
		t.Skip("the copy made no shadow repo on this disk")
	}
	fixture(t, s.Dir, map[string]string{"a.txt": "one\nwork\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// The person adds keep.txt to .gitignore in the folder but keeps the file.
	fixture(t, src, map[string]string{".gitignore": "keep.txt\n"})
	res, err := b.UpdateFrom(s, "", src)
	if err != nil || !res.Clean {
		t.Fatalf("update: clean=%v conflicts=%v err=%v", res.Clean, res.Conflicts, err)
	}
	if read(t, filepath.Join(s.Dir, "keep.txt")) != "k\n" {
		t.Fatalf("keep.txt was removed from the copy by a new ignore rule, want it kept")
	}
}
