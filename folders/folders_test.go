package folders

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder keeps its reference: the same path, cleaned, gives the same one,
// across a restart; the file is the owner's only; the path never shows in
// what List reports.
func TestRegistry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "folders.json")
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !connectorShape(r.Machine()) {
		t.Fatalf("connector %q", r.Machine())
	}
	dir := t.TempDir()
	ref, err := r.Register(dir)
	if err != nil || !strings.HasPrefix(ref, "fld_") || len(ref) != 20 || ref != Ref(dir) {
		t.Fatalf("%q %v", ref, err)
	}
	if again, _ := r.Register(dir + "/."); again != ref {
		t.Fatalf("the cleaned path got %q, want %q", again, ref)
	}
	if _, err := r.Register("relative/path"); err == nil {
		t.Fatal("a relative path was kept")
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("folders.json: %v %v", info.Mode(), err)
	}
	r2, err := Open(p)
	if err != nil || r2.Machine() != r.Machine() {
		t.Fatalf("reopened: %v", err)
	}
	if got, ok := r2.Path(ref); !ok || got != filepath.Clean(dir) {
		t.Fatalf("path %q %v", got, ok)
	}
	if _, ok := r2.Path("fld_0000000000000000"); ok {
		t.Fatal("an unknown reference resolved")
	}
	list := r2.List(10)
	if len(list) != 1 || list[0].Ref != ref || list[0].Name != filepath.Base(dir) {
		t.Fatalf("%+v", list)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("a broken file was read")
	}
}

// A session copy is never listed as a folder: one inside the data folder's
// copies, and a 0.2 copy (worktrees/job_...) that 0.3.0's migration
// registered from a data folder since moved. Each stays registered, so a
// job that names it still finds it, and nothing on disk is touched.
func TestListLeavesOutCopies(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "folders.json")
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	r.CopiesIn(filepath.Join(home, "sessions"), filepath.Join(home, "worktrees"))
	mine := t.TempDir()
	keep := []string{mine, "/work/worktrees", "/work/worktrees/job_2f2e608012a9ca1", "/work/worktrees/job_2F2E608012A9CA19",
		filepath.Join(home, "sessions"), home + "-other/sessions/ses_1"}
	hide := []string{filepath.Join(home, "sessions", "ses_0123456789abcdef"), filepath.Join(home, "sessions", "ses_0123456789abcdef", "sub"),
		filepath.Join(home, "worktrees", "job_0123456789abcdef"), "/old/home/worktrees/job_2f2e608012a9ca19",
		"/old/home/worktrees/job_2f2e608012a9ca19/sub"}
	refs := map[string]string{}
	for _, f := range append(append([]string{}, keep...), hide...) {
		ref, err := r.Register(f)
		if err != nil {
			t.Fatal(err)
		}
		refs[ref] = f
	}
	listed := map[string]bool{}
	for _, f := range r.List(100) {
		listed[refs[f.Ref]] = true
	}
	for _, f := range keep {
		if !listed[f] {
			t.Errorf("%s is not listed", f)
		}
	}
	for _, f := range hide {
		if listed[f] {
			t.Errorf("the copy %s is listed", f)
		}
		if got, ok := r.Path(Ref(f)); !ok || got != f {
			t.Errorf("the copy %s is no longer registered: %q %v", f, got, ok)
		}
	}
	// max counts listed folders only.
	if got := r.List(len(keep)); len(got) != len(keep) {
		t.Fatalf("List(%d) gave %d", len(keep), len(got))
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal(err)
	}
}

// A typed folder must be an existing directory this user can read; each
// refusal is its own sentence, and Add registers only a folder that passes.
func TestAddChecks(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	cases := []struct {
		path string
		want error
	}{
		{"relative", ErrNotAbsolute},
		{filepath.Join(dir, "nope"), ErrNoFolder},
		{file, ErrNotDir},
		{locked, ErrUnreadable},
		{dir, nil},
	}
	for _, c := range cases {
		if c.want == ErrUnreadable && os.Geteuid() == 0 {
			continue // root reads every folder
		}
		ref, err := r.Add(c.path)
		if err != c.want {
			t.Errorf("Add(%s): %v, want %v", c.path, err, c.want)
		}
		if _, ok := r.Path(Ref(c.path)); ok != (c.want == nil) || (c.want == nil && ref != Ref(dir)) {
			t.Errorf("Add(%s) registered %v, ref %q", c.path, ok, ref)
		}
	}
}

// compat 0.6: drop connector. A folders.json a build before 0.6 wrote holds
// the machine id under "connector". Open reads it, and the next save writes
// it under "machine" and drops the old key, so the name leaves after one
// release without a runner losing its id.
func TestOpenFoldsOldConnectorKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "folders.json")
	const id = "con_0123456789abcdef"
	if err := os.WriteFile(p, []byte(`{"connector":"`+id+`","folders":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(p)
	if err != nil || r.Machine() != id {
		t.Fatalf("read old key: machine %q, err %v", r.Machine(), err)
	}
	// A register saves; the file now names the id under machine, not connector.
	if _, err := r.Register(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"machine":"`+id+`"`) || strings.Contains(s, `"connector"`) {
		t.Fatalf("saved file still names connector or lost machine: %s", s)
	}
	// And it reopens from the new key alone.
	r2, err := Open(p)
	if err != nil || r2.Machine() != id {
		t.Fatalf("reopen from machine key: %q %v", r2.Machine(), err)
	}
}

// Expand reads a leading ~ or ~/ as the given home and leaves every other
// path for CheckShape; with no home, a ~ path is refused in plain words.
func TestExpand(t *testing.T) {
	home := filepath.Join(t.TempDir(), "u")
	cases := []struct {
		path, home, want string
		err              error
	}{
		{"~", home, home, nil},
		{"~/", home, home, nil},
		{"~/Sites/project", home, filepath.Join(home, "Sites", "project"), nil},
		{"/abs/path", home, "/abs/path", nil},
		{"relative", home, "relative", nil},
		{"~root/x", home, "~root/x", nil},
		{"~/x", "", "", ErrNoHome},
	}
	for _, c := range cases {
		got, err := Expand(c.path, c.home)
		if got != c.want || err != c.err {
			t.Errorf("Expand(%q, %q) = %q, %v; want %q, %v", c.path, c.home, got, err, c.want, c.err)
		}
	}
	// What Expand leaves relative, CheckShape refuses.
	for _, p := range []string{"relative", "~root/x"} {
		got, _ := Expand(p, home)
		if CheckShape(got) != ErrNotAbsolute {
			t.Errorf("CheckShape(Expand(%q)) accepted a relative path", p)
		}
	}
}

// agentbus up and its runner, two processes, open one folders.json: each sees
// what the other registered, and neither write drops the other's folders.
func TestRegistrySharedByTwo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "folders.json")
	a, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Machine() != b.Machine() {
		t.Fatalf("two machine ids: %s %s", a.Machine(), b.Machine())
	}
	one, err := a.Register("/work/one")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := b.Path(one); !ok || got != "/work/one" {
		t.Fatalf("the other process sees %q %v", got, ok)
	}
	two, err := b.Register("/work/two")
	if err != nil {
		t.Fatal(err)
	}
	three, err := a.Register("/work/three")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{one, two, three} {
		if _, ok := c.Path(ref); !ok {
			t.Fatalf("%s lost from the file", ref)
		}
	}
}
