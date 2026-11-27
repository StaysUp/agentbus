package isolation

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
)

// gitDo runs git in dir, failing the test on an error.
func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "-c", "protocol.file.allow=always"}
	if out, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v %s", args, dir, err, out)
	}
}

// A kind of folder a person keeps, and what a turn in it, and the person
// beside it, change: the paths that must reach the other side (in) and the
// ones the built-in list or the folder's own rules leave out (out).
type kind struct {
	name string
	make func(t *testing.T, dir string)
	// turn's writes and the person's, relative to the folder's top.
	turn, person map[string]string
	in, out      []string // of turn's writes; of person's, theirIn, theirOut
	theirIn      []string
	theirOut     []string
}

func kinds() []kind {
	return []kind{
		{
			// (a) no git at all: the built-in list holds, dist/ and
			// node_modules/ stay out.
			name: "no git",
			make: func(t *testing.T, dir string) {
				fixture(t, dir, map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "dist/index.js": "d1\n"})
			},
			turn:     map[string]string{"a.txt": "a2\n", "src/new.go": "n\n", "dist/index.js": "d2\n", "node_modules/y.js": "y\n"},
			in:       []string{"a.txt", "src/new.go"},
			out:      []string{"node_modules/y.js"},
			person:   map[string]string{"b.txt": "b2\n", "theirs.txt": "t\n", "node_modules/t.js": "t\n", "dist/t.js": "t\n"},
			theirIn:  []string{"b.txt", "theirs.txt"},
			theirOut: []string{"node_modules/t.js", "dist/t.js"},
		},
		{
			// (b) a repo with a submodule, and a committed Dist/: the
			// submodule's files are the work, a new file beside a tracked one
			// in Dist/ is too, and a node_modules/ the repo does not ignore
			// stays out.
			name: "submodule",
			make: func(t *testing.T, dir string) {
				lib := t.TempDir()
				fixture(t, lib, map[string]string{"lib.go": "l1\n", "lib2.go": "m1\n"})
				gitDo(t, lib, "init", "-q")
				gitDo(t, lib, "add", "-A")
				gitDo(t, lib, "commit", "-q", "-m", "lib")
				fixture(t, dir, map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "Dist/index.js": "d1\n"})
				gitDo(t, dir, "init", "-q")
				gitDo(t, dir, "submodule", "add", "-q", lib, "sub")
				gitDo(t, dir, "add", "-A")
				gitDo(t, dir, "commit", "-q", "-m", "c")
			},
			turn:     map[string]string{"a.txt": "a2\n", "sub/lib.go": "l2\n", "Dist/chunk.js": "c\n", "node_modules/y.js": "y\n"},
			in:       []string{"a.txt", "sub/lib.go", "Dist/chunk.js"},
			out:      []string{"node_modules/y.js"},
			person:   map[string]string{"b.txt": "b2\n", "sub/lib2.go": "m2\n", "Dist/theirs.js": "t\n", "node_modules/t.js": "t\n"},
			theirIn:  []string{"b.txt", "sub/lib2.go", "Dist/theirs.js"},
			theirOut: []string{"node_modules/t.js"},
		},
		{
			// (c) a repo holding a nested repo that is no submodule: the
			// nested repo's files are the work, a dist/ inside it is not the
			// top's (the list holds at the top only).
			name: "nested repo",
			make: func(t *testing.T, dir string) {
				fixture(t, dir, map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "inner/code": "c1\n", "inner/more": "m1\n"})
				gitDo(t, filepath.Join(dir, "inner"), "init", "-q")
				gitDo(t, filepath.Join(dir, "inner"), "add", "-A")
				gitDo(t, filepath.Join(dir, "inner"), "commit", "-q", "-m", "inner")
				gitDo(t, dir, "init", "-q")
				gitDo(t, dir, "add", "a.txt", "b.txt")
				gitDo(t, dir, "commit", "-q", "-m", "c")
			},
			turn:     map[string]string{"a.txt": "a2\n", "inner/code": "c2\n", "inner/dist/n.js": "n\n", "node_modules/y.js": "y\n"},
			in:       []string{"a.txt", "inner/code", "inner/dist/n.js"},
			out:      []string{"node_modules/y.js"},
			person:   map[string]string{"b.txt": "b2\n", "inner/more": "m2\n", "node_modules/t.js": "t\n"},
			theirIn:  []string{"b.txt", "inner/more"},
			theirOut: []string{"node_modules/t.js"},
		},
	}
}

