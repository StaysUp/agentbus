package isolation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// A tree records what a Snapshot does: random edits made through the
// editor, a file over a folder and a folder over a file among them, flatten
// back to the same files, and the tree diff lists what Changed lists.
func TestTreeMatchesSnapshot(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	names := []string{"a", "b", "c", "a.txt", "b b", "z"}
	randPath := func() string {
		n := 1 + rng.Intn(4)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = names[rng.Intn(len(names))]
		}
		return strings.Join(parts, "/")
	}
	file := func() workspace.File {
		f := workspace.File{Size: int64(rng.Intn(3)), Mode: 0o644, ModTime: int64(rng.Intn(3)), Hash: fmt.Sprint(rng.Intn(3))}
		if rng.Intn(4) == 0 {
			f.Mode = 0o755
		}
		if rng.Intn(5) == 0 {
			f.Hash = ""
		}
		return f
	}
	tr := &trees{dir: t.TempDir(), pending: map[string][]byte{}}
	// flat applies the same edits to a plain map, as a Snapshot holds files.
	flat := map[string]workspace.File{}
	removeUnder := func(rel string) {
		for p := range flat {
			if p == rel || strings.HasPrefix(p, rel+"/") {
				delete(flat, p)
			}
		}
	}
	root, count := emptyTree, 0
	for round := 0; round < 200; round++ {
		before := map[string]workspace.File{}
		for k, v := range flat {
			before[k] = v
		}
		ed := tr.edit(root, count)
		for i := 0; i < 1+rng.Intn(6); i++ {
			rel := randPath()
			switch rng.Intn(3) {
			case 0, 1:
				if err := ed.set(rel, file()); err != nil {
					t.Fatal(err)
				}
				// The file replaces a folder of its name, and any file
				// where one of its folders is.
				removeUnder(rel)
				parts := strings.Split(rel, "/")
				for j := 1; j < len(parts); j++ {
					delete(flat, strings.Join(parts[:j], "/"))
				}
				flat[rel] = file()
				// set again with the value recorded, so both agree.
				ed.set(rel, flat[rel])
			default:
				if err := ed.remove(rel); err != nil {
					t.Fatal(err)
				}
				removeUnder(rel)
			}
		}
		next, n := ed.finish()
		if err := tr.flush(); err != nil {
			t.Fatal(err)
		}
		got, err := tr.flatten(next)
		if err != nil {
			t.Fatal(err)
		}
		want := workspace.Empty()
		for p, f := range flat {
			want.Set(p, f)
		}
		if !reflect.DeepEqual(got.Paths(), want.Paths()) || n != len(flat) {
			t.Fatalf("round %d: tree holds %v (%d), want %v", round, got.Paths(), n, want.Paths())
		}
		for _, p := range want.Paths() {
			g, _ := got.File(p)
			w, _ := want.File(p)
			if g != w {
				t.Fatalf("round %d: %s is %+v, want %+v", round, p, g, w)
			}
		}
		old := workspace.Empty()
		for p, f := range before {
			old.Set(p, f)
		}
		d, err := tr.diff(root, next, nil)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, x := range d {
			paths = append(paths, x.rel)
		}
		sort.Strings(paths)
		if wantD := want.Changed(old); !reflect.DeepEqual(paths, wantD) && !(len(paths) == 0 && len(wantD) == 0) {
			t.Fatalf("round %d: diff %v, want %v", round, paths, wantD)
		}
		if next == root && len(paths) != 0 {
			t.Fatalf("round %d: equal trees with a diff", round)
		}
		root, count = next, n
	}
}

// treeFiles counts the trees a space's packs hold.
func treeFiles(t *testing.T, s *Space) int {
	t.Helper()
	n := 0
	packs, _ := filepath.Glob(filepath.Join(s.state(), "trees", "*.pack"))
	for _, p := range packs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for len(b) > 0 {
			nl := strings.IndexByte(string(b), '\n')
			_, l, _ := strings.Cut(string(b[:nl]), " ")
			size, _ := strconv.Atoi(l)
			b = b[nl+1+size:]
			n++
		}
	}
	return n
}

