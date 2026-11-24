package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/staysup/agentbus/workspace"
)

// tree makes dir hold entries: "a/b" is a file with the value as its
// content, "a/" an empty folder, and a value "-> x" a symbolic link to x.
func tree(t *testing.T, dir string, entries map[string]string) {
	t.Helper()
	for p, v := range entries {
		full := filepath.Join(dir, filepath.FromSlash(p))
		target, link := strings.CutPrefix(v, "-> ")
		var err error
		switch {
		case strings.HasSuffix(p, "/"):
			err = os.MkdirAll(full, 0o755)
		case link:
			if err = os.MkdirAll(filepath.Dir(full), 0o755); err == nil {
				err = os.Symlink(target, full)
			}
		default:
			write(t, dir, map[string]string{p: v})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// held is the files and links of dir, by the names its folders list, in the
// words of tree. Folders are left out: an emptied one may stay.
func held(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			out[rel] = "-> " + target
			return err
		}
		out[rel] = text(t, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// files is held's answer without what it would need a folder for.
func files(entries map[string]string) map[string]string {
	out := map[string]string{}
	for p, v := range entries {
		if !strings.HasSuffix(p, "/") {
			out[p] = v
		}
	}
	return out
}

// turnChanges puts in c's store the changeset of a turn that found base and
// left now, as checkpoints of a copy would list both.
func turnChanges(t *testing.T, c *Connector, base, now map[string]string) string {
	t.Helper()
	base, now = files(base), files(now)
	state := func(of map[string]string, p string) *workspace.File {
		v, ok := of[p]
		if !ok {
			return nil
		}
		content, mode := []byte(v), fs.FileMode(0o644)
		if target, link := strings.CutPrefix(v, "-> "); link {
			content, mode = workspace.LinkContent(target), fs.ModeSymlink|0o777
		}
		if _, err := c.Store.PutBytes(content); err != nil {
			t.Fatal(err)
		}
		return &workspace.File{Size: int64(len(v)), Mode: mode, Hash: workspace.TreeHash(content)}
	}
	cs := workspace.Changeset{Space: "s", Checkpoint: "t1"}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range now {
		paths[p] = true
	}
	for p := range paths {
		if fc := (workspace.FileChange{Path: p, Base: state(base, p), Now: state(now, p)}); !workspace.SameFile(fc.Base, fc.Now) {
			cs.Files = append(cs.Files, fc)
		}
	}
	sort.Slice(cs.Files, func(i, j int) bool { return cs.Files[i].Path < cs.Files[j].Path })
	b, _ := json.Marshal(cs)
	h, err := c.Store.PutBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// caseBlind says whether dir is on a disk that takes names that differ only
// in case for one, as a Mac's is by default.
func caseBlind(t *testing.T, dir string) bool {
	t.Helper()
	tree(t, dir, map[string]string{"case-probe": "x"})
	defer os.Remove(filepath.Join(dir, "case-probe"))
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// A turn may put one kind of thing where another was, or change only the
// case of a name. The folder the turn started from takes it: the folder then
// holds what the turn left, and the preview lists what the apply wrote.
func TestApplyReplaces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base, now map[string]string
		caseBlind bool
	}{
		{name: "a file by a folder",
			base: map[string]string{"conf": "x"},
			now:  map[string]string{"conf/main": "n"}},
		{name: "a file by a deep folder",
			base: map[string]string{"conf": "x"},
			now:  map[string]string{"conf/a/b/main": "n", "conf/a/c": "m"}},
		{name: "a folder by a file",
			base: map[string]string{"conf/a": "x"},
			now:  map[string]string{"conf": "n"}},
		{name: "a folder of folders by a file",
			base: map[string]string{"conf/a/b": "x", "conf/a/c/d": "y", "conf/e/": ""},
			now:  map[string]string{"conf": "n"}},
		{name: "an empty folder by a file",
			base: map[string]string{"conf/": "", "keep": "k"},
			now:  map[string]string{"conf": "n", "keep": "k"}},
		{name: "a folder by a link",
			base: map[string]string{"conf/a": "x", "other/a": "x"},
			now:  map[string]string{"conf": "-> other", "other/a": "x"}},
		{name: "a link by a copy of what it named",
			base: map[string]string{"v1/a": "a", "v1/b/c": "c", "cur": "-> v1"},
			now:  map[string]string{"v1/a": "a", "v1/b/c": "c", "cur/a": "a", "cur/b/c": "c"}},
		{name: "a link by a folder of the same names",
			base: map[string]string{"v1/a": "a", "cur": "-> v1"},
			now:  map[string]string{"v1/a": "a", "cur/a": "other"}},
		{name: "a link out of the folder by a folder",
			base: map[string]string{"cur": "-> /etc"},
			now:  map[string]string{"cur/hostname": "n"}},
		{name: "a link and its folder swapped",
			base: map[string]string{"v1/a": "a", "cur": "-> v1"},
			now:  map[string]string{"cur/a": "a", "v1": "-> cur"}},
		{name: "a name's case", caseBlind: true,
			base: map[string]string{"button.tsx": "x", "keep": "k"},
			now:  map[string]string{"Button.tsx": "x", "keep": "k"}},
		{name: "a name's case, and the file", caseBlind: true,
			base: map[string]string{"button.tsx": "x"},
			now:  map[string]string{"Button.tsx": "y"}},
		{name: "a folder's case", caseBlind: true,
			base: map[string]string{"ui/comp/a": "x", "ui/comp/b/c": "y", "ui/empty/": ""},
			now:  map[string]string{"ui/Comp/a": "x", "ui/Comp/b/c": "z", "ui/empty/": ""}},
		{name: "a name's accent, written in another way",
			base: map[string]string{"caf\u00e9": "x"},
			now:  map[string]string{"cafe\u0301": "x"}},
		{name: "a folder by a file, in another case", caseBlind: true,
			base: map[string]string{"conf/a": "x"},
			now:  map[string]string{"Conf": "n"}},
		{name: "a link by a folder, in another case", caseBlind: true,
			base: map[string]string{"v1/a": "a", "cur": "-> v1"},
			now:  map[string]string{"v1/a": "a", "Cur/a": "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := t.TempDir()
			if tc.caseBlind && !caseBlind(t, folder) {
				t.Skip("this disk tells names that differ only in case apart")
			}
			tree(t, folder, tc.base)
			c, ref := newConnector(t, folder)
			h := turnChanges(t, c, tc.base, tc.now)
			p, err := c.Preview(context.Background(), ref, "", h)
			if err != nil || len(p.Conflicts) != 0 {
				t.Fatalf("preview: %+v %v", p.Conflicts, err)
			}
			got, err := c.Apply(context.Background(), ref, "", h)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if now := held(t, folder); !reflect.DeepEqual(now, files(tc.now)) {
				t.Errorf("the folder holds %v, want %v", now, files(tc.now))
			}
			for p := range tc.now {
				if fi, err := os.Lstat(filepath.Join(folder, p)); err != nil || strings.HasSuffix(p, "/") && !fi.IsDir() {
					t.Errorf("%s is gone", p)
				}
			}
			var shown []string
			for _, f := range p.Files {
				shown = append(shown, f.Path)
				want := "added"
				if _, ok := files(tc.now)[f.Path]; !ok {
					want = "deleted"
				}
				if f.Change != want {
					t.Errorf("preview: %s is %s, want %s", f.Path, f.Change, want)
				}
			}
			if !reflect.DeepEqual(shown, got.Files) {
				t.Errorf("preview files %v, apply wrote %v", shown, got.Files)
			}
			// Again, as after an apply that stopped part way: nothing to do.
			if again, err := c.Apply(context.Background(), ref, "", h); err != nil || len(again.Files) != 0 {
				t.Errorf("a second apply: %+v %v", again, err)
			}
			entries, _ := os.ReadDir(folder)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".agentbus-apply-") {
					t.Errorf("left in the folder: %s", e.Name())
				}
			}
		})
	}
}

// What the person put in the way is kept: the apply is refused, whole.
func TestApplyReplacesNothingOfThePersons(t *testing.T) {
	for _, tc := range []struct {
		name            string
		base, now, mine map[string]string // mine: the person's, after base
		gone            []string          // removed by the person first
		caseBlind       bool
		want            map[string]string // the conflicts
	}{
		{name: "a file added to the folder a file replaces",
			base: map[string]string{"conf/a": "x"},
			now:  map[string]string{"conf": "n"},
			mine: map[string]string{"conf/mine": "m"},
			want: map[string]string{"conf": WhyNotFile}},
		{name: "a link added to the folder a file replaces",
			base: map[string]string{"conf/a": "x", "k": "k"},
			now:  map[string]string{"conf": "n", "k": "k"},
			mine: map[string]string{"conf/mine": "-> ../k"},
			want: map[string]string{"conf": WhyNotFile}},
		{name: "a file added deep in the folder a link replaces",
			base: map[string]string{"conf/a": "x", "other/a": "x"},
			now:  map[string]string{"conf": "-> other", "other/a": "x"},
			mine: map[string]string{"conf/b/c/mine": "m"},
			want: map[string]string{"conf": WhyNotFile}},
		{name: "a file added to the folder a file replaces, in another case", caseBlind: true,
			base: map[string]string{"conf/a": "x"},
			now:  map[string]string{"Conf": "n"},
			mine: map[string]string{"conf/mine": "m"},
			want: map[string]string{"Conf": WhyNotFile}},
		{name: "the file a folder replaces, made a link",
			base: map[string]string{"conf": "x", "k": "k"},
			now:  map[string]string{"conf/main": "n", "k": "k"},
			gone: []string{"conf"},
			mine: map[string]string{"conf": "-> k"},
			want: map[string]string{"conf": WhyChanged}},
		{name: "a file changed in the folder a file replaces",
			base: map[string]string{"conf/a": "x"},
			now:  map[string]string{"conf": "n"},
			mine: map[string]string{"conf/a": "m"},
			want: map[string]string{"conf/a": WhyChanged}},
		{name: "the file a folder replaces, changed",
			base: map[string]string{"conf": "x"},
			now:  map[string]string{"conf/main": "n"},
			mine: map[string]string{"conf": "m"},
			want: map[string]string{"conf": WhyChanged}},
		{name: "the link a folder replaces, pointed elsewhere",
			base: map[string]string{"v1/a": "a", "v2/a": "a", "cur": "-> v1"},
			now:  map[string]string{"v1/a": "a", "v2/a": "a", "cur/a": "a"},
			gone: []string{"cur"},
			mine: map[string]string{"cur": "-> v2"},
			want: map[string]string{"cur": WhyChanged}},
		{name: "a file where the changes make a folder",
			base: map[string]string{"k": "k"},
			now:  map[string]string{"k": "k", "conf/main": "n"},
			mine: map[string]string{"conf": "m"},
			want: map[string]string{"conf/main": WhyNoFolder}},
		{name: "a link where the changes make a folder",
			base: map[string]string{"k": "k"},
			now:  map[string]string{"k": "k", "conf/main": "n"},
			mine: map[string]string{"other/o": "o", "conf": "-> other"},
			want: map[string]string{"conf/main": WhyNoFolder}},
		{name: "a folder made a link to another that changes too",
			base: map[string]string{"d/x": "same", "e/x": "same"},
			now:  map[string]string{"d/x": "D", "e/x": "E"},
			gone: []string{"d/x", "d"},
			mine: map[string]string{"d": "-> e"},
			want: map[string]string{"d/x": WhyNoFolder}},
		{name: "a folder where the changes write a file",
			base: map[string]string{"conf": "x"},
			now:  map[string]string{"conf": "n"},
			gone: []string{"conf"},
			mine: map[string]string{"conf/mine": "m"},
			want: map[string]string{"conf": WhyNotFile}},
		{name: "an empty folder where the changes write a file",
			base: map[string]string{"conf": "x"},
			now:  map[string]string{"conf": "n"},
			gone: []string{"conf"},
			mine: map[string]string{"conf/": ""},
			want: map[string]string{"conf": WhyNotFile}},
		{name: "a file respelt by the person too, and changed", caseBlind: true,
			base: map[string]string{"button.tsx": "x"},
			now:  map[string]string{"Button.tsx": "x"},
			gone: []string{"button.tsx"},
			mine: map[string]string{"BUTTON.tsx": "m"},
			want: map[string]string{"button.tsx": WhyChanged}},
		{name: "two files this disk takes for one", caseBlind: true,
			base: map[string]string{"button.tsx": "x"},
			now:  map[string]string{"button.tsx": "y", "Button.tsx": "z"},
			want: map[string]string{"Button.tsx": WhyAdded}},
		{name: "three files this disk takes for one", caseBlind: true,
			base: map[string]string{"BUTTON.tsx": "x", "button.tsx": "x"},
			now:  map[string]string{"button.tsx": "y", "Button.tsx": "z"},
			gone: []string{"BUTTON.tsx"},
			mine: map[string]string{"button.tsx": "x"},
			want: map[string]string{"Button.tsx": WhyAdded}},
		{name: "a file added under another spelling", caseBlind: true,
			base: map[string]string{"k": "k"},
			now:  map[string]string{"k": "k", "README": "n"},
			mine: map[string]string{"Readme": "m"},
			want: map[string]string{"README": WhyAdded}},
		{name: "a file added under another spelling, and another removed", caseBlind: true,
			base: map[string]string{"k": "k", "other": "o"},
			now:  map[string]string{"k": "k", "README": "n"},
			mine: map[string]string{"Readme": "m"},
			want: map[string]string{"README": WhyAdded}},
		{name: "a file renamed in case and changed", caseBlind: true,
			base: map[string]string{"button.tsx": "x"},
			now:  map[string]string{"Button.tsx": "x"},
			mine: map[string]string{"button.tsx": "m"},
			want: map[string]string{"button.tsx": WhyChanged}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := t.TempDir()
			if tc.caseBlind && !caseBlind(t, folder) {
				t.Skip("this disk tells names that differ only in case apart")
			}
			tree(t, folder, tc.base)
			c, ref := newConnector(t, folder)
			h := turnChanges(t, c, tc.base, tc.now)
			for _, p := range tc.gone {
				if err := os.Remove(filepath.Join(folder, p)); err != nil {
					t.Fatal(err)
				}
			}
			tree(t, folder, tc.mine)
			before := held(t, folder)
			p, err := c.Preview(context.Background(), ref, "", h)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range p.Files {
				if why := tc.want[f.Path]; why == WhyNoFolder && f.Change != "added" || why == WhyNotFile && f.Change != "modified" {
					t.Errorf("preview: %s is %s, and %s", f.Path, f.Change, why)
				}
			}
			_, err = c.Apply(context.Background(), ref, "", h)
			var cf *Conflict
			if !errors.As(err, &cf) {
				t.Fatalf("want a conflict, got %v; the folder holds %v", err, held(t, folder))
			}
			got := map[string]string{}
			for _, f := range cf.Files {
				got[f.Path] = f.Why
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("conflicts %v, want %v", got, tc.want)
			}
			if after := held(t, folder); !reflect.DeepEqual(after, before) {
				t.Errorf("a refused apply wrote: %v, was %v", after, before)
			}
		})
	}
}

// A file the person moved and left a link for is not the changes' to remove:
// its folder is a link now, so the folder holds no file at that path.
func TestApplyRemovesNothingThroughALink(t *testing.T) {
	folder := t.TempDir()
	base := map[string]string{"d/x": "x", "k": "k"}
	tree(t, folder, base)
	c, ref := newConnector(t, folder)
	h := turnChanges(t, c, base, map[string]string{"k": "K"})
	if err := os.Rename(filepath.Join(folder, "d"), filepath.Join(folder, "e")); err != nil {
		t.Fatal(err)
	}
	tree(t, folder, map[string]string{"d": "-> e"})
	got, err := c.Apply(context.Background(), ref, "", h)
	if err != nil || !reflect.DeepEqual(got.Files, []string{"k"}) {
		t.Fatalf("apply: %+v %v", got, err)
	}
	want := map[string]string{"d": "-> e", "e/x": "x", "k": "K"}
	if now := held(t, folder); !reflect.DeepEqual(now, want) {
		t.Fatalf("the folder holds %v, want %v", now, want)
	}
}

// A file the changes remove is not there when the person has a folder at its
// path, or a file where its folder was: nothing is removed.
func TestApplyRemovesWhatIsThere(t *testing.T) {
	folder := t.TempDir()
	base := map[string]string{"a": "x", "d/x": "x", "k": "k"}
	c, ref := newConnector(t, folder)
	h := turnChanges(t, c, base, map[string]string{"k": "K"})
	tree(t, folder, map[string]string{"a/mine": "m", "d": "m", "k": "k"})
	got, err := c.Apply(context.Background(), ref, "", h)
	if err != nil || !reflect.DeepEqual(got.Files, []string{"k"}) {
		t.Fatalf("apply: %+v %v", got, err)
	}
	want := map[string]string{"a/mine": "m", "d": "m", "k": "K"}
	if now := held(t, folder); !reflect.DeepEqual(now, want) {
		t.Fatalf("the folder holds %v, want %v", now, want)
	}
}

// A file that shows up in an emptied folder after the check is kept: placing
// a file over the folder removes folders only.
func TestPlaceStagedKeepsAFile(t *testing.T) {
	folder := t.TempDir()
	rt, err := os.OpenRoot(folder)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	tree(t, folder, map[string]string{"staged": "n", "conf/a/": "", "conf/b/late": "m"})
	if err := placeStaged(rt, "staged", "conf"); err == nil {
		t.Error("a file was placed over a folder that holds one")
	}
	want := map[string]string{"staged": "n", "conf/b/late": "m"}
	if now := held(t, folder); !reflect.DeepEqual(now, want) {
		t.Fatalf("the folder holds %v, want %v", now, want)
	}
	if err := os.Remove(filepath.Join(folder, "conf", "b", "late")); err != nil {
		t.Fatal(err)
	}
	if err := placeStaged(rt, "staged", "conf"); err != nil {
		t.Fatalf("placing over empty folders: %v", err)
	}
	if now := held(t, folder); !reflect.DeepEqual(now, map[string]string{"conf": "n"}) {
		t.Fatalf("the folder holds %v", now)
	}
}

// A folder that cannot be read may hold a file, so it does not make way.
func TestApplyOverAnUnreadableFolder(t *testing.T) {
	if os.Geteuid() <= 0 {
		t.Skip("every folder can be read here")
	}
	folder := t.TempDir()
	base := map[string]string{"conf/a": "x"}
	tree(t, folder, base)
	tree(t, folder, map[string]string{"conf/private/mine": "m"})
	c, ref := newConnector(t, folder)
	h := turnChanges(t, c, base, map[string]string{"conf": "n"})
	private := filepath.Join(folder, "conf", "private")
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(private, 0o755)
	var cf *Conflict
	if _, err := c.Apply(context.Background(), ref, "", h); !errors.As(err, &cf) || cf.Files[0] != (ConflictFile{"conf", WhyNotFile}) {
		t.Fatalf("apply over a folder that cannot be read: %v", err)
	}
	os.Chmod(private, 0o755)
	want := map[string]string{"conf/a": "x", "conf/private/mine": "m"}
	if now := held(t, folder); !reflect.DeepEqual(now, want) {
		t.Fatalf("a refused apply wrote: %v", now)
	}
}

// Two names of one file are two files on every disk: one added by the person
// is not the one the changes remove.
func TestApplyTellsHardLinksApart(t *testing.T) {
	folder := t.TempDir()
	base := map[string]string{"a": "x"}
	tree(t, folder, base)
	c, ref := newConnector(t, folder)
	h := turnChanges(t, c, base, map[string]string{"b": "n"})
	if err := os.Link(filepath.Join(folder, "a"), filepath.Join(folder, "b")); err != nil {
		t.Skip("no hard links here")
	}
	var cf *Conflict
	if _, err := c.Apply(context.Background(), ref, "", h); !errors.As(err, &cf) || cf.Files[0] != (ConflictFile{"b", WhyAdded}) {
		t.Fatalf("apply over the person's hard link: %v", err)
	}
}

// A file the person respelt too, unchanged, is the one the turn respelt: it
// ends as the turn spells it.
func TestApplyRespeltTwice(t *testing.T) {
	folder := t.TempDir()
	if !caseBlind(t, folder) {
		t.Skip("this disk tells names that differ only in case apart")
	}
	base, now := map[string]string{"button.tsx": "x"}, map[string]string{"Button.tsx": "x"}
	tree(t, folder, map[string]string{"BUTTON.tsx": "x"})
	c, ref := newConnector(t, folder)
	if _, err := c.Apply(context.Background(), ref, "", turnChanges(t, c, base, now)); err != nil {
		t.Fatal(err)
	}
	if got := held(t, folder); !reflect.DeepEqual(got, now) {
		t.Fatalf("the folder holds %v, want %v", got, now)
	}
}

// A folder respelt by the turn keeps its spelling while it holds a file of
// the person's, and loses nothing.
func TestApplyRespeltFolderInUse(t *testing.T) {
	folder := t.TempDir()
	if !caseBlind(t, folder) {
		t.Skip("this disk tells names that differ only in case apart")
	}
	base := map[string]string{"comp/a": "x"}
	tree(t, folder, base)
	tree(t, folder, map[string]string{"comp/mine": "m", "comp/empty/": ""})
	c, ref := newConnector(t, folder)
	h := turnChanges(t, c, base, map[string]string{"Comp/a": "x"})
	if _, err := c.Apply(context.Background(), ref, "", h); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"comp/a": "x", "comp/mine": "m"}
	if got := held(t, folder); !reflect.DeepEqual(got, want) {
		t.Fatalf("the folder holds %v, want %v", got, want)
	}
	if _, err := os.Lstat(filepath.Join(folder, "comp", "empty")); err != nil {
		t.Fatal("an empty folder of the person's is gone")
	}
}