// checkApply runs k's turn in the copy under prefix, applies it, and checks
// the folder dir.
func missing(t *testing.T, p string) bool {
	_, err := os.Lstat(p)
	return os.IsNotExist(err)
}

func turnAndApply(t *testing.T, b Backend, s *Space, ks []kind, prefixes []string, dirs []string) {
	t.Helper()
	for i, k := range ks {
		for p, c := range k.turn {
			fixture(t, s.Dir, map[string]string{path.Join(prefixes[i], p): c})
		}
	}
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	if len(ks) == 1 {
		if _, err := applyIn(t, s, dirs[0], Base, "t1"); err != nil {
			t.Fatal(err)
		}
	} else {
		applyPlacesIn(t, s, dirs, Base, "t1")
	}
	for i, k := range ks {
		for _, p := range k.in {
			if got := read(t, filepath.Join(dirs[i], filepath.FromSlash(p))); got != k.turn[p] {
				t.Errorf("%s: the folder's %s after the apply: %q, want %q", k.name, p, got, k.turn[p])
			}
		}
		for _, p := range k.out {
			if !missing(t, filepath.Join(dirs[i], filepath.FromSlash(p))) {
				t.Errorf("%s: the apply wrote %s, which is left out", k.name, p)
			}
		}
	}
}