// A checkpoint after a few edits writes only the trees of the folders on
// their paths, and a checkpoint of nothing changed writes none.
func TestCheckpointWritesOnlyChangedTrees(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			files := map[string]string{}
			for i := 0; i < 20; i++ {
				for j := 0; j < 5; j++ {
					// Each file its own size, so no two folders are equal.
					files[fmt.Sprintf("d%02d/e%d/f.txt", i, j)] = strings.Repeat("x", 1+i*5+j)
				}
			}
			fixture(t, src, files)
			age(t, src)
			b, s := open(t, name, src)
			defer b.Close(s)
			at := treeFiles(t, s)
			if at < 20*6 {
				t.Fatalf("the Base wrote %d trees", at)
			}
			// Two contents: two equal folders would be one tree.
			fixture(t, s.Dir, map[string]string{"d03/e1/f.txt": "y", "d17/e4/f.txt": "zz"})
			if _, err := b.Checkpoint(s, "t1"); err != nil {
				t.Fatal(err)
			}
			// Two paths of two folders each below the top, and the top.
			if got := treeFiles(t, s) - at; got != 5 {
				t.Fatalf("a checkpoint of 2 edits wrote %d trees, want 5", got)
			}
			c, err := b.Diff(s, Base, "t1")
			if err != nil || c.Count != 2 {
				t.Fatalf("diff %+v, %v", c, err)
			}
			n := treeFiles(t, s)
			if _, err := b.Checkpoint(s, "t2"); err != nil {
				t.Fatal(err)
			}
			if treeFiles(t, s) != n {
				t.Fatal("a checkpoint of nothing changed wrote trees")
			}
		})
	}
}

// A checkpoint kept from before the trees, with its files in a
// .snap.json, is turned into a tree when first read: its diff and apply
// answer as they did, and the next checkpoint builds on it.
func TestOldCheckpointMigrated(t *testing.T) {
	src := t.TempDir()
	fixture(t, src, map[string]string{"a.txt": "a1", "sub/b.txt": "b1"})
	b, s := open(t, Copy, src)
	defer b.Close(s)
	fixture(t, s.Dir, map[string]string{"a.txt": "a2", "sub/c.txt": "c"})
	if _, err := b.Checkpoint(s, "t1"); err != nil {
		t.Fatal(err)
	}
	// Write both checkpoints back in the old form.
	list, err := headers(s)
	if err != nil || len(list) != 2 {
		t.Fatalf("%d checkpoints, %v", len(list), err)
	}
	for _, h := range list {
		snap, err := readSnapshot(s, h)
		if err != nil {
			t.Fatal(err)
		}
		snap.SetTaken(time.Unix(0, h.Tree.Taken))
		sb, _ := json.Marshal(snap)
		os.WriteFile(filepath.Join(s.checkpointDir(), h.file+".snap.json"), sb, 0o600)
		h.Tree = nil
		hb, _ := json.Marshal(h)
		os.WriteFile(filepath.Join(s.checkpointDir(), h.file+".json"), hb, 0o600)
	}
	os.RemoveAll(filepath.Join(s.state(), "trees"))
	c, err := b.Diff(s, Base, "t1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range c.Files {
		got[f.Path] = f.Change
	}
	if want := map[string]string{"a.txt": "modified", "sub/c.txt": "added"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("diff after migrate %v, want %v", got, want)
	}
	if left, _ := filepath.Glob(filepath.Join(s.checkpointDir(), "*.snap.json")); len(left) != 0 {
		t.Fatalf("old manifests left: %v", left)
	}
	fixture(t, s.Dir, map[string]string{"sub/b.txt": "b2"})
	if _, err := b.Checkpoint(s, "t2"); err != nil {
		t.Fatal(err)
	}
	if c, _ := b.Diff(s, "t1", "t2"); c.Count != 1 || c.Files[0].Path != "sub/b.txt" {
		t.Fatalf("diff on a migrated checkpoint %+v", c)
	}
	if _, err := b.Apply(s, Base, "t2"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(src, "sub", "b.txt")) != "b2" || read(t, filepath.Join(src, "a.txt")) != "a2" {
		t.Fatal("apply after migrate")
	}
}

// The tree format reads back what it wrote, a name with spaces and an
// unhashed file included, and a tree whose bytes do not match its name is
// refused.
func TestTreeEncoding(t *testing.T) {
	es := []tentry{
		{name: "a b", f: workspace.File{Size: 3, Mode: 0o644, ModTime: 7, Hash: "h"}},
		{name: "d", dir: true, hash: "abc", count: 4},
		{name: "link", f: workspace.File{Size: 1, Mode: fs.ModeSymlink | 0o777, ModTime: -1}},
	}
	got, err := decodeTree(encodeTree(es))
	if err != nil || !reflect.DeepEqual(got, es) {
		t.Fatalf("%+v, %v", got, err)
	}
	tr := &trees{dir: t.TempDir(), pending: map[string][]byte{}}
	h := tr.put(es, "")
	body := tr.pending[h]
	treeCache.Lock()
	delete(treeCache.m, h)
	treeCache.Unlock()
	// A pack whose tree does not match its hash is refused.
	os.WriteFile(filepath.Join(tr.dir, "bad.pack"), []byte(h+" "+strconv.Itoa(len(body))+"\n"+strings.Replace(string(body), "a b", "a c", 1)), 0o600)
	tr.pending = map[string][]byte{}
	if _, err := tr.get(h); err == nil {
		t.Fatal("a tree that does not match its hash was read")
	}
	os.Remove(filepath.Join(tr.dir, "bad.pack"))
	tr.pending[h] = body
	if err := tr.flush(); err != nil {
		t.Fatal(err)
	}
	treeCache.Lock()
	delete(treeCache.m, h)
	treeCache.Unlock()
	if got, err := tr.get(h); err != nil || !reflect.DeepEqual(got, es) {
		t.Fatalf("read back from its pack: %+v, %v", got, err)
	}
}