// A file the base recorded without a hash is not found through a link
// either, though what the link names has its size, time and mode.
func TestApplyUnhashedThroughALink(t *testing.T) {
	folder := t.TempDir()
	tree(t, folder, map[string]string{"d/x": "x"})
	fi, err := os.Lstat(filepath.Join(folder, "d", "x"))
	if err != nil {
		t.Fatal(err)
	}
	c, ref := newConnector(t, folder)
	h := putChangeset(t, c, workspace.Changeset{Space: "s", Checkpoint: "t1", Files: []workspace.FileChange{
		{Path: "d/x", Base: &workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}, Now: fileOf("n", 0o644)}}},
		map[string]string{"n": "n"})
	if err := os.Rename(filepath.Join(folder, "d"), filepath.Join(folder, "e")); err != nil {
		t.Fatal(err)
	}
	tree(t, folder, map[string]string{"d": "-> e"})
	var cf *Conflict
	if _, err := c.Apply(context.Background(), ref, "", h); !errors.As(err, &cf) || cf.Files[0] != (ConflictFile{"d/x", WhyNoFolder}) {
		t.Fatalf("apply through a link: %v", err)
	}
	if got := text(t, filepath.Join(folder, "e", "x")); got != "x" {
		t.Fatalf("e/x: %q", got)
	}
}
