package isolation

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// caseBlind skips a test where dir tells names apart by case.
func caseBlind(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "probe")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	if _, err := os.Lstat(filepath.Join(dir, "PROBE")); err != nil {
		t.Skip("this disk tells case apart")
	}
}

// onDisk are the files under dir as the disk spells them, .git left out.
func onDisk(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// A case-only rename on a disk that does not tell case apart (#76): a file
// and a folder renamed by case alone reach the changeset, and an apply
// renames them in the folder. Before, the record kept the old names, the
// changeset was empty, and Apply said applied with the folder unchanged.
func TestCaseOnlyRenameApplies(t *testing.T) {
	src := t.TempDir()
	caseBlind(t, src)
	fixture(t, src, map[string]string{"button.tsx": "b\n", "comp/a": "a\n", "keep.txt": "k\n"})
	b, s := open(t, Copy, src)
	for _, mv := range [][2]string{{"button.tsx", "Button.tsx"}, {"comp", "Comp"}} {
		tmp := filepath.Join(s.Dir, "tmp-rename")
		if err := os.Rename(filepath.Join(s.Dir, mv[0]), tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(s.Dir, mv[1])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"Button.tsx", "Comp/a", "button.tsx", "comp/a"}
	if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset: %v, want %v", got, want)
	}
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, want := onDisk(t, src), []string{"Button.tsx", "Comp/a", "keep.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the folder after the apply: %v, want %v", got, want)
	}
	if got := read(t, filepath.Join(src, "Button.tsx")) + read(t, filepath.Join(src, "Comp", "a")); got != "b\na\n" {
		t.Fatalf("contents after the apply: %q", got)
	}
	// A discard puts the old names back, and loses no file.
	if err := b.RestoreTo(s, Base); err != nil {
		t.Fatal(err)
	}
	if got, want := onDisk(t, s.Dir), []string{"button.tsx", "comp/a", "keep.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy after a discard: %v, want %v", got, want)
	}
}

// An update brings the person's case-only rename into the copy.
func TestUpdateBringsACaseOnlyRename(t *testing.T) {
	src := t.TempDir()
	caseBlind(t, src)
	fixture(t, src, map[string]string{"button.tsx": "b\n", "other.txt": "o\n"})
	b, s := open(t, Copy, src)
	fixture(t, s.Dir, map[string]string{"other.txt": "o2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(src, "button.tsx"), filepath.Join(src, "tmp-rename"))
	os.Rename(filepath.Join(src, "tmp-rename"), filepath.Join(src, "Button.tsx"))
	res, err := b.UpdateFrom(s, "", at(src), false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := strings.Join(onDisk(t, s.Dir), " "); got != "Button.tsx other.txt" {
		t.Fatalf("the copy after the update: %s", got)
	}
	// The folder's side records the rename: the old name gone, not both held.
	if got, want := changesetNames(t, s, res.Theirs), []string{"Button.tsx", "button.tsx"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the folder's side: %v, want %v", got, want)
	}
}

// Codex review t_631a1bb6f9d48cda: a hard link under another name is the same
// file, never another spelling of the name. A force-added config.local, a
// hard link to it the rules ignore (Archive.local), then a case-only rename
// to Config.local: the rename applies, and the link stands in for nothing.
func TestCaseRenameBesideAHardLink(t *testing.T) {
	src := t.TempDir()
	caseBlind(t, src)
	forceAdded(t, src, "config.local")
	b, s := open(t, Copy, src)
	if err := os.Link(filepath.Join(s.Dir, "config.local"), filepath.Join(s.Dir, "Archive.local")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.Dir, "config.local"), filepath.Join(s.Dir, "temporary")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.Dir, "temporary"), filepath.Join(s.Dir, "Config.local")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, want := changesetNames(t, s, "t1"), []string{"Config.local", "config.local"}; !reflect.DeepEqual(got, want) {
		t.Errorf("changeset: %v, want %v", got, want)
	}
	if _, err := applyIn(t, s, src, Base, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(src, "Config.local")); got != "mode=1\n" {
		t.Errorf("Config.local after the apply: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(src, "Archive.local")); err == nil {
		t.Error("the ignored hard link reached the folder")
	}
}