// A tree recorded from a Snapshot, and one edited from it to another,
// flatten back to that Snapshot: a file where a folder was, and a folder
// where a file was, among them.
func TestTreeFromSnapshot(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	names := []string{"item", "x", "y.txt"}
	random := func() *workspace.Snapshot {
		snap := workspace.Empty()
		var files []string
		for i := 0; i < 1+rng.Intn(8); i++ {
			parts := make([]string, 1+rng.Intn(3))
			for j := range parts {
				parts[j] = names[rng.Intn(len(names))]
			}
			files = append(files, strings.Join(parts, "/"))
		}
		// A path is a file or a folder, never both.
		for _, p := range files {
			clash := false
			for _, q := range files {
				if strings.HasPrefix(q, p+"/") {
					clash = true
				}
			}
			if !clash {
				snap.Set(p, workspace.File{Size: int64(rng.Intn(2)), Mode: 0o644, Hash: fmt.Sprint(rng.Intn(2))})
			}
		}
		return snap
	}
	same := func(a, b *workspace.Snapshot) bool {
		if !reflect.DeepEqual(a.Paths(), b.Paths()) {
			return false
		}
		for _, p := range a.Paths() {
			x, _ := a.File(p)
			y, _ := b.File(p)
			if x != y {
				return false
			}
		}
		return true
	}
	tr := &trees{dir: t.TempDir(), pending: map[string][]byte{}}
	for round := 0; round < 300; round++ {
		prev, cur := random(), random()
		st, _, err := tr.fromSnapshot(nil, nil, prev)
		if err != nil {
			t.Fatal(err)
		}
		old, _ := tr.flatten(st.Root)
		if !same(old, prev) {
			t.Fatalf("round %d: %v recorded as %v", round, prev.Paths(), old.Paths())
		}
		st2, n, err := tr.fromSnapshot(&st, old, cur)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := tr.flatten(st2.Root)
		if !same(got, cur) || n != cur.Len() {
			t.Fatalf("round %d: %v then %v recorded as %v (%d)", round, prev.Paths(), cur.Paths(), got.Paths(), n)
		}
	}
}

// A file replaced by a folder of the same name, and the reverse, show in
// the diff. Apply refuses them and writes nothing (it checks each path on
// its own, and a folder where a file goes is not a file): a limit kept in
// the framework TODO.
func TestFileBecomesFolder(t *testing.T) {
	for _, name := range fast(t) {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"item": "a file", "dir/x.txt": "x"})
			age(t, src)
			b, s := open(t, name, src)
			defer b.Close(s)
			os.Remove(filepath.Join(s.Dir, "item"))
			os.RemoveAll(filepath.Join(s.Dir, "dir"))
			fixture(t, s.Dir, map[string]string{"item/child.txt": "c", "dir": "now a file"})
			if _, err := b.Checkpoint(s, "t1"); err != nil {
				t.Fatal(err)
			}
			c, err := b.Diff(s, Base, "t1")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, f := range c.Files {
				got[f.Path] = f.Change
			}
			want := map[string]string{"item": "deleted", "item/child.txt": "added", "dir/x.txt": "deleted", "dir": "added"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("diff %v, want %v", got, want)
			}
			var cf *Conflict
			if _, err := b.Apply(s, Base, "t1"); !errors.As(err, &cf) {
				t.Fatalf("apply: %v", err)
			}
			if read(t, filepath.Join(src, "item")) != "a file" || read(t, filepath.Join(src, "dir", "x.txt")) != "x" {
				t.Fatal("a refused apply wrote")
			}
		})
	}
}

// A tree is found in its pack even when reading the packs fills the cache
// past its limit and empties it.
func TestTreeReadPastTheCache(t *testing.T) {
	was := maxCached
	maxCached = 3
	defer func() { maxCached = was }()
	tr := &trees{dir: t.TempDir(), pending: map[string][]byte{}}
	var hs []string
	for i := 0; i < 6; i++ {
		hs = append(hs, tr.put([]tentry{{name: fmt.Sprint("f", i), f: workspace.File{Size: int64(i), Mode: 0o644}}}, ""))
	}
	if err := tr.flush(); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		treeCache.Lock()
		treeCache.m, treeCache.n = map[string][]tentry{}, 0
		treeCache.Unlock()
		if es, err := tr.get(h); err != nil || len(es) != 1 {
			t.Fatalf("tree %s: %v, %v", h[:8], es, err)
		}
	}
}