func personAndUpdate(t *testing.T, b Backend, s *Space, ks []kind, prefixes []string, dirs []string) {
	t.Helper()
	fixture(t, s.Dir, map[string]string{path.Join(prefixes[0], "a.txt"): "a2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for i, k := range ks {
		fixture(t, dirs[i], k.person)
		refs[s.refOf(i)] = dirs[i]
	}
	res, err := b.UpdateFrom(s, "", func(ref string) (string, bool) { p, ok := refs[ref]; return p, ok }, false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	for i, k := range ks {
		for _, p := range k.theirIn {
			if got := read(t, filepath.Join(s.Dir, filepath.FromSlash(path.Join(prefixes[i], p)))); got != k.person[p] {
				t.Errorf("%s: the copy's %s after the update: %q, want %q", k.name, p, got, k.person[p])
			}
		}
		for _, p := range k.theirOut {
			if !missing(t, filepath.Join(s.Dir, filepath.FromSlash(path.Join(prefixes[i], p)))) {
				t.Errorf("%s: the update brought in %s, which is left out", k.name, p)
			}
		}
	}
}

// refOf is the reference of the space's folder i: its own for one folder.
func (s *Space) refOf(i int) string {
	if len(s.Folders) == 0 {
		return s.Ref
	}
	return s.Folders[i].Ref
}

// (a), (b), (c): each kind of folder alone, on Apply and on Update.
func TestKindsOfFolder(t *testing.T) {
	for _, k := range kinds() {
		for _, side := range []string{"apply", "update"} {
			t.Run(k.name+"/"+side, func(t *testing.T) {
				if _, err := exec.LookPath("git"); err != nil {
					t.Skip("no git on PATH")
				}
				src := t.TempDir()
				k.make(t, src)
				b, s := open(t, Copy, src)
				if side == "apply" {
					turnAndApply(t, b, s, []kind{k}, []string{""}, []string{src})
				} else {
					personAndUpdate(t, b, s, []kind{k}, []string{""}, []string{src})
				}
			})
		}
	}
}

// (d): one workspace of several folders, one of each kind, on Apply and on
// Update: each folder's rules hold for it alone.
func TestKindsOfFolderTogether(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	for _, side := range []string{"apply", "update"} {
		t.Run(side, func(t *testing.T) {
			ks := kinds()
			var places []Place
			var prefixes, dirs []string
			for i, k := range ks {
				d := t.TempDir()
				k.make(t, d)
				name := string(rune('a' + i))
				places = append(places, Place{Name: name, Folder: d, Ref: "r" + name})
				prefixes, dirs = append(prefixes, name), append(dirs, d)
			}
			b, s := openPlacesT(t, places)
			if side == "apply" {
				turnAndApply(t, b, s, ks, prefixes, dirs)
			} else {
				personAndUpdate(t, b, s, ks, prefixes, dirs)
			}
		})
	}
}

// The person's own exclude files count as git status counts them: the repo's
// .git/info/exclude, and core.excludesFile (the user's here, through HOME).
// A new file either leaves out stays out of the changeset and of an update;
// a tracked file they match is still the work. In a copy of several folders
// they hold under their folder only.
func TestPersonsExcludeFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	fixture(t, home, map[string]string{".gitconfig": "[core]\n\texcludesFile = ~/global-ignore\n", "global-ignore": "*.swp\n"})
	mk := func(t *testing.T) string {
		d := t.TempDir()
		fixture(t, d, map[string]string{"a.txt": "a1\n", "notes/kept.md": "k1\n"})
		gitDo(t, d, "init", "-q")
		gitDo(t, d, "add", "-A")
		gitDo(t, d, "commit", "-q", "-m", "c")
		fixture(t, d, map[string]string{".git/info/exclude": "notes/\n"})
		return d
	}
	turn := map[string]string{"a.txt": "a2\n", "x.swp": "s\n", "notes/kept.md": "k2\n", "notes/new.md": "n\n", "deep/y.swp": "s\n"}
	check := func(t *testing.T, got []string, prefix string) {
		t.Helper()
		want := []string{prefix + "a.txt", prefix + "notes/kept.md"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("changeset: %v, want %v", got, want)
		}
	}
	t.Run("one folder", func(t *testing.T) {
		src := mk(t)
		b, s := open(t, Copy, src)
		fixture(t, s.Dir, turn)
		if _, err := b.Checkpoint(s, "t1"); err != nil {
			t.Fatal(err)
		}
		check(t, changesetNames(t, s, "t1"), "")
		fixture(t, src, map[string]string{"notes/theirs.md": "t\n", "z.swp": "s\n"})
		res, err := b.UpdateFrom(s, "", at(src), false)
		if err != nil || !res.Clean {
			t.Fatalf("update: %+v %v", res, err)
		}
		if !missing(t, filepath.Join(s.Dir, "notes", "theirs.md")) || !missing(t, filepath.Join(s.Dir, "z.swp")) {
			t.Fatal("the update brought in a file the person's excludes leave out")
		}
	})
	t.Run("several folders", func(t *testing.T) {
		a, c := mk(t), t.TempDir()
		fixture(t, c, map[string]string{"notes/n.md": "n1\n"})
		b, s := openPlacesT(t, []Place{{Name: "a", Folder: a, Ref: "ra"}, {Name: "c", Folder: c, Ref: "rc"}})
		for p, v := range turn {
			fixture(t, s.Dir, map[string]string{"a/" + p: v})
		}
		fixture(t, s.Dir, map[string]string{"c/notes/new.md": "n\n"})
		if _, err := b.Checkpoint(s, "t1"); err != nil {
			t.Fatal(err)
		}
		got := changesetNames(t, s, "t1")
		check(t, got[:2], "a/")
		if len(got) != 3 || got[2] != "c/notes/new.md" {
			t.Fatalf("changeset: %v: c is no repo, a's excludes are not its own", got)
		}
	})
}

// In a copy of several folders, the person's side of an update compares names
// as a checkpoint does, by the disk's probe: on a disk that ignores case, a
// Node_Modules/ the person makes stays out, as /node_modules/ leaves it out of
// every checkpoint. Before, its scratch repo had no core.ignorecase and took
// it in.
func TestUpdateOfSeveralFoldersFoldsCaseAsTheDisk(t *testing.T) {
	a, c := t.TempDir(), t.TempDir()
	caseBlind(t, a)
	fixture(t, a, map[string]string{"a.txt": "a1\n"})
	fixture(t, c, map[string]string{"c.txt": "c1\n"})
	b, s := openPlacesT(t, []Place{{Name: "a", Folder: a, Ref: "ra"}, {Name: "c", Folder: c, Ref: "rc"}})
	fixture(t, s.Dir, map[string]string{"c/c.txt": "c2\n"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	fixture(t, a, map[string]string{"Node_Modules/x.js": "x\n"})
	paths := map[string]string{"ra": a, "rc": c}
	res, err := b.UpdateFrom(s, "", func(ref string) (string, bool) { p, ok := paths[ref]; return p, ok }, false)
	if err != nil || !res.Clean {
		t.Fatalf("update: %+v %v", res, err)
	}
	if !missing(t, filepath.Join(s.Dir, "a", "Node_Modules", "x.js")) {
		t.Fatal("the update brought in a folder the built-in list leaves out")
	}
}
